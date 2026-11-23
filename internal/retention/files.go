package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/fence"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// File windows (D070).
const (
	// AbandonedGrace: an attempt's files go this long after a newer
	// attempt replaced it or its job ended. Its PUT URLs last 15 minutes
	// and only a live attempt can sign, so nothing can write there after.
	AbandonedGrace = 30 * time.Minute
	// TmpAge and StagingAge bound the filesystem store's leftovers: a
	// staged write lasts seconds, and an upload idle for a day is aborted.
	TmpAge     = 24 * time.Hour
	StagingAge = 7 * 24 * time.Hour
	// CommitGrace: an output's files are checked against its manifest
	// this long after its commit. The worker's PUT URLs last 15 minutes and
	// a committed attempt can't sign more, so nothing can add a file after.
	CommitGrace = 16 * time.Minute
	// SweepAssets is how many assets each pass checks for orphaned output
	// prefixes, resuming where the last pass stopped.
	SweepAssets = 500
)

// Files deletes stored files nothing can play or write any more: retired
// outputs past the retire window, the outputs of replaced attempts, output
// prefixes with no row, and the filesystem store's leftovers. Retired
// outputs are first marked purged and their routes refreshed, so they stop
// being served before their files go.
type Files struct {
	pool   *pgxpool.Pool
	stores *stores.Set
	// FS is the install's filesystem store, swept for leftovers; nil skips it.
	FS     *storage.Filesystem
	Now    func() time.Time
	Logger *slog.Logger
}

// NewFiles returns the file rules over the project's stores.
func NewFiles(pool *pgxpool.Pool, set *stores.Set, fs *storage.Filesystem) *Files {
	return &Files{pool: pool, stores: set, FS: fs, Now: time.Now, Logger: slog.New(slog.DiscardHandler)}
}

// Run is one pass of every file rule, the hourly leader loop.
func (f *Files) Run(ctx context.Context) (int, error) {
	var errs []error
	total := 0
	for _, rule := range []struct {
		name string
		run  func(context.Context) (int, error)
	}{
		{"retired_outputs", f.PurgeRetired},
		{"retired_tracks", f.PurgeRetiredTracks},
		{"abandoned_outputs", f.PurgeAbandoned},
		{"orphan_prefixes", f.SweepOrphans},
		{"unlisted_files", f.SweepUnlisted},
		{"filesystem_leftovers", f.sweepFS},
	} {
		n, err := rule.run(ctx)
		total += n
		if n > 0 {
			f.Logger.Info("deleted files", "rule", rule.name, "count", n)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rule.name, err))
		}
	}
	return total, errors.Join(errs...)
}

func (f *Files) ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// errFenced stops a rule part way when its guard raised the fence.
var errFenced = errors.New("the restore fence went up")

// guard starts a rule's pass. It is nil when the fence is up: the database
// may be older than the files, and what it doesn't know may be newer, not
// garbage (D144). Each store is then checked again before anything on it is
// deleted (fence.Guard).
func (f *Files) guard(ctx context.Context) (*fence.Guard, error) {
	g, err := fence.NewGuard(ctx, db.New(f.pool), db.DefaultProjectID, f.Logger)
	if err != nil {
		return nil, err
	}
	if g.Fenced() {
		f.Logger.Warn("the restore fence is up; not deleting files")
		return nil, nil
	}
	return g, nil
}

// media returns the store holding an asset's encoded files and tracks, and
// whether g lets this pass delete from it.
func (f *Files) media(ctx context.Context, g *fence.Guard, assetID string) (storage.Storage, bool, error) {
	row, err := db.New(f.pool).AssetStores(ctx, assetID)
	if err != nil {
		return nil, false, fmt.Errorf("storage of %s: %w", assetID, err)
	}
	st, err := f.stores.Get(ctx, row.MediaStorageID)
	if err != nil {
		return nil, false, fmt.Errorf("store of %s: %w", assetID, err)
	}
	ok, err := g.MayDelete(ctx, row.MediaStorageID, st)
	return st, ok, err
}

