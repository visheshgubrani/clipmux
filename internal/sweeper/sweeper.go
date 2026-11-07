package sweeper

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// Task is one periodic recovery pass. It returns how many items it handled.
type Task func(ctx context.Context) (int, error)

// Every runs task immediately and then every interval until ctx ends. Errors
// are logged and the loop keeps going: a failed pass is retried next tick.
// A pass that panics is logged with its stack and counts as failed, so one
// bad row can't stop serve. timeout bounds each pass (0 for none: passes
// whose work is bounded by its own lease, such as imports). serve wraps each
// task in Leader.Only, so one replica runs it.
func Every(ctx context.Context, interval, timeout time.Duration, name string, task Task, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := pass(ctx, timeout, task)
		switch {
		case err != nil && ctx.Err() == nil:
			logger.Error("sweeper pass failed", "sweeper", name, "err", err)
		case n > 0:
			logger.Info("sweeper pass", "sweeper", name, "handled", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pass runs one pass under its deadline, turning a panic into an error.
func pass(ctx context.Context, timeout time.Duration, task Task) (n int, err error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	defer func() {
		if v := recover(); v != nil {
			n, err = 0, fmt.Errorf("panic: %v\n%s", v, debug.Stack())
		}
	}()
	return task(ctx)
}
