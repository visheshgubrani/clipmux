// Package shares serves share links (D104): a page on the public URL that
// plays one asset without a session or an app's token. Each link owns one
// signed playback ID, created with it, and its state follows that ID:
// revoking the link revokes the ID through the existing unpublish, and the
// link's views are that ID's views.
//
// A link's URL is {public_url}/s/{secret}. The page fetches short-lived
// tokens from POST /s/{secret}/playback, signed with the server's internal
// key, so a link can outlive the longest token and still expire or be
// revoked on its own. The secret is looked up by its SHA-256 and sealed
// beside it, so the owner can copy the link again.
package shares

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/idempotency"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/ratelimit"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// Link states.
const (
	Active  = "active"
	Expired = "expired"
	Revoked = "revoked"
)

const (
	// sealTable names the sealed secrets (secrets.Columns).
	sealTable = "share_links"
	// MaxName bounds a link's name.
	MaxName = 100
	// minPlayLifetime is the shortest token /playback signs, unless the
	// link expires sooner; a longer video gets twice its duration.
	minPlayLifetime = 4 * time.Hour
	// viewsDays is the window of a link's views in a listing.
	viewsDays = 30
)

// viewerID is the grammar of the page's per-browser viewer ID; anything
// else is dropped, never refused.
var viewerID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// Service serves share links.
type Service struct {
	pool *pgxpool.Pool
	box  *secrets.Box
	life *lifecycle.Service

	// Auth accepts API keys, and the dashboard session once serve sets it.
	Auth *apikeys.Authenticator
	// ClientIP is the address a public request is limited by.
	ClientIP func(*http.Request) netip.Addr
	// Plays limits the public endpoint's successes per address (failures
	// count toward delivery's refusal limiter instead).
	Plays  *ratelimit.Buckets
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns the share links service. life creates and revokes the links'
// playback IDs.
func New(pool *pgxpool.Pool, box *secrets.Box, life *lifecycle.Service) *Service {
	return &Service{
		pool: pool, box: box, life: life, Auth: apikeys.NewAuthenticator(pool),
		ClientIP: func(r *http.Request) netip.Addr { return ratelimit.ClientIP(r, nil) },
		Plays:    ratelimit.NewBuckets(2, 20),
		Now:      time.Now, Logger: slog.New(slog.DiscardHandler),
	}
}

// Register adds the share link routes. The public endpoint's wildcard is
// named token, so the access line redacts it (D064).
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/assets/{id}/shares", s.handleCreate)
	mux.HandleFunc("GET /v1/assets/{id}/shares", s.handleList)
	mux.HandleFunc("DELETE /v1/shares/{id}", s.handleRevoke)
	mux.HandleFunc("POST /v1/shares/{id}/stream-url", s.handleStreamURL)
	mux.HandleFunc("POST /s/{token}/playback", s.handlePlayback)
}

// View is a share link as the API returns it. URL holds the secret.
type View struct {
	ID         string     `json:"id"`
	AssetID    string     `json:"asset_id"`
	Name       string     `json:"name"`
	URL        string     `json:"url"`
	PlaybackID string     `json:"playback_id"`
	ExpiresAt  *time.Time `json:"expires_at"`
	// Status is active, expired or revoked.
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	// Views30d is the link's views over the last 30 UTC days, in listings.
	Views30d *int64 `json:"views_30d,omitempty"`
}

// CreateRequest is the body of POST /v1/assets/{id}/shares.
type CreateRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// status is a link's state from its expiry and its playback ID's status.
// Once the expiry passes the link reads expired, whatever stopped its ID
// (the expiry loop, a revoke or the asset's deletion); before it, a stopped
// ID means the link was revoked. Only the service's clock is compared.
func status(expires pgtype.Timestamptz, pidStatus string, now time.Time) string {
	switch {
	case expires.Valid && !expires.Time.After(now):
		return Expired
	case pidStatus != "active":
		return Revoked
	}
	return Active
}

