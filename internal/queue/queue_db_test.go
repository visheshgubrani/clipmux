package queue_test

// Real-Postgres tests for the leased queue (docs/architecture.md, "Compute"
// and "Leases"). Lease expiry is simulated by moving lease_expires_at into
// the past, so no test sleeps.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/queue"
)

var builtin = queue.Worker{Executor: "builtin"}

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *queue.Queue
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.New(t)
	return &fixture{t: t, pool: pool, q: queue.New(pool)}
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// asset creates a processing asset (or a ready one with an output) and returns its ID.
func (f *fixture) asset(id string, ready bool) string {
	f.t.Helper()
	f.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', 'app_dashboard', 'processing', 'sto_default')`, id)
	if ready {
		f.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ($1, 'prj_default', $2, 1, 'committed', '{}')`, "out_"+id, id)
		f.exec(`UPDATE assets SET status = 'ready', current_output_id = $1 WHERE id = $2`, "out_"+id, id)
	}
	return id
}

func (f *fixture) enqueue(assetID, kind string) db.Job {
	f.t.Helper()
	j, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: assetID, Kind: kind})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) claim(w queue.Worker) *queue.Claim {
	f.t.Helper()
	c, err := f.q.Claim(context.Background(), w)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) job(id string) db.Job {
	f.t.Helper()
	j, err := db.New(f.pool).GetJob(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *fixture) assetRow(id string) (status, code string, version int64) {
	f.t.Helper()
	var c *string
	if err := f.pool.QueryRow(context.Background(), `SELECT status, error_code, version FROM assets WHERE id = $1`, id).Scan(&status, &c, &version); err != nil {
		f.t.Fatal(err)
	}
	if c != nil {
		code = *c
	}
	return status, code, version
}

func (f *fixture) attemptStatus(id string) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM attempts WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) expireLease(attemptID string) {
	f.exec(`UPDATE attempts SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, attemptID)
}

func (f *fixture) sweep() int {
	f.t.Helper()
	n, err := f.q.SweepLostLeases(context.Background(), 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) events() []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT payload FROM outbox WHERE kind = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var ev struct{ Type string }
		_ = json.Unmarshal(raw, &ev)
		types = append(types, ev.Type)
	}
	return types
}

