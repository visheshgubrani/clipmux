// Package fence stops a restore from turning into data loss (D144).
//
// Bringing back an older database leaves storage holding what the database
// no longer knows, and the sweeps that keep storage tidy delete exactly that.
// So every store of the install carries a marker, _clipmux/epoch.json, with
// the install's ID and a generation. The generation goes up in Postgres
// first and is written to the stores after, so a store can never be ahead
// of the database that stamped it, unless the database went back in time.
// A store stamped by another install, or ahead of this database, raises the
// fence: the storage sweeps and the route reconciler stop deleting
// (retention and routes read the fence), and the owner reads Report and then
// lifts it with Finish.
package fence

import (
	"bytes"
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
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

// MarkerKey is the object each store carries.
const MarkerKey = "_clipmux/epoch.json"

// Interval is how often the leader stamps the stores.
const Interval = 5 * time.Minute

// Reasons a fence is raised, as restore_fence.reason has them.
const (
	// Restored: `clipmux admin restore` raised it.
	Restored = "restored"
	// DatabaseBehind: a store holds a generation this database never reached.
	DatabaseBehind = "database_behind"
	// ForeignInstall: a store was stamped by another install.
	ForeignInstall = "foreign_install"
)

// Marker is _clipmux/epoch.json.
type Marker struct {
	V          int       `json:"v"`
	InstallID  string    `json:"install_id"`
	Generation int64     `json:"generation"`
	WrittenAt  time.Time `json:"written_at"`
}

// Epoch stamps an install's stores and notices when they disagree with its
// database.
type Epoch struct {
	Pool      *pgxpool.Pool
	Stores    *stores.Set
	ProjectID string
	Logger    *slog.Logger
	Now       func() time.Time
}

// New returns the epoch of the default project over its stores.
func New(pool *pgxpool.Pool, set *stores.Set) *Epoch {
	return &Epoch{Pool: pool, Stores: set, ProjectID: db.DefaultProjectID, Logger: slog.New(slog.DiscardHandler), Now: time.Now}
}

// state is one store and what it says.
type state struct {
	id     string
	store  storage.Storage
	marker *Marker // nil: never stamped
}

// read opens every store of the project and reads its marker. A store it
// can't open or read is an error of its own, and is left out of the result:
// the others are still checked.
func (e *Epoch) read(ctx context.Context) ([]state, []error) {
	cfgs, err := db.New(e.Pool).ListStorageConfigs(ctx, e.ProjectID)
	if err != nil {
		return nil, []error{fmt.Errorf("list storage configs: %w", err)}
	}
	var out []state
	var errs []error
	for _, cfg := range cfgs {
		st, err := e.Stores.Get(ctx, cfg.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("store %s: %w", cfg.ID, err))
			continue
		}
		m, err := readMarker(ctx, st)
		if err != nil {
			errs = append(errs, fmt.Errorf("store %s: %w", cfg.ID, err))
			continue
		}
		out = append(out, state{id: cfg.ID, store: st, marker: m})
	}
	return out, errs
}

func readMarker(ctx context.Context, st storage.Storage) (*Marker, error) {
	rc, err := st.Get(ctx, MarkerKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", MarkerKey, err)
	}
	defer rc.Close()
	var m Marker
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return nil, fmt.Errorf("%s isn't a marker: %w", MarkerKey, err)
	}
	if m.V != 1 || m.InstallID == "" || m.Generation < 0 {
		return nil, fmt.Errorf("%s isn't a marker this version reads (v%d)", MarkerKey, m.V)
	}
	return &m, nil
}

