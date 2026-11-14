package lifecycle_test

// Tracks through the integration API (P5-T16): asking for them, reading
// them and their transcripts, regenerating and deleting them, and the
// events that say so.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
	"github.com/visheshgubrani/clipmux/internal/uploads"
)

// raw is a request's status and exact body.
func (r *rig) raw(method, path string, body any, idemKey string) (int, string) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+r.key)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// withSubtitles makes ast_1's output audible and gives it a ready subtitles
// track, transcribed through the queue as a worker would.
func (r *rig) withSubtitles() string {
	r.t.Helper()
	ctx := context.Background()
	r.exec(`UPDATE outputs SET manifest = '{"audio": true, "duration_seconds": 30}' WHERE id = 'out_1'`)
	r.exec(`UPDATE assets SET duration_seconds = 30 WHERE id = 'ast_1'`)
	qs := db.New(r.pool)
	made, err := tracks.Create(ctx, qs, "prj_default", "ast_1", tracks.Generate{Subtitles: true})
	if err != nil {
		r.t.Fatal(err)
	}
	asset, _ := qs.GetAssetByID(ctx, "ast_1")
	if _, err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		r.t.Fatal(err)
	}
	r.commitTranscript("Hello world.")
	return made[0].ID
}

// commitTranscript runs the queued transcription to its commit.
func (r *rig) commitTranscript(text string) {
	r.t.Helper()
	ctx := context.Background()
	c, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}, Speech: true})
	if err != nil || c == nil {
		r.t.Fatalf("claim %+v, %v", c, err)
	}
	secret, _ := contentkey.FromHex(1, strings.Repeat("00", 32))
	pub := &tracks.Publisher{Stores: stores.Fixed(r.store), Keys: contentkey.NewRing(secret)}
	raw, _ := json.Marshal(transcripts.Transcript{Version: 1, Language: "en", Model: "m@sha256:0",
		Segments: []transcripts.Segment{{ID: "s0", Text: text, StartMS: 1234, EndMS: 2345,
			Words: []transcripts.Word{{ID: "s0w0", Text: " " + text, StartMS: 1234, EndMS: 2345}}}}})
	if _, err := r.q.CommitAttempt(ctx, c.Token, pub.CommitTranscript(ctx, raw)); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.routes.SyncRoutes(ctx, 100); err != nil {
		r.t.Fatal(err)
	}
}

func TestChaptersImplySubtitlesWhenAnUploadAsksForThem(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	key, err := apikeys.Authenticate(ctx, r.pool, r.key)
	if err != nil {
		t.Fatal(err)
	}
	up, err := r.upl.Create(ctx, key, uploads.CreateRequest{Size: 10, Generate: tracks.Generate{Chapters: true}}, "")
	if err != nil {
		t.Fatal(err)
	}
	status, body := r.call("GET", "/v1/assets/"+up.AssetID, r.key)
	trks, _ := body["tracks"].([]any)
	if status != 200 || len(trks) != 2 || body["duration_seconds"] != nil {
		t.Fatalf("GET = %d %v", status, body)
	}
	for i, kind := range []string{"subtitles", "chapters"} {
		tr := trks[i].(map[string]any)
		if tr["kind"] != kind || tr["status"] != "preparing" || tr["source"] != "generated" {
			t.Errorf("track %d = %v, want %s preparing", i, tr, kind)
		}
	}
}

func TestTheViewTranscriptAndTrackOfAReadySubtitlesTrack(t *testing.T) {
	r := newRig(t)
	r.ready()
	trk := r.withSubtitles()

	status, body := r.raw("GET", "/v1/assets/ast_1", nil, "")
	var view map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &view); status != 200 || err != nil {
		t.Fatalf("GET = %d %s", status, body)
	}
	if got, want := string(view["tracks"]), `[{"id":"`+trk+`","kind":"subtitles","source":"generated","status":"ready","language":"en","name":"English"}]`; got != want {
		t.Fatalf("tracks = %s\nwant     %s", got, want)
	}
	if got := string(view["duration_seconds"]); got != "30" {
		t.Fatalf("duration_seconds = %s", got)
	}

	status, body = r.raw("GET", "/v1/assets/ast_1/tracks/"+trk+"/transcript", nil, "")
	if want := `{"track_id":"` + trk + `","language":"en","duration_ms":30000,"text":"Hello world.","segments":[{"id":"s0","start_ms":1234,"end_ms":2345,"text":"Hello world."}]}` + "\n"; status != 200 || body != want {
		t.Fatalf("transcript = %d %s\nwant %s", status, body, want)
	}
	status, body = r.raw("GET", "/v1/assets/ast_1/tracks/"+trk+"/transcript?words=true", nil, "")
	if !strings.Contains(body, `"words":[{"id":"s0w0","start_ms":1234,"end_ms":2345,"text":" Hello world."}]`) || status != 200 {
		t.Fatalf("words = %d %s", status, body)
	}
	status, body = r.raw("GET", "/v1/assets/ast_1/tracks/"+trk, nil, "")
	if want := `{"id":"` + trk + `","kind":"subtitles","source":"generated","status":"ready","language":"en","name":"English","duration_ms":30000}` + "\n"; status != 200 || body != want {
		t.Fatalf("track = %d %s", status, body)
	}
	if status, _ := r.raw("GET", "/v1/assets/ast_1/tracks/trk_nope", nil, ""); status != 404 {
		t.Fatalf("an unknown track = %d", status)
	}
	if ev := r.events(); len(ev) == 0 || ev[len(ev)-1] != "video.asset.track.ready" {
		t.Fatalf("events %v", ev)
	}
	var track string
	if err := r.pool.QueryRow(context.Background(), `SELECT payload->'track'->>'id' FROM outbox WHERE payload->>'type' = 'video.asset.track.ready'`).Scan(&track); err != nil || track != trk {
		t.Fatalf("the event names track %q (%v)", track, err)
	}
}

