package sweeper

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// leaderLockClass is the first key of the session advisory lock that elects
// the replica running the background loops ("cmx1"). The second key is the
// install's schema, hashed: advisory locks are per database, and installs
// in separate schemas of one database (tests, shared servers) each elect
// their own leader.
const leaderLockClass = 0x636d7831

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
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`, int32(leaderLockClass)).Scan(&got); err != nil || !got {
		return
	}
	l.held.Store(true)
	l.logger.Info("this replica runs the background loops")
	defer func() {
		l.held.Store(false)
		// Unlock on a live connection; a dead one has lost it already.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, hashtext(current_schema()))`, int32(leaderLockClass))
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

// HoldLeaderLock takes the leader lock for a command that must have the
// database to itself, such as a restore: while it is held, no serve runs the
// background loops, and one that boots waits as a follower. held is false when
// a serve already has the lock; call release, once, when the command is done.
// The lock is keyed by the schema's name rather than by current_schema(), so
// it survives a command that drops and recreates the schema.
func HoldLeaderLock(ctx context.Context, pool *pgxpool.Pool) (release func(), held bool, err error) {
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return nil, false, fmt.Errorf("current schema: %w", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("take the leader lock: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2))`, int32(leaderLockClass), schema).Scan(&held); err != nil || !held {
		conn.Release()
		if err != nil {
			return nil, false, fmt.Errorf("take the leader lock: %w", err)
		}
		return nil, false, nil
	}
	return func() {
		// Unlock on a live connection; a dead one has lost it already.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, hashtext($2))`, int32(leaderLockClass), schema)
		conn.Release()
	}, true, nil
}
