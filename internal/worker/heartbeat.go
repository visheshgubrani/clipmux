package worker

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// heartbeat reports phase and progress on an interval, and cancels the job
// the moment the server says to stop (a newer attempt exists, or the job was
// cancelled). Network errors are logged and retried on the next tick; the
// lease outlasts several missed beats.
type heartbeat struct {
	mu       sync.Mutex
	phase    string
	progress float64
	why      string
	done     chan struct{}
	finished chan struct{}
}

func (w *Worker) startHeartbeat(ctx context.Context, cancel context.CancelFunc, job *workerapi.Job) *heartbeat {
	every := w.HeartbeatEvery
	if every == 0 {
		every = time.Duration(job.HeartbeatSeconds) * time.Second
	}
	if every <= 0 {
		every = 15 * time.Second
	}
	hb := &heartbeat{phase: "claimed", done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(hb.finished)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			hb.mu.Lock()
			body := map[string]any{"phase": hb.phase, "progress": hb.progress}
			hb.mu.Unlock()
			var v struct {
				Action string `json:"action"`
				Reason string `json:"reason"`
			}
			status, err := w.post(ctx, "/worker/v1/attempts/"+job.AttemptID+"/heartbeat", job.AttemptToken, body, &v)
			switch {
			case err != nil && ctx.Err() == nil:
				w.Logger.Warn("heartbeat failed", "attempt", job.AttemptID, "err", err)
			case status == http.StatusOK && v.Action == "stop":
				hb.mu.Lock()
				hb.why = v.Reason
				hb.mu.Unlock()
				cancel()
				return
			}
			select {
			case <-hb.done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return hb
}

func (hb *heartbeat) report(phase string, fraction float64) {
	hb.mu.Lock()
	hb.phase, hb.progress = phase, fraction
	hb.mu.Unlock()
}

func (hb *heartbeat) stop() {
	close(hb.done)
	<-hb.finished
}

func (hb *heartbeat) stopped() bool { return hb.reason() != "" }

func (hb *heartbeat) reason() string {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return hb.why
}
