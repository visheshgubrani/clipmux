package sweeper

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

func TestOnlyOneReplicaLeadsAndAnotherTakesOver(t *testing.T) {
	pool := dbtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, b := NewLeader(pool, logger), NewLeader(pool, logger)
	a.Retry, b.Retry = 20*time.Millisecond, 20*time.Millisecond
	ctxA, stopA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() { a.Run(ctxA); close(doneA) }()
	waitFor(t, a.IsLeader)
	ctxB, stopB := context.WithCancel(context.Background())
	defer stopB()
	go b.Run(ctxB)
	time.Sleep(100 * time.Millisecond)
	if b.IsLeader() {
		t.Fatal("two replicas lead at once")
	}
	ran := 0
	task := b.Only(func(context.Context) (int, error) { ran++; return 1, nil })
	if n, _ := task(context.Background()); n != 0 || ran != 0 {
		t.Fatal("a follower ran a leader-only task")
	}
	stopA()
	<-doneA
	waitFor(t, b.IsLeader)
	if n, _ := task(context.Background()); n != 1 {
		t.Fatal("the new leader didn't run the task")
	}
}

// A restore takes the leader lock to have the database to itself: it can't
// while a serve leads, and a serve that boots meanwhile stays a follower.
func TestACommandHoldingTheLeaderLockExcludesServe(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	l := NewLeader(pool, logger)
	l.Retry = 20 * time.Millisecond
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { l.Run(lctx); close(done) }()
	waitFor(t, l.IsLeader)
	if _, held, err := HoldLeaderLock(ctx, pool); err != nil || held {
		t.Fatalf("HoldLeaderLock under a leading serve = held %v, %v; want it refused", held, err)
	}
	stop()
	<-done

	release, held, err := HoldLeaderLock(ctx, pool)
	if err != nil || !held {
		t.Fatalf("HoldLeaderLock with no serve = held %v, %v; want it taken", held, err)
	}
	l2 := NewLeader(pool, logger)
	l2.Retry = 20 * time.Millisecond
	l2ctx, stop2 := context.WithCancel(ctx)
	defer stop2()
	go l2.Run(l2ctx)
	time.Sleep(150 * time.Millisecond)
	if l2.IsLeader() {
		t.Fatal("a serve took the lock while a command held it")
	}
	release()
	waitFor(t, l2.IsLeader)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Advisory locks are per database: two installs in separate schemas of one
// database must each have a leader, or one's loops never run.
func TestInstallsInSeparateSchemasEachLead(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, b := NewLeader(dbtest.New(t), logger), NewLeader(dbtest.New(t), logger)
	a.Retry, b.Retry = 20*time.Millisecond, 20*time.Millisecond
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go a.Run(ctx)
	go b.Run(ctx)
	waitFor(t, func() bool { return a.IsLeader() && b.IsLeader() })
}
