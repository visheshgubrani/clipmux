package push_test

// `clipmux push` against the real upload API and filesystem storage, with
// tiny parts so a file spans several.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/push"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

const partSize = 64

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *storage.Filesystem
	url   string
	key   string
	puts  atomic.Int32 // part PUTs that reached storage
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{t: t, pool: dbtest.New(t)}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: srv.URL, PartSize: partSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e.store, e.url = store, srv.URL
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			e.puts.Add(1)
		}
		store.Handler().ServeHTTP(w, r)
	})
	for _, m := range []string{"GET", "PUT"} {
		mux.Handle(m+" /_storage/", counted)
	}
	svc := uploads.New(e.pool, stores.Fixed(store))
	svc.Register(mux)
	assets.New(e.pool).Register(mux)
	if _, err := e.pool.Exec(ctx, `INSERT INTO apps (id, project_id, name) VALUES ('app_cli', 'prj_default', 'cli')`); err != nil {
		t.Fatal(err)
	}
	e.key, _, _ = apikeys.Create(ctx, e.pool, "prj_default", "app_cli", "cli", []string{apikeys.UploadsWrite, apikeys.AssetsRead})
	return e
}

func (e *env) client(http *http.Client) *push.Client {
	return &push.Client{
		Server: e.url, Key: e.key, HTTP: http, StateDir: filepath.Join(e.t.TempDir(), "state"), Concurrency: 1,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func (e *env) count(sql string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func file(t *testing.T, size int) (string, []byte) {
	t.Helper()
	body := bytes.Repeat([]byte("clipmux!"), size/8+1)[:size]
	p := filepath.Join(t.TempDir(), "lesson.mp4")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, body
}

func (e *env) source(assetID string) []byte {
	e.t.Helper()
	rc, err := e.store.Get(context.Background(), "uploads/"+assetID+"/source")
	if err != nil {
		e.t.Fatal(err)
	}
	defer rc.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(rc)
	return b.Bytes()
}

// failingPuts refuses the PUT of every part from the nth on, as if the
// laptop dropped off the network mid-push.
type failingPuts struct {
	n, seen int
}

func (f *failingPuts) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		f.seen++
		if f.seen >= f.n {
			return nil, errors.New("network is unreachable")
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestPushUploadsAFileInParts(t *testing.T) {
	e := newEnv(t)
	path, body := file(t, 250) // 4 parts: 64, 64, 64, 58
	var last int64
	a, err := e.client(http.DefaultClient).File(context.Background(), path, push.Options{
		Passthrough: "lesson-1", Progress: func(done, _ int64) { last = done },
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "processing" || len(a.PlaybackIDs) != 1 || a.PlaybackIDs[0].Policy != "signed" {
		t.Fatalf("asset = %+v", a)
	}
	if got := e.source(a.ID); !bytes.Equal(got, body) {
		t.Fatalf("stored source is %d bytes, want the file's %d", len(got), len(body))
	}
	if e.puts.Load() != 4 || last != 250 {
		t.Fatalf("puts = %d, progress = %d; want 4 parts and all 250 bytes", e.puts.Load(), last)
	}
}

func TestAKilledPushResumesWithOnlyTheMissingParts(t *testing.T) {
	e := newEnv(t)
	path, body := file(t, 250)
	c := e.client(&http.Client{Transport: &failingPuts{n: 3}})
	if _, err := c.File(context.Background(), path, push.Options{}); err == nil {
		t.Fatal("the push should fail when part 3 can't be sent")
	}
	e.puts.Store(0)

	c.HTTP = http.DefaultClient // the network is back; the state directory is the same
	a, err := c.File(context.Background(), path, push.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.puts.Load(); n != 2 {
		t.Fatalf("resume sent %d parts, want only parts 3 and 4", n)
	}
	if got := e.source(a.ID); !bytes.Equal(got, body) {
		t.Fatal("the resumed source differs from the file")
	}
	if n := e.count(`SELECT count(*) FROM assets`); n != 1 {
		t.Fatalf("%d assets, want the one push", n)
	}
}

func TestPushingAFinishedFileAgainCreatesNothing(t *testing.T) {
	e := newEnv(t)
	path, _ := file(t, 100)
	c := e.client(http.DefaultClient)
	first, err := c.File(context.Background(), path, push.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e.puts.Store(0)
	again, err := c.File(context.Background(), path, push.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || e.puts.Load() != 0 || e.count(`SELECT count(*) FROM assets`) != 1 {
		t.Fatalf("second push = %s with %d puts; want %s again and nothing sent", again.ID, e.puts.Load(), first.ID)
	}
}

func TestAnExpiredPushStartsOver(t *testing.T) {
	e := newEnv(t)
	path, body := file(t, 250)
	c := e.client(&http.Client{Transport: &failingPuts{n: 2}})
	if _, err := c.File(context.Background(), path, push.Options{}); err == nil {
		t.Fatal("the push should fail")
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE uploads SET status = 'expired'`); err != nil {
		t.Fatal(err)
	}
	c.HTTP = http.DefaultClient
	a, err := c.File(context.Background(), path, push.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.source(a.ID); !bytes.Equal(got, body) || e.count(`SELECT count(*) FROM assets`) != 2 {
		t.Fatal("an expired push should start a new upload and finish it")
	}
}

// D130: a push may choose renditions, and a push of the same file with a
// different choice is a different push, not a replay of the first.
func TestAPushCarriesItsRenditions(t *testing.T) {
	e := newEnv(t)
	path, _ := file(t, 100)
	c := e.client(http.DefaultClient)
	first, err := c.File(context.Background(), path, push.Options{Renditions: []string{"720p", "360p"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM assets WHERE id = '` + first.ID + `' AND renditions = '{720p,360p}'`); n != 1 {
		t.Fatalf("asset %s doesn't carry renditions 720p, 360p", first.ID)
	}
	second, err := c.File(context.Background(), path, push.Options{Renditions: []string{"360p"}})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || e.count(`SELECT count(*) FROM assets`) != 2 {
		t.Fatalf("a push with other renditions replayed %s", first.ID)
	}
}
