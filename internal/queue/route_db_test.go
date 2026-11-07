package queue_test

// Real-Postgres tests for routing (docs/architecture.md, "Routing and
// fallback"; D053). Liveness and waiting are simulated by moving
// last_seen_at, builtin_seen_at and available_at into the past, so no test
// sleeps.

import (
	"context"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/queue"
)

// routing sets the project's primary, fallback ("" for none) and queue-age
// minutes (nil for off).
func (f *fixture) routing(primary, fallback string, queueAge any) {
	f.t.Helper()
	f.exec(`UPDATE projects SET primary_executor = $1, fallback_executor = nullif($2, ''), queue_age_fallback_minutes = $3
		WHERE id = 'prj_default'`, primary, fallback, queueAge)
}

// agent adds an approved PC agent last seen secondsAgo ago.
func (f *fixture) agent(id string, sandboxed bool, secondsAgo int) queue.Worker {
	f.t.Helper()
	f.exec(`INSERT INTO agents (id, project_id, name, key_hash, status, sandboxed, last_seen_at)
		VALUES ($1, 'prj_default', $1, convert_to($1, 'UTF8'), 'approved', $2, now() - make_interval(secs => $3))`,
		id, sandboxed, secondsAgo)
	return queue.Worker{Executor: "agent", AgentID: id, Untrusted: !sandboxed}
}

func (f *fixture) agentSeen(id string, secondsAgo int) {
	f.exec(`UPDATE agents SET last_seen_at = now() - make_interval(secs => $2) WHERE id = $1`, id, secondsAgo)
}

// builtinSeen records the built-in worker's last call, secondsAgo ago.
func (f *fixture) builtinSeen(secondsAgo int) {
	f.exec(`UPDATE projects SET builtin_seen_at = now() - make_interval(secs => $1) WHERE id = 'prj_default'`, secondsAgo)
}

// publicAsset is an asset owned by an integration app rather than the
// owner's Dashboard app: a public upload.
func (f *fixture) publicAsset(id string) string {
	f.t.Helper()
	f.exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS') ON CONFLICT DO NOTHING`)
	f.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_lms', 'processing', 'sto_default')`, id)
	return id
}

func (f *fixture) route() int {
	f.t.Helper()
	n, err := f.q.Route(context.Background(), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) wantRoute(jobID, to, reason string) {
	f.t.Helper()
	j := f.job(jobID)
	if j.RoutedTo.String != to || j.RouteReason.String != reason {
		f.t.Fatalf("job %s routed (%s, %s), want (%s, %s)", jobID, j.RoutedTo.String, j.RouteReason.String, to, reason)
	}
}

// claimable makes a job claimable now, as if its backoff had passed.
func (f *fixture) claimable(jobID string) {
	f.exec(`UPDATE jobs SET available_at = now() WHERE id = $1`, jobID)
}

func TestEnqueueRoutesMediaJobsToThePrimaryAndPinnedJobsToTheirAgent(t *testing.T) {
	f := newFixture(t)
	f.wantRoute(f.enqueue(f.asset("ast_1", false), "transcode").ID, "workers", "primary")

	f.routing("agents", "", nil)
	f.wantRoute(f.enqueue(f.asset("ast_2", true), "reencode").ID, "agents", "primary")

	f.agent("agt_1", true, 0)
	pinned, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: f.asset("ast_3", false), Kind: "transcode", PinnedAgentID: "agt_1"})
	if err != nil {
		t.Fatal(err)
	}
	f.wantRoute(pinned.ID, "agents", "pinned")

	// Control-plane jobs aren't routed: no worker ever claims them.
	f.wantRoute(f.enqueue(f.asset("ast_4", true), "delete").ID, "", "")
}

func TestAPrimaryOfPCsLeavesTheBuiltinWorkerIdle(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "", nil)
	f.builtinSeen(0)
	pc := f.agent("agt_1", true, 0)
	job := f.enqueue(f.asset("ast_1", false), "transcode")

	if c := f.claim(builtin); c != nil {
		t.Fatalf("the built-in worker claimed %s, routed to PCs", c.Job.ID)
	}
	c := f.claim(pc)
	if c == nil || c.Job.ID != job.ID || c.Attempt.RouteReason.String != "primary" {
		t.Fatalf("the PC's claim = %+v, want %s routed primary", c, job.ID)
	}
}

func TestTheRouterSendsWorkToTheFallbackOnlyWhileThePrimaryIsOffline(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	pc := f.agent("agt_1", true, 0)

	// Online primary: nothing moves.
	first := f.enqueue(f.asset("ast_1", false), "transcode")
	if n := f.route(); n != 0 {
		t.Fatalf("route moved %d jobs with the primary online", n)
	}
	f.wantRoute(first.ID, "agents", "primary")

	// The PC goes quiet for longer than OnlineWithin (60 s).
	f.agentSeen("agt_1", 61)
	if n := f.route(); n != 1 {
		t.Fatalf("route moved %d jobs, want 1", n)
	}
	f.wantRoute(first.ID, "builtin", "primary_offline")
	if c := f.claim(pc); c != nil {
		t.Fatal("the PC claimed a job routed to the fallback")
	}
	c := f.claim(builtin)
	if c == nil || c.Job.ID != first.ID || c.Attempt.RouteReason.String != "primary_offline" {
		t.Fatalf("the fallback's claim = %+v, want %s routed primary_offline", c, first.ID)
	}

	// A job routed to the fallback but not yet claimed goes back to the
	// primary when it returns.
	second := f.enqueue(f.asset("ast_2", false), "transcode")
	f.route()
	f.wantRoute(second.ID, "builtin", "primary_offline")
	f.agentSeen("agt_1", 0)
	f.route()
	f.wantRoute(second.ID, "agents", "primary")

	// Both gone: the job waits for the primary.
	f.agentSeen("agt_1", 61)
	f.builtinSeen(61)
	f.route()
	f.wantRoute(second.ID, "agents", "fallback_offline")

	// No fallback at all.
	f.routing("agents", "", nil)
	f.route()
	f.wantRoute(second.ID, "agents", "no_fallback")
}

