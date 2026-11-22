package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Options configure an S3-compatible store (R2, AWS S3, Garage, SeaweedFS).
type S3Options struct {
	// Endpoint is where serve sends its own requests, such as
	// https://{account}.r2.cloudflarestorage.com or http://seaweedfs:8333.
	Endpoint string
	// PublicEndpoint is what presigned URLs name, because browsers and
	// workers must reach it (docs/architecture.md, "Compatibility"). It
	// defaults to Endpoint.
	PublicEndpoint  string
	Region          string // "auto" on R2; defaults to us-east-1
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	// HTTPClient overrides the bounded default client (tests).
	HTTPClient *http.Client
}

// S3 is the Storage adapter for S3-compatible object stores. Parts are always
// PartSize: R2 requires equal parts except the last, and the declared size
// is enforced by the upload API with Head after completion.
type S3 struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
	uploads *transfermanager.Client
}

// listPageS3 is the page size of List; S3 caps it at 1000.
const listPageS3 = 1000

// OpenS3 returns an adapter for one bucket. It makes no request; Check does.
func OpenS3(opts S3Options) (*S3, error) {
	if opts.Endpoint == "" || opts.Bucket == "" || opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
		return nil, errors.New("s3 storage: endpoint, bucket and credentials are required")
	}
	if opts.PublicEndpoint == "" {
		opts.PublicEndpoint = opts.Endpoint
	}
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = boundedClient()
	}
	cfg := aws.Config{
		Region:      opts.Region,
		Credentials: credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		HTTPClient:  httpClient,
		// R2 and most S3-compatible stores reject the SDK's default
		// CRC checksums on some calls; send them only where S3 requires.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	}
	clientFor := func(endpoint string) *s3.Client {
		return s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	client := clientFor(opts.Endpoint)
	return &S3{
		bucket:  opts.Bucket,
		client:  client,
		presign: s3.NewPresignClient(clientFor(opts.PublicEndpoint)),
		uploads: transfermanager.New(client, func(o *transfermanager.Options) {
			o.PartSizeBytes = PartSize
			o.MultipartUploadThreshold = PartSize
			o.Concurrency = 4
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}),
	}, nil
}

// boundedClient bounds connecting and waiting for headers, so a stalled
// store can't hold a request forever; bodies are bounded by the caller's
// context (D016's idle watchdog applies to workers, not here).
func boundedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: t}
}

// Bucket names the adapter's bucket.
func (s *S3) Bucket() string { return s.bucket }

// Check proves the credentials can list, write, read and delete in the
// bucket, so a bad config fails when it is saved rather than mid-job.
func (s *S3) Check(ctx context.Context) error {
	key := "_check/" + fmt.Sprint(time.Now().UnixNano())
	if err := s.Put(ctx, key, strings.NewReader("ok")); err != nil {
		return err
	}
	if _, err := s.Head(ctx, key); err != nil {
		return err
	}
	if _, _, err := s.List(ctx, "_check/", ""); err != nil {
		return err
	}
	return s.DeletePrefix(ctx, "_check/")
}

// ApplyCORS lets browsers PUT parts and read ETag from any origin. The
// presigned URL is the authorisation (D011), so the origin isn't checked.
func (s *S3) ApplyCORS(ctx context.Context) error {
	_, err := s.client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: &s.bucket,
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"GET", "HEAD", "PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(3600),
		}}},
	})
	return s.wrap("put bucket cors", s.bucket, err)
}

// Put streams body into key, in PartSize parts when it is large, so a move
// or an import of a source of any size needs no spooling.
func (s *S3) Put(ctx context.Context, key string, body io.Reader) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	_, err := s.uploads.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: &s.bucket, Key: &key, Body: body})
	return s.wrap("put", key, err)
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, s.wrap("get", key, err)
	}
	return out.Body, nil
}

// GetRange reads the inclusive byte range [start, end] of key and returns its stream and length.
func (s *S3) GetRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error) {
	if !ValidKey(key) {
		return nil, 0, ErrInvalidKey
	}
	if start < 0 || end < start {
		return nil, 0, fmt.Errorf("get range %s: invalid bounds %d-%d", key, start, end)
	}
	rng := fmt.Sprintf("bytes=%d-%d", start, end)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, Range: &rng})
	if err != nil {
		return nil, 0, s.wrap("get range", key, err)
	}
	return out.Body, aws.ToInt64(out.ContentLength), nil
}

func (s *S3) Head(ctx context.Context, key string) (Object, error) {
	if !ValidKey(key) {
		return Object{}, ErrInvalidKey
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return Object{}, s.wrap("head", key, err)
	}
	return Object{Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag)}, nil
}

func (s *S3) List(ctx context.Context, prefix, cursor string) ([]Object, string, error) {
	if !validPrefix(prefix) {
		return nil, "", ErrInvalidKey
	}
	in := &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix, MaxKeys: aws.Int32(listPageS3)}
	if cursor != "" {
		// The cursor is the last key returned, as on the filesystem, so a
		// caller can resume a listing across adapters and restarts.
		in.StartAfter = &cursor
	}
	out, err := s.client.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, "", s.wrap("list", prefix, err)
	}
	objs := make([]Object, 0, len(out.Contents))
	for _, o := range out.Contents {
		objs = append(objs, Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ETag: aws.ToString(o.ETag)})
	}
	if aws.ToBool(out.IsTruncated) && len(objs) > 0 {
		return objs, objs[len(objs)-1].Key, nil
	}
	return objs, "", nil
}

