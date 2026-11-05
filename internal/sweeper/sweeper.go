package sweeper

import (
	"context"
	"log/slog"
	"time"
)

// Task is one periodic recovery pass. It returns how many items it handled.
type Task func(ctx context.Context) (int, error)

// Every runs task immediately and then every interval until ctx ends. Errors
// are logged and the loop keeps going: a failed pass is retried next tick.
// serve wraps each task in Leader.Only, so one replica runs it.
func Every(ctx context.Context, interval time.Duration, name string, task Task, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := task(ctx)
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
