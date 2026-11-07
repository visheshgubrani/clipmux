// Package delivery is the built-in delivery handler (docs/architecture.md,
// "Delivery"): it turns a playback ID into bytes, deciding from the route
// (Postgres here; route records in R2 for the Phase 2 Worker) rather than
// from the shape of the URL, and refusing unknown IDs.
//
//	Signed: {media}/v/{token}/{playback_id}/master.m3u8
//	Public: {media}/p/{playback_id}/master.m3u8
//
// The master is generated per request with {output_id}/ in front of every
// playlist URI, so a session stays pinned to one output across a re-encode.
// Everything under {playback_id}/{output_id}/ is immutable; the master and
// the content key are never cached. The Worker must reproduce these rules;
// testdata/vectors.json is the shared contract.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// clockSkew tolerates an app server's clock running slightly ahead.
const clockSkew = 60 * time.Second

// contentTypes is the extension allowlist; nothing else is ever served.
var contentTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".mp4":  "video/mp4",
	".m4s":  "video/iso.segment",
	".jpg":  "image/jpeg",
}

// Handler serves playback.
type Handler struct {
	pool   *pgxpool.Pool
	stores *stores.Set
	keys   contentkey.Ring
	cache  *cache
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns the built-in delivery handler.
func New(pool *pgxpool.Pool, set *stores.Set, keys contentkey.Ring) *Handler {
	h := &Handler{pool: pool, stores: set, keys: keys, Now: time.Now, Logger: slog.Default()}
	h.cache = newCache(func() time.Time { return h.Now() })
	return h
}

// Register adds the playback routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v/{token}/{pid}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, r.PathValue("token"), true)
	})
	mux.HandleFunc("GET /p/{pid}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, "", false)
	})
	for _, p := range []string{"OPTIONS /v/", "OPTIONS /p/"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			common(w)
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
			w.Header().Set("Access-Control-Allow-Headers", "Range")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// common headers: players on any origin may fetch (the URL is the
// credential, there are no cookies), and nothing is ever sniffed.
func common(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// Refusals the player acts on: 401 means fetch a new token, 403 means stop,
// 404 means the ID (or file) is unknown.
func expired(w http.ResponseWriter) {
	httpjson.Error(w, http.StatusUnauthorized, "token_expired", "fetch a new token")
}
func denied(w http.ResponseWriter) { httpjson.Error(w, http.StatusForbidden, "denied", "denied") }
func notFound(w http.ResponseWriter) {
	httpjson.Error(w, http.StatusNotFound, "not_found", "not found")
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, token string, signedURL bool) {
	common(w)
	ctx := r.Context()
	q := db.New(h.pool)
	route, err := h.cache.route(ctx, q, r.PathValue("pid"))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		notFound(w)
		return
	case err != nil:
		h.fail(w, "route", err)
		return
	case route.PlaybackStatus != "active" || route.AssetStatus == "deleted" || !route.CurrentOutputID.Valid:
		notFound(w)
		return
	}

	if route.AppDisabled {
		denied(w) // disabling an app cuts off everything it owns
		return
	}

	audience := tokens.AudiencePlayback
	if route.Policy == "signed" {
		if !signedURL {
			denied(w) // a signed ID never plays without its token
			return
		}
		aud, status := h.authorize(ctx, q, route, token)
		if status != 0 {
			if status == http.StatusUnauthorized {
				expired(w)
			} else {
				denied(w)
			}
			return
		}
		audience = aud
	}

	p := r.PathValue("path")
	current := route.CurrentOutputID.String
	store, err := h.stores.Get(ctx, route.StorageID)
	if err != nil {
		h.fail(w, "storage", err)
		return
	}
	switch {
	case p == "master.m3u8" && audience == tokens.AudiencePlayback:
		trks, err := h.cache.tracksOf(ctx, q, route.AssetID)
		if err != nil {
			h.fail(w, "tracks", err)
			return
		}
		h.master(w, r, store, route.AssetID, current, trks)
	case p == "poster.jpg":
		// Unpinned: it follows the current output, so it can't be immutable.
		h.object(w, r, store, route.AssetID, current, "poster.jpg", route.Policy, "max-age=300")
	case strings.HasPrefix(p, "tracks/"):
		// Before the output lookup, which would take "tracks" for an
		// output. A thumbnail token never reads a track.
		if audience != tokens.AudiencePlayback {
			denied(w)
			return
		}
		h.track(w, r, q, store, route, strings.TrimPrefix(p, "tracks/"))
	case audience != tokens.AudiencePlayback && !strings.HasSuffix(p, "/poster.jpg"):
		denied(w) // a thumbnail token fetches posters only
	default:
		outputID, rest, ok := strings.Cut(p, "/")
		if !ok {
			notFound(w)
			return
		}
		out, err := h.cache.output(ctx, q, outputID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (out.AssetID != route.AssetID || out.Status == "writing" || out.PurgedAt.Valid)) {
			notFound(w) // only this asset's committed or retired outputs whose files are kept
			return
		}
		if err != nil {
			h.fail(w, "output", err)
			return
		}
		if v, isKey := strings.CutPrefix(rest, "key/"); isKey {
			h.key(w, route.AssetID, out, v)
			return
		}
		if audience != tokens.AudiencePlayback && rest != "poster.jpg" {
			denied(w)
			return
		}
		h.object(w, r, store, route.AssetID, outputID, rest, route.Policy, "max-age=31536000, immutable")
	}
}

// authorize verifies a token for route. It returns the token's audience, or
// a status: 401 for an expired token, 403 for anything else wrong.
func (h *Handler) authorize(ctx context.Context, q *db.Queries, route db.GetRouteRow, token string) (string, int) {
	tok, err := tokens.Parse(token)
	if err != nil {
		return "", http.StatusForbidden
	}
	key, err := h.cache.signingKey(ctx, q, tok.Kid)
	if err != nil || key.RevokedAt.Valid || (key.RetiredAt.Valid && !key.RetiredAt.Time.After(h.Now())) {
		return "", http.StatusForbidden
	}
	// An app's key signs only its own playback IDs; the server's internal
	// key (no app) signs for any app of its own project.
	if key.ProjectID != route.ProjectID || (key.AppID.Valid && key.AppID.String != route.AppID) {
		return "", http.StatusForbidden
	}
	pub, err := tokens.ParsePublicKey(key.PublicKeyPem)
	if err != nil || tok.Verify(pub) != nil {
		return "", http.StatusForbidden
	}
	c, now := tok.Claims, h.Now()
	switch {
	case c.Sub != route.PlaybackID,
		c.Aud != tokens.AudiencePlayback && c.Aud != tokens.AudienceThumbnail,
		c.Iat <= 0 || time.Unix(c.Iat, 0).After(now.Add(clockSkew)),
		c.Exp-c.Iat > int64(route.MaxTokenLifetimeSeconds):
		return "", http.StatusForbidden
	case !time.Unix(c.Exp, 0).After(now):
		return "", http.StatusUnauthorized
	}
	return c.Aud, 0
}

// master serves the current output's master with {output_id}/ in front of
// every URI, so relative URLs resolve to .../{playback_id}/{output_id}/...
// with the token still in the path.
func (h *Handler) master(w http.ResponseWriter, r *http.Request, store storage.Storage, assetID, outputID string, trks []db.ListServableTracksRow) {
	rc, err := store.Get(r.Context(), "media/"+assetID+"/"+outputID+"/master.m3u8")
	if err != nil {
		h.fail(w, "master", err)
		return
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		h.fail(w, "master", err)
		return
	}
	w.Header().Set("Content-Type", contentTypes[".m3u8"])
	w.Header().Set("Cache-Control", "no-store")
	var subs []Subtitles
	for _, t := range trks {
		if routes.TrackOf(t.Kind, t.Status, t.Language, t.Name, t.DurationMs, t.KeyVersion).Listed {
			subs = append(subs, Subtitles{ID: t.ID, Name: t.Name.String, Language: t.Language.String})
		}
	}
	_, _ = io.WriteString(w, Master(string(raw), outputID, subs))
}

// Subtitles is a subtitles track the master lists.
type Subtitles struct{ ID, Name, Language string }

// Master is the master playlist served: RewriteMaster, then, with
// subtitles, one EXT-X-MEDIA per track (in ID order) before the first
// variant and SUBTITLES="subs" on every variant.
func Master(raw, outputID string, subs []Subtitles) string {
	m := RewriteMaster(raw, outputID)
	if len(subs) == 0 {
		return m
	}
	var media strings.Builder
	for _, s := range subs {
		fmt.Fprintf(&media, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"%s\",LANGUAGE=\"%s\",AUTOSELECT=YES,DEFAULT=NO,URI=\"tracks/%s.m3u8\"\n",
			attrValue(s.Name), attrValue(s.Language), s.ID)
	}
	var b strings.Builder
	inserted := false
	for line := range strings.Lines(m) {
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			if !inserted {
				b.WriteString(media.String())
				inserted = true
			}
			line = strings.TrimSuffix(line, "\n") + ",SUBTITLES=\"subs\"\n"
		}
		b.WriteString(line)
	}
	if !inserted {
		b.WriteString(media.String())
	}
	return b.String()
}

