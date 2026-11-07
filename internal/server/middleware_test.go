package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/logging"
)

// logged runs one request through the request log over a mux holding the
// patterns the server uses, and returns the response and the decoded line.
func logged(t *testing.T, l requestLog, req *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := logging.New(&buf, "json", "debug")
	if err != nil {
		t.Fatal(err)
	}
	l.logger = logger
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }
	mux.HandleFunc("GET /v/{token}/{pid}/{path...}", ok)
	mux.HandleFunc("GET /v1/agents/pairings/{code}", ok)
	mux.HandleFunc("GET /v1/assets/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	})
	rec := httptest.NewRecorder()
	l.wrap(mux).ServeHTTP(rec, req)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %q", buf.String())
	}
	if strings.Contains(buf.String(), "SECRET") {
		t.Fatalf("a credential reached the log: %s", buf.String())
	}
	return rec, line
}

func TestRequestLogRedactsThePlaybackToken(t *testing.T) {
	req := httptest.NewRequest("GET", "/v/eyJSECRET.eyJx.sig/pid_1/master.m3u8?debug=SECRET", nil)
	req.Header.Set("Authorization", "Bearer SECRET")
	req.Header.Set("Referer", "https://lms.example/lesson?SECRET")
	rec, line := logged(t, requestLog{}, req)
	want := map[string]any{
		"level": "DEBUG", "msg": "http request", "method": "GET",
		"route": "GET /v/{token}/{pid}/{path...}", "path": "/v/REDACTED/pid_1/master.m3u8",
		"status": float64(200), "bytes": float64(5),
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	id, _ := line["request_id"].(string)
	if !strings.HasPrefix(id, "req_") || rec.Header().Get("X-Request-Id") != id {
		t.Errorf("request_id %q, header %q", id, rec.Header().Get("X-Request-Id"))
	}
}

func TestRequestLogRedactsPairingCodes(t *testing.T) {
	_, line := logged(t, requestLog{}, httptest.NewRequest("GET", "/v1/agents/pairings/SECRET-CODE", nil))
	if line["path"] != "/v1/agents/pairings/REDACTED" || line["level"] != "INFO" {
		t.Fatalf("got %v", line)
	}
}

func TestRequestLogKeepsOtherWildcardsAndErrors(t *testing.T) {
	_, line := logged(t, requestLog{}, httptest.NewRequest("GET", "/v1/assets/ast_1", nil))
	if line["path"] != "/v1/assets/ast_1" || line["status"] != float64(http.StatusTeapot) {
		t.Fatalf("got %v", line)
	}
	_, line = logged(t, requestLog{}, httptest.NewRequest("GET", "/nowhere/v/x", nil))
	if line["path"] != "/nowhere/v/x" || line["route"] != "" || line["status"] != float64(404) {
		t.Fatalf("unmatched: %v", line)
	}
}

func TestRequestIDIsBelievedOnlyFromTrustedProxies(t *testing.T) {
	trusted := requestLog{trusted: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	for _, c := range []struct {
		name, remote, id string
		kept             bool
	}{
		{"untrusted peer", "203.0.113.5:4000", "abc-123", false},
		{"trusted proxy", "10.1.2.3:4000", "abc-123", true},
		{"trusted proxy, forged fields", "10.1.2.3:4000", `x" level=ERROR`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/assets/ast_1", nil)
			req.RemoteAddr = c.remote
			req.Header.Set("X-Request-Id", c.id)
			_, line := logged(t, trusted, req)
			if got := line["request_id"] == c.id; got != c.kept {
				t.Fatalf("request_id %v, kept %v want %v", line["request_id"], got, c.kept)
			}
		})
	}
}

func TestRecorderUnwrapsForResponseController(t *testing.T) {
	rec := &recorder{ResponseWriter: httptest.NewRecorder()}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through the recorder: %v", err)
	}
	if n, err := rec.ReadFrom(strings.NewReader("abc")); n != 3 || err != nil || rec.bytes != 3 || rec.status != 200 {
		t.Fatalf("ReadFrom: %d %v bytes=%d status=%d", n, err, rec.bytes, rec.status)
	}
}

func TestAPanickingHandlerGets500AndServingContinues(t *testing.T) {
	var buf bytes.Buffer
	logger, _ := logging.New(&buf, "json", "info")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom: a bad row") })
	mux.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("after writing")
	})
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(requestLog{logger: logger}.wrap(mux))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 500 || !strings.Contains(string(body), `"code":"internal"`) {
		t.Fatalf("boom: %d %s", res.StatusCode, body)
	}
	// A response that had started can't become a 500: the connection is
	// dropped, so the client never sees a complete success.
	if res, err := http.Get(srv.URL + "/half"); err == nil {
		_, rerr := io.ReadAll(res.Body)
		res.Body.Close()
		if rerr == nil {
			t.Fatalf("a half-written response completed: %d", res.StatusCode)
		}
	}
	res, err = http.Get(srv.URL + "/ok")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("the server stopped serving after panics: %v %v", res, err)
	}
	res.Body.Close()
	out := buf.String()
	if !strings.Contains(out, `"msg":"handler panicked"`) || !strings.Contains(out, "boom: a bad row") ||
		!strings.Contains(out, `"stack":"goroutine`) {
		t.Fatalf("the panic wasn't logged with its stack: %s", out)
	}
	if !strings.Contains(out, `"path":"/boom","status":500`) {
		t.Fatalf("no access line for the panicked request: %s", out)
	}
}
