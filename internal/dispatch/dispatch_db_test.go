package dispatch_test

// Real-Postgres tests for dispatches and one-time keys (D054), against a
// fake provider that records what it was asked to start and stop.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// fakeProvider starts nothing; it records each Wake and Stop, and reports
// as running every machine it started and hasn't stopped, plus extra.
type fakeProvider struct {
	mu      sync.Mutex
	wakes   []dispatch.Wake
	stops   []string
	failing int // the next n Wakes fail
	// preparing: the next n Wakes report the provider still preparing.
	preparing int
	// hangFrom: the hangFrom-th Wake (1-based) and later block until their
	// context ends; 0 never.
	hangFrom  int
	gate      chan struct{} // if non-nil, Wake blocks until closed or ctx ends
	entered   chan struct{} // notified when Wake enters gate/hang
	wakeDelay time.Duration
	inFlight  int
	extra     []dispatch.Instance
}

func (p *fakeProvider) currentInFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inFlight
}

func (p *fakeProvider) stopList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.stops...)
}

func (p *fakeProvider) Running(context.Context) ([]dispatch.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stopped := map[string]bool{}
	for _, id := range p.stops {
		stopped[id] = true
	}
	var out []dispatch.Instance
	for _, w := range p.wakes {
		if id := "pod-" + w.DispatchID; !stopped[id] {
			out = append(out, dispatch.Instance{ID: id, Name: w.Name})
		}
	}
	for _, m := range p.extra {
		if !stopped[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (p *fakeProvider) Wake(ctx context.Context, w dispatch.Wake) (string, error) {
	p.mu.Lock()
	p.inFlight++
	defer func() { p.inFlight--; p.mu.Unlock() }()
	p.wakes = append(p.wakes, w)
	n := len(p.wakes)
	hang := p.hangFrom > 0 && n >= p.hangFrom
	gate := p.gate
	entered := p.entered
	delay := p.wakeDelay
	if hang || gate != nil {
		p.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if hang {
			<-ctx.Done()
			p.mu.Lock()
			return "", ctx.Err()
		}
		select {
		case <-gate:
		case <-ctx.Done():
			p.mu.Lock()
			return "", ctx.Err()
		}
		p.mu.Lock()
	}
	if delay > 0 {
		p.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			p.mu.Lock()
			return "", ctx.Err()
		}
		p.mu.Lock()
	}
	if p.preparing > 0 {
		p.preparing--
		return "", fmt.Errorf("image still converting: %w", dispatch.ErrPreparing)
	}
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
	t      *testing.T
	pool   *pgxpool.Pool
	q      *queue.Queue
	svc    *dispatch.Service
	prov   *fakeProvider
	cancel context.CancelFunc
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
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.svc.Start(ctx)
	t.Cleanup(func() {
		r.cancel()
		r.svc.Wait()
	})
	r.q.Dispatch = r.svc.Route
	r.exec(`INSERT INTO executor_configs (id, project_id, kind, config, secrets_encrypted, max_concurrent, monthly_limit_cents, rate_cents_per_hour, disk_bytes, verified_at)
		VALUES ('exe_runpod', 'prj_default', 'runpod', '{"gpu": "L4"}', 'sealed', 2, 5000, 80, 50000000000, now())`)
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
	r.svc.Wait()
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
		!strings.HasPrefix(key, "cmxd_") || w.Env["CLIPMUX_SPEECH"] != "off" || w.Env["CLIPMUX_FOLLOW_ON_SPEECH"] != "auto" ||
		len(w.Env) != 4 || string(w.Config) != `{"gpu": "L4"}` {
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

// A crash after the provider started the machine but before the publisher
// committed replays the wake: same name, so the provider finds its machine,
// and same key, so that machine can still claim (D054).
func TestAReplayedWakeHandsTheSameMachineTheSameKey(t *testing.T) {
	r := newRig(t)
	job, key := r.woken("ast_1")
	r.exec(`UPDATE outbox SET done_at = NULL, available_at = now() WHERE kind = 'wake'`)
	r.exec(`UPDATE dispatches SET instance_id = NULL, woken_at = NULL WHERE job_id = $1`, job.ID)
	if n := r.publish(); n != 1 {
		t.Fatalf("published %d on the replay", n)
	}
	first, second := r.prov.wakes[0], r.prov.wakes[1]
	if first.Name != second.Name || second.Env["CLIPMUX_WORKER_KEY"] != key {
		t.Fatalf("the replay changed the machine: %s/%s", first.Name, second.Name)
	}
	if ds := r.dispatches(job.ID); len(ds) != 1 {
		t.Fatalf("a replayed wake made %d dispatches", len(ds))
	}
}

// A provider that fails to start a machine ends that dispatch, so the job
// returns to routing without spending a budget; three failures in a row
// back the provider off for 5 minutes, and jobs wait for the primary
// (D055).
func TestFailedWakesReturnTheJobAndBackTheProviderOff(t *testing.T) {
	r := newRig(t)
	r.prov.failing = 3
	job := r.job("ast_1")
	for i := range 3 {
		r.route()
		r.publish()
		ds := r.dispatches(job.ID)
		if len(ds) != i+1 || ds[i].Status != "ended" || ds[i].EndReason.String != "wake_failed" {
			t.Fatalf("after failure %d: %d dispatches, last %s %s", i+1, len(ds), ds[len(ds)-1].Status, ds[len(ds)-1].EndReason.String)
		}
	}
	j, _ := db.New(r.pool).GetJob(context.Background(), job.ID)
	if j.Status != "queued" || j.LostCount != 0 || j.FailedCount != 0 {
		t.Fatalf("job = %s lost %d failed %d, want queued with no budget spent", j.Status, j.LostCount, j.FailedCount)
	}
	r.route()
	j, _ = db.New(r.pool).GetJob(context.Background(), job.ID)
	if j.RoutedTo.String != "agents" || j.RouteReason.String != "fallback_limit" || len(r.dispatches(job.ID)) != 3 {
		t.Fatalf("during backoff: routed (%s, %s) with %d dispatches", j.RoutedTo.String, j.RouteReason.String, len(r.dispatches(job.ID)))
	}
	r.exec(`UPDATE executor_configs SET backoff_until = now() - interval '1 second'`)
	r.route()
	r.publish()
	if ds := r.dispatches(job.ID); len(ds) != 4 || !ds[3].InstanceID.Valid {
		t.Fatalf("after the backoff: %d dispatches", len(ds))
	}
}

// A provider still preparing (Modal converting a new image, D097) leaves
// the dispatch waking and its wake due again later, with no failure
// counted; the next try wakes it.
func TestAProviderStillPreparingIsAskedAgainLater(t *testing.T) {
	r := newRig(t)
	r.prov.preparing = 1
	job := r.job("ast_1")
	r.route()
	r.publish()
	ds := r.dispatches(job.ID)
	if len(ds) != 1 || ds[0].Status != "waking" || ds[0].InstanceID.Valid {
		t.Fatalf("dispatches %+v: want one still waking", ds)
	}
	var failures, attempts int
	if err := r.pool.QueryRow(context.Background(), `SELECT wake_failures FROM executor_configs WHERE id = 'exe_runpod'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("wake failures %d (%v), want 0", failures, err)
	}
	if err := r.pool.QueryRow(context.Background(), `SELECT attempts FROM outbox WHERE kind = 'wake' AND done_at IS NULL`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("the wake row: attempts %d (%v), want 1 and pending", attempts, err)
	}
	r.exec(`UPDATE outbox SET available_at = now() WHERE kind = 'wake'`)
	r.publish()
	if ds := r.dispatches(job.ID); len(ds) != 1 || ds[0].Status != "waking" || ds[0].InstanceID.String != "pod-"+ds[0].ID {
		t.Fatalf("after the retry: %+v, want the same dispatch woken", ds)
	}
}

// A provider call that hangs fails at its own bound as a failed wake, so
// one stuck provider can't hold the pass (and the stops behind it) until
// the pass times out.
func TestAHungWakeFailsAtItsCallTimeout(t *testing.T) {
	r := newRig(t)
	r.prov.hangFrom = 1
	r.svc.CallTimeout = 200 * time.Millisecond
	job := r.job("ast_1")
	r.route()
	started := time.Now()
	r.publish()
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the pass took %s", took)
	}
	if ds := r.dispatches(job.ID); len(ds) != 1 || ds[0].Status != "ended" || ds[0].EndReason.String != "wake_failed" {
		t.Fatalf("dispatches %+v: want one ended wake_failed", ds)
	}
}

// Stops run beside a hanging wake: while a wake is blocked in flight, a
// stop queued after it still finishes and records its StoppedAt, and
// cancelling the service leaves the hanging wake pending without marking it
// woken (D097, D149).
func TestAStopIsNeverHeldUpByAHangingWake(t *testing.T) {
	r := newRig(t)
	first, _ := r.woken("ast_1")
	d := r.dispatches(first.ID)[0]
	r.prov.hangFrom = 2
	r.prov.entered = make(chan struct{}, 1)
	second := r.job("ast_2")
	r.route()
	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil || n != 1 {
		t.Fatalf("Publish wake = %d, %v; want 1, nil", n, err)
	}
	select {
	case <-r.prov.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hanging wake never entered")
	}

	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		return dispatch.End(context.Background(), db.New(tx), d.ID, "attempt_ended")
	}); err != nil {
		t.Fatal(err)
	}
	n, err = r.svc.Publish(context.Background(), 20)
	if err != nil || n != 1 {
		t.Fatalf("Publish stop = %d, %v; want 1, nil", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.prov.stopList()) == 1 && r.dispatches(first.ID)[0].StoppedAt.Valid {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stops := r.prov.stopList(); len(stops) != 1 {
		t.Fatalf("stops = %v, want the first machine's", stops)
	}
	if d := r.dispatches(first.ID)[0]; !d.StoppedAt.Valid {
		t.Fatal("the stop happened but wasn't recorded")
	}
	if n := r.prov.currentInFlight(); n != 1 {
		t.Fatalf("in-flight wakes = %d, want 1 still hanging", n)
	}
	r.cancel()
	r.svc.Wait()
	if ds := r.dispatches(second.ID); len(ds) != 1 || ds[0].Status != "waking" || ds[0].InstanceID.Valid {
		t.Fatalf("second job's dispatches %+v: want one still waking", ds)
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

func (r *rig) reap() int {
	r.t.Helper()
	n, err := r.svc.Reap(context.Background(), 100)
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *rig) ended(jobID string) db.Dispatch {
	r.t.Helper()
	ds := r.dispatches(jobID)
	return ds[len(ds)-1]
}

// Each reason a woken machine has nothing left to do ends its dispatch
// and stops the machine (D055).
func TestTheReaperStopsMachinesWithNothingLeftToDo(t *testing.T) {
	ctx := context.Background()
	claimed := func(r *rig) (db.Job, *queue.Claim) {
		job, key := r.woken("ast_1")
		wk, _ := r.svc.AuthenticateWorker(ctx, key)
		return job, r.claim(wk)
	}
	cases := []struct {
		reason string
		setup  func(r *rig) db.Job
	}{
		{"attempt_ended", func(r *rig) db.Job {
			job, c := claimed(r)
			if _, err := r.q.Fail(ctx, c.Token, queue.Failure{Code: "upload_failed"}); err != nil {
				t.Fatal(err)
			}
			return job
		}},
		{"attempt_ended", func(r *rig) db.Job { // the machine went silent
			job, c := claimed(r)
			r.exec(`UPDATE attempts SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, c.Attempt.ID)
			if _, err := r.q.SweepLostLeases(ctx, 10); err != nil {
				t.Fatal(err)
			}
			return job
		}},
		{"job_gone", func(r *rig) db.Job {
			job, _ := r.woken("ast_1")
			r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1`, job.ID)
			return job
		}},
		{"wake_timeout", func(r *rig) db.Job {
			job, _ := r.woken("ast_1")
			r.exec(`UPDATE dispatches SET claim_deadline = now() - interval '1 second' WHERE job_id = $1`, job.ID)
			return job
		}},
		{"max_lifetime", func(r *rig) db.Job {
			job, _ := claimed(r)
			r.exec(`UPDATE dispatches SET created_at = now() - interval '7 hours' WHERE job_id = $1`, job.ID)
			return job
		}},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			r := newRig(t)
			job := c.setup(r)
			if n := r.reap(); n != 1 {
				t.Fatalf("reaped %d, want 1", n)
			}
			d := r.ended(job.ID)
			if d.Status != "ended" || d.EndReason.String != c.reason {
				t.Fatalf("dispatch = %s %s, want ended %s", d.Status, d.EndReason.String, c.reason)
			}
			r.publish()
			if len(r.prov.stops) != 1 || r.prov.stops[0] != d.InstanceID.String {
				t.Fatalf("stops = %v, want %s", r.prov.stops, d.InstanceID.String)
			}
			if n := r.reap(); n != 0 {
				t.Fatalf("a second pass reaped %d", n)
			}
		})
	}

	t.Run("a wake timeout spends no budget and wakes another machine", func(t *testing.T) {
		r := newRig(t)
		job, _ := r.woken("ast_1")
		r.exec(`UPDATE dispatches SET claim_deadline = now() - interval '1 second' WHERE job_id = $1`, job.ID)
		r.reap()
		r.route()
		j, _ := db.New(r.pool).GetJob(ctx, job.ID)
		if ds := r.dispatches(job.ID); len(ds) != 2 || ds[1].Status != "waking" || j.LostCount != 0 || j.FailedCount != 0 {
			t.Fatalf("%d dispatches, budgets lost %d failed %d", len(ds), j.LostCount, j.FailedCount)
		}
	})

	t.Run("a running attempt keeps its machine", func(t *testing.T) {
		r := newRig(t)
		claimed(r)
		if n := r.reap(); n != 0 {
			t.Fatalf("reaped %d machines that are working", n)
		}
	})
}

// A machine the provider runs that no unstopped dispatch accounts for is
// stopped; machines of live dispatches, including one whose wake is still
// in flight, are left alone (D055).
func TestTheReconcilerStopsMachinesNoDispatchAccountsFor(t *testing.T) {
	r := newRig(t)
	r.exec(`UPDATE executor_configs SET max_concurrent = 10`)
	// A machine whose dispatch ended and was marked stopped, but which the
	// provider still runs: its stop was lost.
	finished, _ := r.woken("ast_3")
	d := r.dispatches(finished.ID)[0]
	r.exec(`UPDATE dispatches SET status = 'ended', ended_at = now(), end_reason = 'attempt_ended', stopped_at = now() WHERE id = $1`, d.ID)
	working, _ := r.woken("ast_1")
	inFlight := r.job("ast_2")
	r.route() // routed and not yet published: no instance ID
	pending := r.dispatches(inFlight.ID)[0]
	r.prov.extra = []dispatch.Instance{
		{ID: "pod-in-flight", Name: dispatch.Name(pending.ID)},
		{ID: "pod-from-a-crash", Name: "clipmux-dsp_never_recorded"},
	}

	n, err := r.svc.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stopped := map[string]bool{}
	for _, id := range r.prov.stops {
		stopped[id] = true
	}
	if n != 2 || !stopped["pod-from-a-crash"] || !stopped["pod-"+d.ID] || len(stopped) != 2 {
		t.Fatalf("stopped %d: %v; want the crash's machine and the one whose stop was lost", n, r.prov.stops)
	}
	if stopped["pod-"+r.dispatches(working.ID)[0].ID] || stopped["pod-in-flight"] {
		t.Fatal("stopped a machine a live dispatch accounts for")
	}
}

// Parallel routers can't overshoot max_concurrent: the provider's row lock
// serialises them (D055), and room is counted after the lock (D058).
func TestConcurrencyHoldsUnderParallelRouters(t *testing.T) {
	r := newRig(t)
	for i := range 10 {
		r.job("ast_" + string(rune('a'+i)))
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := r.q.Route(context.Background(), 100); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	r.route()
	var live, limited int
	if err := r.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM dispatches WHERE status <> 'ended'),
		(SELECT count(*) FROM jobs WHERE route_reason = 'fallback_limit')`).Scan(&live, &limited); err != nil {
		t.Fatal(err)
	}
	if live != 2 || limited != 8 {
		t.Fatalf("%d live dispatches and %d jobs at the limit, want 2 and 8", live, limited)
	}
}

// The month's spend counts each machine from wake to stop, a live
// dispatch at least its reservation (an hour per 4 GiB, an hour for an
// unknown size), and nothing from earlier months (D055).
func TestTheMonthlyLimitCountsReservationsAndRealRuns(t *testing.T) {
	r := newRig(t)
	// $60 an hour against a $100 month, with room for 10 machines.
	r.exec(`UPDATE executor_configs SET rate_cents_per_hour = 6000, monthly_limit_cents = 10000, max_concurrent = 10`)
	first := r.job("ast_1")
	second := r.job("ast_2")
	r.route()
	routes := func(id string) string {
		j, _ := db.New(r.pool).GetJob(context.Background(), id)
		return j.RoutedTo.String + "/" + j.RouteReason.String
	}
	if len(r.dispatches(first.ID)) != 1 || routes(second.ID) != "agents/fallback_limit" {
		t.Fatalf("first has %d dispatches, second routed %s; want one reservation of $60 and the second waiting",
			len(r.dispatches(first.ID)), routes(second.ID))
	}

	// The first machine ran six minutes ($6) and stopped.
	r.exec(`UPDATE dispatches SET status = 'ended', end_reason = 'attempt_ended', woken_at = now() - interval '6 minutes',
		ended_at = now(), stopped_at = now(), instance_id = 'pod-1' WHERE job_id = $1`, first.ID)
	r.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, first.ID)
	r.route()
	if routes(second.ID) != "runpod/primary_offline" || len(r.dispatches(second.ID)) != 1 {
		t.Fatalf("second routed %s; $6 spent plus $60 reserved fits $100", routes(second.ID))
	}

	// A 12 GiB upload fits the machines' 50 GB disk but reserves three hours
	// ($180): more than the month has left.
	big := r.job("ast_big")
	r.exec(`INSERT INTO uploads (id, project_id, asset_id, multipart_id, declared_size, part_size, token_hash, status)
		VALUES ('upl_big', 'prj_default', 'ast_big', 'mp', 12884901888, 33554432, 'h', 'completed')`)
	r.exec(`UPDATE dispatches SET status = 'ended', end_reason = 'attempt_ended', ended_at = now() WHERE job_id = $1`, second.ID)
	r.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, second.ID)
	r.route()
	if routes(big.ID) != "agents/fallback_limit" {
		t.Fatalf("a 12 GiB job routed %s, want fallback_limit", routes(big.ID))
	}

	// Last month's spend doesn't count.
	r.exec(`UPDATE dispatches SET rate_cents_per_hour = 1000000, created_at = date_trunc('month', now()) - interval '1 day' WHERE job_id = $1`, first.ID)
	small := r.job("ast_small")
	r.route()
	if routes(small.ID) != "runpod/primary_offline" {
		t.Fatalf("a small job routed %s with last month's spend excluded", routes(small.ID))
	}
}

// Wakes for multiple routed jobs start together in one Publish pass without
// holding database connections while provider calls are in flight (D149).
func TestWakesRunInParallelWithoutHoldingDBConnections(t *testing.T) {
	r := newRig(t)
	r.exec(`UPDATE executor_configs SET max_concurrent = 5 WHERE id = 'exe_runpod'`)
	gate := make(chan struct{})
	entered := make(chan struct{}, 5)
	r.prov.gate = gate
	r.prov.entered = entered

	jobs := make([]db.Job, 5)
	for i := range jobs {
		jobs[i] = r.job(fmt.Sprintf("ast_%d", i))
	}
	r.route()

	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil || n != 5 {
		t.Fatalf("Publish = %d, %v; want 5, nil", n, err)
	}
	for i := range 5 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d wakes entered in parallel", i)
		}
	}
	if inFlight := r.prov.currentInFlight(); inFlight != 5 {
		t.Fatalf("in-flight wakes = %d, want 5", inFlight)
	}
	if conns := r.pool.Stat().AcquiredConns(); conns != 0 {
		t.Fatalf("AcquiredConns = %d while 5 wakes block, want 0", conns)
	}

	close(gate)
	r.svc.Wait()
	for _, j := range jobs {
		d := r.dispatches(j.ID)[0]
		if !d.InstanceID.Valid || !d.WokenAt.Valid {
			t.Fatalf("dispatch %s for %s not woken: %+v", d.ID, j.ID, d)
		}
	}
}

