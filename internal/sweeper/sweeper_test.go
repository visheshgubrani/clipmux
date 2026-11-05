package sweeper

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestEveryRunsImmediatelyRepeatsAndSurvivesErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		Every(ctx, 10*time.Millisecond, "test", func(context.Context) (int, error) {
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
