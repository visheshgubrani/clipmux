package sweeper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEveryRunsImmediatelyRepeatsAndSurvivesErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		Every(ctx, 10*time.Millisecond, 0, "test", func(context.Context) (int, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("first pass fails")
			}
			return 1, nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("only %d passes ran", calls.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Every did not stop when ctx was cancelled")
	}
}

func TestEverySurvivesAPanickingPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	second := make(chan struct{})
	var logged strings.Builder
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}), nil))
	go Every(ctx, 10*time.Millisecond, 0, "panics", func(context.Context) (int, error) {
		if calls.Add(1) == 1 {
			panic("boom: a bad row") // the first pass panics
		} else if calls.Load() == 2 {
			close(second)
		}
		return 0, nil
	}, logger)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop stopped after a panic")
	}
	mu.Lock()
	defer mu.Unlock()
	if out := logged.String(); !strings.Contains(out, "sweeper pass failed") || !strings.Contains(out, "panic: boom: a bad row") {
		t.Fatalf("the panic wasn't logged as a failed pass: %s", out)
	}
}

func TestEachPassHasItsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan error, 1)
	go Every(ctx, time.Hour, 20*time.Millisecond, "slow", func(ctx context.Context) (int, error) {
		<-ctx.Done()
		got <- ctx.Err()
		return 0, ctx.Err()
	}, slog.New(slog.DiscardHandler))
	select {
	case err := <-got:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pass ended with %v, want its deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pass ran past its deadline")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
