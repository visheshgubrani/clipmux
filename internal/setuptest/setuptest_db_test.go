package setuptest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/delivery"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio/index.m3u8",GROUP-ID="a",NAME="x"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="a"
360p/index.m3u8
`

const mediaPL = `#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="../key/1",IV=0x00000000000000000000000000000001
#EXTINF:6.000,
seg_00001.m4s
#EXT-X-ENDLIST
`

// fragment is the start of a CMAF media segment: styp, then moof.
var fragment = append([]byte("\x00\x00\x00\x18stypmsdh\x00\x00\x00\x00msdhmsix\x00\x00\x00\x10moof"), bytes.Repeat([]byte{0}, 40)...)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *storage.Filesystem
	ring   contentkey.Ring
	runner *Runner
}

// newRig serves delivery for a hand-built ready asset (ast_1, signed
// pid_1, output out_1) and points the media URL at mediaURL, or at
// delivery itself when it is "".
func newRig(t *testing.T, mediaURL string, encrypted bool) *rig {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{6}, 32))
	if err := signingkeys.EnsureInternal(ctx, pool, box, "prj_default"); err != nil {
		t.Fatal(err)
	}
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	secret, _ := contentkey.FromHex(1, strings.Repeat("ab", 32))
	ring := contentkey.NewRing(secret)
	mux := http.NewServeMux()
	delivery.New(pool, stores.Fixed(store), ring).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	if mediaURL == "" {
		mediaURL = srv.URL
	}
	urls, err := settings.Load(ctx, pool, "prj_default", config.Config{PublicURL: srv.URL, MediaURL: mediaURL, URLsPinned: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
		INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{}');
		UPDATE assets SET status = 'ready', current_output_id = 'out_1';
		INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'signed');`); err != nil {
		t.Fatal(err)
	}
	seg := fragment
	if encrypted {
		key := secret.Derive("ast_1", "out_1")
		iv := make([]byte, 16)
		iv[15] = 1
		pad := aes.BlockSize - len(fragment)%aes.BlockSize
		plain := append(bytes.Clone(fragment), bytes.Repeat([]byte{byte(pad)}, pad)...)
		block, _ := aes.NewCipher(key[:])
		seg = make([]byte, len(plain))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(seg, plain)
	}
	for name, body := range map[string][]byte{
		"master.m3u8": []byte(master), "360p/index.m3u8": []byte(mediaPL), "360p/init.mp4": []byte("init"),
		"360p/seg_00001.m4s": seg, "audio/index.m3u8": []byte(mediaPL), "poster.jpg": []byte("\xff\xd8jpeg"),
	} {
		if err := store.Put(ctx, "media/ast_1/out_1/"+name, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	return &rig{t: t, pool: pool, store: store, ring: ring, runner: New(pool, stores.Fixed(store), box, urls, nil, nil, "")}
}

func names(checks []Check) []string {
	var out []string
	for _, c := range checks {
		s := c.Name + "=ok"
		if !c.OK {
			s = c.Name + "=" + c.Detail
		}
		out = append(out, s)
	}
	return out
}

func TestPlaybackChecksPassOnAnEncryptedOutput(t *testing.T) {
	r := newRig(t, "", true)
	got := names(r.runner.playbackChecks(context.Background(), "ast_1"))
	want := []string{
		"play_master=ok", "play_key=ok", "play_segment_decrypts=ok",
		"thumbnail_token_refused_segment=ok", "thumbnail_token_refused_key=ok", "thumbnail_token_poster=ok",
		"no_token_refused=ok", "stored_segment_encrypted=ok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlaybackChecksFailOnCleartextSegments(t *testing.T) {
	r := newRig(t, "", false)
	got := names(r.runner.playbackChecks(context.Background(), "ast_1"))
	for _, want := range []string{
		"play_segment_decrypts=the segment doesn't decrypt to an fMP4 fragment with the delivered key",
		"stored_segment_encrypted=the stored segment is a plain fMP4 fragment, not ciphertext",
		"no_token_refused=ok",
	} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("checks %v lack %q", got, want)
		}
	}
}

