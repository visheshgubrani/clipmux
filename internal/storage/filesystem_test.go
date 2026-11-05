package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPartSize = 8

type harness struct {
	t   *testing.T
	fs  *Filesystem
	dir string
	now time.Time
}

// newHarness serves a Filesystem over HTTP with 8-byte parts, so multipart
// behaviour is visible without 32 MiB fixtures.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	fsys, err := OpenFilesystem(h.dir, FilesystemOptions{BaseURL: srv.URL, PartSize: testPartSize, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	h.fs, handler = fsys, fsys.Handler()
	return h
}

func (h *harness) do(method, rawURL string, body []byte, header map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { res.Body.Close() })
	return res
}

func (h *harness) putPart(key, uploadID string, n int, body string) *http.Response {
	h.t.Helper()
	u, err := h.fs.PresignPart(context.Background(), key, uploadID, n, 15*time.Minute)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.do(http.MethodPut, u, []byte(body), nil)
}

func (h *harness) read(key string) string {
	h.t.Helper()
	rc, err := h.fs.Get(context.Background(), key)
	if err != nil {
		h.t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestPutGetHeadRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.fs.Put(ctx, "_routes/pid_1.json", strings.NewReader(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := h.read("_routes/pid_1.json"); got != `{"v":1}` {
		t.Fatalf("Get = %q", got)
	}
	obj, err := h.fs.Head(ctx, "_routes/pid_1.json")
	if err != nil || obj.Size != 7 || obj.Key != "_routes/pid_1.json" {
		t.Fatalf("Head = %+v, %v", obj, err)
	}
	if _, err := h.fs.Get(ctx, "_routes/missing.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: err = %v, want ErrNotFound", err)
	}
	if _, err := h.fs.Head(ctx, "_routes/missing.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head missing: err = %v, want ErrNotFound", err)
	}
}

func TestUnsafeKeysAreRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, key := range []string{
		"", "/etc/passwd", "../outside", "media/../../outside", "media/./x", "media//x", "media/x/",
		`media\x`, "media/x\x00", "media/x y", "media/é", strings.Repeat("a", 513),
	} {
		if err := h.fs.Put(ctx, key, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q): err = %v, want ErrInvalidKey", key, err)
		}
	}
}

