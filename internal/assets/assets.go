// Package assets serves the read side of the integration API
// (docs/architecture.md, "Integration API"): an asset's status, version and
// playback IDs, by ID, by the app's own passthrough reference, or every
// change since a time (catch-up after missed webhooks). Apps see only their
// own assets.
package assets

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
	// Playable is false until the asset has an output to serve.
	Playable bool `json:"playable"`
	// Status is active, revoking (unpublish not yet confirmed) or revoked.
	Status string `json:"status"`
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
	// UpdatedAt moves with Version; GET /v1/assets?updated_since= pages on it.
	UpdatedAt time.Time `json:"updated_at"`
}

// Load builds the view of an asset.
func Load(ctx context.Context, q *db.Queries, a db.Asset, baseURL string) (View, error) {
	pids, err := q.ListPlaybackIDs(ctx, a.ID)
	if err != nil {
		return View{}, err
	}
	playable := a.CurrentOutputID.Valid && a.Status != "deleted"
	v := View{
		ID: a.ID, Status: a.Status, Version: a.Version, BaseURL: baseURL,
		Passthrough: a.Passthrough.String, ErrorCode: a.ErrorCode.String,
		PlaybackIDs: make([]PlaybackID, 0, len(pids)),
		CreatedAt:   a.CreatedAt.Time.UTC(), UpdatedAt: a.UpdatedAt.Time.UTC(),
	}
	for _, p := range pids {
		v.PlaybackIDs = append(v.PlaybackIDs, PlaybackID{ID: p.ID, Policy: p.Policy, Playable: playable && p.Status == "active", Status: p.Status})
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
	return apikeys.Require(w, r, a.pool, apikeys.AssetsRead)
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

// list answers GET /v1/assets in one of two forms: ?passthrough= finds an
// app's assets by its own reference, newest first; ?updated_since= (with
// optional cursor and limit) lists every asset changed since a time, oldest
// first, deleted ones included, for catching up on missed webhooks.
func (a *API) list(w http.ResponseWriter, r *http.Request) {
	key, ok := a.authorize(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	passthrough, since := query.Get("passthrough"), query.Get("updated_since")
	switch {
	case passthrough != "" && since == "":
		a.byPassthrough(w, r, key, passthrough)
	case since != "" && passthrough == "":
		a.updatedSince(w, r, key, since)
	default:
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "pass either passthrough or updated_since")
	}
}

func (a *API) byPassthrough(w http.ResponseWriter, r *http.Request, key apikeys.Key, passthrough string) {
	q := db.New(a.pool)
	rows, err := q.ListAssetsByPassthrough(r.Context(), db.ListAssetsByPassthroughParams{
		AppID: key.AppID, Passthrough: pgtype.Text{String: passthrough, Valid: true}, Limit: 100,
	})
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	a.writeList(w, r, q, rows, "")
}

const maxPage = 100

func (a *API) updatedSince(w http.ResponseWriter, r *http.Request, key apikeys.Key, since string) {
	query := r.URL.Query()
	after, err := time.Parse(time.RFC3339Nano, since)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "updated_since must be an RFC 3339 time")
		return
	}
	afterID := "" // every ID sorts after "", so updated_since is inclusive
	if c := query.Get("cursor"); c != "" {
		if after, afterID, err = decodeCursor(c); err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid_cursor", "cursor is not one this API returned")
			return
		}
	}
	limit := maxPage
	if l := query.Get("limit"); l != "" {
		if limit, err = strconv.Atoi(l); err != nil || limit < 1 || limit > maxPage {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
			return
		}
	}
	q := db.New(a.pool)
	rows, err := q.ListAssetsUpdatedSince(r.Context(), db.ListAssetsUpdatedSinceParams{
		AppID: key.AppID, AfterTime: pgtype.Timestamptz{Time: after, Valid: true}, AfterID: afterID, MaxAssets: int32(limit + 1),
	})
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = encodeCursor(last.UpdatedAt.Time, last.ID)
	}
	a.writeList(w, r, q, rows, next)
}

func (a *API) writeList(w http.ResponseWriter, r *http.Request, q *db.Queries, rows []db.Asset, next string) {
	out := make([]View, 0, len(rows))
	for _, asset := range rows {
		v, err := Load(r.Context(), q, asset, a.baseURL)
		if err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
		out = append(out, v)
	}
	body := map[string]any{"data": out}
	if next != "" {
		body["next_cursor"] = next
	}
	httpjson.Write(w, http.StatusOK, body)
}

// A cursor is the last row's (updated_at, id), opaque to clients.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "," + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", err
	}
	ts, id, ok := strings.Cut(string(raw), ",")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", errors.New("assets: malformed cursor")
	}
	return t, id, nil
}