func TestPlaybackChecksFailWhenTheMediaURLMissesDelivery(t *testing.T) {
	elsewhere := httptest.NewServer(http.NotFoundHandler())
	defer elsewhere.Close()
	r := newRig(t, elsewhere.URL, true)
	got := names(r.runner.playbackChecks(context.Background(), "ast_1"))
	if got[0] != "play_master=HTTP 404" || got[1] != "play_key=skipped: play_master failed" || len(got) != len(PlaybackChecks) {
		t.Fatalf("checks = %v; want play_master to fail with HTTP 404 and the rest skipped", got)
	}
}

func TestOnlyOneSetupTestRunsAtATime(t *testing.T) {
	r := newRig(t, "", true)
	ctx := context.Background()
	a, err := r.runner.Queue(ctx, "manual")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.runner.Queue(ctx, "urls_saved")
	if err != nil || b.ID != a.ID || b.Status != "queued" {
		t.Fatalf("second queue = %+v, %v; want the queued %s", b, err, a.ID)
	}
	var n int
	_ = r.pool.QueryRow(ctx, `SELECT count(*) FROM setup_tests`).Scan(&n)
	if n != 1 {
		t.Fatalf("setup tests = %d, want 1", n)
	}
}

func TestWaitingForTheSampleReportsAShutdownAsAShutdown(t *testing.T) {
	r := newRig(t, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.runner.waitReady(ctx, "ast_never")
	if err == nil || strings.Contains(err.Error(), "not ready after") || !strings.Contains(err.Error(), "stopped before the sample was ready") {
		t.Fatalf("waitReady after shutdown = %v, want a shutdown error rather than the encode timeout", err)
	}
}

// withSubtitles gives ast_1 a ready subtitles track whose VTT holds cue,
// sealed as serve stores it (or plain, when sealed is false).
func (r *rig) withSubtitles(cue string, sealed bool) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `INSERT INTO tracks (id, project_id, asset_id, kind, status, language, name, duration_ms, key_version, output_id, ready_at)
		VALUES ('trk_1', 'prj_default', 'ast_1', 'subtitles', 'ready', 'en', 'English', 11000, 1, 'out_1', now())`); err != nil {
		r.t.Fatal(err)
	}
	body := []byte("WEBVTT\n" + transcripts.TimestampMap + "\n\n1\n" + cue + "\n\n")
	key := tracks.StorageKey("ast_1", "trk_1")
	if sealed {
		var err error
		if body, err = contentkey.SealTrack(r.ring.Current(), "ast_1", "trk_1", key, body); err != nil {
			r.t.Fatal(err)
		}
	}
	if err := r.store.Put(ctx, key, bytes.NewReader(body)); err != nil {
		r.t.Fatal(err)
	}
}

