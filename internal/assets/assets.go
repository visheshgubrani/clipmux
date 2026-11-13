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
	"unicode"
	"unicode/utf8"

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
	// Share names the share link this ID belongs to (D104); absent for
	// the asset's other IDs.
	Share *ShareRef `json:"share,omitempty"`
}

// ShareRef is the share link a playback ID was created for.
type ShareRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// View is an asset as the API returns it.
type View struct {
	ID          string `json:"id"`
	AppID       string `json:"app_id"`
	Status      string `json:"status"`
	Version     int64  `json:"version"`
	Passthrough string `json:"passthrough,omitempty"`
	// Title is a name for people (D099), absent until one is given.
	Title     string `json:"title,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	// BaseURL is the media origin; playback URLs are
	// {base_url}/v/{token}/{playback_id}/master.m3u8 (signed) and
	// {base_url}/p/{playback_id}/master.m3u8 (public).
	BaseURL     string       `json:"base_url"`
	PlaybackIDs []PlaybackID `json:"playback_ids"`
	// DurationSeconds is the current output's duration; null until the
	// first output commits.
	DurationSeconds *float64 `json:"duration_seconds"`
	// Tracks are its subtitles and chapters (P5-T16), oldest first; retired
	// and deleted tracks aren't shown.
	Tracks    []Track   `json:"tracks"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt moves with Version; GET /v1/assets?updated_since= pages on it.
	UpdatedAt time.Time `json:"updated_at"`
}