// PurgeRetired deletes retired outputs past the retire window: the larger
// of 7 days and the project's longest token, plus an hour, so a viewer
// mid-session on the old output finishes first.
func (f *Files) PurgeRetired(ctx context.Context) (int, error) {
	g, err := f.guard(ctx)
	if g == nil {
		return 0, err
	}
	rows, err := db.New(f.pool).ListPurgeableRetired(ctx, db.ListPurgeableRetiredParams{Now: f.ts(f.Now()), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		// Asked before the row is marked, so a fence that goes up leaves both.
		store, ok, err := f.media(ctx, g, o.AssetID)
		if err != nil || !ok {
			return n, err
		}
		// Stop serving it first: the route records drop the output.
		err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			if _, err := q.MarkOutputPurged(ctx, o.ID); err != nil {
				return err
			}
			return routes.QueueAssetRefresh(ctx, q, o.ProjectID, o.AppID, o.AssetID)
		})
		if err != nil {
			return n, fmt.Errorf("purge output %s: %w", o.ID, err)
		}
		if err := store.DeletePrefix(ctx, o.Prefix.String); err != nil {
			return n, fmt.Errorf("delete %s: %w", o.Prefix.String, err) // the orphan sweep finishes it
		}
		n++
	}
	return n, nil
}

// PurgeRetiredTracks deletes tracks retired past the same window as
// outputs (P5 "Retention": retired tracks go with the output window): the
// track stops being served first (it becomes deleted and the records drop
// it), then its file goes.
func (f *Files) PurgeRetiredTracks(ctx context.Context) (int, error) {
	g, err := f.guard(ctx)
	if g == nil {
		return 0, err
	}
	rows, err := db.New(f.pool).ListPurgeableRetiredTracks(ctx, db.ListPurgeableRetiredTracksParams{Now: f.ts(f.Now()), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range rows {
		store, ok, err := f.media(ctx, g, t.AssetID)
		if err != nil || !ok {
			return n, err
		}
		err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			if _, err := q.PurgeTrack(ctx, t.ID); err != nil {
				return err
			}
			return routes.QueueAssetRefresh(ctx, q, t.ProjectID, t.AppID, t.AssetID)
		})
		if err != nil {
			return n, fmt.Errorf("purge track %s: %w", t.ID, err)
		}
		if err := store.Delete(ctx, tracks.StorageKey(t.AssetID, t.ID)); err != nil {
			return n, fmt.Errorf("delete track %s: %w", t.ID, err)
		}
		n++
	}
	return n, nil
}