func TestTrackChecksPlayTheSamplesWordAtItsTime(t *testing.T) {
	r := newRig(t, "", true)
	r.withSubtitles("00:00:00.300 --> 00:00:03.500\nAnd so, my fellow Americans, ask not", true)
	got := names(r.runner.trackChecks(context.Background(), "ast_1"))
	want := []string{"subtitles_ready=ok", "play_subtitles=ok", "thumbnail_token_refused_track=ok", "stored_track_encrypted=ok"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTrackChecksFailOnSubtitlesOutOfTime(t *testing.T) {
	r := newRig(t, "", true)
	// The word, but 3.4 s late: past the half-second allowed.
	r.withSubtitles("00:00:02.600 --> 00:00:05.000\nmy fellow Americans", true)
	got := names(r.runner.trackChecks(context.Background(), "ast_1"))
	if !strings.HasPrefix(got[1], `play_subtitles=no cue says "americans" at 1.6 s ± 0.5 s`) || got[2] != "thumbnail_token_refused_track=ok" {
		t.Fatalf("checks = %v", got)
	}
}

func TestTrackChecksFailOnPlainStoredSubtitles(t *testing.T) {
	r := newRig(t, "", true)
	r.withSubtitles("00:00:00.300 --> 00:00:03.500\nmy fellow Americans", false)
	got := names(r.runner.trackChecks(context.Background(), "ast_1"))
	if got[3] != "stored_track_encrypted=the stored subtitles are plain WebVTT, not ciphertext" {
		t.Fatalf("checks = %v", got)
	}
}

func TestTrackChecksWaitThenFailOnAnErroredTranscription(t *testing.T) {
	r := newRig(t, "", true)
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO tracks (id, project_id, asset_id, kind, status, error_code) VALUES ('trk_1', 'prj_default', 'ast_1', 'subtitles', 'errored', 'transcribe_failed')`); err != nil {
		t.Fatal(err)
	}
	got := names(r.runner.trackChecks(context.Background(), "ast_1"))
	want := []string{"subtitles_ready=the transcription failed: transcribe_failed", "play_subtitles=skipped: subtitles_ready failed",
		"thumbnail_token_refused_track=skipped: subtitles_ready failed", "stored_track_encrypted=skipped: subtitles_ready failed"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks = %v", got)
	}
}

// A provider with no capacity for one wake isn't a broken provider: the
// router tries again, and the test waits (RunPod answered "no instances
// available" once, then woke a pod seconds later, D097). Three failed wakes
// in a row, when the provider backs off, or a machine that never claimed,
// fail the test at once.
func TestTheEncodeCheckWaitsOutOneFailedWake(t *testing.T) {
	r := newRig(t, "", true)
	ctx := context.Background()
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_p', 'prj_default', 'app_dashboard', 'processing', 'sto_default');
		INSERT INTO jobs (id, project_id, asset_id, kind, status, routed_to) VALUES ('job_p', 'prj_default', 'ast_p', 'transcode', 'queued', 'runpod');
		INSERT INTO executor_configs (id, project_id, kind, config, secrets_encrypted, max_concurrent, monthly_limit_cents, rate_cents_per_hour, disk_bytes)
			VALUES ('exe_runpod', 'prj_default', 'runpod', '{}', 'sealed', 1, 100, 80, 1)`); err != nil {
		t.Fatal(err)
	}
	ended := func(id, reason string) {
		t.Helper()
		if _, err := r.pool.Exec(ctx, `INSERT INTO dispatches (id, project_id, job_id, executor_config_id, kind, status, end_reason, key_hash, key_sealed, rate_cents_per_hour, claim_deadline, ended_at)
			VALUES ($1, 'prj_default', 'job_p', 'exe_runpod', 'runpod', 'ended', $2, convert_to($1::text, 'UTF8'), 'x', 80, now(), now())`, id, reason); err != nil {
			t.Fatal(err)
		}
	}
	r.runner.EncodeTimeout, r.runner.Poll = 500*time.Millisecond, 20*time.Millisecond

	ended("dsp_1", "wake_failed")
	if err := r.runner.waitReady(ctx, "ast_p"); err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Fatalf("after one failed wake: %v, want the wait to run on", err)
	}
	ended("dsp_2", "wake_failed")
	ended("dsp_3", "wake_failed")
	const three = "the runpod machine never took the sample: wake_failed, 3 times in a row"
	if err := r.runner.waitReady(ctx, "ast_p"); err == nil || err.Error() != three {
		t.Fatalf("after three failed wakes: %v, want %q", err, three)
	}
	ended("dsp_4", "wake_timeout")
	if err := r.runner.waitReady(ctx, "ast_p"); err == nil || err.Error() != "the runpod machine never took the sample: wake_timeout" {
		t.Fatalf("a machine that never claimed: %v", err)
	}
}
