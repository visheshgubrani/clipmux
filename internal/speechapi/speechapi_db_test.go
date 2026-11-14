package speechapi

// The speech API end to end against a fake OpenAI-compatible server
// (P5-T14b, D075): routing by speech mode, chunked uploads of the real
// audio rendition, retries, refusals, the drain, Connect, and the key kept
// out of rows, errors and logs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

const testKey = "gsk_SPEECH_KEY_must_never_show"

// fake is an OpenAI-compatible speech server. Each transcription answers
// one segment, "chunk N", from 0.5 s to 1.5 s of the uploaded file, with
// two words; script can replace an answer by call number (1-based).
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []call
	script map[int]func(w http.ResponseWriter)
	block  chan struct{} // when set, transcriptions wait for it
}

type call struct {
	fields map[string][]string
	audio  []byte
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, script: map[int]func(http.ResponseWriter){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid API Key"}}`)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, `{"data":[{"id":"whisper-large-v3-turbo"}]}`)
		return
	case "/v1/audio/transcriptions":
	default:
		http.NotFound(w, r)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		f.t.Errorf("multipart: %v", err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		f.t.Errorf("no file: %v", err)
		return
	}
	audio, _ := io.ReadAll(file)
	if hdr.Header.Get("Content-Type") != "audio/mp4" || hdr.Filename != "chunk.m4a" {
		f.t.Errorf("file part %q %q", hdr.Filename, hdr.Header.Get("Content-Type"))
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{fields: r.MultipartForm.Value, audio: audio})
	n := len(f.calls)
	scripted, block := f.script[n], f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if scripted != nil {
		scripted(w)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"language": "english", "duration": 6.0, "text": fmt.Sprintf(" chunk %d", n),
		"segments": []map[string]any{{"start": 0.5, "end": 1.5, "text": fmt.Sprintf(" chunk %d", n)}},
		"words":    []map[string]any{{"word": "chunk", "start": 0.5, "end": 1.0}, {"word": fmt.Sprint(n), "start": 1.0, "end": 1.5}},
	})
}

func (f *fake) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// rig is serve's speech side on real Postgres and a filesystem store, with
// one asset whose real encoded output (16 s of speech and silence) has a
// subtitles track waiting for its transcription.
type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  storage.Storage
	q      *queue.Queue
	svc    *Service
	runner *Runner
	fake   *fake
	logs   *bytes.Buffer
	output db.Output
}

func newRig(t *testing.T) *rig {
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
			t.Skipf("%s not found", bin)
		}
	}
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	secret, _ := contentkey.FromHex(1, strings.Repeat("ab", 32))
	keys := contentkey.NewRing(secret)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{7}, 32))
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	q := queue.New(pool)
	set := stores.Fixed(store)
	svc := NewService(pool, box, db.DefaultProjectID)
	svc.Logger = logger
	r := &rig{t: t, pool: pool, store: store, q: q, svc: svc, fake: newFake(t), logs: logs,
		runner: &Runner{Pool: pool, Queue: q, Stores: set, Keys: keys, Box: box, Publisher: &tracks.Publisher{Stores: set, Keys: keys},
			HTTP: http.DefaultClient, Logger: logger, Heartbeat: 50 * time.Millisecond}}
	r.output = r.encoded(tools, secret)
	return r
}

// encoded runs the real pipeline on 11 s of speech and 5 s of silence (three
// six-second segments of audio), stores the output, and makes it the
// asset's current output with a subtitles track waiting.
func (r *rig) encoded(tools media.Tools, secret contentkey.Secret) db.Output {
	r.t.Helper()
	ctx := context.Background()
	dir := r.t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=black:size=320x180:rate=25:duration=16",
		"-i", filepath.Join("..", "media", "testdata", "speech", "jfk.ogg"), "-filter_complex", "[1]apad=whole_dur=16[a]",
		"-map", "0:v", "-map", "[a]", "-t", "16", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", src).CombinedOutput(); err != nil {
		r.t.Fatalf("ffmpeg: %v %s", err, out)
	}
	probe, err := media.Probe(ctx, tools, src)
	if err != nil {
		r.t.Fatal(err)
	}
	key := media.Key{Version: 1, Key: secret.Derive("ast_1", "out_1"), KeyID: contentkey.KeyID("out_1")}
	out := filepath.Join(dir, "out")
	m, err := media.Transcode(ctx, tools, src, probe, nil, filepath.Join(dir, "work"), out, key, func(string, float64) {})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, f := range m.Files {
		b, err := os.ReadFile(filepath.Join(out, f.Path))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := r.store.Put(ctx, "media/ast_1/out_1/"+f.Path, bytes.NewReader(b)); err != nil {
			r.t.Fatal(err)
		}
	}
	manifest, _ := json.Marshal(m)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', $1)`, manifest)
	r.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_1' WHERE id = 'ast_1'`)
	o, err := db.New(r.pool).GetOutput(ctx, "out_1")
	if err != nil {
		r.t.Fatal(err)
	}
	return o
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) connect() {
	r.t.Helper()
	res, err := r.svc.Connect(context.Background(), ConnectRequest{BaseURL: r.fake.srv.URL + "/v1", Model: "whisper-large-v3-turbo", APIKey: testKey})
	if err != nil || !res.Connected {
		r.t.Fatalf("connect: %+v, %v", res, err)
	}
}