// attrValue keeps a quoted playlist attribute on one line and quoted.
func attrValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// SubtitlePlaylist is a subtitles track's media playlist: its whole VTT as
// one segment as long as the output. Times are written from integer
// milliseconds, so the Worker's copy matches byte for byte.
func SubtitlePlaylist(trackID string, durationMS int64) string {
	target := max((durationMS+999)/1000, 1)
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%d.%03d,\n%s.vtt\n#EXT-X-ENDLIST\n",
		target, durationMS/1000, durationMS%1000, trackID)
}

// trackPath is a track's file: its playlist or its VTT.
var trackPath = regexp.MustCompile(`^(trk_[0-9a-z_]{1,64})\.(m3u8|vtt)$`)

// maxTrack bounds a VTT read into memory to decrypt it.
const maxTrack = 64 << 20

// track serves tracks/{id}.m3u8 (subtitles only) or tracks/{id}.vtt, for
// this asset's servable tracks: the VTT decrypted (D079), text/vtt,
// immutable; the playlist never cached.
func (h *Handler) track(w http.ResponseWriter, r *http.Request, q *db.Queries, store storage.Storage, route db.GetRouteRow, rel string) {
	m := trackPath.FindStringSubmatch(rel)
	if m == nil {
		notFound(w)
		return
	}
	trks, err := h.cache.tracksOf(r.Context(), q, route.AssetID)
	if err != nil {
		h.fail(w, "tracks", err)
		return
	}
	i := slices.IndexFunc(trks, func(t db.ListServableTracksRow) bool { return t.ID == m[1] })
	if i < 0 {
		notFound(w)
		return
	}
	t := trks[i]
	if m[2] == "m3u8" {
		if t.Kind != tracks.Subtitles {
			notFound(w)
			return
		}
		w.Header().Set("Content-Type", contentTypes[".m3u8"])
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, SubtitlePlaylist(t.ID, t.DurationMs.Int64))
		return
	}
	key := tracks.StorageKey(route.AssetID, t.ID)
	rc, err := store.Get(r.Context(), key)
	if errors.Is(err, storage.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.fail(w, "track", err)
		return
	}
	defer rc.Close()
	sealed, err := io.ReadAll(io.LimitReader(rc, maxTrack))
	if err != nil {
		h.fail(w, "track", err)
		return
	}
	plain, err := h.keys.OpenTrack(route.AssetID, t.ID, key, sealed)
	if err != nil {
		h.fail(w, "track", err)
		return
	}
	scope := "public"
	if route.Policy == "signed" {
		scope = "private"
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", scope+", max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(plain)))
	_, _ = w.Write(plain)
}

