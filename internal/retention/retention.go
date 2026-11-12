// Package retention deletes what ClipMux no longer needs once its window
// has passed: rows here (D069) and, later, stored files. Each window
// follows from another rule rather than from taste, so they are constants.
// Jobs, attempts and dispatches are kept: they are history and spend.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// Windows.
const (
	// IdempotencyWindow: a client retries a request within minutes; a day
	// covers a retry after an outage.
	IdempotencyWindow = 24 * time.Hour
	// SessionIdle matches the dashboard's idle limit: a session idle this
	// long can't be used, so it goes even before it expires.
	SessionIdle = 7 * 24 * time.Hour
	// PairingWindow keeps an expired or used pairing a day, for the page
	// that shows its outcome.
	PairingWindow = 24 * time.Hour
	// WebhookWindow keeps finished deliveries and their events 30 days:
	// GET /v1/events and clipmux listen --since reach back that far.
	WebhookWindow = 30 * 24 * time.Hour
	// OutboxWindow keeps other finished outbox rows (route and key
	// publications, wakes, stops) a week, for debugging.
	OutboxWindow = 7 * 24 * time.Hour
	// SetupTestsKept are the newest finished setup tests kept.
	SetupTestsKept = 20
	// AnalyticsWindow keeps daily analytics and viewers 180 days: the API's
	// longest range is 90 days, compared with the 90 before it (D105).
	AnalyticsWindow = 180 * 24 * time.Hour
	// HourlyAnalyticsWindow keeps hourly analytics 14 days: only the 24-hour
	// range and the 24 hours before it read them, and an edge pull
	// recomputes recent hours (D105).
	HourlyAnalyticsWindow = 14 * 24 * time.Hour
	// StuckAfter is when a pending publication or stop counts as stuck.
	StuckAfter = time.Hour
)

// batch is how many rows one statement deletes, so no pass holds a long
// lock or a large transaction.
const batch = 1000

// Service runs the rules.
type Service struct {
	pool   *pgxpool.Pool
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns the retention service.
func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, Now: time.Now, Logger: slog.New(slog.DiscardHandler)}
}

// Rule is one kind of row that goes, and how many went.
type Rule struct {
	Name    string
	Deleted int64
}

// rules are applied in order; deliveries go before the events they point
// at.
func (s *Service) rules(now time.Time) []struct {
	name string
	run  func(ctx context.Context, q *db.Queries) (int64, error)
} {
	ts := func(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
	analyticsDay := pgtype.Date{Time: now.Add(-AnalyticsWindow).UTC(), Valid: true}
	return []struct {
		name string
		run  func(ctx context.Context, q *db.Queries) (int64, error)
	}{
		{"idempotency_keys", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneIdempotencyKeys(ctx, db.PruneIdempotencyKeysParams{Before: ts(now.Add(-IdempotencyWindow)), MaxRows: batch})
		}},
		{"owner_sessions", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneOwnerSessions(ctx, db.PruneOwnerSessionsParams{Now: ts(now), IdleBefore: ts(now.Add(-SessionIdle)), MaxRows: batch})
		}},
		{"agent_pairings", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneAgentPairings(ctx, db.PruneAgentPairingsParams{Before: ts(now.Add(-PairingWindow)), MaxRows: batch})
		}},
		{"app_pairings", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneAppPairings(ctx, db.PruneAppPairingsParams{Before: ts(now.Add(-PairingWindow)), MaxRows: batch})
		}},
		{"webhook_deliveries", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneWebhookDeliveries(ctx, db.PruneWebhookDeliveriesParams{Before: ts(now.Add(-WebhookWindow)), MaxRows: batch})
		}},
		{"webhook_events", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneWebhookEvents(ctx, db.PruneWebhookEventsParams{Before: ts(now.Add(-WebhookWindow)), MaxRows: batch})
		}},
		{"outbox", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneDoneOutbox(ctx, db.PruneDoneOutboxParams{Before: ts(now.Add(-OutboxWindow)), MaxRows: batch})
		}},
		{"setup_tests", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PruneSetupTests(ctx, db.PruneSetupTestsParams{Keep: SetupTestsKept, MaxRows: batch})
		}},
		{"dispatch_keys", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.ForgetSpentDispatchKeys(ctx, batch)
		}},
		{"playback_hourly", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PrunePlaybackHourly(ctx, db.PrunePlaybackHourlyParams{Before: ts(now.Add(-HourlyAnalyticsWindow)), MaxRows: batch})
		}},
		{"playback_daily", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PrunePlaybackDaily(ctx, db.PrunePlaybackDailyParams{Before: analyticsDay, MaxRows: batch})
		}},
		{"playback_viewers", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PrunePlaybackViewers(ctx, db.PrunePlaybackViewersParams{Before: analyticsDay, MaxRows: batch})
		}},
		{"playback_viewer_days", func(ctx context.Context, q *db.Queries) (int64, error) {
			return q.PrunePlaybackViewerDays(ctx, db.PrunePlaybackViewerDaysParams{Before: analyticsDay, MaxRows: batch})
		}},
	}
}

// Prune applies every rule until each comes up short of a batch, and
// reports what went. With dryRun it does the same inside a transaction it
// rolls back, so the counts are exact and nothing changes.
func (s *Service) Prune(ctx context.Context, dryRun bool) ([]Rule, error) {
	if dryRun {
		var out []Rule
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var err error
			out, err = s.prune(ctx, db.New(tx))
			if err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			err = nil
		}
		return out, err
	}
	return s.prune(ctx, db.New(s.pool))
}

var errRollback = errors.New("retention: dry run")

func (s *Service) prune(ctx context.Context, q *db.Queries) ([]Rule, error) {
	var out []Rule
	for _, r := range s.rules(s.Now()) {
		total := int64(0)
		for {
			n, err := r.run(ctx, q)
			if err != nil {
				return out, fmt.Errorf("prune %s: %w", r.name, err)
			}
			total += n
			if n < batch {
				break
			}
		}
		out = append(out, Rule{Name: r.name, Deleted: total})
	}
	return out, nil
}

// Run is the hourly leader loop: prune, and report stuck publications.
func (s *Service) Run(ctx context.Context) (int, error) {
	rules, err := s.Prune(ctx, false)
	n := 0
	for _, r := range rules {
		n += int(r.Deleted)
		if r.Deleted > 0 {
			s.Logger.Info("pruned", "rule", r.Name, "rows", r.Deleted)
		}
	}
	if err != nil {
		return n, err
	}
	stuck, err := s.Stuck(ctx)
	if err != nil {
		return n, err
	}
	if len(stuck) > 0 {
		s.Logger.Error("outbox rows are stuck; see clipmux admin outbox", "count", len(stuck), "oldest", stuck[0].ID, "kind", stuck[0].Kind)
	}
	return n, nil
}

// Stuck lists route and key publications, wakes and stops still pending
// after StuckAfter. They are never given up on, because giving up would
// strand a revocation, an asset's readiness or a paid machine.
func (s *Service) Stuck(ctx context.Context) ([]db.ListStuckOutboxRow, error) {
	rows, err := db.New(s.pool).ListStuckOutbox(ctx, pgtype.Timestamptz{Time: s.Now().Add(-StuckAfter), Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list stuck outbox: %w", err)
	}
	return rows, nil
}

// RetryNow makes a pending outbox row due at once.
func (s *Service) RetryNow(ctx context.Context, id string) (bool, error) {
	n, err := db.New(s.pool).RetryOutboxNow(ctx, id)
	return n == 1, err
}
