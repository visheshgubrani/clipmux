package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// The token the fake accepts. deploy/seaweedfs-dev.json holds the R2
// credentials it derives to: its ID, and the SHA-256 of its value.
const (
	testToken   = "cf-test-token-value"
	testTokenID = "cftesttokenid"
)

// fakeCloudflare is the slice of the API Connect uses, recording each call.
type fakeCloudflare struct {
	mu       sync.Mutex
	calls    []string
	paid     bool
	buckets  map[string]bool
	records  []DNSRecord
	script   []byte
	scripts  []string // names of every Worker uploaded, in order
	metadata map[string]any
	domain   map[string]string
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	ok := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1000, "message": "Invalid API Token"}}})
		return
	}
	p := r.URL.Path
	switch {
	case p == "/user/tokens/verify":
		ok(map[string]string{"id": testTokenID, "status": "active"})
	case p == "/accounts":
		ok([]Account{{ID: "acct1", Name: "Lessons Ltd"}})
	case p == "/zones" && r.URL.Query().Get("name") == "":
		if r.URL.Query().Get("account.id") == "acct1" && r.URL.Query().Get("status") == "active" {
			ok([]map[string]any{{"id": "zone1", "name": "example.com", "status": "active"}})
		} else {
			ok([]any{})
		}
	case p == "/accounts/acct1/r2/buckets" && r.Method == http.MethodGet:
		var list []map[string]string
		for name := range f.buckets {
			list = append(list, map[string]string{"name": name})
		}
		ok(map[string]any{"buckets": list})
	case p == "/zones":
		if r.URL.Query().Get("name") == "example.com" && r.URL.Query().Get("account.id") == "acct1" {
			ok([]map[string]any{{"id": "zone1", "name": "example.com", "status": "active"}})
		} else {
			ok([]any{})
		}
	case p == "/accounts/acct1/workers/domains" && r.Method == http.MethodGet:
		if f.domain != nil {
			ok([]map[string]string{f.domain})
		} else {
			ok([]any{})
		}
	case p == "/accounts/acct1/subscriptions":
		if f.paid {
			ok([]map[string]any{{"rate_plan": map[string]string{"id": "workers_paid", "public_name": "Workers Paid"}, "state": "Paid"}})
		} else {
			ok([]any{})
		}
	case strings.HasPrefix(p, "/accounts/acct1/r2/buckets/") && r.Method == http.MethodGet:
		if !f.buckets[strings.TrimPrefix(p, "/accounts/acct1/r2/buckets/")] {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 10006, "message": "bucket not found"}}})
			return
		}
		ok(map[string]any{})
	case p == "/accounts/acct1/r2/buckets" && r.Method == http.MethodPost:
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.buckets[b["name"]] = true
		ok(map[string]any{})
	case strings.HasPrefix(p, "/accounts/acct1/workers/scripts/") && r.Method == http.MethodPut:
		f.scripts = append(f.scripts, strings.TrimPrefix(p, "/accounts/acct1/workers/scripts/"))
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "metadata":
				_ = json.Unmarshal(b, &f.metadata)
			case "worker.js":
				f.script = b
			}
		}
		ok(map[string]any{})
	case p == "/zones/zone1/dns_records" && r.Method == http.MethodGet:
		ok(f.records)
	case strings.HasPrefix(p, "/zones/zone1/dns_records/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(p, "/zones/zone1/dns_records/")
		for i, rec := range f.records {
			if rec.ID == id {
				f.records = append(f.records[:i], f.records[i+1:]...)
				break
			}
		}
		ok(map[string]any{})
	case p == "/accounts/acct1/workers/domains" && r.Method == http.MethodPut:
		_ = json.NewDecoder(r.Body).Decode(&f.domain)
		ok(map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 7003, "message": "no route " + p}}})
	}
}

