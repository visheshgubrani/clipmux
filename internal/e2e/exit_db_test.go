package e2e

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// play decodes a signed playback URL with ffmpeg as the HLS client and
// returns the video and audio frames it got.
func play(t *testing.T, url string) (video, audio int) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "http,tcp,crypto,data",
		"-i", url, "-map", "0:v:0", "-map", "0:a:0", "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg playback: %v", err)
	}
	return strings.Count(string(out), "\n0,"), strings.Count(string(out), "\n1,")
}

func sign(t *testing.T, kid, privatePEM, pid string, ttl time.Duration) string {
	t.Helper()
	key, err := tokens.ParsePrivateKey(privatePEM)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	tok, err := tokens.Sign(key, kid, tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: now, Exp: now + int64(ttl/time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRealBinaryPlaysASignedEncryptedVideo(t *testing.T) {
	in := start(t)
	key, kid, priv := in.credentials("LMS")
	asset, pid := in.upload(key, fixture(t, 8, 854, 480))
	in.waitReady(key, asset, 3*time.Minute)
	video, audio := play(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8")
	if video < 190 || audio == 0 {
		t.Fatalf("decoded %d video and %d audio frames, want 8 s of both", video, audio)
	}
}

// children returns the PIDs whose parent is ppid and whose command line
// contains want.
func children(ppid int, want string) []int {
	var out []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))
		if len(fields) < 2 || fields[1] != strconv.Itoa(ppid) {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if strings.Contains(string(cmdline), want) {
			out = append(out, pid)
		}
	}
	return out
}

// waitSetupTestsIdle waits until no setup test is queued or running.
func (in *instance) waitSetupTestsIdle(limit time.Duration) {
	in.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		var active int
		if err := in.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM setup_tests WHERE status IN ('queued', 'running')`).Scan(&active); err != nil {
			in.t.Fatal(err)
		}
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			in.t.Fatalf("a setup test was still active after %s", limit)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// attemptRunning reports whether asset has a running attempt.
func (in *instance) attemptRunning(asset string) bool {
	in.t.Helper()
	var running bool
	if err := in.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM attempts WHERE asset_id = $1 AND status = 'running')`, asset).Scan(&running); err != nil {
		in.t.Fatal(err)
	}
	return running
}

// Killing the built-in worker mid-encode loses its lease; the job comes back
// on a new attempt with a new output, and the asset plays. Real lease (90 s)
// and sweep (15 s): about two and a half minutes.
func TestKilledWorkerRecoversOnANewAttemptAndOutput(t *testing.T) {
	in := start(t)
	key, kid, priv := in.credentials("LMS")
	// serve queues a first-boot setup test before it listens. Let it finish,
	// so the ffmpeg we kill below is encoding this test's asset, not the sample.
	in.waitSetupTestsIdle(5 * time.Minute)
	asset, pid := in.upload(key, fixture(t, 30, 1280, 720))

	var worker int
	deadline := time.Now().Add(2 * time.Minute)
	for worker == 0 || len(children(worker, "ffmpeg")) == 0 || !in.attemptRunning(asset) {
		if time.Now().After(deadline) {
			t.Fatal("the built-in worker never started encoding")
		}
		if ws := children(in.cmd.Process.Pid, "worker"); len(ws) == 1 {
			worker = ws[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := syscall.Kill(worker, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	in.waitReady(key, asset, 5*time.Minute)

	rows, err := in.pool.Query(context.Background(), `
		SELECT a.id, a.status, a.output_id, j.lost_count, o.status
		FROM attempts a JOIN jobs j ON j.id = a.job_id JOIN outputs o ON o.id = a.output_id
		WHERE a.asset_id = $1 ORDER BY a.attempt_number`, asset)
	if err != nil {
		t.Fatal(err)
	}
	var statuses, outputs []string
	var lost int32
	var first string
	for rows.Next() {
		var id, status, output, outStatus string
		if err := rows.Scan(&id, &status, &output, &lost, &outStatus); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = id
		}
		statuses = append(statuses, status)
		outputs = append(outputs, output+":"+outStatus)
	}
	rows.Close()
	if strings.Join(statuses, ",") != "lost,committed" || lost != 1 {
		t.Fatalf("attempts %v (lost_count %d), want [lost committed] and 1", statuses, lost)
	}
	var current string
	_ = in.pool.QueryRow(context.Background(), `SELECT current_output_id FROM assets WHERE id = $1`, asset).Scan(&current)
	if !strings.HasPrefix(outputs[1], current+":committed") || strings.HasSuffix(outputs[0], ":committed") {
		t.Fatalf("outputs %v, current %s: want the second attempt's output, the first never committed", outputs, current)
	}
	// The killed attempt couldn't clean up after itself; the next worker did.
	if _, err := os.Stat(filepath.Join(in.dataDir, "worker", first)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the killed attempt's scratch directory survived: %v", err)
	}
	if video, _ := play(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < 700 {
		t.Fatalf("decoded %d frames after recovery, want 30 s", video)
	}
}

// A fresh install with the built-in worker proves its path on first boot.
func TestSetupTestPassesOnAFreshInstall(t *testing.T) {
	in := start(t)
	out := in.admin("setup-test", "--wait", "--timeout", "5m")
	for _, check := range []string{"upload", "encode", "play_key", "play_segment_decrypts", "thumbnail_token_refused_key", "no_token_refused", "stored_segment_encrypted"} {
		if !strings.Contains(out, "ok   "+check) {
			t.Fatalf("setup test output lacks a passing %s:\n%s", check, out)
		}
	}
}