// PurgeAbandoned deletes the outputs of attempts that were replaced, or
// whose job ended, more than AbandonedGrace ago. Nothing ever served them.
func (f *Files) PurgeAbandoned(ctx context.Context) (int, error) {
	g, err := f.guard(ctx)
	if g == nil {
		return 0, err
	}
	q := db.New(f.pool)
	rows, err := q.ListPurgeableAbandoned(ctx, db.ListPurgeableAbandonedParams{Before: f.ts(f.Now().Add(-AbandonedGrace)), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		store, ok, err := f.media(ctx, g, o.AssetID)
		if err != nil || !ok {
			return n, err
		}
		if _, err := q.MarkOutputPurged(ctx, o.ID); err != nil {
			return n, fmt.Errorf("purge output %s: %w", o.ID, err)
		}
		if err := store.DeletePrefix(ctx, o.Prefix.String); err != nil {
			return n, fmt.Errorf("delete %s: %w", o.Prefix.String, err)
		}
		n++
	}
	return n, nil
}

// SweepOrphans deletes output prefixes no row accounts for, on the stores
// assets live on: a purge that marked its row but failed to delete, or a
// crash's leftovers. It checks SweepAssets assets per pass, resuming after
// the last one, and only out_ prefixes, so other files under an asset (its
// tracks, later) are never touched. A claim creates its output's row
// before the worker gets a URL, so a prefix with no row has no writer.
func (f *Files) SweepOrphans(ctx context.Context) (int, error) {
	g, err := f.guard(ctx)
	if g == nil {
		return 0, err
	}
	q := db.New(f.pool)
	after, err := q.GetRetentionCursor(ctx, db.GetRetentionCursorParams{ProjectID: db.DefaultProjectID, Name: "orphan_prefixes"})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	assets, err := q.ListAssetsForSweep(ctx, db.ListAssetsForSweepParams{ID: after, Limit: SweepAssets})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range assets {
		removed, err := f.sweepAsset(ctx, q, g, a.ID)
		n += removed
		if errors.Is(err, errFenced) {
			return n, nil // the cursor stays, for when the fence is lifted
		}
		if err != nil {
			return n, err
		}
	}
	next := ""
	if len(assets) == SweepAssets {
		next = assets[len(assets)-1].ID
	}
	return n, q.SetRetentionCursor(ctx, db.SetRetentionCursorParams{ProjectID: db.DefaultProjectID, Name: "orphan_prefixes", Value: next})
}

func (f *Files) sweepAsset(ctx context.Context, q *db.Queries, g *fence.Guard, assetID string) (int, error) {
	known, err := q.KnownOutputs(ctx, assetID)
	if err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	for _, id := range known {
		keep[id] = true
	}
	store, ok, err := f.media(ctx, g, assetID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errFenced
	}
	base := "media/" + assetID + "/"
	orphans := map[string]bool{}
	for cursor := ""; ; {
		objs, next, err := store.List(ctx, base, cursor)
		if err != nil {
			return 0, fmt.Errorf("list %s: %w", base, err)
		}
		for _, o := range objs {
			seg, _, ok := strings.Cut(strings.TrimPrefix(o.Key, base), "/")
			if ok && strings.HasPrefix(seg, "out_") && !keep[seg] {
				orphans[seg] = true
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	n := 0
	for id := range orphans {
		if err := store.DeletePrefix(ctx, base+id+"/"); err != nil {
			return n, fmt.Errorf("delete orphan %s%s/: %w", base, id, err)
		}
		n++
	}
	return n, nil
}

// SweepUnlisted deletes the files of committed outputs that their manifest
// doesn't list (manifest.json aside), once CommitGrace has passed: a worker
// could add them after the commit's verification while its PUT URLs were
// still valid (D124). Built-in delivery already refuses them; this removes
// them for the Cloudflare Worker too, which reads the bucket. Each output
// is checked once.
func (f *Files) SweepUnlisted(ctx context.Context) (int, error) {
	g, err := f.guard(ctx)
	if g == nil {
		return 0, err
	}
	q := db.New(f.pool)
	rows, err := q.ListUnsweptOutputs(ctx, db.ListUnsweptOutputsParams{Before: f.ts(f.Now().Add(-CommitGrace)), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		var m struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		if err := json.Unmarshal(o.Manifest, &m); err != nil {
			return n, fmt.Errorf("manifest of %s: %w", o.ID, err)
		}
		// A commit always lists the playlists; a manifest that lists
		// nothing wasn't verified by one, so its files stay.
		if len(m.Files) == 0 {
			f.Logger.Warn("output's manifest lists no files; not sweeping it", "output", o.ID)
			if err := q.MarkOutputSwept(ctx, o.ID); err != nil {
				return n, fmt.Errorf("mark %s swept: %w", o.ID, err)
			}
			continue
		}
		keep := map[string]bool{"manifest.json": true}
		for _, file := range m.Files {
			keep[file.Path] = true
		}
		store, ok, err := f.media(ctx, g, o.AssetID)
		if err != nil || !ok {
			return n, err
		}
		var extra []string
		for cursor := ""; ; {
			objs, next, err := store.List(ctx, o.Prefix.String, cursor)
			if err != nil {
				return n, fmt.Errorf("list %s: %w", o.Prefix.String, err)
			}
			for _, obj := range objs {
				if !keep[strings.TrimPrefix(obj.Key, o.Prefix.String)] {
					extra = append(extra, obj.Key)
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
		for _, key := range extra {
			if err := store.Delete(ctx, key); err != nil {
				return n, fmt.Errorf("delete %s: %w", key, err)
			}
			f.Logger.Warn("deleted a file added to an output after its commit", "output", o.ID, "key", key)
			n++
		}
		if err := q.MarkOutputSwept(ctx, o.ID); err != nil {
			return n, fmt.Errorf("mark %s swept: %w", o.ID, err)
		}
	}
	return n, nil
}

func (f *Files) sweepFS(context.Context) (int, error) {
	if f.FS == nil {
		return 0, nil
	}
	return f.FS.SweepStale(f.Now(), TmpAge, StagingAge)
}
