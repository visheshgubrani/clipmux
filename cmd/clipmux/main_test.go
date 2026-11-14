package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunWithoutArgsPrintsUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("clipmux serve")) {
		t.Fatalf("usage missing from stderr: %q", errOut.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.String() != "dev\n" {
		t.Fatalf("version = %q, want %q", out.String(), "dev\n")
	}
}

func TestHealthcheckAsksTheLocalServe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	env := func(listen string) func(string) string {
		return func(k string) string {
			if k == "CLIPMUX_LISTEN" {
				return listen
			}
			return ""
		}
	}
	if code := healthcheck(env(":"+port), io.Discard); code != 0 {
		t.Fatalf("healthy serve: exit %d", code)
	}
	if code := healthcheck(env("127.0.0.1:1"), io.Discard); code != 1 {
		t.Fatalf("no serve: exit %d, want 1", code)
	}
}

func TestListenTakesTheServerFromTheAppsEnvFile(t *testing.T) {
	// The same CLIPMUX_URL the SDK's env file has; the server refuses the
	// key, so listen reaches it and stops with 1 rather than asking for --server.
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthorized","message":"bad key"}}`)
	}))
	defer srv.Close()
	env := map[string]string{"CLIPMUX_URL": srv.URL, "CLIPMUX_API_KEY": "cmx_x", "CLIPMUX_LISTEN_SECRET": "whsec_x"}
	code := runListen([]string{"--forward-to", "localhost:1/hook"}, func(k string) string { return env[k] }, io.Discard, io.Discard)
	if code != 1 || asked != "/v1/events" {
		t.Fatalf("exit %d after asking %q; want 1 after /v1/events", code, asked)
	}
}
