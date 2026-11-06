package dispatch_test

// Real-Postgres tests for dispatches and one-time keys (D054), against a
// fake provider that records what it was asked to start and stop.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// fakeProvider starts nothing; it records each Wake and Stop.
type fakeProvider struct {
	mu      sync.Mutex
	wakes   []dispatch.Wake
	stops   []string
	failing int // the next n Wakes fail
}

func (p *fakeProvider) Wake(_ context.Context, w dispatch.Wake) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wakes = append(p.wakes, w)
	if p.failing > 0 {
		p.failing--
		return "", errors.New("provider unavailable")
	}
	return "pod-" + w.DispatchID, nil
}

func (p *fakeProvider) Stop(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stops = append(p.stops, id)
	return nil
}

type rig struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *queue.Queue
	svc  *dispatch.Service
	prov *fakeProvider
}

// newRig has RunPod connected and verified as the fallback for PCs, and no
// PC online, so a new job goes to RunPod.
func newRig(t *testing.T) *rig {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secrets.NewBox(1, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, pool: pool, q: queue.New(pool), prov: &fakeProvider{}}
	r.svc = dispatch.New(pool, box, func() string { return "https://video.example.com" })
	r.svc.Resolve = func(_ context.Context, cfg db.ExecutorConfig) (dispatch.Executor, error) {
		if cfg.Kind != "runpod" {
			t.Fatalf("resolved %s", cfg.Kind)
		}
		return r.prov, nil
	}
	r.q.Dispatch = r.svc.Route
	r.exec(`INSERT INTO executor_configs (id, project_id, kind, config, max_concurrent, monthly_limit_cents, rate_cents_per_hour, disk_bytes, verified_at)
		VALUES ('exe_runpod', 'prj_default', 'runpod', '{"gpu": "L4"}', 2, 5000, 80, 50000000000, now())`)
	r.exec(`UPDATE projects SET primary_executor = 'agents', fallback_executor = 'runpod' WHERE id = 'prj_default'`)
	return r
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) job(assetID string) db.Job {
	r.t.Helper()
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', 'sto_default')`, assetID)
	j, err := queue.Enqueue(context.Background(), r.pool, queue.Job{ProjectID: "prj_default", AssetID: assetID, Kind: "transcode"})
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

func (r *rig) route() {
	r.t.Helper()
	if _, err := r.q.Route(context.Background(), 100); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) publish() int {
	r.t.Helper()
	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) dispatches(jobID string) []db.Dispatch {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `SELECT * FROM dispatches WHERE job_id = $1 ORDER BY created_at, id`, jobID)
	if err != nil {
		r.t.Fatal(err)
	}
	ds, err := pgx.CollectRows(rows, pgx.RowToStructByPos[db.Dispatch])
	if err != nil {
		r.t.Fatal(err)
	}
	return ds
}

