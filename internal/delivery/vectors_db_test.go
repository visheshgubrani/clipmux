package delivery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/analytics"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

var (
	update       = flag.Bool("update", false, "regenerate test keys and tokens in testdata/vectors.json")
	updateBucket = flag.Bool("update-bucket", false, "regenerate testdata/worker-bucket.json from the route publisher")
)

const vectorsFile = "testdata/vectors.json"

type vectors struct {
	Description             string             `json:"description"`
	Now                     int64              `json:"now"`
	ContentKeySecretHex     string             `json:"content_key_secret_hex"`
	ContentKeyVersion       int32              `json:"content_key_version"`
	MaxTokenLifetimeSeconds int                `json:"max_token_lifetime_seconds"`
	Apps                    []string           `json:"apps"`
	DisabledApps            []string           `json:"disabled_apps,omitempty"`
	SigningKeys             []vKey             `json:"signing_keys"`
	Assets                  []vAsset           `json:"assets"`
	Outputs                 []vOutput          `json:"outputs"`
	PlaybackIDs             []vPlaybackID      `json:"playback_ids"`
	Tracks                  []vTrack           `json:"tracks,omitempty"`
	Files                   map[string]string  `json:"files"`
	TokenClaims             map[string]vClaims `json:"token_claims"`
	Tokens                  map[string]string  `json:"tokens,omitempty"`
	Cases                   []vCase            `json:"cases"`
	// AnalyticsKeyHex and ViewerKeys pin analytics.ViewerKey (D103).
	AnalyticsKeyHex string       `json:"analytics_key_hex"`
	ViewerKeys      []vViewerKey `json:"viewer_keys"`
}

type vViewerKey struct {
	AppID  string `json:"app_id"`
	Vid    string `json:"vid"`
	KeyHex string `json:"key_hex"`
}

type vKey struct {
	Kid           string `json:"kid"`
	AppID         string `json:"app_id"`
	Revoked       bool   `json:"revoked,omitempty"`
	Retired       bool   `json:"retired,omitempty"`
	PublicKeyPEM  string `json:"public_key_pem,omitempty"`
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
}

type vAsset struct {
	ID, AppID, Status, CurrentOutputID string
}

func (a *vAsset) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID              string `json:"id"`
		AppID           string `json:"app_id"`
		Status          string `json:"status"`
		CurrentOutputID string `json:"current_output_id"`
	}
	err := json.Unmarshal(b, &raw)
	*a = vAsset{raw.ID, raw.AppID, raw.Status, raw.CurrentOutputID}
	return err
}

func (a vAsset) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"id": a.ID, "app_id": a.AppID, "status": a.Status, "current_output_id": a.CurrentOutputID})
}

type vOutput struct {
	ID         string `json:"id"`
	AssetID    string `json:"asset_id"`
	Status     string `json:"status"`
	KeyVersion int32  `json:"key_version"`
	// NoAudio marks an output whose manifest says it has no audio, so its
	// video segments carry the watch time.
	NoAudio bool `json:"no_audio,omitempty"`
}

type vPlaybackID struct {
	ID      string `json:"id"`
	AssetID string `json:"asset_id"`
	Policy  string `json:"policy"`
	Status  string `json:"status"`
	// Share makes it a share link's own ID (D104).
	Share bool `json:"share,omitempty"`
}