// Delete deletes one object; S3 answers 204 for a missing one too.
func (s *S3) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	if errors.Is(s.wrap("delete", key, err), ErrNotFound) {
		return nil
	}
	return s.wrap("delete", key, err)
}

func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	if prefix == "" || !validPrefix(prefix) {
		return ErrInvalidKey
	}
	for {
		// Always list from the start: the page just deleted is gone.
		objs, next, err := s.List(ctx, prefix, "")
		if err != nil {
			return err
		}
		if len(objs) == 0 {
			return nil
		}
		ids := make([]types.ObjectIdentifier, len(objs))
		for i, o := range objs {
			ids[i] = types.ObjectIdentifier{Key: aws.String(o.Key)}
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket,
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return s.wrap("delete", prefix, err)
		}
		// DeleteObjects answers 200 even when single keys fail.
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete %s: %d keys failed, first %s: %s", prefix, len(out.Errors), aws.ToString(e.Key), aws.ToString(e.Code))
		}
		if next == "" {
			return nil
		}
	}
}

// --- multipart ---

func (s *S3) CreateMultipart(ctx context.Context, key string, size int64) (Multipart, error) {
	if !ValidKey(key) {
		return Multipart{}, ErrInvalidKey
	}
	if size <= 0 {
		return Multipart{}, fmt.Errorf("%w: size must be positive", ErrInvalidParts)
	}
	if size > PartSize*MaxParts {
		return Multipart{}, ErrTooLarge
	}
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return Multipart{}, s.wrap("create multipart", key, err)
	}
	return Multipart{UploadID: aws.ToString(out.UploadId), PartSize: PartSize}, nil
}

// PresignPart signs ContentLength into the URL (X-Amz-SignedHeaders), so
// the store refuses a body of any other length. S3 doesn't keep the
// declared size, so the caller computes the part's length.
func (s *S3) PresignPart(ctx context.Context, key, uploadID string, part int, size int64, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	if part < 1 || part > MaxParts || size <= 0 || size > PartSize {
		return "", ErrInvalidParts
	}
	req, err := s.presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID, PartNumber: aws.Int32(int32(part)), ContentLength: aws.Int64(size),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign part %d: %w", part, err)
	}
	return req.URL, nil
}

func (s *S3) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	if !ValidKey(key) {
		return nil, ErrInvalidKey
	}
	var parts []Part
	in := &s3.ListPartsInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID}
	for {
		out, err := s.client.ListParts(ctx, in)
		if err != nil {
			return nil, s.wrap("list parts", key, err)
		}
		for _, p := range out.Parts {
			parts = append(parts, Part{Number: int(aws.ToInt32(p.PartNumber)), ETag: aws.ToString(p.ETag), Size: aws.ToInt64(p.Size)})
		}
		if !aws.ToBool(out.IsTruncated) {
			return parts, nil
		}
		in.PartNumberMarker = out.NextPartNumberMarker
	}
}

// CompleteMultipart checks the parts against what storage holds, with the
// filesystem adapter's rules: numbered 1..n, ETags matching, every part but
// the last exactly PartSize.
func (s *S3) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	stored, err := s.ListParts(ctx, key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) == 0 || len(stored) != len(parts) {
		return fmt.Errorf("%w: want %d parts", ErrInvalidParts, len(stored))
	}
	completed := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		sp := stored[i]
		if p.Number != i+1 || sp.Number != i+1 || strings.Trim(p.ETag, `"`) != strings.Trim(sp.ETag, `"`) {
			return fmt.Errorf("%w: part %d", ErrInvalidParts, i+1)
		}
		if i < len(parts)-1 && sp.Size != PartSize {
			return fmt.Errorf("%w: part %d is %d bytes, want %d", ErrInvalidParts, i+1, sp.Size, PartSize)
		}
		completed[i] = types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: aws.String(sp.ETag)}
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &s.bucket, Key: &key, UploadId: &uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	return s.wrap("complete", key, err)
}

func (s *S3) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &key, UploadId: &uploadID})
	return s.wrap("abort", key, err)
}

// --- signed URLs ---

func (s *S3) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

// PresignPut signs a create-once PUT: If-None-Match: * is a signed header,
// so the uploader must send it and the store refuses an overwrite with 412.
func (s *S3) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	req, err := s.presign.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, IfNoneMatch: aws.String("*")}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign put: %w", err)
	}
	return req.URL, nil
}

// wrap maps store errors onto the package's errors and names the operation.
// SDK errors name the operation and status, never a presigned URL.
func (s *S3) wrap(op, key string, err error) error {
	if err == nil {
		return nil
	}
	var nsk *types.NoSuchKey
	var nsu *types.NoSuchUpload
	var nf *types.NotFound
	var api smithy.APIError
	switch {
	case errors.As(err, &nsk), errors.As(err, &nsu), errors.As(err, &nf):
		return fmt.Errorf("%s %s: %w", op, key, ErrNotFound)
	case errors.As(err, &api):
		switch api.ErrorCode() {
		case "NoSuchKey", "NoSuchUpload", "NotFound":
			return fmt.Errorf("%s %s: %w", op, key, ErrNotFound)
		case "InvalidPart", "InvalidPartOrder", "EntityTooSmall":
			return fmt.Errorf("%s %s: %w: %s", op, key, ErrInvalidParts, api.ErrorCode())
		case "PreconditionFailed":
			return fmt.Errorf("%s %s: %w", op, key, ErrExists)
		}
	}
	return fmt.Errorf("%s %s: %w", op, key, err)
}

var _ Storage = (*S3)(nil)
