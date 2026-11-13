// Package storage is the Storage adapter boundary (docs/architecture.md,
// "Adapters"). Core logic calls Storage and never branches on a vendor. v1
// has the filesystem adapter; R2 and S3 arrive in Phase 2 behind the same
// interface. Keys are opaque to adapters, but the layout is owned by the core:
//
//	uploads/{asset_id}/source
//	media/{asset_id}/{output_id}/...
//	_routes/{playback_id}.json
//	_keys.json
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// PartSize is the multipart part size. R2 needs equal parts except the last,
// and 32 MiB × 10,000 parts allows sources up to about 312 GiB.
const PartSize = 32 << 20

// MaxParts is the S3/R2 limit on parts per multipart upload.
const MaxParts = 10_000

// Errors adapters return; callers test them with errors.Is.
var (
	ErrNotFound     = errors.New("storage: not found")
	ErrInvalidKey   = errors.New("storage: invalid key")
	ErrInvalidParts = errors.New("storage: invalid parts")
	ErrTooLarge     = errors.New("storage: too large")
	// ErrExists means a create-once write found the object already there.
	ErrExists = errors.New("storage: object already exists")
)

// Multipart is a started multipart upload.
type Multipart struct {
	UploadID string
	// PartSize is the size of every part except the last.
	PartSize int64
}

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

// PartLength is part n's length in an upload of size bytes split into
// partSize parts: partSize, except the last, which holds the rest. A part
// URL is signed for it (PresignPart).
func PartLength(size, partSize int64, n int) int64 {
	return min(partSize, size-int64(n-1)*partSize)
}

// Storage is the narrow interface every storage backend implements.
type Storage interface {
	// CreateMultipart starts an upload of exactly size bytes to key.
	CreateMultipart(ctx context.Context, key string, size int64) (Multipart, error)
	// PresignPart returns a URL a browser can PUT one part of exactly size
	// bytes to; a body of any other length is refused (D122).
	PresignPart(ctx context.Context, key, uploadID string, part int, size int64, ttl time.Duration) (string, error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignPut returns a create-once URL: it can write the object if it
	// doesn't exist and can never overwrite it (412), so a worker can't
	// change an output after commit while its URLs are still valid. On R2
	// and S3 this is a signed If-None-Match: *.
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)
	Put(ctx context.Context, key string, body io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Head(ctx context.Context, key string) (Object, error)
	// List returns objects whose keys start with prefix, in key order, a page
	// at a time. next is empty on the last page.
	List(ctx context.Context, prefix, cursor string) (objs []Object, next string, err error)
	// Delete deletes one object; a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// DeletePrefix deletes every object under prefix, which must end in "/".
	DeletePrefix(ctx context.Context, prefix string) error
}

// maxKeyLen bounds keys well under every backend's limit.
const maxKeyLen = 512

// ValidKey reports whether key is a safe object key: segments of
// [A-Za-z0-9._-] separated by single slashes, with no "." or ".." segment.
// It is the second line of defence behind the output path grammar.
func ValidKey(key string) bool {
	if key == "" || len(key) > maxKeyLen {
		return false
	}
	for seg := range strings.SplitSeq(key, "/") {
		if !validSegment(seg) {
			return false
		}
	}
	return true
}

// validPrefix reports whether prefix is "" or a run of valid segments ending
// in "/".
func validPrefix(prefix string) bool {
	if prefix == "" {
		return true
	}
	dir, ok := strings.CutSuffix(prefix, "/")
	return ok && ValidKey(dir)
}

func validSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	for _, c := range seg {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}
