// Package storage is the Storage adapter boundary (docs/architecture.md,
// "Adapters"). Core logic never branches on a vendor; v1 adapters are
// filesystem (P1-T04), R2 and S3 (Phase 2).
//
// Layout, owned by the core and never by an adapter:
//
//	uploads/{asset_id}/source
//	media/{asset_id}/{output_id}/...
//	_routes/{playback_id}.json
//	_keys.json
package storage

import (
	"context"
	"io"
	"time"
)

// Part is one stored part of a multipart upload.
type Part struct {
	Number int
	ETag   string
	Size   int64
}

// Object describes a stored object.
type Object struct {
	Key  string
	Size int64
	ETag string
}

// Caps reports what an adapter can reach, so the dashboard only offers valid
// adapter combinations (see "Compatibility").
type Caps struct {
	// Name identifies the adapter, e.g. "fs", "r2", "s3".
	Name string
	// PublicURLs is true when presigned URLs are reachable from outside the host.
	PublicURLs bool
	// WorkerBinding is true when a Cloudflare Worker can read it through a binding (R2 only).
	WorkerBinding bool
}

// Storage is the narrow interface every storage backend implements.
type Storage interface {
	CreateMultipart(ctx context.Context, key string) (uploadID string, err error)
	PresignPart(ctx context.Context, key, uploadID string, part int, ttl time.Duration) (url string, err error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (url string, err error)
	PresignPut(ctx context.Context, key string, ttl time.Duration) (url string, err error)
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Head(ctx context.Context, key string) (Object, error)
	List(ctx context.Context, prefix, cursor string) (objs []Object, next string, err error)
	DeletePrefix(ctx context.Context, prefix string) error
	Capabilities() Caps
}
