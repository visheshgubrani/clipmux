// Package executor is the Executor adapter boundary (docs/architecture.md,
// "Compute" and "Adapters"). Every executor claims work through the same
// worker API; an Executor only reports availability and wakes workers.
// v1 adapters: built-in (Phase 1), PC agent (Phase 3), Modal and RunPod (Phase 4).
package executor

import "context"

// Job identifies queued work an executor may be asked to start a worker for.
type Job struct {
	ID      string
	AssetID string
	Kind    string
}

// Caps describes what an executor can run and reach.
type Caps struct {
	Name string
	// Push is true when Wake starts a worker (serverless); pull executors no-op.
	Push bool
	// NeedsPublicURL is true when the worker must reach the API over the internet.
	NeedsPublicURL bool
}

// Executor is the narrow interface every compute backend implements.
type Executor interface {
	// Available reports agent heartbeat or provider health.
	Available(ctx context.Context) (bool, error)
	// Wake starts a worker for job; a no-op for pull executors.
	Wake(ctx context.Context, job Job) error
	Capabilities() Caps
}
