package storage

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
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
