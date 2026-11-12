package queue_test

// Real-Postgres tests for transcriptions in the queue (P5-T13; P5 "Two job
// families"): a transcription is a routed worker job of the track family,
// beside the asset's encodes.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// speechWorker is the built-in worker's speech loop.
var speechWorker = queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}, Speech: true}

// subtitles adds a preparing subtitles track (and chapters, if asked) to an
// asset and queues its transcription, as an output's commit does.
func (f *fixture) subtitles(assetID string, chapters bool) db.Job {
	f.t.Helper()
	ctx := context.Background()
	qs := db.New(f.pool)
	if _, err := tracks.Create(ctx, qs, "prj_default", assetID, tracks.Generate{Subtitles: true, Chapters: chapters}); err != nil {
		f.t.Fatal(err)
	}
	asset, err := qs.GetAssetByID(ctx, assetID)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		f.t.Fatal(err)
	}
	jobs := f.jobsOf(assetID)
	for _, j := range jobs {
		if j.Kind == "transcribe" {
			return j
		}
	}
	f.t.Fatalf("no transcription queued for %s: %+v", assetID, jobs)
	return db.Job{}
}

// track returns an asset's track of a kind: its status and error code.
func (f *fixture) track(assetID, kind string) (status, code string) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, coalesce(error_code, '') FROM tracks WHERE asset_id = $1 AND kind = $2 ORDER BY created_at DESC LIMIT 1`,
		assetID, kind).Scan(&status, &code); err != nil {
		f.t.Fatal(err)
	}
	return status, code
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func TestATranscriptionRunsBesideAReencode(t *testing.T) {
	f := newFixture(t)
	asset := f.asset("ast_1", true)
	tr := f.subtitles(asset, false)
	if tr.RoutedTo.String != "workers" || tr.RouteReason.String != "primary" || !tr.TrackID.Valid {
		t.Fatalf("transcription %+v: want routed to the primary, for its track", tr)
	}
	re := f.enqueue(asset, "reencode")

	// The media loop takes the re-encode and never the transcription; the
	// speech loop takes the transcription.
	media := f.claim(builtin)
	if media == nil || media.Job.ID != re.ID {
		t.Fatalf("media claim = %+v, want the re-encode", media)
	}
	sp := f.claim(speechWorker)
	if sp == nil || sp.Job.ID != tr.ID {
		t.Fatalf("speech claim = %+v, want the transcription", sp)
	}
	// It writes no output and reads the current one, kept while it runs.
	if sp.Output.ID != "" || sp.Attempt.OutputID.Valid || sp.Attempt.InputOutputID.String != "out_ast_1" {
		t.Fatalf("transcription attempt %+v: want no output, reading out_ast_1", sp.Attempt)
	}

	// One track job at a time: a second transcription is refused by the
	// family's index, and so is a move, which excludes both families.
	_, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: asset, Kind: "transcribe", TrackID: tr.TrackID.String})
	if !uniqueViolation(err) {
		t.Fatalf("a second transcription: %v, want a unique violation", err)
	}
	f.exec(`INSERT INTO storage_configs (id, project_id, kind) VALUES ('sto_two', 'prj_default', 'fs')`)
	if _, err := lifecycle.QueueMove(context.Background(), db.New(f.pool), "prj_default", asset,
		lifecycle.Move{Source: "sto_default", Target: "sto_two", MediaSource: "sto_default", MediaTarget: "sto_two"}); !uniqueViolation(err) {
		t.Fatalf("a move during a transcription: %v, want a unique violation", err)
	}
}

func TestOnlyAWorkerWithSpeechTakesATranscription(t *testing.T) {
	f := newFixture(t)
	tr := f.subtitles(f.asset("ast_1", true), false)
	if c := f.claim(queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}}); c != nil {
		t.Fatalf("a worker without speech claimed %s", c.Job.ID)
	}
	if c := f.claim(builtin); c != nil {
		t.Fatalf("a media claim took %s", c.Job.ID)
	}
	if c := f.claim(speechWorker); c == nil || c.Job.ID != tr.ID {
		t.Fatalf("speech claim = %+v", c)
	}
}

func TestReleasingATranscriptionDeletesItsAttemptAndRequeues(t *testing.T) {
	f := newFixture(t)
	tr := f.subtitles(f.asset("ast_1", true), false)
	c := f.claim(speechWorker)
	if err := f.q.Release(context.Background(), c.Token); err != nil {
		t.Fatal(err)
	}
	if j := f.job(tr.ID); j.Status != "queued" || j.LostCount != 0 {
		t.Fatalf("job after release %+v", j)
	}
	if again := f.claim(speechWorker); again == nil || again.Attempt.AttemptNumber != 1 {
		t.Fatalf("claim after release = %+v, want attempt 1 again", again)
	}
}

func TestAFailedTranscriptionLeavesTheAssetReady(t *testing.T) {
	f := newFixture(t)
	asset := f.asset("ast_1", true)
	f.exec(`UPDATE assets SET status = 'ready' WHERE id = $1`, asset)
	f.subtitles(asset, true)
	c := f.claim(speechWorker)
	if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Permanent: true, Code: "whisper_crashed"}); err != nil {
		t.Fatal(err)
	}
	if status, code, _ := f.assetRow(asset); status != "ready" || code != "" {
		t.Fatalf("asset %s %q after a failed transcription", status, code)
	}
	if status, code := f.track(asset, "subtitles"); status != "errored" || code != "transcribe_failed" {
		t.Fatalf("subtitles %s %s, want errored transcribe_failed", status, code)
	}
	if status, code := f.track(asset, "chapters"); status != "errored" || code != "subtitles_failed" {
		t.Fatalf("chapters %s %s, want errored subtitles_failed", status, code)
	}
	// The app hears about each track, and nothing about the asset: a track
	// failing is not the asset failing.
	if ev := f.events(); strings.Join(ev, ",") != "video.asset.track.errored,video.asset.track.errored" {
		t.Fatalf("events %v", ev)
	}
}

func TestATranscriptionNobodyTakesFor24HoursErrors(t *testing.T) {
	f := newFixture(t)
	asset := f.asset("ast_1", true)
	young := f.subtitles(asset, true)
	if n, err := f.q.SweepStaleTranscriptions(context.Background(), 10); err != nil || n != 0 {
		t.Fatalf("swept %d (%v) at once", n, err)
	}
	f.exec(`UPDATE jobs SET created_at = now() - interval '24 hours 1 minute' WHERE id = $1`, young.ID)
	if n, err := f.q.SweepStaleTranscriptions(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("swept %d (%v), want 1", n, err)
	}
	if j := f.job(young.ID); j.Status != "failed" || j.ErrorCode.String != "transcribe_unavailable" {
		t.Fatalf("job %+v", j)
	}
	if status, code := f.track(asset, "subtitles"); status != "errored" || code != "transcribe_unavailable" {
		t.Fatalf("subtitles %s %s", status, code)
	}
	if status, code := f.track(asset, "chapters"); status != "errored" || code != "subtitles_failed" {
		t.Fatalf("chapters %s %s", status, code)
	}
}

func TestATranscriptionGoesToTheFallbackWhenNoPCCanTranscribe(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.agent("agt_1", true, 0) // online, but reports no speech
	f.builtinSeen(0)
	f.exec(`UPDATE projects SET builtin_speech = true WHERE id = 'prj_default'`)
	asset := f.asset("ast_1", true)
	tr := f.subtitles(asset, false)
	re := f.enqueue(asset, "reencode")
	f.route()
	f.wantRoute(tr.ID, "builtin", "primary_offline")
	f.wantRoute(re.ID, "agents", "primary") // the PC can still encode

	// Once the PC reports speech, transcriptions stay on it.
	f.exec(`UPDATE agents SET capabilities = '{"speech": {"backends": ["vulkan"], "model": "m"}}' WHERE id = 'agt_1'`)
	f.exec(`UPDATE jobs SET routed_to = 'agents', route_reason = 'primary' WHERE id = $1`, tr.ID)
	f.route()
	f.wantRoute(tr.ID, "agents", "primary")
}

func TestTheFallbackBarIsScopedToAFamily(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	f.exec(`UPDATE projects SET builtin_speech = true WHERE id = 'prj_default'`)
	asset := f.asset("ast_1", true)

	// A transcription failed in a way that may be the file's.
	tr := f.subtitles(asset, false)
	f.exec(`UPDATE jobs SET routed_to = 'builtin' WHERE id = $1`, tr.ID)
	c := f.claim(speechWorker)
	if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Code: "transcribe_failed"}); err != nil {
		t.Fatal(err)
	}
	f.claimable(tr.ID)
	re := f.enqueue(asset, "reencode")
	f.exec(`UPDATE jobs SET routed_to = 'agents' WHERE id = $1`, tr.ID)
	f.route()
	f.wantRoute(tr.ID, "agents", "fallback_refused")
	f.wantRoute(re.ID, "builtin", "primary_offline") // the encode isn't barred
}

func TestAFailedEncodeDoesNotBarATranscription(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	f.exec(`UPDATE projects SET builtin_speech = true WHERE id = 'prj_default'`)
	other := f.asset("ast_2", true)
	enc := f.enqueue(other, "reencode")
	f.exec(`UPDATE jobs SET routed_to = 'builtin' WHERE id = $1`, enc.ID)
	ec := f.claim(builtin)
	if ec == nil || ec.Job.ID != enc.ID {
		t.Fatalf("claim = %+v", ec)
	}
	if _, err := f.q.Fail(context.Background(), ec.Token, queue.Failure{Code: "transcode_failed"}); err != nil {
		t.Fatal(err)
	}
	f.claimable(enc.ID)
	f.exec(`UPDATE jobs SET routed_to = 'agents' WHERE id = $1`, enc.ID)
	tr2 := f.subtitles(other, false)
	f.exec(`UPDATE jobs SET routed_to = 'agents' WHERE id = $1`, tr2.ID)
	f.route()
	f.wantRoute(enc.ID, "agents", "fallback_refused")
	f.wantRoute(tr2.ID, "builtin", "primary_offline")
}

func TestATranscriptionIsQueuedOnlyWithAudio(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	qs := db.New(f.pool)
	silent := f.asset("ast_silent", true)
	if _, err := tracks.Create(ctx, qs, "prj_default", silent, tracks.Generate{Chapters: true}); err != nil {
		t.Fatal(err)
	}
	asset, err := qs.GetAssetByID(ctx, silent)
	if err != nil {
		t.Fatal(err)
	}
	if err := tracks.QueueTranscriptions(ctx, qs, asset, false); err != nil {
		t.Fatal(err)
	}
	if jobs := f.jobsOf(silent); len(jobs) != 0 {
		t.Fatalf("jobs for a silent video: %+v", jobs)
	}
	if status, code := f.track(silent, "subtitles"); status != "errored" || code != "no_audio" {
		t.Fatalf("subtitles %s %s, want errored no_audio", status, code)
	}
	if status, code := f.track(silent, "chapters"); status != "errored" || code != "subtitles_failed" {
		t.Fatalf("chapters %s %s", status, code)
	}
	_ = time.Second
}