func heartbeat(t *testing.T, q *queue.Queue, token string) queue.Verdict {
	t.Helper()
	v, err := q.Heartbeat(context.Background(), token, queue.Progress{Phase: "transcode"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestClaimOnEmptyQueueReturnsNothing(t *testing.T) {
	f := newFixture(t)
	if c := f.claim(builtin); c != nil {
		t.Fatalf("claimed %+v from an empty queue", c.Job)
	}
}

func TestClaimCreatesAttemptLeaseAndFreshOutput(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")

	c := f.claim(builtin)
	if c == nil || c.Job.ID != job.ID {
		t.Fatalf("claim = %+v, want job %s", c, job.ID)
	}
	if c.Attempt.AttemptNumber != 1 || c.Attempt.Executor != "builtin" || c.Attempt.Status != "running" {
		t.Fatalf("attempt = %+v", c.Attempt)
	}
	if c.Output.AssetID != "ast_1" || c.Output.Status != "writing" || c.Attempt.OutputID != c.Output.ID {
		t.Fatalf("output = %+v", c.Output)
	}
	if lease := time.Until(c.Attempt.LeaseExpiresAt.Time); lease < 80*time.Second || lease > 100*time.Second {
		t.Fatalf("lease = %v, want about 90s", lease)
	}
	if c.Token == "" || string(c.Attempt.TokenHash) == c.Token {
		t.Fatal("the attempt token must be stored hashed")
	}
	if got := f.job(job.ID); got.Status != "running" || got.RoutedTo.String != "builtin" {
		t.Fatalf("job = status %q routed_to %q", got.Status, got.RoutedTo.String)
	}
	if again := f.claim(builtin); again != nil {
		t.Fatal("a running job was claimed twice")
	}
}

func TestConcurrentClaimsTakeAJobOnce(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.asset("ast_1", false), "transcode")

	var wg sync.WaitGroup
	results := make(chan *queue.Claim, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := f.q.Claim(context.Background(), builtin)
			if err != nil {
				t.Error(err)
			}
			results <- c
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for c := range results {
		if c != nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d workers claimed the job, want exactly 1", won)
	}
}

func TestExecutorsOnlyTakeMediaWorkTheyMayRun(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.asset("ast_move", false), "move")
	f.enqueue(f.asset("ast_delete", false), "delete")
	f.exec(`INSERT INTO agents (id, project_id, name, key_hash, status, scope, sandboxed) VALUES ('agt_pc', 'prj_default', 'pc', 'h', 'approved', 'local', false)`)
	pinned, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: f.asset("ast_local", false), Kind: "transcode", PinnedAgentID: "agt_pc"})
	if err != nil {
		t.Fatal(err)
	}

	if c := f.claim(builtin); c != nil {
		t.Fatalf("built-in worker claimed %s job %s", c.Job.Kind, c.Job.ID)
	}
	open := f.enqueue(f.asset("ast_open", false), "transcode")
	pc := queue.Worker{Executor: "agent", AgentID: "agt_pc", LocalOnly: true}
	c := f.claim(pc)
	if c == nil || c.Job.ID != pinned.ID {
		t.Fatalf("local agent claim = %+v, want its pinned job", c)
	}
	if c := f.claim(pc); c != nil {
		t.Fatal("a local-only agent claimed an unpinned job")
	}
	if c := f.claim(builtin); c == nil || c.Job.ID != open.ID {
		t.Fatal("built-in worker should take the unpinned transcode")
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	f.exec(`UPDATE attempts SET lease_expires_at = now() + interval '5 seconds' WHERE id = $1`, c.Attempt.ID)

	half := float32(0.5)
	v, err := f.q.Heartbeat(context.Background(), c.Token, queue.Progress{Phase: "package", Progress: &half})
	if err != nil || v.Stop {
		t.Fatalf("heartbeat = %+v, %v", v, err)
	}
	var phase string
	var progress float32
	var remaining float64
	if err := f.pool.QueryRow(context.Background(), `SELECT j.phase, j.progress, extract(epoch FROM a.lease_expires_at - now())
		FROM attempts a JOIN jobs j ON j.id = a.job_id WHERE a.id = $1`, c.Attempt.ID).Scan(&phase, &progress, &remaining); err != nil {
		t.Fatal(err)
	}
	if phase != "package" || progress != 0.5 || remaining < 80 {
		t.Fatalf("phase %q progress %v lease %vs", phase, progress, remaining)
	}
	if _, err := f.q.Heartbeat(context.Background(), "not-a-token", queue.Progress{}); err != queue.ErrUnknownToken {
		t.Fatalf("unknown token: err = %v, want ErrUnknownToken", err)
	}
}

// The Phase 1 exit criterion: killing a worker mid-job recovers the job.
func TestKilledWorkerJobIsRecoveredOnANewAttemptAndOutput(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	first := f.claim(builtin)

	f.expireLease(first.Attempt.ID) // the worker died and stopped heartbeating
	if n := f.sweep(); n != 1 {
		t.Fatalf("sweep handled %d leases, want 1", n)
	}
	if s := f.attemptStatus(first.Attempt.ID); s != "lost" {
		t.Fatalf("attempt status = %q, want lost", s)
	}
	if j := f.job(job.ID); j.Status != "queued" || j.LostCount != 1 || j.FailedCount != 0 {
		t.Fatalf("job = status %q lost %d failed %d, want queued 1 0", j.Status, j.LostCount, j.FailedCount)
	}

	second := f.claim(builtin)
	if second == nil || second.Job.ID != job.ID || second.Attempt.AttemptNumber != 2 {
		t.Fatalf("retry claim = %+v", second)
	}
	if second.Output.ID == first.Output.ID {
		t.Fatal("a retry must write a new output")
	}

	// The old worker comes back: it is stale now and must stop.
	if v := heartbeat(t, f.q, first.Token); !v.Stop || v.Reason != "superseded" {
		t.Fatalf("stale heartbeat = %+v, want stop/superseded", v)
	}
	if v := heartbeat(t, f.q, second.Token); v.Stop {
		t.Fatalf("live heartbeat = %+v", v)
	}
}

func TestSlowWorkerCanHeartbeatBackBeforeARetryStarts(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	f.expireLease(c.Attempt.ID)
	f.sweep()

	if v := heartbeat(t, f.q, c.Token); v.Stop {
		t.Fatalf("slow but still latest attempt was told to stop: %+v", v)
	}
	if s := f.attemptStatus(c.Attempt.ID); s != "running" {
		t.Fatalf("attempt = %q, want running again", s)
	}
	if j := f.job(job.ID); j.Status != "running" {
		t.Fatalf("job = %q, want running again", j.Status)
	}
	if again := f.claim(builtin); again != nil {
		t.Fatal("the recovered job was claimed by another worker")
	}
}

func TestLostLeaseBudgetIsFive(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	for i := 1; i <= 5; i++ {
		c := f.claim(builtin)
		if c == nil {
			t.Fatalf("loss %d: job was not claimable", i)
		}
		f.expireLease(c.Attempt.ID)
		f.sweep()
	}
	if j := f.job(job.ID); j.Status != "failed" || j.LostCount != 5 || j.ErrorCode.String != "worker_lost" {
		t.Fatalf("job = status %q lost %d code %q, want failed 5 worker_lost", j.Status, j.LostCount, j.ErrorCode.String)
	}
	if status, code, _ := f.assetRow("ast_1"); status != "errored" || code != "worker_lost" {
		t.Fatalf("asset = %s/%s, want errored/worker_lost", status, code)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.errored" {
		t.Fatalf("events = %v", ev)
	}
}

func TestTransientFailuresBackOffAndStopAfterThree(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	fail := func(c *queue.Claim) {
		t.Helper()
		if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Code: "disk_full"}); err != nil {
			t.Fatal(err)
		}
	}

	c := f.claim(builtin)
	fail(c)
	if s := f.attemptStatus(c.Attempt.ID); s != "failed" {
		t.Fatalf("attempt = %q, want failed", s)
	}
	if j := f.job(job.ID); j.Status != "queued" || j.FailedCount != 1 || !j.AvailableAt.Time.After(time.Now()) {
		t.Fatalf("job = %q failed %d available %v, want queued with backoff", j.Status, j.FailedCount, j.AvailableAt.Time)
	}
	if again := f.claim(builtin); again != nil {
		t.Fatal("a job in backoff was claimed")
	}

	for i := 2; i <= 3; i++ {
		f.exec(`UPDATE jobs SET available_at = now() WHERE id = $1`, job.ID) // backoff elapsed
		fail(f.claim(builtin))
	}
	if j := f.job(job.ID); j.Status != "failed" || j.FailedCount != 3 || j.ErrorCode.String != "processing_failed" {
		t.Fatalf("job = %q failed %d code %q, want failed 3 processing_failed", j.Status, j.FailedCount, j.ErrorCode.String)
	}
	if status, code, _ := f.assetRow("ast_1"); status != "errored" || code != "processing_failed" {
		t.Fatalf("asset = %s/%s", status, code)
	}
}

