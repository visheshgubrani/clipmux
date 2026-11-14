package shares_test

// Real Postgres, filesystem storage and built-in delivery, driven over HTTP.
// Expected behaviour comes from D104: a link plays without a session, its
// tokens never outlive it, and revoking or expiring it stops it.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/analytics"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/ratelimit"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/shares"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio/index.m3u8",GROUP-ID="a",NAME="x"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="a"
360p/index.m3u8
`

// t0 is the rig's clock: whole seconds, so token times compare exactly.
var t0 = time.Now().UTC().Truncate(time.Second)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *storage.Filesystem
	svc    *shares.Service
	url    string
	key    string // assets:read and assets:write for app_dashboard
	other  string // the same for app_other
	now    time.Time
	mu     sync.Mutex
	events []analytics.Event
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	box, err := secrets.NewBox(1, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
		t.Fatal(err)
	}
	committer := outputs.New(pool, queue.New(pool), stores.Fixed(store))
	life := lifecycle.New(pool, stores.Fixed(store), committer)
	r := &rig{t: t, pool: pool, store: store, now: t0}
	svc := shares.New(pool, box, life)
	svc.Now = r.clock
	r.svc = svc
	keys, err := contentkey.FromHex(1, strings.Repeat("00", 32))
	if err != nil {
		t.Fatal(err)
	}
	playback := delivery.New(pool, stores.Fixed(store), contentkey.NewRing(keys))
	playback.Now = r.clock
	playback.Record = func(e analytics.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
	}
	committer.OnPublished, committer.OnKeys = playback.Invalidate, playback.InvalidateKeys
	mux := http.NewServeMux()
	assets.New(pool).Register(mux)
	life.Register(mux)
	svc.Register(mux)
	playback.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r.url = srv.URL
	r.exec(`UPDATE projects SET public_url = 'https://clipmux.example', media_url = $1`, srv.URL)
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_other', 'prj_default', 'Other')`)
	scopes := []string{apikeys.AssetsRead, apikeys.AssetsWrite}
	if r.key, _, err = apikeys.Create(ctx, pool, "prj_default", "app_dashboard", "backend", scopes); err != nil {
		t.Fatal(err)
	}
	if r.other, _, err = apikeys.Create(ctx, pool, "prj_default", "app_other", "backend", scopes); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) scalar(sql string, args ...any) any {
	r.t.Helper()
	var v any
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// ready makes ast_1, "Board meeting", a ready asset of duration seconds
// with a public playback ID and its current output's files.
func (r *rig) ready(duration float64) {
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, title) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'Board meeting')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{"audio": true}')`)
	r.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_1', duration_seconds = $1 WHERE id = 'ast_1'`, duration)
	r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_pub', 'prj_default', 'ast_1', 'public')`)
	if err := r.store.Put(context.Background(), "media/ast_1/out_1/master.m3u8", strings.NewReader(master)); err != nil {
		r.t.Fatal(err)
	}
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (r *rig) send(method, path, bearer string, body any, idemKey string) reply {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	return r.do(req)
}

func (r *rig) do(req *http.Request) reply {
	r.t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return reply{status: res.StatusCode, header: res.Header, body: out, raw: string(raw)}
}

func (r *rig) get(rawURL string) reply {
	r.t.Helper()
	req, _ := http.NewRequest("GET", rawURL, nil)
	return r.do(req)
}

