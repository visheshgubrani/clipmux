package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// Routing (docs/architecture.md, "Routing and fallback"; D053). A project
// names a primary executor and an optional fallback. Every media job is
// routed when it is enqueued, to its pinned agent or to the primary, and the
// router moves queued jobs between the two. A worker claims only jobs routed
// to its class.

// Executor classes a job can be routed to.
const (
	// RouteWorkers is the built-in worker and PC agents together, whichever
	// is free: the behaviour before routing, and the default primary.
	RouteWorkers = "workers"
	RouteAgents  = "agents"
	RouteBuiltin = "builtin"
	RouteModal   = "modal"
	RouteRunPod  = "runpod"
)

// Route reasons, kept on the job while it is queued and copied to the
// attempt that claims it.
const (
	// ReasonPinned: the source stays on one agent, so only it can run the job.
	ReasonPinned = "pinned"
	// ReasonPrimary: the primary has a worker that could take the job.
	ReasonPrimary = "primary"
	// ReasonPrimaryOffline: no worker of the primary that could take the job
	// was seen within OnlineWithin, so the job went to the fallback.
	ReasonPrimaryOffline = "primary_offline"
	// ReasonQueueAge: the primary is online but the job waited longer than
	// the project's queue-age setting, so it went to the fallback.
	ReasonQueueAge = "queue_age"
	// ReasonNoFallback: the primary is offline and no fallback is set; the
	// job waits for the primary.
	ReasonNoFallback = "no_fallback"
	// ReasonFallbackRefused: the primary is offline, but an earlier attempt
	// on this asset failed in a way that may be the file's fault, so the job
	// waits for the primary rather than reach a fallback.
	ReasonFallbackRefused = "fallback_refused"
	// ReasonFallbackOffline: the primary and the fallback are both offline.
	ReasonFallbackOffline = "fallback_offline"
	// ReasonFallbackLimit: the primary is offline and the fallback provider
	// is at its concurrency or spend limit, or backing off after failed
	// wakes; the job waits for the primary (D055).
	ReasonFallbackLimit = "fallback_limit"
	// ReasonPrimaryLimit: the primary is a provider at its limit; the job
	// waits for room there (or for queue age to send it to the fallback).
	ReasonPrimaryLimit = "primary_limit"
	// ReasonSetupTest: a setup test's sample, which runs on the executor
	// its test names and nowhere else.
	ReasonSetupTest = "setup_test"
)

// provider reports whether a class is a serverless provider, woken by a
// dispatch rather than pulling on its own.
func provider(class string) bool { return class == RouteModal || class == RouteRunPod }

// OnlineWithin is how recently a worker must have called (claim or
// heartbeat) to count as online, for routing and the Workers page.
const OnlineWithin = 60 * time.Second

// CapacityCodes are the failure codes that say nothing about the file: a
// job whose asset failed only with these (or lost leases, or a wake_* code
// once providers exist) may still go to a fallback. Anything else, such as
// transcode_failed, output_invalid, job_timeout or processing_failed, keeps
// the asset off every fallback for good, so a file that kills the encoder
// never reaches a paid machine.
var CapacityCodes = []string{
	CodeAgentRevoked,
	"source_download_failed",
	"upload_failed",
	"scratch_full",
	// A worker couldn't fetch or open its speech model.
	"model_unavailable",
}

// classes are the routes a worker of this executor claims.
func classes(executor string) []string {
	switch executor {
	case RouteBuiltin:
		return []string{RouteWorkers, RouteBuiltin}
	case "agent":
		return []string{RouteWorkers, RouteAgents}
	default:
		return []string{executor}
	}
}

// routeFacts is what deciding one job's route depends on.
type routeFacts struct {
	Primary, Fallback string // Fallback is "" when none is set
	// PrimaryUp and FallbackUp: an online worker of that class could claim
	// this job.
	PrimaryUp, FallbackUp bool
	// PrimaryRoom and FallbackRoom: a provider has room under its limits.
	// Pull executors always have room; a busy one just doesn't claim.
	PrimaryRoom, FallbackRoom bool
	// FallbackEligible is false once any attempt on the asset failed for a
	// reason that may be the file's.
	FallbackEligible bool
	// QueueAge is the queue-age setting, 0 when off; Waited is how long the
	// job has been claimable.
	QueueAge, Waited time.Duration
}

// decideRoute applies the routing table. It is the whole policy; Route
// gathers the facts and writes the answer.
func decideRoute(f routeFacts) (to, reason string) {
	fallbackOK := f.Fallback != "" && f.FallbackUp && f.FallbackRoom && f.FallbackEligible
	if f.PrimaryUp {
		switch {
		case f.QueueAge > 0 && f.Waited >= f.QueueAge && fallbackOK:
			return f.Fallback, ReasonQueueAge
		case !f.PrimaryRoom:
			return f.Primary, ReasonPrimaryLimit
		}
		return f.Primary, ReasonPrimary
	}
	switch {
	case f.Fallback == "":
		return f.Primary, ReasonNoFallback
	case !f.FallbackEligible:
		return f.Primary, ReasonFallbackRefused
	case !f.FallbackUp:
		return f.Primary, ReasonFallbackOffline
	case !f.FallbackRoom:
		return f.Primary, ReasonFallbackLimit
	}
	return f.Fallback, ReasonPrimaryOffline
}

