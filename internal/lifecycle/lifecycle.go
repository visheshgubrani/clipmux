// Package lifecycle changes assets from the control plane
// (docs/architecture.md, "One mutating job per asset" and "Integration
// API"): re-encoding an asset, adding and revoking its playback IDs, and
// deleting it. Deleting an asset always wins over its other work and runs in
// this order:
//
//  1. In the request's transaction: cancel the asset's active upload and job
//     (which fences their attempts), mark the asset deleted, mark its
//     playback IDs revoking with an unpublish row each, and queue a delete
//     job.
//  2. The delete job, run by serve (never by a worker): wait until every
//     unpublish is confirmed, delete the asset's storage, then write
//     video.asset.deleted once.
//  3. A second storage pass after the PUT URLs signed for a cancelled job
//     have expired, so a stopped worker can't leave files behind.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/publicnet"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

var (
	errNotFound = errors.New("lifecycle: no such asset")
	// errChanged means the asset's active job changed between reading and
	// locking it; the transaction is retried.
	errChanged = errors.New("lifecycle: the asset's active job changed")
)

// Service changes assets and runs the control-plane jobs that delete them.
type Service struct {
	pool   *pgxpool.Pool
	stores *stores.Set
	routes *outputs.Committer

	// Auth accepts API keys, and the dashboard session once serve sets it.
	Auth *apikeys.Authenticator

	Logger *slog.Logger
	// Inline is how long DELETE runs the delete job itself before answering
	// 202; 0 leaves the job to Run.
	Inline time.Duration
	// Lease is how long a claimed control job is held before another pass
	// may take it.
	Lease time.Duration
	// RouteRetry is the wait while unpublishes are still pending.
	RouteRetry time.Duration
	// LateWrites is how long after a job ends its PUT URLs may still write
	// (15 min, plus a margin).
	LateWrites time.Duration

	// Fetch is the client imports use: public addresses only (publicnet).
	Fetch *http.Client
	// MaxImportSize caps an imported source, like a declared upload size.
	MaxImportSize int64
	// ImportTimeout bounds one fetch; ImportLease outlasts it.
	ImportTimeout, ImportLease time.Duration
}

// New returns the lifecycle service. routes publishes route changes.
func New(pool *pgxpool.Pool, set *stores.Set, routes *outputs.Committer) *Service {
	return &Service{
		pool: pool, stores: set, routes: routes, Auth: apikeys.NewAuthenticator(pool), Logger: slog.New(slog.DiscardHandler),
		Inline: 10 * time.Second, Lease: 2 * time.Minute, RouteRetry: 15 * time.Second, LateWrites: 16 * time.Minute,
		Fetch: publicnet.Client(publicnet.Options{}), MaxImportSize: storage.PartSize * storage.MaxParts,
		ImportTimeout: 6 * time.Hour, ImportLease: 6*time.Hour + 10*time.Minute,
	}
}

// Register adds the asset lifecycle routes.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/assets", s.handleImport)
	mux.HandleFunc("DELETE /v1/assets/{id}", s.handleDelete)
	mux.HandleFunc("POST /v1/assets/{id}/reencode", s.handleReencode)
	mux.HandleFunc("POST /v1/assets/{id}/tracks", s.handleRegenerateTrack)
	mux.HandleFunc("DELETE /v1/assets/{id}/tracks/{track}", s.handleDeleteTrack)
	mux.HandleFunc("POST /v1/assets/{id}/playback-ids", s.handleAddPlaybackID)
	mux.HandleFunc("DELETE /v1/assets/{id}/playback-ids/{pid}", s.handleRevokePlaybackID)
}