func code(b map[string]any) string {
	e, _ := b["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

var linkURL = regexp.MustCompile(`^https://clipmux\.example/s/([A-Z2-7]{26})$`)

// share creates a link to ast_1 and returns its body and secret.
func (r *rig) share(body map[string]any) (map[string]any, string) {
	r.t.Helper()
	got := r.send("POST", "/v1/assets/ast_1/shares", r.key, body, "")
	if got.status != http.StatusCreated {
		r.t.Fatalf("create share = %d %s", got.status, got.raw)
	}
	m := linkURL.FindStringSubmatch(got.body["url"].(string))
	if m == nil {
		r.t.Fatalf("share url = %v, want https://clipmux.example/s/{26 base32 characters}", got.body["url"])
	}
	return got.body, m[1]
}

func (r *rig) play(secret string, body any) reply {
	r.t.Helper()
	return r.send("POST", "/s/"+secret+"/playback", "", body, "")
}

func TestAShareLinkPlaysWithoutASessionAndCounts(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	expires := t0.Add(48 * time.Hour)
	link, secret := r.share(map[string]any{"name": "  For the board  ", "expires_at": expires})
	pid, _ := link["playback_id"].(string)
	if !strings.HasPrefix(pid, "pid_") || pid == "pid_pub" || link["name"] != "For the board" || link["status"] != "active" ||
		link["asset_id"] != "ast_1" || link["expires_at"] != expires.Format(time.RFC3339) || !strings.HasPrefix(link["id"].(string), "shr_") {
		t.Fatalf("link = %v", link)
	}
	if n := r.scalar(`SELECT count(*) FROM playback_ids WHERE id = $1 AND policy = 'signed' AND status = 'active'`, pid); n != int64(1) {
		t.Fatalf("the link's own signed playback ID: %v rows", n)
	}
	// The secret is kept only as its hash and sealed.
	if n := r.scalar(`SELECT count(*) FROM share_links WHERE position($1::bytea IN secret_sealed) > 0`, secret); n != int64(0) {
		t.Fatal("the secret is stored in the clear")
	}

	got := r.play(secret, map[string]any{"viewer_id": "browser_0123456789abcdef"})
	if got.status != http.StatusOK || got.header.Get("Cache-Control") != "no-store" || got.header.Get("Referrer-Policy") != "no-referrer" || got.header.Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("playback = %d %v %s", got.status, got.header, got.raw)
	}
	tok, _ := got.body["token"].(string)
	parsed, err := tokens.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	var internal string
	_ = r.pool.QueryRow(context.Background(), `SELECT id FROM signing_keys WHERE app_id IS NULL`).Scan(&internal)
	// 30 s of video: the 4 h minimum, inside the link's 48 h.
	want := tokens.Claims{Sub: pid, Aud: "v", Iat: t0.Unix(), Exp: t0.Add(4 * time.Hour).Unix(), Vid: "browser_0123456789abcdef"}
	if parsed.Kid != internal || parsed.Claims != want {
		t.Fatalf("token kid %s claims %+v, want the internal key and %+v", parsed.Kid, parsed.Claims, want)
	}
	if got.body["title"] != "Board meeting" || got.body["duration_seconds"] != 30.0 || got.body["base_url"] != r.url ||
		got.body["playback_id"] != pid || got.body["expires_at"] != t0.Add(4*time.Hour).Format(time.RFC3339) ||
		len(got.body["tracks"].([]any)) != 0 {
		t.Fatalf("playback body = %s", got.raw)
	}
	// The poster has its own token, good for the poster only (docs/api.md).
	poster, _ := got.body["poster_url"].(string)
	prefix, suffix := r.url+"/v/", "/"+pid+"/poster.jpg"
	if !strings.HasPrefix(poster, prefix) || !strings.HasSuffix(poster, suffix) {
		t.Fatalf("poster_url = %q", poster)
	}
	posterTok, err := tokens.Parse(strings.TrimSuffix(strings.TrimPrefix(poster, prefix), suffix))
	if err != nil {
		t.Fatal(err)
	}
	if wantPoster := (tokens.Claims{Sub: pid, Aud: "t", Iat: want.Iat, Exp: want.Exp}); posterTok.Kid != internal || posterTok.Claims != wantPoster {
		t.Fatalf("poster token kid %s claims %+v, want %+v", posterTok.Kid, posterTok.Claims, wantPoster)
	}

	// It plays through delivery, and the play counts though the internal key
	// signed it (D103).
	if res := r.get(r.url + "/v/" + tok + "/" + pid + "/master.m3u8"); res.status != http.StatusOK {
		t.Fatalf("master through the share token = %d %s", res.status, res.raw)
	}
	r.mu.Lock()
	events := r.events
	r.mu.Unlock()
	if len(events) != 1 || events[0].PlaybackID != pid || events[0].Views != 1 || events[0].Vid != "browser_0123456789abcdef" || events[0].AssetID != "ast_1" {
		t.Fatalf("recorded %+v, want one view of %s by browser_0123456789abcdef", events, pid)
	}

	// The asset's view labels the link's ID.
	view := r.send("GET", "/v1/assets/ast_1", r.key, nil, "")
	labelled := 0
	for _, p := range view.body["playback_ids"].([]any) {
		m := p.(map[string]any)
		if s, ok := m["share"].(map[string]any); ok && m["id"] == pid && s["id"] == link["id"] && s["name"] == "For the board" {
			labelled++
		} else if ok {
			t.Errorf("playback ID %v labelled %v", m["id"], s)
		}
	}
	if labelled != 1 {
		t.Fatalf("asset view = %s, want the link's ID labelled", view.raw)
	}

	// The listing shows the same link, with its URL again and its views.
	r.exec(`INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes) VALUES
	          ('prj_default', 'origin', $1, 'app_dashboard', 'ast_1', $2, 4, 0, 0),
	          ('prj_default', 'edge', $1, 'app_dashboard', 'ast_1', $2, 3, 0, 0),
	          ('prj_default', 'origin', $3, 'app_dashboard', 'ast_1', $2, 50, 0, 0),
	          ('prj_default', 'origin', $1, 'app_dashboard', 'ast_1', 'pid_pub', 9, 0, 0)`,
		t0.AddDate(0, 0, -29), pid, t0.AddDate(0, 0, -30))
	list := r.send("GET", "/v1/assets/ast_1/shares", r.key, nil, "")
	data, _ := list.body["data"].([]any)
	if list.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("list = %d %s", list.status, list.raw)
	}
	if l := data[0].(map[string]any); l["url"] != link["url"] || l["status"] != "active" || l["views_30d"] != 7.0 {
		t.Fatalf("listed link = %v, want its url, active, 7 views in 30 days", l)
	}
	// Disabling the app cuts off its links too.
	r.exec(`UPDATE apps SET disabled_at = now() WHERE id = 'app_dashboard'`)
	if got := r.play(secret, nil); got.status != http.StatusNotFound || code(got.body) != "share_not_found" {
		t.Fatalf("playback of a disabled app's link = %d %s", got.status, got.raw)
	}
}