func TestARegenerationKeepsTheOldTrackUntilTheNewOneIsReady(t *testing.T) {
	r := newRig(t)
	r.ready()
	old := r.withSubtitles()

	status, body := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "subtitles"}, "regen-1")
	if status != 202 || !strings.Contains(body, `"status":"preparing"`) || !strings.Contains(body, `"kind":"transcribe"`) {
		t.Fatalf("regenerate = %d %s", status, body)
	}
	// The same key replays the same answer and queues nothing more.
	if again, body2 := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "subtitles"}, "regen-1"); again != 202 || body2 != body {
		t.Fatalf("replay = %d %s\nwant %s", again, body2, body)
	}
	var transcriptions int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE kind = 'transcribe' AND status = 'queued'`).Scan(&transcriptions); err != nil || transcriptions != 1 {
		t.Fatalf("%d queued transcriptions (%v), want 1", transcriptions, err)
	}
	// A second regeneration while it waits: 409, or supersede.
	if status, body := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "subtitles"}, ""); status != 409 || !strings.Contains(body, "job_active") {
		t.Fatalf("while one waits = %d %s", status, body)
	}
	_, view := r.call("GET", "/v1/assets/ast_1", r.key)
	trks := view["tracks"].([]any)
	if len(trks) != 2 || trks[0].(map[string]any)["status"] != "ready" || trks[1].(map[string]any)["status"] != "preparing" {
		t.Fatalf("while the new one is made: %v", trks)
	}
	r.commitTranscript("Hello again.")
	_, view = r.call("GET", "/v1/assets/ast_1", r.key)
	trks = view["tracks"].([]any)
	if len(trks) != 1 || trks[0].(map[string]any)["id"] == old || trks[0].(map[string]any)["status"] != "ready" {
		t.Fatalf("after: %v (old %s retired and hidden)", trks, old)
	}
	// What can't be regenerated.
	if status, body := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "chapters"}, ""); status != 409 || !strings.Contains(body, "chapters_not_connected") {
		t.Fatalf("chapters without an LLM = %d %s", status, body)
	}
	r.exec(`UPDATE outputs SET manifest = '{"audio": false}' WHERE id = 'out_1'`)
	if status, body := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "subtitles"}, ""); status != 409 || !strings.Contains(body, "no_audio") {
		t.Fatalf("a silent video = %d %s", status, body)
	}
	if status, _ := r.raw("POST", "/v1/assets/ast_1/tracks", map[string]any{"kind": "captions"}, ""); status != 400 {
		t.Fatalf("an unknown kind = %d", status)
	}
}

func TestDeletingATrackLeavesNothing(t *testing.T) {
	r := newRig(t)
	r.ready()
	trk := r.withSubtitles()
	if r.stored("media/ast_1/tracks/") != 1 {
		t.Fatal("the track's file isn't stored")
	}
	status, body := r.raw("DELETE", "/v1/assets/ast_1/tracks/"+trk, nil, "")
	if status != 200 || !strings.Contains(body, `"tracks":[]`) {
		t.Fatalf("DELETE = %d %s", status, body)
	}
	if r.stored("media/ast_1/tracks/") != 0 {
		t.Fatal("the track's file survived")
	}
	var transcripts int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM transcripts`).Scan(&transcripts); err != nil || transcripts != 0 {
		t.Fatalf("%d transcripts left", transcripts)
	}
	if status, _ := r.raw("GET", "/v1/assets/ast_1/tracks/"+trk, nil, ""); status != 404 {
		t.Fatalf("GET a deleted track = %d", status)
	}
	if status, _ := r.raw("DELETE", "/v1/assets/ast_1/tracks/"+trk, nil, ""); status != 404 {
		t.Fatalf("deleting it again = %d", status)
	}
	if ev := r.events(); ev[len(ev)-1] != "video.asset.track.deleted" {
		t.Fatalf("events %v", ev)
	}
}