// subtitles asks for subtitles and queues the transcription, as the
// output's commit does.
func (r *rig) subtitles() db.Job {
	r.t.Helper()
	ctx := context.Background()
	qs := db.New(r.pool)
	if _, err := tracks.Create(ctx, qs, "prj_default", "ast_1", tracks.Generate{Subtitles: true}); err != nil {
		r.t.Fatal(err)
	}
	asset, err := qs.GetAssetByID(ctx, "ast_1")
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		r.t.Fatal(err)
	}
	return r.transcription()
}

func (r *rig) transcription() db.Job {
	r.t.Helper()
	var id string
	if err := r.pool.QueryRow(context.Background(), `SELECT id FROM jobs WHERE kind = 'transcribe' ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	j, err := db.New(r.pool).GetJob(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rig) run() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if n, err := r.runner.Run(ctx); err != nil || n != 1 {
		r.t.Fatalf("Run = %d, %v", n, err)
	}
}

func (r *rig) track() (status, code string) {
	r.t.Helper()
	if err := r.pool.QueryRow(context.Background(), `SELECT status, coalesce(error_code, '') FROM tracks ORDER BY created_at DESC LIMIT 1`).Scan(&status, &code); err != nil {
		r.t.Fatal(err)
	}
	return status, code
}

// noKeyAnywhere checks the API key is in no row of any table, and not in
// the logs.
func (r *rig) noKeyAnywhere() {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM ai_connections WHERE position($1::bytea IN key_sealed) > 0 OR row_to_json(ai_connections)::text LIKE '%' || $2 || '%')
		+ (SELECT count(*) FROM transcripts WHERE body::text LIKE '%' || $2 || '%')
		+ (SELECT count(*) FROM attempts WHERE row_to_json(attempts)::text LIKE '%' || $2 || '%')
		+ (SELECT count(*) FROM jobs WHERE row_to_json(jobs)::text LIKE '%' || $2 || '%')`, []byte(testKey), testKey).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	if n != 0 || strings.Contains(r.logs.String(), testKey) {
		r.t.Fatalf("the API key shows: %d rows, logs contain it: %v", n, strings.Contains(r.logs.String(), testKey))
	}
}

