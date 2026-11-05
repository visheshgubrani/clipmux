// Package assets serves the read side of the integration API
// (docs/architecture.md, "Integration API"): an asset's status, version and
// playback IDs, by ID or by the app's own passthrough reference. Apps see
// only their own assets.
package assets

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// PlaybackID is a public or signed handle for an asset.
type PlaybackID struct {
	ID     string `json:"id"`
	Policy string `json:"policy"`
}

// View is an asset as the API returns it.
type View struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
	Passthrough string `json:"passthrough,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	// BaseURL is the media origin; playback URLs are
	// {base_url}/v/{token}/{playback_id}/master.m3u8 (signed) and
	// {base_url}/p/{playback_id}/master.m3u8 (public).
	BaseURL     string       `json:"base_url"`
	PlaybackIDs []PlaybackID `json:"playback_ids"`
	CreatedAt   time.Time    `json:"created_at"`
}

// Load builds the view of an asset.
func Load(ctx context.Context, q *db.Queries, a db.Asset, baseURL string) (View, error) {
	pids, err := q.ListPlaybackIDs(ctx, a.ID)
	if err != nil {
		return View{}, err
	}
	v := View{
		ID: a.ID, Status: a.Status, Version: a.Version, BaseURL: baseURL,
		Passthrough: a.Passthrough.String, ErrorCode: a.ErrorCode.String,
		PlaybackIDs: make([]PlaybackID, 0, len(pids)), CreatedAt: a.CreatedAt.Time,
	}
	for _, p := range pids {
		v.PlaybackIDs = append(v.PlaybackIDs, PlaybackID{ID: p.ID, Policy: p.Policy})
	}
	return v, nil
}

// API serves GET /v1/assets and GET /v1/assets/{id}.
type API struct {
	pool    *pgxpool.Pool
	baseURL string
}

// New returns the assets API.
func New(pool *pgxpool.Pool, baseURL string) *API { return &API{pool: pool, baseURL: baseURL} }

// Register adds the routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/assets/{id}", a.get)
	mux.HandleFunc("GET /v1/assets", a.list)
}

func (a *API) authorize(w http.ResponseWriter, r *http.Request) (apikeys.Key, bool) {
	key, err := apikeys.Authenticate(r.Context(), a.pool, httpjson.Bearer(r))
	if err != nil {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
		return apikeys.Key{}, false
	}
	if !key.Can(apikeys.AssetsRead) {
		httpjson.Error(w, http.StatusForbidden, "insufficient_scope", "this key lacks assets:read")
		return apikeys.Key{}, false
	}
	return key, true
}

func (a *API) get(w http.ResponseWriter, r *http.Request) {
	key, ok := a.authorize(w, r)
	if !ok {
		return
	}
	q := db.New(a.pool)
	asset, err := q.GetAsset(r.Context(), db.GetAssetParams{ID: r.PathValue("id"), AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	v, err := Load(r.Context(), q, asset, a.baseURL)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	httpjson.Write(w, http.StatusOK, v)
}

// list finds an app's assets by passthrough, newest first. A general
// paginated listing arrives with the dashboard.
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	key, ok := a.authorize(w, r)
	if !ok {
		return
	}
	passthrough := r.URL.Query().Get("passthrough")
	if passthrough == "" {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "the passthrough query parameter is required")
		return
	}
	q := db.New(a.pool)
	rows, err := q.ListAssetsByPassthrough(r.Context(), db.ListAssetsByPassthroughParams{
		AppID: key.AppID, Passthrough: pgtype.Text{String: passthrough, Valid: true}, Limit: 100,
	})
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	out := make([]View, 0, len(rows))
	for _, asset := range rows {
		v, err := Load(r.Context(), q, asset, a.baseURL)
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		out = append(out, v)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
}
