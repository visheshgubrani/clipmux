package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/openapi"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestOpenAPIOperationsAreRouted proves every operation in the spec (D112)
// reaches an API handler on the real serve mux: the answer is JSON, never
// the dashboard's HTML or the mux's own 404 or 405, and an operation that
// needs a credential refuses a request without one.
func TestOpenAPIOperationsAreRouted(t *testing.T) {
	ops, err := openapi.Operations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) < 30 {
		t.Fatalf("only %d operations in the spec", len(ops))
	}
	base, client := startServe(t)
	for _, op := range ops {
		path := pathParam.ReplaceAllStringFunc(op.Path, func(p string) string {
			if p == "{token}" {
				return "eyJhbGciOiJFUzI1NiJ9.e30.AAAA"
			}
			return "x_0"
		})
		var body io.Reader
		if op.Method == http.MethodPost || op.Method == http.MethodPatch {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequest(op.Method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s (%s): %d %s, not an API handler", op.Method, op.Path, op.ID, res.StatusCode, ct)
			continue
		}
		if !op.Public && !op.Media && res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s (%s): %d without a credential, want 401", op.Method, op.Path, op.ID, res.StatusCode)
		}
	}
}

func TestOpenAPIIsServed(t *testing.T) {
	rec := get(t, Handler(Deps{DB: fakeDB{}}), "/openapi.json")
	var doc struct {
		OpenAPI string `json:"openapi"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil || doc.OpenAPI != "3.1.0" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}
}

// TestOpenAPICoversTheDocumentedIntegrationRoutes keeps docs/api.md and the
// spec together: every route row in the sections an app calls is in the
// spec, and every non-delivery operation in the spec is documented there.
func TestOpenAPICoversTheDocumentedIntegrationRoutes(t *testing.T) {
	f, err := os.Open("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	row := regexp.MustCompile("^\\| `(GET|POST|PATCH|PUT|DELETE) (/[^`?]*)")
	sections := map[string]bool{"## Integration API": true, "## App pairing": true, "## Analytics (D100, D103)": true, "## Share links (D104)": true}
	documented := map[string]bool{}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "## ") {
			in = sections[line]
			continue
		}
		m := row.FindStringSubmatch(line)
		if !in || m == nil {
			continue
		}
		if m[1] == http.MethodGet && strings.HasPrefix(m[2], "/s/") {
			continue // the share page is HTML, not an API
		}
		documented[m[1]+" "+pathParam.ReplaceAllString(m[2], "{}")] = true
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	ops, err := openapi.Operations()
	if err != nil {
		t.Fatal(err)
	}
	inSpec := map[string]bool{}
	for _, op := range ops {
		key := op.Method + " " + pathParam.ReplaceAllString(op.Path, "{}")
		inSpec[key] = true
		if !op.Media && !documented[key] {
			t.Errorf("%s (%s) is in the spec but not in docs/api.md's integration sections", key, op.ID)
		}
	}
	for key := range documented {
		if !inSpec[key] {
			t.Errorf("docs/api.md documents %s, but the spec doesn't", key)
		}
	}
	if len(documented) < 25 {
		t.Fatalf("parsed only %d documented routes", len(documented))
	}
}

// startServe runs serve on a free port against a fresh schema and returns
// its base URL and a client for it; it stops when the test ends.
//
// The client keeps no connections: a pooled transport can dial a spare
// connection it never sends on, and Shutdown waits up to 5 s for such a
// connection's first request (golang.org/issue/22682), which made this
// test's stop deadline flaky.
func startServe(t *testing.T) (string, *http.Client) {
	t.Helper()
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://localhost"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, config.Config{}, ln, slog.New(slog.NewTextHandler(io.Discard, nil)), testBoot(t, pool, store, testBox(t)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop")
		}
	})
	base := "http://" + ln.Addr().String()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; ; i++ {
		res, err := client.Get(base + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return base, client
			}
		}
		if i == 100 {
			t.Fatalf("server never became ready: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