// handleDelete answers 200 once the asset's files are gone, or 202 while the
// delete job is still running. Either way the asset reads deleted and no
// longer plays.
func (s *Service) handleDelete(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	view, done, err := s.Delete(r.Context(), key, r.PathValue("id"))
	switch {
	case errors.Is(err, errNotFound):
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such asset")
	case err != nil:
		s.Logger.ErrorContext(r.Context(), "delete asset", "asset", r.PathValue("id"), "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	case done:
		httpjson.Write(w, http.StatusOK, view)
	default:
		httpjson.Write(w, http.StatusAccepted, view)
	}
}

// Delete deletes one of the key's app's assets. It reports whether the
// asset's files are already gone. Deleting a deleted asset changes nothing.
func (s *Service) Delete(ctx context.Context, key apikeys.Key, assetID string) (assets.View, bool, error) {
	err := errChanged
	for try := 0; errors.Is(err, errChanged) && try < 3; try++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error { return s.markDeleted(ctx, tx, key, assetID) })
	}
	if err != nil {
		return assets.View{}, false, err
	}
	q := db.New(s.pool)
	if s.Inline > 0 {
		if jobs, err := q.GetActiveJobForAsset(ctx, db.GetActiveJobForAssetParams{AssetID: assetID, Kinds: []string{"delete"}}); err == nil && len(jobs) == 1 {
			ictx, cancel := context.WithTimeout(ctx, s.Inline)
			_, err := s.runNext(ictx, pgtype.Text{String: jobs[0].ID, Valid: true})
			cancel()
			if err != nil {
				// Committed; Run finishes the job.
				s.Logger.Warn("delete continues in the background", "asset", assetID, "err", err)
			}
		}
	}
	asset, err := q.GetAssetByID(ctx, assetID)
	if err != nil {
		return assets.View{}, false, err
	}
	view, err := assets.Load(ctx, q, asset)
	return view, asset.DeletedAt.Valid, err
}

func (s *Service) markDeleted(ctx context.Context, tx pgx.Tx, key apikeys.Key, assetID string) error {
	q := db.New(tx)
	if _, err := q.GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID}); errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	} else if err != nil {
		return err
	}
	// Both families: the encode or import and the transcription stop. A
	// cancelled job's attempt is fenced: its next call is told to stop.
	up, jobs, asset, err := lockForChange(ctx, q, assetID, tracks.AllJobs)
	if err != nil || asset.Status == "deleted" {
		return err
	}
	if up != nil {
		// Its storage upload is aborted by the delete job.
		if err := q.FinishUpload(ctx, db.FinishUploadParams{ID: up.ID, Status: "cancelled"}); err != nil {
			return err
		}
	}
	for _, job := range jobs {
		if err := q.CancelJob(ctx, job.ID); err != nil {
			return err
		}
	}
	if asset, err = q.MarkAssetDeleted(ctx, assetID); err != nil {
		return err
	}
	if err := tracks.DeleteAll(ctx, q, assetID); err != nil {
		return err
	}
	revoking, err := q.MarkPlaybackIDsRevoking(ctx, assetID)
	if err != nil {
		return err
	}
	for _, pid := range revoking {
		if err := outputs.QueueUnpublish(ctx, q, asset, pid); err != nil {
			return err
		}
	}
	_, err = queue.Enqueue(ctx, tx, queue.Job{ProjectID: asset.ProjectID, AssetID: asset.ID, Kind: "delete"})
	return err
}

// lockForChange locks an asset's active upload, then its active jobs of
// the given kinds (one family, or both), then the asset itself: the order
// every other writer uses, so a change never deadlocks with a commit or an
// upload completing. Every writer that creates a job locks the asset first,
// so once it is locked the active jobs can't change; if they did while we
// waited, errChanged retries the transaction.
func lockForChange(ctx context.Context, q *db.Queries, assetID string, kinds []string) (*db.Upload, []db.Job, db.Asset, error) {
	var up *db.Upload
	switch u, err := q.LockActiveUploadForAsset(ctx, assetID); {
	case err == nil:
		up = &u
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, nil, db.Asset{}, err
	}
	active, err := q.GetActiveJobForAsset(ctx, db.GetActiveJobForAssetParams{AssetID: assetID, Kinds: kinds})
	if err != nil {
		return nil, nil, db.Asset{}, err
	}
	var jobs []db.Job
	for _, j := range active {
		if j, err = q.LockJob(ctx, j.ID); err != nil {
			return nil, nil, db.Asset{}, err
		}
		if j.Status == "queued" || j.Status == "running" {
			jobs = append(jobs, j)
		}
	}
	asset, err := q.LockAsset(ctx, assetID)
	if err != nil {
		return nil, nil, db.Asset{}, err
	}
	now, err := q.GetActiveJobForAsset(ctx, db.GetActiveJobForAssetParams{AssetID: assetID, Kinds: kinds})
	if err != nil {
		return nil, nil, db.Asset{}, err
	}
	if jobIDs(now) != jobIDs(jobs) {
		return nil, nil, db.Asset{}, errChanged
	}
	return up, jobs, asset, nil
}

