// Package queue is the leased Postgres job queue (docs/architecture.md,
// "Compute" and "Leases").
//
//   - Claim takes the next queued media job with FOR UPDATE SKIP LOCKED and
//     creates its attempt (token stored hashed), a 90 s lease and a fresh
//     output. move and delete jobs are never claimable by executors.
//   - Liveness is one rule: an attempt is live while it is the latest
//     attempt of a job that isn't cancelled. Heartbeat and Fail (and later
//     sign and complete) check it, and tell stale workers to stop.
//   - SweepLostLeases marks expired leases lost and requeues the job; a slow
//     worker that is still the latest attempt can heartbeat back.
//   - Retry budgets are separate: 5 lost leases, 3 reported failures.
//     Permanent failures end the job at once. A failed re-encode leaves a
//     ready asset ready and emits video.asset.reencode_failed.
//
// Lock order everywhere is job, then attempt, then asset.
package queue
