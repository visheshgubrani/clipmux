package e2e

// The Phase 5 exit test (docs/plans/P5-production.md, "Exit test") on the
// real binary: subtitles and chapters through a fake speech API and a fake
// LLM (checks 10, 11, 13, 16, 19 and 20), the drain (2), limits (4), the
// soak (5), metrics (6) and a schema newer than the binary (7). Check 1, no
// secret in any log, runs in every real-binary test (checkLogs, D074).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

// silentFixture writes a clip with no audio track.
func silentFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent.mp4")
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25",
		"-t", "4", "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg silent fixture: %v\n%s", err, out)
	}
	return path
}

// get fetches a URL and returns its status and body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// currentOutput is the asset's current output ID.
func (in *instance) currentOutput(asset string) string {
	in.t.Helper()
	return in.scalar(`SELECT coalesce(current_output_id, '') FROM assets WHERE id = $1`, asset)
}

// reencode re-encodes a ready asset and waits for the new output.
func (in *instance) reencode(key, asset string) {
	in.t.Helper()
	before := in.currentOutput(asset)
	if s := in.call("POST", "/v1/assets/"+asset+"/reencode", key, map[string]any{}, nil); s != 202 {
		in.t.Fatalf("reencode: %d", s)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for in.currentOutput(asset) == before {
		if time.Now().After(deadline) {
			in.t.Fatal("the re-encode never committed")
		}
		time.Sleep(500 * time.Millisecond)
	}
	in.waitReady(key, asset, time.Minute)
}

// webhookTypes lists the webhook events written for an asset, in order,
// each with its track's kind when it has one.
func (in *instance) webhookTypes(asset string) []string {
	in.t.Helper()
	rows, err := in.pool.Query(context.Background(), `SELECT payload->>'type' || coalesce(':' || (payload->'track'->>'kind'), '')
		FROM outbox WHERE kind = 'webhook' AND asset_id = $1 ORDER BY created_at, id`, asset)
	if err != nil {
		in.t.Fatal(err)
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

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// Checks 10, 11, 13, 16 (delete) and 20 (api mode): video never waits for
// its captions, failures stay on their track, subtitles survive a
// re-encode, and deleting leaves nothing.
func TestPhase5ExitTracksOnTheRealBinary(t *testing.T) {
	in := start(t)
	owner := in.owner()
	ai := newAIFakes(t)
	in.connectSpeech(owner, ai, "api")
	in.connectLLM(owner, ai)
	key, kid, priv := in.credentials("Tracks")

	// 10. With the speech API answering nothing, the asset is ready, and a
	// second upload becomes ready too.
	ai.holdTranscriptions()
	a, pidA := in.uploadWith(key, fixture(t, 12, 640, 360), map[string]any{"generate": map[string]bool{"chapters": true}})
	in.waitReady(key, a, 3*time.Minute)
	ai.waitEntered(t, time.Minute)
	b, _ := in.upload(key, fixture(t, 4, 640, 360))
	in.waitReady(key, b, 3*time.Minute)
	if got := in.scalar(`SELECT routed_to || ':' || route_reason FROM jobs WHERE asset_id = $1 AND kind = 'transcribe'`, a); got != "speech_api:primary" {
		t.Fatalf("the transcription is routed %s, want speech_api in api mode", got)
	}

	// 13. A re-encode commits while the transcription runs, which still
	// completes from the output it was reading.
	first := in.currentOutput(a)
	in.reencode(key, a)
	ai.release()
	trks := in.waitTracks(key, a, 2*time.Minute)
	if trks["subtitles"].Status != "ready" || trks["chapters"].Status != "ready" || trks["subtitles"].Language != "en" {
		t.Fatalf("tracks = %+v", trks)
	}
	// The one attempt that was held across the re-encode's commit is the
	// one that committed, reading the output it started on.
	if got := in.scalar(`SELECT string_agg(a.status || ':' || a.input_output_id, ',' ORDER BY a.attempt_number)
		FROM attempts a JOIN jobs j ON j.id = a.job_id WHERE j.asset_id = $1 AND j.kind = 'transcribe'`, a); got != "committed:"+first {
		t.Fatalf("transcription attempts = %s, want one, committed, reading %s", got, first)
	}
	events := in.webhookTypes(a)
	ready, subs, chapters := indexOf(events, "video.asset.ready"), indexOf(events, "video.asset.track.ready:subtitles"), indexOf(events, "video.asset.track.ready:chapters")
	if ready < 0 || subs < ready || chapters < subs {
		t.Fatalf("events %v: want the asset ready, then its subtitles, then its chapters", events)
	}
	vttURL := func() string {
		return in.url + "/v/" + sign(t, kid, priv, pidA, time.Hour) + "/" + pidA + "/tracks/" + trks["subtitles"].ID + ".vtt"
	}
	status, before := get(t, vttURL())
	if status != 200 || !strings.Contains(before, "00:00:01.000 --> 00:00:03.000\n"+cueText) {
		t.Fatalf("VTT = %d %q", status, before)
	}
	in.reencode(key, a)
	after := in.waitTracks(key, a, time.Minute)
	if after["subtitles"].ID != trks["subtitles"].ID {
		t.Fatalf("after a re-encode the subtitles are %s, want %s unchanged", after["subtitles"].ID, trks["subtitles"].ID)
	}
	if status, vtt := get(t, vttURL()); status != 200 || vtt != before {
		t.Fatalf("after a re-encode the VTT is %d %q, want the same cues", status, vtt)
	}
	master := func() string {
		_, m := get(t, in.url+"/v/"+sign(t, kid, priv, pidA, time.Hour)+"/"+pidA+"/master.m3u8")
		return m
	}
	if m := master(); !strings.Contains(m, `URI="tracks/`+trks["subtitles"].ID+`.m3u8"`) || !strings.Contains(m, `SUBTITLES="subs"`) {
		t.Fatalf("the master doesn't list the subtitles:\n%s", m)
	}

	// 11. No audio: no_audio, and no job.
	s, _ := in.uploadWith(key, silentFixture(t), map[string]any{"generate": map[string]bool{"chapters": true}})
	in.waitReady(key, s, 3*time.Minute)
	silent := in.waitTracks(key, s, time.Minute)
	if silent["subtitles"].ErrorCode != "no_audio" || silent["chapters"].ErrorCode != "subtitles_failed" {
		t.Fatalf("silent tracks = %+v", silent)
	}
	if n := in.scalar(`SELECT count(*)::text FROM jobs WHERE asset_id = $1 AND kind IN ('transcribe', 'chapters')`, s); n != "0" {
		t.Fatalf("%s track jobs for a silent video", n)
	}
	// 11. A failed transcription errors its track and its chapters, never the asset.
	ai.rejectTranscriptions(true)
	c, _ := in.uploadWith(key, fixture(t, 4, 640, 360), map[string]any{"generate": map[string]bool{"chapters": true}})
	in.waitReady(key, c, 3*time.Minute)
	failed := in.waitTracks(key, c, 2*time.Minute)
	ai.rejectTranscriptions(false)
	if failed["subtitles"].Status != "errored" || failed["subtitles"].ErrorCode != "speech_api_rejected" ||
		failed["chapters"].ErrorCode != "subtitles_failed" {
		t.Fatalf("failed tracks = %+v", failed)
	}
	var view struct{ Status string }
	in.call("GET", "/v1/assets/"+c, key, nil, &view)
	if view.Status != "ready" {
		t.Fatalf("an asset with failed subtitles is %s", view.Status)
	}

	// 16. Delete leaves no live track, no transcript and no object.
	if s := in.call("DELETE", "/v1/assets/"+a, key, nil, nil); s != 200 && s != 202 {
		t.Fatalf("delete: %d", s)
	}
	// The delete job's first pass removes the files and sets deleted_at; a
	// second pass waits out the PUT URLs' lifetime (lifecycle.LateWrites).
	deadline := time.Now().Add(time.Minute)
	for in.scalar(`SELECT (deleted_at IS NOT NULL)::text FROM assets WHERE id = $1`, a) != "true" {
		if time.Now().After(deadline) {
			t.Fatal("the delete job never removed the files")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if got := in.scalar(`SELECT (SELECT count(*) FROM tracks WHERE asset_id = $1 AND status <> 'deleted') || ':' || (SELECT count(*) FROM transcripts WHERE asset_id = $1)`, a); got != "0:0" {
		t.Fatalf("live tracks:transcripts after delete = %s", got)
	}
	if status, _ := get(t, vttURL()); status != 404 {
		t.Fatalf("a deleted asset's VTT = %d", status)
	}
	var left []string
	_ = filepath.WalkDir(in.dataDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, a) {
			left = append(left, p)
		}
		return nil
	})
	if len(left) > 0 {
		t.Fatalf("files left after delete: %v", left)
	}
	in.noAIKeys()
}

// Checks 2 and 20 (the drain): SIGTERM with a speech API request and an
// encode in flight and a long-poll open. readyz says 503 at once, the
// long-poll answers, both attempts end worker_stopped without spending the
// failure budget, serve exits 0, and after a restart both finish.
func TestPhase5ExitSIGTERMDrainsAndHandsWorkBack(t *testing.T) {
	in := start(t)
	owner := in.owner()
	ai := newAIFakes(t)
	in.connectSpeech(owner, ai, "api")
	key, _, _ := in.credentials("Drain")
	ai.holdTranscriptions()
	a, _ := in.uploadWith(key, fixture(t, 4, 640, 360), map[string]any{"generate": map[string]bool{"subtitles": true}})
	in.waitReady(key, a, 3*time.Minute)
	ai.waitEntered(t, time.Minute)
	m, _ := in.upload(key, fixture(t, 120, 1280, 720))
	deadline := time.Now().Add(2 * time.Minute)
	for in.scalar(`SELECT count(*)::text FROM attempts WHERE asset_id = $1 AND status = 'running' AND executor = 'builtin' AND coalesce(progress, 0) > 0`, m) != "1" {
		if time.Now().After(deadline) {
			t.Fatal("the encode never got under way")
		}
		time.Sleep(200 * time.Millisecond)
	}

	poll := make(chan error, 1)
	var pollTook time.Duration
	var signalled time.Time
	var mu sync.Mutex
	go func() {
		req, _ := http.NewRequest("GET", in.url+"/v1/events?wait=25", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				err = fmt.Errorf("long-poll: HTTP %d", res.StatusCode)
			}
		}
		mu.Lock()
		pollTook = time.Since(signalled)
		mu.Unlock()
		poll <- err
	}()
	time.Sleep(time.Second) // the long-poll is waiting

	mu.Lock()
	signalled = time.Now()
	mu.Unlock()
	if err := in.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- in.cmd.Wait() }()
	for {
		res, err := http.Get(in.url + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusServiceUnavailable {
				break
			}
		}
		if time.Since(signalled) > time.Second {
			t.Fatalf("/readyz isn't 503 a second after SIGTERM (%v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-poll; err != nil {
		t.Fatal(err)
	}
	if mu.Lock(); pollTook > 2*time.Second {
		t.Fatalf("the long-poll answered %s after SIGTERM, want 2 s", pollTook)
	}
	mu.Unlock()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("serve exited with %v, want 0", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve didn't exit within 30 s of SIGTERM")
	}
	took := time.Since(signalled)
	in.pw.Close()
	for _, c := range []struct{ asset, executor string }{{a, "speech_api"}, {m, "builtin"}} {
		got := in.scalar(`SELECT a.status || ':' || coalesce(a.error_code, '') || ':' || j.failed_count || ':' || j.lost_count || ':' || j.status
			FROM attempts a JOIN jobs j ON j.id = a.job_id WHERE a.asset_id = $1 AND a.executor = $2 ORDER BY a.started_at DESC LIMIT 1`, c.asset, c.executor)
		if got != "lost:worker_stopped:0:1:queued" {
			t.Fatalf("the %s attempt after the drain = %s, want lost:worker_stopped:0:1:queued", c.executor, got)
		}
	}
	if took > 15*time.Second {
		t.Fatalf("serve took %s to stop, want the attempts back within 15 s", took)
	}

	in.launch()
	ai.release()
	in.waitReady(key, m, 5*time.Minute)
	if trk := in.waitTracks(key, a, 2*time.Minute)["subtitles"]; trk.Status != "ready" {
		t.Fatalf("subtitles after the restart = %+v", trk)
	}
	in.noAIKeys()
}

// Check 4: limits refuse with 429 rate_limited and Retry-After, per key and
// per address, while delivery counts only refusals.
func TestPhase5ExitLimits(t *testing.T) {
	fresh := startUnpinned(t)
	for i := 1; i <= 6; i++ {
		req, _ := http.NewRequest("POST", fresh.url+"/v1/setup/owner", strings.NewReader(`{"setup_token":"WRONGWRONGWRONG","email":"a@b.test","password":"a long test password"}`))
		req.Header.Set("Content-Type", "application/json")
		// A spoofed address from an untrusted peer counts as the peer.
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if i < 6 && res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("bad setup token %d already limited", i)
		}
		if i == 6 && (res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "") {
			t.Fatalf("the 6th bad setup token = %d (Retry-After %q)", res.StatusCode, res.Header.Get("Retry-After"))
		}
	}
	for i := 1; i <= 11; i++ {
		res, err := http.Post(fresh.url+"/worker/v1/pair", "application/json", strings.NewReader(`{"code":"ABCD-EFGH"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if i < 11 && res.StatusCode != http.StatusNotFound {
			t.Fatalf("wrong pairing code %d = %d, want 404", i, res.StatusCode)
		}
		if i == 11 && (res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "") {
			t.Fatalf("the 11th wrong pairing code = %d", res.StatusCode)
		}
	}

	in := start(t)
	keyA, _, _ := in.credentials("Burst A")
	keyB, kidB, privB := in.credentials("Burst B")
	statuses := make([]int, 61)
	retryAfter := make([]string, 61)
	var wg sync.WaitGroup
	for i := range statuses {
		wg.Go(func() {
			raw, _ := json.Marshal(map[string]any{"size": 1000, "playback_policy": "signed"})
			req, _ := http.NewRequest("POST", in.url+"/v1/uploads", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+keyA)
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			res.Body.Close()
			statuses[i], retryAfter[i] = res.StatusCode, res.Header.Get("Retry-After")
		})
	}
	wg.Wait()
	limited := 0
	for i, s := range statuses {
		switch s {
		case http.StatusCreated:
		case http.StatusTooManyRequests:
			limited++
			if retryAfter[i] == "" {
				t.Error("a 429 without Retry-After")
			}
		default:
			t.Fatalf("upload %d = %d", i, s)
		}
	}
	if limited != 1 {
		t.Fatalf("%d of 61 creates in a burst were limited, want the 61st alone", limited)
	}
	if s := in.call("POST", "/v1/uploads", keyB, map[string]any{"size": 1000, "playback_policy": "signed"}, nil); s != http.StatusCreated {
		t.Fatalf("key B = %d while key A is limited", s)
	}

	// 40 viewers behind one address play a signed video with no 429.
	asset, pid := in.upload(keyB, fixture(t, 6, 640, 360))
	in.waitReady(keyB, asset, 3*time.Minute)
	var refused sync.Map
	for range 40 {
		wg.Go(func() {
			base := in.url + "/v/" + sign(t, kidB, privB, pid, time.Hour) + "/" + pid + "/"
			for _, p := range viewerPaths(t, base) {
				res, err := http.Get(base + p)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, res.Body)
				res.Body.Close()
				if res.StatusCode != 200 {
					refused.Store(p, res.StatusCode)
				}
			}
		})
	}
	wg.Wait()
	refused.Range(func(k, v any) bool {
		t.Errorf("a viewer got %v for %v", v, k)
		return true
	})
}

// viewerPaths are what a player fetches: the master, its first variant, the
// key and every segment of it.
func viewerPaths(t *testing.T, base string) []string {
	res, err := http.Get(base + "master.m3u8")
	if err != nil {
		t.Error(err)
		return nil
	}
	m, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var variant string
	for _, l := range strings.Split(string(m), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			variant = l
			break
		}
	}
	out := []string{"master.m3u8", variant}
	res, err = http.Get(base + variant)
	if err != nil {
		t.Error(err)
		return out
	}
	pl, _ := io.ReadAll(res.Body)
	res.Body.Close()
	dir := variant[:strings.LastIndex(variant, "/")+1]
	for _, l := range strings.Split(string(pl), "\n") {
		switch {
		case strings.HasPrefix(l, "#EXT-X-KEY"):
			uri := regexp.MustCompile(`URI="([^"]+)"`).FindStringSubmatch(l)[1]
			out = append(out, filepath.Clean(dir+uri))
		case strings.HasPrefix(l, "#EXT-X-MAP"):
			out = append(out, dir+regexp.MustCompile(`URI="([^"]+)"`).FindStringSubmatch(l)[1])
		case l != "" && !strings.HasPrefix(l, "#"):
			out = append(out, dir+l)
		}
	}
	return out
}

// metric sums a family's samples whose labels contain every given pair.
func metric(body, name string, labels ...string) float64 {
	sum := 0.0
	for _, l := range strings.Split(body, "\n") {
		if !strings.HasPrefix(l, name+"{") && !strings.HasPrefix(l, name+" ") {
			continue
		}
		ok := true
		for _, want := range labels {
			ok = ok && strings.Contains(l, want)
		}
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(l[strings.LastIndex(l, " ")+1:], 64)
		if err == nil {
			sum += v
		}
	}
	return sum
}

// Checks 6 and 7: /metrics on its own listener (and 404 on the public
// one) shows the upload's committed attempt and passes promtool when it is
// installed; a schema newer than the binary refuses to boot.
func TestPhase5ExitMetricsAndANewerSchema(t *testing.T) {
	port := freePort(t)
	in := start(t, fmt.Sprintf("CLIPMUX_METRICS_ADDR=127.0.0.1:%d", port))
	key, _, _ := in.credentials("Metrics")
	asset, _ := in.upload(key, fixture(t, 4, 640, 360))
	in.waitReady(key, asset, 3*time.Minute)
	status, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if status != 200 || metric(body, "clipmux_attempts_finished_total", `executor="builtin"`, `result="committed"`) < 1 {
		t.Fatalf("/metrics = %d, no committed attempt:\n%.2000s", status, body)
	}
	if status, _ := get(t, in.url+"/metrics"); status != 404 {
		t.Fatalf("/metrics on the public listener = %d", status)
	}
	if promtool, err := exec.LookPath("promtool"); err == nil {
		cmd := exec.Command(promtool, "check", "metrics")
		cmd.Stdin = strings.NewReader(body)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("promtool check metrics: %v\n%s", err, out)
		}
	} else {
		t.Log("promtool isn't installed; its check runs in CI")
	}

	in.halt(os.Interrupt)
	if _, err := in.pool.Exec(context.Background(), `INSERT INTO goose_db_version (version_id, is_applied) VALUES ((SELECT max(version_id) + 1 FROM goose_db_version), true)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = in.env
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), "schema_newer") {
		t.Fatalf("serve on a newer schema: %v\n%s", err, out)
	}
}

// Check 5: 50 HLS clients on 3 assets with uploads and listing, for 60 s
// (30 minutes with CLIPMUX_E2E_SOAK=1): no 5xx, a route-cache hit ratio of
// at least 0.95, and goroutines and heap back near their baseline after.
func TestPhase5ExitSoak(t *testing.T) {
	soak := 60 * time.Second
	if os.Getenv("CLIPMUX_E2E_SOAK") == "1" {
		soak = 30 * time.Minute
	}
	port := freePort(t)
	in := start(t, fmt.Sprintf("CLIPMUX_METRICS_ADDR=127.0.0.1:%d", port))
	metrics := func() string { _, b := get(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", port)); return b }
	key, kid, priv := in.credentials("Soak")
	var pids []string
	for range 3 {
		asset, pid := in.upload(key, fixture(t, 12, 640, 360))
		in.waitReady(key, asset, 3*time.Minute)
		pids = append(pids, pid)
	}
	time.Sleep(2 * time.Second)
	base := metrics()
	goroutines0, heap0 := metric(base, "clipmux_go_goroutines"), metric(base, "clipmux_go_heap_bytes")
	hits0 := metric(base, "clipmux_delivery_cache_lookups_total", `table="route"`, `result="hit"`)
	misses0 := metric(base, "clipmux_delivery_cache_lookups_total", `table="route"`, `result="miss"`)

	ctx, stop := context.WithTimeout(context.Background(), soak)
	defer stop()
	var fiveXX, requests sync.Map
	count := func(m *sync.Map, k string) {
		v, _ := m.LoadOrStore(k, new(int64))
		mu := v.(*int64)
		*mu++ // a rough count; the test reads it after every client stopped
	}
	var wg sync.WaitGroup
	var countMu sync.Mutex
	for i := range 50 {
		wg.Go(func() {
			pid := pids[i%len(pids)]
			for ctx.Err() == nil {
				prefix := in.url + "/v/" + sign(t, kid, priv, pid, time.Hour) + "/" + pid + "/"
				for _, p := range viewerPaths(t, prefix) {
					if ctx.Err() != nil {
						return
					}
					res, err := http.Get(prefix + p)
					if err != nil {
						continue
					}
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
					countMu.Lock()
					count(&requests, "playback")
					if res.StatusCode >= 500 {
						count(&fiveXX, p)
					}
					countMu.Unlock()
				}
			}
		})
	}
	wg.Go(func() { // uploads
		for ctx.Err() == nil {
			var res map[string]any
			s := in.call("POST", "/v1/uploads", key, map[string]any{"size": 1000, "playback_policy": "signed"}, &res)
			countMu.Lock()
			if s >= 500 {
				count(&fiveXX, "POST /v1/uploads")
			}
			countMu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	})
	wg.Go(func() { // listing
		for ctx.Err() == nil {
			s := in.call("GET", "/v1/assets?limit=100", key, nil, nil)
			countMu.Lock()
			if s >= 500 {
				count(&fiveXX, "GET /v1/assets")
			}
			countMu.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	})
	wg.Wait()
	fiveXX.Range(func(k, v any) bool {
		t.Errorf("%d 5xx answers for %v", *v.(*int64), k)
		return true
	})
	served := int64(0)
	if v, ok := requests.Load("playback"); ok {
		served = *v.(*int64)
	}
	end := metrics()
	hits := metric(end, "clipmux_delivery_cache_lookups_total", `table="route"`, `result="hit"`) - hits0
	misses := metric(end, "clipmux_delivery_cache_lookups_total", `table="route"`, `result="miss"`) - misses0
	ratio := hits / max(hits+misses, 1)
	t.Logf("soak %s: %d playback requests, route cache %.0f hits and %.0f misses (%.4f)", soak, served, hits, misses, ratio)
	if served < 1000 || ratio < 0.95 {
		t.Fatalf("%d requests with a route-cache hit ratio of %.4f, want at least 0.95", served, ratio)
	}
	// Back near the baseline once the clients are gone: idle connections
	// close after IdleTimeout, so goroutines are given that long.
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(150 * time.Second)
	for {
		now := metrics()
		g, h := metric(now, "clipmux_go_goroutines"), metric(now, "clipmux_go_heap_bytes")
		if g <= goroutines0+10 && h <= 2*heap0+64<<20 {
			t.Logf("goroutines %.0f (baseline %.0f), heap %.0f MiB (baseline %.0f MiB)", g, goroutines0, h/(1<<20), heap0/(1<<20))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the soak goroutines are %.0f (baseline %.0f) and heap %.0f MiB (baseline %.0f MiB)", g, goroutines0, h/(1<<20), heap0/(1<<20))
		}
		time.Sleep(5 * time.Second)
	}
}

// webhookSink receives an app's webhooks and checks each signature.
type webhookSink struct {
	srv    *httptest.Server
	mu     sync.Mutex
	secret string
	events []string
	bad    int
}

func newWebhookSink(t *testing.T) *webhookSink {
	s := &webhookSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get("X-Webhook-Timestamp"), 10, 64)
		var e struct {
			Type  string `json:"type"`
			Track *struct {
				Kind string `json:"kind"`
			} `json:"track"`
		}
		_ = json.Unmarshal(body, &e)
		s.mu.Lock()
		defer s.mu.Unlock()
		if webhooks.Sign([]byte(s.secret), ts, body) != r.Header.Get("X-Webhook-Signature") {
			s.bad++
		}
		name := e.Type
		if e.Track != nil {
			name += ":" + e.Track.Kind
		}
		s.events = append(s.events, name)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *webhookSink) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// lmsStage maps a ClipMux track status to the LMS's subtitle_status and
// chapters_status (LMS/api/src/test/harness.ts): what the LMS adapter
// will do at cutover.
func lmsStage(status string) string {
	switch status {
	case "ready":
		return "completed"
	case "errored":
		return "failed"
	}
	return "pending"
}

// Check 19: the LMS parity contract. The shapes the LMS's own fake of its
// ClipMux adapter returns (LMS/api/src/test/harness.ts) come out of the
// real API: stages pending, completed and failed; the transcript the moment
// its track reads ready, as {language, duration_ms, text, segments:
// [{startMs, endMs, text}]}; the sidecar VTT; chapters in seconds as
// [{startTime, endTime, title}]; a signed webhook for every stage; and
// nothing left after a delete.
func TestPhase5ExitLMSParityContract(t *testing.T) {
	in := start(t)
	owner := in.owner()
	ai := newAIFakes(t)
	in.connectSpeech(owner, ai, "api")
	in.connectLLM(owner, ai)
	key, kid, priv := in.credentials("LMS")
	appID := in.scalar(`SELECT id FROM apps WHERE name = 'LMS'`)
	sink := newWebhookSink(t)
	var ep struct {
		Secret string `json:"secret"`
	}
	if s := ownerCall(t, owner, "POST", in.url+"/v1/apps/"+appID+"/webhook-endpoints", map[string]any{"url": sink.srv.URL}, &ep); s != 201 {
		t.Fatalf("create webhook endpoint: %d", s)
	}
	sink.mu.Lock()
	sink.secret = ep.Secret
	sink.mu.Unlock()

	ai.holdTranscriptions()
	asset, pid := in.uploadWith(key, fixture(t, 12, 640, 360), map[string]any{"generate": map[string]bool{"subtitles": true, "chapters": true}})
	in.waitReady(key, asset, 3*time.Minute)
	type assetView struct {
		DurationSeconds float64 `json:"duration_seconds"`
		Tracks          []trackView
	}
	var v assetView
	in.call("GET", "/v1/assets/"+asset, key, nil, &v)
	stages := map[string]string{}
	for _, tr := range v.Tracks {
		stages[tr.Kind] = lmsStage(tr.Status)
	}
	if stages["subtitles"] != "pending" || stages["chapters"] != "pending" {
		t.Fatalf("stages while transcribing = %v", stages)
	}
	ai.release()

	// The transcript can be fetched the moment its track reads ready.
	var subsID string
	deadline := time.Now().Add(2 * time.Minute)
	for subsID == "" {
		in.call("GET", "/v1/assets/"+asset, key, nil, &v)
		for _, tr := range v.Tracks {
			if tr.Kind == "subtitles" && tr.Status == "ready" {
				subsID = tr.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the subtitles never became ready")
		}
		if subsID == "" {
			time.Sleep(100 * time.Millisecond) // under the key's 20 requests a second
		}
	}
	var tr struct {
		Language   string `json:"language"`
		DurationMS int64  `json:"duration_ms"`
		Text       string `json:"text"`
		Segments   []struct {
			StartMS int64  `json:"start_ms"`
			EndMS   int64  `json:"end_ms"`
			Text    string `json:"text"`
		} `json:"segments"`
	}
	if s := in.call("GET", "/v1/assets/"+asset+"/tracks/"+subsID+"/transcript", key, nil, &tr); s != 200 {
		t.Fatalf("the transcript of a ready track = %d", s)
	}
	type lmsSegment struct {
		StartMs int64  `json:"startMs"`
		EndMs   int64  `json:"endMs"`
		Text    string `json:"text"`
	}
	lms := struct {
		Language   string       `json:"language"`
		DurationMS int64        `json:"duration_ms"`
		Text       string       `json:"text"`
		Segments   []lmsSegment `json:"segments"`
	}{Language: tr.Language, DurationMS: tr.DurationMS, Text: tr.Text}
	for _, s := range tr.Segments {
		lms.Segments = append(lms.Segments, lmsSegment{s.StartMS, s.EndMS, s.Text})
	}
	got, _ := json.Marshal(lms)
	want := fmt.Sprintf(`{"language":"en","duration_ms":%d,"text":"Hello from ClipMux. Chapter two begins.","segments":[{"startMs":1000,"endMs":3000,"text":"Hello from ClipMux."},{"startMs":6000,"endMs":8000,"text":"Chapter two begins."}]}`,
		int64(v.DurationSeconds*1000+0.5))
	if string(got) != want {
		t.Fatalf("LMS transcript =\n%s\nwant\n%s", got, want)
	}

	// The sidecar VTT loads.
	res, err := http.Get(in.url + "/v/" + sign(t, kid, priv, pid, time.Hour) + "/" + pid + "/tracks/" + subsID + ".vtt")
	if err != nil {
		t.Fatal(err)
	}
	vtt, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/vtt; charset=utf-8" || !bytes.Contains(vtt, []byte(cueText)) {
		t.Fatalf("sidecar VTT = %d %s %q", res.StatusCode, res.Header.Get("Content-Type"), vtt)
	}

	// Chapters convert to seconds.
	trks := in.waitTracks(key, asset, time.Minute)
	var detail struct {
		Chapters []struct {
			StartMS int64  `json:"start_ms"`
			EndMS   int64  `json:"end_ms"`
			Title   string `json:"title"`
		} `json:"chapters"`
	}
	in.call("GET", "/v1/assets/"+asset+"/tracks/"+trks["chapters"].ID, key, nil, &detail)
	type lmsChapter struct {
		StartTime float64 `json:"startTime"`
		EndTime   float64 `json:"endTime"`
		Title     string  `json:"title"`
	}
	var chapters []lmsChapter
	for _, c := range detail.Chapters {
		chapters = append(chapters, lmsChapter{float64(c.StartMS) / 1000, float64(c.EndMS) / 1000, c.Title})
	}
	got, _ = json.Marshal(chapters)
	end := strconv.FormatFloat(float64(int64(v.DurationSeconds*1000+0.5))/1000, 'f', -1, 64)
	if want := `[{"startTime":0,"endTime":6,"title":"Opening"},{"startTime":6,"endTime":` + end + `,"title":"Chapter two"}]`; string(got) != want {
		t.Fatalf("LMS chapters = %s, want %s", got, want)
	}
	if stages := lmsStage(trks["subtitles"].Status) + "," + lmsStage(trks["chapters"].Status); stages != "completed,completed" {
		t.Fatalf("stages = %s", stages)
	}

	// A failed transcription reads failed for both.
	ai.rejectTranscriptions(true)
	failedAsset, _ := in.uploadWith(key, fixture(t, 4, 640, 360), map[string]any{"generate": map[string]bool{"chapters": true}})
	in.waitReady(key, failedAsset, 3*time.Minute)
	failed := in.waitTracks(key, failedAsset, 2*time.Minute)
	if stages := lmsStage(failed["subtitles"].Status) + "," + lmsStage(failed["chapters"].Status); stages != "failed,failed" {
		t.Fatalf("failed stages = %s", stages)
	}

	// Delete leaves nothing, and says so.
	if s := in.call("DELETE", "/v1/assets/"+asset, key, nil, nil); s != 200 && s != 202 {
		t.Fatalf("delete: %d", s)
	}
	deadline = time.Now().Add(time.Minute)
	for in.scalar(`SELECT (SELECT count(*) FROM tracks WHERE asset_id = $1 AND status <> 'deleted') + (SELECT count(*) FROM transcripts WHERE asset_id = $1)`, asset) != "0" ||
		indexOf(sink.received(), "video.asset.deleted") < 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after delete: %s rows left, events %v", in.scalar(`SELECT count(*)::text FROM tracks WHERE asset_id = $1 AND status <> 'deleted'`, asset), sink.received())
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Every stage change sent a signed webhook.
	deadline = time.Now().Add(time.Minute)
	for {
		ev := sink.received()
		ready, subs, chap := indexOf(ev, "video.asset.ready"), indexOf(ev, "video.asset.track.ready:subtitles"), indexOf(ev, "video.asset.track.ready:chapters")
		if ready >= 0 && subs > ready && chap > subs && indexOf(ev, "video.asset.track.errored:subtitles") >= 0 && indexOf(ev, "video.asset.track.errored:chapters") >= 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhooks received: %v", ev)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if sink.mu.Lock(); sink.bad > 0 {
		t.Fatalf("%d webhooks failed their signature", sink.bad)
	}
	sink.mu.Unlock()
	in.noAIKeys()
}
