package storage

import (
	"context"
	"crypto/hmac"
	"crypto/md5" // S3-compatible part ETags are MD5; not a security use
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Filesystem is the single-node storage adapter. Every file operation goes
// through an os.Root, so neither a crafted key nor a planted symlink can
// reach outside the storage directory. Browsers and workers reach it through
// URLs the app signs and serves itself (Handler).
//
// On disk:
//
//	signing.key                  HMAC key for signed URLs (survives restarts)
//	objects/{key}                stored objects
//	multipart/{id}/meta.json     an upload in progress: key, size, part size
//	multipart/{id}/data          preallocated at the declared size
//	multipart/{id}/parts/{n}     written after part n lands: ETag and size
//	tmp/                         staging for atomic writes
type Filesystem struct {
	root          *os.Root
	key           []byte
	baseURL       string
	partSize      int64
	maxObjectSize int64
	now           func() time.Time
}

var _ Storage = (*Filesystem)(nil)

// FilesystemOptions configures OpenFilesystem.
type FilesystemOptions struct {
	// BaseURL is the app's public URL; signed URLs point at BaseURL/_storage/.
	BaseURL string
	// PartSize defaults to PartSize.
	PartSize int64
	// MaxObjectSize bounds a single Put or presigned PUT; defaults to 5 GiB,
	// the S3 single-PUT limit.
	MaxObjectSize int64
	// Now defaults to time.Now.
	Now func() time.Time
}

const (
	pathPrefix  = "/_storage/"
	listPage    = 1000
	signVersion = "clipmux-storage-v1"
)

var errShortPart = errors.New("storage: part shorter than its range")

// OpenFilesystem opens (creating if needed) a storage directory.
func OpenFilesystem(dir string, opts FilesystemOptions) (*Filesystem, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	f := &Filesystem{
		root:          root,
		baseURL:       strings.TrimSuffix(opts.BaseURL, "/"),
		partSize:      orDefault(opts.PartSize, PartSize),
		maxObjectSize: orDefault(opts.MaxObjectSize, 5<<30),
		now:           opts.Now,
	}
	if f.now == nil {
		f.now = time.Now
	}
	for _, d := range []string{"objects", "multipart", "tmp"} {
		if err := root.MkdirAll(d, 0o750); err != nil {
			root.Close()
			return nil, fmt.Errorf("open storage: %w", err)
		}
	}
	if f.key, err = loadSigningKey(root); err != nil {
		root.Close()
		return nil, err
	}
	return f, nil
}

func orDefault(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}

// loadSigningKey reads signing.key, creating it on first open.
func loadSigningKey(root *os.Root) ([]byte, error) {
	const name = "signing.key"
	b, err := root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		key := make([]byte, 32)
		rand.Read(key)
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return loadSigningKey(root) // another process created it first
		}
		if err != nil {
			return nil, fmt.Errorf("create signing key: %w", err)
		}
		_, werr := file.WriteString(hex.EncodeToString(key))
		if cerr := file.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, fmt.Errorf("write signing key: %w", werr)
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 32 {
		return nil, errors.New("storage: signing.key is corrupt")
	}
	return key, nil
}

// Close releases the storage directory.
func (f *Filesystem) Close() error { return f.root.Close() }

func objectPath(key string) string { return "objects/" + key }

// --- objects ---

