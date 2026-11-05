// Package s3test gives each test its own bucket on the S3-compatible test
// store (SeaweedFS in deploy/compose.dev.yml), the way dbtest gives each a
// schema. It skips without CLIPMUX_TEST_S3_ENDPOINT, unless CI requires it.
package s3test

import (
	"context"
	"crypto/rand"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Bucket is a fresh, empty bucket and the credentials that reach it.
type Bucket struct {
	Endpoint, Name, AccessKeyID, SecretAccessKey string
}

// New creates a bucket that is emptied and deleted when the test ends.
func New(t testing.TB) Bucket {
	t.Helper()
	b := Bucket{
		Endpoint:        os.Getenv("CLIPMUX_TEST_S3_ENDPOINT"),
		Name:            "cmx-" + strings.ToLower(rand.Text()[:12]),
		AccessKeyID:     os.Getenv("CLIPMUX_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("CLIPMUX_TEST_S3_SECRET_KEY"),
	}
	if b.Endpoint == "" {
		if os.Getenv("CLIPMUX_REQUIRE_S3") == "1" {
			t.Fatal("CLIPMUX_TEST_S3_ENDPOINT is required in CI")
		}
		t.Skip("CLIPMUX_TEST_S3_ENDPOINT not set")
	}
	c := s3.New(s3.Options{
		BaseEndpoint: aws.String(b.Endpoint), UsePathStyle: true, Region: "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(b.AccessKeyID, b.SecretAccessKey, ""),
	})
	ctx := context.Background()
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &b.Name}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		for {
			out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &b.Name})
			if err != nil || len(out.Contents) == 0 {
				break
			}
			ids := make([]types.ObjectIdentifier, len(out.Contents))
			for i, o := range out.Contents {
				ids[i] = types.ObjectIdentifier{Key: o.Key}
			}
			if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &b.Name, Delete: &types.Delete{Objects: ids}}); err != nil {
				break
			}
		}
		_, _ = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &b.Name})
	})
	return b
}