// If a dispatch is ended while its Wake call is in flight, Record records
// the returned instance_id and queues a stop immediately instead of waiting
// for the reconciler (D149).
func TestADispatchEndedDuringItsWakeStopsTheMachine(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	r.prov.gate = gate
	r.prov.entered = entered

	job := r.job("ast_1")
	r.route()
	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil || n != 1 {
		t.Fatalf("Publish = %d, %v; want 1, nil", n, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wake never entered")
	}

	r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1`, job.ID)
	if reaped := r.reap(); reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}
	if d := r.dispatches(job.ID)[0]; d.Status != "ended" || d.EndReason.String != dispatch.EndJobGone {
		t.Fatalf("dispatch after reap = %s %s, want ended job_gone", d.Status, d.EndReason.String)
	}
	var stopsBefore int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'stop'`).Scan(&stopsBefore)
	if stopsBefore != 0 {
		t.Fatalf("stops before wake finished = %d, want 0", stopsBefore)
	}

	close(gate)
	r.svc.Wait()

	d := r.dispatches(job.ID)[0]
	if d.InstanceID.String != "pod-"+d.ID || !d.WokenAt.Valid {
		t.Fatalf("dispatch after wake finished = %+v, want instance_id and woken_at recorded", d)
	}
	var wakePending, stopPending int
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'wake' AND done_at IS NULL`).Scan(&wakePending)
	_ = r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'stop' AND done_at IS NULL`).Scan(&stopPending)
	if wakePending != 0 || stopPending != 1 {
		t.Fatalf("outbox wakePending=%d stopPending=%d, want 0 and 1", wakePending, stopPending)
	}

	r.publish()
	if stops := r.prov.stopList(); len(stops) != 1 || stops[0] != d.InstanceID.String {
		t.Fatalf("stops = %v, want [%s]", stops, d.InstanceID.String)
	}
	if d := r.dispatches(job.ID)[0]; !d.StoppedAt.Valid {
		t.Fatal("dispatch not marked stopped")
	}
}