// Put writes an object atomically.
func (f *Filesystem) Put(_ context.Context, key string, body io.Reader) error {
	if !ValidKey(key) {
		return ErrInvalidKey
	}
	tmp := "tmp/" + rand.Text()
	file, err := f.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	n, err := io.Copy(file, io.LimitReader(body, f.maxObjectSize+1))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > f.maxObjectSize {
		err = ErrTooLarge
	}
	if err == nil {
		err = f.root.MkdirAll(path.Dir(objectPath(key)), 0o750)
	}
	if err == nil {
		err = f.root.Rename(tmp, objectPath(key))
	}
	if err != nil {
		_ = f.root.Remove(tmp)
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// Get opens an object for reading.
func (f *Filesystem) Get(_ context.Context, key string) (io.ReadCloser, error) {
	file, _, err := f.open(key)
	return file, err
}

func (f *Filesystem) open(key string) (*os.File, fs.FileInfo, error) {
	if !ValidKey(key) {
		return nil, nil, ErrInvalidKey
	}
	file, err := f.root.Open(objectPath(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", key, err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, ErrNotFound
	}
	return file, info, nil
}

// Head describes an object.
func (f *Filesystem) Head(_ context.Context, key string) (Object, error) {
	if !ValidKey(key) {
		return Object{}, ErrInvalidKey
	}
	info, err := f.root.Stat(objectPath(key))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, fmt.Errorf("head %s: %w", key, err)
	}
	return objectOf(key, info), nil
}

func objectOf(key string, info fs.FileInfo) Object {
	return Object{Key: key, Size: info.Size(), ETag: fmt.Sprintf(`"%x-%x"`, info.Size(), info.ModTime().UnixNano())}
}

// List returns objects under prefix ("" or ending in "/") in key order.
func (f *Filesystem) List(ctx context.Context, prefix, cursor string) ([]Object, string, error) {
	return f.list(ctx, prefix, cursor, listPage)
}

func (f *Filesystem) list(_ context.Context, prefix, cursor string, limit int) ([]Object, string, error) {
	if !validPrefix(prefix) {
		return nil, "", ErrInvalidKey
	}
	start := strings.TrimSuffix(objectPath(prefix), "/")
	var objs []Object
	err := fs.WalkDir(f.root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // directories, and symlinks, which are never objects
		}
		key := strings.TrimPrefix(p, "objects/")
		if key <= cursor || !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		objs = append(objs, objectOf(key, info))
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("list %s: %w", prefix, err)
	}
	// WalkDir orders by directory, not by full key ("a/b" before "a.txt").
	slices.SortFunc(objs, func(a, b Object) int { return strings.Compare(a.Key, b.Key) })
	if len(objs) > limit {
		objs = objs[:limit]
		return objs, objs[limit-1].Key, nil
	}
	return objs, "", nil
}

// DeletePrefix deletes every object under prefix, which must end in "/".
func (f *Filesystem) DeletePrefix(_ context.Context, prefix string) error {
	if prefix == "" || !validPrefix(prefix) {
		return ErrInvalidKey
	}
	if err := f.root.RemoveAll(strings.TrimSuffix(objectPath(prefix), "/")); err != nil {
		return fmt.Errorf("delete %s: %w", prefix, err)
	}
	return nil
}

// --- multipart ---

type uploadMeta struct {
	Key      string `json:"key"`
	Size     int64  `json:"size"`
	PartSize int64  `json:"part_size"`
}

func (m uploadMeta) parts() int { return int((m.Size + m.PartSize - 1) / m.PartSize) }

// partRange returns where part n sits and how long it must be.
func (m uploadMeta) partRange(n int) (offset, length int64, err error) {
	if n < 1 || n > m.parts() {
		return 0, 0, ErrInvalidParts
	}
	offset = int64(n-1) * m.PartSize
	return offset, min(m.PartSize, m.Size-offset), nil
}

func uploadDir(id string) string { return "multipart/" + id }

// CreateMultipart starts an upload and preallocates its file, so parts are
// written at their offsets and completion has nothing to join.
func (f *Filesystem) CreateMultipart(_ context.Context, key string, size int64) (Multipart, error) {
	if !ValidKey(key) {
		return Multipart{}, ErrInvalidKey
	}
	if size <= 0 {
		return Multipart{}, fmt.Errorf("%w: size must be positive", ErrInvalidParts)
	}
	if size > f.partSize*MaxParts {
		return Multipart{}, ErrTooLarge
	}
	id := rand.Text()
	dir := uploadDir(id)
	meta, _ := json.Marshal(uploadMeta{Key: key, Size: size, PartSize: f.partSize})
	err := f.root.MkdirAll(dir+"/parts", 0o750)
	if err == nil {
		err = f.root.WriteFile(dir+"/meta.json", meta, 0o640)
	}
	if err == nil {
		var data *os.File
		if data, err = f.root.OpenFile(dir+"/data", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640); err == nil {
			err = data.Truncate(size)
			if cerr := data.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		_ = f.root.RemoveAll(dir)
		return Multipart{}, fmt.Errorf("create multipart %s: %w", key, err)
	}
	return Multipart{UploadID: id, PartSize: f.partSize}, nil
}

// loadUpload reads an upload's metadata; a non-empty key must match it.
func (f *Filesystem) loadUpload(key, id string) (uploadMeta, error) {
	if !validSegment(id) {
		return uploadMeta{}, ErrNotFound
	}
	b, err := f.root.ReadFile(uploadDir(id) + "/meta.json")
	if errors.Is(err, fs.ErrNotExist) {
		return uploadMeta{}, ErrNotFound
	}
	if err != nil {
		return uploadMeta{}, fmt.Errorf("read upload %s: %w", id, err)
	}
	var m uploadMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return uploadMeta{}, fmt.Errorf("read upload %s: %w", id, err)
	}
	if key != "" && m.Key != key {
		return uploadMeta{}, ErrNotFound
	}
	return m, nil
}

// PresignPart signs a PUT for one part within the declared size.
func (f *Filesystem) PresignPart(_ context.Context, key, uploadID string, part int, ttl time.Duration) (string, error) {
	m, err := f.loadUpload(key, uploadID)
	if err != nil {
		return "", err
	}
	if _, _, err := m.partRange(part); err != nil {
		return "", err
	}
	return f.sign(http.MethodPut, partResource(uploadID, part), ttl), nil
}

func partResource(id string, n int) string { return "parts/" + id + "/" + strconv.Itoa(n) }

func partMarker(id string, n int) string { return fmt.Sprintf("%s/parts/%05d", uploadDir(id), n) }

// writePart writes part n at its offset. It never writes past the part's
// range, so an oversized body cannot clobber the next part. The marker that
// makes the part visible is written only after the bytes land.
func (f *Filesystem) writePart(id string, n int, body io.Reader, declared int64) (string, error) {
	m, err := f.loadUpload("", id)
	if err != nil {
		return "", err
	}
	offset, length, err := m.partRange(n)
	if err != nil {
		return "", err
	}
	if declared > length {
		return "", ErrTooLarge
	}
	if declared >= 0 && declared < length {
		return "", errShortPart
	}
	data, err := f.root.OpenFile(uploadDir(id)+"/data", os.O_WRONLY, 0)
	if err != nil {
		return "", fmt.Errorf("write part: %w", err)
	}
	sum := md5.New()
	written, err := io.Copy(io.MultiWriter(io.NewOffsetWriter(data, offset), sum), io.LimitReader(body, length))
	if cerr := data.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("write part: %w", err)
	case written < length:
		return "", errShortPart
	}
	if extra, _ := io.ReadFull(body, make([]byte, 1)); extra > 0 {
		return "", ErrTooLarge
	}
	etag := `"` + hex.EncodeToString(sum.Sum(nil)) + `"`
	marker, _ := json.Marshal(Part{Number: n, ETag: etag, Size: length})
	tmp := "tmp/" + rand.Text()
	if err := f.root.WriteFile(tmp, marker, 0o640); err != nil {
		return "", fmt.Errorf("write part marker: %w", err)
	}
	if err := f.root.Rename(tmp, partMarker(id, n)); err != nil {
		_ = f.root.Remove(tmp)
		return "", fmt.Errorf("write part marker: %w", err)
	}
	return etag, nil
}

