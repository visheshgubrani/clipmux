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
