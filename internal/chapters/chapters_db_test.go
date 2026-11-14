package chapters

// Chapters in serve on real Postgres (P5-T17): Connect against fakes of
// both providers, the job that follows ready subtitles, its commit through
// the routes, retries and permanent failures, the drain, a job cancelled
// while the model answers, and the key kept out of every row and log.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

type dbRig struct {
	t        *testing.T
	pool     *pgxpool.Pool
	store    storage.Storage
	keys     contentkey.Ring
	svc      *Service
	runner   *Runner
	logs     *bytes.Buffer
	subs     string // the subtitles track
	chapters string // the chapters track
}

// newDBRig makes ast_1 ready with an audible 30.789 s output and asks for
// chapters: its subtitles are transcribed through the queue as a worker
// would, which queues the chapters job.
func newDBRig(t *testing.T) *dbRig {
	t.Helper()
	ctx := context.Background()
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
	pub := &tracks.Publisher{Stores: stores.Fixed(store), Keys: keys}
	svc := NewService(pool, box, db.DefaultProjectID)
	svc.Logger = logger
	r := &dbRig{t: t, pool: pool, store: store, keys: keys, svc: svc, logs: logs,
		runner: &Runner{Pool: pool, Box: box, Publisher: pub, HTTP: http.DefaultClient, Logger: logger,
			Backoff: func(int32) time.Duration { return 0 }}}
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri, duration_seconds) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_1/source', 30.789)`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{"audio": true, "duration_seconds": 30.789}')`)
	r.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_1' WHERE id = 'ast_1'`)
	qs := db.New(pool)
	made, err := tracks.Create(ctx, qs, db.DefaultProjectID, "ast_1", tracks.Generate{Chapters: true})
	if err != nil {
		t.Fatal(err)
	}
	r.subs, r.chapters = made[0].ID, made[1].ID
	asset, _ := qs.GetAssetByID(ctx, "ast_1")
	if _, err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		t.Fatal(err)
	}
	q := queue.New(pool)
	c, err := q.Claim(ctx, queue.Worker{Executor: "builtin", Kinds: []string{tracks.JobTranscribe}, Speech: true})
	if err != nil || c == nil {
		t.Fatalf("claim %+v, %v", c, err)
	}
	raw, _ := json.Marshal(transcripts.Transcript{Version: 1, Language: "en", Model: "m@sha256:0", Segments: []transcripts.Segment{
		{ID: "s0", Text: "Hello world.", StartMS: 1234, EndMS: 2345},
		{ID: "s1", Text: "More of the same.", StartMS: 11234, EndMS: 12345},
		{ID: "s2", Text: "New topic.", StartMS: 21234, EndMS: 22345},
	}})
	if _, err := q.CommitAttempt(ctx, c.Token, pub.CommitTranscript(ctx, raw)); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *dbRig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *dbRig) scalar(sql string, args ...any) any {
	r.t.Helper()
	var v any
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func (r *dbRig) track(id string) db.Track {
	r.t.Helper()
	t, err := db.New(r.pool).GetTrack(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return t
}

func (r *dbRig) run() {
	r.t.Helper()
	if _, err := r.runner.Run(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// connectOpenAI connects the fake, which answers /models first and then
// whatever replies follow.
func (r *dbRig) connectOpenAI(replies ...reply) *recorder {
	r.t.Helper()
	rec := newRecorder(r.t, append([]reply{{200, `{"data":[]}`}}, replies...)...)
	res, err := r.svc.Connect(context.Background(), ConnectRequest{Provider: ProviderOpenAI, BaseURL: rec.srv.URL + "/v1", Model: "gpt-5-mini", APIKey: llmKey})
	if err != nil || !res.Connected {
		r.t.Fatalf("Connect = %+v, %v", res, err)
	}
	return rec
}

func (r *dbRig) events() []string {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT payload->>'type' || ':' || coalesce(payload->'track'->>'kind', '') || ':' || coalesce(payload->'track'->>'error_code', '') FROM outbox WHERE kind = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// noKeyAnywhere checks every text column of every table, and the logs.
func (r *dbRig) noKeyAnywhere() {
	r.t.Helper()
	ctx := context.Background()
	rows, err := r.pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		r.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tbl := range tables {
		var n int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM "`+tbl+`" x WHERE x::text LIKE '%' || $1 || '%'`, llmKey).Scan(&n); err != nil {
			r.t.Fatal(err)
		}
		if n > 0 {
			r.t.Errorf("table %s holds the LLM key in %d rows", tbl, n)
		}
	}
	if strings.Contains(r.logs.String(), llmKey) {
		r.t.Error("the logs hold the LLM key")
	}
}

func TestChaptersFollowReadySubtitlesAndPublish(t *testing.T) {
	r := newDBRig(t)
	if got := r.track(r.subs).Status; got != tracks.Ready {
		t.Fatalf("subtitles = %s", got)
	}
	if n := r.scalar(`SELECT count(*) FROM jobs WHERE kind = 'chapters' AND status = 'queued' AND track_id = $1`, r.chapters); n != int64(1) {
		t.Fatalf("%v queued chapters jobs, want 1 once the subtitles are ready", n)
	}
	rec := r.connectOpenAI(chatReply(`{"chapters":[{"start_segment_id":"s0","title":"Intro"}]}`), chatReply(outlineJSON))
	r.run()

	tr := r.track(r.chapters)
	if tr.Status != tracks.Ready || tr.Language.String != "en" || tr.Name.String != "Chapters" || tr.DurationMs.Int64 != 30789 || tr.OutputID.String != "out_1" {
		t.Fatalf("chapters track = %+v", tr)
	}
	if got := string(tr.Chapters); got != `[{"title": "Intro", "end_ms": 21234, "start_ms": 0}, {"title": "Topic", "end_ms": 30789, "start_ms": 21234}]` {
		t.Fatalf("chapters = %s", got)
	}
	if st := r.scalar(`SELECT status FROM jobs WHERE kind = 'chapters'`); st != "succeeded" {
		t.Fatalf("job = %v", st)
	}
	// Connect's sample and this job: one call each.
	if n := len(rec.requests()); n != 3 {
		t.Fatalf("%d requests, want models, the sample and the job", n)
	}
	key := tracks.StorageKey("ast_1", r.chapters)
	rc, err := r.store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := io.ReadAll(rc)
	rc.Close()
	if bytes.Contains(sealed, []byte("Intro")) {
		t.Fatal("the stored chapters VTT is plaintext")
	}
	plain, err := r.keys.OpenTrack("ast_1", r.chapters, key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n" + transcripts.TimestampMap + "\n\n1\n00:00:00.000 --> 00:00:21.234\nIntro\n\n2\n00:00:21.234 --> 00:00:30.789\nTopic\n\n"
	if string(plain) != want {
		t.Fatalf("VTT = %q", plain)
	}
	got := strings.Join(r.events(), " ")
	if want := "video.asset.track.ready:subtitles: video.asset.track.ready:chapters:"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	r.noKeyAnywhere()
}

func TestTransientFailuresBackOffThenErrorOnlyTheTrack(t *testing.T) {
	r := newDBRig(t)
	rec := r.connectOpenAI(chatReply(outlineJSON), reply{503, `{"error":{"message":"overloaded"}}`})
	for range 3 {
		r.run()
	}
	tr := r.track(r.chapters)
	if tr.Status != tracks.Errored || tr.ErrorCode.String != CodeUnavailable {
		t.Fatalf("track = %s %s", tr.Status, tr.ErrorCode.String)
	}
	if st := r.scalar(`SELECT status || ':' || failed_count || ':' || error_code FROM jobs WHERE kind = 'chapters'`); st != "failed:3:llm_unavailable" {
		t.Fatalf("job = %v", st)
	}
	if n := len(rec.requests()); n != 2+3 {
		t.Fatalf("%d requests, want 2 at Connect and one per try", n)
	}
	if st := r.scalar(`SELECT status FROM assets WHERE id = 'ast_1'`); st != "ready" {
		t.Fatalf("asset = %v", st)
	}
	if got := r.events(); got[len(got)-1] != "video.asset.track.errored:chapters:llm_unavailable" {
		t.Fatalf("events = %v", got)
	}
	r.noKeyAnywhere()
}

func TestAClaudeRefusalErrorsTheTrackForGood(t *testing.T) {
	r := newDBRig(t)
	refusal := claudeReply("refusal", "")
	rec := newRecorder(t, reply{200, `{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-01-01T00:00:00Z"}`},
		claudeReply("end_turn", `{"chapters":[{"start_segment_id":"s0","title":"Setup"},{"start_segment_id":"s2","title":"First program"}]}`), refusal)
	res, err := r.svc.Connect(context.Background(), ConnectRequest{Provider: ProviderAnthropic, BaseURL: rec.srv.URL, APIKey: llmKey})
	if err != nil || !res.Connected || !strings.Contains(res.Steps[1].Detail, `"First program"`) {
		t.Fatalf("Connect = %+v, %v", res, err)
	}
	st, _ := r.svc.Status(context.Background())
	if st.Connection == nil || st.Connection.Provider != ProviderAnthropic || st.Connection.Model != DefaultClaudeModel || !st.Connection.HasKey {
		t.Fatalf("Status = %+v", st.Connection)
	}
	r.run()
	if tr := r.track(r.chapters); tr.Status != tracks.Errored || tr.ErrorCode.String != CodeRefused {
		t.Fatalf("track = %s %s", tr.Status, tr.ErrorCode.String)
	}
	if n := len(rec.requests()); n != 3 {
		t.Fatalf("%d requests: a refusal is never retried", n)
	}
	r.noKeyAnywhere()
}

func TestWithoutAConnectionChaptersEndLLMNotConnected(t *testing.T) {
	r := newDBRig(t)
	r.run()
	if tr := r.track(r.chapters); tr.Status != tracks.Errored || tr.ErrorCode.String != CodeNotConnected {
		t.Fatalf("track = %s %s", tr.Status, tr.ErrorCode.String)
	}
	if tr := r.track(r.subs); tr.Status != tracks.Ready {
		t.Fatalf("subtitles = %s", tr.Status)
	}
}

func TestConnectFallsBackToJSONModeAndAFailedStepSavesNothing(t *testing.T) {
	r := newDBRig(t)
	bad := newRecorder(t, reply{200, `{"data":[]}`}, reply{401, `{"error":{"message":"bad key"}}`})
	res, err := r.svc.Connect(context.Background(), ConnectRequest{Provider: ProviderOpenAI, BaseURL: bad.srv.URL, Model: "m", APIKey: llmKey})
	if err != nil || res.Connected || len(res.Steps) != 2 || res.Steps[1].OK {
		t.Fatalf("Connect = %+v, %v", res, err)
	}
	if st, _ := r.svc.Status(context.Background()); st.Connection != nil {
		t.Fatal("a failed Connect saved a connection")
	}
	// No strict schemas on this server: the schema request is refused, JSON mode works.
	rec := newRecorder(t, reply{200, `{"data":[]}`}, reply{400, `{"error":{"message":"response_format json_schema is not supported"}}`}, chatReply(outlineJSON))
	res, err = r.svc.Connect(context.Background(), ConnectRequest{Provider: ProviderOpenAI, BaseURL: rec.srv.URL, Model: "llama3.3"})
	if err != nil || !res.Connected {
		t.Fatalf("Connect = %+v, %v", res, err)
	}
	st, _ := r.svc.Status(context.Background())
	if st.Connection.ResponseFormat != FormatJSONObject || st.Connection.HasKey {
		t.Fatalf("Status = %+v", st.Connection)
	}
	if reqs := rec.requests(); reqs[2].body["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("third request = %v", reqs[2].body)
	}
	for _, bad := range []ConnectRequest{
		{Provider: "gemini", Model: "x"},
		{Provider: ProviderOpenAI, BaseURL: "ftp://x", Model: "m"},
		{Provider: ProviderOpenAI, BaseURL: "https://u:p@x", Model: "m"},
		{Provider: ProviderOpenAI, BaseURL: "https://x"},
		{Provider: ProviderAnthropic},
	} {
		if _, err := r.svc.Connect(context.Background(), bad); err == nil {
			t.Errorf("Connect(%+v) passed", bad)
		}
	}
	if err := r.svc.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Disconnect(context.Background()); err != ErrNotConnected {
		t.Fatalf("second Disconnect = %v", err)
	}
}

// blocker is a Model that waits until released or cancelled.
type blocker struct {
	started chan struct{}
	release chan struct{}
}

func (b *blocker) Complete(ctx context.Context, _ Prompt) (json.RawMessage, error) {
	close(b.started)
	select {
	case <-b.release:
		return json.RawMessage(outlineJSON), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestADrainHandsTheJobBackWithoutSpendingATry(t *testing.T) {
	r := newDBRig(t)
	drain, stop := context.WithCancel(context.Background())
	b := &blocker{started: make(chan struct{}), release: make(chan struct{})}
	r.runner.Drain = drain
	r.runner.OpenModel = func(context.Context, string) (Model, error) { return b, nil }
	done := make(chan error)
	go func() { _, err := r.runner.Run(context.Background()); done <- err }()
	<-b.started
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := r.scalar(`SELECT status || ':' || failed_count || ':' || (available_at <= now()) FROM jobs WHERE kind = 'chapters'`); st != "running:0:true" {
		t.Fatalf("job = %v, want claimable again with no try spent", st)
	}
	if tr := r.track(r.chapters); tr.Status != tracks.Preparing {
		t.Fatalf("track = %s", tr.Status)
	}
	// After the restart it runs.
	r.runner.Drain = nil
	r.runner.OpenModel = func(context.Context, string) (Model, error) { return &script{answers: []string{outlineJSON}}, nil }
	r.run()
	if tr := r.track(r.chapters); tr.Status != tracks.Ready {
		t.Fatalf("track after restart = %s", tr.Status)
	}
}

func TestAJobCancelledWhileTheModelAnswersCommitsNothing(t *testing.T) {
	r := newDBRig(t)
	b := &blocker{started: make(chan struct{}), release: make(chan struct{})}
	r.runner.OpenModel = func(context.Context, string) (Model, error) { return b, nil }
	done := make(chan error)
	go func() { _, err := r.runner.Run(context.Background()); done <- err }()
	<-b.started
	r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE kind = 'chapters'`)
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if tr := r.track(r.chapters); tr.Status != tracks.Preparing || tr.Chapters != nil {
		t.Fatalf("track = %s %s", tr.Status, tr.Chapters)
	}
	if _, err := r.store.Get(context.Background(), tracks.StorageKey("ast_1", r.chapters)); err == nil {
		t.Fatal("a cancelled job stored its VTT")
	}
}

func TestDeletedSubtitlesFailTheirChapters(t *testing.T) {
	r := newDBRig(t)
	r.exec(`UPDATE tracks SET status = 'deleted' WHERE id = $1`, r.subs)
	r.runner.OpenModel = func(context.Context, string) (Model, error) { return &script{answers: []string{outlineJSON}}, nil }
	r.run()
	if tr := r.track(r.chapters); tr.Status != tracks.Errored || tr.ErrorCode.String != tracks.CodeSubtitlesFailed {
		t.Fatalf("track = %s %s", tr.Status, tr.ErrorCode.String)
	}
}