func hash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Create makes a share link to one of the key's app's assets, with its own
// signed playback ID. An asset still processing may be shared; its link
// plays once the asset does.
func (s *Service) Create(ctx context.Context, key apikeys.Key, assetID string, req CreateRequest, idemKey string) (View, error) {
	name := strings.TrimSpace(req.Name)
	switch {
	case !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxName:
		return View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "name is limited to 100 characters")
	case strings.ContainsFunc(name, unicode.IsControl):
		return View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "name must be one line of text")
	case req.ExpiresAt != nil && !req.ExpiresAt.After(s.Now()):
		return View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "expires_at must be in the future")
	}
	expires := pgtype.Timestamptz{}
	if req.ExpiresAt != nil {
		expires = pgtype.Timestamptz{Time: req.ExpiresAt.UTC().Truncate(time.Second), Valid: true}
	}
	body, _ := json.Marshal(CreateRequest{Name: name, ExpiresAt: req.ExpiresAt})
	var linkID string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets/{id}/shares "+assetID, body)
		if err != nil {
			return idempotency.Problem(err)
		}
		// The replay keeps only the link's ID: its secret stays sealed.
		if claim != nil && claim.Replay != nil {
			var stored struct{ ID string }
			if err := json.Unmarshal(claim.Replay, &stored); err != nil {
				return err
			}
			linkID = stored.ID
			return nil
		}
		asset, pid, err := s.life.AddPlaybackIDIn(ctx, q, key, assetID, "signed")
		if err != nil {
			return err
		}
		linkID = ids.New(ids.ShareLink)
		secret := rand.Text()
		if _, err := q.CreateShareLink(ctx, db.CreateShareLinkParams{
			ID: linkID, ProjectID: asset.ProjectID, AppID: asset.AppID, AssetID: asset.ID, PlaybackID: pid.ID,
			Name: name, ExpiresAt: expires, SecretHash: hash(secret), SecretSealed: s.box.Seal(sealTable, linkID, []byte(secret)),
		}); err != nil {
			return err
		}
		stored, _ := json.Marshal(map[string]string{"id": linkID})
		return claim.Save(ctx, http.StatusCreated, stored)
	})
	if err != nil {
		return View{}, err
	}
	s.life.Publish(ctx, assetID)
	return s.Get(ctx, key, linkID)
}

