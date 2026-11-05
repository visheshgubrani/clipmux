package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// A move copies an asset's source and every output a viewer may still be
// pinned to (committed or retired) from one store to another, switches the
// asset to the new store, republishes its routes with the new backend, and
// deletes the old copies only after the Worker's route cache has expired
// (docs/architecture.md, "Per-asset storage"). Encrypted outputs move as
// they are: their keys depend only on IDs. Every pass is safe to repeat.

// moveCleanupDelay outlasts the Worker's 60 s route cache, so no edge still
// sends viewers to the old store when its files go.
const moveCleanupDelay = 2 * time.Minute

// singlePutLimit is the largest object copied with one PUT (S3's limit);
// larger ones go up in parts. Tests lower it.
var singlePutLimit int64 = 5 << 30

// QueueMove queues a move of one asset to the target store. It fails with
// the unique-index violation when the asset already has an active job.
func QueueMove(ctx context.Context, q *db.Queries, projectID, assetID, source, target string) (db.Job, error) {
	return q.QueueMoveJob(ctx, db.QueueMoveJobParams{
		ID: ids.New(ids.Job), ProjectID: projectID, AssetID: assetID,
		SourceStorageID: pgtype.Text{String: source, Valid: true}, TargetStorageID: pgtype.Text{String: target, Valid: true},
	})
}