type rig struct {
	conn   *Connector
	fake   *fakeCloudflare
	urls   *settings.Store
	pool   *db.Queries
	raw    *pgxpool.Pool
	stash  *stores.Set
	bucket string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	// The bucket exists on the test store; to the fake API it is new, so
	// Connect "creates" it there. The token's derived credentials reach it.
	b := s3test.New(t)
	pool := dbtest.New(t)
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "https://video.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{7}, 32))
	set := stores.New(pool, box, fs)
	urls, err := settings.Load(context.Background(), pool, db.DefaultProjectID, config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := urls.Set(context.Background(), "https://video.example.org", ""); err != nil {
		t.Fatal(err)
	}
	ring, _ := contentkey.FromHex(1, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	fake := &fakeCloudflare{paid: true, buckets: map[string]bool{},
		records: []DNSRecord{{ID: "rec1", Type: "CNAME", Name: "media.example.com"}, {ID: "rec2", Type: "A", Name: "www.example.com"}}}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	conn := &Connector{
		Pool: pool, Stores: set, Keys: contentkey.NewRing(ring), URLs: urls, ProjectID: db.DefaultProjectID,
		API: api.URL, R2Endpoint: func(string) string { return b.Endpoint },
		AnalyticsKey: bytes.Repeat([]byte{0xab}, 32),
	}
	return &rig{conn: conn, fake: fake, urls: urls, pool: db.New(pool), raw: pool, stash: set, bucket: b.Name}
}

func TestConnectMovesDeliveryToTheWorker(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "Media.Example.com", Bucket: r.bucket})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Connected {
		t.Fatalf("not connected: %+v", res.Steps)
	}
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "token,account,zone,workers_paid,bucket,storage,worker,routes,dns,domain,media_url,saved" {
		t.Fatalf("steps = %s", got)
	}
	// The Worker went up with its bucket, origin and the whole key ring.
	if !r.fake.buckets[r.bucket] {
		t.Fatal("the bucket wasn't created through the API")
	}
	if !bytes.Equal(r.fake.script, workerJS) || r.fake.metadata["main_module"] != "worker.js" {
		t.Fatal("the embedded Worker wasn't uploaded as the main module")
	}
	installID, err := r.pool.GetAnalyticsInstallID(ctx, db.DefaultProjectID)
	if err != nil || !strings.HasPrefix(installID, "ins_") {
		t.Fatalf("install ID = %q, %v", installID, err)
	}
	bindings, _ := json.Marshal(r.fake.metadata["bindings"])
	for _, want := range []string{
		`{"bucket_name":"` + r.bucket + `","name":"MEDIA","type":"r2_bucket"}`,
		`{"name":"ORIGIN_URL","text":"https://video.example.org","type":"plain_text"}`,
		`{"name":"CONTENT_KEYS","text":"{\"1\":\"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\"}","type":"secret_text"}`,
		// Counting at the edge (D103): one dataset, every point naming its project and install.
		`{"dataset":"clipmux_delivery","name":"ANALYTICS","type":"analytics_engine"}`,
		`{"name":"PROJECT_ID","text":"prj_default","type":"plain_text"}`,
		`{"name":"INSTALL_ID","text":"` + installID + `","type":"plain_text"}`,
		`{"name":"ANALYTICS_KEY","text":"abababababababababababababababababababababababababababababababab","type":"secret_text"}`,
	} {
		if !strings.Contains(string(bindings), want) {
			t.Fatalf("bindings %s lack %s", bindings, want)
		}
	}
	// The hostname's old record went, other names stayed, the domain is on.
	if len(r.fake.records) != 1 || r.fake.records[0].Name != "www.example.com" {
		t.Fatalf("dns records left = %+v", r.fake.records)
	}
	if r.fake.domain["hostname"] != "media.example.com" || r.fake.domain["service"] != "clipmux-media-example-com" || r.fake.domain["zone_id"] != "zone1" {
		t.Fatalf("domain = %v", r.fake.domain)
	}
	// Delivery and the media URL moved; uploads stay where Storage sends
	// them, and new videos' encoded files go to the bucket (D117).
	p, _ := r.pool.GetProjectDelivery(ctx, db.DefaultProjectID)
	def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID)
	media, _ := r.pool.GetMediaTarget(ctx, db.DefaultProjectID)
	if p.Delivery != "worker" || def.Kind != "fs" || media != p.DeliveryStorageID.String {
		t.Fatalf("delivery = %+v, default storage = %+v, media target %s", p, def, media)
	}
	if r.urls.MediaURL() != "https://media.example.com" {
		t.Fatalf("media URL = %s", r.urls.MediaURL())
	}
	// The bucket holds _keys.json, and the raw token is stored nowhere.
	bucket, _ := r.stash.Get(ctx, p.DeliveryStorageID.String)
	if _, err := bucket.Head(ctx, routes.KeysKey); err != nil {
		t.Fatalf("_keys.json: %v", err)
	}
	var leaked int
	_ = r.raw.QueryRow(ctx, `SELECT count(*) FROM storage_configs WHERE config::text LIKE '%'||$1||'%'`, testToken).Scan(&leaked)
	row, err := r.pool.GetCloudflareConnection(ctx, db.DefaultProjectID)
	if err != nil || row.TokenID != testTokenID || leaked != 0 || !row.AnalyticsTagged || row.AnalyticsLegacyUntil.Valid {
		t.Fatalf("connection = %+v, %v; token in configs: %d", row, err, leaked)
	}
	st, err := r.conn.Status(ctx)
	if err != nil || !st.Connected || st.NeedsRedeploy || !st.StorageOK {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// Cloudflare adds a managed DNS record when attaching a custom domain.
	// Reconnect must preserve that record instead of trying to delete it.
	r.fake.records = append(r.fake.records, DNSRecord{ID: "managed-worker-dns", Type: "AAAA", Name: "media.example.com"})
	// Connecting again (a new token, or an upgrade) reuses the config and
	// keeps analytics_legacy_until unset when already tagged.
	res, err = r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket})
	if err != nil || !res.Connected {
		t.Fatalf("reconnect: %+v, %v", res.Steps, err)
	}
	if row, _ = r.pool.GetCloudflareConnection(ctx, db.DefaultProjectID); !row.AnalyticsTagged || row.AnalyticsLegacyUntil.Valid {
		t.Fatalf("reconnect on tagged connection = %+v, want legacy_until unset", row)
	}
	// Upgrading an older untagged connection sets analytics_legacy_until.
	if _, err := r.raw.Exec(ctx, `UPDATE cloudflare_connections SET analytics_tagged = false`); err != nil {
		t.Fatal(err)
	}
	if res, err = r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("upgrade reconnect: %+v, %v", res.Steps, err)
	}
	if row, _ = r.pool.GetCloudflareConnection(ctx, db.DefaultProjectID); !row.AnalyticsTagged || !row.AnalyticsLegacyUntil.Valid {
		t.Fatalf("upgrade reconnect = %+v, want analytics_tagged and legacy_until set", row)
	}
	if len(r.fake.records) != 2 || r.fake.records[1].ID != "managed-worker-dns" {
		t.Fatalf("reconnect removed Worker-managed DNS: %+v", r.fake.records)
	}
	configs, _ := r.pool.ListStorageConfigs(ctx, db.DefaultProjectID)
	if len(configs) != 2 {
		t.Fatalf("storage configs = %d, want fs and one r2", len(configs))
	}
}

