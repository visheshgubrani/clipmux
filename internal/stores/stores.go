// Package stores resolves an asset's storage (docs/architecture.md,
// "Per-asset storage"). Each asset records two stores: storage_id for its
// source, from the project default, and media_storage_id for its encoded
// files and tracks, the Cloudflare delivery bucket when the Worker delivers
// (D117). Changing either target leaves old assets where they are. The filesystem store is one per install; R2 and S3 configs are
// storage_configs rows whose credentials are sealed with the secrets key
// (D017, associated data storage_configs/{id}).
package stores

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// Kinds of storage config.
const (
	KindFilesystem = "fs"
	KindR2         = "r2"
	KindS3         = "s3"
)

// ErrUnknown means no storage config has that ID.
var ErrUnknown = errors.New("stores: unknown storage")

// S3Config is the non-secret part of an r2 or s3 config.
type S3Config struct {
	Endpoint       string `json:"endpoint"`
	PublicEndpoint string `json:"public_endpoint,omitempty"`
	Region         string `json:"region,omitempty"`
	Bucket         string `json:"bucket"`
	// AccountID is the Cloudflare account an r2 bucket belongs to.
	AccountID string `json:"account_id,omitempty"`
}

// S3Secret is sealed, never stored or logged in the clear.
type S3Secret struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// Set opens each config once and keeps it.
type Set struct {
	pool  *pgxpool.Pool
	box   *secrets.Box
	fs    storage.Storage
	fixed storage.Storage

	mu   sync.Mutex
	open map[string]storage.Storage
}

// New returns the project's stores; fs serves every "fs" config.
func New(pool *pgxpool.Pool, box *secrets.Box, fs storage.Storage) *Set {
	return &Set{pool: pool, box: box, fs: fs, open: map[string]storage.Storage{}}
}

// Fixed resolves every storage ID to one store, for tests and tools that
// run with a single store.
func Fixed(s storage.Storage) *Set { return &Set{fixed: s} }

// Get returns the store a config ID names.
func (s *Set) Get(ctx context.Context, id string) (storage.Storage, error) {
	return s.GetIn(ctx, db.New(s.pool), id)
}

// GetIn is Get reading through q, a caller's transaction. Inside a
// transaction, a lookup through the pool would need a second connection
// while holding one, and a burst of such requests larger than the pool
// would wait on each other forever.
func (s *Set) GetIn(ctx context.Context, q *db.Queries, id string) (storage.Storage, error) {
	if s.fixed != nil {
		return s.fixed, nil
	}
	s.mu.Lock()
	st, ok := s.open[id]
	s.mu.Unlock()
	if ok {
		return st, nil
	}
	row, err := q.GetStorageConfig(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if err != nil {
		return nil, fmt.Errorf("load storage %s: %w", id, err)
	}
	st, err = s.openRow(row)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.open[id]; ok {
		return prev, nil
	}
	s.open[id] = st
	return st, nil
}

// SourceFor returns the store holding an asset's source, uploads/{asset}/….
func (s *Set) SourceFor(ctx context.Context, assetID string) (storage.Storage, error) {
	return s.SourceForIn(ctx, db.New(s.pool), assetID)
}

// SourceForIn is SourceFor reading through q, a caller's transaction (see
// GetIn).
func (s *Set) SourceForIn(ctx context.Context, q *db.Queries, assetID string) (storage.Storage, error) {
	at, err := s.assetStores(ctx, q, assetID)
	if err != nil || s.fixed != nil {
		return s.fixed, err
	}
	return s.GetIn(ctx, q, at.StorageID)
}

// MediaFor returns the store holding an asset's encoded files and tracks,
// media/{asset}/… (D117).
func (s *Set) MediaFor(ctx context.Context, assetID string) (storage.Storage, error) {
	return s.MediaForIn(ctx, db.New(s.pool), assetID)
}

// MediaForIn is MediaFor reading through q, a caller's transaction (see
// GetIn).
func (s *Set) MediaForIn(ctx context.Context, q *db.Queries, assetID string) (storage.Storage, error) {
	at, err := s.assetStores(ctx, q, assetID)
	if err != nil || s.fixed != nil {
		return s.fixed, err
	}
	return s.GetIn(ctx, q, at.MediaStorageID)
}

func (s *Set) assetStores(ctx context.Context, q *db.Queries, assetID string) (db.AssetStoresRow, error) {
	if s.fixed != nil {
		return db.AssetStoresRow{}, nil
	}
	row, err := q.AssetStores(ctx, assetID)
	if err != nil {
		return row, fmt.Errorf("storage of %s: %w", assetID, err)
	}
	return row, nil
}

// Default returns the project's default store, which new assets use.
func (s *Set) Default(ctx context.Context, projectID string) (string, storage.Storage, error) {
	row, err := db.New(s.pool).GetDefaultStorage(ctx, projectID)
	if err != nil {
		return "", nil, fmt.Errorf("default storage: %w", err)
	}
	st, err := s.Get(ctx, row.ID)
	return row.ID, st, err
}

func (s *Set) openRow(row db.StorageConfig) (storage.Storage, error) {
	switch row.Kind {
	case KindFilesystem:
		if s.fs == nil {
			return nil, fmt.Errorf("storage %s: no filesystem store", row.ID)
		}
		return s.fs, nil
	case KindR2, KindS3:
		var cfg S3Config
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return nil, fmt.Errorf("storage %s: config: %w", row.ID, err)
		}
		sec, err := s.secret(row)
		if err != nil {
			return nil, err
		}
		return storage.OpenS3(options(cfg, sec))
	}
	return nil, fmt.Errorf("storage %s: unknown kind %q", row.ID, row.Kind)
}

