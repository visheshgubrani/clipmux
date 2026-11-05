package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

// The contract every adapter keeps (docs/architecture.md, "Adapters"): one
// suite, run against the filesystem always and against an S3-compatible
// store when CLIPMUX_TEST_S3_ENDPOINT is set (make s3-up starts one).

type contractStore struct {
	Storage
	partSize int64
}

func filesystemStore(t *testing.T) contractStore {
	h := newHarness(t)
	return contractStore{h.fs, testPartSize}
}

// s3Store returns an S3 adapter on a fresh bucket of the test store, or
// skips (see s3test).
func s3Store(t *testing.T) contractStore {
	t.Helper()
	b := s3test.New(t)
	store, err := OpenS3(S3Options{Endpoint: b.Endpoint, Bucket: b.Name, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	return contractStore{store, PartSize}
}

func eachStore(t *testing.T, test func(t *testing.T, s contractStore)) {
	t.Run("filesystem", func(t *testing.T) { test(t, filesystemStore(t)) })
	t.Run("s3", func(t *testing.T) { test(t, s3Store(t)) })
}

func send(t *testing.T, method, url string, body []byte, header map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

func readAll(t *testing.T, s Storage, key string) string {
	t.Helper()
	rc, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContractPutGetHeadAndMissingKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		if err := s.Put(ctx, "media/a/o/manifest.json", strings.NewReader(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, s, "media/a/o/manifest.json"); got != `{"v":1}` {
			t.Fatalf("Get = %q", got)
		}
		obj, err := s.Head(ctx, "media/a/o/manifest.json")
		if err != nil || obj.Size != 7 || obj.ETag == "" {
			t.Fatalf("Head = %+v, %v; want 7 bytes with an ETag", obj, err)
		}
		// Put replaces: route records are rewritten in place.
		if err := s.Put(ctx, "media/a/o/manifest.json", strings.NewReader(`{}`)); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, s, "media/a/o/manifest.json"); got != `{}` {
			t.Fatalf("Get after replace = %q", got)
		}
		if _, err := s.Head(ctx, "media/a/o/none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Head(missing) = %v, want ErrNotFound", err)
		}
		if _, err := s.Get(ctx, "media/a/o/none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
		}
		for _, k := range []string{"", "../x", "a//b", "a/./b", "a b"} {
			if err := s.Put(ctx, k, strings.NewReader("x")); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("Put(%q) = %v, want ErrInvalidKey", k, err)
			}
		}
	})
}

func TestContractListIsOrderedPrefixedAndResumable(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		for _, k := range []string{"_routes/b.json", "_routes/a.json", "_routes/c.json", "_routesx/d.json", "media/x/1"} {
			if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
				t.Fatal(err)
			}
		}
		objs, next, err := s.List(ctx, "_routes/", "")
		if err != nil || next != "" {
			t.Fatalf("List = %v, next %q", err, next)
		}
		var keys []string
		for _, o := range objs {
			keys = append(keys, o.Key)
		}
		if want := []string{"_routes/a.json", "_routes/b.json", "_routes/c.json"}; !slices.Equal(keys, want) {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
		objs, _, err = s.List(ctx, "_routes/", "_routes/a.json")
		if err != nil || len(objs) != 2 || objs[0].Key != "_routes/b.json" {
			t.Fatalf("List after cursor = %v, %v; want b and c", objs, err)
		}
		if _, _, err := s.List(ctx, "_routes", ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("List(no slash) = %v, want ErrInvalidKey", err)
		}
	})
}

