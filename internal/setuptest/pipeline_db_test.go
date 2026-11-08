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
	"path/filepath"
	"runtime"
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
	"github.com/visheshgubrani/clipmux/internal/tracks"
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
	latest := runFreshInstall(t, tools, nil)
	want := append([]string{"upload=ok", "encode=ok"}, func() []string {
		var out []string
		for _, n := range PlaybackChecks {
			out = append(out, n+"=ok")
		}
		// Nothing here can transcribe: the subtitles checks pass as skipped.
		for _, n := range TrackChecks {
			out = append(out, n+"=ok")
		}
		return out
	}()...)
	if got := names(latest.Checks); latest.Status != "passed" || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("setup test %s:\n%s", latest.Status, strings.Join(got, "\n"))
	}
	for _, c := range latest.Checks[len(latest.Checks)-len(TrackChecks):] {
		if !strings.HasPrefix(c.Detail, "skipped: nothing here can transcribe") {
			t.Fatalf("%s = %+v, want skipped", c.Name, c)
		}
	}
}

// With a worker that transcribes, the sample's speech must come back as
// subtitles that say "Americans" at 1.6 s ± 0.5 s through delivery (P5-T19).
func TestSetupTestProvesSubtitlesWithWhisper(t *testing.T) {
	tools := mediaTools(t)
	if os.Getenv("CLIPMUX_SKIP_SPEECH") == "1" {
		t.Skip("speech tests skipped (CLIPMUX_SKIP_SPEECH=1); they run on the GPU runner")
	}
	whisper := os.Getenv("CLIPMUX_WHISPER")
	if whisper == "" {
		whisper = "whisper-cli"
	}
	dir := os.Getenv("CLIPMUX_TEST_MODELS_DIR")
	model, vad := filepath.Join(dir, media.SpeechModel.FileName()), filepath.Join(dir, media.VADModel.FileName())
	for _, f := range []string{model, vad} {
		_, err := os.Stat(f)
		if _, lerr := exec.LookPath(whisper); err != nil || lerr != nil || dir == "" {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("whisper-cli and CLIPMUX_TEST_MODELS_DIR are required: %v %v", err, lerr)
			}
			t.Skip("no whisper-cli or speech models")
		}
	}
	tools.Whisper = whisper
	speech := &media.Speech{Backend: media.SpeechCPU, Model: model, VAD: vad, ModelName: media.SpeechModel.ID(), Threads: max(runtime.NumCPU()/2, 1)}
	latest := runFreshInstall(t, tools, speech)
	for _, c := range latest.Checks {
		if !c.OK || strings.HasPrefix(c.Detail, "skipped") {
			t.Fatalf("setup test %s: %s = %+v\n%v", latest.Status, c.Name, c, names(latest.Checks))
		}
	}
	if got := len(latest.Checks); got != 2+len(PlaybackChecks)+len(TrackChecks) {
		t.Fatalf("%d checks: %v", got, names(latest.Checks))
	}
}

// runFreshInstall runs one setup test on a fresh install with a built-in
// worker, and a speech worker beside it when speech is set.
func runFreshInstall(t *testing.T, tools media.Tools, speech *media.Speech) Test {
	t.Helper()
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
	api := workerapi.New(q, workerapi.BuiltinKey("k"), stores.Fixed(store), ring, committer)
	api.Tracks = &tracks.Publisher{Stores: stores.Fixed(store), Keys: ring}
	mux.Handle("POST /worker/v1/", api.Handler())
	mux.Handle("GET /worker/v1/attempts/", api.Handler()) // a transcription's audio
	for _, m := range []string{"GET", "PUT", "OPTIONS"} {
		mux.Handle(m+" /_storage/", store.Handler())
	}
	upl.Register(mux)
	playback := delivery.New(pool, stores.Fixed(store), ring)
	committer.OnPublished, committer.OnKeys = playback.Invalidate, playback.InvalidateKeys // as serve wires them
	playback.Register(mux)
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
	if speech != nil {
		// The built-in worker's speech loop, as serve runs it: its claims
		// say it can transcribe, so the setup test asks for subtitles.
		sw := *w
		sw.WorkDir, sw.Kinds, sw.Speech = t.TempDir(), []string{"transcribe"}, &worker.SpeechSlot{}
		sw.Speech.Set(speech)
		sdone := make(chan struct{})
		go func() {
			defer close(sdone)
			_ = sw.Run(ctx)
		}()
		defer func() { cancel(); <-sdone }()
		// serve's routes loop: a stored track is ready once its routes are.
		rdone := make(chan struct{})
		go func() {
			defer close(rdone)
			for ctx.Err() == nil {
				_, _ = committer.SyncRoutes(ctx, 100)
				time.Sleep(100 * time.Millisecond)
			}
		}()
		defer func() { cancel(); <-rdone }()
		deadline := time.Now().Add(30 * time.Second)
		for {
			var ok bool
			if err := pool.QueryRow(ctx, `SELECT builtin_speech FROM projects WHERE id = 'prj_default'`).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the speech worker never said it can transcribe")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	r := New(pool, stores.Fixed(store), box, urls, upl, lifecycle.New(pool, stores.Fixed(store), committer), "")
	// A 4-vCPU CI runner gives whisper 2 threads while other packages
	// transcribe too, so the 11 s sample can take minutes there.
	r.EncodeTimeout = 8 * time.Minute
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
	return latest
}