// Get reads one of the key's app's links.
func (s *Service) Get(ctx context.Context, key apikeys.Key, linkID string) (View, error) {
	q := db.New(s.pool)
	l, err := q.GetShareLink(ctx, db.GetShareLinkParams{ID: linkID, AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, lifecycle.ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	base, err := s.publicURL(ctx, q, key.ProjectID)
	if err != nil {
		return View{}, err
	}
	return s.view(base, l.ID, l.AssetID, l.PlaybackID, l.Name, l.ExpiresAt, l.CreatedAt, l.SecretSealed,
		status(l.ExpiresAt, l.PlaybackStatus, s.Now()))
}

// List reads an asset's links, newest first, with their recent views.
func (s *Service) List(ctx context.Context, key apikeys.Key, assetID string) ([]View, error) {
	q := db.New(s.pool)
	if _, err := q.GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, lifecycle.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	now := s.Now().UTC()
	since := time.Date(now.Year(), now.Month(), now.Day()-(viewsDays-1), 0, 0, 0, 0, time.UTC)
	rows, err := q.ListShareLinks(ctx, db.ListShareLinksParams{AssetID: assetID, AppID: key.AppID, Since: pgtype.Date{Time: since, Valid: true}})
	if err != nil {
		return nil, err
	}
	base, err := s.publicURL(ctx, q, key.ProjectID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, l := range rows {
		v, err := s.view(base, l.ID, l.AssetID, l.PlaybackID, l.Name, l.ExpiresAt, l.CreatedAt, l.SecretSealed,
			status(l.ExpiresAt, l.PlaybackStatus, now))
		if err != nil {
			return nil, err
		}
		views := l.Views
		v.Views30d = &views
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) view(base, id, assetID, pid, name string, expires, created pgtype.Timestamptz, sealed []byte, st string) (View, error) {
	secret, err := s.box.Open(sealTable, id, sealed)
	if err != nil {
		return View{}, err
	}
	v := View{
		ID: id, AssetID: assetID, Name: name, URL: base + "/s/" + string(secret), PlaybackID: pid,
		Status: st, CreatedAt: created.Time.UTC(),
	}
	if expires.Valid {
		t := expires.Time.UTC()
		v.ExpiresAt = &t
	}
	return v, nil
}

func (s *Service) publicURL(ctx context.Context, q *db.Queries, projectID string) (string, error) {
	u, err := q.GetProjectURLs(ctx, projectID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(u.PublicUrl.String, "/"), nil
}

// Revoke stops one of the key's app's links by revoking its playback ID.
// It reports whether the unpublish is confirmed. Revoking a stopped link
// changes nothing.
func (s *Service) Revoke(ctx context.Context, key apikeys.Key, linkID string) (View, bool, error) {
	l, err := db.New(s.pool).GetShareLink(ctx, db.GetShareLinkParams{ID: linkID, AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, false, lifecycle.ErrNotFound
	}
	if err != nil {
		return View{}, false, err
	}
	_, done, err := s.life.RevokePlaybackID(ctx, key, l.AssetID, l.PlaybackID)
	if err != nil {
		return View{}, false, err
	}
	v, err := s.Get(ctx, key, linkID)
	return v, done, err
}

// StreamURL is a playback URL for other players: one token, signed with
// the internal key, that lasts until the link expires or the project's
// longest token lifetime, whichever is sooner. It can't be refreshed, and
// revoking the link ends it.
type StreamURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Stream signs a link's stream URL.
func (s *Service) Stream(ctx context.Context, key apikeys.Key, linkID string) (StreamURL, error) {
	q := db.New(s.pool)
	l, err := q.GetShareLink(ctx, db.GetShareLinkParams{ID: linkID, AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		return StreamURL{}, lifecycle.ErrNotFound
	}
	if err != nil {
		return StreamURL{}, err
	}
	now := s.Now()
	switch status(l.ExpiresAt, l.PlaybackStatus, now) {
	case Expired:
		return StreamURL{}, httpjson.Fail(http.StatusConflict, "share_expired", "the share link has expired")
	case Revoked:
		return StreamURL{}, httpjson.Fail(http.StatusConflict, "share_revoked", "the share link was revoked")
	}
	asset, err := q.GetAsset(ctx, db.GetAssetParams{ID: l.AssetID, AppID: key.AppID})
	if err != nil {
		return StreamURL{}, err
	}
	if !asset.CurrentOutputID.Valid {
		return StreamURL{}, errNotReady()
	}
	maxLife, err := q.GetProjectTokenLifetime(ctx, l.ProjectID)
	if err != nil {
		return StreamURL{}, err
	}
	tok, exp, err := s.sign(ctx, l.ProjectID, l.PlaybackID, "", now, time.Duration(maxLife)*time.Second, l.ExpiresAt, maxLife)
	if err != nil {
		return StreamURL{}, err
	}
	base, err := q.GetMediaURL(ctx, l.ProjectID)
	if err != nil {
		return StreamURL{}, err
	}
	return StreamURL{URL: base + "/v/" + tok + "/" + l.PlaybackID + "/master.m3u8", ExpiresAt: exp}, nil
}

func errNotReady() error {
	return httpjson.Fail(http.StatusConflict, "asset_not_ready", "the video is still processing")
}

// sign signs a playback token for pid with the internal key that lasts
// life, but never past the link's expiry or the project's longest token
// lifetime (maxLife seconds, which delivery enforces).
func (s *Service) sign(ctx context.Context, projectID, pid, vid string, now time.Time, life time.Duration, expires pgtype.Timestamptz, maxLife int32) (string, time.Time, error) {
	iat := now.Unix()
	exp := now.Add(life).Unix()
	if expires.Valid && expires.Time.Unix() < exp {
		exp = expires.Time.Unix()
	}
	exp = min(exp, iat+int64(maxLife))
	signer, err := signingkeys.Internal(ctx, s.pool, s.box, projectID)
	if err != nil {
		return "", time.Time{}, err
	}
	tok, err := signer.Sign(tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: iat, Exp: exp, Vid: vid})
	return tok, time.Unix(exp, 0).UTC(), err
}

// Playback is what POST /s/{secret}/playback answers: everything the page
// needs to play the link.
type Playback struct {
	Title           string         `json:"title,omitempty"`
	DurationSeconds float64        `json:"duration_seconds"`
	BaseURL         string         `json:"base_url"`
	PlaybackID      string         `json:"playback_id"`
	Token           string         `json:"token"`
	ExpiresAt       time.Time      `json:"expires_at"`
	PosterURL       string         `json:"poster_url"`
	Tracks          []assets.Track `json:"tracks"`
}

// PlaybackRequest is the optional body of POST /s/{secret}/playback.
type PlaybackRequest struct {
	ViewerID string `json:"viewer_id"`
}

var (
	errShareNotFound = httpjson.Fail(http.StatusNotFound, "share_not_found", "no such share link")
	errShareExpired  = httpjson.Fail(http.StatusGone, "share_expired", "the share link has expired")
)

// Play answers a link's playback, for anyone holding its secret.
func (s *Service) Play(ctx context.Context, secret string, req PlaybackRequest, allow func() (bool, time.Duration)) (Playback, error) {
	if secret == "" || len(secret) > 64 {
		return Playback{}, errShareNotFound
	}
	q := db.New(s.pool)
	l, err := q.GetShareBySecret(ctx, hash(secret))
	if errors.Is(err, pgx.ErrNoRows) {
		return Playback{}, errShareNotFound
	}
	if err != nil {
		return Playback{}, err
	}
	now := s.Now()
	switch {
	case l.AssetStatus == "deleted" || l.AppDisabled:
		return Playback{}, errShareNotFound
	case status(l.ExpiresAt, l.PlaybackStatus, now) == Expired:
		return Playback{}, errShareExpired
	case l.PlaybackStatus != "active":
		return Playback{}, errShareNotFound
	}
	if ok, wait := allow(); !ok {
		return Playback{}, ratelimit.Problem(wait)
	}
	asset, err := q.GetAsset(ctx, db.GetAssetParams{ID: l.AssetID, AppID: l.AppID})
	if err != nil {
		return Playback{}, err
	}
	if !asset.CurrentOutputID.Valid || !asset.DurationSeconds.Valid {
		return Playback{}, errNotReady()
	}
	v, err := assets.Load(ctx, q, asset)
	if err != nil {
		return Playback{}, err
	}
	maxLife, err := q.GetProjectTokenLifetime(ctx, l.ProjectID)
	if err != nil {
		return Playback{}, err
	}
	vid := ""
	if viewerID.MatchString(req.ViewerID) {
		vid = req.ViewerID
	}
	life := max(time.Duration(2*asset.DurationSeconds.Float64*float64(time.Second)), minPlayLifetime)
	tok, exp, err := s.sign(ctx, l.ProjectID, l.PlaybackID, vid, now, life, l.ExpiresAt, maxLife)
	if err != nil {
		return Playback{}, err
	}
	out := Playback{
		Title: v.Title, DurationSeconds: asset.DurationSeconds.Float64, BaseURL: v.BaseURL,
		PlaybackID: l.PlaybackID, Token: tok, ExpiresAt: exp,
		PosterURL: v.BaseURL + "/v/" + tok + "/" + l.PlaybackID + "/poster.jpg",
		Tracks:    []assets.Track{},
	}
	for _, t := range v.Tracks {
		if t.Status == "ready" {
			out.Tracks = append(out.Tracks, t)
		}
	}
	return out, nil
}

// Expire revokes the playback IDs of links past their expiry, at most limit a
// pass, through the same unpublish as a revoke, so their route records
// leave the Worker too. Tokens already stop at the expiry; this stops the
// ID itself.
func (s *Service) Expire(ctx context.Context, limit int) (int, error) {
	links, err := db.New(s.pool).ListExpiredShareLinks(ctx, db.ListExpiredShareLinksParams{
		Now: pgtype.Timestamptz{Time: s.Now(), Valid: true}, MaxLinks: int32(limit),
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range links {
		key := apikeys.Key{ID: "shares", ProjectID: l.ProjectID, AppID: l.AppID}
		if _, _, err := s.life.RevokePlaybackID(ctx, key, l.AssetID, l.PlaybackID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// --- handlers ---

func (s *Service) respond(w http.ResponseWriter, op string, status int, v any, err error) {
	switch {
	case err == nil:
		httpjson.Write(w, status, v)
	case errors.Is(err, lifecycle.ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "not_found", "not found")
	case httpjson.WriteProblem(w, err):
	default:
		s.Logger.Error("shares api", "op", op, "request_id", w.Header().Get("X-Request-Id"), "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var req CreateRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	v, err := s.Create(r.Context(), key, r.PathValue("id"), req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "create_share", http.StatusCreated, v, err)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsRead)
	if !ok {
		return
	}
	links, err := s.List(r.Context(), key, r.PathValue("id"))
	s.respond(w, "list_shares", http.StatusOK, map[string]any{"data": links}, err)
}

// handleRevoke answers 200 once the link's ID is unpublished, or 202 while
// it is still being unpublished.
func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	v, done, err := s.Revoke(r.Context(), key, r.PathValue("id"))
	status := http.StatusAccepted
	if done {
		status = http.StatusOK
	}
	s.respond(w, "revoke_share", status, v, err)
}

func (s *Service) handleStreamURL(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	u, err := s.Stream(r.Context(), key, r.PathValue("id"))
	s.respond(w, "share_stream_url", http.StatusOK, u, err)
}

// maxPlaybackBody bounds the public endpoint's body: a viewer ID.
const maxPlaybackBody = 1 << 10

// handlePlayback is the public endpoint. It needs no session or key; its
// refusals (404, 410) count toward delivery's refusal limiter, and its
// successes are limited per address.
func (s *Service) handlePlayback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
	var req PlaybackRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPlaybackBody))
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "body must be a JSON object")
		return
	}
	addr := ratelimit.AddrKey(s.ClientIP(r))
	p, err := s.Play(r.Context(), r.PathValue("token"), req, func() (bool, time.Duration) { return s.Plays.Take(addr) })
	var prob *httpjson.Problem
	if errors.As(err, &prob) && prob.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", prob.Details["retry_after"])
	}
	s.respond(w, "share_playback", http.StatusOK, p, err)
}