func TestATokenNeverOutlivesItsLink(t *testing.T) {
	r := newRig(t)
	r.ready(3 * 3600)
	// Three hours of video: twice that, six hours.
	_, open := r.share(nil)
	got := r.play(open, nil)
	parsed, err := tokens.Parse(got.body["token"].(string))
	if err != nil || parsed.Claims.Exp != t0.Add(6*time.Hour).Unix() || parsed.Claims.Vid != "" {
		t.Fatalf("token for a 3 h video = %+v (%v), want 6 h and no vid", parsed.Claims, err)
	}
	// A link expiring sooner caps it.
	_, soon := r.share(map[string]any{"expires_at": t0.Add(time.Hour)})
	got = r.play(soon, map[string]any{"viewer_id": "short"})
	parsed, err = tokens.Parse(got.body["token"].(string))
	if err != nil || parsed.Claims.Exp != t0.Add(time.Hour).Unix() || parsed.Claims.Vid != "" {
		t.Fatalf("token for a link expiring in 1 h = %+v (%v), want 1 h and the malformed viewer ID dropped", parsed.Claims, err)
	}
	// And the project's longest lifetime caps everything.
	r.exec(`UPDATE projects SET max_token_lifetime_seconds = 7200`)
	got = r.play(open, nil)
	parsed, err = tokens.Parse(got.body["token"].(string))
	if err != nil || parsed.Claims.Exp != t0.Add(2*time.Hour).Unix() {
		t.Fatalf("token under a 2 h project maximum = %+v (%v)", parsed.Claims, err)
	}
}

