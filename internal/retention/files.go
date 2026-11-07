package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
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
		{"abandoned_outputs", f.PurgeAbandoned},
		{"orphan_prefixes", f.SweepOrphans},
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

// PurgeRetired deletes retired outputs past the retire window: the larger
// of 7 days and the project's longest token, plus an hour, so a viewer
// mid-session on the old output finishes first.
func (f *Files) PurgeRetired(ctx context.Context) (int, error) {
	rows, err := db.New(f.pool).ListPurgeableRetired(ctx, db.ListPurgeableRetiredParams{Now: f.ts(f.Now()), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		// Stop serving it first: the route records drop the output.
		err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			if _, err := q.MarkOutputPurged(ctx, o.ID); err != nil {
				return err
			}
			return routes.QueueAssetRefresh(ctx, q, o.ProjectID, o.AppID, o.AssetID)
		})
		if err != nil {
			return n, fmt.Errorf("purge output %s: %w", o.ID, err)
		}
		if err := f.deletePrefix(ctx, o.AssetID, o.Prefix.String); err != nil {
			return n, err // the orphan sweep finishes it
		}
		n++
	}
	return n, nil
}

// PurgeAbandoned deletes the outputs of attempts that were replaced, or
// whose job ended, more than AbandonedGrace ago. Nothing ever served them.
func (f *Files) PurgeAbandoned(ctx context.Context) (int, error) {
	q := db.New(f.pool)
	rows, err := q.ListPurgeableAbandoned(ctx, db.ListPurgeableAbandonedParams{Before: f.ts(f.Now().Add(-AbandonedGrace)), MaxRows: 100})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		if _, err := q.MarkOutputPurged(ctx, o.ID); err != nil {
			return n, fmt.Errorf("purge output %s: %w", o.ID, err)
		}
		if err := f.deletePrefix(ctx, o.AssetID, o.Prefix.String); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (f *Files) deletePrefix(ctx context.Context, assetID, prefix string) error {
	store, err := f.stores.ForAsset(ctx, assetID)
	if err != nil {
		return fmt.Errorf("store of %s: %w", assetID, err)
	}
	if err := store.DeletePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("delete %s: %w", prefix, err)
	}
	return nil
}

// SweepOrphans deletes output prefixes no row accounts for, on the stores
// assets live on: a purge that marked its row but failed to delete, or a
// crash's leftovers. It checks SweepAssets assets per pass, resuming after
// the last one, and only out_ prefixes, so other files under an asset (its
// tracks, later) are never touched. A claim creates its output's row
// before the worker gets a URL, so a prefix with no row has no writer.
func (f *Files) SweepOrphans(ctx context.Context) (int, error) {
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
		removed, err := f.sweepAsset(ctx, q, a.ID)
		n += removed
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

func (f *Files) sweepAsset(ctx context.Context, q *db.Queries, assetID string) (int, error) {
	known, err := q.KnownOutputs(ctx, assetID)
	if err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	for _, id := range known {
		keep[id] = true
	}
	store, err := f.stores.ForAsset(ctx, assetID)
	if err != nil {
		return 0, err
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

func (f *Files) sweepFS(context.Context) (int, error) {
	if f.FS == nil {
		return 0, nil
	}
	return f.FS.SweepStale(f.Now(), TmpAge, StagingAge)
}
