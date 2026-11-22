package delivery

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/analytics"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// countingRangeStore wraps storage.S3 so tests verify HEAD performs zero
// object-body downloads while real S3 fulfills Range GETs.
type countingRangeStore struct {
	*storage.S3
	mu   sync.Mutex
	gets int
}

func (s *countingRangeStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.S3.Get(ctx, key)
}

func (s *countingRangeStore) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.S3.GetRange(ctx, key, start, end)
}

func (s *countingRangeStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func TestS3DeliveryServesRangesHeadersAndHEAD(t *testing.T) {
	b := s3test.New(t)
	s3Store, err := storage.OpenS3(storage.S3Options{
		Endpoint: b.Endpoint, Bucket: b.Name, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &countingRangeStore{S3: s3Store}
	pool := dbtest.New(t)
	ctx := context.Background()
	for _, sql := range []string{
		`INSERT INTO apps (id, project_id, name) VALUES ('app_a', 'prj_default', 'a')`,
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_a', 'processing', 'sto_default')`,
		`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{"audio": true, "files": [{"path": "audio/seg_00001.m4s", "size": 11}]}')`,
		`UPDATE assets SET current_output_id = 'out_1', status = 'ready' WHERE id = 'ast_1'`,
		`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_pub', 'prj_default', 'ast_1', 'public')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	const key = "media/ast_1/out_1/audio/seg_00001.m4s"
	if err := s3Store.Put(ctx, key, strings.NewReader("0123456789A")); err != nil {
		t.Fatal(err)
	}

	secret, _ := contentkey.FromHex(1, strings.Repeat("11", 32))
	h := New(pool, stores.Fixed(store), contentkey.NewRing(secret))
	var (
		mu     sync.Mutex
		events []analytics.Event
	)
	h.Record = func(e analytics.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	takeEvents := func() []analytics.Event {
		mu.Lock()
		defer mu.Unlock()
		out := slices.Clone(events)
		events = nil
		return out
	}

	mux := http.NewServeMux()
	h.Register(mux)
	served := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		served <- struct{}{}
	}))
	defer srv.Close()

	do := func(method, rng string) (*http.Response, string, []analytics.Event) {
		t.Helper()
		takeEvents()
		req, _ := http.NewRequest(method, srv.URL+"/p/pid_pub/out_1/audio/seg_00001.m4s", nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		<-served
		return res, string(body), takeEvents()
	}

	// HEAD answers Content-Length and Accept-Ranges without reading object bytes or counting.
	res, body, ev := do(http.MethodHead, "")
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != "11" || res.Header.Get("Accept-Ranges") != "bytes" || body != "" || len(ev) != 0 {
		t.Fatalf("HEAD: status=%d len=%q ar=%q body=%q ev=%v", res.StatusCode, res.Header.Get("Content-Length"), res.Header.Get("Accept-Ranges"), body, ev)
	}
	if store.getCount() != 0 {
		t.Fatalf("HEAD read %d objects, want 0", store.getCount())
	}

	// 206 from byte 0 counts watch time + range bytes only.
	res, body, ev = do(http.MethodGet, "bytes=0-3")
	if res.StatusCode != 206 || res.Header.Get("Content-Range") != "bytes 0-3/11" || res.Header.Get("Content-Length") != "4" || body != "0123" {
		t.Fatalf("GET 0-3: status=%d cr=%q len=%q body=%q", res.StatusCode, res.Header.Get("Content-Range"), res.Header.Get("Content-Length"), body)
	}
	if len(ev) != 1 || ev[0].Bytes != 4 || ev[0].WatchSeconds != 6 {
		t.Fatalf("GET 0-3 analytics = %+v, want 4 bytes & 6 s", ev)
	}

	// 206 after byte 0 counts range bytes only, zero watch time.
	res, body, ev = do(http.MethodGet, "bytes=4-7")
	if res.StatusCode != 206 || res.Header.Get("Content-Range") != "bytes 4-7/11" || body != "4567" {
		t.Fatalf("GET 4-7: status=%d cr=%q body=%q", res.StatusCode, res.Header.Get("Content-Range"), body)
	}
	if len(ev) != 1 || ev[0].Bytes != 4 || ev[0].WatchSeconds != 0 {
		t.Fatalf("GET 4-7 analytics = %+v, want 4 bytes & 0 s", ev)
	}

	// 416 unsatisfiable range emits no analytics event.
	res, _, ev = do(http.MethodGet, "bytes=99-")
	if res.StatusCode != 416 || res.Header.Get("Content-Range") != "bytes */11" || len(ev) != 0 {
		t.Fatalf("GET 99-: status=%d cr=%q ev=%v", res.StatusCode, res.Header.Get("Content-Range"), ev)
	}

	// Multi-range falls back to full 200 with Content-Length.
	res, body, ev = do(http.MethodGet, "bytes=0-1,4-5")
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != "11" || body != "0123456789A" || len(ev) != 1 || ev[0].Bytes != 11 {
		t.Fatalf("GET multi-range: status=%d len=%q body=%q ev=%v", res.StatusCode, res.Header.Get("Content-Length"), body, ev)
	}
}
