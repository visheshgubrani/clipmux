package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/idempotency"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/publicnet"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// Import from URL (docs/architecture.md, "Integration API"). serve fetches
// the URL itself, through publicnet, and streams it into the asset's store
// as its source; a transcode job then runs as for an upload. Workers never
// fetch arbitrary URLs.

// ImportRequest is POST /v1/assets.
type ImportRequest struct {
	URL string `json:"url,omitempty"`
	// AgentSourceID attaches a file a PC agent registered for push --local.
	AgentSourceID  string `json:"agent_source_id,omitempty"`
	PlaybackPolicy string `json:"playback_policy,omitempty"`
	Passthrough    string `json:"passthrough,omitempty"`
}

// Import failure codes on the asset.
const (
	// The URL, or an address it led to, isn't public http(s): permanent.
	codeImportRefused = "import_url_refused"
	// The fetch kept failing, or the server said no: permanent.
	codeImportFailed = "import_failed"
	// The source exceeds the upload limit: permanent.
	codeImportTooLarge = "import_too_large"
)

// importRetries is how many transient failures an import absorbs.
const importRetries = 3

// Import creates a processing asset and its import job.
func (s *Service) Import(ctx context.Context, key apikeys.Key, req ImportRequest, idemKey string) (assets.View, error) {
	if req.AgentSourceID != "" {
		return s.AttachLocal(ctx, key, req, idemKey)
	}
	if req.PlaybackPolicy == "" {
		req.PlaybackPolicy = "signed"
	}
	if _, err := publicnet.CheckURL(req.URL); err != nil {
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, codeImportRefused, "url must be a public http or https URL without credentials")
	}
	switch {
	case req.PlaybackPolicy != "signed" && req.PlaybackPolicy != "public":
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", `playback_policy must be "signed" or "public"`)
	case len(req.Passthrough) > 255:
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "passthrough is limited to 255 bytes")
	}
	body, _ := json.Marshal(req)
	var out assets.View
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets", body)
		if err != nil {
			return idempotency.Problem(err)
		}
		if claim != nil && claim.Replay != nil {
			return json.Unmarshal(claim.Replay, &out)
		}
		st, err := q.GetDefaultStorage(ctx, key.ProjectID)
		if err != nil {
			return err
		}
		asset, err := q.CreateImportedAsset(ctx, db.CreateImportedAssetParams{
			ID: ids.New(ids.Asset), ProjectID: key.ProjectID, AppID: key.AppID, StorageID: st.ID,
			Passthrough: pgtype.Text{String: req.Passthrough, Valid: req.Passthrough != ""},
		})
		if err != nil {
			return err
		}
		if _, err := q.CreatePlaybackID(ctx, db.CreatePlaybackIDParams{
			ID: ids.New(ids.PlaybackID), ProjectID: key.ProjectID, AssetID: asset.ID, Policy: req.PlaybackPolicy,
		}); err != nil {
			return err
		}
		if _, err := q.CreateImportJob(ctx, db.CreateImportJobParams{
			ID: ids.New(ids.Job), ProjectID: key.ProjectID, AssetID: asset.ID, ImportUrl: pgtype.Text{String: req.URL, Valid: true},
		}); err != nil {
			return err
		}
		if out, err = assets.Load(ctx, q, asset); err != nil {
			return err
		}
		stored, _ := json.Marshal(out)
		return claim.Save(ctx, http.StatusCreated, stored)
	})
	return out, err
}

func (s *Service) handleImport(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var req ImportRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	view, err := s.Import(r.Context(), key, req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "import", http.StatusCreated, view, err)
}

// RunImports fetches due imports one at a time, up to four per pass.
func (s *Service) RunImports(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	for range 4 {
		job, err := db.New(s.pool).ClaimImportJob(ctx, s.ImportLease.Seconds())
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("claim import: %w", err)
		}
		if err := s.runImport(ctx, job); err != nil {
			errs = append(errs, fmt.Errorf("import job %s: %w", job.ID, err))
		}
		n++
	}
	return n, errors.Join(errs...)
}

// importFailure is a fetch that failed; permanent ones end the import.
type importFailure struct {
	code      string
	permanent bool
	cause     error
}

func (f *importFailure) Error() string { return f.code + ": " + f.cause.Error() }

