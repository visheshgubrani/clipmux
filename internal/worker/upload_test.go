package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// uploadServer signs every path to a PUT URL on itself and stores PUTs
// after a delay, recording how many overlap and the order they finish in.
type uploadServer struct {
	delay          time.Duration
	mu             sync.Mutex
	landed         []string
	inFlight, peak atomic.Int32
}

func (u *uploadServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sign"):
		var req struct{ Paths []string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := workerapi.SignResponse{Action: "continue", URLs: map[string]string{}}
		for _, p := range req.Paths {
			out.URLs[p] = "http://" + r.Host + "/put/" + p
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPut:
		n := u.inFlight.Add(1)
		defer u.inFlight.Add(-1)
		for p := u.peak.Load(); n > p && !u.peak.CompareAndSwap(p, n); p = u.peak.Load() {
		}
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(u.delay)
		u.mu.Lock()
		u.landed = append(u.landed, strings.TrimPrefix(r.URL.Path, "/put/"))
		u.mu.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func TestUploadKeepsThePoolFullAcrossSignBatchesAndPutsTheManifestLast(t *testing.T) {
	// 600 files cross the first 500-path sign batch.
	const files = 600
	dir := t.TempDir()
	var m media.Manifest
	for i := range files {
		p := fmt.Sprintf("v/seg_%04d.m4s", i)
		if err := os.MkdirAll(filepath.Join(dir, "v"), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, media.File{Path: p, Size: 1})
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	us := &uploadServer{delay: 20 * time.Millisecond}
	srv := httptest.NewServer(us)
	defer srv.Close()
	w := &Worker{Server: srv.URL, HTTP: DefaultHTTPClient()}
	job := &workerapi.Job{AttemptID: "att_1", AttemptToken: "tok"}

	start := time.Now()
	if err := w.upload(context.Background(), job, dir, m, func(string, float64) {}); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)

	if got := len(us.landed); got != files+1 {
		t.Fatalf("%d files landed, want %d", got, files+1)
	}
	if last := us.landed[len(us.landed)-1]; last != "manifest.json" {
		t.Errorf("last file to land is %s, want manifest.json", last)
	}
	if p := us.peak.Load(); p != uploadConcurrency {
		t.Errorf("%d PUTs overlapped at most, want %d", p, uploadConcurrency)
	}
	// 600 PUTs of 20 ms, 32 at a time: about 0.4 s; 8 at a time was 1.5 s.
	if took > time.Second {
		t.Errorf("upload took %v", took)
	}
}