func TestSymlinksCannotEscapeTheStorageRoot(t *testing.T) {
	h := newHarness(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hostile process with disk access plants a symlink inside the root.
	if err := os.MkdirAll(filepath.Join(h.dir, "objects", "media"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.dir, "objects", "media", "leak")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fs.Get(context.Background(), "media/leak"); err == nil {
		t.Fatal("Get followed a symlink out of the storage root")
	}
}

func TestListIsSortedPagedAndPrefixed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	keys := []string{"media/a/o1/master.m3u8", "media/a/o1/720p/seg_00001.m4s", "media/a/o1.txt", "media/a/o2/master.m3u8", "media/b/o3/master.m3u8"}
	for _, k := range keys {
		if err := h.fs.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	// An in-progress multipart upload must never appear in a listing.
	if _, err := h.fs.CreateMultipart(ctx, "uploads/ast_1/source", 20); err != nil {
		t.Fatal(err)
	}

	var got []string
	cursor := ""
	for page := 0; page < 10; page++ {
		objs, next, err := h.fs.list(ctx, "media/a/", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			got = append(got, o.Key)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"media/a/o1.txt", "media/a/o1/720p/seg_00001.m4s", "media/a/o1/master.m3u8", "media/a/o2/master.m3u8"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("List(media/a/) = %v\nwant %v", got, want)
	}

	objs, _, err := h.fs.List(ctx, "media/a/o1/", "")
	if err != nil || len(objs) != 2 {
		t.Fatalf("List(media/a/o1/) = %v, %v", objs, err)
	}
	if objs, _, err := h.fs.List(ctx, "media/zzz/", ""); err != nil || len(objs) != 0 {
		t.Fatalf("List of a missing prefix = %v, %v", objs, err)
	}
}

func TestDeletePrefixRemovesOnlyThatDirectory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, k := range []string{"media/a/o1/master.m3u8", "media/a/o1/720p/init.mp4", "media/a/o10/master.m3u8"} {
		if err := h.fs.Put(ctx, k, strings.NewReader("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.fs.DeletePrefix(ctx, "media/a/o1"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("prefix without a trailing slash: err = %v, want ErrInvalidKey", err)
	}
	if err := h.fs.DeletePrefix(ctx, "media/a/o1/"); err != nil {
		t.Fatal(err)
	}
	objs, _, _ := h.fs.List(ctx, "media/", "")
	if len(objs) != 1 || objs[0].Key != "media/a/o10/master.m3u8" {
		t.Fatalf("after delete: %v, want only media/a/o10/master.m3u8", objs)
	}
	if err := h.fs.DeletePrefix(ctx, "media/none/"); err != nil {
		t.Fatalf("deleting a missing prefix: %v", err)
	}
}

func TestMultipartWritesPartsAtTheirOffsets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const key = "uploads/ast_1/source"
	mp, err := h.fs.CreateMultipart(ctx, key, 20) // 8 + 8 + 4 bytes
	if err != nil {
		t.Fatal(err)
	}
	if mp.PartSize != testPartSize || mp.UploadID == "" {
		t.Fatalf("multipart = %+v", mp)
	}

	// Parts arrive out of order, as parallel browser uploads do.
	etags := map[int]string{}
	for _, p := range []struct {
		n    int
		body string
	}{{3, "CCCC"}, {1, "AAAAAAAA"}, {2, "BBBBBBBB"}} {
		res := h.putPart(key, mp.UploadID, p.n, p.body)
		if res.StatusCode != http.StatusOK || res.Header.Get("ETag") == "" {
			t.Fatalf("part %d: %d etag %q", p.n, res.StatusCode, res.Header.Get("ETag"))
		}
		etags[p.n] = res.Header.Get("ETag")
	}

	parts, err := h.fs.ListParts(ctx, key, mp.UploadID)
	if err != nil || len(parts) != 3 || parts[0].Number != 1 || parts[2].Size != 4 {
		t.Fatalf("ListParts = %+v, %v", parts, err)
	}
	if err := h.fs.CompleteMultipart(ctx, key, mp.UploadID, []Part{{Number: 1, ETag: etags[1]}, {Number: 2, ETag: `"bogus"`}, {Number: 3, ETag: etags[3]}}); !errors.Is(err, ErrInvalidParts) {
		t.Fatalf("complete with a wrong ETag: err = %v, want ErrInvalidParts", err)
	}
	if err := h.fs.CompleteMultipart(ctx, key, mp.UploadID, []Part{{Number: 1, ETag: etags[1]}, {Number: 3, ETag: etags[3]}}); !errors.Is(err, ErrInvalidParts) {
		t.Fatalf("complete with a missing part: err = %v, want ErrInvalidParts", err)
	}
	if err := h.fs.CompleteMultipart(ctx, key, mp.UploadID, []Part{{Number: 1, ETag: etags[1]}, {Number: 2, ETag: etags[2]}, {Number: 3, ETag: etags[3]}}); err != nil {
		t.Fatal(err)
	}
	if got := h.read(key); got != "AAAAAAAABBBBBBBBCCCC" {
		t.Fatalf("assembled source = %q", got)
	}
	if _, err := h.fs.ListParts(ctx, key, mp.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a completed upload must be gone: err = %v", err)
	}
}

func TestPartsMustHaveExactSizesAndNeverSpillIntoTheNextPart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const key = "uploads/ast_1/source"
	mp, _ := h.fs.CreateMultipart(ctx, key, 20)

	if res := h.putPart(key, mp.UploadID, 2, "BBBBBBBB"); res.StatusCode != http.StatusOK {
		t.Fatalf("part 2: %d", res.StatusCode)
	}
	// An oversized part 1 would overwrite part 2's bytes if it were written.
	if res := h.putPart(key, mp.UploadID, 1, "AAAAAAAAXXXX"); res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized part: %d, want 413", res.StatusCode)
	}
	if res := h.putPart(key, mp.UploadID, 1, "AAAA"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("short non-final part: %d, want 400", res.StatusCode)
	}
	if res := h.putPart(key, mp.UploadID, 3, "CCC"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("final part of the wrong size: %d, want 400", res.StatusCode)
	}
	if _, err := h.fs.PresignPart(ctx, key, mp.UploadID, 4, time.Minute); !errors.Is(err, ErrInvalidParts) {
		t.Fatalf("part beyond the declared size: err = %v", err)
	}
	parts, _ := h.fs.ListParts(ctx, key, mp.UploadID)
	if len(parts) != 1 || parts[0].Number != 2 {
		t.Fatalf("only part 2 should be stored: %+v", parts)
	}
	res := h.putPart(key, mp.UploadID, 1, "AAAAAAAA")
	h.putPart(key, mp.UploadID, 3, "CCCC")
	parts, _ = h.fs.ListParts(ctx, key, mp.UploadID)
	var list []Part
	for _, p := range parts {
		list = append(list, Part{Number: p.Number, ETag: p.ETag})
	}
	if err := h.fs.CompleteMultipart(ctx, key, mp.UploadID, list); err != nil {
		t.Fatal(err)
	}
	if got := h.read(key); got != "AAAAAAAABBBBBBBBCCCC" || res.StatusCode != http.StatusOK {
		t.Fatalf("assembled = %q", got)
	}
}

func TestDeclaredSizeIsBoundedByTheTenThousandPartLimit(t *testing.T) {
	h := newHarness(t)
	if _, err := h.fs.CreateMultipart(context.Background(), "uploads/a/source", testPartSize*10_000+1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := h.fs.CreateMultipart(context.Background(), "uploads/a/source", 0); err == nil {
		t.Fatal("a zero-byte upload must be refused")
	}
}

func TestAbortDiscardsTheUpload(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mp, _ := h.fs.CreateMultipart(ctx, "uploads/a/source", 8)
	h.putPart("uploads/a/source", mp.UploadID, 1, "AAAAAAAA")
	signedBefore, _ := h.fs.PresignPart(ctx, "uploads/a/source", mp.UploadID, 1, time.Minute)
	if err := h.fs.AbortMultipart(ctx, "uploads/a/source", mp.UploadID); err != nil {
		t.Fatal(err)
	}
	if res := h.do(http.MethodPut, signedBefore, []byte("AAAAAAAA"), nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("URL signed before the abort: %d, want 404", res.StatusCode)
	}
	if _, err := h.fs.PresignPart(ctx, "uploads/a/source", mp.UploadID, 1, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("signing after abort: err = %v, want ErrNotFound", err)
	}
	if _, err := h.fs.Head(ctx, "uploads/a/source"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an aborted upload must not leave an object")
	}
	if err := h.fs.AbortMultipart(ctx, "uploads/b/source", mp.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("abort under another key: err = %v, want ErrNotFound", err)
	}
}

func TestSignedURLsAreTheOnlyAuthorisation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.fs.Put(ctx, "uploads/a/source", strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}
	get, _ := h.fs.PresignGet(ctx, "uploads/a/source", time.Hour)

	res := h.do(http.MethodGet, get, nil, map[string]string{"Range": "bytes=2-5"})
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusPartialContent || string(body) != "2345" {
		t.Fatalf("ranged GET: %d %q", res.StatusCode, body)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("stored bytes must never be sniffed: %v", res.Header)
	}

	// The same signature does not authorise another key, another method, or a tampered expiry.
	other := strings.Replace(get, "uploads/a/source", "uploads/b/source", 1)
	u, _ := url.Parse(get)
	q := u.Query()
	q.Set("exp", "9999999999")
	u.RawQuery = q.Encode()
	for name, req := range map[string]struct{ method, url string }{
		"other key":      {http.MethodGet, other},
		"PUT with a GET": {http.MethodPut, get},
		"later expiry":   {http.MethodGet, u.String()},
		"no signature":   {http.MethodGet, strings.Split(get, "?")[0]},
	} {
		if res := h.do(req.method, req.url, []byte("x"), nil); res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, res.StatusCode)
		}
	}

	h.now = h.now.Add(61 * time.Minute)
	if res := h.do(http.MethodGet, get, nil, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("expired URL: %d, want 403", res.StatusCode)
	}
}