func TestRevokingALinkStopsItAndNothingElse(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	link, secret := r.share(map[string]any{"name": "Leaked"})
	tok := r.play(secret, nil).body["token"].(string)
	pid := link["playback_id"].(string)

	if got := r.send("DELETE", "/v1/shares/"+link["id"].(string), r.other, nil, ""); got.status != http.StatusNotFound {
		t.Fatalf("another app revoking = %d, want 404", got.status)
	}
	got := r.send("DELETE", "/v1/shares/"+link["id"].(string), r.key, nil, "")
	if got.status != http.StatusOK || got.body["status"] != "revoked" {
		t.Fatalf("revoke = %d %s, want 200 revoked", got.status, got.raw)
	}
	if got := r.play(secret, nil); got.status != http.StatusNotFound || code(got.body) != "share_not_found" {
		t.Fatalf("playback after revoke = %d %s, want 404 share_not_found", got.status, got.raw)
	}
	if res := r.get(r.url + "/v/" + tok + "/" + pid + "/master.m3u8"); res.status != http.StatusNotFound {
		t.Fatalf("an issued token after revoke = %d, want 404", res.status)
	}
	if got := r.send("POST", "/v1/shares/"+link["id"].(string)+"/stream-url", r.key, nil, ""); got.status != http.StatusConflict || code(got.body) != "share_revoked" {
		t.Fatalf("stream URL of a revoked link = %d %s", got.status, got.raw)
	}
	// Again changes nothing; the asset's other ID still plays.
	if got := r.send("DELETE", "/v1/shares/"+link["id"].(string), r.key, nil, ""); got.status != http.StatusOK || got.body["status"] != "revoked" {
		t.Fatalf("second revoke = %d %s", got.status, got.raw)
	}
	if res := r.get(r.url + "/p/pid_pub/master.m3u8"); res.status != http.StatusOK {
		t.Fatalf("the public ID after revoking a link = %d", res.status)
	}
}

func TestAnExpiredLinkAnswers410AndTheLoopRevokesItsID(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	link, secret := r.share(map[string]any{"expires_at": t0.Add(time.Hour)})
	ctx := context.Background()
	r.advance(time.Hour - time.Second)
	if n, err := r.svc.Expire(ctx, 100); n != 0 || err != nil {
		t.Fatalf("expire a second early = %d %v, want 0", n, err)
	}
	r.advance(time.Second)
	if got := r.play(secret, nil); got.status != http.StatusGone || code(got.body) != "share_expired" {
		t.Fatalf("playback at expiry = %d %s, want 410 share_expired", got.status, got.raw)
	}
	if got := r.send("POST", "/v1/shares/"+link["id"].(string)+"/stream-url", r.key, nil, ""); got.status != http.StatusConflict || code(got.body) != "share_expired" {
		t.Fatalf("stream URL of an expired link = %d %s", got.status, got.raw)
	}
	if n, err := r.svc.Expire(ctx, 100); n != 1 || err != nil {
		t.Fatalf("expire = %d %v, want 1", n, err)
	}
	if n, err := r.svc.Expire(ctx, 100); n != 0 || err != nil {
		t.Fatalf("second expire = %d %v, want 0", n, err)
	}
	if st := r.scalar(`SELECT status FROM playback_ids WHERE id = $1`, link["playback_id"]); st != "revoked" {
		t.Fatalf("the expired link's ID is %v, want revoked", st)
	}
	// Stopped by its expiry, it still reads expired, not revoked.
	if l := r.send("GET", "/v1/assets/ast_1/shares", r.key, nil, "").body["data"].([]any)[0].(map[string]any); l["status"] != "expired" {
		t.Fatalf("listed status = %v, want expired", l["status"])
	}
	if got := r.play(secret, nil); got.status != http.StatusGone {
		t.Fatalf("playback after the loop = %d, want 410", got.status)
	}
}

func TestStreamURLPlaysInOtherPlayers(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	link, _ := r.share(map[string]any{"expires_at": t0.Add(48 * time.Hour)})
	got := r.send("POST", "/v1/shares/"+link["id"].(string)+"/stream-url", r.key, nil, "")
	if got.status != http.StatusOK || got.body["expires_at"] != t0.Add(48*time.Hour).Format(time.RFC3339) {
		t.Fatalf("stream url = %d %s, want the link's expiry", got.status, got.raw)
	}
	u := got.body["url"].(string)
	if res := r.get(u); res.status != http.StatusOK || !strings.HasPrefix(u, r.url+"/v/") || !strings.HasSuffix(u, "/"+link["playback_id"].(string)+"/master.m3u8") {
		t.Fatalf("stream url %s = %d", u, res.status)
	}
	// With no expiry, the project's longest token: 7 days by default.
	open, _ := r.share(nil)
	if got := r.send("POST", "/v1/shares/"+open["id"].(string)+"/stream-url", r.key, nil, ""); got.body["expires_at"] != t0.Add(7*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("stream url without expiry = %s", got.raw)
	}
}