// MoveLibrary queues a move to target for every live asset elsewhere that
// has no active job, and returns how many it queued.
func (s *Service) MoveLibrary(ctx context.Context, projectID, target string) (int, error) {
	q := db.New(s.pool)
	todo, err := q.ListAssetsToMove(ctx, db.ListAssetsToMoveParams{ProjectID: projectID, Target: target})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range todo {
		if _, err := QueueMove(ctx, q, projectID, a.ID, a.StorageID, target); err != nil {
			if isUniqueViolation(err) {
				continue // a job started since the listing; move it later
			}
			return n, err
		}
		n++
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

func (s *Service) runMove(ctx context.Context, job db.Job) error {
	q := db.New(s.pool)
	asset, err := q.GetAssetByID(ctx, job.AssetID)
	if err != nil {
		return err
	}
	from, to := job.SourceStorageID.String, job.TargetStorageID.String
	if job.Phase.String == "cleanup" {
		return s.moveCleanup(ctx, job, from)
	}
	if asset.Status == "deleted" || asset.StorageID != from {
		// Deleted, or already moved by an earlier pass that crashed before
		// recording its phase.
		if asset.StorageID == to {
			return q.SetJobPhase(ctx, db.SetJobPhaseParams{ID: job.ID, Phase: pgtype.Text{String: "cleanup", Valid: true}, DelaySeconds: moveCleanupDelay.Seconds()})
		}
		return q.SucceedJob(ctx, job.ID)
	}
	// PUT URLs signed for earlier jobs may still write into the old store.
	late, err := q.LateWritesUntil(ctx, db.LateWritesUntilParams{AssetID: job.AssetID, WindowSeconds: s.LateWrites.Seconds()})
	if err != nil {
		return err
	}
	if late.Pending {
		return q.DeferJob(ctx, db.DeferJobParams{ID: job.ID, DelaySeconds: late.WaitSeconds})
	}
	src, err := s.stores.Get(ctx, from)
	if err != nil {
		return err
	}
	dst, err := s.stores.Get(ctx, to)
	if err != nil {
		return err
	}
	outs, err := q.ListServableOutputIDs(ctx, job.AssetID)
	if err != nil {
		return err
	}
	prefixes := []string{"uploads/" + job.AssetID + "/"}
	for _, o := range outs {
		prefixes = append(prefixes, "media/"+job.AssetID+"/"+o+"/")
	}
	want, err := listAll(ctx, src, prefixes)
	if err != nil {
		return err
	}
	for key, size := range want {
		if err := copyObject(ctx, src, dst, key, size); err != nil {
			return fmt.Errorf("copy %s: %w", key, err)
		}
	}
	got, err := listAll(ctx, dst, prefixes)
	if err != nil {
		return err
	}
	for key, size := range want {
		if got[key] != size {
			return fmt.Errorf("verify %s: %d bytes in the new store, want %d", key, got[key], size)
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		j, err := qs.LockJob(ctx, job.ID)
		if err != nil || j.Status != "running" {
			return err // cancelled (the asset is being deleted): leave it
		}
		if _, err := qs.LockAsset(ctx, job.AssetID); err != nil {
			return err
		}
		n, err := qs.MoveAssetStorage(ctx, db.MoveAssetStorageParams{ID: job.AssetID, Source: from, Target: to})
		if err != nil || n == 0 {
			return err
		}
		// Every route now points at the new store (backend r2 or origin).
		if err := routes.QueueAssetRefresh(ctx, qs, asset.ProjectID, asset.AppID, job.AssetID); err != nil {
			return err
		}
		if err := outbox.AssetEvent(ctx, qs, job.AssetID, outbox.AssetUpdated, &outbox.Job{ID: job.ID, Kind: "move"}); err != nil {
			return err
		}
		return qs.SetJobPhase(ctx, db.SetJobPhaseParams{ID: job.ID, Phase: pgtype.Text{String: "cleanup", Valid: true}, DelaySeconds: moveCleanupDelay.Seconds()})
	})
}

// moveCleanup deletes the old copies once every route of the asset has been
// republished and the Worker's cache has expired.
func (s *Service) moveCleanup(ctx context.Context, job db.Job, from string) error {
	q := db.New(s.pool)
	if _, err := s.routes.SyncRoutes(ctx, 1000); err != nil {
		return err
	}
	pending, err := q.CountPendingRouteSyncsForAsset(ctx, pgtype.Text{String: job.AssetID, Valid: true})
	if err != nil {
		return err
	}
	if pending > 0 {
		return q.SetJobPhase(ctx, db.SetJobPhaseParams{ID: job.ID, Phase: pgtype.Text{String: "cleanup", Valid: true}, DelaySeconds: moveCleanupDelay.Seconds()})
	}
	old, err := s.stores.Get(ctx, from)
	if err != nil {
		return err
	}
	for _, prefix := range []string{"uploads/" + job.AssetID + "/", "media/" + job.AssetID + "/"} {
		if err := old.DeletePrefix(ctx, prefix); err != nil {
			return err
		}
	}
	return q.SucceedJob(ctx, job.ID)
}

func listAll(ctx context.Context, st storage.Storage, prefixes []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, p := range prefixes {
		for cursor := ""; ; {
			objs, next, err := st.List(ctx, p, cursor)
			if err != nil {
				return nil, err
			}
			for _, o := range objs {
				out[o.Key] = o.Size
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return out, nil
}

// copyObject copies one object unless the target already holds it at the
// same size (a resumed pass).
func copyObject(ctx context.Context, src, dst storage.Storage, key string, size int64) error {
	if obj, err := dst.Head(ctx, key); err == nil && obj.Size == size {
		return nil
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	rc, err := src.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	// Adapters sign uploads over a seekable body; spool a stream to disk.
	body, ok := rc.(interface {
		io.ReadSeeker
		io.ReaderAt
	})
	if !ok {
		f, err := os.CreateTemp("", "clipmux-move-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if _, err := io.Copy(f, rc); err != nil {
			return err
		}
		body = f
	}
	if size <= singlePutLimit {
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return dst.Put(ctx, key, body)
	}
	return putParts(ctx, dst, key, size, body)
}

// putParts uploads a large object through the store's multipart upload.
func putParts(ctx context.Context, dst storage.Storage, key string, size int64, body io.ReaderAt) error {
	mp, err := dst.CreateMultipart(ctx, key, size)
	if err != nil {
		return err
	}
	var parts []storage.Part
	for n, off := 1, int64(0); off < size; n, off = n+1, off+mp.PartSize {
		length := min(mp.PartSize, size-off)
		url, err := dst.PresignPart(ctx, key, mp.UploadID, n, 15*time.Minute)
		if err == nil {
			var etag string
			etag, err = putPart(ctx, url, io.NewSectionReader(body, off, length), length)
			parts = append(parts, storage.Part{Number: n, ETag: etag, Size: length})
		}
		if err != nil {
			_ = dst.AbortMultipart(context.WithoutCancel(ctx), key, mp.UploadID)
			return err
		}
	}
	return dst.CompleteMultipart(ctx, key, mp.UploadID, parts)
}

func putPart(ctx context.Context, url string, body io.Reader, length int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = length
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// The URL is a credential; the error names only the failure.
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) {
			return "", fmt.Errorf("put part: %w", ue.Unwrap())
		}
		return "", errors.New("put part failed")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("put part: HTTP %d", res.StatusCode)
	}
	return strings.Trim(res.Header.Get("ETag"), `"`), nil
}
