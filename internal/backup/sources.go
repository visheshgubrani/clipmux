package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// The source mirror (T29.7, D146). Each committed source is copied, encrypted
// to the same identity as the database dumps, to sources/{asset}.age on the
// target. Outputs are not copied: they can be made again from the sources. A
// deleted asset leaves sources/{asset}.deleted, so a restore knows the delete
// was meant.
const (
	// SourceBatch is how many sources one pass copies, oldest first.
	SourceBatch = 20
	// SourceRetry is how long an asset whose copy failed is left alone, so one
	// broken source doesn't hold up the ones behind it.
	SourceRetry = time.Hour
	// TombstoneGrace is how long a deleted asset's copy is kept before pruning
	// removes it: long enough to notice a delete made by mistake.
	TombstoneGrace = 30 * 24 * time.Hour
	// pruneEvery is how often the loop prunes, where the key may delete.
	pruneEvery = 24 * time.Hour
)

func sourceObject(assetID string) string { return "sources/" + assetID + ".age" }
func tombstoneObject(assetID string) string {
	return "sources/" + assetID + ".deleted"
}

// Tombstone is the body of sources/{asset}.deleted.
type Tombstone struct {
	AssetID   string    `json:"asset_id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// putEncrypted streams src through age into the target, so no plaintext
// reaches it. A source that fails part way aborts the upload, so no truncated
// object is left looking whole.
func (s *Service) putEncrypted(ctx context.Context, key string, src io.Reader, recips []age.Recipient) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		enc, err := age.Encrypt(pw, recips...)
		if err == nil {
			_, err = io.Copy(enc, src)
		}
		if err == nil {
			err = enc.Close()
		}
		pw.CloseWithError(err) // nil closes it cleanly
		done <- err
	}()
	putErr := s.Target.Put(ctx, key, pr)
	pr.CloseWithError(putErr) // stop a copy nobody is reading
	copyErr := <-done
	switch {
	case copyErr != nil && (putErr == nil || errors.Is(putErr, copyErr)):
		return fmt.Errorf("read the source: %w", copyErr)
	case putErr != nil:
		return fmt.Errorf("write to %s: %w", s.Target.Desc, putErr)
	}
	return nil
}

// RunSources is the leader loop's pass: it writes the tombstones owed, copies
// up to SourceBatch sources not yet copied, and (once a day, where the key may
// delete) prunes the copies of long-deleted assets. It returns how many
// objects it wrote. An asset whose copy fails is skipped for SourceRetry and
// the pass carries on with the next.
func (s *Service) RunSources(ctx context.Context) (int, error) {
	if s.Stores == nil {
		return 0, nil
	}
	q := db.New(s.Pool)
	n := 0
	var errs []error

	toTomb, err := q.ListSourcesToTombstone(ctx, SourceBatch)
	if err != nil {
		return 0, fmt.Errorf("list the deleted assets to tombstone: %w", err)
	}
	for _, id := range toTomb {
		body, _ := json.Marshal(Tombstone{AssetID: id, DeletedAt: s.Now().UTC()}) // plain struct; can't fail
		if err := s.Target.Put(ctx, tombstoneObject(id), strings.NewReader(string(body))); err != nil {
			errs = append(errs, fmt.Errorf("tombstone %s: %w", id, err))
			continue
		}
		if err := q.MarkSourceTombstoned(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("record the tombstone of %s: %w", id, err))
			continue
		}
		n++
	}

	skip := s.skippedSources()
	todo, err := q.ListSourcesToMirror(ctx, db.ListSourcesToMirrorParams{Exclude: skip, MaxRows: SourceBatch})
	if err != nil {
		return n, errors.Join(append(errs, fmt.Errorf("list the sources to copy: %w", err))...)
	}
	if len(todo) > 0 {
		recips, err := recipients(s.Box, s.Config.Recipients)
		if err != nil {
			return n, errors.Join(append(errs, err)...)
		}
		for _, a := range todo {
			if ctx.Err() != nil {
				break
			}
			switch err := s.mirrorSource(ctx, q, a.ID, a.SourceUri.String, recips); {
			case err == nil:
				n++
			case errors.Is(err, storage.ErrNotFound):
				// Not a failure of the backup: the source is gone from the store.
				s.sourceFailed[a.ID] = s.Now()
				s.Logger.Warn("a source to back up is missing from its store", "asset", a.ID)
			default:
				s.sourceFailed[a.ID] = s.Now()
				errs = append(errs, fmt.Errorf("copy the source of %s: %w", a.ID, err))
			}
		}
	}

	if s.Now().Sub(s.sourcesPrunedAt) >= pruneEvery {
		s.sourcesPrunedAt = s.Now()
		can, err := s.targetCanDelete(ctx)
		if err == nil && can {
			var pruned int
			if pruned, err = s.PruneSources(ctx, TombstoneGrace); pruned > 0 {
				s.Logger.Info("pruned the copies of deleted assets' sources", "count", pruned)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return n, errors.Join(errs...)
}

// skippedSources lists the assets whose copy failed within SourceRetry,
// forgetting the older ones.
func (s *Service) skippedSources() []string {
	if s.sourceFailed == nil {
		s.sourceFailed = map[string]time.Time{}
	}
	skip := []string{}
	for id, at := range s.sourceFailed {
		if s.Now().Sub(at) >= SourceRetry {
			delete(s.sourceFailed, id)
			continue
		}
		skip = append(skip, id)
	}
	slices.Sort(skip)
	return skip
}

func (s *Service) mirrorSource(ctx context.Context, q *db.Queries, assetID, uri string, recips []age.Recipient) error {
	store, err := s.Stores.SourceFor(ctx, assetID)
	if err != nil {
		return err
	}
	src, err := store.Get(ctx, uri)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := s.putEncrypted(ctx, sourceObject(assetID), src, recips); err != nil {
		return err
	}
	return q.MarkSourceBackedUp(ctx, assetID)
}

// targetCanDelete is whether ClipMux may delete from the target (Probe.Prunes),
// asked of it once and remembered for as long as this process runs.
func (s *Service) targetCanDelete(ctx context.Context) (bool, error) {
	if s.canDelete.Load() == 0 {
		p, err := s.Target.Probe(ctx)
		if err != nil {
			return false, err
		}
		s.noteProbe(p)
	}
	return s.canDelete.Load() == 1, nil
}

// ErrNotPrunable is returned by PruneSources on a target whose key can't
// delete: its lifecycle rules remove old copies instead (D146).
var ErrNotPrunable = errors.New("the backup target is immutable to this server (its key can't delete, or its bucket has Object Lock), so its lifecycle rules remove old source copies")

// PruneSources deletes the copies of assets deleted at least grace ago, and
// their tombstones, and returns how many assets that was. It needs a key that
// may delete.
func (s *Service) PruneSources(ctx context.Context, grace time.Duration) (int, error) {
	can, err := s.targetCanDelete(ctx)
	if err != nil {
		return 0, err
	}
	if !can {
		return 0, ErrNotPrunable
	}
	objs, err := s.Target.list(ctx, "sources/")
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, o := range objs {
		id, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, "sources/"), ".deleted")
		if !ok {
			continue
		}
		t, err := s.readTombstone(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if s.Now().Sub(t.DeletedAt) < grace {
			continue
		}
		// The copy first: a tombstone with no copy is harmless, a copy with no
		// tombstone would be restored.
		if err := s.Target.Delete(ctx, sourceObject(id)); err != nil {
			errs = append(errs, fmt.Errorf("prune the source of %s: %w", id, err))
			continue
		}
		if err := s.Target.Delete(ctx, tombstoneObject(id)); err != nil {
			errs = append(errs, fmt.Errorf("prune the tombstone of %s: %w", id, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// readTombstone reads sources/{asset}.deleted; it returns storage.ErrNotFound
// when there is none.
func (s *Service) readTombstone(ctx context.Context, assetID string) (Tombstone, error) {
	rc, err := s.Target.Get(ctx, tombstoneObject(assetID))
	if err != nil {
		return Tombstone{}, err
	}
	defer rc.Close()
	var t Tombstone
	if err := json.NewDecoder(io.LimitReader(rc, 4096)).Decode(&t); err != nil {
		return Tombstone{}, fmt.Errorf("read the tombstone of %s: %w", assetID, err)
	}
	return t, nil
}

// SourceRestore says what RestoreSources did for each asset it looked at.
type SourceRestore struct {
	// Restored sources were copied back to their store.
	Restored []string
	// Queued assets have a re-encode queued, because their source was restored
	// or their encoded files are gone.
	Queued []string
	// Present assets lost nothing: source and encoded files are there.
	Present []string
	// Tombstoned assets were deleted after the backup, on purpose, and are
	// left alone.
	Tombstoned []string
	// Missing assets have no copy on the target and no source on the store: a
	// source the mirror never reached.
	Missing []string
	// Problems are what went wrong, one per asset.
	Problems []string
}

// RestoreSourcesOptions chooses what RestoreSources brings back.
type RestoreSourcesOptions struct {
	// AssetID restores one asset; empty restores every live asset.
	AssetID string
	// Reencode queues a re-encode of an asset through the job path.
	// A job already running on the asset is not a problem.
	Reencode func(ctx context.Context, projectID, appID, assetID string) (queued bool, err error)
}

// RestoreSources copies the mirrored sources of live assets back to the stores
// they belong on, where a store lost them, and queues a re-encode for each
// asset whose source it restored or whose encoded files are gone. Running it
// again changes nothing that is already restored.
func (s *Service) RestoreSources(ctx context.Context, opt RestoreSourcesOptions) (SourceRestore, error) {
	var res SourceRestore
	if s.Stores == nil {
		return res, errors.New("no stores to restore sources to")
	}
	assets, err := db.New(s.Pool).ListAssetsToRestoreSources(ctx, opt.AssetID)
	if err != nil {
		return res, fmt.Errorf("list the assets: %w", err)
	}
	if opt.AssetID != "" && len(assets) == 0 {
		return res, fmt.Errorf("no live asset %s with a source on a store", opt.AssetID)
	}
	for _, a := range assets {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		fail := func(err error) { res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", a.ID, err)) }
		if _, err := s.readTombstone(ctx, a.ID); err == nil {
			res.Tombstoned = append(res.Tombstoned, a.ID)
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			fail(err)
			continue
		}
		source, err := s.Stores.SourceFor(ctx, a.ID)
		if err != nil {
			fail(err)
			continue
		}
		restored := false
		switch _, err := source.Head(ctx, a.SourceUri.String); {
		case err == nil:
		case errors.Is(err, storage.ErrNotFound):
			switch err := s.restoreSource(ctx, source, a.ID, a.SourceUri.String); {
			case err == nil:
				restored = true
				res.Restored = append(res.Restored, a.ID)
			case errors.Is(err, storage.ErrNotFound):
				res.Missing = append(res.Missing, a.ID)
				continue
			default:
				fail(err)
				continue
			}
		default:
			fail(err)
			continue
		}
		lost, err := s.outputsLost(ctx, a.ID, a.OutputPrefix)
		if err != nil {
			fail(err)
			continue
		}
		if !restored && !lost {
			res.Present = append(res.Present, a.ID)
			continue
		}
		if opt.Reencode == nil {
			continue
		}
		queued, err := opt.Reencode(ctx, a.ProjectID, a.AppID, a.ID)
		if err != nil {
			fail(err)
			continue
		}
		if queued {
			res.Queued = append(res.Queued, a.ID)
		}
	}
	return res, nil
}

// restoreSource decrypts the mirrored copy of an asset's source into its store.
// It returns storage.ErrNotFound when the target has none.
func (s *Service) restoreSource(ctx context.Context, store storage.Storage, assetID, uri string) error {
	rc, err := s.Target.Get(ctx, sourceObject(assetID))
	if err != nil {
		return err
	}
	defer rc.Close()
	plain, err := s.decrypt(rc, 0)
	if err != nil {
		return err
	}
	// age authenticates every chunk and its end, so a copy cut short or
	// altered fails the read and the upload aborts, leaving no object.
	if err := store.Put(ctx, uri, plain); err != nil {
		return fmt.Errorf("write the source to its store: %w", err)
	}
	return nil
}

// outputsLost reports that the asset's current output has no files on its
// media store; an asset with no output yet has none to lose.
func (s *Service) outputsLost(ctx context.Context, assetID, prefix string) (bool, error) {
	if prefix == "" {
		return false, nil
	}
	media, err := s.Stores.MediaFor(ctx, assetID)
	if err != nil {
		return false, err
	}
	objs, _, err := media.List(ctx, prefix, "")
	if err != nil {
		return false, err
	}
	return len(objs) == 0, nil
}
