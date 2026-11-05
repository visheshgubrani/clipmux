package worker

import (
	"context"
	"io"
	"time"
)

// watchdog cancels a request when its body moves no bytes for a while. An
// HTTP client timeout can't do this: a legitimate upload or download of a
// large file takes as long as it takes, but a stalled one must not hang the
// attempt (the heartbeat would keep its lease alive forever).
type watchdog struct {
	timer *time.Timer
	d     time.Duration
}

// idle returns a context that is cancelled after d without progress, and the
// watchdog whose reader wrappers count as progress.
func idle(ctx context.Context, d time.Duration) (context.Context, *watchdog) {
	ctx, cancel := context.WithCancel(ctx)
	w := &watchdog{d: d}
	w.timer = time.AfterFunc(d, cancel)
	context.AfterFunc(ctx, func() { w.timer.Stop() })
	return ctx, w
}

// reader wraps r so every read that returns bytes resets the timer.
func (w *watchdog) reader(r io.Reader) io.Reader { return &progressReader{r: r, w: w} }

// stop releases the watchdog; the request's context is no longer needed.
func (w *watchdog) stop() { w.timer.Stop() }

type progressReader struct {
	r io.Reader
	w *watchdog
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.w.timer.Reset(p.w.d)
	}
	return n, err
}
