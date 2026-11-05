// Package queue is the leased Postgres job queue: claim with
// FOR UPDATE SKIP LOCKED, heartbeats, the attempt liveness rule, lost-lease
// recovery and retry budgets (5 lost, 3 failed). Lands in P1-T03.
package queue
