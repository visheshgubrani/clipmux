package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	neturl "net/url"
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	cases := map[string]string{
		"key cmx_ABC234 used":                                                "key cmx_REDACTED used",
		"agent cmxa_Q7XK2 and cmxd_ZZ":                                       "agent cmxa_REDACTED and cmxd_REDACTED",
		"secret whsec_abc-DEF_1":                                             "secret whsec_REDACTED",
		"GET /v/eyJhbGc.eyJzdWIi.c2ln/pid_1/master.m3u8":                     "GET /v/REDACTED/pid_1/master.m3u8",
		"token eyJhbGciOi.eyJzdWIiOi.SflKxw end":                             "token REDACTED end",
		`Put "https://r2.example/b/k?X-Amz-Signature=abc&X-Amz-Date=1": EOF`: `Put "https://r2.example/b/k?REDACTED": EOF`,
		"/_storage/objects/k?exp=17&sig=ff":                                  "/_storage/objects/k?exp=REDACTED&sig=REDACTED",
		"ast_01 committed":                                                   "ast_01 committed",
		"route GET /v/{token}/{pid}":                                         "route GET /v/{token}/{pid}",
		"plain message":                                                      "plain message",
	}
	for in, want := range cases {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"https://media.example/v/eyJx.eyJy.sig/pid_1/720p/index.m3u8": "https://media.example/v/REDACTED/pid_1/720p/index.m3u8",
		"https://u:p@r2.example/b/k?X-Amz-Signature=abc#frag":         "https://r2.example/b/k?REDACTED",
		"https://example.com/video.mp4":                               "https://example.com/video.mp4",
	}
	for in, want := range cases {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactErrorKeepsOperationAndCause(t *testing.T) {
	cause := errors.New("connection refused")
	err := RedactError(&neturl.Error{Op: "Get", URL: "https://host.example/src?X-Amz-Signature=secret", Err: cause})
	if got, want := err.Error(), "Get https://host.example/src?REDACTED: connection refused"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("the cause is lost")
	}
	plain := errors.New("x")
	if RedactError(plain) != plain {
		t.Fatal("a plain error changed")
	}
}

func TestJSONLinesCarryContextAttributesAndAreScrubbed(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, "json", "debug")
	if err != nil {
		t.Fatal(err)
	}
	ctx := With(context.Background(), "request_id", "req_1")
	ctx = With(ctx, "job", "job_7")
	logger.With("component", "test").DebugContext(ctx, "fetched cmx_SECRET",
		"url", "https://s3.example/k?X-Amz-Signature=zz", "err", errors.New("bad whsec_abc"))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not one JSON line: %q", buf.String())
	}
	want := map[string]any{
		"level": "DEBUG", "msg": "fetched cmx_REDACTED", "component": "test",
		"url": "https://s3.example/k?REDACTED", "err": "bad whsec_REDACTED",
		"request_id": "req_1", "job": "job_7",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	if _, ok := line["time"]; !ok {
		t.Error("no time")
	}
	if Attr(ctx, "job") != "job_7" || Attr(ctx, "attempt") != "" {
		t.Error("Attr")
	}
}

func TestLevelAndFormatFromEnv(t *testing.T) {
	var buf bytes.Buffer
	env := map[string]string{EnvLevel: "warn"}
	logger, err := FromEnv(func(k string) string { return env[k] }, &buf)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hidden")
	logger.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "level=WARN msg=shown") {
		t.Fatalf("text at warn: %q", out)
	}
	for _, bad := range []map[string]string{{EnvLevel: "loud"}, {EnvFormat: "xml"}} {
		l, err := FromEnv(func(k string) string { return bad[k] }, &buf)
		if err == nil || l == nil {
			t.Errorf("%v: want an error and a usable logger", bad)
		}
	}
}
