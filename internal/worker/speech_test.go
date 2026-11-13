package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/media"
)

func TestParseSpeechMode(t *testing.T) {
	for in, want := range map[string]SpeechMode{"": SpeechAuto, "auto": SpeechAuto, "off": SpeechOff, "cpu": SpeechCPU, "vulkan": SpeechVulkan} {
		if got, err := ParseSpeechMode(in); err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	if _, err := ParseSpeechMode("cuda"); err == nil || !strings.Contains(err.Error(), "D077") {
		t.Errorf("cuda: %v, want the reason there is none", err)
	}
	if _, err := ParseSpeechMode("gpu"); err == nil {
		t.Error("an unknown mode must be refused")
	}
}

// Models are fetched once, checked against their pinned size and sha256,
// and a download that doesn't match leaves nothing behind.
func TestFetchModelChecksTheDigest(t *testing.T) {
	body := []byte("model weights")
	sum := sha256.Sum256(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dir := t.TempDir()
	m := media.Model{Name: "test", URL: srv.URL + "/ggml-test.bin", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	ctx := context.Background()
	path, err := FetchModel(ctx, srv.Client(), dir, m)
	if err != nil || path != filepath.Join(dir, "ggml-test.bin") {
		t.Fatalf("fetch = %s, %v", path, err)
	}
	if b, _ := os.ReadFile(path); string(b) != string(body) {
		t.Fatalf("file %q", b)
	}
	if _, err := FetchModel(ctx, srv.Client(), dir, m); err != nil || hits.Load() != 1 {
		t.Fatalf("second fetch: %v, %d downloads; want the file kept", err, hits.Load())
	}

	bad := m
	bad.URL, bad.SHA256 = srv.URL+"/ggml-other.bin", strings.Repeat("0", 64)
	if _, err := FetchModel(ctx, srv.Client(), dir, bad); err == nil {
		t.Fatal("a download that doesn't match its digest must fail")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left %d files, want only the good model", len(entries))
	}
	// A file that was changed on disk is fetched again.
	if err := os.WriteFile(path, []byte("model weightz"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FetchModel(ctx, srv.Client(), dir, m); err != nil || hits.Load() != 3 {
		t.Fatalf("refetch: %v, %d downloads", err, hits.Load())
	}
}

// A model baked into the image is used as it is (D118): nothing is
// downloaded, and a file of the wrong size there falls back to the fetch.
func TestFindModelPrefersTheBakedCopy(t *testing.T) {
	body := []byte("model weights")
	sum := sha256.Sum256(body)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	m := media.Model{Name: "test", URL: srv.URL + "/ggml-test.bin", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
	baked, cache := t.TempDir(), t.TempDir()
	ctx := context.Background()

	// Baked files are trusted on size: these bytes don't match the digest,
	// and are used all the same.
	if err := os.WriteFile(filepath.Join(baked, "ggml-test.bin"), []byte("baked weight!"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := findModel(ctx, srv.Client(), baked, cache, m)
	if err != nil || path != filepath.Join(baked, "ggml-test.bin") || hits.Load() != 0 {
		t.Fatalf("find = %s, %v, %d downloads; want the baked file and no download", path, err, hits.Load())
	}

	if err := os.WriteFile(filepath.Join(baked, "ggml-test.bin"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err = findModel(ctx, srv.Client(), baked, cache, m)
	if err != nil || path != filepath.Join(cache, "ggml-test.bin") || hits.Load() != 1 {
		t.Fatalf("find = %s, %v, %d downloads; want a fetch into the cache", path, err, hits.Load())
	}
	if _, ok := BakedModel("", m); ok {
		t.Fatal("no baked directory must find nothing")
	}
}
