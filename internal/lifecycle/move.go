package lifecycle

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// A move puts an asset where the settings say its parts belong (D117): its
// source on the uploads default, and every output a viewer may still be
// pinned to (committed or retired) and its tracks on the media target. It
// copies each part that isn't there yet, switches the asset, republishes
// its routes with the new backend, and deletes the old copies only after
// the Worker's route cache has expired (docs/architecture.md, "Per-asset
// storage"). Encrypted outputs move as they are: their keys depend only on
// IDs. Every pass is safe to repeat.

// moveCleanupDelay outlasts the Worker's 60 s route cache, so no edge still
// sends viewers to the old store when its files go.
const moveCleanupDelay = 2 * time.Minute

// Move is one move's two parts: the source from Source to Target, the
// encoded files from MediaSource to MediaTarget. A part already on its
// target stays put.
type Move struct {
	Source, Target           string
	MediaSource, MediaTarget string
}

// QueueMove queues a move of one asset. It fails with the unique-index
// violation when the asset already has an active job.
func QueueMove(ctx context.Context, q *db.Queries, projectID, assetID string, m Move) (db.Job, error) {
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	return q.QueueMoveJob(ctx, db.QueueMoveJobParams{
		ID: ids.New(ids.Job), ProjectID: projectID, AssetID: assetID,
		Source: text(m.Source), Target: text(m.Target), MediaSource: text(m.MediaSource), MediaTarget: text(m.MediaTarget),
	})
}

// MoveLibrary queues a move for every live asset with a part away from its
// target and no active job, and returns how many it queued.
func (s *Service) MoveLibrary(ctx context.Context, projectID string) (int, error) {
	q := db.New(s.pool)
	def, err := q.GetDefaultStorage(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("default storage: %w", err)
	}
	media, err := q.GetMediaTarget(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("media target: %w", err)
	}
	todo, err := q.ListAssetsToMove(ctx, db.ListAssetsToMoveParams{ProjectID: projectID, Target: def.ID, MediaTarget: media})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range todo {
		m := Move{Source: a.StorageID, Target: def.ID, MediaSource: a.MediaStorageID, MediaTarget: media}
		if _, err := QueueMove(ctx, q, projectID, a.ID, m); err != nil {
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

// moveOf reads a move job's parts. A job with no media stores, queued
// before D117, moves the encoded files with the source.
func moveOf(job db.Job) Move {
	return Move{
		Source: job.SourceStorageID.String, Target: job.TargetStorageID.String,
		MediaSource: cmp.Or(job.SourceMediaStorageID.String, job.SourceStorageID.String),
		MediaTarget: cmp.Or(job.TargetMediaStorageID.String, job.TargetStorageID.String),
	}
}

func (s *Service) runMove(ctx context.Context, job db.Job) error {
	q := db.New(s.pool)
	asset, err := q.GetAssetByID(ctx, job.AssetID)
	if err != nil {
		return err
	}
	m := moveOf(job)
	if job.Phase.String == "cleanup" {
		return s.moveCleanup(ctx, job, m)
	}
	if asset.Status == "deleted" || asset.StorageID != m.Source || asset.MediaStorageID != m.MediaSource {
		// Deleted, or already moved by an earlier pass that crashed before
		// recording its phase.
		if asset.StorageID == m.Target && asset.MediaStorageID == m.MediaTarget {
			return q.SetJobPhase(ctx, db.SetJobPhaseParams{ID: job.ID, Phase: pgtype.Text{String: "cleanup", Valid: true}, DelaySeconds: moveCleanupDelay.Seconds()})
		}
		return q.SucceedJob(ctx, job.ID)
	}
	// PUT URLs signed for earlier jobs may still be valid. They can only add
	// files no manifest names, so they can't change what is copied; cleanup
	// waits them out so none lands after the old copies are deleted.
	if m.Source != m.Target {
		if err := s.copyPart(ctx, m.Source, m.Target, []string{"uploads/" + job.AssetID + "/"}); err != nil {
			return err
		}
	}
	if m.MediaSource != m.MediaTarget {
		outs, err := q.ListServableOutputIDs(ctx, job.AssetID)
		if err != nil {
			return err
		}
		// Track files sit apart from outputs (tracks.StorageKey); only tracks
		// that can be served have one, since a purge deletes its file.
		prefixes := []string{"media/" + job.AssetID + "/tracks/"}
		for _, o := range outs {
			prefixes = append(prefixes, "media/"+job.AssetID+"/"+o+"/")
		}
		if err := s.copyPart(ctx, m.MediaSource, m.MediaTarget, prefixes); err != nil {
			return err
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
		n, err := qs.MoveAssetStorage(ctx, db.MoveAssetStorageParams{
			ID: job.AssetID, Source: m.Source, Target: m.Target, MediaSource: m.MediaSource, MediaTarget: m.MediaTarget,
		})
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

// copyPart copies every object under prefixes from one store to another,
// then checks each arrived whole.
func (s *Service) copyPart(ctx context.Context, from, to string, prefixes []string) error {
	src, err := s.stores.Get(ctx, from)
	if err != nil {
		return err
	}
	dst, err := s.stores.Get(ctx, to)
	if err != nil {
		return err
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
	return nil
}

// moveCleanup deletes the old copies once every route of the asset has been
// republished and the Worker's cache has expired: each part's prefix, from
// the store that part left.
func (s *Service) moveCleanup(ctx context.Context, job db.Job, m Move) error {
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
	late, err := q.LateWritesUntil(ctx, db.LateWritesUntilParams{AssetID: job.AssetID, WindowSeconds: s.LateWrites.Seconds()})
	if err != nil {
		return err
	}
	if late.Pending {
		return q.SetJobPhase(ctx, db.SetJobPhaseParams{ID: job.ID, Phase: pgtype.Text{String: "cleanup", Valid: true}, DelaySeconds: late.WaitSeconds})
	}
	for _, part := range []struct{ from, to, prefix string }{
		{m.Source, m.Target, "uploads/" + job.AssetID + "/"},
		{m.MediaSource, m.MediaTarget, "media/" + job.AssetID + "/"},
	} {
		if part.from == part.to {
			continue
		}
		old, err := s.stores.Get(ctx, part.from)
		if err != nil {
			return err
		}
		if err := old.DeletePrefix(ctx, part.prefix); err != nil {
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
// same size (a resumed pass). Put streams it, in parts when it is large.
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
	return dst.Put(ctx, key, rc)
}
