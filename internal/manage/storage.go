package manage

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/cloudflare"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// Storage configs (D099, D110): the owner lists them, adds a generic S3
// bucket (checked with a test write before it is saved) or an R2 bucket
// with a Cloudflare token, rotates a bucket's credentials, picks the
// default for new uploads (this server's disk included), and removes one
// nothing needs any more.

type storageView struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Bucket         string `json:"bucket,omitempty"`
	Endpoint       string `json:"endpoint,omitempty"`
	PublicEndpoint string `json:"public_endpoint,omitempty"`
	Region         string `json:"region,omitempty"`
	Default        bool   `json:"default"`
	AssetCount     int64  `json:"asset_count"`
	SourceBytes    int64  `json:"source_bytes"`
	MediaBytes     int64  `json:"media_bytes"`
	TotalBytes     int64  `json:"total_bytes"`
	// CloudflareDelivery: the Worker reads this bucket; it can't be removed.
	CloudflareDelivery bool      `json:"cloudflare_delivery"`
	Created            time.Time `json:"created_at"`
}

func viewStorage(id, kind string, config []byte, isDefault bool, assetCount, sourceBytes, mediaBytes int64, delivery bool, created time.Time) storageView {
	v := storageView{
		ID: id, Kind: kind, Default: isDefault, AssetCount: assetCount,
		SourceBytes: sourceBytes, MediaBytes: mediaBytes, TotalBytes: sourceBytes + mediaBytes,
		CloudflareDelivery: delivery, Created: created.UTC(),
	}
	if kind != stores.KindFilesystem {
		var cfg stores.S3Config
		_ = json.Unmarshal(config, &cfg) // written by stores.AddS3; a bad row shows without its details
		v.Bucket, v.Endpoint, v.PublicEndpoint, v.Region = cfg.Bucket, cfg.Endpoint, cfg.PublicEndpoint, cfg.Region
	}
	return v
}

func (m *API) listStorage(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Stores == nil {
		return errNotFound
	}
	rows, err := db.New(m.pool).ListStorageSummaries(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	out := make([]storageView, 0, len(rows))
	for _, c := range rows {
		out = append(out, viewStorage(c.ID, c.Kind, c.Config, c.IsDefault, c.AssetCount, c.SourceBytes, c.MediaBytes, c.Delivery, c.CreatedAt.Time))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

type s3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

func (c s3Credentials) check() error {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "access_key_id and secret_access_key are required")
	}
	return nil
}

// checkFailed is a bucket that refused the test write: the owner fixes the
// endpoint, bucket or keys and tries again.
func checkFailed(err error) error {
	return httpjson.Fail(http.StatusBadRequest, "storage_check_failed", err.Error())
}

// addStorage saves a generic S3 bucket once a test write, read, list and
// delete pass and CORS for browser uploads is set. It doesn't become the
// default.
func (m *API) addStorage(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Stores == nil {
		return errNotFound
	}
	var body struct {
		Endpoint       string `json:"endpoint"`
		PublicEndpoint string `json:"public_endpoint"`
		Region         string `json:"region"`
		Bucket         string `json:"bucket"`
		s3Credentials
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	for name, raw := range map[string]string{"endpoint": body.Endpoint, "public_endpoint": body.PublicEndpoint} {
		if raw == "" && name == "public_endpoint" {
			continue
		}
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", name+" must be an http or https URL without credentials")
		}
	}
	if n := len(body.Bucket); n < 3 || n > 63 || strings.ContainsAny(body.Bucket, "/ ") {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "bucket must be a bucket name (3 to 63 characters)")
	}
	if err := body.check(); err != nil {
		return err
	}
	cfg := stores.S3Config{Endpoint: strings.TrimRight(body.Endpoint, "/"), PublicEndpoint: strings.TrimRight(body.PublicEndpoint, "/"), Region: body.Region, Bucket: body.Bucket}
	row, err := m.Stores.AddS3(r.Context(), o.ProjectID, stores.KindS3, cfg, stores.S3Secret{AccessKeyID: body.AccessKeyID, SecretAccessKey: body.SecretAccessKey})
	if err != nil && r.Context().Err() == nil && !isDB(err) {
		return checkFailed(err)
	}
	if err != nil {
		return err
	}
	m.Logger.InfoContext(r.Context(), "storage added", "storage", row.ID, "bucket", cfg.Bucket, "endpoint", cfg.Endpoint)
	httpjson.Write(w, http.StatusCreated, viewStorage(row.ID, row.Kind, row.Config, row.IsDefault, 0, 0, 0, false, row.CreatedAt.Time))
	return nil
}