// The owner chooses where uploads go, under Storage (D110, D117): Connect
// never changes it, on a first connect or a reconnect.
func TestConnectLeavesTheUploadStoreToTheOwner(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket})
	if err != nil || !res.Connected {
		t.Fatalf("connect: %+v, %v", res.Steps, err)
	}
	if def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID); def.Kind != "fs" {
		t.Fatalf("after the first connect new uploads go to %s, want fs", def.Kind)
	}
	if d := stepDetail(res, "routes"); !strings.Contains(d, "new videos are encoded into the bucket") {
		t.Fatalf("routes step says %q, want it to say where new videos are encoded", d)
	}
	p, _ := r.pool.GetProjectDelivery(ctx, db.DefaultProjectID)
	if err := r.stash.SetDefault(ctx, db.DefaultProjectID, p.DeliveryStorageID.String); err != nil {
		t.Fatal(err)
	}
	res, err = r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket})
	if err != nil || !res.Connected {
		t.Fatalf("reconnect: %+v, %v", res.Steps, err)
	}
	if def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID); def.Kind != "r2" {
		t.Fatalf("a reconnect moved new uploads to %s; the owner chose r2", def.Kind)
	}
}

// Workers are named after their hostname (D114), so two installs in one
// account don't overwrite each other's; a reconnect keeps the name the
// connection was saved with, including the fixed name installs used before.
func TestConnectNamesTheWorkerAfterItsHostname(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("connect: %+v, %v", res.Steps, err)
	}
	if _, err := r.raw.Exec(ctx, `UPDATE cloudflare_connections SET script_name = 'clipmux-media'`); err != nil {
		t.Fatal(err)
	}
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("reconnect: %+v, %v", res.Steps, err)
	}
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "edge.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("connect elsewhere: %+v, %v", res.Steps, err)
	}
	if got := strings.Join(r.fake.scripts, ","); got != "clipmux-media-example-com,clipmux-media,clipmux-edge-example-com" {
		t.Fatalf("Workers uploaded = %s", got)
	}
	row, err := r.pool.GetCloudflareConnection(ctx, db.DefaultProjectID)
	if err != nil || row.ScriptName != "clipmux-edge-example-com" || r.fake.domain["service"] != "clipmux-edge-example-com" {
		t.Fatalf("saved script = %q, %v; domain = %v", row.ScriptName, err, r.fake.domain)
	}
}

