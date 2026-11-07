package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

func testWorker(server string) *Worker {
	return &Worker{Server: server, HTTP: http.DefaultClient, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), IdleTimeout: 200 * time.Millisecond}
}

// source serves body with Range support; the first `drops` requests send
// half of what's asked and then cut the connection, and `stalls` requests
// send a little and then hang.
func source(t *testing.T, body string, drops, stalls int32) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := n.Add(1)
		start := 0
		if rg := r.Header.Get("Range"); rg != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(body)-1)+"/"+strconv.Itoa(len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		}
		rest := body[start:]
		switch {
		case i <= drops:
			_, _ = io.WriteString(w, rest[:max(1, len(rest)/2)])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case i <= drops+stalls:
			_, _ = io.WriteString(w, rest[:1])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, rest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestDownloadThatFinishesOnTheLastTrySucceeds(t *testing.T) {
	body := strings.Repeat("0123456789", 6400) // 64 KB, halved five times then finished
	srv, n := source(t, body, 5, 0)
	dst := filepath.Join(t.TempDir(), "src")
	job := &workerapi.Job{Source: workerapi.Source{URL: srv.URL, Size: int64(len(body))}}
	if err := testWorker(srv.URL).download(context.Background(), job, dst); err != nil {
		t.Fatalf("download: %v (after %d requests)", err, n.Load())
	}
	got, _ := os.ReadFile(dst)
	if string(got) != body {
		t.Fatalf("downloaded %d bytes, want %d intact", len(got), len(body))
	}
}

func TestStalledDownloadIsCutAndResumed(t *testing.T) {
	body := strings.Repeat("x", 10_000)
	srv, _ := source(t, body, 0, 1)
	dst := filepath.Join(t.TempDir(), "src")
	job := &workerapi.Job{Source: workerapi.Source{URL: srv.URL, Size: int64(len(body))}}
	start := time.Now()
	if err := testWorker(srv.URL).download(context.Background(), job, dst); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a stalled transfer took %v to recover", time.Since(start))
	}
	if got, _ := os.ReadFile(dst); string(got) != body {
		t.Fatal("resumed download is not intact")
	}
}

func TestRedactRemovesSignaturesAndTokens(t *testing.T) {
	err := logging.RedactError(&url.Error{Op: "Get", URL: "https://m.example/_storage/objects/uploads/a/source?exp=1&sig=SECRET", Err: errors.New("EOF")})
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "sig=") {
		t.Fatalf("signature leaked: %v", err)
	}
	err = logging.RedactError(&url.Error{Op: "Get", URL: "https://m.example/v/eyJTOKEN.x.y/pid_1/master.m3u8", Err: errors.New("EOF")})
	if strings.Contains(err.Error(), "eyJTOKEN") {
		t.Fatalf("token leaked: %v", err)
	}
	if !errors.Is(logging.RedactError(&url.Error{Op: "Get", URL: "http://x", Err: io.ErrUnexpectedEOF}), io.ErrUnexpectedEOF) {
		t.Fatal("redact must keep the cause")
	}
}

func TestReportRetriesServerErrorsButNotRefusals(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := calls.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/fail") && c <= 2:
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			w.WriteHeader(http.StatusUnauthorized)
		default:
			_, _ = io.WriteString(w, `{"action":"stop"}`)
		}
	}))
	defer srv.Close()
	w := testWorker(srv.URL)
	job := &workerapi.Job{AttemptID: "att_1", AttemptToken: "t"}
	if err := w.fail(context.Background(), job, true, "unsupported_source", errors.New("bad")); err != nil || calls.Load() != 3 {
		t.Fatalf("fail after two 500s: %v, %d calls", err, calls.Load())
	}
	calls.Store(10)
	if _, err := w.report(context.Background(), job, "complete", map[string]any{}); err == nil || calls.Load() != 11 {
		t.Fatalf("a 401 must fail at once: %v, %d calls", err, calls.Load()-10)
	}
}

func TestPutTreatsAlreadyWrittenAsDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv.Close()
	f := filepath.Join(t.TempDir(), "seg")
	_ = os.WriteFile(f, []byte("data"), 0o600)
	if err := testWorker(srv.URL).putFile(context.Background(), srv.URL+"/x", f); err != nil {
		t.Fatalf("412 on a create-once PUT means an earlier try landed: %v", err)
	}
}

// R2 and S3 sign If-None-Match: * into a create-once URL, so a PUT without
// the header is refused there (D016).
func TestPutSendsTheCreateOnceHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got = r.Header.Get("If-None-Match")
	}))
	defer srv.Close()
	f := filepath.Join(t.TempDir(), "seg")
	_ = os.WriteFile(f, []byte("data"), 0o600)
	if err := testWorker(srv.URL).putFile(context.Background(), srv.URL+"/x", f); err != nil || got != "*" {
		t.Fatalf("putFile = %v, If-None-Match %q; want *", err, got)
	}
}

// Once the owner sets a public URL, signed storage URLs point at it. The
// built-in worker reaches serve over loopback instead, so it never depends
// on the public hostname routing back to this machine (a hairpin).
func TestStorageURLsGoThroughTheServerWhenAsked(t *testing.T) {
	w := &Worker{Server: "http://127.0.0.1:8080", StorageViaServer: true}
	cases := map[string]string{
		"https://video.example.com/_storage/objects/uploads/ast_1/source?exp=1&sig=ab": "http://127.0.0.1:8080/_storage/objects/uploads/ast_1/source?exp=1&sig=ab",
		"https://bucket.r2.example.com/uploads/ast_1/source?X-Amz-Signature=cd":        "https://bucket.r2.example.com/uploads/ast_1/source?X-Amz-Signature=cd",
	}
	for in, want := range cases {
		if got := w.storageURL(in); got != want {
			t.Errorf("storageURL(%q) = %q, want %q", in, got, want)
		}
	}
	w.StorageViaServer = false
	in := "https://video.example.com/_storage/objects/x?exp=1&sig=ab"
	if got := w.storageURL(in); got != in {
		t.Errorf("without StorageViaServer: %q, want it unchanged", got)
	}
}

// fakeTools are scripts that answer the version checks a claim makes.
func fakeTools(t *testing.T) media.Tools {
	t.Helper()
	dir := t.TempDir()
	write := func(name, out string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho '"+out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return media.Tools{FFmpeg: write("ffmpeg", "ffmpeg version 8.0"), Packager: write("packager", "packager version v3.9.3-x")}
}

func TestARevokedKeyEndsTheWorkerAndStaleAttemptsAreCleared(t *testing.T) {
	var claimed map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&claimed)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "att_left_by_a_crash"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Server: srv.URL, Key: "cmxa_revoked", WorkDir: dir, Tools: fakeTools(t), Logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Run(ctx); !errors.Is(err, ErrKeyRefused) {
		t.Fatalf("Run = %v, want ErrKeyRefused without retrying", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("scratch directory still holds %v", entries)
	}
	if free, _ := claimed["free_disk_bytes"].(float64); free <= 0 {
		t.Fatalf("claim reported free disk %v, want the scratch directory's", claimed["free_disk_bytes"])
	}
}

// A provider's machine keeps asking for its one job through server errors
// and empty polls, and stops when the server refuses its key (D054).
func TestADispatchedWorkerRetriesUntilItsKeyIsRefused(t *testing.T) {
	answers := []int{http.StatusServiceUnavailable, http.StatusNoContent, http.StatusUnauthorized}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := calls.Add(1) - 1
		w.WriteHeader(answers[min(int(i), len(answers)-1)])
	}))
	defer srv.Close()
	w := &Worker{Server: srv.URL, Key: "cmxd_test", WorkDir: t.TempDir(), Tools: fakeTools(t), Logger: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.RunDispatched(ctx); !errors.Is(err, ErrKeyRefused) {
		t.Fatalf("RunDispatched = %v, want ErrKeyRefused", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("claimed %d times, want 3 (503, 204, 401)", calls.Load())
	}
}
