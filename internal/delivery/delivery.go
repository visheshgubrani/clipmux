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
	"encoding/json"
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

	"github.com/visheshgubrani/clipmux/internal/analytics"
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
	// Record, when set, is given each counted response (D103) after it is
	// sent, on the request's goroutine. It must not block.
	Record func(analytics.Event)
}

// EdgeHeader marks a fetch the Worker forwards to origin; the Worker has
// counted it already. Spoofing it only hides the spoofer's own requests.
const EdgeHeader = "X-ClipMux-Edge"

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

// serve answers one playback request and, when recording, counts it.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, token string, signedURL bool) {
	if h.Record == nil || r.Method != http.MethodGet || r.Header.Get(EdgeHeader) == "1" {
		h.respond(w, r, token, signedURL, &counted{})
		return
	}
	cw := &countingWriter{ResponseWriter: w}
	var c counted
	h.respond(cw, r, token, signedURL, &c)
	h.count(r.Context(), cw, &c, r.PathValue("path"))
}

// counted is what respond learns that counting needs: the route, once it
// is servable, and the token's viewer.
type counted struct {
	route    *db.GetRouteRow
	vid      string
	internal bool // signed by the server's internal key: previews, posters, the setup test
}

func (h *Handler) respond(w http.ResponseWriter, r *http.Request, token string, signedURL bool, c *counted) {
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
	c.route = &route

	audience := tokens.AudiencePlayback
	if route.Policy == "signed" {
		if !signedURL {
			denied(w) // a signed ID never plays without its token
			return
		}
		g, status := h.authorize(ctx, q, route, token)
		if status != 0 {
			if status == http.StatusUnauthorized {
				expired(w)
			} else {
				denied(w)
			}
			return
		}
		audience, c.vid, c.internal = g.aud, g.vid, g.internal
	} else if signedURL {
		// Soft-check tokens on public routes: invalid tokens still play a
		// public ID, while valid internal tokens opt out of counting and valid
		// app tokens forward their viewer ID.
		if g, status := h.authorize(ctx, q, route, token); status == 0 {
			c.vid, c.internal = g.vid, g.internal
		}
	}

	p := r.PathValue("path")
	current := route.CurrentOutputID.String
	store, err := h.stores.Get(ctx, route.MediaStorageID)
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
		if out, err := h.cache.output(ctx, q, current); err != nil || !out.lists("poster.jpg") {
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				h.fail(w, "output", err)
				return
			}
			notFound(w)
			return
		}
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
			h.key(w, route.AssetID, out.Output, v)
			return
		}
		if audience != tokens.AudiencePlayback && rest != "poster.jpg" {
			denied(w)
			return
		}
		// Only what the committed manifest lists: a worker's PUT URLs
		// outlive its commit (D124).
		if !out.lists(rest) {
			notFound(w)
			return
		}
		h.object(w, r, store, route.AssetID, outputID, rest, route.Policy, "max-age=31536000, immutable")
	}
}

// grant is what a verified token allows and says.
type grant struct {
	aud      string
	vid      string
	internal bool
}

// authorize verifies a token for route. It returns what the token grants,
// or a status: 401 for an expired token, 403 for anything else wrong.
func (h *Handler) authorize(ctx context.Context, q *db.Queries, route db.GetRouteRow, token string) (grant, int) {
	tok, err := tokens.Parse(token)
	if err != nil {
		return grant{}, http.StatusForbidden
	}
	key, err := h.cache.signingKey(ctx, q, tok.Kid)
	if err != nil || key.RevokedAt.Valid || (key.RetiredAt.Valid && !key.RetiredAt.Time.After(h.Now())) {
		return grant{}, http.StatusForbidden
	}
	// An app's key signs only its own playback IDs; the server's internal
	// key (no app) signs for any app of its own project.
	if key.ProjectID != route.ProjectID || (key.AppID.Valid && key.AppID.String != route.AppID) {
		return grant{}, http.StatusForbidden
	}
	pub, err := tokens.ParsePublicKey(key.PublicKeyPem)
	if err != nil || tok.Verify(pub) != nil {
		return grant{}, http.StatusForbidden
	}
	c, now := tok.Claims, h.Now()
	switch {
	case c.Sub != route.PlaybackID,
		c.Aud != tokens.AudiencePlayback && c.Aud != tokens.AudienceThumbnail,
		c.Iat <= 0 || time.Unix(c.Iat, 0).After(now.Add(clockSkew)),
		c.Exp-c.Iat > int64(route.MaxTokenLifetimeSeconds):
		return grant{}, http.StatusForbidden
	case !time.Unix(c.Exp, 0).After(now):
		return grant{}, http.StatusUnauthorized
	}
	// A share link's ID is signed by the internal key too, but its plays
	// are real viewers (D104), so they count.
	return grant{aud: c.Aud, vid: c.Vid, internal: !key.AppID.Valid && !route.Share}, 0
}

// count records a response under D103's rules: a GET (serve checked) on a
// servable route, answered 200 or 206, not under the internal key unless
// the route is a share link's. Views and watch time count only from the
// first byte, so a player's later ranges add bytes alone.
func (h *Handler) count(ctx context.Context, w *countingWriter, c *counted, p string) {
	if c.route == nil || c.internal || (w.status != http.StatusOK && w.status != http.StatusPartialContent) {
		return
	}
	kind, rendition, outputID := kindOf(p)
	if kind == "" {
		return
	}
	route := c.route
	vid := c.vid
	if kind == analytics.KindPoster {
		vid = ""
	}
	e := analytics.Event{
		ProjectID: route.ProjectID, AppID: route.AppID, AssetID: route.AssetID, PlaybackID: route.PlaybackID,
		Kind: kind, Bytes: w.n, Vid: vid,
	}
	if w.status == http.StatusOK || strings.HasPrefix(w.Header().Get("Content-Range"), "bytes 0-") {
		switch {
		case kind == analytics.KindMaster:
			e.Views = 1
		case kind == analytics.KindSegment && (rendition == "audio" || !h.hasAudio(ctx, outputID)):
			e.WatchSeconds = media.SegmentSeconds
		}
	}
	h.Record(e)
}