// R2 storage without Cloudflare delivery (D110): the bucket is created if
// missing, saved as r2 with keys derived from the token, and Connect later
// reuses that same config.
func TestAddR2SavesTheBucketAsStorage(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	res, storageID, err := r.conn.AddR2(ctx, R2Request{Token: testToken, Bucket: r.bucket})
	if err != nil || !res.Connected || storageID == "" {
		t.Fatalf("add R2: %+v, %q, %v", res.Steps, storageID, err)
	}
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "token,account,bucket,storage" {
		t.Fatalf("steps = %s", got)
	}
	if !r.fake.buckets[r.bucket] {
		t.Fatal("the bucket wasn't created through the API")
	}
	row, err := r.pool.GetStorageConfig(ctx, storageID)
	if err != nil || row.Kind != "r2" || row.IsDefault {
		t.Fatalf("storage = %+v, %v; want r2 and not the default", row, err)
	}

	res, again, err := r.conn.AddR2(ctx, R2Request{Token: testToken, Bucket: r.bucket, Default: true})
	if err != nil || !res.Connected || again != storageID {
		t.Fatalf("adding the same bucket again = %q (%+v, %v), want %q renewed", again, res.Steps, err, storageID)
	}
	if def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID); def.ID != storageID {
		t.Fatalf("default = %s, want %s", def.ID, storageID)
	}
	if res, err := r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket}); err != nil || !res.Connected {
		t.Fatalf("connect: %+v, %v", res.Steps, err)
	}
	if configs, _ := r.pool.ListStorageConfigs(ctx, db.DefaultProjectID); len(configs) != 2 {
		t.Fatalf("storage configs = %d, want fs and the one r2 Connect reused", len(configs))
	}
	if _, _, err := r.conn.AddR2(ctx, R2Request{Token: testToken, Bucket: "No_Such"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed bucket = %v, want ErrInvalid", err)
	}
}

// Inspect (D109) shows what a pasted token reaches, so the dashboard offers
// choices instead of typed IDs, and changes nothing.
func TestInspectShowsWhatTheTokenReaches(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.fake.buckets["clipmux-media"] = true
	got, err := r.conn.Inspect(ctx, testToken, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Inspection{
		Accounts: []Account{{ID: "acct1", Name: "Lessons Ltd"}}, AccountID: "acct1",
		Zones: []string{"example.com"}, Buckets: []string{"clipmux-media"}, WorkersPaid: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inspect = %+v, want %+v", got, want)
	}
	for _, c := range r.fake.calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("inspect changed Cloudflare: %s", c)
		}
	}
	if _, err := r.conn.Inspect(ctx, "wrong", ""); !errors.Is(err, ErrTokenRejected) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("a bad token = %v, want ErrTokenRejected without echoing the token", err)
	}
}