// addR2Storage saves an R2 bucket with a token that can edit R2, creating
// the bucket if it's missing, without moving delivery (D110). The token is
// used for this request only; every step's result comes back, like Connect
// Cloudflare's, and a failed step can be fixed and tried again.
func (m *API) addR2Storage(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Cloudflare == nil {
		return errNotFound
	}
	var req cloudflare.R2Request
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, id, err := m.Cloudflare.AddR2(r.Context(), req)
	if errors.Is(err, cloudflare.ErrInvalid) {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), cloudflare.ErrInvalid.Error()+": "))
	}
	if err != nil {
		return err
	}
	if res.Connected {
		m.Logger.InfoContext(r.Context(), "R2 storage added", "storage", id, "bucket", req.Bucket)
	}
	httpjson.Write(w, http.StatusOK, res)
	return nil
}

// ownStorage loads one of the owner's storage configs that isn't removed.
func (m *API) ownStorage(r *http.Request, o accounts.Owner) (db.StorageConfig, error) {
	row, err := db.New(m.pool).GetStorageConfig(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (row.ProjectID != o.ProjectID || row.RemovedAt.Valid) {
		return db.StorageConfig{}, httpjson.Fail(http.StatusNotFound, "not_found", "no such storage")
	}
	return row, err
}

// rotateStorageCredentials replaces an r2 or s3 bucket's keys once the new
// ones pass the same checks as adding it; the old keys work until then.
func (m *API) rotateStorageCredentials(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Stores == nil {
		return errNotFound
	}
	row, err := m.ownStorage(r, o)
	if err != nil {
		return err
	}
	if row.Kind == stores.KindFilesystem {
		return httpjson.Fail(http.StatusConflict, "builtin_storage", "the filesystem storage has no credentials")
	}
	var body s3Credentials
	if err := decode(w, r, &body); err != nil {
		return err
	}
	if err := body.check(); err != nil {
		return err
	}
	var cfg stores.S3Config
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return err
	}
	err = m.Stores.UpdateS3Secret(r.Context(), row.ID, cfg, stores.S3Secret(body))
	switch {
	case errors.Is(err, stores.ErrUnknown):
		return errNotFound
	case err != nil && r.Context().Err() == nil && !isDB(err):
		return checkFailed(err)
	case err != nil:
		return err
	}
	m.Logger.InfoContext(r.Context(), "storage credentials rotated", "storage", row.ID)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// setDefaultStorage makes a store the one new uploads go to. Existing
// assets stay where they are until moved (POST /v1/storage/moves).
func (m *API) setDefaultStorage(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Stores == nil {
		return errNotFound
	}
	err := m.Stores.SetDefault(r.Context(), o.ProjectID, r.PathValue("id"))
	if errors.Is(err, stores.ErrUnknown) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	return m.listStorage(w, r, o)
}

// removeStorage retires a store nothing needs any more.
func (m *API) removeStorage(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Stores == nil {
		return errNotFound
	}
	err := m.Stores.Remove(r.Context(), o.ProjectID, r.PathValue("id"))
	var inUse *stores.InUseError
	switch {
	case errors.Is(err, stores.ErrUnknown):
		return errNotFound
	case errors.Is(err, stores.ErrBuiltin):
		return httpjson.Fail(http.StatusConflict, "builtin_storage", "the filesystem storage is this install's own and can't be removed")
	case errors.Is(err, stores.ErrDefault):
		return httpjson.Fail(http.StatusConflict, "storage_default", "new uploads go to this storage; make another the default first")
	case errors.As(err, &inUse) && inUse.Delivery:
		return httpjson.Fail(http.StatusConflict, "cloudflare_delivery", "Cloudflare delivery reads this bucket; disconnect Cloudflare first")
	case errors.As(err, &inUse):
		p := httpjson.Fail(http.StatusConflict, "storage_in_use", "assets still have files here, or are moving; move the library to another storage first")
		p.Details = map[string]string{"asset_count": strconv.FormatInt(inUse.Assets, 10), "active_moves": strconv.FormatInt(inUse.Jobs, 10)}
		return p
	case err != nil:
		return err
	}
	m.Logger.InfoContext(r.Context(), "storage removed", "storage", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
