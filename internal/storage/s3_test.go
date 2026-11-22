package storage

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

// R2 and S3 refuse a body whose length differs from a signed
// Content-Length, so the part URL must sign it.
func TestS3PartURLSignsContentLength(t *testing.T) {
	store, err := OpenS3(S3Options{Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.PresignPart(context.Background(), "uploads/a/source", "upload-1", 2, PartSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if signed := u.Query().Get("X-Amz-SignedHeaders"); !strings.Contains(signed, "content-length") {
		t.Fatalf("X-Amz-SignedHeaders = %q, want content-length in it", signed)
	}
}

func TestS3GetRangeReadsInclusiveByteSlice(t *testing.T) {
	b := s3test.New(t)
	store, err := OpenS3(S3Options{Endpoint: b.Endpoint, Bucket: b.Name, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const key = "media/ast_1/out_1/360p/seg_00001.m4s"
	if err := store.Put(ctx, key, strings.NewReader("segment-one")); err != nil {
		t.Fatal(err)
	}
	rc, length, err := store.GetRange(ctx, key, 2, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if length != 5 || string(got) != "gment" {
		t.Fatalf("GetRange(2, 6) = %d bytes %q, want 5 bytes %q", length, string(got), "gment")
	}
}