// ListParts returns the parts that have landed, in order.
func (f *Filesystem) ListParts(_ context.Context, key, uploadID string) ([]Part, error) {
	if _, err := f.loadUpload(key, uploadID); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(f.root.FS(), uploadDir(uploadID)+"/parts")
	if err != nil {
		return nil, fmt.Errorf("list parts: %w", err)
	}
	parts := make([]Part, 0, len(entries))
	for _, e := range entries { // ReadDir sorts by name; names are zero-padded
		b, err := f.root.ReadFile(uploadDir(uploadID) + "/parts/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("list parts: %w", err)
		}
		var p Part
		if err := json.Unmarshal(b, &p); err != nil {
			return nil, fmt.Errorf("list parts: %w", err)
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// CompleteMultipart checks that every part landed with the given ETags and
// moves the preallocated file into place.
func (f *Filesystem) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	m, err := f.loadUpload(key, uploadID)
	if err != nil {
		return err
	}
	stored, err := f.ListParts(ctx, key, uploadID)
	if err != nil {
		return err
	}
	if len(parts) != m.parts() || len(stored) != m.parts() {
		return fmt.Errorf("%w: want %d parts", ErrInvalidParts, m.parts())
	}
	for i, p := range parts {
		if p.Number != i+1 || stored[i].Number != i+1 || strings.Trim(p.ETag, `"`) != strings.Trim(stored[i].ETag, `"`) {
			return fmt.Errorf("%w: part %d", ErrInvalidParts, i+1)
		}
	}
	dst := objectPath(key)
	if err := f.root.MkdirAll(path.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("complete %s: %w", key, err)
	}
	if err := f.root.Rename(uploadDir(uploadID)+"/data", dst); err != nil {
		return fmt.Errorf("complete %s: %w", key, err)
	}
	if err := f.root.RemoveAll(uploadDir(uploadID)); err != nil {
		return fmt.Errorf("complete %s: clean up: %w", key, err)
	}
	return nil
}

// AbortMultipart discards an upload and its bytes.
func (f *Filesystem) AbortMultipart(_ context.Context, key, uploadID string) error {
	if _, err := f.loadUpload(key, uploadID); err != nil {
		return err
	}
	if err := f.root.RemoveAll(uploadDir(uploadID)); err != nil {
		return fmt.Errorf("abort %s: %w", key, err)
	}
	return nil
}

// --- signed URLs ---

// PresignGet signs a GET (and HEAD) of an object.
func (f *Filesystem) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	return f.sign(http.MethodGet, "objects/"+key, ttl), nil
}

// PresignPut signs a PUT of an object.
func (f *Filesystem) PresignPut(_ context.Context, key string, ttl time.Duration) (string, error) {
	if !ValidKey(key) {
		return "", ErrInvalidKey
	}
	return f.sign(http.MethodPut, "objects/"+key, ttl), nil
}

// sign returns BaseURL/_storage/{resource}?exp=…&sig=…. Keys are restricted
// to URL-safe characters, so the resource needs no escaping.
func (f *Filesystem) sign(method, resource string, ttl time.Duration) string {
	exp := strconv.FormatInt(f.now().Add(ttl).Unix(), 10)
	return f.baseURL + pathPrefix + resource + "?exp=" + exp + "&sig=" + f.mac(method, resource, exp)
}

func (f *Filesystem) mac(method, resource, exp string) string {
	h := hmac.New(sha256.New, f.key)
	h.Write([]byte(signVersion + "\n" + method + "\n" + resource + "\n" + exp))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// verify checks a request's signature and expiry for resource.
func (f *Filesystem) verify(r *http.Request, resource string) bool {
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	q := r.URL.Query()
	exp, sig := q.Get("exp"), q.Get("sig")
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || f.now().Unix() > unix {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(f.mac(method, resource, exp)))
}

// --- HTTP ---

// Handler serves signed URLs under /_storage/. The URL is the only
// authorisation, so any origin may use it (CORS *), and ETag is exposed so
// browsers can complete multipart uploads.
func (f *Filesystem) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS "+pathPrefix, func(w http.ResponseWriter, _ *http.Request) {
		cors(w)
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT "+pathPrefix+"parts/{id}/{n}", f.servePart)
	mux.HandleFunc("GET "+pathPrefix+"objects/{key...}", f.serveGet)
	mux.HandleFunc("PUT "+pathPrefix+"objects/{key...}", f.servePut)
	return mux
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "ETag")
}