// stamp writes the epoch to each store, and returns how many it wrote.
func (e *Epoch) stamp(ctx context.Context, states []state, ep db.InstallEpoch) (int, error) {
	body, _ := json.Marshal(Marker{V: 1, InstallID: ep.InstallID, Generation: ep.Generation, WrittenAt: e.Now().UTC()}) // plain struct; can't fail
	var errs []error
	n := 0
	for _, s := range states {
		if err := s.store.Put(ctx, MarkerKey, bytes.NewReader(body)); err != nil {
			errs = append(errs, fmt.Errorf("stamp store %s: %w", s.id, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// Found is why a fence is raised.
type Found struct {
	Reason string
	Detail string
	// BackupID and BackupAt name the backup a restore brought back.
	BackupID string
	BackupAt time.Time
}

// Engage raises the fence, unless one is up already, and reports whether it
// raised one.
func Engage(ctx context.Context, q *db.Queries, projectID string, f Found) (bool, error) {
	p := db.EngageFenceParams{ProjectID: projectID, Reason: f.Reason, Detail: f.Detail}
	if f.BackupID != "" {
		p.BackupID = pgtype.Text{String: f.BackupID, Valid: true}
	}
	if !f.BackupAt.IsZero() {
		p.BackupAt = pgtype.Timestamptz{Time: f.BackupAt, Valid: true}
	}
	_, err := q.EngageFence(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("raise the restore fence: %w", err)
	}
	return true, nil
}

// check compares the markers with the database's epoch.
func check(ep db.InstallEpoch, states []state) (Found, bool) {
	var found Found
	var lines []string
	for _, s := range states {
		m := s.marker
		switch {
		case m == nil:
		case m.InstallID != ep.InstallID:
			found.Reason = ForeignInstall
			lines = append(lines, fmt.Sprintf("store %s was stamped by install %s (generation %d); this database is install %s", s.id, m.InstallID, m.Generation, ep.InstallID))
		case m.Generation > ep.Generation:
			if found.Reason == "" {
				found.Reason = DatabaseBehind
			}
			lines = append(lines, fmt.Sprintf("store %s is at generation %d; this database is at generation %d", s.id, m.Generation, ep.Generation))
		}
	}
	found.Detail = strings.Join(lines, "; ")
	return found, found.Reason != ""
}

// Pass is one run of the leader loop. It raises the fence if a store
// disagrees with the database. Otherwise, with no fence up, it commits the
// next generation to Postgres and stamps every store with it, and returns how
// many it stamped. Under a fence it writes nothing, so the stores keep what
// shows how far ahead they are.
func (e *Epoch) Pass(ctx context.Context) (int, error) {
	q := db.New(e.Pool)
	ep, err := q.EnsureInstallEpoch(ctx, e.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("install epoch: %w", err)
	}
	states, errs := e.read(ctx)
	if found, bad := check(ep, states); bad {
		raised, err := Engage(ctx, q, e.ProjectID, found)
		if err != nil {
			errs = append(errs, err)
		}
		if raised {
			e.Logger.Error("restore fence raised: nothing will be deleted from storage until `clipmux admin restore finish --yes`",
				"reason", found.Reason, "detail", found.Detail)
		}
		return 0, errors.Join(errs...)
	}
	fenced, err := q.FenceActive(ctx, e.ProjectID)
	if err != nil {
		return 0, errors.Join(append(errs, fmt.Errorf("restore fence: %w", err))...)
	}
	if fenced {
		e.Logger.Warn("the restore fence is up: nothing is deleted from storage; see `clipmux admin restore report`")
		return 0, errors.Join(errs...)
	}
	if len(states) == 0 {
		return 0, errors.Join(errs...)
	}
	if ep, err = q.AdvanceInstallEpoch(ctx, e.ProjectID); err != nil {
		return 0, errors.Join(append(errs, fmt.Errorf("advance install epoch: %w", err))...)
	}
	n, err := e.stamp(ctx, states, ep)
	if err != nil {
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}

// Finish lifts the fence, once the owner has read the report: the database
// takes the highest generation any store holds, moves one past it, and
// stamps every store as its own. It reports whether there was a fence to lift.
// The fence stays up until every store is stamped, so a failure leaves
// nothing exposed; run it again.
func (e *Epoch) Finish(ctx context.Context) (bool, error) {
	q := db.New(e.Pool)
	fenced, err := q.FenceActive(ctx, e.ProjectID)
	if err != nil || !fenced {
		return false, err
	}
	if _, err := q.EnsureInstallEpoch(ctx, e.ProjectID); err != nil {
		return false, fmt.Errorf("install epoch: %w", err)
	}
	states, errs := e.read(ctx)
	if len(errs) > 0 {
		// A store that can't be read may hold more than the others.
		return false, fmt.Errorf("not lifting the fence while a store can't be read: %w", errors.Join(errs...))
	}
	var highest int64
	for _, s := range states {
		if s.marker != nil {
			highest = max(highest, s.marker.Generation)
		}
	}
	if err := q.RaiseInstallEpoch(ctx, db.RaiseInstallEpochParams{ProjectID: e.ProjectID, Generation: highest}); err != nil {
		return false, fmt.Errorf("raise install epoch: %w", err)
	}
	ep, err := q.AdvanceInstallEpoch(ctx, e.ProjectID)
	if err != nil {
		return false, fmt.Errorf("advance install epoch: %w", err)
	}
	if _, err := e.stamp(ctx, states, ep); err != nil {
		return false, err
	}
	if _, err := q.LiftFence(ctx, e.ProjectID); err != nil {
		return false, fmt.Errorf("lift the restore fence: %w", err)
	}
	return true, nil
}