// vTrack is a track row and its file: VTT is the plaintext delivery must
// serve, SealedHex what storage holds (sealed once by -update, then fixed,
// so the Worker's tests decrypt the same bytes).
type vTrack struct {
	ID         string `json:"id"`
	AssetID    string `json:"asset_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Language   string `json:"language"`
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	KeyVersion int32  `json:"key_version"`
	VTT        string `json:"vtt,omitempty"`
	SealedHex  string `json:"sealed_hex,omitempty"`
}

type vClaims struct {
	Kid string `json:"kid"`
	Sub string `json:"sub"`
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Vid string `json:"vid,omitempty"`
	// Raw, when set, is signed as the payload verbatim (for claim-parsing cases).
	Raw string `json:"raw,omitempty"`
}

type vCase struct {
	Name string `json:"name"`
	// Method is GET unless set (HEAD).
	Method string `json:"method,omitempty"`
	// Range, when set, is sent as the Range header.
	Range        string `json:"range,omitempty"`
	Path         string `json:"path"`
	Status       int    `json:"status"`
	ContentType  string `json:"content_type,omitempty"`
	CacheControl string `json:"cache_control,omitempty"`
	Body         string `json:"body,omitempty"`
	BodyHex      string `json:"body_hex,omitempty"`
	// Analytics is the event delivery records for the response; nil means
	// it records none.
	Analytics *vEvent `json:"analytics,omitempty"`
}

// vEvent is analytics.Event as the vectors spell it.
type vEvent struct {
	AppID        string `json:"app_id"`
	AssetID      string `json:"asset_id"`
	PlaybackID   string `json:"playback_id"`
	Kind         string `json:"kind"`
	Bytes        int64  `json:"bytes"`
	Views        int64  `json:"views"`
	WatchSeconds int64  `json:"watch_seconds"`
	Vid          string `json:"vid,omitempty"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// regenerate fills in test keys (if missing) and re-signs every token.
func regenerate(t *testing.T, v *vectors) {
	t.Helper()
	privates := map[string]string{}
	for i, k := range v.SigningKeys {
		if k.PrivateKeyPEM == "" {
			priv, pub, err := tokens.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			v.SigningKeys[i].PrivateKeyPEM, v.SigningKeys[i].PublicKeyPEM = priv, pub
		}
		privates[k.Kid] = v.SigningKeys[i].PrivateKeyPEM
	}
	// A key nobody registered, for the unknown-kid case.
	stray, _, _ := tokens.GenerateKey()
	v.Tokens = map[string]string{}
	for name, c := range v.TokenClaims {
		pem := privates[c.Kid]
		if pem == "" {
			pem = stray
		}
		priv, err := tokens.ParsePrivateKey(pem)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := tokens.Sign(priv, c.Kid, tokens.Claims{Sub: c.Sub, Aud: c.Aud, Iat: c.Iat, Exp: c.Exp, Vid: c.Vid})
		if err != nil {
			t.Fatal(err)
		}
		if c.Raw != "" {
			tok = signRaw(t, priv, c.Kid, c.Raw)
		}
		v.Tokens[name] = tok
	}
	// Flip one signature byte of a valid token.
	sig := []byte(v.Tokens["v"])
	sig[len(sig)-5] ^= 1
	if sig[len(sig)-5] == '-' || sig[len(sig)-5] == '_' {
		sig[len(sig)-5] = 'A'
	}
	v.Tokens["tampered"] = string(sig)
	// The same signature spelled differently: the last character's unused
	// low bits changed. Delivery must accept exactly one spelling.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	canonical := v.Tokens["v"]
	last := strings.IndexByte(alphabet, canonical[len(canonical)-1])
	v.Tokens["noncanonical"] = canonical[:len(canonical)-1] + string(alphabet[last^1])
	// The other ECDSA form of the same signature: (r, n-s).
	parts := strings.Split(canonical, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sv := new(big.Int).SetBytes(raw[32:])
	sv.Sub(elliptic.P256().Params().N, sv)
	sv.FillBytes(raw[32:])
	v.Tokens["alt_form"] = parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(raw)
	secret, err := contentkey.FromHex(v.ContentKeyVersion, v.ContentKeySecretHex)
	if err != nil {
		t.Fatal(err)
	}
	for i, tr := range v.Tracks {
		if tr.VTT == "" || tr.SealedHex != "" {
			continue
		}
		sealed, err := contentkey.SealTrack(secret, tr.AssetID, tr.ID, tracks.StorageKey(tr.AssetID, tr.ID), []byte(tr.VTT))
		if err != nil {
			t.Fatal(err)
		}
		v.Tracks[i].SealedHex = hex.EncodeToString(sealed)
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(vectorsFile, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVectors(t *testing.T) {
	v := loadVectors(t)
	if *update {
		regenerate(t, &v)
	}
	if len(v.Tokens) == 0 {
		t.Fatal("testdata/vectors.json has no tokens; run with -update")
	}
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`UPDATE projects SET max_token_lifetime_seconds = $1`, v.MaxTokenLifetimeSeconds)
	for _, app := range v.Apps {
		exec(`INSERT INTO apps (id, project_id, name) VALUES ($1, 'prj_default', $1)`, app)
	}
	for _, k := range v.SigningKeys {
		if k.AppID == "" { // the server's internal key keeps its (encrypted) private key
			exec(`INSERT INTO signing_keys (id, project_id, public_key_pem, private_key_encrypted) VALUES ($1, 'prj_default', $2, 'sealed')`, k.Kid, k.PublicKeyPEM)
		} else {
			exec(`INSERT INTO signing_keys (id, project_id, app_id, public_key_pem) VALUES ($1, 'prj_default', $2, $3)`, k.Kid, k.AppID, k.PublicKeyPEM)
		}
		if k.Revoked {
			exec(`UPDATE signing_keys SET revoked_at = now() WHERE id = $1`, k.Kid)
		}
		if k.Retired {
			exec(`UPDATE signing_keys SET retired_at = to_timestamp($1) WHERE id = $2`, v.Now-86400, k.Kid)
		}
	}
	for _, app := range v.DisabledApps {
		exec(`UPDATE apps SET disabled_at = now() WHERE id = $1`, app)
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, a := range v.Assets {
		exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', $2, 'processing', 'sto_default')`, a.ID, a.AppID)
	}
	for _, o := range v.Outputs {
		manifest := any(nil)
		if o.Status != "writing" {
			manifest = fmt.Sprintf(`{"audio": %t}`, !o.NoAudio)
		}
		exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ($1, 'prj_default', $2, $3, $4, $5)`, o.ID, o.AssetID, o.KeyVersion, o.Status, manifest)
		for name, body := range v.Files {
			if err := store.Put(ctx, "media/"+o.AssetID+"/"+o.ID+"/"+name, strings.NewReader(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, a := range v.Assets {
		if a.CurrentOutputID != "" {
			exec(`UPDATE assets SET current_output_id = $1, status = $2 WHERE id = $3`, a.CurrentOutputID, a.Status, a.ID)
		}
	}
	for _, p := range v.PlaybackIDs {
		exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', $2, $3)`, p.ID, p.AssetID, p.Policy)
		if p.Status == "revoked" {
			exec(`UPDATE playback_ids SET status = 'revoked', revoked_at = now() WHERE id = $1`, p.ID)
		}
		if p.Share {
			exec(`INSERT INTO share_links (id, project_id, app_id, asset_id, playback_id, secret_hash, secret_sealed)
				SELECT 'shr_' || $1, 'prj_default', a.app_id, a.id, $1, sha256($1::bytea), '\x00' FROM assets a WHERE a.id = $2`, p.ID, p.AssetID)
		}
	}
	for _, tr := range v.Tracks {
		exec(`INSERT INTO tracks (id, project_id, asset_id, kind, status, language, name, duration_ms, key_version, error_code, retired_at, deleted_at)
			VALUES ($1, 'prj_default', $2, $3, $4, nullif($5, ''), nullif($6, ''), $7, $8,
				CASE WHEN $4 = 'errored' THEN 'transcribe_failed' END, CASE WHEN $4 = 'retired' THEN now() END, CASE WHEN $4 = 'deleted' THEN now() END)`,
			tr.ID, tr.AssetID, tr.Kind, tr.Status, tr.Language, tr.Name, tr.DurationMS, tr.KeyVersion)
		if tr.SealedHex != "" {
			sealed, err := hex.DecodeString(tr.SealedHex)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Put(ctx, tracks.StorageKey(tr.AssetID, tr.ID), strings.NewReader(string(sealed))); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The Worker has no Postgres: it reads what the route publisher writes.
	// Build that projection from this same database, so the Worker's copy of
	// these cases runs on exactly what serve would publish.
	checkWorkerBucket(t, pool, v)

	secret, err := contentkey.FromHex(v.ContentKeyVersion, v.ContentKeySecretHex)
	if err != nil {
		t.Fatal(err)
	}
	h := New(pool, stores.Fixed(store), contentkey.NewRing(secret))
	h.Now = func() time.Time { return time.Unix(v.Now, 0) }
	var (
		eventsMu sync.Mutex
		events   []analytics.Event
	)
	h.Record = func(e analytics.Event) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, e)
	}
	takeEvents := func() []analytics.Event {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		got := events
		events = nil
		return got
	}
	mux := http.NewServeMux()
	h.Register(mux)
	// served is told when each request's handler has returned, after which
	// its event, if any, has been recorded.
	served := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		served <- struct{}{}
	}))
	defer srv.Close()
	do := func(t *testing.T, req *http.Request) (*http.Response, []byte, []analytics.Event) {
		t.Helper()
		takeEvents()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		<-served
		return res, body, takeEvents()
	}

	// Twice: the second pass is answered from the route cache (D072), and
	// must answer the same.
	for pass, c := range append(slices.Clone(v.Cases), v.Cases...) {
		name := c.Name
		if pass >= len(v.Cases) {
			name += "/cached"
		}
		t.Run(name, func(t *testing.T) {
			p := c.Path
			for name, tok := range v.Tokens {
				p = strings.ReplaceAll(p, "{"+name+"}", tok)
			}
			method := c.Method
			if method == "" {
				method = http.MethodGet
			}
			req, _ := http.NewRequest(method, srv.URL+p, nil)
			if c.Range != "" {
				req.Header.Set("Range", c.Range)
			}
			res, body, got := do(t, req)
			if res.StatusCode != c.Status {
				t.Fatalf("status = %d, want %d (%s)", res.StatusCode, c.Status, body)
			}
			if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Errorf("missing nosniff/CORS: %v", res.Header)
			}
			if c.ContentType != "" && res.Header.Get("Content-Type") != c.ContentType {
				t.Errorf("Content-Type = %q, want %q", res.Header.Get("Content-Type"), c.ContentType)
			}
			if c.CacheControl != "" && res.Header.Get("Cache-Control") != c.CacheControl {
				t.Errorf("Cache-Control = %q, want %q", res.Header.Get("Cache-Control"), c.CacheControl)
			}
			if c.Body != "" && string(body) != c.Body {
				t.Errorf("body = %q, want %q", body, c.Body)
			}
			if c.BodyHex != "" && hex.EncodeToString(body) != c.BodyHex {
				t.Errorf("body = %x, want %s", body, c.BodyHex)
			}
			var want []analytics.Event
			if a := c.Analytics; a != nil {
				want = []analytics.Event{{ProjectID: db.DefaultProjectID, AppID: a.AppID, AssetID: a.AssetID, PlaybackID: a.PlaybackID,
					Kind: a.Kind, Bytes: a.Bytes, Views: a.Views, WatchSeconds: a.WatchSeconds, Vid: a.Vid}}
			}
			if !slices.Equal(got, want) {
				t.Errorf("analytics = %+v, want %+v", got, want)
			}
		})
	}

	// A fetch the Worker forwards to origin is counted at the edge, not here.
	t.Run("a fetch forwarded by the Worker is not counted again", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/p/pid_public/master.m3u8", nil)
		req.Header.Set("X-ClipMux-Edge", "1")
		res, _, got := do(t, req)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		if len(got) != 0 {
			t.Errorf("analytics = %+v, want none", got)
		}
	})
}