// RewriteMaster prefixes every URI in a master playlist with outputID/.
func RewriteMaster(master, outputID string) string {
	var b strings.Builder
	for line := range strings.Lines(master) {
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "#"):
			trimmed = strings.ReplaceAll(trimmed, `URI="`, `URI="`+outputID+"/")
		case trimmed != "":
			trimmed = outputID + "/" + trimmed
		}
		b.WriteString(trimmed + "\n")
	}
	return b.String()
}

// key serves an output's content key: 16 bytes, derived, never stored or cached.
func (h *Handler) key(w http.ResponseWriter, assetID string, out db.Output, version string) {
	v, err := strconv.ParseInt(version, 10, 32)
	secret, ok := h.keys.Get(out.KeyVersion)
	if err != nil || int32(v) != out.KeyVersion || !ok {
		notFound(w)
		return
	}
	k := secret.Derive(assetID, out.ID)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(k[:])
}

// object streams one stored file of an output, if the grammar and the
// extension allowlist permit it.
func (h *Handler) object(w http.ResponseWriter, r *http.Request, store storage.Storage, assetID, outputID, rel, policy, cache string) {
	ctype, ok := contentTypes[path.Ext(rel)]
	if !ok || rel == "manifest.json" || !media.ValidOutputPath(rel) {
		notFound(w)
		return
	}
	rc, err := store.Get(r.Context(), "media/"+assetID+"/"+outputID+"/"+rel)
	if errors.Is(err, storage.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.fail(w, "object", err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", ctype)
	// A pinned path is never reused, so pinned files are immutable. Signed
	// bytes stay out of shared caches.
	scope := "public"
	if policy == "signed" {
		scope = "private"
	}
	w.Header().Set("Cache-Control", scope+", "+cache)
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", time.Time{}, rs) // ranges and HEAD
		return
	}
	_, _ = io.Copy(w, rc)
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.Logger.Error("delivery", "op", op, "request_id", w.Header().Get("X-Request-Id"), "err", err)
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}