func (s *Set) secret(row db.StorageConfig) (S3Secret, error) {
	var sec S3Secret
	if s.box == nil || len(row.SecretsEncrypted) == 0 {
		return sec, fmt.Errorf("storage %s: no credentials", row.ID)
	}
	plain, err := s.box.Open("storage_configs", row.ID, row.SecretsEncrypted)
	if err != nil {
		return sec, fmt.Errorf("storage %s: open credentials: %w", row.ID, err)
	}
	if err := json.Unmarshal(plain, &sec); err != nil {
		return sec, fmt.Errorf("storage %s: credentials: %w", row.ID, err)
	}
	return sec, nil
}

func options(cfg S3Config, sec S3Secret) storage.S3Options {
	return storage.S3Options{
		Endpoint: cfg.Endpoint, PublicEndpoint: cfg.PublicEndpoint, Region: cfg.Region, Bucket: cfg.Bucket,
		AccessKeyID: sec.AccessKeyID, SecretAccessKey: sec.SecretAccessKey,
	}
}

// Origins are the origins browsers reach this project's S3 and R2 buckets
// at (their public endpoints), for the dashboard's Content-Security-Policy:
// uploads PUT their parts there. The filesystem store is the public URL.
func (s *Set) Origins(ctx context.Context, projectID string) ([]string, error) {
	rows, err := db.New(s.pool).ListStorageConfigs(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list storage configs: %w", err)
	}
	var out []string
	for _, row := range rows {
		if row.Kind == KindFilesystem {
			continue
		}
		var cfg S3Config
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return nil, fmt.Errorf("storage config %s: %w", row.ID, err)
		}
		if o := origin(cmp.Or(cfg.PublicEndpoint, cfg.Endpoint)); o != "" && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out, nil
}

// origin is a URL's scheme and host, or "" if it has neither.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// AddS3 checks an r2 or s3 config against its bucket (list, write, read,
// delete, and CORS for browser uploads), then stores it with its
// credentials sealed. It doesn't change the default.
func (s *Set) AddS3(ctx context.Context, projectID, kind string, cfg S3Config, sec S3Secret) (db.StorageConfig, error) {
	if kind != KindR2 && kind != KindS3 {
		return db.StorageConfig{}, fmt.Errorf("add storage: kind %q", kind)
	}
	st, err := storage.OpenS3(options(cfg, sec))
	if err != nil {
		return db.StorageConfig{}, err
	}
	if err := st.Check(ctx); err != nil {
		return db.StorageConfig{}, fmt.Errorf("check bucket: %w", err)
	}
	if err := st.ApplyCORS(ctx); err != nil {
		return db.StorageConfig{}, fmt.Errorf("apply CORS: %w", err)
	}
	id := ids.New(ids.Storage)
	conf, _ := json.Marshal(cfg) // plain strings; can't fail
	plain, _ := json.Marshal(sec)
	row, err := db.New(s.pool).CreateStorageConfig(ctx, db.CreateStorageConfigParams{
		ID: id, ProjectID: projectID, Kind: kind, Config: conf, SecretsEncrypted: s.box.Seal("storage_configs", id, plain),
	})
	if err != nil {
		return db.StorageConfig{}, fmt.Errorf("add storage: %w", err)
	}
	s.mu.Lock()
	s.open[id] = st
	s.mu.Unlock()
	return row, nil
}