// TestViewerKeys checks analytics.ViewerKey against the vectors, which the
// Worker checks too.
func TestViewerKeys(t *testing.T) {
	v := loadVectors(t)
	key, err := hex.DecodeString(v.AnalyticsKeyHex)
	if err != nil || len(v.ViewerKeys) == 0 {
		t.Fatalf("analytics_key_hex %q / %d viewer_keys", v.AnalyticsKeyHex, len(v.ViewerKeys))
	}
	for _, vk := range v.ViewerKeys {
		if got := hex.EncodeToString(analytics.ViewerKey(key, vk.AppID, vk.Vid)); got != vk.KeyHex {
			t.Errorf("ViewerKey(%s, %q) = %s, want %s", vk.AppID, vk.Vid, got, vk.KeyHex)
		}
	}
}

const workerBucketFile = "testdata/worker-bucket.json"

// workerBucket is the route records and _keys.json for the vectors, as the
// Worker's tests load them (worker/src/vectors.test.ts).
type workerBucket struct {
	Description string                   `json:"description"`
	Routes      map[string]routes.Record `json:"routes"`
	Keys        routes.Keys              `json:"keys"`
}

func checkWorkerBucket(t *testing.T, pool *pgxpool.Pool, v vectors) {
	t.Helper()
	ctx := context.Background()
	q := db.New(pool)
	got := workerBucket{
		Description: "What the route publisher writes for testdata/vectors.json (P2-T02): the Worker's fixture. Regenerate with: go test ./internal/delivery -run TestVectors -update-bucket",
		Routes:      map[string]routes.Record{},
	}
	for _, p := range append(v.PlaybackIDs, vPlaybackID{ID: "pid_nope"}) {
		// Every vectors asset is in the Worker's own bucket.
		r, err := routes.Desired(ctx, q, p.ID, "sto_default")
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			got.Routes[p.ID] = *r
		}
	}
	keys, err := routes.DesiredKeys(ctx, q, db.DefaultProjectID)
	if err != nil {
		t.Fatal(err)
	}
	got.Keys = keys
	out, _ := json.MarshalIndent(got, "", "  ")
	out = append(out, '\n')
	if *update || *updateBucket {
		if err := os.WriteFile(workerBucketFile, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(workerBucketFile)
	if err != nil || string(want) != string(out) {
		t.Fatalf("%s is out of date with the publisher; run with -update-bucket and review the diff (%v)", workerBucketFile, err)
	}
}

func TestRewriteMasterHandlesCRLF(t *testing.T) {
	got := RewriteMaster("#EXTM3U\r\n#EXT-X-MEDIA:URI=\"audio/index.m3u8\"\r\n360p/index.m3u8\r\n", "out_1")
	want := "#EXTM3U\n#EXT-X-MEDIA:URI=\"out_1/audio/index.m3u8\"\nout_1/360p/index.m3u8\n"
	if got != want {
		t.Fatalf("got %q", got)
	}
}

// signRaw signs payload verbatim, for cases a Claims struct can't express.
func signRaw(t *testing.T, key *ecdsa.PrivateKey, kid, payload string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	input := enc.EncodeToString([]byte(`{"alg":"ES256","kid":"`+kid+`","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload))
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + enc.EncodeToString(sig)
}