// attemptReason is why an attempt ran where it did, from its job's route
// at the claim. The reasons a job waits on its primary for (the fallback
// missing, barred, offline or full, or the primary at its own limit) explain
// the wait, not the run: once the primary claims, it ran there as the
// primary.
func attemptReason(jobReason string) string {
	switch jobReason {
	case ReasonNoFallback, ReasonFallbackRefused, ReasonFallbackOffline, ReasonFallbackLimit, ReasonPrimaryLimit:
		return ReasonPrimary
	}
	return jobReason
}

// up reports whether an executor class could take the job now: a pull
// executor with an online worker that could claim it, or a verified provider
// whose machines fit it (and only when this queue can wake one).
func (q *Queue) up(class string, row db.JobRouteFactsRow) bool {
	switch class {
	case RouteWorkers:
		return row.BuiltinUp || row.AgentsUp
	case RouteAgents:
		return row.AgentsUp
	case RouteBuiltin:
		return row.BuiltinUp
	case RouteModal:
		return q.Dispatch != nil && row.ModalUp
	case RouteRunPod:
		return q.Dispatch != nil && row.RunpodUp
	}
	return false
}

// room reports whether a class has room for the job: always for pull
// executors, and for a provider while it is under its limits (D055).
func room(class string, row db.JobRouteFactsRow) bool {
	switch class {
	case RouteModal:
		return row.ModalRoom
	case RouteRunPod:
		return row.RunpodRoom
	}
	return true
}

// Route re-evaluates up to limit queued, unpinned media jobs against the
// routing table and moves those whose route changed. Each job is decided in
// its own transaction under its row lock, so a concurrent claim either sees
// the old route or waits for the new one. It returns how many jobs moved.
func (q *Queue) Route(ctx context.Context, limit int) (int, error) {
	jobIDs, err := db.New(q.pool).ListRoutableJobs(ctx, db.ListRoutableJobsParams{ProjectID: q.ProjectID, Limit: int32(limit)})
	if err != nil {
		return 0, fmt.Errorf("list routable jobs: %w", err)
	}
	moved := 0
	for _, id := range jobIDs {
		err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
			qs := db.New(tx)
			job, err := qs.LockRoutableJob(ctx, id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // claimed, cancelled or being routed elsewhere
			}
			if err != nil {
				return err
			}
			row, err := qs.JobRouteFacts(ctx, db.JobRouteFactsParams{
				JobID: id, OnlineSeconds: OnlineWithin.Seconds(), CapacityCodes: CapacityCodes,
			})
			if err != nil {
				return err
			}
			if row.HasLiveDispatch {
				return nil // a woken machine keeps its job (D054)
			}
			var to, reason string
			var wake bool
			if row.ForcedExecutor != "" {
				to, reason = row.ForcedExecutor, ReasonSetupTest
				// Its own setup test needs no verification, but still
				// respects the provider's limits.
				wake = q.Dispatch != nil && row.ForcedConfigured && room(to, row)
			} else {
				to, reason = decideRoute(routeFacts{
					Primary:          row.PrimaryExecutor,
					Fallback:         row.FallbackExecutor.String,
					PrimaryUp:        q.up(row.PrimaryExecutor, row),
					FallbackUp:       row.FallbackExecutor.Valid && q.up(row.FallbackExecutor.String, row),
					PrimaryRoom:      room(row.PrimaryExecutor, row),
					FallbackRoom:     room(row.FallbackExecutor.String, row),
					FallbackEligible: row.FallbackEligible.Bool,
					QueueAge:         time.Duration(row.QueueAgeFallbackMinutes.Int32) * time.Minute,
					Waited:           time.Duration(row.WaitedSeconds * float64(time.Second)),
				})
				wake = q.up(to, row) && room(to, row)
			}
			if job.RoutedTo.String != to || job.RouteReason.String != reason {
				moved++
				if err := qs.SetJobRoute(ctx, db.SetJobRouteParams{ID: id, RoutedTo: text(to), RouteReason: text(reason)}); err != nil {
					return err
				}
				job.RoutedTo, job.RouteReason = text(to), text(reason)
			}
			if provider(to) && wake {
				// Routed to a provider with nothing awake for it: wake one.
				return q.Dispatch(ctx, qs, job, to)
			}
			return nil
		})
		if err != nil {
			return moved, fmt.Errorf("route job %s: %w", id, err)
		}
	}
	return moved, nil
}

// TouchBuiltin records that the built-in worker called, with the free disk
// and the speech its claim reported (0 and nil from a heartbeat, which
// says neither). Writes are coalesced to one per 10 s.
func (q *Queue) TouchBuiltin(ctx context.Context, freeDiskBytes int64, speech *bool) error {
	var sp pgtype.Bool
	if speech != nil {
		sp = pgtype.Bool{Bool: *speech, Valid: true}
	}
	return db.New(q.pool).TouchBuiltinWorker(ctx, db.TouchBuiltinWorkerParams{
		ID: q.ProjectID, FreeDiskBytes: pgtype.Int8{Int64: freeDiskBytes, Valid: freeDiskBytes > 0}, Speech: sp,
	})
}