func TestPresignedPutWritesAnObject(t *testing.T) {
	h := newHarness(t)
	put, err := h.fs.PresignPut(context.Background(), "media/a/o1/master.m3u8", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.do(http.MethodPut, put, []byte("#EXTM3U\n"), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d", res.StatusCode)
	}
	if got := h.read("media/a/o1/master.m3u8"); got != "#EXTM3U\n" {
		t.Fatalf("object = %q", got)
	}
}

func TestBrowserPreflightExposesETag(t *testing.T) {
	h := newHarness(t)
	mp, _ := h.fs.CreateMultipart(context.Background(), "uploads/a/source", 8)
	u, _ := h.fs.PresignPart(context.Background(), "uploads/a/source", mp.UploadID, 1, time.Minute)
	res := h.do(http.MethodOptions, u, nil, map[string]string{
		"Origin": "https://lms.example", "Access-Control-Request-Method": "PUT",
	})
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(res.Header.Get("Access-Control-Allow-Methods"), "PUT") {
		t.Fatalf("preflight: %d %v", res.StatusCode, res.Header)
	}
	put := h.do(http.MethodPut, u, []byte("AAAAAAAA"), map[string]string{"Origin": "https://lms.example"})
	if put.Header.Get("Access-Control-Expose-Headers") != "ETag" {
		t.Fatalf("part PUT must expose ETag to the browser: %v", put.Header)
	}
}

func TestSigningKeySurvivesARestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.fs.Put(ctx, "uploads/a/source", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	get, _ := h.fs.PresignGet(ctx, "uploads/a/source", time.Hour)

	reopened, err := OpenFilesystem(h.dir, FilesystemOptions{BaseURL: "http://unused", PartSize: testPartSize, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	srv := httptest.NewServer(reopened.Handler())
	defer srv.Close()
	u, _ := url.Parse(get)
	res := h.do(http.MethodGet, srv.URL+u.RequestURI(), nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("URL signed before a restart: %d, want 200", res.StatusCode)
	}
}

func TestPresignedPutsCreateOnceAndNeverOverwrite(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	put, _ := h.fs.PresignPut(ctx, "media/a/o1/720p/index.m3u8", 15*time.Minute)
	if res := h.do(http.MethodPut, put, []byte("encrypted playlist"), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("first PUT: %d", res.StatusCode)
	}
	// After commit, the worker still holds a valid URL: it must not be able
	// to swap the playlist.
	if res := h.do(http.MethodPut, put, []byte("swapped"), nil); res.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("second PUT: %d, want 412", res.StatusCode)
	}
	if got := h.read("media/a/o1/720p/index.m3u8"); got != "encrypted playlist" {
		t.Fatalf("object = %q after an attempted overwrite", got)
	}
	// The control plane's own Put still replaces (route records, key lists).
	if err := h.fs.Put(ctx, "media/a/o1/720p/index.m3u8", strings.NewReader("replaced")); err != nil || h.read("media/a/o1/720p/index.m3u8") != "replaced" {
		t.Fatalf("Put must replace: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.dir, "tmp")); len(entries) != 0 {
		t.Fatalf("tmp/ holds %d leftover files", len(entries))
	}
}

func TestCompleteRecoversFromACrashBetweenRenameAndCleanup(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const key = "uploads/ast_1/source"
	mp, _ := h.fs.CreateMultipart(ctx, key, 16)
	e1 := h.putPart(key, mp.UploadID, 1, "AAAAAAAA").Header.Get("ETag")
	e2 := h.putPart(key, mp.UploadID, 2, "BBBBBBBB").Header.Get("ETag")
	// The process died right after moving data into place.
	if err := os.MkdirAll(filepath.Join(h.dir, "objects", "uploads", "ast_1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h.dir, "multipart", mp.UploadID, "data"), filepath.Join(h.dir, "objects", key)); err != nil {
		t.Fatal(err)
	}
	if err := h.fs.CompleteMultipart(ctx, key, mp.UploadID, []Part{{Number: 1, ETag: e1}, {Number: 2, ETag: e2}}); err != nil {
		t.Fatalf("retry after the crash: %v", err)
	}
	if got := h.read(key); got != "AAAAAAAABBBBBBBB" {
		t.Fatalf("object = %q", got)
	}
	if _, err := h.fs.ListParts(ctx, key, mp.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("staging must be cleaned up: %v", err)
	}
}
