package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// bin is the clipmux binary under test, built once by TestMain.
var bin string

func TestMain(m *testing.M) {
	if os.Getenv("CLIPMUX_E2E") != "1" {
		fmt.Println("e2e: set CLIPMUX_E2E=1 to run the real-binary exit tests")
		os.Exit(0)
	}
	bin = os.Getenv("CLIPMUX_E2E_BIN")
	if bin == "" {
		dir, err := os.MkdirTemp("", "clipmux-e2e")
		if err != nil {
			panic(err)
		}
		defer os.RemoveAll(dir)
		bin = filepath.Join(dir, "clipmux")
		build := exec.Command("go", "build", "-o", bin, "../../cmd/clipmux")
		build.Stdout, build.Stderr = os.Stdout, os.Stderr
		if err := build.Run(); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

// instance is one running `clipmux serve`.
type instance struct {
	t       *testing.T
	pool    *pgxpool.Pool
	url     string
	dataDir string
	env     []string
	cmd     *exec.Cmd
	pw      *io.PipeWriter
	mu      sync.Mutex
	logs    bytes.Buffer
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// start boots serve on a fresh schema with the built-in worker, its public
// URL pinned to where it listens.
func start(t *testing.T, extraEnv ...string) *instance { return boot(t, true, extraEnv...) }

// startUnpinned boots serve without CLIPMUX_PUBLIC_URL, as a fresh install
// whose owner picks the URLs at first boot.
func startUnpinned(t *testing.T) *instance { return boot(t, false) }

// servePortTaken is set once a pinned install has CLIPMUX_E2E_SERVE_PORT.
var servePortTaken atomic.Bool

func boot(t *testing.T, pinned bool, extraEnv ...string) *instance {
	t.Helper()
	pool := dbtest.New(t)
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dbURL := os.Getenv("TEST_DATABASE_URL")
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	port := freePort(t)
	// A device behind `adb reverse` needs to know the port in advance. Only
	// the first pinned install takes it: a second (the fixture's TOTP
	// install) would fail to bind while /readyz answered from the first.
	if p, err := strconv.Atoi(os.Getenv("CLIPMUX_E2E_SERVE_PORT")); err == nil && pinned && servePortTaken.CompareAndSwap(false, true) {
		port = p
	}
	in := &instance{t: t, pool: pool, url: fmt.Sprintf("http://127.0.0.1:%d", port), dataDir: t.TempDir()}
	in.env = []string{
		"PATH=" + os.Getenv("PATH"),
		// At most 8 connections each, so the Phase 4 tests' parallel
		// instances fit Postgres's default 100 on any number of cores.
		"DATABASE_URL=" + dbURL + sep + "search_path=" + schema + "&pool_max_conns=8",
		"CLIPMUX_DATA_DIR=" + in.dataDir,
		fmt.Sprintf("CLIPMUX_LISTEN=127.0.0.1:%d", port),
	}
	if pinned {
		in.env = append(in.env, "CLIPMUX_PUBLIC_URL="+in.url)
	}
	// Every real-binary run logs JSON at debug, and its logs are checked
	// for credentials when it ends (D064): every request, playback included,
	// is in them.
	in.env = append(in.env, "CLIPMUX_LOG_FORMAT=json", "CLIPMUX_LOG_LEVEL=debug")
	// Transcription is off unless a test turns it on: each instance's data
	// directory is new, and the speech models are 0.9 GB.
	in.env = append(in.env, "CLIPMUX_SPEECH=off")
	in.env = append(in.env, extraEnv...)
	for _, k := range []string{"CLIPMUX_PACKAGER", "CLIPMUX_UNSANDBOXED_WORKER"} {
		if v := os.Getenv(k); v != "" {
			in.env = append(in.env, k+"="+v)
		}
	}
	t.Cleanup(func() {
		in.halt(os.Interrupt)
		checkLogs(t, "serve", in.log())
		if t.Failed() {
			t.Logf("serve logs:\n%s", in.log())
		}
	})
	in.launch()
	return in
}

// launch starts serve and waits until it is ready.
func (in *instance) launch() {
	t := in.t
	t.Helper()
	in.cmd = exec.Command(bin, "serve")
	in.cmd.Env = in.env
	// A relative CLIPMUX_DATA_DIR resolves here, as it would from make run.
	in.cmd.Dir = in.dataDir
	// Its own process group, so a crash takes the built-in worker with it,
	// as it would in a container.
	in.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw := io.Pipe()
	in.cmd.Stdout, in.cmd.Stderr, in.pw = pw, pw, pw
	go func() {
		s := bufio.NewScanner(pr)
		for s.Scan() {
			in.mu.Lock()
			in.logs.WriteString(s.Text() + "\n")
			in.mu.Unlock()
		}
	}()
	if err := in.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if res, err := http.Get(in.url + "/readyz"); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never became ready:\n%s", in.log())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// halt stops serve with sig and waits for it; os.Kill is a crash, which
// kills its process group. It does nothing if serve isn't running.
func (in *instance) halt(sig os.Signal) {
	if in.cmd == nil || in.cmd.ProcessState != nil {
		return
	}
	if sig == os.Kill {
		_ = syscall.Kill(-in.cmd.Process.Pid, syscall.SIGKILL)
	} else {
		_ = in.cmd.Process.Signal(sig)
	}
	_ = in.cmd.Wait()
	in.pw.Close()
}

// restart stops serve cleanly and starts it again on the same database,
// data directory and port.
func (in *instance) restart() {
	in.t.Helper()
	in.halt(os.Interrupt)
	in.launch()
}

func (in *instance) log() string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.logs.String()
}

// admin runs `clipmux admin …` against this instance's database.
func (in *instance) admin(args ...string) string {
	in.t.Helper()
	cmd := exec.Command(bin, append([]string{"admin"}, args...)...)
	cmd.Env = in.env
	cmd.Dir = in.dataDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		in.t.Fatalf("clipmux admin %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// credentials creates an app's API key and signing key.
func (in *instance) credentials(app string) (key, kid, privatePEM string) {
	in.t.Helper()
	out := in.admin("create-key", "--app", app, "--allow-loopback", "--scopes", "uploads:write,assets:read,assets:write")
	key = regexp.MustCompile(`key:\s+(cmx_\S+)`).FindStringSubmatch(out)[1]
	out = in.admin("create-signing-key", "--app", app)
	kid = regexp.MustCompile(`kid: (kid_\S+)`).FindStringSubmatch(out)[1]
	privatePEM = out[strings.Index(out, "-----BEGIN"):]
	privatePEM = privatePEM[:strings.Index(privatePEM, "-----END PRIVATE KEY-----")+len("-----END PRIVATE KEY-----")] + "\n"
	return key, kid, privatePEM
}

func (in *instance) call(method, path, bearer string, body any, out any) int {
	in.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, in.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		in.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

// fixture writes a test clip: testsrc2 with a tone, seconds long, w×h.
func fixture(t *testing.T, seconds, w, h int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.mp4")
	out, err := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=25", w, h),
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", fmt.Sprint(seconds), "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg fixture: %v\n%s", err, out)
	}
	return path
}

// upload sends a file through the real upload API, as an app's backend and
// a browser would, and returns the asset and its signed playback ID.
func (in *instance) upload(key, path string) (asset, pid string) {
	in.t.Helper()
	return in.uploadWith(key, path, nil)
}

// uploadWith is upload with more fields in the create request, such as
// generate.
func (in *instance) uploadWith(key, path string, extra map[string]any) (asset, pid string) {
	in.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		in.t.Fatal(err)
	}
	var created struct {
		UploadID    string                `json:"upload_id"`
		AssetID     string                `json:"asset_id"`
		PartSize    int64                 `json:"part_size"`
		PartCount   int                   `json:"part_count"`
		UploadToken string                `json:"upload_token"`
		PlaybackIDs []struct{ ID string } `json:"playback_ids"`
	}
	body := map[string]any{"size": len(data), "playback_policy": "signed"}
	maps.Copy(body, extra)
	if s := in.call("POST", "/v1/uploads", key, body, &created); s != 201 {
		in.t.Fatalf("create upload: %d", s)
	}
	numbers := make([]int, created.PartCount)
	for i := range numbers {
		numbers[i] = i + 1
	}
	var signed struct {
		Parts []struct {
			PartNumber int    `json:"part_number"`
			URL        string `json:"url"`
		} `json:"parts"`
	}
	in.call("POST", "/v1/uploads/"+created.UploadID+"/parts", created.UploadToken, map[string]any{"part_numbers": numbers}, &signed)
	var parts []map[string]any
	for _, p := range signed.Parts {
		start := int64(p.PartNumber-1) * created.PartSize
		end := min(start+created.PartSize, int64(len(data)))
		req, _ := http.NewRequest("PUT", p.URL, bytes.NewReader(data[start:end]))
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			in.t.Fatalf("PUT part %d: %v %v", p.PartNumber, err, res)
		}
		res.Body.Close()
		parts = append(parts, map[string]any{"part_number": p.PartNumber, "etag": res.Header.Get("ETag")})
	}
	if s := in.call("POST", "/v1/uploads/"+created.UploadID+"/complete", created.UploadToken, map[string]any{"parts": parts}, nil); s != 200 {
		in.t.Fatalf("complete: %d", s)
	}
	return created.AssetID, created.PlaybackIDs[0].ID
}

// waitReady polls the asset until it is ready.
func (in *instance) waitReady(key, asset string, limit time.Duration) {
	in.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		var a struct {
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		}
		in.call("GET", "/v1/assets/"+asset, key, nil, &a)
		switch {
		case a.Status == "ready":
			return
		case a.Status == "errored":
			in.t.Fatalf("asset errored: %s", a.ErrorCode)
		case time.Now().After(deadline):
			in.t.Fatalf("asset still %s after %s", a.Status, limit)
		}
		time.Sleep(time.Second)
	}
}

// setupTokenLine finds the setup token serve prints, in JSON or text.
var setupTokenLine = regexp.MustCompile(`setup_token(?:=|":")([A-Z2-7]+)`)

// leaks are what a credential looks like in a log: API, agent and dispatch
// keys, webhook secrets, a JWT (in a /v/ path or anywhere), and a URL's
// signature. Redacted values read REDACTED, which none of these match.
var leaks = regexp.MustCompile(`\bcmx[ad]?_[A-Z2-7]{6}|whsec_[A-Za-z0-9]{6}|eyJ[A-Za-z0-9_-]{8,}\.|X-Amz-Signature=[0-9a-f]|[?&]sig=[0-9a-f]|[?&]token=[A-Za-z0-9]`)

// checkLogs fails the test if any line of a run's log isn't a JSON object
// with time, level and msg, or holds something that looks like a
// credential. The setup token is printed on purpose until an owner exists.
func checkLogs(t *testing.T, name, logs string) {
	t.Helper()
	for i, line := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("%s log line %d isn't JSON: %.200s", name, i+1, line)
			continue
		}
		for _, k := range []string{"time", "level", "msg"} {
			if _, ok := rec[k]; !ok {
				t.Errorf("%s log line %d has no %s: %.200s", name, i+1, k, line)
			}
		}
		if m := leaks.FindString(line); m != "" {
			t.Errorf("%s log line %d holds a credential (%q): %.300s", name, i+1, m, line)
		}
	}
}