// jobIDs names a set of jobs, in ID order, for comparing.
func jobIDs(jobs []db.Job) string {
	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	return strings.Join(ids, ",")
}

// Run takes due control-plane jobs one at a time, up to ten per pass, and
// returns how many it ran. A job that fails keeps its lease and is retried
// once the lease passes.
func (s *Service) Run(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	for range 10 {
		ran, err := s.runNext(ctx, pgtype.Text{})
		if err != nil {
			errs = append(errs, err)
		}
		if !ran {
			break
		}
		n++
	}
	return n, errors.Join(errs...)
}

// controlKinds are the jobs this loop runs; chapters have a loop of their
// own (internal/chapters), so a slow model never holds up a delete.
var controlKinds = []string{"delete", "move"}

// runNext claims a due control job (the given one, if set) and runs it. It
// reports whether there was one.
func (s *Service) runNext(ctx context.Context, only pgtype.Text) (bool, error) {
	job, err := db.New(s.pool).ClaimControlJob(ctx, db.ClaimControlJobParams{LeaseSeconds: s.Lease.Seconds(), JobID: only, Kinds: controlKinds})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim control job: %w", err)
	}
	var run func(context.Context, db.Job) error
	switch job.Kind {
	case "delete":
		run = s.runDelete
	case "move":
		run = s.runMove
	default:
		return true, fmt.Errorf("control job %s has kind %q, which serve doesn't run", job.ID, job.Kind)
	}
	if err := run(ctx, job); err != nil {
		return true, fmt.Errorf("%s job %s: %w", job.Kind, job.ID, err)
	}
	return true, nil
}

func sourceKey(assetID string) string { return "uploads/" + assetID + "/source" }

// runDelete is one pass of a delete job. Every step is safe to repeat.
func (s *Service) runDelete(ctx context.Context, job db.Job) error {
	q := db.New(s.pool)
	if _, err := s.routes.SyncRoutes(ctx, 1000); err != nil {
		return err
	}
	pending, err := q.CountPendingRoutesForAsset(ctx, pgtype.Text{String: job.AssetID, Valid: true})
	if err != nil {
		return err
	}
	if pending > 0 {
		// Files are deleted only once no route can lead to them.
		return q.DeferJob(ctx, db.DeferJobParams{ID: job.ID, DelaySeconds: s.RouteRetry.Seconds()})
	}
	ups, err := q.ListUploadsForAsset(ctx, job.AssetID)
	if err != nil {
		return err
	}
	store, err := s.stores.ForAsset(ctx, job.AssetID)
	if err != nil {
		return err
	}
	for _, up := range ups {
		// Multipart staging lives outside the asset's prefixes.
		if err := store.AbortMultipart(ctx, sourceKey(job.AssetID), up.MultipartID); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	for _, prefix := range []string{"uploads/" + job.AssetID + "/", "media/" + job.AssetID + "/"} {
		if err := store.DeletePrefix(ctx, prefix); err != nil {
			return err
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		if _, err := qs.LockJob(ctx, job.ID); err != nil {
			return err
		}
		if _, err := qs.LockAsset(ctx, job.AssetID); err != nil {
			return err
		}
		first, err := qs.MarkAssetFilesDeleted(ctx, job.AssetID)
		if err != nil {
			return err
		}
		if first == 1 {
			if err := outbox.AssetEvent(ctx, qs, job.AssetID, outbox.AssetDeleted, nil); err != nil {
				return err
			}
		}
		late, err := qs.LateWritesUntil(ctx, db.LateWritesUntilParams{AssetID: job.AssetID, WindowSeconds: s.LateWrites.Seconds()})
		if err != nil {
			return err
		}
		if late.Pending {
			return qs.DeferJob(ctx, db.DeferJobParams{ID: job.ID, DelaySeconds: late.WaitSeconds})
		}
		return qs.SucceedJob(ctx, job.ID)
	})
}