func TestCreatingALink(t *testing.T) {
	r := newRig(t)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	for name, body := range map[string]map[string]any{
		"a long name":      {"name": strings.Repeat("é", 101)},
		"two lines":        {"name": "a\nb"},
		"a past expiry":    {"expires_at": t0.Add(-time.Minute)},
		"an expiry of now": {"expires_at": t0},
	} {
		if got := r.send("POST", "/v1/assets/ast_1/shares", r.key, body, ""); got.status != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, got.status, got.raw)
		}
	}
	if got := r.send("POST", "/v1/assets/ast_1/shares", r.other, nil, ""); got.status != http.StatusNotFound {
		t.Errorf("another app's asset = %d, want 404", got.status)
	}
	if got := r.send("GET", "/v1/assets/ast_1/shares", r.other, nil, ""); got.status != http.StatusNotFound {
		t.Errorf("listing another app's asset = %d, want 404", got.status)
	}

	// Idempotent: the same key answers the same link, with its URL.
	first := r.send("POST", "/v1/assets/ast_1/shares", r.key, map[string]any{"name": "Once"}, "idem-1")
	again := r.send("POST", "/v1/assets/ast_1/shares", r.key, map[string]any{"name": "Once"}, "idem-1")
	if first.status != http.StatusCreated || again.status != http.StatusCreated || first.body["id"] != again.body["id"] || first.body["url"] != again.body["url"] {
		t.Fatalf("idempotent create = %s then %s", first.raw, again.raw)
	}
	if n := r.scalar(`SELECT count(*) FROM share_links`); n != int64(1) {
		t.Fatalf("%v links, want 1", n)
	}
	// A processing asset may be shared; its link waits for it.
	secret := linkURL.FindStringSubmatch(first.body["url"].(string))[1]
	if got := r.play(secret, nil); got.status != http.StatusConflict || code(got.body) != "asset_not_ready" {
		t.Fatalf("playback while processing = %d %s, want 409 asset_not_ready", got.status, got.raw)
	}
	if got := r.play("NOTASECRETNOTASECRETNOTASE", nil); got.status != http.StatusNotFound || code(got.body) != "share_not_found" {
		t.Fatalf("an unknown secret = %d %s", got.status, got.raw)
	}
}

func TestDeletingTheAssetEndsItsLinks(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	link, secret := r.share(nil)
	if got := r.send("DELETE", "/v1/assets/ast_1", r.key, nil, ""); got.status != http.StatusOK && got.status != http.StatusAccepted {
		t.Fatalf("delete asset = %d %s", got.status, got.raw)
	}
	if got := r.play(secret, nil); got.status != http.StatusNotFound || code(got.body) != "share_not_found" {
		t.Fatalf("playback of a deleted asset's link = %d %s", got.status, got.raw)
	}
	if l := r.send("GET", "/v1/assets/ast_1/shares", r.key, nil, "").body["data"].([]any)[0].(map[string]any); l["status"] != "revoked" || l["id"] != link["id"] {
		t.Fatalf("listed status = %v, want revoked", l)
	}
	if got := r.send("POST", "/v1/assets/ast_1/shares", r.key, nil, ""); got.status != http.StatusConflict || code(got.body) != "asset_deleted" {
		t.Fatalf("sharing a deleted asset = %d %s", got.status, got.raw)
	}
}

func TestPlaysAreLimitedPerAddress(t *testing.T) {
	r := newRig(t)
	r.ready(30)
	r.svc.Plays = ratelimit.NewBuckets(0.001, 2)
	_, secret := r.share(nil)
	for i := range 2 {
		if got := r.play(secret, nil); got.status != http.StatusOK {
			t.Fatalf("play %d = %d", i+1, got.status)
		}
	}
	got := r.play(secret, nil)
	if got.status != http.StatusTooManyRequests || got.header.Get("Retry-After") == "" || code(got.body) != "rate_limited" {
		t.Fatalf("third play = %d %v %s, want 429 with Retry-After", got.status, got.header, got.raw)
	}
	// Refusals don't spend the budget: they are delivery's refusal
	// limiter's to count.
	if got := r.play("NOTASECRETNOTASECRETNOTASE", nil); got.status != http.StatusNotFound {
		t.Fatalf("an unknown secret while limited = %d, want 404", got.status)
	}
}