// A machine woken near the end of its initial claim window gets a fresh
// claim_deadline_seconds from the wake (D149).
func TestALateWakeGetsTheFullClaimWindow(t *testing.T) {
	r := newRig(t)
	job := r.job("ast_1")
	r.route()
	r.exec(`UPDATE dispatches SET claim_deadline = now() + interval '5 seconds' WHERE job_id = $1`, job.ID)
	r.publish()
	var left float64
	if err := r.pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM claim_deadline - now()) FROM dispatches WHERE job_id = $1`, job.ID,
	).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left < 580 || left > 610 {
		t.Fatalf("claim window left after late wake = %.1f s, want ~600 s", left)
	}
}

// ErrPreparing backoff runs from after the Wake call returns and caps at
// 30 s rather than doubling to 5 minutes (D149).
func TestPreparingBackoffStartsAfterCallAndCapsAt30Seconds(t *testing.T) {
	r := newRig(t)
	r.prov.preparing = 10
	r.prov.wakeDelay = time.Second
	r.job("ast_1")
	r.route()

	var before time.Time
	if err := r.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	r.publish()

	var attempts int
	var sinceBefore float64
	if err := r.pool.QueryRow(context.Background(),
		`SELECT attempts, extract(epoch FROM available_at - $1) FROM outbox WHERE kind = 'wake' AND done_at IS NULL`, before,
	).Scan(&attempts, &sinceBefore); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || sinceBefore < 5.5 {
		t.Fatalf("first preparing retry: attempts=%d available_at-before=%.2f s, want 1 and >= 5.5 s", attempts, sinceBefore)
	}

	r.prov.wakeDelay = 0
	wantDelays := []float64{10, 20, 30, 30}
	for i, want := range wantDelays {
		r.exec(`UPDATE outbox SET available_at = now() - interval '1 second' WHERE kind = 'wake' AND done_at IS NULL`)
		r.publish()
		var delay float64
		if err := r.pool.QueryRow(context.Background(),
			`SELECT attempts, extract(epoch FROM available_at - now()) FROM outbox WHERE kind = 'wake' AND done_at IS NULL`,
		).Scan(&attempts, &delay); err != nil {
			t.Fatal(err)
		}
		if attempts != i+2 || delay < want-2 || delay > want+2 {
			t.Fatalf("attempt %d: attempts=%d delay=%.2f s, want %.0f s", i+2, attempts, delay, want)
		}
	}
}

// A wake whose pass dies mid-call stays leased until released (at boot or
// lease expiry) and then replays with the same name and key (D054, D149).
func TestALeaseFromADeadPassIsReplayedWithTheSameNameAndKey(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	r.prov.gate = gate
	r.prov.entered = entered

	job := r.job("ast_1")
	r.route()
	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil || n != 1 {
		t.Fatalf("Publish = %d, %v; want 1, nil", n, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wake never entered")
	}

	// While leased, another Publish skips the row.
	if again, err := r.svc.Publish(context.Background(), 20); err != nil || again != 0 {
		t.Fatalf("Publish while leased = %d, %v; want 0, nil", again, err)
	}

	// Simulate shutdown/crash while Wake is in flight.
	r.cancel()
	r.svc.Wait()

	var attempts int
	var done bool
	if err := r.pool.QueryRow(context.Background(),
		`SELECT attempts, done_at IS NOT NULL FROM outbox WHERE kind = 'wake'`,
	).Scan(&attempts, &done); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || done {
		t.Fatalf("after dead pass: attempts=%d done=%v, want 0 and false", attempts, done)
	}
	if d := r.dispatches(job.ID)[0]; d.Status != "waking" || d.InstanceID.Valid {
		t.Fatalf("dispatch after dead pass = %+v, want still waking without instance", d)
	}

	// Restart: ReleaseLeases makes the leased row due immediately and replays it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	r.cancel = cancel2
	r.svc.Start(ctx2)
	r.prov.mu.Lock()
	r.prov.gate = nil
	r.prov.entered = nil
	r.prov.mu.Unlock()
	if err := r.svc.ReleaseLeases(ctx2); err != nil {
		t.Fatal(err)
	}
	if n := r.publish(); n != 1 {
		t.Fatalf("replayed Publish = %d, want 1", n)
	}
	if d := r.dispatches(job.ID)[0]; !d.InstanceID.Valid || !d.WokenAt.Valid {
		t.Fatalf("dispatch after replay = %+v, want woken", d)
	}
	if len(r.prov.wakes) != 2 {
		t.Fatalf("wakes = %d, want 2", len(r.prov.wakes))
	}
	first, second := r.prov.wakes[0], r.prov.wakes[1]
	if first.Name != second.Name || first.Env["CLIPMUX_WORKER_KEY"] != second.Env["CLIPMUX_WORKER_KEY"] {
		t.Fatalf("replay changed machine or key: %s/%s", first.Name, second.Name)
	}
}

// A wake that fails after its dispatch already ended (for instance, the job
// was cancelled mid-call) marks the outbox row done without counting a
// provider failure.
func TestAFailedWakeForAnEndedDispatchCountsNoFailure(t *testing.T) {
	r := newRig(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	r.prov.gate = gate
	r.prov.entered = entered
	r.prov.failing = 1

	job := r.job("ast_1")
	r.route()
	n, err := r.svc.Publish(context.Background(), 20)
	if err != nil || n != 1 {
		t.Fatalf("Publish = %d, %v; want 1, nil", n, err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wake never entered")
	}

	r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = $1`, job.ID)
	if reaped := r.reap(); reaped != 1 {
		t.Fatalf("reaped = %d, want 1", reaped)
	}

	close(gate)
	r.svc.Wait()

	var failures int
	if err := r.pool.QueryRow(context.Background(), `SELECT wake_failures FROM executor_configs WHERE id = 'exe_runpod'`).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("wake_failures = %d (%v), want 0", failures, err)
	}
	var pending int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'wake' AND done_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending wakes = %d (%v), want 0", pending, err)
	}
}
