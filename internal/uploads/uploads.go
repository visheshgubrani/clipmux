// Package uploads is the direct-upload API (docs/architecture.md,
// "Multipart upload flow" and "Abandoned uploads"). An app's backend creates
// an upload with its API key; the browser then signs parts, PUTs them
// straight to storage, and completes with the upload token. The endpoints map
// one to one onto Uppy's S3 multipart callbacks (D011).
//
// Reliability rules:
//   - Create is one transaction, and an Idempotency-Key replays it instead of
//     creating a second asset.
//   - Complete is idempotent and crash-safe: if storage finished but the
//     database did not, the next complete or sweep finishes the database side.
//   - The sweeper completes uploads whose parts all landed, and expires an
//     upload only after 24 hours without activity (a signed part or a newly
//     stored part), never by age.
package uploads

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/idempotency"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// Error codes the upload API returns or records on assets.
const (
	CodeUploadCancelled = "upload_cancelled"
	CodeUploadExpired   = "upload_expired"
)

const (
	maxPartsPerSign = 100
	maxPassthrough  = 255
)

// Service runs uploads against one storage backend.
type Service struct {
	pool  *pgxpool.Pool
	store storage.Storage

	// MaxSize is the largest declared size accepted.
	MaxSize int64
	// PartURLTTL is how long a signed part URL lasts. An hour leaves room for
	// a batch of URLs to be used one after another on a slow link.
	PartURLTTL time.Duration
	// IdleExpiry is how long an upload may go without activity.
	IdleExpiry time.Duration
	// BaseURL is the media origin returned with assets.
	BaseURL string
	Logger  *slog.Logger
}

// New returns a Service with the architecture's defaults.
func New(pool *pgxpool.Pool, store storage.Storage) *Service {
	return &Service{
		pool: pool, store: store,
		MaxSize:    storage.PartSize * storage.MaxParts,
		PartURLTTL: time.Hour,
		IdleExpiry: 24 * time.Hour,
		Logger:     slog.Default(),
	}
}

// fail is an expected failure with a stable code.
func fail(status int, code, message string) error { return httpjson.Fail(status, code, message) }

var errNotActive = fail(http.StatusConflict, "upload_not_active", "the upload is no longer active")

func sourceKey(assetID string) string { return "uploads/" + assetID + "/source" }

func tokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func partCount(u db.Upload) int { return int((u.DeclaredSize + u.PartSize - 1) / u.PartSize) }

// --- create ---

// CreateRequest is the body of POST /v1/uploads.
type CreateRequest struct {
	Size           int64  `json:"size"`
	PlaybackPolicy string `json:"playback_policy"`
	Passthrough    string `json:"passthrough"`
}

// Created is the response to POST /v1/uploads.
type Created struct {
	UploadID    string              `json:"upload_id"`
	AssetID     string              `json:"asset_id"`
	PartSize    int64               `json:"part_size"`
	PartCount   int                 `json:"part_count"`
	UploadToken string              `json:"upload_token,omitempty"`
	PlaybackIDs []assets.PlaybackID `json:"playback_ids"`
}

