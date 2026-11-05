package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
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
	case p == "/zones":
		if r.URL.Query().Get("name") == "example.com" && r.URL.Query().Get("account.id") == "acct1" {
			ok([]map[string]any{{"id": "zone1", "name": "example.com", "status": "active"}})
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
	case p == "/accounts/acct1/workers/scripts/clipmux-media" && r.Method == http.MethodPut:
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
	bindings, _ := json.Marshal(r.fake.metadata["bindings"])
	for _, want := range []string{
		`{"bucket_name":"` + r.bucket + `","name":"MEDIA","type":"r2_bucket"}`,
		`{"name":"ORIGIN_URL","text":"https://video.example.org","type":"plain_text"}`,
		`{"name":"CONTENT_KEYS","text":"{\"1\":\"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f\"}","type":"secret_text"}`,
	} {
		if !strings.Contains(string(bindings), want) {
			t.Fatalf("bindings %s lack %s", bindings, want)
		}
	}
	// The hostname's old record went, other names stayed, the domain is on.
	if len(r.fake.records) != 1 || r.fake.records[0].Name != "www.example.com" {
		t.Fatalf("dns records left = %+v", r.fake.records)
	}
	if r.fake.domain["hostname"] != "media.example.com" || r.fake.domain["service"] != "clipmux-media" || r.fake.domain["zone_id"] != "zone1" {
		t.Fatalf("domain = %v", r.fake.domain)
	}
	// Delivery, default storage and the media URL all moved.
	p, _ := r.pool.GetProjectDelivery(ctx, db.DefaultProjectID)
	def, _ := r.pool.GetDefaultStorage(ctx, db.DefaultProjectID)
	if p.Delivery != "worker" || p.DeliveryStorageID.String != def.ID || def.Kind != "r2" {
		t.Fatalf("delivery = %+v, default storage = %+v", p, def)
	}
	if r.urls.MediaURL() != "https://media.example.com" {
		t.Fatalf("media URL = %s", r.urls.MediaURL())
	}
	// The bucket holds _keys.json, and the raw token is stored nowhere.
	bucket, _ := r.stash.Get(ctx, def.ID)
	if _, err := bucket.Head(ctx, routes.KeysKey); err != nil {
		t.Fatalf("_keys.json: %v", err)
	}
	var leaked int
	_ = r.raw.QueryRow(ctx, `SELECT count(*) FROM storage_configs WHERE config::text LIKE '%'||$1||'%'`, testToken).Scan(&leaked)
	row, err := r.pool.GetCloudflareConnection(ctx, db.DefaultProjectID)
	if err != nil || row.TokenID != testTokenID || leaked != 0 {
		t.Fatalf("connection = %+v, %v; token in configs: %d", row, err, leaked)
	}
	st, err := r.conn.Status(ctx)
	if err != nil || !st.Connected || st.NeedsRedeploy || !st.StorageOK {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// Connecting again (a new token, or an upgrade) reuses the config.
	res, err = r.conn.Connect(ctx, Request{Token: testToken, Hostname: "media.example.com", Bucket: r.bucket})
	if err != nil || !res.Connected {
		t.Fatalf("reconnect: %+v, %v", res.Steps, err)
	}
	configs, _ := r.pool.ListStorageConfigs(ctx, db.DefaultProjectID)
	if len(configs) != 2 {
		t.Fatalf("storage configs = %d, want fs and one r2", len(configs))
	}
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