// Unpublishing a route deletes exactly _routes/{pid}.json.
func TestContractDeleteRemovesOneObject(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		for _, k := range []string{"_routes/pid_1.json", "_routes/pid_10.json"} {
			if err := s.Put(ctx, k, strings.NewReader("{}")); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Delete(ctx, "_routes/pid_1.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Head(ctx, "_routes/pid_1.json"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Head after Delete = %v, want ErrNotFound", err)
		}
		if _, err := s.Head(ctx, "_routes/pid_10.json"); err != nil {
			t.Fatalf("Delete removed a neighbour: %v", err)
		}
		if err := s.Delete(ctx, "_routes/pid_1.json"); err != nil {
			t.Fatalf("Delete(missing) = %v, want nil", err)
		}
		if err := s.Delete(ctx, "../x"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Delete(../x) = %v, want ErrInvalidKey", err)
		}
	})
}

func TestContractDeletePrefixRemovesOnlyThatPrefix(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		for _, k := range []string{"media/a/1", "media/a/o/2", "media/ab/3"} {
			if err := s.Put(ctx, k, strings.NewReader("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.DeletePrefix(ctx, "media/a/"); err != nil {
			t.Fatal(err)
		}
		objs, _, err := s.List(ctx, "media/", "")
		if err != nil || len(objs) != 1 || objs[0].Key != "media/ab/3" {
			t.Fatalf("left = %v, %v; want only media/ab/3", objs, err)
		}
		if err := s.DeletePrefix(ctx, "media/none/"); err != nil {
			t.Fatalf("DeletePrefix(empty) = %v, want nil", err)
		}
		if err := s.DeletePrefix(ctx, "media/ab"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("DeletePrefix(no slash) = %v, want ErrInvalidKey", err)
		}
	})
}

// partBody returns part n's bytes for an upload of size bytes.
func partBody(src []byte, partSize int64, n int) []byte {
	start := int64(n-1) * partSize
	return src[start:min(start+partSize, int64(len(src)))]
}

func TestContractMultipartResumesAndCompletes(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		key := "uploads/a/source"
		src := make([]byte, s.partSize*2+5)
		_, _ = rand.Read(src)
		mp, err := s.CreateMultipart(ctx, key, int64(len(src)))
		if err != nil || mp.PartSize != s.partSize {
			t.Fatalf("CreateMultipart = %+v, %v; want part size %d", mp, err, s.partSize)
		}
		put := func(n int) {
			u, err := s.PresignPart(ctx, key, mp.UploadID, n, 15*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if code := send(t, http.MethodPut, u, partBody(src, s.partSize, n), nil); code != http.StatusOK {
				t.Fatalf("PUT part %d: HTTP %d", n, code)
			}
		}
		put(1)
		put(3)
		// A browser resumes from what storage says landed.
		parts, err := s.ListParts(ctx, key, mp.UploadID)
		if err != nil || len(parts) != 2 || parts[0].Number != 1 || parts[1].Number != 3 || parts[0].Size != s.partSize {
			t.Fatalf("ListParts = %+v, %v; want parts 1 and 3", parts, err)
		}
		if err := s.CompleteMultipart(ctx, key, mp.UploadID, parts); !errors.Is(err, ErrInvalidParts) {
			t.Fatalf("Complete with a gap = %v, want ErrInvalidParts", err)
		}
		put(2)
		parts, err = s.ListParts(ctx, key, mp.UploadID)
		if err != nil || len(parts) != 3 {
			t.Fatalf("ListParts = %+v, %v", parts, err)
		}
		bad := slices.Clone(parts)
		bad[1].ETag = `"0000"`
		if err := s.CompleteMultipart(ctx, key, mp.UploadID, bad); !errors.Is(err, ErrInvalidParts) {
			t.Fatalf("Complete with a wrong ETag = %v, want ErrInvalidParts", err)
		}
		// ETags arrive quoted or not, depending on the browser's client.
		for i := range parts {
			parts[i].ETag = strings.Trim(parts[i].ETag, `"`)
		}
		if err := s.CompleteMultipart(ctx, key, mp.UploadID, parts); err != nil {
			t.Fatal(err)
		}
		if got := readAll(t, s, key); got != string(src) {
			t.Fatalf("completed object differs (%d bytes, want %d)", len(got), len(src))
		}
		// The upload is gone; a retried complete finds nothing (callers
		// recover from the object, D011).
		if _, err := s.ListParts(ctx, key, mp.UploadID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ListParts after complete = %v, want ErrNotFound", err)
		}
	})
}

