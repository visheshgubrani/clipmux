// Package stores resolves an asset's storage (docs/architecture.md,
// "Per-asset storage"). Each asset records its storage_id; new uploads use
// the project default, and changing the default leaves old assets where they
// are. The filesystem store is one per install; R2 and S3 configs are
// storage_configs rows whose credentials are sealed with the secrets key
// (D017, associated data storage_configs/{id}).
package stores

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if s.fixed != nil {
		return s.fixed, nil
	}
	s.mu.Lock()
	st, ok := s.open[id]
	s.mu.Unlock()
	if ok {
		return st, nil
	}
	row, err := db.New(s.pool).GetStorageConfig(ctx, id)
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

// ForAsset returns the store holding an asset's source and outputs.
func (s *Set) ForAsset(ctx context.Context, assetID string) (storage.Storage, error) {
	if s.fixed != nil {
		return s.fixed, nil
	}
	id, err := db.New(s.pool).AssetStorageID(ctx, assetID)
	if err != nil {
		return nil, fmt.Errorf("storage of %s: %w", assetID, err)
	}
	return s.Get(ctx, id)
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
	if err := db.New(s.pool).UpdateStorageSecrets(ctx, db.UpdateStorageSecretsParams{ID: id, SecretsEncrypted: s.box.Seal("storage_configs", id, plain)}); err != nil {
		return fmt.Errorf("update storage credentials: %w", err)
	}
	s.mu.Lock()
	s.open[id] = st
	s.mu.Unlock()
	return nil
}
