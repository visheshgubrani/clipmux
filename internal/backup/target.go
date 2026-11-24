package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/storage"
)

// spec is a parsed target URL.
type spec struct {
	dir                    string // a directory target
	bucket, prefix         string // an S3 target; prefix is "" or ends in "/"
	endpoint, region, desc string
}

func parseTarget(raw string) (spec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return spec{}, errors.New("CLIPMUX_BACKUP_URL is not a URL")
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" {
			return spec{}, errors.New("CLIPMUX_BACKUP_URL: a file target is file:///absolute/path")
		}
		return spec{dir: u.Path, desc: "file://" + u.Path}, nil
	case "s3":
		s := spec{bucket: u.Host, region: u.Query().Get("region"), endpoint: u.Query().Get("endpoint")}
		if s.bucket == "" {
			return spec{}, errors.New("CLIPMUX_BACKUP_URL: an s3 target is s3://bucket/prefix?endpoint=https://…&region=…")
		}
		if p := strings.Trim(u.Path, "/"); p != "" {
			if !storage.ValidKey(p) {
				return spec{}, fmt.Errorf("CLIPMUX_BACKUP_URL: the prefix %q may hold only letters, digits and . _ - between slashes", p)
			}
			s.prefix = p + "/"
		}
		if s.region == "" {
			s.region = "us-east-1"
		}
		if s.endpoint == "" {
			s.endpoint = "https://s3." + s.region + ".amazonaws.com"
		}
		if e, err := url.Parse(s.endpoint); err != nil || (e.Scheme != "https" && e.Scheme != "http") || e.Host == "" {
			return spec{}, errors.New("CLIPMUX_BACKUP_URL: endpoint must be an http or https URL")
		}
		s.desc = "s3://" + s.bucket + "/" + s.prefix
		return s, nil
	default:
		return spec{}, errors.New("CLIPMUX_BACKUP_URL must start with s3:// or file:///")
	}
}

// Target is where backups are kept: a directory or a prefix of an
// S3-compatible bucket, reached through the storage adapters. Keys passed to
// it are relative to the target's prefix.
type Target struct {
	store  storage.Storage
	prefix string
	closer io.Closer
	// Desc names the target without its credentials: s3://bucket/prefix/ or
	// file:///path.
	Desc string
}

// OpenTarget opens the target of cfg. It makes no request; Probe does.
func OpenTarget(cfg Config) (*Target, error) {
	s, err := parseTarget(cfg.URL)
	if err != nil {
		return nil, err
	}
	if s.dir != "" {
		fsStore, err := storage.OpenFilesystem(s.dir, storage.FilesystemOptions{BaseURL: "http://localhost"})
		if err != nil {
			return nil, fmt.Errorf("open the backup directory: %w", err)
		}
		return &Target{store: fsStore, closer: fsStore, Desc: s.desc}, nil
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, errors.New("an s3 backup target needs CLIPMUX_BACKUP_ACCESS_KEY_ID and CLIPMUX_BACKUP_SECRET_ACCESS_KEY")
	}
	s3, err := storage.OpenS3(storage.S3Options{
		Endpoint: s.endpoint, Region: s.region, Bucket: s.bucket,
		AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey,
		ObjectLockChecksums: true,
	})
	if err != nil {
		return nil, err
	}
	return &Target{store: s3, prefix: s.prefix, Desc: s.desc}, nil
}

// Close releases the target.
func (t *Target) Close() error {
	if t.closer == nil {
		return nil
	}
	return t.closer.Close()
}

// Put streams body to key.
func (t *Target) Put(ctx context.Context, key string, body io.Reader) error {
	return t.store.Put(ctx, t.prefix+key, body)
}

// Get opens key for reading.
func (t *Target) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return t.store.Get(ctx, t.prefix+key)
}

// Delete deletes key, which only a credential allowed to delete can do.
func (t *Target) Delete(ctx context.Context, key string) error {
	return t.store.Delete(ctx, t.prefix+key)
}

// list returns every object under dir ("db/hourly/"), with keys relative to
// the target.
func (t *Target) list(ctx context.Context, dir string) ([]storage.Object, error) {
	var out []storage.Object
	for cursor := ""; ; {
		objs, next, err := t.store.List(ctx, t.prefix+dir, cursor)
		if err != nil {
			return nil, fmt.Errorf("list %s%s: %w", t.Desc, dir, err)
		}
		for _, o := range objs {
			o.Key = strings.TrimPrefix(o.Key, t.prefix)
			out = append(out, o)
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// Probe is what a target lets this server do.
type Probe struct {
	// CanDelete reports that a delete of the probe's object was accepted.
	// On some stores that isn't a delete: on B2, through the S3 API, a key
	// without deleteFiles may still hide a file, and on a bucket with
	// versioning a delete only adds a marker. So CanDelete alone doesn't
	// say the backups are safe or that ClipMux should prune; Prunes does.
	CanDelete bool
	// Locked reports that the bucket has Object Lock on with a default
	// retention: no key can remove a backup before it expires, which is what
	// a ransomware-proof setup wants (D146).
	Locked bool
	// LockErr is why the lock couldn't be read, when it couldn't (the key
	// may lack the permission); Locked is then false.
	LockErr error
}

// Prunes reports whether ClipMux prunes old backups itself: the key may
// delete and the bucket isn't locked. Otherwise the bucket's lifecycle rules
// expire them, and the target is immutable to this server.
func (p Probe) Prunes() bool { return p.CanDelete && !p.Locked }

// Probe writes, reads, lists and tries to delete a small object, so a bad
// target or key fails here and not in the middle of the first dump. A delete
// that is refused isn't a failure: it means the target is immutable. On an
// S3 target it also reads the bucket's Object Lock.
func (t *Target) Probe(ctx context.Context) (Probe, error) {
	key := "_probe/" + strings.ToLower(rand.Text())
	body := []byte("clipmux backup probe")
	if err := t.Put(ctx, key, bytes.NewReader(body)); err != nil {
		return Probe{}, fmt.Errorf("write to the backup target %s: %w", t.Desc, err)
	}
	rc, err := t.Get(ctx, key)
	if err != nil {
		return Probe{}, fmt.Errorf("read from the backup target %s: %w", t.Desc, err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		return Probe{}, fmt.Errorf("read back from the backup target %s: %v", t.Desc, err)
	}
	objs, err := t.list(ctx, "_probe/")
	if err != nil {
		return Probe{}, err
	}
	found := false
	for _, o := range objs {
		found = found || o.Key == key
	}
	if !found {
		return Probe{}, fmt.Errorf("list the backup target %s: the object just written isn't listed", t.Desc)
	}
	p := Probe{CanDelete: t.Delete(ctx, key) == nil}
	if l, ok := t.store.(interface {
		ObjectLock(context.Context) (bool, error)
	}); ok {
		p.Locked, p.LockErr = l.ObjectLock(ctx)
	}
	return p, nil
}
