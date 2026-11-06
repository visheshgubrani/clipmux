package e2e

// The Phase 3 exit test (docs/plans/P3-pc-agent.md): sleeping a PC
// mid-job recovers it, and a local push uploads only outputs. Agents are the
// real binary, paired with a code from `clipmux admin create-pairing`, each
// in its own process group so SIGSTOP freezes its ffmpeg too. Real lease
// (90 s) and sweep (15 s), with no knob.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// agent is one running `clipmux worker` paired with an instance.
type agent struct {
	t    *testing.T
	cmd  *exec.Cmd
	dir  string // CLIPMUX_AGENT_DIR
	env  []string
	mu   sync.Mutex
	logs bytes.Buffer
}

// startAgent pairs a new PC agent and starts it in its own process group.
// It encodes on the CPU, so the test means the same on every runner.
func (in *instance) startAgent(name string) *agent {
	in.t.Helper()
	code := regexp.MustCompile(`code: (\S+)`).FindStringSubmatch(in.admin("create-pairing", "--app", name))[1]
	a := &agent{t: in.t, dir: in.t.TempDir()}
	a.env = []string{"PATH=" + os.Getenv("PATH"), "CLIPMUX_AGENT_DIR=" + a.dir, "CLIPMUX_ENCODER=cpu", "HOME=" + a.dir}
	if v := os.Getenv("CLIPMUX_PACKAGER"); v != "" {
		a.env = append(a.env, "CLIPMUX_PACKAGER="+v)
	}
	args := []string{"worker", "--server", in.url, "--pair", code, "--name", name, "--work-dir", filepath.Join(a.dir, "work")}
	if os.Getenv("CLIPMUX_UNSANDBOXED_WORKER") == "true" {
		args = append(args, "--unsandboxed")
	}
	a.cmd = exec.Command(bin, args...)
	a.cmd.Env = a.env
	a.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	a.cmd.Stdout, a.cmd.Stderr = pw, pw
	go func() {
		s := bufio.NewScanner(pr)
		for s.Scan() {
			a.mu.Lock()
			a.logs.WriteString(s.Text() + "\n")
			a.mu.Unlock()
		}
	}()
	if err := a.cmd.Start(); err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() {
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGCONT)
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		_ = a.cmd.Wait()
		pw.Close()
		if in.t.Failed() {
			in.t.Logf("agent %s logs:\n%s", name, a.log())
		}
	})
	a.waitFor(func() bool { return strings.Contains(a.log(), "clipmux worker started") }, time.Minute, "the agent to start")
	return a
}

func (a *agent) log() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logs.String()
}