func TestPermanentFailureErrorsTheAssetWithoutRetry(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	_, before := func() (string, int64) { _, _, v := f.assetRow("ast_1"); return "", v }()

	if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Permanent: true, Code: "unsupported_codec"}); err != nil {
		t.Fatal(err)
	}
	if j := f.job(job.ID); j.Status != "failed" || j.FailedCount != 1 {
		t.Fatalf("job = %q failed %d", j.Status, j.FailedCount)
	}
	status, code, version := f.assetRow("ast_1")
	if status != "errored" || code != "unsupported_codec" || version != before+1 {
		t.Fatalf("asset = %s/%s v%d, want errored/unsupported_codec v%d", status, code, version, before+1)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.errored" {
		t.Fatalf("events = %v", ev)
	}
}

func TestUnsafeFailureCodesAreReplaced(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Permanent: true, Code: "Bad Code; DROP"}); err != nil {
		t.Fatal(err)
	}
	if _, code, _ := f.assetRow("ast_1"); code != "processing_failed" {
		t.Fatalf("code = %q, want processing_failed", code)
	}
}

func TestFailedReencodeKeepsTheAssetReady(t *testing.T) {
	f := newFixture(t)
	f.enqueue(f.asset("ast_1", true), "reencode")
	c := f.claim(builtin)
	if _, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Permanent: true, Code: "unsupported_codec"}); err != nil {
		t.Fatal(err)
	}
	if status, code, _ := f.assetRow("ast_1"); status != "ready" || code != "" {
		t.Fatalf("asset = %s/%s, want ready with no error", status, code)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.reencode_failed" {
		t.Fatalf("events = %v", ev)
	}
}