// Track is a subtitles or chapters track as the API shows it. A track
// being published shows as preparing: it is ready once every route serves
// it.
type Track struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	// Status is preparing, ready or errored.
	Status    string `json:"status"`
	Language  string `json:"language,omitempty"`
	Name      string `json:"name,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

// TrackView is a track row as the API shows it.
func TrackView(t db.Track) Track {
	status := t.Status
	if status == "publishing" {
		status = "preparing"
	}
	return Track{ID: t.ID, Kind: t.Kind, Source: t.Source, Status: status, Language: t.Language.String, Name: t.Name.String, ErrorCode: t.ErrorCode.String}
}

// MaxTitle is the longest asset title, in characters.
const MaxTitle = 200

// Title checks an asset title as an upload, an import or a rename gives it:
// trimmed, at most MaxTitle characters, one line. Empty means none.
func Title(raw string) (pgtype.Text, error) {
	t := strings.TrimSpace(raw)
	if utf8.RuneCountInString(t) > MaxTitle {
		return pgtype.Text{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "title is limited to 200 characters")
	}
	if strings.ContainsFunc(t, unicode.IsControl) {
		return pgtype.Text{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "title must be one line of text")
	}
	return pgtype.Text{String: t, Valid: t != ""}, nil
}

// Load builds the view of an asset. base_url is the project's media URL,
// read in the same transaction as the asset.
func Load(ctx context.Context, q *db.Queries, a db.Asset) (View, error) {
	views, err := LoadMany(ctx, q, []db.Asset{a})
	if err != nil {
		return View{}, err
	}
	return views[0], nil
}

// LoadMany builds the views of a page of assets in three queries, whatever
// its length (D072, D082): the media URL once per project, and every
// asset's playback IDs and tracks at once.
func LoadMany(ctx context.Context, q *db.Queries, rows []db.Asset) ([]View, error) {
	ids := make([]string, len(rows))
	for i, a := range rows {
		ids[i] = a.ID
	}
	pids, err := q.ListPlaybackIDsForAssets(ctx, ids)
	if err != nil {
		return nil, err
	}
	byAsset := map[string][]db.ListPlaybackIDsForAssetsRow{}
	for _, p := range pids {
		byAsset[p.AssetID] = append(byAsset[p.AssetID], p)
	}
	trks, err := q.ListTracksForAssets(ctx, ids)
	if err != nil {
		return nil, err
	}
	tracksOf := map[string][]db.Track{}
	for _, t := range trks {
		tracksOf[t.AssetID] = append(tracksOf[t.AssetID], t)
	}
	baseURLs := map[string]string{}
	out := make([]View, 0, len(rows))
	for _, a := range rows {
		baseURL, ok := baseURLs[a.ProjectID]
		if !ok {
			if baseURL, err = q.GetMediaURL(ctx, a.ProjectID); err != nil {
				return nil, err
			}
			baseURLs[a.ProjectID] = baseURL
		}
		out = append(out, view(a, baseURL, byAsset[a.ID], tracksOf[a.ID]))
	}
	return out, nil
}

func view(a db.Asset, baseURL string, pids []db.ListPlaybackIDsForAssetsRow, trks []db.Track) View {
	playable := a.CurrentOutputID.Valid && a.Status != "deleted"
	v := View{
		ID: a.ID, AppID: a.AppID, Status: a.Status, Version: a.Version, BaseURL: baseURL,
		Passthrough: a.Passthrough.String, Title: a.Title.String, ErrorCode: a.ErrorCode.String,
		PlaybackIDs: make([]PlaybackID, 0, len(pids)),
		CreatedAt:   a.CreatedAt.Time.UTC(), UpdatedAt: a.UpdatedAt.Time.UTC(),
	}
	for _, p := range pids {
		pv := PlaybackID{ID: p.ID, Policy: p.Policy, Playable: playable && p.Status == "active", Status: p.Status}
		if p.ShareID.Valid {
			pv.Share = &ShareRef{ID: p.ShareID.String, Name: p.ShareName.String}
		}
		v.PlaybackIDs = append(v.PlaybackIDs, pv)
	}
	if a.DurationSeconds.Valid {
		d := a.DurationSeconds.Float64
		v.DurationSeconds = &d
	}
	v.Tracks = make([]Track, 0, len(trks))
	for _, t := range trks {
		v.Tracks = append(v.Tracks, TrackView(t))
	}
	return v
}

// API serves GET /v1/assets and GET /v1/assets/{id}.
type API struct {
	pool *pgxpool.Pool
	// Auth accepts API keys, and the dashboard session once serve sets it.
	Auth *apikeys.Authenticator
	// Drain is done once serve starts shutting down: event long-polls then
	// answer with what they have. nil never drains.
	Drain context.Context
	// UploadOrigins lists the S3 and R2 bucket origins browsers PUT upload
	// parts to (stores.Set.Origins); nil means none besides the server.
	UploadOrigins func(ctx context.Context, projectID string) ([]string, error)
}

// New returns the assets API.
func New(pool *pgxpool.Pool) *API { return &API{pool: pool, Auth: apikeys.NewAuthenticator(pool)} }

// Register adds the routes to mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/assets/{id}", a.get)
	mux.HandleFunc("GET /v1/assets/{id}/tracks/{track}", a.getTrack)
	mux.HandleFunc("GET /v1/assets/{id}/tracks/{track}/transcript", a.getTranscript)
	mux.HandleFunc("GET /v1/assets", a.list)
	mux.HandleFunc("GET /v1/events", a.events)
	mux.HandleFunc("GET /v1/app", a.getApp)
}

func (a *API) authorize(w http.ResponseWriter, r *http.Request) (apikeys.Key, bool) {
	return a.Auth.Require(w, r, apikeys.AssetsRead)
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
	v, err := Load(r.Context(), q, asset)
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
		if after, afterID, err = DecodeCursor(c); err != nil {
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
		next = EncodeCursor(last.UpdatedAt.Time, last.ID)
	}
	a.writeList(w, r, q, rows, next)
}

func (a *API) writeList(w http.ResponseWriter, r *http.Request, q *db.Queries, rows []db.Asset, next string) {
	if err := WriteList(r.Context(), w, q, rows, next); err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// WriteList sends a page of asset views, {"data": [...], "next_cursor"?}.
func WriteList(ctx context.Context, w http.ResponseWriter, q *db.Queries, rows []db.Asset, next string) error {
	out, err := LoadMany(ctx, q, rows)
	if err != nil {
		return err
	}
	body := map[string]any{"data": out}
	if next != "" {
		body["next_cursor"] = next
	}
	httpjson.Write(w, http.StatusOK, body)
	return nil
}

// EncodeCursor makes an opaque page cursor from the last row's time and ID.
func EncodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "," + id))
}

// DecodeCursor reads a cursor EncodeCursor made.
func DecodeCursor(c string) (time.Time, string, error) {
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