func (a *agent) waitFor(ok func() bool, limit time.Duration, what string) {
	a.t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			a.t.Fatalf("waited %s for %s", limit, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// encoding waits until the agent's ffmpeg is encoding asset.
func (a *agent) encoding(in *instance, asset string) {
	a.t.Helper()
	a.waitFor(func() bool {
		return len(children(a.cmd.Process.Pid, "ffmpeg")) > 0 && in.attemptRunning(asset)
	}, 2*time.Minute, "the agent to start encoding")
}

// sleep freezes the agent and its tools, as a closed laptop lid would.
func (a *agent) sleep() { a.signal(syscall.SIGSTOP) }
func (a *agent) wake()  { a.signal(syscall.SIGCONT) }

func (a *agent) signal(sig syscall.Signal) {
	a.t.Helper()
	if err := syscall.Kill(-a.cmd.Process.Pid, sig); err != nil {
		a.t.Fatal(err)
	}
}

func (in *instance) agentID(name string) string {
	in.t.Helper()
	var id string
	if err := in.pool.QueryRow(context.Background(), `SELECT id FROM agents WHERE name = $1`, name).Scan(&id); err != nil {
		in.t.Fatal(err)
	}
	return id
}

// waitLost waits for the sweeper to mark the asset's attempt lost: a lease
// of 90 s, then a sweep within 15 s.
func (in *instance) waitLost(asset string) {
	in.t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var lost bool
		if err := in.pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM attempts WHERE asset_id = $1 AND status = 'lost')`, asset).Scan(&lost); err != nil {
			in.t.Fatal(err)
		}
		if lost {
			return
		}
		if time.Now().After(deadline) {
			in.t.Fatal("the sleeping agent's lease was never swept")
		}
		time.Sleep(time.Second)
	}
}

type attemptRow struct {
	status, agentID, output string
}

func (in *instance) attempts(asset string) (rows []attemptRow, lost int32, current string) {
	in.t.Helper()
	ctx := context.Background()
	r, err := in.pool.Query(ctx, `
		SELECT a.status, coalesce(a.agent_id, ''), a.output_id, j.lost_count
		FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE a.asset_id = $1 AND j.kind = 'transcode' ORDER BY a.attempt_number`, asset)
	if err != nil {
		in.t.Fatal(err)
	}
	for r.Next() {
		var a attemptRow
		if err := r.Scan(&a.status, &a.agentID, &a.output, &lost); err != nil {
			in.t.Fatal(err)
		}
		rows = append(rows, a)
	}
	r.Close()
	_ = in.pool.QueryRow(ctx, `SELECT coalesce(current_output_id, '') FROM assets WHERE id = $1`, asset).Scan(&current)
	return rows, lost, current
}

// A PC that sleeps through its lease, with no other worker around, comes
// back to the same attempt: the job waited for it, spending one lost lease.
func TestExitASleepingPCRecoversItsJob(t *testing.T) {
	in := start(t, "CLIPMUX_BUILTIN_WORKER=false")
	key, kid, priv := in.credentials("LMS")
	laptop := in.startAgent("laptop")
	asset, pid := in.upload(key, fixture(t, 30, 1280, 720))

	laptop.encoding(in, asset)
	laptop.sleep()
	in.waitLost(asset)
	laptop.wake()
	in.waitReady(key, asset, 5*time.Minute)

	rows, lost, current := in.attempts(asset)
	if len(rows) != 1 || rows[0].status != "committed" || rows[0].agentID != in.agentID("laptop") || lost != 1 || current != rows[0].output {
		t.Fatalf("attempts %+v, lost %d, current %s: want the laptop's one attempt committed after one lost lease", rows, lost, current)
	}
	if video, _ := play(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < 700 {
		t.Fatalf("decoded %d frames, want 30 s", video)
	}
}

// While a PC sleeps, another worker takes its job on a new attempt and
// output; when the PC wakes it is told to stop, and nothing it wrote is
// used.
func TestExitASleepingPCsJobMovesToAnotherWorker(t *testing.T) {
	in := start(t, "CLIPMUX_BUILTIN_WORKER=false")
	key, kid, priv := in.credentials("LMS")
	laptop := in.startAgent("laptop")
	asset, pid := in.upload(key, fixture(t, 30, 1280, 720))

	laptop.encoding(in, asset)
	laptop.sleep()
	in.waitLost(asset)
	in.startAgent("desktop")
	in.waitReady(key, asset, 5*time.Minute)
	laptop.wake()
	laptop.waitFor(func() bool { return strings.Contains(laptop.log(), "attempt stopped by the server") }, time.Minute,
		"the woken laptop to be told to stop")

	rows, _, current := in.attempts(asset)
	if len(rows) != 2 || rows[0].agentID != in.agentID("laptop") || rows[0].status == "committed" ||
		rows[1].agentID != in.agentID("desktop") || rows[1].status != "committed" || current != rows[1].output {
		t.Fatalf("attempts %+v, current %s: want the laptop's attempt superseded and the desktop's committed", rows, current)
	}
	if video, _ := play(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < 700 {
		t.Fatalf("decoded %d frames, want 30 s", video)
	}
}

// A local push leaves the file on the PC: storage never holds a source,
// only the encrypted outputs, and a re-encode runs on the same PC.
func TestExitALocalPushUploadsOnlyOutputs(t *testing.T) {
	in := start(t, "CLIPMUX_BUILTIN_WORKER=false")
	key, kid, priv := in.credentials("Dashboard") // the owner's own uploads' app
	pc := in.startAgent("studio")
	pc.waitFor(func() bool {
		_, err := os.Stat(filepath.Join(pc.dir, "agent.sock"))
		return err == nil
	}, 30*time.Second, "the agent's socket")

	source := fixture(t, 14, 1280, 720)
	push := exec.Command(bin, "push", "--local", "--quiet", source)
	push.Env = pc.env // no API key: the owner's push
	out, err := push.CombinedOutput()
	if err != nil {
		t.Fatalf("clipmux push --local: %v\n%s", err, out)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "ast_") {
		t.Fatalf("push printed %q", out)
	}
	asset, pid := fields[0], fields[1]
	in.waitReady(key, asset, 3*time.Minute)

	if _, err := os.Stat(filepath.Join(in.dataDir, "storage", "objects", "uploads", asset)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("storage holds the local source: %v", err)
	}
	var uploads int
	_ = in.pool.QueryRow(context.Background(), `SELECT count(*) FROM uploads WHERE asset_id = $1`, asset).Scan(&uploads)
	if uploads != 0 {
		t.Fatal("a local push created an upload")
	}
	if video, _ := play(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < 330 {
		t.Fatalf("decoded %d frames, want 14 s", video)
	}

	if s := in.call("POST", "/v1/assets/"+asset+"/reencode", key, map[string]any{}, nil); s != 202 {
		t.Fatalf("re-encode: %d", s)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var status, pinned, ranOn string
		_ = in.pool.QueryRow(context.Background(), `
			SELECT j.status, coalesce(j.pinned_agent_id, ''), coalesce(max(a.agent_id), '')
			FROM jobs j LEFT JOIN attempts a ON a.job_id = j.id
			WHERE j.asset_id = $1 AND j.kind = 'reencode' GROUP BY j.id`, asset).Scan(&status, &pinned, &ranOn)
		if status == "succeeded" {
			if id := in.agentID("studio"); pinned != id || ranOn != id {
				t.Fatalf("re-encode pinned to %q and ran on %q, want the studio PC", pinned, ranOn)
			}
			break
		}
		if status == "failed" || time.Now().After(deadline) {
			t.Fatalf("re-encode %s", status)
		}
		time.Sleep(time.Second)
	}
	if _, err := os.Stat(filepath.Join(in.dataDir, "storage", "objects", "uploads", asset)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the re-encode uploaded the source")
	}
}