// The create-token links pre-fill Cloudflare's form with exactly the
// permissions each flow uses (account-owned token template URLs).
func TestTokenLinksAskForWhatEachFlowUses(t *testing.T) {
	for _, c := range []struct {
		url, name, perms string
	}{
		{ConnectTokenURL(), "ClipMux", `[{"key":"workers_scripts","type":"edit"},{"key":"workers_routes","type":"edit"},{"key":"workers_r2","type":"edit"},{"key":"billing","type":"read"},{"key":"zone","type":"read"},{"key":"dns","type":"edit"}]`},
		{R2TokenURL(), "ClipMux storage", `[{"key":"workers_r2","type":"edit"}]`},
		{AnalyticsTokenURL(), "ClipMux analytics", `[{"key":"account_analytics","type":"read"}]`},
	} {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if u.Scheme+"://"+u.Host+u.Path != "https://dash.cloudflare.com/" || q.Get("to") != "/:account/api-tokens" || q.Get("name") != c.name || q.Get("permissionGroupKeys") != c.perms {
			t.Errorf("%s = %s (name %q, permissions %s), want an account token template named %q with %s", c.name, c.url, q.Get("name"), q.Get("permissionGroupKeys"), c.name, c.perms)
		}
	}
}

func stepDetail(res Result, name string) string {
	for _, s := range res.Steps {
		if s.Name == name {
			return s.Detail
		}
	}
	return ""
}

func TestConnectStopsWithoutWorkersPaid(t *testing.T) {
	r := newRig(t)
	r.fake.paid = false
	res, err := r.conn.Connect(context.Background(), Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket})
	if err != nil {
		t.Fatal(err)
	}
	last := res.Steps[len(res.Steps)-1]
	if res.Connected || last.Name != "workers_paid" || last.OK || !strings.Contains(last.Detail, "Workers Paid") {
		t.Fatalf("result = %+v", res)
	}
	for _, c := range r.fake.calls {
		if strings.Contains(c, "/r2/") || strings.Contains(c, "/workers/") {
			t.Fatalf("changed Cloudflare before refusing: %s", c)
		}
	}
	p, _ := r.pool.GetProjectDelivery(context.Background(), db.DefaultProjectID)
	if p.Delivery != "builtin" {
		t.Fatalf("delivery = %s, want builtin", p.Delivery)
	}
}

func TestConnectRefusesABadTokenAndHostname(t *testing.T) {
	r := newRig(t)
	res, err := r.conn.Connect(context.Background(), Request{Token: "wrong", Hostname: "media.example.com"})
	if err != nil || res.Connected || len(res.Steps) != 1 || res.Steps[0].OK || strings.Contains(res.Steps[0].Detail, "wrong") {
		t.Fatalf("bad token: %+v, %v (the detail must not echo the token)", res, err)
	}
	if _, err := r.conn.Connect(context.Background(), Request{Token: testToken, Hostname: "video.example.org"}); err == nil {
		t.Fatal("the public URL's hostname was accepted as the media hostname")
	}
	if _, err := r.conn.Connect(context.Background(), Request{Token: testToken, Hostname: "not a host"}); err == nil {
		t.Fatal("a malformed hostname was accepted")
	}
}

func TestTheEmbeddedWorkerIsBuilt(t *testing.T) {
	if len(workerJS) < 1000 || !bytes.Contains(workerJS, []byte("_routes/")) {
		t.Fatal("internal/cloudflare/worker.js is missing or not the Worker bundle; run make worker")
	}
	if os.Getenv("CI") != "" && WorkerVersion() == "" {
		t.Fatal("no version")
	}
}
