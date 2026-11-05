package sweeper

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// leaderLockKey is the session advisory lock that elects the replica
// running the background loops ("clipmux1" as an integer).
const leaderLockKey = 0x636c69706d757831

// Leader elects one serve replica to run the background loops, with a
// session advisory lock held on a connection of its own. Every loop is
// already safe to run twice (SKIP LOCKED, idempotent passes), so a lost or
// contested lock costs duplicate work, never correctness.
type Leader struct {
	pool   *pgxpool.Pool
	held   atomic.Bool
	logger *slog.Logger
	// Retry is how often a follower tries for the lock and the leader checks
	// its connection.
	Retry time.Duration
}

// NewLeader returns a Leader; call Run to take part in the election.
func NewLeader(pool *pgxpool.Pool, logger *slog.Logger) *Leader {
	return &Leader{pool: pool, logger: logger, Retry: 10 * time.Second}
}

// IsLeader reports whether this replica holds the lock.
func (l *Leader) IsLeader() bool { return l.held.Load() }

// Run takes part in the election until ctx ends.
func (l *Leader) Run(ctx context.Context) {
	for ctx.Err() == nil {
		l.hold(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(l.Retry):
		}
	}
}

// hold acquires a connection, tries for the lock, and while it has the lock
// keeps checking the connection, because a dropped connection drops the lock.
func (l *Leader) hold(ctx context.Context) {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(leaderLockKey)).Scan(&got); err != nil || !got {
		return
	}
	l.held.Store(true)
	l.logger.Info("this replica runs the background loops")
	defer func() {
		l.held.Store(false)
		// Unlock on a live connection; a dead one has lost it already.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(leaderLockKey))
	}()
	ticker := time.NewTicker(l.Retry)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.Ping(ctx); err != nil {
				l.logger.Warn("lost the background-loop lock", "err", err)
				conn.Conn().Close(context.Background())
				return
			}
		}
	}
}

// Only runs task while this replica is the leader.
func (l *Leader) Only(task Task) Task {
	return func(ctx context.Context) (int, error) {
		if !l.IsLeader() {
			return 0, nil
		}
		return task(ctx)
	}
}

// Await waits up to d for this replica to lead, so a lone serve runs its
// first passes at boot instead of one interval later.
func (l *Leader) Await(ctx context.Context, d time.Duration) {
	deadline := time.Now().Add(d)
	for !l.IsLeader() && time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
}