// SetDefault makes a config the one new uploads use. Existing assets stay
// where they are until a move job moves them.
func (s *Set) SetDefault(ctx context.Context, projectID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set default storage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)
	if err := q.ClearDefaultStorage(ctx, projectID); err != nil {
		return fmt.Errorf("set default storage: %w", err)
	}
	n, err := q.SetDefaultStorage(ctx, db.SetDefaultStorageParams{ProjectID: projectID, ID: id})
	if err != nil {
		return fmt.Errorf("set default storage: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	return tx.Commit(ctx)
}

// UpdateS3Secret checks new credentials for an existing r2 or s3 config
// against its bucket and stores them sealed, as when a Cloudflare token is
// replaced. The config itself doesn't change.
func (s *Set) UpdateS3Secret(ctx context.Context, id string, cfg S3Config, sec S3Secret) error {
	st, err := storage.OpenS3(options(cfg, sec))
	if err != nil {
		return err
	}
	if err := st.Check(ctx); err != nil {
		return fmt.Errorf("check bucket: %w", err)
	}
	if err := st.ApplyCORS(ctx); err != nil {
		return fmt.Errorf("apply CORS: %w", err)
	}
	plain, _ := json.Marshal(sec)
	n, err := db.New(s.pool).UpdateStorageSecrets(ctx, db.UpdateStorageSecretsParams{ID: id, SecretsEncrypted: s.box.Seal("storage_configs", id, plain)})
	if err != nil {
		return fmt.Errorf("update storage credentials: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update storage credentials: %w: %s", ErrUnknown, id)
	}
	s.mu.Lock()
	s.open[id] = st
	s.mu.Unlock()
	return nil
}

// Errors Remove answers with.
var (
	// ErrDefault: new uploads go to it; make another the default first.
	ErrDefault = errors.New("stores: the default storage can't be removed")
	// ErrBuiltin: the filesystem store is this install's own.
	ErrBuiltin = errors.New("stores: the filesystem storage can't be removed")
)

// InUseError says what still needs a store.
type InUseError struct {
	// Assets may still have files there (deleted ones until their files go);
	// Jobs are moves to or from it; Delivery is Cloudflare's Worker reading it.
	Assets, Jobs int64
	Delivery     bool
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("stores: storage in use by %d assets and %d move jobs (delivery %v)", e.Assets, e.Jobs, e.Delivery)
}

// Remove retires an r2 or s3 config that nothing needs any more (D099): no
// asset's files, no move, not Cloudflare delivery's bucket and not the
// default. The row stays for the history that names it, without its
// credentials, and is never listed or chosen again.
func (s *Set) Remove(ctx context.Context, projectID, id string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockStorageConfig(ctx, db.LockStorageConfigParams{ProjectID: projectID, ID: id})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrUnknown, id)
		case err != nil:
			return err
		case row.Kind == KindFilesystem:
			return ErrBuiltin
		case row.IsDefault:
			return ErrDefault
		}
		use, err := q.StorageUsage(ctx, id)
		if err != nil {
			return err
		}
		if use.Assets > 0 || use.Jobs > 0 || use.Delivery {
			return &InUseError{Assets: use.Assets, Jobs: use.Jobs, Delivery: use.Delivery}
		}
		return q.RemoveStorageConfig(ctx, id)
	})
	if err != nil {
		return fmt.Errorf("remove storage %s: %w", id, err)
	}
	s.mu.Lock()
	delete(s.open, id)
	s.mu.Unlock()
	return nil
}