func (r *rig) claim(w queue.Worker) *queue.Claim {
	r.t.Helper()
	c, err := r.q.Claim(context.Background(), w)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// woken routes a job to RunPod, publishes the wake and returns the key the
// machine was given.
func (r *rig) woken(assetID string) (db.Job, string) {
	r.t.Helper()
	job := r.job(assetID)
	r.route()
	r.publish()
	w := r.prov.wakes[len(r.prov.wakes)-1]
	return job, w.Env["CLIPMUX_WORKER_KEY"]
}

func TestARoutedJobWakesOneMachineWhoseKeyClaimsThatJobOnce(t *testing.T) {
	r := newRig(t)
	job := r.job("ast_1")
	other := r.job("ast_2") // routed to RunPod too, with its own dispatch
	r.route()
	ds := r.dispatches(job.ID)
	if len(ds) != 1 || ds[0].Status != "waking" || ds[0].Kind != "runpod" || ds[0].RateCentsPerHour != 80 {
		t.Fatalf("dispatches = %+v, want one waking RunPod dispatch at 80 cents an hour", ds)
	}

	if n := r.publish(); n != 2 {
		t.Fatalf("published %d, want both wakes", n)
	}
	var w dispatch.Wake
	for _, x := range r.prov.wakes {
		if x.DispatchID == ds[0].ID {
			w = x
		}
	}
	key := w.Env["CLIPMUX_WORKER_KEY"]
	if w.Name != "clipmux-"+ds[0].ID || w.Env["CLIPMUX_SERVER"] != "https://video.example.com" ||
		!strings.HasPrefix(key, "cmxd_") || len(w.Env) != 2 || string(w.Config) != `{"gpu": "L4"}` {
		t.Fatalf("wake = %+v", w)
	}
	if d := r.dispatches(job.ID)[0]; d.InstanceID.String != "pod-"+ds[0].ID || !d.WokenAt.Valid {
		t.Fatalf("dispatch after wake = %+v", d)
	}
	// The key is never stored as itself.
	var leaked int
	if err := r.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM dispatches WHERE key_hash = convert_to($1, 'UTF8') OR position(convert_to($1, 'UTF8') IN key_sealed) > 0`, key,
	).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the key is stored in the clear (%d rows, %v)", leaked, err)
	}

	// A second router pass leaves a woken job alone.
	r.route()
	if ds := r.dispatches(job.ID); len(ds) != 1 {
		t.Fatalf("a second pass made %d dispatches", len(ds))
	}

	// Nobody else may take a job a machine was woken for.
	if c := r.claim(queue.Worker{Executor: "builtin"}); c != nil {
		t.Fatalf("the built-in worker claimed %s", c.Job.ID)
	}
	wk, ok := r.svc.AuthenticateWorker(context.Background(), key)
	if !ok || wk.Executor != "runpod" || wk.JobID != job.ID {
		t.Fatalf("AuthenticateWorker = %+v %v", wk, ok)
	}
	c := r.claim(wk)
	if c == nil || c.Job.ID != job.ID || c.Job.ID == other.ID {
		t.Fatalf("the key claimed %+v, want only %s", c, job.ID)
	}
	if c.Attempt.Executor != "runpod" || c.Attempt.DispatchID.String != ds[0].ID || c.Attempt.RouteReason.String != "primary_offline" {
		t.Fatalf("attempt = executor %s dispatch %s reason %s", c.Attempt.Executor, c.Attempt.DispatchID.String, c.Attempt.RouteReason.String)
	}
	if d := r.dispatches(job.ID)[0]; d.Status != "claimed" || d.AttemptID.String != c.Attempt.ID {
		t.Fatalf("dispatch after claim = %s attempt %s", d.Status, d.AttemptID.String)
	}
	if _, ok := r.svc.AuthenticateWorker(context.Background(), key); ok {
		t.Fatal("a spent key authenticated again")
	}
}

func TestAKeyStopsWorkingAtItsClaimDeadline(t *testing.T) {
	r := newRig(t)
	job, key := r.woken("ast_1")
	r.exec(`UPDATE dispatches SET claim_deadline = now() - interval '1 second' WHERE job_id = $1`, job.ID)
	if _, ok := r.svc.AuthenticateWorker(context.Background(), key); ok {
		t.Fatal("a key past its claim deadline authenticated")
	}
	if _, ok := r.svc.AuthenticateWorker(context.Background(), "cmxd_"+strings.Repeat("A", 26)); ok {
		t.Fatal("an unknown key authenticated")
	}
}

func TestARetriedWakeHandsTheSameMachineTheSameKey(t *testing.T) {
	r := newRig(t)
	r.prov.failing = 1
	job := r.job("ast_1")
	r.route()
	if n := r.publish(); n != 0 {
		t.Fatalf("published %d with the provider failing", n)
	}
	var lastError string
	if err := r.pool.QueryRow(context.Background(), `SELECT last_error FROM outbox WHERE kind = 'wake'`).Scan(&lastError); err != nil || lastError != "wake_failed" {
		t.Fatalf("outbox last_error = %q, %v", lastError, err)
	}
	r.exec(`UPDATE outbox SET available_at = now() WHERE kind = 'wake'`)
	if n := r.publish(); n != 1 {
		t.Fatalf("published %d on the retry", n)
	}
	first, second := r.prov.wakes[0], r.prov.wakes[1]
	if first.Name != second.Name || first.Env["CLIPMUX_WORKER_KEY"] != second.Env["CLIPMUX_WORKER_KEY"] {
		t.Fatalf("the retry changed the machine: %s/%s", first.Name, second.Name)
	}
	if ds := r.dispatches(job.ID); len(ds) != 1 {
		t.Fatalf("a retried wake made %d dispatches", len(ds))
	}
}

func TestAJobThatWentAwayIsNeverWoken(t *testing.T) {
	r := newRig(t)
	job := r.job("ast_1")
	r.route()
	r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1`, job.ID)
	r.publish()
	if len(r.prov.wakes) != 0 {
		t.Fatalf("woke %d machines for a cancelled job", len(r.prov.wakes))
	}
	d := r.dispatches(job.ID)[0]
	if d.Status != "ended" || d.EndReason.String != "job_gone" {
		t.Fatalf("dispatch = %s %s, want ended job_gone", d.Status, d.EndReason.String)
	}
	var stops int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'stop'`).Scan(&stops)
	if stops != 0 {
		t.Fatal("queued a stop for a machine that never started")
	}
}

func TestAReleasedClaimLeavesTheDispatchClaimable(t *testing.T) {
	r := newRig(t)
	job, key := r.woken("ast_1")
	wk, _ := r.svc.AuthenticateWorker(context.Background(), key)
	c := r.claim(wk)
	// The machine hung up before it got the payload.
	if err := r.q.Release(context.Background(), c.Token); err != nil {
		t.Fatal(err)
	}
	if d := r.dispatches(job.ID)[0]; d.Status != "waking" || d.AttemptID.Valid {
		t.Fatalf("dispatch after release = %s attempt %v", d.Status, d.AttemptID)
	}
	wk, ok := r.svc.AuthenticateWorker(context.Background(), key)
	if !ok {
		t.Fatal("the released key no longer authenticates")
	}
	if again := r.claim(wk); again == nil || again.Job.ID != job.ID {
		t.Fatalf("the key couldn't claim its job again: %+v", again)
	}
}

func TestEndingAWokenDispatchStopsItsMachine(t *testing.T) {
	r := newRig(t)
	job, _ := r.woken("ast_1")
	d := r.dispatches(job.ID)[0]
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return dispatch.End(context.Background(), db.New(tx), d.ID, "wake_timeout")
	}); err != nil {
		t.Fatal(err)
	}
	r.publish()
	if len(r.prov.stops) != 1 || r.prov.stops[0] != "pod-"+d.ID {
		t.Fatalf("stops = %v, want pod-%s", r.prov.stops, d.ID)
	}
	if d := r.dispatches(job.ID)[0]; !d.StoppedAt.Valid || d.EndReason.String != "wake_timeout" {
		t.Fatalf("dispatch = %+v", d)
	}
	// With the dispatch ended, the router may wake another machine.
	r.route()
	if ds := r.dispatches(job.ID); len(ds) != 2 || ds[1].Status != "waking" {
		t.Fatalf("after the end: %d dispatches", len(ds))
	}
}

func TestAnUnverifiedProviderTakesOnlyItsSetupTest(t *testing.T) {
	r := newRig(t)
	r.exec(`UPDATE executor_configs SET verified_at = NULL`)
	job := r.job("ast_1")
	r.route()
	if j, _ := db.New(r.pool).GetJob(context.Background(), job.ID); j.RoutedTo.String != "agents" || j.RouteReason.String != "fallback_offline" {
		t.Fatalf("job routed (%s, %s), want to wait for the PCs", j.RoutedTo.String, j.RouteReason.String)
	}
	if len(r.dispatches(job.ID)) != 0 {
		t.Fatal("dispatched to an unverified provider")
	}

	// The setup test after Connect RunPod runs there and nowhere else, even
	// with the built-in worker online and the primary set to it.
	r.exec(`UPDATE projects SET primary_executor = 'workers', fallback_executor = NULL, builtin_seen_at = now() WHERE id = 'prj_default'`)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_sample', 'prj_default', 'app_dashboard', 'waiting_upload', 'sto_default')`)
	r.exec(`INSERT INTO setup_tests (id, project_id, trigger, executor, status, asset_id, started_at)
		VALUES ('stt_1', 'prj_default', 'executor_connected', 'runpod', 'running', 'ast_sample', now())`)
	sample, err := queue.Enqueue(context.Background(), r.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_sample", Kind: "transcode"})
	if err != nil {
		t.Fatal(err)
	}
	if sample.RoutedTo.String != "runpod" || sample.RouteReason.String != "setup_test" {
		t.Fatalf("the sample routed (%s, %s), want (runpod, setup_test)", sample.RoutedTo.String, sample.RouteReason.String)
	}
	r.route()
	if ds := r.dispatches(sample.ID); len(ds) != 1 {
		t.Fatalf("the sample has %d dispatches, want 1", len(ds))
	}
	if c := r.claim(queue.Worker{Executor: "builtin"}); c != nil && c.Job.ID == sample.ID {
		t.Fatal("the built-in worker took the provider's setup test")
	}
}
