package setuptest

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/uploads"
	"github.com/visheshgubrani/clipmux/internal/worker"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

func mediaTools(t *testing.T) media.Tools {
	t.Helper()
	tools := media.DefaultTools()
	if p := os.Getenv("CLIPMUX_PACKAGER"); p != "" {
		tools.Packager = p
	}
	for _, bin := range []string{tools.FFmpeg, tools.FFprobe, tools.Packager} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("%s is required: %v", bin, err)
			}
			t.Skipf("%s not found; skipping the pipeline test", bin)
		}
	}
	return tools
}

// The whole path on a fresh install: the embedded sample is uploaded through
// signed URLs on the public URL, encoded and encrypted by a worker, and
// played through delivery with the internal key.
func TestSetupTestPassesEveryCheckOnAFreshInstall(t *testing.T) {
	tools := mediaTools(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{8}, 32))
	if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret, _ := contentkey.FromHex(1, strings.Repeat("cd", 32))
	ring := contentkey.NewRing(secret)
	q := queue.New(pool)
	committer := outputs.New(pool, q, stores.Fixed(store))
	upl := uploads.New(pool, stores.Fixed(store))
	mux.Handle("POST /worker/v1/", workerapi.New(q, workerapi.BuiltinKey("k"), stores.Fixed(store), ring, committer).Handler())
	for _, m := range []string{"GET", "PUT", "OPTIONS"} {
		mux.Handle(m+" /_storage/", store.Handler())
	}
	upl.Register(mux)
	delivery.New(pool, stores.Fixed(store), ring).Register(mux)
	urls, err := settings.Load(ctx, pool, "prj_default", config.Config{PublicURL: srv.URL, MediaURL: srv.URL, URLsPinned: true})
	if err != nil {
		t.Fatal(err)
	}

	w := &worker.Worker{Server: srv.URL, Key: "k", WorkDir: t.TempDir(), Tools: tools, HTTP: worker.DefaultHTTPClient(), Wait: 50 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	defer func() { cancel(); <-done }()

	r := New(pool, stores.Fixed(store), box, urls, upl, lifecycle.New(pool, stores.Fixed(store), committer), "")
	r.EncodeTimeout = 3 * time.Minute
	if _, err := r.Queue(ctx, "manual"); err != nil {
		t.Fatal(err)
	}
	if n, err := r.Run(ctx); err != nil || n != 1 {
		t.Fatalf("Run = %d, %v", n, err)
	}
	latest, ok, err := r.Latest(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	want := append([]string{"upload=ok", "encode=ok"}, func() []string {
		var out []string
		for _, n := range PlaybackChecks {
			out = append(out, n+"=ok")
		}
		return out
	}()...)
	if got := names(latest.Checks); latest.Status != "passed" || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("setup test %s:\n%s", latest.Status, strings.Join(got, "\n"))
	}
}