// Create starts an upload for the key's app: an asset in waiting_upload, a
// playback ID, a storage multipart upload and an upload token, in one
// transaction. With an idempotency key, a repeat returns the same upload
// with a newly issued token (tokens are never stored).
func (s *Service) Create(ctx context.Context, key apikeys.Key, req CreateRequest, idemKey string) (Created, error) {
	if req.PlaybackPolicy == "" {
		req.PlaybackPolicy = "signed"
	}
	switch {
	case req.Size <= 0:
		return Created{}, fail(http.StatusBadRequest, "invalid_request", "size must be a positive number of bytes")
	case req.Size > s.MaxSize:
		return Created{}, fail(http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("size exceeds this instance's limit of %d bytes", s.MaxSize))
	case req.PlaybackPolicy != "signed" && req.PlaybackPolicy != "public":
		return Created{}, fail(http.StatusBadRequest, "invalid_request", `playback_policy must be "signed" or "public"`)
	case len(req.Passthrough) > maxPassthrough:
		return Created{}, fail(http.StatusBadRequest, "invalid_request", "passthrough is limited to 255 bytes")
	}
	canonical, _ := json.Marshal(req)

	var out Created
	var started *storage.Multipart
	var startedKey string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/uploads", canonical)
		if err != nil {
			return idempotency.Problem(err)
		}
		if claim != nil && claim.Replay != nil {
			return s.replay(ctx, q, claim.Replay, &out)
		}
		st, err := q.GetDefaultStorage(ctx, key.ProjectID)
		if err != nil {
			return err
		}
		asset, err := q.CreateAsset(ctx, db.CreateAssetParams{
			ID: ids.New(ids.Asset), ProjectID: key.ProjectID, AppID: key.AppID, StorageID: st.ID,
			Passthrough: pgtype.Text{String: req.Passthrough, Valid: req.Passthrough != ""},
		})
		if err != nil {
			return err
		}
		pid, err := q.CreatePlaybackID(ctx, db.CreatePlaybackIDParams{
			ID: ids.New(ids.PlaybackID), ProjectID: key.ProjectID, AssetID: asset.ID, Policy: req.PlaybackPolicy,
		})
		if err != nil {
			return err
		}
		mp, err := s.store.CreateMultipart(ctx, sourceKey(asset.ID), req.Size)
		if err != nil {
			return err
		}
		started, startedKey = &mp, sourceKey(asset.ID)
		token := rand.Text()
		up, err := q.CreateUpload(ctx, db.CreateUploadParams{
			ID: ids.New(ids.Upload), ProjectID: key.ProjectID, AssetID: asset.ID, MultipartID: mp.UploadID,
			DeclaredSize: req.Size, PartSize: mp.PartSize, TokenHash: tokenHash(token),
		})
		if err != nil {
			return err
		}
		out = Created{
			UploadID: up.ID, AssetID: asset.ID, PartSize: up.PartSize, PartCount: partCount(up),
			PlaybackIDs: []assets.PlaybackID{{ID: pid.ID, Policy: pid.Policy}},
		}
		stored, _ := json.Marshal(out) // without the token
		if err := claim.Save(ctx, http.StatusCreated, stored); err != nil {
			return err
		}
		out.UploadToken = token
		return nil
	})
	if err != nil && started != nil {
		// The transaction rolled back after storage started an upload.
		_ = s.store.AbortMultipart(context.WithoutCancel(ctx), startedKey, started.UploadID)
	}
	if err != nil {
		return Created{}, err
	}
	return out, nil
}

// replay answers a repeated Idempotency-Key from the stored response.
func (s *Service) replay(ctx context.Context, q *db.Queries, stored []byte, out *Created) error {
	if err := json.Unmarshal(stored, out); err != nil {
		return err
	}
	token := rand.Text()
	n, err := q.RotateUploadToken(ctx, db.RotateUploadTokenParams{ID: out.UploadID, TokenHash: tokenHash(token)})
	if err != nil {
		return err
	}
	if n == 1 {
		out.UploadToken = token // still active: a fresh token to finish it with
	}
	return nil
}

// --- parts ---

// SignedPart is one signed part URL.
type SignedPart struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
}

// Signed is the response to POST /v1/uploads/{id}/parts.
type Signed struct {
	Parts     []SignedPart `json:"parts"`
	ExpiresAt time.Time    `json:"expires_at"`
}

// SignParts signs PUT URLs for parts within the declared size. Signing
// counts as activity.
func (s *Service) SignParts(ctx context.Context, up db.Upload, numbers []int) (Signed, error) {
	if up.Status != "active" {
		return Signed{}, errNotActive
	}
	if len(numbers) == 0 || len(numbers) > maxPartsPerSign {
		return Signed{}, fail(http.StatusBadRequest, "invalid_request", "part_numbers must list 1 to 100 parts")
	}
	n := partCount(up)
	for _, p := range numbers {
		if p < 1 || p > n {
			return Signed{}, fail(http.StatusBadRequest, "invalid_request", fmt.Sprintf("part %d is outside the declared size (parts 1 to %d)", p, n))
		}
	}
	if err := db.New(s.pool).TouchUpload(ctx, up.ID); err != nil {
		return Signed{}, err
	}
	out := Signed{Parts: make([]SignedPart, 0, len(numbers)), ExpiresAt: time.Now().Add(s.PartURLTTL).UTC()}
	for _, p := range slices.Compact(slices.Sorted(slices.Values(numbers))) {
		url, err := s.store.PresignPart(ctx, sourceKey(up.AssetID), up.MultipartID, p, s.PartURLTTL)
		if err != nil {
			return Signed{}, err
		}
		out.Parts = append(out.Parts, SignedPart{PartNumber: p, URL: url})
	}
	return out, nil
}

// StoredPart is a part that has landed in storage.
type StoredPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

