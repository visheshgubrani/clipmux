package e2e

// The Phase 4 exit test (docs/plans/P4-serverless.md): fallback follows the
// routing table, and a corrupt source never hits fallback. serve runs its
// real router, dispatcher, one-time keys, reaper, reconciler and RunPod
// adapter against a fake RunPod API, which runs each pod as a local
// `clipmux worker --once` with the pod's own command and environment, and
// restarts it when it exits, as RunPod restarts a container. The shipped
// 60 s offline rule and 90 s lease apply, with no knob, so these tests run
// in parallel with each other.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
)

// pods runs the fake RunPod's pods as local worker processes.
type pods struct {
	t    *testing.T
	dir  string
	mu   sync.Mutex
	logs bytes.Buffer
	keys []string // each pod's one-time key, in creation order
	live int
	peak int
	// onCreate, if set, runs inside RunPod's create call, before it
	// answers: a test can crash serve there.
	onCreate func(runpodtest.Pod)
	// gate, if set, holds each new pod's process until it is closed: a
	// cold start the test controls.
	gate chan struct{}
	// speech, if set, lets each pod transcribe on the CPU with this
	// machine's whisper.cpp and the models in CLIPMUX_TEST_MODELS_DIR. A
	// pod woken for an encode proves it only if handed the transcription
	// (D128).
	speech bool
	// brokenFollowOn, with speech, makes that proof fail on encode pods, as
	// on a machine that can't transcribe.
	brokenFollowOn bool
}

func newPods(t *testing.T, rp *runpodtest.Fake) *pods {
	ps := &pods{t: t, dir: t.TempDir()}
	rp.Start = ps.start
	t.Cleanup(func() {
		if t.Failed() {
			ps.mu.Lock()
			t.Logf("pod logs:\n%s", ps.logs.String())
			ps.mu.Unlock()
		}
	})
	return ps
}

func (ps *pods) start(p runpodtest.Pod) (stop func()) {
	ps.mu.Lock()
	ps.keys = append(ps.keys, p.Env["CLIPMUX_WORKER_KEY"])
	ps.live++
	ps.peak = max(ps.peak, ps.live)
	onCreate, gate := ps.onCreate, ps.gate
	ps.mu.Unlock()
	if onCreate != nil {
		onCreate(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return
			}
		}
		for ctx.Err() == nil {
			ps.run(ctx, p)
			select { // RunPod restarts a container that exits
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			ps.mu.Lock()
			ps.live--
			ps.mu.Unlock()
		})
	}
}

// run runs the pod's container once: the image's entrypoint is clipmux,
// its command the pod's start command, on CPU encoders so the test means the
// same on every runner.
func (ps *pods) run(ctx context.Context, p runpodtest.Pod) {
	home, err := os.MkdirTemp(ps.dir, p.ID+"-")
	if err != nil {
		ps.printf("%s: %v\n", p.ID, err)
		return
	}
	defer os.RemoveAll(home)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "CLIPMUX_ENCODER=cpu", "CLIPMUX_SPEECH=off"}
	if v := os.Getenv("CLIPMUX_PACKAGER"); v != "" {
		env = append(env, "CLIPMUX_PACKAGER="+v)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	// After the pod's own: the last value wins.
	_, encodePod := p.Env["CLIPMUX_FOLLOW_ON_SPEECH"]
	switch {
	case !ps.speech:
		// Never handed a transcription: it would fetch the models.
		env = append(env, "CLIPMUX_FOLLOW_ON_SPEECH=off")
	case encodePod:
		env = append(env, "CLIPMUX_FOLLOW_ON_SPEECH=cpu", "CLIPMUX_MODELS_DIR="+os.Getenv("CLIPMUX_TEST_MODELS_DIR"))
	default:
		env = append(env, "CLIPMUX_SPEECH=cpu", "CLIPMUX_MODELS_DIR="+os.Getenv("CLIPMUX_TEST_MODELS_DIR"))
	}
	if v := os.Getenv("CLIPMUX_WHISPER"); ps.speech && v != "" {
		env = append(env, "CLIPMUX_WHISPER="+v)
	}
	if ps.speech && encodePod && ps.brokenFollowOn {
		env = append(env, "CLIPMUX_WHISPER="+filepath.Join(home, "no-whisper"))
	}
	cmd := exec.Command(bin, p.Cmd...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.StdoutPipe()
	if err != nil {
		ps.printf("%s: %v\n", p.ID, err)
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		ps.printf("%s: %v\n", p.ID, err)
		return
	}
	exited := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case <-exited:
		}
	}()
	s := bufio.NewScanner(out)
	for s.Scan() {
		ps.printf("%s %s\n", p.ID, s.Text())
	}
	err = cmd.Wait()
	close(exited)
	ps.printf("%s exited: %v\n", p.ID, err)
}