func TestContractAbortDiscardsTheUpload(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		mp, err := s.CreateMultipart(ctx, "uploads/b/source", 10)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AbortMultipart(ctx, "uploads/b/source", mp.UploadID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListParts(ctx, "uploads/b/source", mp.UploadID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ListParts after abort = %v, want ErrNotFound", err)
		}
		if _, err := s.Head(ctx, "uploads/b/source"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Head after abort = %v, want ErrNotFound", err)
		}
	})
}

func TestContractDeclaredSizeIsBoundedByThePartLimit(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		if _, err := s.CreateMultipart(context.Background(), "uploads/c/source", s.partSize*MaxParts+1); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("CreateMultipart(too large) = %v, want ErrTooLarge", err)
		}
	})
}

// D016: a worker's PUT URL creates once and never overwrites, so an output
// can't change after commit while the URL is still valid. Workers send
// If-None-Match: *; on S3 it is a signed header.
func TestContractPresignedPutsCreateOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		key := "media/a/o/720p/seg_00001.m4s"
		u, err := s.PresignPut(ctx, key, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		once := map[string]string{"If-None-Match": "*"}
		if code := send(t, http.MethodPut, u, []byte("first"), once); code != http.StatusOK {
			t.Fatalf("first PUT: HTTP %d", code)
		}
		if code := send(t, http.MethodPut, u, []byte("second"), once); code != http.StatusPreconditionFailed {
			t.Fatalf("second PUT: HTTP %d, want 412", code)
		}
		if got := readAll(t, s, key); got != "first" {
			t.Fatalf("object = %q, want the first write", got)
		}
	})
}

func TestContractPresignedGetIsTheOnlyAuthorisation(t *testing.T) {
	eachStore(t, func(t *testing.T, s contractStore) {
		ctx := context.Background()
		if err := s.Put(ctx, "uploads/d/source", strings.NewReader("video")); err != nil {
			t.Fatal(err)
		}
		u, err := s.PresignGet(ctx, "uploads/d/source", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if code := send(t, http.MethodGet, u, nil, nil); code != http.StatusOK {
			t.Fatalf("signed GET: HTTP %d", code)
		}
		unsigned, _, _ := strings.Cut(u, "?")
		if code := send(t, http.MethodGet, unsigned, nil, nil); code != http.StatusForbidden && code != http.StatusUnauthorized {
			t.Fatalf("unsigned GET: HTTP %d, want 403", code)
		}
		other, err := s.PresignGet(ctx, "uploads/e/source", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, otherQuery, _ := strings.Cut(other, "?")
		if code := send(t, http.MethodGet, unsigned+"?"+otherQuery, nil, nil); code == http.StatusOK {
			t.Fatal("a signature for one key opened another")
		}
	})
}

// Browsers PUT parts straight to the bucket and read the ETag back.
func TestS3BucketCORSAllowsBrowserPartUploads(t *testing.T) {
	s := s3Store(t)
	store := s.Storage.(*S3)
	ctx := context.Background()
	if err := store.ApplyCORS(ctx); err != nil {
		t.Fatal(err)
	}
	mp, err := store.CreateMultipart(ctx, "uploads/f/source", 10)
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.PresignPart(ctx, "uploads/f/source", mp.UploadID, 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodOptions, u, nil)
	req.Header.Set("Origin", "https://lms.example")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("preflight: HTTP %d, allow-origin %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	put, _ := http.NewRequest(http.MethodPut, u, strings.NewReader("0123456789"))
	put.Header.Set("Origin", "https://lms.example")
	res, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.Contains(strings.ToLower(res.Header.Get("Access-Control-Expose-Headers")), "etag") {
		t.Fatalf("PUT exposes %q, want ETag", res.Header.Get("Access-Control-Expose-Headers"))
	}
}