// ListParts reports the stored parts, so a reloaded page can resume.
func (s *Service) ListParts(ctx context.Context, up db.Upload) ([]StoredPart, error) {
	if up.Status != "active" {
		return nil, errNotActive
	}
	parts, err := s.store.ListParts(ctx, sourceKey(up.AssetID), up.MultipartID)
	if err != nil {
		return nil, err
	}
	out := make([]StoredPart, 0, len(parts))
	for _, p := range parts {
		out = append(out, StoredPart{PartNumber: p.Number, ETag: p.ETag, Size: p.Size})
	}
	return out, nil
}

// --- complete and cancel ---

// Complete finishes the upload, checks the stored size, moves the asset to
// processing and queues its transcode, all under the upload's row lock.
// Completing a completed upload returns the asset again.
func (s *Service) Complete(ctx context.Context, uploadID string, parts []storage.Part) (assets.View, error) {
	var view assets.View
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		up, err := q.LockUpload(ctx, uploadID)
		if err != nil {
			return err
		}
		switch up.Status {
		case "completed":
			view, err = s.assetView(ctx, q, up.AssetID)
			return err
		case "active":
		default:
			return errNotActive
		}
		if err := s.finishStorage(ctx, up, parts); err != nil {
			return err
		}
		view, err = s.markCompleted(ctx, tx, up)
		return err
	})
	return view, err
}

// finishStorage completes the multipart upload, or confirms that a previous
// attempt already did, and checks the stored size.
func (s *Service) finishStorage(ctx context.Context, up db.Upload, parts []storage.Part) error {
	key := sourceKey(up.AssetID)
	err := s.store.CompleteMultipart(ctx, key, up.MultipartID, parts)
	switch {
	case errors.Is(err, storage.ErrInvalidParts):
		return fail(http.StatusBadRequest, "invalid_parts", "some parts are missing or their ETags do not match; list the stored parts and upload the rest")
	case err != nil && !errors.Is(err, storage.ErrNotFound):
		return err
	}
	// ErrNotFound: storage already completed it (a crash before our commit),
	// or lost it. The source object tells which.
	obj, err := s.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return fail(http.StatusConflict, "upload_missing", "storage no longer has this upload")
	}
	if err != nil {
		return err
	}
	if obj.Size != up.DeclaredSize {
		return fail(http.StatusConflict, "size_mismatch", fmt.Sprintf("stored %d bytes, declared %d", obj.Size, up.DeclaredSize))
	}
	return nil
}

func (s *Service) markCompleted(ctx context.Context, tx pgx.Tx, up db.Upload) (assets.View, error) {
	q := db.New(tx)
	asset, err := q.LockAsset(ctx, up.AssetID)
	if err != nil {
		return assets.View{}, err
	}
	if asset.Status != "waiting_upload" {
		return assets.View{}, fail(http.StatusConflict, "asset_not_waiting", "the asset is no longer waiting for an upload")
	}
	if err := q.FinishUpload(ctx, db.FinishUploadParams{ID: up.ID, Status: "completed"}); err != nil {
		return assets.View{}, err
	}
	asset, err = q.MarkAssetProcessing(ctx, db.MarkAssetProcessingParams{
		ID: asset.ID, SourceUri: pgtype.Text{String: sourceKey(asset.ID), Valid: true},
	})
	if err != nil {
		return assets.View{}, err
	}
	if _, err := queue.Enqueue(ctx, tx, queue.Job{ProjectID: asset.ProjectID, AssetID: asset.ID, Kind: "transcode"}); err != nil {
		return assets.View{}, err
	}
	return assets.Load(ctx, q, asset, s.BaseURL)
}

// Cancel stops an active upload: storage is aborted and the asset errors
// with upload_cancelled. Cancelling twice is harmless.
func (s *Service) Cancel(ctx context.Context, uploadID string) (assets.View, error) {
	var view assets.View
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		up, err := q.LockUpload(ctx, uploadID)
		if err != nil {
			return err
		}
		switch up.Status {
		case "cancelled":
		case "active":
			if err := s.end(ctx, q, up, "cancelled", CodeUploadCancelled); err != nil {
				return err
			}
		default:
			return errNotActive
		}
		view, err = s.assetView(ctx, q, up.AssetID)
		return err
	})
	return view, err
}

