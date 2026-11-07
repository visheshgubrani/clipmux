package worker

// Transcription from worker to commit (P5-T14), on the real tools: the
// audio endpoint, the speech claim loop, the transcript's commit and what
// it refuses.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

var speechClaim = queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}, Speech: true}

// encodedWithSubtitles takes a clip with the public-domain speech sample
// through the real transcode, with a subtitles track asked for, so the
// commit queues its transcription. It returns the committed output.
func (r *rig) encodedWithSubtitles(t *testing.T) db.Output {
	t.Helper()
	ctx := context.Background()
	clip := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25",
		"-i", filepath.Join("..", "media", "testdata", "speech", "jfk.ogg"), "-t", "11", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", clip).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	f, _ := os.Open(clip)
	defer f.Close()
	if err := r.store.Put(ctx, "uploads/ast_1/source", f); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`,
		`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'workers')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracks.Create(ctx, db.New(r.pool), "prj_default", "ast_1", tracks.Generate{Subtitles: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := r.worker.RunOnce(ctx); err != nil || !got {
		t.Fatalf("transcode: %v, %v", got, err)
	}
	var id string
	if err := r.pool.QueryRow(ctx, `SELECT current_output_id FROM assets WHERE id = 'ast_1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	o, err := db.New(r.pool).GetOutput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (r *rig) speechAPI(t *testing.T, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, r.media+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// The commit queued the transcription; the speech claim takes it, and
// /audio serves the output's audio rendition decrypted: one AAC stream as
// long as the manifest says.
func TestTheAudioEndpointServesTheOutputsAudioDecrypted(t *testing.T) {
	r := newRig(t)
	out := r.encodedWithSubtitles(t)
	c, err := queue.New(r.pool).Claim(context.Background(), speechClaim)
	if err != nil || c == nil || c.Job.Kind != "transcribe" || c.Attempt.InputOutputID.String != out.ID {
		t.Fatalf("speech claim %+v, %v", c, err)
	}
	status, body := r.speechAPI(t, "GET", "/worker/v1/attempts/"+c.Attempt.ID+"/audio", c.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("audio: %d %s", status, body)
	}
	f := filepath.Join(t.TempDir(), "audio.mp4")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name:format=duration", "-of", "default=noprint_wrappers=1", f).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var m media.Manifest
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(probe)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	// AAC works in 1024-sample frames, with an encoder delay at the start
	// and the last frame padded, so the stream runs a few frames (here
	// about 0.12 s) past the source's duration.
	d, _ := strconv.ParseFloat(got["duration"], 64)
	if got["codec_name"] != "aac" || d < m.DurationSeconds-0.25 || d > m.DurationSeconds+0.25 {
		t.Fatalf("audio is %v, want AAC of %.3f s", got, m.DurationSeconds)
	}
	// Another attempt's token, or none, gets nothing.
	if status, _ := r.speechAPI(t, "GET", "/worker/v1/attempts/"+c.Attempt.ID+"/audio", "wrong", nil); status != http.StatusUnauthorized {
		t.Fatalf("a wrong token got %d", status)
	}
}

// A transcript that breaks the rules is 409 transcript_invalid, and a stale
// attempt's transcript commits nothing and writes nothing.
func TestTheCommitRefusesBadAndStaleTranscripts(t *testing.T) {
	r := newRig(t)
	r.encodedWithSubtitles(t)
	ctx := context.Background()
	q := queue.New(r.pool)
	c, err := q.Claim(ctx, speechClaim)
	if err != nil || c == nil {
		t.Fatalf("claim %+v, %v", c, err)
	}
	backwards := transcripts.Transcript{Version: 1, Language: "en", Model: "m@sha256:0", Segments: []transcripts.Segment{
		{ID: "s0", Text: "later", StartMS: 5000, EndMS: 6000}, {ID: "s1", Text: "earlier", StartMS: 1000, EndMS: 2000},
	}}
	status, body := r.speechAPI(t, "POST", "/worker/v1/attempts/"+c.Attempt.ID+"/complete", c.Token, map[string]any{"transcript": backwards})
	if status != http.StatusConflict || !strings.Contains(string(body), "transcript_invalid") {
		t.Fatalf("out of order: %d %s", status, body)
	}

	// The lease lapses and a new attempt takes the job: the old one's
	// transcript is refused, and nothing is stored.
	if _, err := r.pool.Exec(ctx, `UPDATE attempts SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, c.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SweepLostLeases(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if again, err := q.Claim(ctx, speechClaim); err != nil || again == nil {
		t.Fatalf("second claim %+v, %v", again, err)
	}
	good := transcripts.Transcript{Version: 1, Language: "en", Model: "m@sha256:0", Segments: []transcripts.Segment{{ID: "s0", Text: "hi", StartMS: 0, EndMS: 1000}}}
	status, body = r.speechAPI(t, "POST", "/worker/v1/attempts/"+c.Attempt.ID+"/complete", c.Token, map[string]any{"transcript": good})
	if status != http.StatusOK || !strings.Contains(string(body), `"stop"`) {
		t.Fatalf("stale complete: %d %s, want a stop", status, body)
	}
	var trk string
	if err := r.pool.QueryRow(ctx, `SELECT id FROM tracks WHERE asset_id = 'ast_1'`).Scan(&trk); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Head(ctx, tracks.StorageKey("ast_1", trk)); err == nil {
		t.Fatal("a stale attempt wrote the track's file")
	}
	var n int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM transcripts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d transcripts, %v", n, err)
	}
}

// The whole path on the real model: the speech loop transcribes the clip,
// and the track is published with its language and a sealed VTT.
func TestASpeechWorkerTranscribesToAPublishedTrack(t *testing.T) {
	r := newRig(t)
	sp := speechSetup(t, r)
	r.encodedWithSubtitles(t)
	sw := *r.worker
	sw.WorkDir, sw.Kinds, sw.Speech = t.TempDir(), []string{"transcribe"}, &SpeechSlot{}
	sw.Speech.Set(sp)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if got, err := sw.RunOnce(ctx); err != nil || !got {
		t.Fatalf("speech RunOnce = %v, %v", got, err)
	}
	var trk, status, lang, name string
	var keyVersion int32
	if err := r.pool.QueryRow(ctx, `SELECT id, status, language, name, key_version FROM tracks WHERE asset_id = 'ast_1'`).
		Scan(&trk, &status, &lang, &name, &keyVersion); err != nil {
		t.Fatal(err)
	}
	// No playback ID to publish to: ready at once (P5-T15).
	if status != "ready" || lang != "en" || name != "English" || keyVersion != 1 {
		t.Fatalf("track %s %s %s v%d", status, lang, name, keyVersion)
	}
	var body []byte
	if err := r.pool.QueryRow(ctx, `SELECT body FROM transcripts WHERE track_id = $1`, trk).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var tr transcripts.Transcript
	if err := json.Unmarshal(body, &tr); err != nil || !strings.Contains(strings.ToLower(tr.Text()), "ask not what your country") {
		t.Fatalf("transcript %q, %v", tr.Text(), err)
	}
	rc, err := r.store.Get(ctx, tracks.StorageKey("ast_1", trk))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := io.ReadAll(rc)
	rc.Close()
	if bytes.Contains(sealed, []byte("WEBVTT")) {
		t.Fatal("the stored VTT is plaintext")
	}
	keys, _ := contentkey.FromHex(1, "0f0e0d0c0b0a090807060504030201000f0e0d0c0b0a09080706050403020100")
	vtt, err := contentkey.NewRing(keys).OpenTrack("ast_1", trk, tracks.StorageKey("ast_1", trk), sealed)
	if err != nil || !strings.HasPrefix(string(vtt), "WEBVTT\n") || !strings.Contains(string(vtt), " --> ") {
		t.Fatalf("vtt %q, %v", vtt, err)
	}
}

// With one worker, a long transcription doesn't hold up the next upload:
// the media loop claims and encodes it while the speech loop is busy.
func TestAMediaLoopEncodesWhileTheSpeechLoopTranscribes(t *testing.T) {
	r := newRig(t)
	r.encodedWithSubtitles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// A whisper that takes its time, then fails.
	slow := filepath.Join(t.TempDir(), "whisper-cli")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 20\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	models := t.TempDir()
	for _, f := range []string{"m.bin", "v.bin"} {
		if err := os.WriteFile(filepath.Join(models, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sw := *r.worker
	sw.Tools.Whisper = slow
	sw.WorkDir, sw.Kinds, sw.Speech = t.TempDir(), []string{"transcribe"}, &SpeechSlot{}
	sw.Speech.Set(&media.Speech{Backend: media.SpeechCPU, Model: filepath.Join(models, "m.bin"), VAD: filepath.Join(models, "v.bin"), ModelName: "m@sha256:0", Threads: 1})
	speechDone := make(chan error, 1)
	go func() { _, err := sw.RunOnce(ctx); speechDone <- err }()
	waitFor(t, r, `SELECT count(*) FROM attempts a JOIN jobs j ON j.id = a.job_id WHERE j.kind = 'transcribe' AND a.status = 'running'`)

	// A second upload arrives while whisper runs.
	src := filepath.Join(t.TempDir(), "two.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25", "-t", "2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	f, _ := os.Open(src)
	defer f.Close()
	if err := r.store.Put(ctx, "uploads/ast_2/source", f); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_2', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`,
		`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to) VALUES ('job_2', 'prj_default', 'ast_2', 'transcode', 'workers')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.worker.RunOnce(ctx); err != nil || !got {
		t.Fatalf("media RunOnce = %v, %v", got, err)
	}
	var encoded, transcribing int
	if err := r.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM assets WHERE id = 'ast_2' AND status = 'ready'),
		(SELECT count(*) FROM attempts a JOIN jobs j ON j.id = a.job_id WHERE j.kind = 'transcribe' AND a.status = 'running')`).
		Scan(&encoded, &transcribing); err != nil {
		t.Fatal(err)
	}
	if encoded != 1 || transcribing != 1 {
		t.Fatalf("encoded %d, transcribing %d: the encode must finish while the transcription runs", encoded, transcribing)
	}
	cancel()
	<-speechDone
}

func waitFor(t *testing.T, r *rig, countSQL string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		var n int
		if err := r.pool.QueryRow(context.Background(), countSQL).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", countSQL)
}

// speechSetup returns the real model's speech setup on the CPU, or skips
// (fails under CLIPMUX_REQUIRE_MEDIA_TOOLS=1), and points the rig's tools at
// whisper-cli.
func speechSetup(t *testing.T, r *rig) *media.Speech {
	t.Helper()
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
	r.worker.Tools.Whisper = whisper
	return &media.Speech{Backend: media.SpeechCPU, Model: model, VAD: vad, ModelName: media.SpeechModel.ID(), Threads: max(runtime.NumCPU()/2, 1)}
}