func TestALostLeaseRetriesOnTheFallbackWhileThePrimaryIsOffline(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	pc := f.agent("agt_1", true, 0)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	first := f.claim(pc)

	// The PC sleeps mid-encode: its lease lapses and it stops calling.
	f.agentSeen("agt_1", 120)
	f.expireLease(first.Attempt.ID)
	f.sweep()
	f.route()
	f.wantRoute(job.ID, "builtin", "primary_offline")

	retry := f.claim(builtin)
	if retry == nil || retry.Job.ID != job.ID || retry.Attempt.AttemptNumber != 2 || retry.Output.ID == first.Output.ID {
		t.Fatalf("retry = %+v, want attempt 2 of %s on a new output", retry, job.ID)
	}
	if got := f.job(job.ID); got.LostCount != 1 || got.FailedCount != 0 {
		t.Fatalf("budgets = lost %d failed %d, want 1 and 0", got.LostCount, got.FailedCount)
	}
}

func TestQueueAgeSendsAJobWaitingOnABusyPrimaryToTheFallback(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", 10)
	f.builtinSeen(0)
	f.agent("agt_1", true, 0)
	old := f.enqueue(f.asset("ast_1", false), "transcode")
	recent := f.enqueue(f.asset("ast_2", false), "transcode")
	f.exec(`UPDATE jobs SET available_at = now() - interval '11 minutes' WHERE id = $1`, old.ID)
	f.exec(`UPDATE jobs SET available_at = now() - interval '9 minutes' WHERE id = $1`, recent.ID)

	f.route()
	f.wantRoute(old.ID, "builtin", "queue_age")
	f.wantRoute(recent.ID, "agents", "primary")

	// Off, the default: however long it waited, the job stays.
	f.routing("agents", "builtin", nil)
	f.route()
	f.wantRoute(old.ID, "agents", "primary")
}

func TestAnUnsandboxedPCDoesNotCountAsAPrimaryForPublicUploads(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	pc := f.agent("agt_1", false, 0)
	public := f.enqueue(f.publicAsset("ast_public"), "transcode")
	owners := f.enqueue(f.asset("ast_owner", false), "transcode")

	f.route()
	f.wantRoute(public.ID, "builtin", "primary_offline")
	f.wantRoute(owners.ID, "agents", "primary")
	if c := f.claim(pc); c == nil || c.Job.ID != owners.ID {
		t.Fatalf("the unsandboxed PC claimed %+v, want only the owner's upload %s", c, owners.ID)
	}
}

func TestAFileThatFailedNeverReachesTheFallbackEvenOnReencode(t *testing.T) {
	cases := []struct {
		code     string
		fallback bool
	}{
		{"transcode_failed", false},
		{"output_invalid", false},
		{"job_timeout", false},
		{"commit_failed", false},
		{"source_download_failed", true},
		{"upload_failed", true},
		{"scratch_full", true},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			f := newFixture(t)
			f.routing("agents", "builtin", nil)
			f.builtinSeen(0)
			pc := f.agent("agt_1", true, 0)
			job := f.enqueue(f.asset("ast_1", true), "reencode")
			a := f.claim(pc)
			if _, err := f.q.Fail(context.Background(), a.Token, queue.Failure{Code: c.code}); err != nil {
				t.Fatal(err)
			}
			f.claimable(job.ID)
			f.agentSeen("agt_1", 61)
			f.route()
			if !c.fallback {
				f.wantRoute(job.ID, "agents", "fallback_refused")
				// The bar is on the asset: a new re-encode of it is barred too.
				f.exec(`UPDATE jobs SET status = 'failed', finished_at = now() WHERE id = $1`, job.ID)
				again := f.enqueue("ast_1", "reencode")
				f.route()
				f.wantRoute(again.ID, "agents", "fallback_refused")
				return
			}
			f.wantRoute(job.ID, "builtin", "primary_offline")
		})
	}
}

func TestAPinnedJobNeverLeavesItsAgent(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.builtinSeen(0)
	f.agent("agt_1", true, 3600)
	job, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: f.asset("ast_1", false), Kind: "transcode", PinnedAgentID: "agt_1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.route(); n != 0 {
		t.Fatalf("route moved %d jobs, want the pinned job left alone", n)
	}
	f.wantRoute(job.ID, "agents", "pinned")
	if c := f.claim(builtin); c != nil {
		t.Fatal("the built-in worker claimed a job pinned to a PC")
	}
}

func TestTheBuiltinWorkerCountsOnlyWithRoomForTheSource(t *testing.T) {
	f := newFixture(t)
	f.routing("agents", "builtin", nil)
	f.agent("agt_1", true, 120)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	// A 2 GB upload needs three times that free.
	f.exec(`INSERT INTO uploads (id, project_id, asset_id, multipart_id, declared_size, part_size, token_hash, status)
		VALUES ('upl_1', 'prj_default', 'ast_1', 'mp', 2000000000, 33554432, 'h', 'completed')`)

	if err := f.q.TouchBuiltin(context.Background(), 5_000_000_000, nil); err != nil {
		t.Fatal(err)
	}
	f.route()
	f.wantRoute(job.ID, "agents", "fallback_offline")

	f.exec(`UPDATE projects SET builtin_free_disk_bytes = 7000000000 WHERE id = 'prj_default'`)
	f.route()
	f.wantRoute(job.ID, "builtin", "primary_offline")
}