func (s *Service) runImport(ctx context.Context, job db.Job) error {
	q := db.New(s.pool)
	store, err := s.stores.ForAsset(ctx, job.AssetID)
	if err != nil {
		return err
	}
	key := sourceKey(job.AssetID)
	if fail := s.fetch(ctx, store, job.ImportUrl.String, key); fail != nil {
		// Only the host: the URL's query may be a signature the app handed us.
		s.Logger.Warn("import failed", "job", job.ID, "host", logging.URLHost(job.ImportUrl.String), "code", fail.code,
			"permanent", fail.permanent, "err", logging.RedactError(fail.cause))
		if !fail.permanent && job.FailedCount+1 < importRetries {
			delay := 30 * math.Pow(2, float64(job.FailedCount))
			return q.RetryImportJob(ctx, db.RetryImportJobParams{ID: job.ID, ErrorCode: pgtype.Text{String: fail.code, Valid: true}, DelaySeconds: delay})
		}
		code := fail.code
		if !fail.permanent {
			code = codeImportFailed
		}
		return s.failImport(ctx, job, code)
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		j, err := qs.LockJob(ctx, job.ID)
		if err != nil || j.Status != "running" {
			return err // cancelled (deleted meanwhile)
		}
		if err := qs.SetAssetSource(ctx, db.SetAssetSourceParams{ID: job.AssetID, SourceUri: pgtype.Text{String: key, Valid: true}}); err != nil {
			return err
		}
		if err := qs.SucceedJob(ctx, job.ID); err != nil {
			return err
		}
		_, err = queue.Enqueue(ctx, tx, queue.Job{ProjectID: job.ProjectID, AssetID: job.AssetID, Kind: "transcode"})
		return err
	})
}

// fetch streams the URL into key, within the upload size limit.
func (s *Service) fetch(ctx context.Context, store storage.Storage, rawURL, key string) *importFailure {
	ctx, cancel := context.WithTimeout(ctx, s.ImportTimeout)
	defer cancel()
	res, err := publicnet.Get(ctx, s.Fetch, rawURL)
	if errors.Is(err, publicnet.ErrRefused) {
		return &importFailure{codeImportRefused, true, err}
	}
	if err != nil {
		return &importFailure{codeImportFailed, false, err}
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusRequestTimeout:
		return &importFailure{codeImportFailed, false, fmt.Errorf("HTTP %d", res.StatusCode)}
	case res.StatusCode != http.StatusOK:
		return &importFailure{codeImportFailed, true, fmt.Errorf("HTTP %d", res.StatusCode)}
	case res.ContentLength > s.MaxImportSize:
		return &importFailure{codeImportTooLarge, true, fmt.Errorf("%d bytes", res.ContentLength)}
	}
	body := &countingReader{r: io.LimitReader(res.Body, s.MaxImportSize+1)}
	if err := store.Put(ctx, key, body); err != nil {
		return &importFailure{codeImportFailed, false, err}
	}
	switch {
	case body.n > s.MaxImportSize:
		_ = store.Delete(context.WithoutCancel(ctx), key)
		return &importFailure{codeImportTooLarge, true, fmt.Errorf("more than %d bytes", s.MaxImportSize)}
	case body.n == 0:
		_ = store.Delete(context.WithoutCancel(ctx), key)
		return &importFailure{codeImportFailed, true, errors.New("empty body")}
	case res.ContentLength >= 0 && body.n != res.ContentLength:
		return &importFailure{codeImportFailed, false, fmt.Errorf("got %d of %d bytes", body.n, res.ContentLength)}
	}
	return nil
}

func (s *Service) failImport(ctx context.Context, job db.Job, code string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		j, err := qs.LockJob(ctx, job.ID)
		if err != nil || j.Status != "running" {
			return err
		}
		if err := qs.FailJob(ctx, db.FailJobParams{ID: job.ID, ErrorCode: pgtype.Text{String: code, Valid: true}}); err != nil {
			return err
		}
		if _, err := qs.LockAsset(ctx, job.AssetID); err != nil {
			return err
		}
		if _, err := qs.MarkAssetErrored(ctx, db.MarkAssetErroredParams{ID: job.AssetID, ErrorCode: pgtype.Text{String: code, Valid: true}}); err != nil {
			return err
		}
		return outbox.AssetEvent(ctx, qs, job.AssetID, outbox.AssetErrored, &outbox.Job{ID: job.ID, Kind: "import", ErrorCode: code})
	})
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
