package fence

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// Guard decides, store by store, whether this install may delete from a
// store. The fence row alone isn't enough: only the epoch loop raises it, so
// a sweep that runs before that loop has read a store (a boot that didn't win
// leadership in time, a store whose marker couldn't be read, a bucket Connect
// has only just added) would delete on a database that went back in time. So
// every pass that deletes reads the marker of each store it deletes from
// first, and deletes only when the marker is this install's and isn't ahead
// of this database. A marker naming another install, or ahead of this
// database, raises the fence there and then. A marker that can't be read
// stops the delete with an error (it fails closed). A store with no marker
// yet, new or stamped by nothing, may be deleted from: the epoch loop stamps
// it on its next pass.
//
// A Guard is for one pass: it remembers what it read, by store ID.
type Guard struct {
	q         *db.Queries
	projectID string
	logger    *slog.Logger
	fenced    bool
	epoch     db.InstallEpoch
	checked   map[string]bool
}

// NewGuard reads the fence and the install's epoch for one pass. logger may
// be nil.
func NewGuard(ctx context.Context, q *db.Queries, projectID string, logger *slog.Logger) (*Guard, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	fenced, err := q.FenceActive(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("restore fence: %w", err)
	}
	g := &Guard{q: q, projectID: projectID, logger: logger, fenced: fenced, checked: map[string]bool{}}
	if !fenced {
		if g.epoch, err = q.EnsureInstallEpoch(ctx, projectID); err != nil {
			return nil, fmt.Errorf("install epoch: %w", err)
		}
	}
	return g, nil
}

// Fenced reports whether the fence is up, as read or as raised by this Guard.
func (g *Guard) Fenced() bool { return g.fenced }

// MayDelete reports whether this install may delete from st, the store with
// config ID storeID. It is false while the fence is up, and false with an
// error when the store's marker can't be read.
func (g *Guard) MayDelete(ctx context.Context, storeID string, st storage.Storage) (bool, error) {
	if g.fenced {
		return false, nil
	}
	if ok, seen := g.checked[storeID]; seen {
		return ok, nil
	}
	m, err := readMarker(ctx, st)
	if err != nil {
		return false, fmt.Errorf("not deleting from store %s, whose marker can't be read: %w", storeID, err)
	}
	if found, bad := check(g.epoch, []state{{id: storeID, store: st, marker: m}}); bad {
		raised, err := Engage(ctx, g.q, g.projectID, found)
		if err != nil {
			return false, err
		}
		g.fenced = true
		if raised {
			g.logger.Error("restore fence raised: nothing will be deleted from storage until `clipmux admin restore finish --yes`",
				"reason", found.Reason, "detail", found.Detail)
		}
		return false, nil
	}
	g.checked[storeID] = true
	return true, nil
}

// CheckForeignBucket reports an error if st already belongs to another
// ClipMux install: either _clipmux/epoch.json names a different install ID,
// or the bucket has no marker yet but holds _routes/{id}.json records this
// database doesn't know. Unlike CheckBucket, it never raises the restore
// fence: Connect calls it before modifying storage, Worker, or delivery. A
// bucket belonging to this install (even ahead of this database after a
// restore) returns nil so recovery can proceed under the restore fence.
func CheckForeignBucket(ctx context.Context, q *db.Queries, projectID string, st storage.Storage) error {
	ep, err := q.EnsureInstallEpoch(ctx, projectID)
	if err != nil {
		return fmt.Errorf("install epoch: %w", err)
	}
	m, err := readMarker(ctx, st)
	if err != nil {
		return fmt.Errorf("read bucket marker: %w", err)
	}
	if m != nil {
		if m.InstallID != ep.InstallID {
			when := ""
			if !m.WrittenAt.IsZero() {
				when = " on " + m.WrittenAt.UTC().Format(time.RFC3339)
			}
			return fmt.Errorf(
				"bucket already belongs to ClipMux install %s (generation %d%s); create or choose a separate R2 bucket for this install (%s)",
				m.InstallID, m.Generation, when, ep.InstallID,
			)
		}
		return nil
	}
	unknown, err := unknownRouteCount(ctx, q, projectID, st)
	if err != nil {
		return err
	}
	if unknown > 0 {
		return fmt.Errorf(
			"bucket holds %d route records from another ClipMux install; create or choose a separate R2 bucket for this install",
			unknown,
		)
	}
	return nil
}

// CheckBucket is for a bucket about to become the Worker's (Connect
// Cloudflare), before its first reconcile. Beyond MayDelete's marker check, a
// bucket with no marker but with route records this database doesn't know
// was used by another install, or by this one before its database went back
// in time, or before markers existed: the fence goes up rather than letting
// the reconcile delete them. It reports whether the fence is up.
func (g *Guard) CheckBucket(ctx context.Context, storeID string, st storage.Storage) (bool, error) {
	ok, err := g.MayDelete(ctx, storeID, st)
	if err != nil || !ok {
		return g.fenced, err
	}
	m, err := readMarker(ctx, st)
	if err != nil || m != nil {
		return g.fenced, err
	}
	unknown, err := unknownRouteCount(ctx, g.q, g.projectID, st)
	if err != nil {
		return g.fenced, fmt.Errorf("store %s: %w", storeID, err)
	}
	if unknown == 0 {
		return false, nil
	}
	found := Found{Reason: ForeignInstall, Detail: fmt.Sprintf(
		"store %s has no %s but holds %d route records this database doesn't know: another install, or this one before its database was restored, published them", storeID, MarkerKey, unknown)}
	raised, err := Engage(ctx, g.q, g.projectID, found)
	if err != nil {
		return false, err
	}
	g.fenced = true
	g.checked[storeID] = false
	if raised {
		g.logger.Error("restore fence raised: nothing will be deleted from storage until `clipmux admin restore finish --yes`",
			"reason", found.Reason, "detail", found.Detail)
	}
	return true, nil
}

func unknownRouteCount(ctx context.Context, q *db.Queries, projectID string, st storage.Storage) (int, error) {
	known, err := q.ListPlaybackIDStrings(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("list playback IDs: %w", err)
	}
	have := make(map[string]bool, len(known))
	for _, id := range known {
		have[id] = true
	}
	unknown := 0
	err = walk(ctx, st, "_routes/", func(o storage.Object) error {
		if pid, ok := routeID(o.Key); ok && !have[pid] {
			unknown++
		}
		return nil
	})
	return unknown, err
}

// routeID is the playback ID of a _routes/{id}.json key.
func routeID(key string) (string, bool) {
	const prefix, suffix = "_routes/", ".json"
	if len(key) <= len(prefix)+len(suffix) || key[:len(prefix)] != prefix || key[len(key)-len(suffix):] != suffix {
		return "", false
	}
	return key[len(prefix) : len(key)-len(suffix)], true
}

// recordTarget is the part of a route record the fence reads: which asset and
// output it plays. It mirrors routes.Record's fields, which this package can't
// import (routes reads the fence).
type recordTarget struct {
	AssetID  string `json:"asset_id"`
	OutputID string `json:"output_id"`
}

func decodeRecord(r io.Reader) (recordTarget, error) {
	var t recordTarget
	err := json.NewDecoder(r).Decode(&t)
	return t, err
}