func (f *Filesystem) servePart(w http.ResponseWriter, r *http.Request) {
	cors(w)
	id, n := r.PathValue("id"), r.PathValue("n")
	if !f.verify(r, "parts/"+id+"/"+n) {
		http.Error(w, "invalid or expired signature", http.StatusForbidden)
		return
	}
	part, err := strconv.Atoi(n)
	if err != nil {
		http.Error(w, "bad part number", http.StatusBadRequest)
		return
	}
	etag, err := f.writePart(id, part, r.Body, r.ContentLength)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (f *Filesystem) serveGet(w http.ResponseWriter, r *http.Request) {
	cors(w)
	key := r.PathValue("key")
	if !f.verify(r, "objects/"+key) {
		http.Error(w, "invalid or expired signature", http.StatusForbidden)
		return
	}
	file, info, err := f.open(key)
	if err != nil {
		writeStorageError(w, err)
		return
	}
	defer file.Close()
	// Stored bytes are untrusted: never let a browser interpret them.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", objectOf(key, info).ETag)
	http.ServeContent(w, r, "", info.ModTime(), file)
}

func (f *Filesystem) servePut(w http.ResponseWriter, r *http.Request) {
	cors(w)
	key := r.PathValue("key")
	if !f.verify(r, "objects/"+key) {
		http.Error(w, "invalid or expired signature", http.StatusForbidden)
		return
	}
	if err := f.Put(r.Context(), key, r.Body); err != nil {
		writeStorageError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func writeStorageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrTooLarge):
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, ErrInvalidKey), errors.Is(err, ErrInvalidParts), errors.Is(err, errShortPart):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, "storage error", http.StatusInternalServerError)
	}
}