// end aborts the storage upload and errors the asset, with an event.
func (s *Service) end(ctx context.Context, q *db.Queries, up db.Upload, status, code string) error {
	if err := s.store.AbortMultipart(ctx, sourceKey(up.AssetID), up.MultipartID); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := q.FinishUpload(ctx, db.FinishUploadParams{ID: up.ID, Status: status}); err != nil {
		return err
	}
	asset, err := q.LockAsset(ctx, up.AssetID)
	if err != nil {
		return err
	}
	if asset.Status != "waiting_upload" {
		return nil
	}
	if _, err := q.MarkAssetErrored(ctx, db.MarkAssetErroredParams{ID: asset.ID, ErrorCode: pgtype.Text{String: code, Valid: true}}); err != nil {
		return err
	}
	return outbox.AssetEvent(ctx, q, asset.ID, outbox.AssetErrored, s.BaseURL, nil)
}

func (s *Service) assetView(ctx context.Context, q *db.Queries, assetID string) (assets.View, error) {
	a, err := q.GetAssetByID(ctx, assetID)
	if err != nil {
		return assets.View{}, err
	}
	return assets.Load(ctx, q, a, s.BaseURL)
}

// --- sweeper ---

// Sweep checks active uploads, least recently active first. It completes
// those whose parts all landed, records new parts as activity, and expires
// those idle longer than IdleExpiry. It returns how many it completed or
// expired, and keeps going past an upload that fails.
func (s *Service) Sweep(ctx context.Context, limit int) (int, error) {
	active, err := db.New(s.pool).ListActiveUploads(ctx, int32(limit))
	if err != nil {
		return 0, err
	}
	handled := 0
	var firstErr error
	for _, id := range active {
		// Recorded outside the pass's transaction, so it sticks even when the
		// pass fails: a failing upload moves to the back of the line.
		if err := db.New(s.pool).MarkUploadSwept(ctx, id); err != nil {
			return handled, err
		}
		done, err := s.sweepOne(ctx, id)
		if err != nil {
			firstErr = cmpErr(firstErr, fmt.Errorf("sweep upload %s: %w", id, err))
			continue
		}
		if done {
			handled++
		}
	}
	return handled, firstErr
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

func (s *Service) sweepOne(ctx context.Context, id string) (bool, error) {
	done := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		up, err := q.LockUpload(ctx, id)
		if err != nil || up.Status != "active" {
			return err // cancelled, expired or completed since listing: never touch it
		}
		parts, err := s.store.ListParts(ctx, sourceKey(up.AssetID), up.MultipartID)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			// Storage finished it before a crash, or lost it.
			if err := s.finishStorage(ctx, up, nil); err != nil {
				done = true
				return s.end(ctx, q, up, "expired", CodeUploadExpired)
			}
			done = true
			_, err := s.markCompleted(ctx, tx, up)
			return err
		case err != nil:
			return err
		case allLanded(parts, partCount(up)):
			list := make([]storage.Part, len(parts))
			for i, p := range parts {
				list[i] = storage.Part{Number: p.Number, ETag: p.ETag}
			}
			if err := s.finishStorage(ctx, up, list); err != nil {
				return err
			}
			done = true
			_, err := s.markCompleted(ctx, tx, up)
			return err
		case len(parts) > int(up.PartsSeen):
			return q.SetUploadPartsSeen(ctx, db.SetUploadPartsSeenParams{ID: up.ID, PartsSeen: int32(len(parts))})
		}
		idle, err := q.UploadIdleSince(ctx, db.UploadIdleSinceParams{ID: up.ID, Seconds: s.IdleExpiry.Seconds()})
		if err != nil || !idle {
			return err
		}
		done = true
		return s.end(ctx, q, up, "expired", CodeUploadExpired)
	})
	return done, err
}

func allLanded(parts []storage.Part, want int) bool {
	if len(parts) != want {
		return false
	}
	for i, p := range parts {
		if p.Number != i+1 {
			return false
		}
	}
	return true
}

// --- HTTP ---

// Register adds the upload routes to mux.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/uploads", s.handleCreate)
	mux.HandleFunc("POST /v1/uploads/{id}/parts", s.handleSign)
	mux.HandleFunc("GET /v1/uploads/{id}/parts", s.handleList)
	mux.HandleFunc("POST /v1/uploads/{id}/complete", s.handleComplete)
	mux.HandleFunc("POST /v1/uploads/{id}/cancel", s.handleCancel)
	mux.HandleFunc("OPTIONS /v1/uploads/", func(w http.ResponseWriter, _ *http.Request) {
		cors(w)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusNoContent)
	})
}

