package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
func start(t *testing.T) *instance { return boot(t, true) }

// startUnpinned boots serve without CLIPMUX_PUBLIC_URL, as a fresh install
// whose owner picks the URLs at first boot.
func startUnpinned(t *testing.T) *instance { return boot(t, false) }

func boot(t *testing.T, pinned bool) *instance {
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
	in := &instance{t: t, pool: pool, url: fmt.Sprintf("http://127.0.0.1:%d", port), dataDir: t.TempDir()}
	in.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"DATABASE_URL=" + dbURL + sep + "search_path=" + schema,
		"CLIPMUX_DATA_DIR=" + in.dataDir,
		fmt.Sprintf("CLIPMUX_LISTEN=127.0.0.1:%d", port),
	}
	if pinned {
		in.env = append(in.env, "CLIPMUX_PUBLIC_URL="+in.url)
	}
	if p := os.Getenv("CLIPMUX_PACKAGER"); p != "" {
		in.env = append(in.env, "CLIPMUX_PACKAGER="+p)
	}
	in.cmd = exec.Command(bin, "serve")
	in.cmd.Env = in.env
	pr, pw := io.Pipe()
	in.cmd.Stdout, in.cmd.Stderr = pw, pw
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
	t.Cleanup(func() {
		_ = in.cmd.Process.Signal(os.Interrupt)
		_ = in.cmd.Wait()
		pw.Close()
		if t.Failed() {
			t.Logf("serve logs:\n%s", in.log())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		if res, err := http.Get(in.url + "/readyz"); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return in
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve never became ready:\n%s", in.log())
		}
		time.Sleep(100 * time.Millisecond)
	}
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
	if s := in.call("POST", "/v1/uploads", key, map[string]any{"size": len(data), "playback_policy": "signed"}, &created); s != 201 {
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
		var a struct{ Status, ErrorCode string }
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