func (ps *pods) printf(format string, args ...any) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	fmt.Fprintf(&ps.logs, format, args...)
}

func (ps *pods) key(i int) string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.keys[i]
}

func (ps *pods) peakLive() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.peak
}

// cloud is serve with RunPod connected through the owner API, its setup
// test passed on a local pod, and routing set to "my PCs, else RunPod".
type cloud struct {
	*instance
	owner          *http.Client
	rp             *runpodtest.Fake
	pods           *pods
	key, kid, priv string
	tag            string
}

// connectOpts are Connect RunPod's limits. The default is generous.
type connectOpts struct {
	maxConcurrent    int
	monthlyLimit     int64
	rateCentsPerHour int
	// speech lets the pods transcribe (pods.speech), brokenFollowOn not
	// after an encode (pods.brokenFollowOn).
	speech, brokenFollowOn bool
}

func newCloud(t *testing.T, opts connectOpts) *cloud {
	t.Helper()
	if os.Getenv("CLIPMUX_UNSANDBOXED_WORKER") == "true" {
		t.Skip("a serverless worker always confines its tools; this machine's sandbox doesn't hold")
	}
	t.Parallel()
	if opts.maxConcurrent == 0 {
		opts = connectOpts{maxConcurrent: 4, monthlyLimit: 100_000, rateCentsPerHour: 50}
	}
	rp := runpodtest.New(t)
	ps := newPods(t, rp)
	ps.speech, ps.brokenFollowOn = opts.speech, opts.brokenFollowOn
	reg := registrytest.New(t)
	certFile := filepath.Join(t.TempDir(), "registry.pem")
	if err := os.WriteFile(certFile, reg.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// The built-in worker stays on; once the primary is "agents" it idles.
	in := start(t, "CLIPMUX_RUNPOD_API="+rp.URL, "CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL=true",
		"SSL_CERT_FILE="+certFile, "CLIPMUX_WORKER_IMAGE="+reg.Image())
	c := &cloud{instance: in, rp: rp, pods: ps}
	c.key, c.kid, c.priv = in.credentials("LMS")
	in.waitSetupTestsIdle(3 * time.Minute)
	c.owner = in.owner()

	var res struct {
		Steps []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"steps"`
	}
	if s := ownerCall(t, c.owner, "POST", in.url+"/v1/executors/runpod/connect", map[string]any{
		"api_key": runpodtest.Key, "registry_username": registrytest.Username, "registry_password": registrytest.Password,
		"max_concurrent": opts.maxConcurrent, "monthly_limit_cents": opts.monthlyLimit, "rate_cents_per_hour": opts.rateCentsPerHour,
	}, &res); s != 200 {
		t.Fatalf("connect runpod: %d", s)
	}
	for _, step := range res.Steps {
		if !step.OK {
			t.Fatalf("connect runpod: step %s failed: %s", step.Name, step.Detail)
		}
	}
	// Its setup test runs on a pod; RunPod takes no other work until then.
	c.waitFor(3*time.Minute, "RunPod's setup test to pass on a pod", func() bool { return c.runpod().Verified })
	c.tag = c.runpod().Config.Tag
	c.waitNoPods()
	if n := c.rp.Created(); n != 1 {
		t.Fatalf("connecting created %d pods, want the setup test's one", n)
	}
	if s := ownerCall(t, c.owner, "PUT", in.url+"/v1/settings/routing",
		map[string]any{"primary_executor": "agents", "fallback_executor": "runpod"}, nil); s != 200 {
		t.Fatalf("set routing: %d", s)
	}
	return c
}

type runpodStatus struct {
	Kind     string  `json:"kind"`
	Verified bool    `json:"verified"`
	Month    float64 `json:"month_cents"`
	Machines int64   `json:"machines"`
	Config   struct {
		Tag string `json:"tag"`
	} `json:"config"`
}

func (c *cloud) runpod() runpodStatus {
	c.t.Helper()
	var list struct{ Data []runpodStatus }
	if s := ownerCall(c.t, c.owner, "GET", c.url+"/v1/executors", nil, &list); s != 200 {
		c.t.Fatalf("list executors: %d", s)
	}
	for _, e := range list.Data {
		if e.Kind == "runpod" {
			return e
		}
	}
	return runpodStatus{}
}

func (c *cloud) waitFor(limit time.Duration, what string, ok func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			c.t.Fatalf("waited %s for %s", limit, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitNoPods waits until every pod was stopped: the reaper ends a dispatch
// within 30 s of its attempt ending, and the stop goes out from the outbox.
func (c *cloud) waitNoPods() {
	c.t.Helper()
	c.waitFor(2*time.Minute, "every pod to be stopped", func() bool { return len(c.rp.Pods()) == 0 })
}

// waitOffline waits until the owner API shows the agent offline, which the
// router also goes by: 60 s without a call.
func (c *cloud) waitOffline(name string) {
	c.t.Helper()
	c.waitFor(2*time.Minute, name+" to show offline", func() bool {
		var list struct {
			Data []struct {
				Name   string `json:"name"`
				Online bool   `json:"online"`
			}
		}
		ownerCall(c.t, c.owner, "GET", c.url+"/v1/agents", nil, &list)
		for _, a := range list.Data {
			if a.Name == name {
				return !a.Online
			}
		}
		return false
	})
}

func (c *cloud) setQueueAge(minutes any) {
	c.t.Helper()
	if s := ownerCall(c.t, c.owner, "PUT", c.url+"/v1/settings/routing", map[string]any{
		"primary_executor": "agents", "fallback_executor": "runpod", "queue_age_fallback_minutes": minutes,
	}, nil); s != 200 {
		c.t.Fatalf("set queue age: %d", s)
	}
}

// route is a job's current route: where it goes while queued, and why.
func (c *cloud) route(asset, kind string) (to, reason string) {
	c.t.Helper()
	_ = c.pool.QueryRow(context.Background(), `
		SELECT coalesce(routed_to, ''), coalesce(route_reason, '') FROM jobs
		WHERE asset_id = $1 AND kind = $2 ORDER BY created_at DESC LIMIT 1`, asset, kind).Scan(&to, &reason)
	return to, reason
}

// ran is one attempt: who ran it, why it went there, and how it ended.
type ran struct {
	executor, reason, status, agentID, dispatchID, output string
}

func (c *cloud) ran(asset, kind string) []ran {
	c.t.Helper()
	rows, err := c.pool.Query(context.Background(), `
		SELECT a.executor, coalesce(a.route_reason, ''), a.status, coalesce(a.agent_id, ''),
		       coalesce(a.dispatch_id, ''), coalesce(a.output_id, '')
		FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE a.asset_id = $1 AND j.kind = $2 ORDER BY a.attempt_number`, asset, kind)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []ran
	for rows.Next() {
		var r ran
		if err := rows.Scan(&r.executor, &r.reason, &r.status, &r.agentID, &r.dispatchID, &r.output); err != nil {
			c.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (c *cloud) dispatches(asset string) int {
	c.t.Helper()
	n, _ := strconv.Atoi(c.scalar(`SELECT count(*)::text FROM dispatches d JOIN jobs j ON j.id = d.job_id WHERE j.asset_id = $1`, asset))
	return n
}

// waitErrored waits for the asset to error and returns its error code.
func (c *cloud) waitErrored(asset string, limit time.Duration) string {
	c.t.Helper()
	var code string
	c.waitFor(limit, "the asset to error", func() bool {
		var a struct {
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		}
		c.call("GET", "/v1/assets/"+asset, c.key, nil, &a)
		if a.Status == "ready" {
			c.t.Fatal("the asset became ready")
		}
		code = a.ErrorCode
		return a.Status == "errored"
	})
	return code
}

func (c *cloud) reencode(asset string) {
	c.t.Helper()
	if s := c.call("POST", "/v1/assets/"+asset+"/reencode", c.key, map[string]any{}, nil); s != 202 {
		c.t.Fatalf("re-encode %s: %d", asset, s)
	}
}

func (c *cloud) plays(pid string, minFrames int) {
	c.t.Helper()
	if video, _ := play(c.t, c.url+"/v/"+sign(c.t, c.kid, c.priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < minFrames {
		c.t.Fatalf("decoded %d frames, want at least %d", video, minFrames)
	}
}

// Checks 1 and 2. With the PC online a job runs there and RunPod sees no
// pod; with the PC asleep the next job wakes a pod, whose one-time key
// claims once, and the pod is stopped once its attempt ends.
func TestExitFallbackFollowsTheRoutingTable(t *testing.T) {
	c := newCloud(t, connectOpts{})
	laptop := c.startAgent("laptop")

	first, firstPID := c.upload(c.key, fixture(t, 8, 640, 360))
	c.waitReady(c.key, first, 3*time.Minute)
	if r := c.ran(first, "transcode"); len(r) != 1 || r[0].executor != "agent" || r[0].reason != "primary" || r[0].agentID != c.agentID("laptop") {
		t.Fatalf("first job's attempts %+v: want one on the laptop, as its primary", r)
	}
	if n := c.rp.Created(); n != 1 {
		t.Fatalf("RunPod created %d pods with the primary online, want only the setup test's", n)
	}
	c.plays(firstPID, 190)

	laptop.sleep()
	c.waitOffline("laptop")
	second, secondPID := c.upload(c.key, fixture(t, 8, 640, 360))
	c.waitReady(c.key, second, 5*time.Minute)
	r := c.ran(second, "transcode")
	if len(r) != 1 || r[0].executor != "runpod" || r[0].reason != "primary_offline" || r[0].dispatchID == "" || r[0].status != "committed" {
		t.Fatalf("second job's attempts %+v: want one committed on a RunPod pod, for primary_offline", r)
	}
	if n := c.rp.Created(); n != 2 {
		t.Fatalf("RunPod created %d pods, want 2 (setup test, then this job)", n)
	}
	c.plays(secondPID, 190)
	c.waitNoPods()
	if reason := c.scalar(`SELECT end_reason FROM dispatches WHERE id = $1`, r[0].dispatchID); reason != "attempt_ended" {
		t.Fatalf("the dispatch ended for %q, want attempt_ended", reason)
	}
	if s := c.call("POST", "/worker/v1/claim", c.pods.key(1), map[string]any{}, nil); s != http.StatusUnauthorized {
		t.Fatalf("a second claim with the spent key got %d, want 401", s)
	}
}

// Check 3. The PC sleeps mid-encode; once its lease is lost it is offline
// too, so the retry wakes RunPod on a new attempt and output. The PC, woken,
// is told to stop.
func TestExitALostLeaseWhileThePrimaryIsOfflineMovesToTheFallback(t *testing.T) {
	c := newCloud(t, connectOpts{})
	laptop := c.startAgent("laptop")
	asset, pid := c.upload(c.key, fixture(t, 30, 1280, 720))

	laptop.encoding(c.instance, asset)
	laptop.sleep()
	c.waitLost(asset)
	c.waitReady(c.key, asset, 5*time.Minute)
	laptop.wake()
	laptop.waitFor(func() bool { return strings.Contains(laptop.log(), "attempt stopped by the server") }, time.Minute,
		"the woken laptop to be told to stop")

	r := c.ran(asset, "transcode")
	if len(r) != 2 || r[0].agentID != c.agentID("laptop") || r[0].status == "committed" ||
		r[1].executor != "runpod" || r[1].reason != "primary_offline" || r[1].status != "committed" || r[1].output == r[0].output {
		t.Fatalf("attempts %+v: want the laptop's superseded and a RunPod pod's committed on a new output", r)
	}
	if current := c.scalar(`SELECT current_output_id FROM assets WHERE id = $1`, asset); current != r[1].output {
		t.Fatalf("current output %s, want the pod's %s", current, r[1].output)
	}
	c.plays(pid, 700)
	c.waitNoPods()
}

// descendants lists every process under pid.
func descendants(pid int) []int {
	var out []int
	for _, child := range children(pid, "") {
		out = append(out, child)
		out = append(out, descendants(child)...)
	}
	return out
}

// Check 4. The PC is online but busy (its encoder frozen, its heartbeats
// going). With queue-age fallback off a second job waits for it; set to a
// minute through the owner API, the job, already waiting longer, wakes
// RunPod with queue_age.
func TestExitQueueAgeWakesTheFallbackForABusyPrimary(t *testing.T) {
	c := newCloud(t, connectOpts{})
	pc := c.startAgent("studio")
	busy, _ := c.upload(c.key, fixture(t, 30, 1280, 720))
	pc.encoding(c.instance, busy)
	// Freeze whatever tool the encode is running; the agent itself keeps
	// heartbeating. A tool may exit between looking and stopping it, so
	// try until one is held.
	var frozen []int
	pc.waitFor(func() bool {
		frozen = append(frozen, descendants(pc.cmd.Process.Pid)...)
		for _, p := range frozen {
			_ = syscall.Kill(p, syscall.SIGSTOP)
		}
		return slices.ContainsFunc(frozen, stopped)
	}, time.Minute, "a media tool to freeze")
	thaw := func() {
		for _, p := range frozen {
			_ = syscall.Kill(p, syscall.SIGCONT)
		}
	}
	t.Cleanup(thaw)

	waiting, pid := c.upload(c.key, fixture(t, 8, 640, 360))
	time.Sleep(75 * time.Second)
	if !c.attemptRunning(busy) {
		t.Fatal("the PC isn't busy any more")
	}
	if to, reason := c.route(waiting, "transcode"); to != "agents" || reason != "primary" || len(c.ran(waiting, "transcode")) != 0 || c.dispatches(waiting) != 0 {
		t.Fatalf("with queue age off the job routed %s/%s with %d attempts and %d dispatches, want agents/primary and none",
			to, reason, len(c.ran(waiting, "transcode")), c.dispatches(waiting))
	}
	c.setQueueAge(1)
	c.waitReady(c.key, waiting, 3*time.Minute)
	if r := c.ran(waiting, "transcode"); len(r) != 1 || r[0].executor != "runpod" || r[0].reason != "queue_age" {
		t.Fatalf("attempts %+v: want one on RunPod for queue_age", r)
	}
	c.plays(pid, 190)

	thaw()
	c.waitReady(c.key, busy, 5*time.Minute)
	if r := c.ran(busy, "transcode"); len(r) != 1 || r[0].agentID != c.agentID("studio") {
		t.Fatalf("the busy job's attempts %+v: want the studio PC's one", r)
	}
	c.waitNoPods()
}

// stopped reports whether pid is alive and stopped by a signal.
func stopped(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+2:]))
	return len(fields) > 0 && fields[0] == "T"
}

// Check 5, concurrency. With a limit of one machine and no PC, the second
// job waits with fallback_limit while the first's pod runs, then runs on its
// own pod; two never run at once.
func TestExitTheConcurrencyLimitHolds(t *testing.T) {
	c := newCloud(t, connectOpts{maxConcurrent: 1, monthlyLimit: 100_000, rateCentsPerHour: 50})
	a, _ := c.upload(c.key, fixture(t, 8, 640, 360))
	b, _ := c.upload(c.key, fixture(t, 8, 640, 360))
	sawLimit := false
	c.waitFor(5*time.Minute, "both jobs to finish", func() bool {
		_, ra := c.route(a, "transcode")
		_, rb := c.route(b, "transcode")
		sawLimit = sawLimit || ra == "fallback_limit" || rb == "fallback_limit"
		return c.scalar(`SELECT count(*)::text FROM assets WHERE id IN ($1, $2) AND status = 'ready'`, a, b) == "2"
	})
	if !sawLimit {
		t.Fatal("neither job ever waited with fallback_limit")
	}
	for _, asset := range []string{a, b} {
		if r := c.ran(asset, "transcode"); len(r) != 1 || r[0].executor != "runpod" || r[0].reason != "primary_offline" {
			t.Fatalf("%s attempts %+v: want one on RunPod for primary_offline", asset, r)
		}
	}
	if peak, n := c.pods.peakLive(), c.rp.Created(); peak != 1 || n != 3 {
		t.Fatalf("%d pods at once and %d created, want 1 at a time and 3 (setup test and two jobs)", peak, n)
	}
	c.waitNoPods()
}

// Check 5, spend. At $360 an hour a wake reserves 15 minutes, $90, and the
// limit is $90.01: the setup test fits, and its few seconds of real cost
// leave no room for another wake. The next job waits with fallback_limit
// until a PC comes online.
func TestExitTheSpendLimitSendsJobsBackToThePrimary(t *testing.T) {
	c := newCloud(t, connectOpts{maxConcurrent: 4, monthlyLimit: 9001, rateCentsPerHour: 36000})
	if month := c.runpod().Month; month <= 0 || month >= 9000 {
		t.Fatalf("month to date %.2f cents after the setup test, want its few seconds' cost", month)
	}
	asset, pid := c.upload(c.key, fixture(t, 8, 640, 360))
	c.waitFor(time.Minute, "the job to wait for the spend limit", func() bool {
		_, reason := c.route(asset, "transcode")
		return reason == "fallback_limit"
	})
	time.Sleep(15 * time.Second) // three more router passes
	if _, reason := c.route(asset, "transcode"); reason != "fallback_limit" || c.dispatches(asset) != 0 || c.rp.Created() != 1 {
		t.Fatalf("route reason %s, %d dispatches and %d pods: want fallback_limit, none and only the setup test's",
			reason, c.dispatches(asset), c.rp.Created())
	}
	c.startAgent("studio")
	c.waitReady(c.key, asset, 3*time.Minute)
	if r := c.ran(asset, "transcode"); len(r) != 1 || r[0].executor != "agent" || r[0].reason != "primary" {
		t.Fatalf("attempts %+v: want one on the PC, as its primary", r)
	}
	c.plays(pid, 190)
}

// Check 6. A source the probe rejects errors at once; a source that probes
// but kills the encoder retries on the PC only, never on RunPod: not when
// the PC goes offline between retries, and not on a later re-encode.
func TestExitACorruptSourceNeverReachesTheFallback(t *testing.T) {
	c := newCloud(t, connectOpts{})
	laptop := c.startAgent("laptop")

	junk := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(junk, []byte(rand.Text()+rand.Text()+rand.Text()), 0o600); err != nil {
		t.Fatal(err)
	}
	rejected, _ := c.upload(c.key, junk)
	if code := c.waitErrored(rejected, 3*time.Minute); code != "unsupported_source" {
		t.Fatalf("the unprobeable source errored with %q, want unsupported_source", code)
	}

	corrupt, err := filepath.Abs("../media/testdata/corrupt.mp4")
	if err != nil {
		t.Fatal(err)
	}
	asset, _ := c.upload(c.key, corrupt)
	c.waitFor(3*time.Minute, "the first encode to fail", func() bool {
		return slices.ContainsFunc(c.ran(asset, "transcode"), func(r ran) bool { return r.status == "failed" })
	})
	// Asleep before its retry is due (30 s), and before its next long-poll
	// (25 s) could be handed the job.
	laptop.sleep()
	c.waitOffline("laptop")
	c.waitFor(time.Minute, "the retry to be refused the fallback", func() bool {
		to, reason := c.route(asset, "transcode")
		return to == "agents" && reason == "fallback_refused"
	})
	laptop.wake()
	// Its three failures spend the retry budget (each attempt
	// transcode_failed), which errors the asset.
	if code := c.waitErrored(asset, 6*time.Minute); code != "processing_failed" {
		t.Fatalf("the corrupt source errored with %q, want processing_failed", code)
	}
	if r := c.ran(asset, "transcode"); len(r) != 3 || slices.ContainsFunc(r, func(r ran) bool { return r.executor != "agent" || r.status != "failed" }) {
		t.Fatalf("attempts %+v: want three failed, all on the PC", r)
	}

	laptop.sleep()
	c.waitOffline("laptop")
	c.reencode(asset)
	c.waitFor(time.Minute, "the re-encode to be refused the fallback", func() bool {
		to, reason := c.route(asset, "reencode")
		return to == "agents" && reason == "fallback_refused"
	})
	time.Sleep(15 * time.Second) // three more router passes
	if to, reason := c.route(asset, "reencode"); to != "agents" || reason != "fallback_refused" {
		t.Fatalf("the re-encode routed %s/%s, want agents/fallback_refused", to, reason)
	}
	if n, d := c.rp.Created(), c.dispatches(asset)+c.dispatches(rejected); n != 1 || d != 0 {
		t.Fatalf("RunPod created %d pods and the sources have %d dispatches, want only the setup test's pod and none", n, d)
	}
}

// Check 7. A local push's re-encode is pinned to its PC: with the PC
// offline it waits for it, and never wakes RunPod.
func TestExitAPinnedJobWaitsForItsPC(t *testing.T) {
	c := newCloud(t, connectOpts{})
	key, _, _ := c.credentials("Dashboard")
	pc := c.startAgent("studio")
	pc.waitFor(func() bool {
		_, err := os.Stat(filepath.Join(pc.dir, "agent.sock"))
		return err == nil
	}, 30*time.Second, "the agent's socket")
	push := exec.Command(bin, "push", "--local", "--quiet", fixture(t, 8, 640, 360))
	push.Env = pc.env
	out, err := push.CombinedOutput()
	if err != nil {
		t.Fatalf("clipmux push --local: %v\n%s", err, out)
	}
	asset := strings.Fields(string(out))[0]
	c.waitReady(key, asset, 3*time.Minute)

	pc.sleep()
	c.waitOffline("studio")
	if s := c.call("POST", "/v1/assets/"+asset+"/reencode", key, map[string]any{}, nil); s != 202 {
		t.Fatalf("re-encode: %d", s)
	}
	time.Sleep(20 * time.Second) // four router passes
	if to, reason := c.route(asset, "reencode"); to != "agents" || reason != "pinned" || c.dispatches(asset) != 0 || c.rp.Created() != 1 {
		t.Fatalf("the pinned re-encode routed %s/%s with %d dispatches and %d pods, want agents/pinned, none and the setup test's",
			to, reason, c.dispatches(asset), c.rp.Created())
	}
	pc.wake()
	c.waitFor(3*time.Minute, "the re-encode to run on its PC", func() bool {
		r := c.ran(asset, "reencode")
		return len(r) == 1 && r[0].status == "committed" && r[0].agentID == c.agentID("studio")
	})
}

// Check 8. A crash while RunPod creates the pod leaves exactly one pod,
// which the restarted serve finds by name; a restart between a wake and its
// claim keeps the same pod, whose worker retries until serve is back; and a
// pod no live dispatch accounts for is stopped by the reconciler's pass at
// boot, while a pod that isn't ClipMux's is left alone.
func TestExitNoOrphansAndNoDoubles(t *testing.T) {
	c := newCloud(t, connectOpts{})

	// A crash around Wake: serve dies after RunPod created the pod, before
	// it answered.
	created := make(chan runpodtest.Pod, 1)
	resume := make(chan struct{})
	c.pods.mu.Lock()
	c.pods.onCreate = func(p runpodtest.Pod) {
		created <- p
		<-resume
	}
	c.pods.mu.Unlock()
	crashed, crashedPID := c.upload(c.key, fixture(t, 8, 640, 360))
	var pod runpodtest.Pod
	select {
	case pod = <-created:
	case <-time.After(2 * time.Minute):
		t.Fatal("no pod was created")
	}
	c.pods.mu.Lock()
	c.pods.onCreate = nil
	c.pods.mu.Unlock()
	c.halt(os.Kill)
	close(resume)
	c.launch()
	c.waitReady(c.key, crashed, 3*time.Minute)
	if n := c.rp.Created(); n != 2 {
		t.Fatalf("RunPod got %d creates, want 2 (setup test, then one for the crashed wake)", n)
	}
	r := c.ran(crashed, "transcode")
	if len(r) != 1 || r[0].executor != "runpod" {
		t.Fatalf("attempts %+v: want one on RunPod", r)
	}
	if instance := c.scalar(`SELECT instance_id FROM dispatches WHERE id = $1`, r[0].dispatchID); instance != pod.ID {
		t.Fatalf("the dispatch recorded %s, want the pod created before the crash, %s", instance, pod.ID)
	}
	c.plays(crashedPID, 190)
	c.waitNoPods()

	// A restart between the wake and the claim: the pod starts while serve
	// is down, and claims once it is back.
	gate := make(chan struct{})
	c.pods.mu.Lock()
	c.pods.gate = gate
	c.pods.mu.Unlock()
	restarted, restartedPID := c.upload(c.key, fixture(t, 8, 640, 360))
	c.waitFor(2*time.Minute, "the wake to record its pod", func() bool {
		return c.scalar(`SELECT count(*)::text FROM dispatches d JOIN jobs j ON j.id = d.job_id WHERE j.asset_id = $1 AND d.instance_id IS NOT NULL`, restarted) == "1"
	})
	c.halt(os.Interrupt)
	close(gate)
	time.Sleep(5 * time.Second) // the worker's first claims fail and back off
	c.launch()
	c.waitReady(c.key, restarted, 3*time.Minute)
	if n := c.rp.Created(); n != 3 {
		t.Fatalf("RunPod got %d creates, want 3: the restart woke nothing new", n)
	}
	if d := c.dispatches(restarted); d != 1 {
		t.Fatalf("%d dispatches for the job, want 1", d)
	}
	c.plays(restartedPID, 190)
	c.waitNoPods()

	// Orphans: a pod named for an ended dispatch, as a crash before its
	// stop would leave, and the owner's own pod.
	ended := c.scalar(`SELECT d.id FROM dispatches d JOIN jobs j ON j.id = d.job_id WHERE j.asset_id = $1`, restarted)
	orphan := c.rp.AddPod("clipmux-" + c.tag + "-" + ended)
	own := c.rp.AddPod("owner-notebook")
	c.restart()
	c.waitFor(time.Minute, "the reconciler to stop the orphan", func() bool {
		return !slices.ContainsFunc(c.rp.Pods(), func(p runpodtest.Pod) bool { return p.ID == orphan })
	})
	if !slices.ContainsFunc(c.rp.Pods(), func(p runpodtest.Pod) bool { return p.ID == own }) {
		t.Fatal("the reconciler stopped a pod that isn't ClipMux's")
	}
	if !strings.Contains(c.log(), "stopped a machine no dispatch accounts for") {
		t.Fatal("serve didn't log the orphan it stopped")
	}
}