// cors lets browsers on any origin call the upload endpoints: they carry an
// upload token, never a cookie.
func cors(w http.ResponseWriter) { w.Header().Set("Access-Control-Allow-Origin", "*") }

func (s *Service) respond(w http.ResponseWriter, op string, status int, v any, err error) {
	switch {
	case err == nil:
		httpjson.Write(w, status, v)
	case httpjson.WriteProblem(w, err):
	case errors.Is(err, storage.ErrTooLarge):
		httpjson.Error(w, http.StatusRequestEntityTooLarge, "too_large", "the upload is too large for this storage")
	default:
		s.Logger.Error("upload api", "op", op, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (s *Service) handleCreate(w http.ResponseWriter, r *http.Request) {
	cors(w)
	key, err := apikeys.Authenticate(r.Context(), s.pool, httpjson.Bearer(r))
	if err != nil {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
		return
	}
	if !key.Can(apikeys.UploadsWrite) {
		httpjson.Error(w, http.StatusForbidden, "insufficient_scope", "this key lacks uploads:write")
		return
	}
	var req CreateRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	out, err := s.Create(r.Context(), key, req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "create", http.StatusCreated, out, err)
}

// upload authenticates the upload token and checks it names the path's upload.
func (s *Service) upload(w http.ResponseWriter, r *http.Request) (db.Upload, bool) {
	up, err := db.New(s.pool).GetUploadByTokenHash(r.Context(), tokenHash(httpjson.Bearer(r)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "a valid upload token is required")
		} else {
			s.respond(w, "authenticate upload", 0, nil, err)
		}
		return db.Upload{}, false
	}
	if up.ID != r.PathValue("id") {
		httpjson.Error(w, http.StatusForbidden, "upload_mismatch", "the token belongs to another upload")
		return db.Upload{}, false
	}
	return up, true
}

func (s *Service) handleSign(w http.ResponseWriter, r *http.Request) {
	cors(w)
	up, ok := s.upload(w, r)
	if !ok {
		return
	}
	var req struct {
		PartNumbers []int `json:"part_numbers"`
	}
	if !httpjson.Decode(w, r, &req) {
		return
	}
	out, err := s.SignParts(r.Context(), up, req.PartNumbers)
	s.respond(w, "sign parts", http.StatusOK, out, err)
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	cors(w)
	up, ok := s.upload(w, r)
	if !ok {
		return
	}
	parts, err := s.ListParts(r.Context(), up)
	s.respond(w, "list parts", http.StatusOK, map[string]any{"parts": parts}, err)
}

func (s *Service) handleComplete(w http.ResponseWriter, r *http.Request) {
	cors(w)
	up, ok := s.upload(w, r)
	if !ok {
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int    `json:"part_number"`
			ETag       string `json:"etag"`
		} `json:"parts"`
	}
	if !httpjson.Decode(w, r, &req) {
		return
	}
	parts := make([]storage.Part, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = storage.Part{Number: p.PartNumber, ETag: p.ETag}
	}
	view, err := s.Complete(r.Context(), up.ID, parts)
	s.respond(w, "complete", http.StatusOK, view, err)
}

// handleCancel accepts the browser's upload token or the owning app's API key.
func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	cors(w)
	id := r.PathValue("id")
	q := db.New(s.pool)
	bearer := httpjson.Bearer(r)
	if up, err := q.GetUploadByTokenHash(r.Context(), tokenHash(bearer)); err == nil {
		if up.ID != id {
			httpjson.Error(w, http.StatusForbidden, "upload_mismatch", "the token belongs to another upload")
			return
		}
	} else {
		key, err := apikeys.Authenticate(r.Context(), s.pool, bearer)
		if err != nil {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "an upload token or API key is required")
			return
		}
		if !key.Can(apikeys.UploadsWrite) {
			httpjson.Error(w, http.StatusForbidden, "insufficient_scope", "this key lacks uploads:write")
			return
		}
		up, err := q.GetUpload(r.Context(), id)
		if err == nil {
			var asset db.Asset
			if asset, err = q.GetAssetByID(r.Context(), up.AssetID); err == nil && asset.AppID != key.AppID {
				err = pgx.ErrNoRows
			}
		}
		if errors.Is(err, pgx.ErrNoRows) {
			httpjson.Error(w, http.StatusNotFound, "not_found", "no such upload")
			return
		}
		if err != nil {
			s.respond(w, "cancel", 0, nil, err)
			return
		}
	}
	view, err := s.Cancel(r.Context(), id)
	s.respond(w, "cancel", http.StatusOK, view, err)
}