func TestCancelledJobTellsItsWorkerToStop(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	f.exec(`UPDATE jobs SET status = 'cancelled' WHERE id = $1`, job.ID)

	if v := heartbeat(t, f.q, c.Token); !v.Stop || v.Reason != "cancelled" {
		t.Fatalf("heartbeat = %+v, want stop/cancelled", v)
	}
	if v, err := f.q.Fail(context.Background(), c.Token, queue.Failure{Code: "x"}); err != nil || !v.Stop {
		t.Fatalf("fail on a cancelled job = %+v, %v", v, err)
	}
	if j := f.job(job.ID); j.Status != "cancelled" || j.FailedCount != 0 {
		t.Fatalf("job = %q failed %d, want untouched", j.Status, j.FailedCount)
	}
}

func TestStaleFailureDoesNotTouchTheRetry(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	first := f.claim(builtin)
	f.expireLease(first.Attempt.ID)
	f.sweep()
	second := f.claim(builtin)

	v, err := f.q.Fail(context.Background(), first.Token, queue.Failure{Permanent: true, Code: "unsupported_codec"})
	if err != nil || !v.Stop || v.Reason != "superseded" {
		t.Fatalf("stale fail = %+v, %v", v, err)
	}
	if j := f.job(job.ID); j.Status != "running" || j.FailedCount != 0 {
		t.Fatalf("job = %q failed %d, want running 0", j.Status, j.FailedCount)
	}
	if s := f.attemptStatus(second.Attempt.ID); s != "running" {
		t.Fatalf("live attempt = %q", s)
	}
}

func TestClaimWaitLongPolls(t *testing.T) {
	f := newFixture(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.enqueue(f.asset("ast_1", false), "transcode")
	}()
	c, err := f.q.ClaimWait(context.Background(), builtin, 3*time.Second, 100*time.Millisecond)
	if err != nil || c == nil {
		t.Fatalf("long-poll claim = %v, %v", c, err)
	}
	start := time.Now()
	c, err = f.q.ClaimWait(context.Background(), builtin, 200*time.Millisecond, 50*time.Millisecond)
	if err != nil || c != nil || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("empty long-poll returned early or with work: %v, %v after %v", c, err, time.Since(start))
	}
}

func TestReleaseGivesAnUndeliveredClaimBackWithoutCountingALoss(t *testing.T) {
	f := newFixture(t)
	job := f.enqueue(f.asset("ast_1", false), "transcode")
	c := f.claim(builtin)
	if err := f.q.Release(context.Background(), c.Token); err != nil {
		t.Fatal(err)
	}
	if j := f.job(job.ID); j.Status != "queued" || j.LostCount != 0 || j.FailedCount != 0 {
		t.Fatalf("job = %s lost %d failed %d, want queued with no losses", j.Status, j.LostCount, j.FailedCount)
	}
	var attempts, outputs int
	_ = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM attempts), (SELECT count(*) FROM outputs)`).Scan(&attempts, &outputs)
	if attempts != 0 || outputs != 0 {
		t.Fatalf("%d attempts and %d outputs left behind", attempts, outputs)
	}
	if _, err := f.q.Heartbeat(context.Background(), c.Token, queue.Progress{}); err != queue.ErrUnknownToken {
		t.Fatalf("the released token must be unknown: %v", err)
	}
	if again := f.claim(builtin); again == nil || again.Attempt.AttemptNumber != 1 {
		t.Fatalf("re-claim = %+v, want attempt 1 again", again)
	}
}