func TestAPIModeTranscribesThroughTheAPIInChunks(t *testing.T) {
	r := newRig(t)
	r.connect()
	if err := r.svc.SetMode(context.Background(), queue.SpeechAPI); err != nil {
		t.Fatal(err)
	}
	job := r.subtitles()
	if job.RoutedTo.String != "speech_api" || job.RouteReason.String != "primary" {
		t.Fatalf("transcription routed %s %s", job.RoutedTo.String, job.RouteReason.String)
	}
	// A cap under any spoken segment (about 90 KB for 6 s at 128 kb/s)
	// makes one segment a chunk, since a chunk always takes at least one:
	// three chunks, the last 4 s of silence that AAC packs into 1 KB.
	init, err := readAll(r.store, "media/ast_1/out_1/audio/init.mp4")
	if err != nil {
		t.Fatal(err)
	}
	r.runner.ChunkBytes = len(init) + 50_000
	r.run()

	calls := r.fake.recorded()
	if len(calls) != 4 { // Connect's sample, then three chunks
		t.Fatalf("%d calls, want 4", len(calls))
	}
	audio, err := outputs.OpenAudio(context.Background(), r.store, r.runner.Keys, r.output)
	if err != nil {
		t.Fatal(err)
	}
	var starts []int64
	at := 0.0
	for _, s := range audio.Playlist.Segments {
		starts = append(starts, ms(at))
		at += s.Duration
	}
	for i, c := range calls[1:] {
		want := map[string]string{"model": "whisper-large-v3-turbo", "response_format": "verbose_json", "temperature": "0"}
		for k, v := range want {
			if got := c.fields[k]; len(got) != 1 || got[0] != v {
				t.Errorf("chunk %d %s = %v", i+1, k, got)
			}
		}
		if g := c.fields["timestamp_granularities[]"]; strings.Join(g, ",") != "segment,word" {
			t.Errorf("chunk %d granularities %v", i+1, g)
		}
		// The first chunk lets the API detect; the others carry its
		// language and the end of the previous chunk's text.
		if i == 0 && (c.fields["language"] != nil || c.fields["prompt"] != nil) {
			t.Errorf("chunk 1 fields %v", c.fields)
		}
		if i > 0 && (strings.Join(c.fields["language"], "") != "en" || strings.Join(c.fields["prompt"], "") != fmt.Sprintf(" chunk %d", i+1)) {
			t.Errorf("chunk %d language %v prompt %v", i+1, c.fields["language"], c.fields["prompt"])
		}
		if codec := ffprobeCodec(t, c.audio); codec != "aac" {
			t.Errorf("chunk %d is %q, want AAC ffprobe can read", i+1, codec)
		}
	}
	// No playback ID to publish to: ready at once (P5-T15).
	if status, _ := r.track(); status != "ready" {
		t.Fatalf("track %s", status)
	}
	var body []byte
	if err := r.pool.QueryRow(context.Background(), `SELECT body FROM transcripts`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var tr struct {
		Language string `json:"language"`
		Model    string `json:"model"`
		Segments []struct {
			Text    string `json:"text"`
			StartMS int64  `json:"start_ms"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Language != "en" || !strings.HasPrefix(tr.Model, "whisper-large-v3-turbo@127.0.0.1:") || len(tr.Segments) != 3 {
		t.Fatalf("transcript %+v", tr)
	}
	for i, s := range tr.Segments {
		if s.Text != fmt.Sprintf("chunk %d", i+2) || s.StartMS != starts[i]+500 {
			t.Errorf("segment %d = %q at %d, want chunk %d at %d", i, s.Text, s.StartMS, i+2, starts[i]+500)
		}
	}
	r.noKeyAnywhere()
}

func TestLocalThenAPISendsTranscriptionsToTheAPIOnlyWhileNoWorkerCanTranscribe(t *testing.T) {
	r := newRig(t)
	r.connect()
	if err := r.svc.SetMode(context.Background(), queue.SpeechLocalThenAPI); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE projects SET primary_executor = 'agents'`)
	r.exec(`INSERT INTO agents (id, project_id, name, key_hash, status, sandboxed, last_seen_at) VALUES ('agt_1', 'prj_default', 'pc', 'x', 'approved', true, now())`)
	job := r.subtitles()
	if _, err := r.q.Route(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if j, _ := db.New(r.pool).GetJob(context.Background(), job.ID); j.RoutedTo.String != "speech_api" || j.RouteReason.String != "primary_offline" {
		t.Fatalf("with a PC that can't transcribe: %s %s", j.RoutedTo.String, j.RouteReason.String)
	}
	// The PC reports speech: the next transcription stays local.
	r.exec(`UPDATE agents SET capabilities = '{"speech": {"backends": ["cpu"], "model": "m"}}'`)
	r.exec(`UPDATE jobs SET routed_to = 'agents', route_reason = 'primary' WHERE id = $1`, job.ID)
	if _, err := r.q.Route(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if j, _ := db.New(r.pool).GetJob(context.Background(), job.ID); j.RoutedTo.String != "agents" || j.RouteReason.String != "primary" {
		t.Fatalf("with a PC that can: %s %s", j.RoutedTo.String, j.RouteReason.String)
	}
}

func TestATransientRefusalIsRetriedAndAPermanentOneFailsTheTrack(t *testing.T) {
	r := newRig(t)
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} })
	r.connect()
	if err := r.svc.SetMode(context.Background(), queue.SpeechAPI); err != nil {
		t.Fatal(err)
	}
	r.fake.script[2] = func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}
	r.subtitles()
	r.run()
	if status, _ := r.track(); status != "ready" {
		t.Fatalf("after a 429 then success: %s", status)
	}

	// A new asset version: a 400 is permanent.
	r.exec(`UPDATE tracks SET status = 'retired', retired_at = now()`)
	r.fake.mu.Lock()
	n := len(r.fake.calls)
	r.fake.script[n+1] = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"file format not supported"}}`)
	}
	r.fake.mu.Unlock()
	r.subtitles()
	r.run()
	if status, code := r.track(); status != "errored" || code != "speech_api_rejected" {
		t.Fatalf("after a 400: %s %s", status, code)
	}
	r.noKeyAnywhere()
}

func TestDisconnectingReturnsToLocalAndReroutes(t *testing.T) {
	r := newRig(t)
	r.connect()
	if err := r.svc.SetMode(context.Background(), queue.SpeechAPI); err != nil {
		t.Fatal(err)
	}
	job := r.subtitles()
	if err := r.svc.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := r.svc.Status(context.Background())
	if err != nil || st.Mode != queue.SpeechLocal || st.Connection != nil {
		t.Fatalf("status %+v, %v", st, err)
	}
	if _, err := r.q.Route(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if j, _ := db.New(r.pool).GetJob(context.Background(), job.ID); j.RoutedTo.String != "workers" {
		t.Fatalf("after disconnecting, routed to %s", j.RoutedTo.String)
	}
	if err := r.svc.SetMode(context.Background(), queue.SpeechAPI); err != ErrNotConnected {
		t.Fatalf("api mode without a connection: %v", err)
	}
}

func TestADrainHandsTheTranscriptionBack(t *testing.T) {
	r := newRig(t)
	r.connect()
	if err := r.svc.SetMode(context.Background(), queue.SpeechAPI); err != nil {
		t.Fatal(err)
	}
	r.fake.block = make(chan struct{})
	drain, drainNow := context.WithCancel(context.Background())
	r.runner.Drain = drain
	r.subtitles()
	done := make(chan error, 1)
	go func() { _, err := r.runner.Run(context.Background()); done <- err }()
	deadline := time.Now().Add(30 * time.Second)
	for len(r.fake.recorded()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	drainNow()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the runner didn't give its attempt back at the drain")
	}
	var status, code string
	if err := r.pool.QueryRow(context.Background(), `SELECT status, error_code FROM attempts`).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "lost" || code != "worker_stopped" {
		t.Fatalf("attempt %s %s, want lost worker_stopped", status, code)
	}
	if j := r.transcription(); j.Status != "queued" || j.FailedCount != 0 {
		t.Fatalf("job %+v", j)
	}
}

func TestConnectChecksTheKeyAndTheFormat(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	res, err := r.svc.Connect(ctx, ConnectRequest{BaseURL: r.fake.srv.URL + "/v1", Model: "m", APIKey: "wrong"})
	if err != nil || res.Connected || len(res.Steps) != 1 || res.Steps[0].Name != "credentials" || res.Steps[0].OK {
		t.Fatalf("a wrong key: %+v, %v", res, err)
	}
	r.fake.script[1] = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid file format"}}`)
	}
	res, err = r.svc.Connect(ctx, ConnectRequest{BaseURL: r.fake.srv.URL + "/v1", Model: "m", APIKey: testKey})
	if err != nil || res.Connected || len(res.Steps) != 2 || res.Steps[1].OK || !strings.Contains(res.Steps[1].Detail, "Invalid file format") {
		t.Fatalf("a refused format: %+v, %v", res, err)
	}
	if st, _ := r.svc.Status(ctx); st.Connection != nil {
		t.Fatal("a failed Connect saved a connection")
	}
	if _, err := r.svc.Connect(ctx, ConnectRequest{BaseURL: "ftp://x", Model: "m"}); err == nil {
		t.Fatal("an ftp URL must be refused")
	}
	r.connect()
	st, err := r.svc.Status(ctx)
	if err != nil || st.Connection == nil || !st.Connection.HasKey || st.Connection.Model != "whisper-large-v3-turbo" {
		t.Fatalf("status %+v, %v", st, err)
	}
	// The sample went up as it is: Connect transcribed the embedded chunk.
	if calls := r.fake.recorded(); !bytes.Equal(calls[len(calls)-1].audio, sample) {
		t.Fatal("Connect didn't send the sample")
	}
	r.noKeyAnywhere()
}

func readAll(s storage.Storage, key string) ([]byte, error) {
	rc, err := s.Get(context.Background(), key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func ffprobeCodec(t *testing.T, audio []byte) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "chunk.m4a")
	if err := os.WriteFile(f, audio, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", f).Output()
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return strings.TrimSpace(string(out))
}