// kindOf names what a served path is. Only paths delivery answered 2xx
// reach it, so they already passed the grammar.
func kindOf(p string) (kind, rendition, outputID string) {
	switch {
	case p == "master.m3u8":
		return analytics.KindMaster, "", ""
	case p == "poster.jpg" || strings.HasSuffix(p, "/poster.jpg"):
		return analytics.KindPoster, "", ""
	case strings.HasPrefix(p, "tracks/"):
		if strings.HasSuffix(p, ".m3u8") {
			return analytics.KindTrackPlaylist, "", ""
		}
		return analytics.KindTrack, "", ""
	}
	parts := strings.Split(p, "/")
	if len(parts) != 3 {
		return "", "", ""
	}
	switch {
	case parts[1] == "key":
		kind = analytics.KindKey
	case parts[2] == "index.m3u8":
		kind = analytics.KindPlaylist
	case parts[2] == "init.mp4":
		kind = analytics.KindInit
	default:
		kind = analytics.KindSegment
	}
	return kind, parts[1], parts[0]
}

// hasAudio reports whether an output has an audio rendition, which then
// carries the watch time. An output that can't be read counts as having
// one, so its video segments add no watch time.
func (h *Handler) hasAudio(ctx context.Context, outputID string) bool {
	out, err := h.cache.output(ctx, db.New(h.pool), outputID)
	if err != nil {
		return true
	}
	var m struct {
		Audio bool `json:"audio"`
	}
	return json.Unmarshal(out.Manifest, &m) != nil || m.Audio
}

// countingWriter counts the body bytes sent and keeps the status.
type countingWriter struct {
	http.ResponseWriter
	status int
	n      int64
}

func (w *countingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// ReadFrom keeps the underlying writer's sendfile path for stored files.
func (w *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.n += n
	return n, err
}

func (w *countingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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

type rangeReader interface {
	GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error)
}

var byteRangeRE = regexp.MustCompile(`^bytes=(\d*)-(\d*)$`)

func parseRange(header string, size int64) (start, end int64, ok, unsatisfiable bool) {
	m := byteRangeRE.FindStringSubmatch(strings.TrimSpace(header))
	if m == nil || (m[1] == "" && m[2] == "") {
		return 0, 0, false, false
	}
	if size <= 0 {
		return 0, 0, false, true
	}
	if m[1] == "" {
		n, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return 0, size - 1, true, false
			}
			return 0, 0, false, false
		}
		if n == 0 {
			return 0, 0, false, true
		}
		return max(int64(0), size-n), size - 1, true, false
	}
	start, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || start >= size {
		return 0, 0, false, true
	}
	if m[2] == "" {
		return start, size - 1, true, false
	}
	end, err = strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return start, size - 1, true, false
		}
		return 0, 0, false, false
	}
	if end < start {
		return 0, 0, false, false
	}
	return start, min(end, size-1), true, false
}

// object streams one stored file of an output, if the grammar and the
// extension allowlist permit it.
func (h *Handler) object(w http.ResponseWriter, r *http.Request, store storage.Storage, assetID, outputID, rel, policy, cache string) {
	ctype, ok := contentTypes[path.Ext(rel)]
	if !ok || rel == "manifest.json" || !media.ValidOutputPath(rel) {
		notFound(w)
		return
	}
	key := "media/" + assetID + "/" + outputID + "/" + rel
	// A pinned path is never reused, so pinned files are immutable. Signed
	// bytes stay out of shared caches.
	scope := "public"
	if policy == "signed" {
		scope = "private"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", scope+", "+cache)

	if rr, ok := store.(rangeReader); ok {
		obj, err := store.Head(r.Context(), key)
		if errors.Is(err, storage.ErrNotFound) {
			notFound(w)
			return
		}
		if err != nil {
			h.fail(w, "object", err)
			return
		}
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng != "" {
			start, end, ok, unsatisfiable := parseRange(rng, obj.Size)
			if unsatisfiable {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", obj.Size))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if ok {
				length := end - start + 1
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, obj.Size))
				w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
				if r.Method == http.MethodHead {
					w.WriteHeader(http.StatusPartialContent)
					return
				}
				rc, _, err := rr.GetRange(r.Context(), key, start, end)
				if errors.Is(err, storage.ErrNotFound) {
					notFound(w)
					return
				}
				if err != nil {
					h.fail(w, "object", err)
					return
				}
				defer rc.Close()
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.Copy(w, rc)
				return
			}
		}
		w.Header().Set("Content-Length", strconv.FormatInt(obj.Size, 10))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		rc, err := store.Get(r.Context(), key)
		if errors.Is(err, storage.ErrNotFound) {
			notFound(w)
			return
		}
		if err != nil {
			h.fail(w, "object", err)
			return
		}
		defer rc.Close()
		_, _ = io.Copy(w, rc)
		return
	}

	rc, err := store.Get(r.Context(), key)
	if errors.Is(err, storage.ErrNotFound) {
		notFound(w)
		return
	}
	if err != nil {
		h.fail(w, "object", err)
		return
	}
	defer rc.Close()
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
