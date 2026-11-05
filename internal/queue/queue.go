package queue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
)

// ErrUnknownToken means no attempt carries the presented token.
var ErrUnknownToken = errors.New("queue: unknown attempt token")

// ErrSignBudget means the attempt has signed as many paths as it may.
var ErrSignBudget = errors.New("queue: attempt sign budget spent")

// ReserveSigns checks the attempt is live and counts n paths against its
// budget, in one locked transaction. It returns the attempt so the caller
// can build URLs under the attempt's own output prefix.
func (q *Queue) ReserveSigns(ctx context.Context, token string, n, budget int) (Verdict, db.Attempt, error) {
	var attempt db.Attempt
	v, err := q.withLiveAttempt(ctx, token, func(qs *db.Queries, l locked) (Verdict, error) {
		attempt = l.attempt
		_, err := qs.ReserveSignedPaths(ctx, db.ReserveSignedPathsParams{ID: l.attempt.ID, N: int32(n), Budget: int32(budget)})
		if errors.Is(err, pgx.ErrNoRows) {
			return Verdict{}, ErrSignBudget
		}
		return Verdict{}, err
	})
	return v, attempt, err
}

// Release undoes a claim whose payload never reached the worker: the
// attempt and its empty output are deleted and the job is queued again at
// once, without counting a lost lease. If the worker did get the token
// after all, its next call is refused as unknown and it stops.
func (q *Queue) Release(ctx context.Context, token string) error {
	_, err := q.withLiveAttempt(ctx, token, func(qs *db.Queries, l locked) (Verdict, error) {
		if err := qs.DeleteAttempt(ctx, l.attempt.ID); err != nil {
			return Verdict{}, err
		}
		if err := qs.DeleteWritingOutput(ctx, l.attempt.OutputID); err != nil {
			return Verdict{}, err
		}
		return Verdict{}, qs.RequeueJob(ctx, db.RequeueJobParams{ID: l.job.ID})
	})
	return err
}

// CommitAttempt runs commit for a live attempt inside the locked liveness
// transaction (job, then attempt, locked), then marks the attempt committed
// and the job succeeded in the same transaction. A late completion from an
// attempt that is no longer live gets a stop verdict and commits nothing.
func (q *Queue) CommitAttempt(ctx context.Context, token string, commit func(qs *db.Queries, attempt db.Attempt, job db.Job) error) (Verdict, error) {
	return q.withLiveAttempt(ctx, token, func(qs *db.Queries, l locked) (Verdict, error) {
		if err := commit(qs, l.attempt, l.job); err != nil {
			return Verdict{}, err
		}
		if err := qs.SetAttemptStatus(ctx, db.SetAttemptStatusParams{ID: l.attempt.ID, Status: "committed"}); err != nil {
			return Verdict{}, err
		}
		return Verdict{}, qs.SucceedJob(ctx, l.job.ID)
	})
}

// Error codes the queue itself assigns.
const (
	// CodeWorkerLost: the lost-lease budget was spent.
	CodeWorkerLost = "worker_lost"
	// CodeProcessingFailed: the failure budget was spent, or a worker sent an
	// unusable code.
	CodeProcessingFailed = "processing_failed"
)

// Stop reasons returned to workers.
const (
	ReasonSuperseded = "superseded" // a newer attempt exists
	ReasonCancelled  = "cancelled"  // the job was cancelled
	ReasonFinished   = "finished"   // the attempt or job already ended
)

// Queue is the leased Postgres job queue.
type Queue struct {
	pool *pgxpool.Pool

	// ProjectID scopes claims; v1 has one hidden project.
	ProjectID string
	// Lease is how long a claim or heartbeat keeps an attempt alive.
	Lease time.Duration
	// LostBudget and FailedBudget are the separate retry budgets.
	LostBudget, FailedBudget int32
	// Backoff is the delay before retrying after the nth transient failure.
	Backoff func(failures int32) time.Duration
	// KeyVersion is the content key secret version for new outputs.
	KeyVersion int32
	// BaseURL is the media origin, carried in webhook events as base_url.
	BaseURL string
}

// New returns a queue with the architecture's defaults: 90 s leases, budgets
// of 5 lost leases and 3 failures, and 30 s then 2 min backoff.
func New(pool *pgxpool.Pool) *Queue {
	return &Queue{
		pool:         pool,
		ProjectID:    db.DefaultProjectID,
		Lease:        90 * time.Second,
		LostBudget:   5,
		FailedBudget: 3,
		Backoff:      DefaultBackoff,
		KeyVersion:   1,
	}
}

// DefaultBackoff waits 30 s after the first failure and multiplies by 4.
func DefaultBackoff(failures int32) time.Duration {
	d := 30 * time.Second
	for i := int32(1); i < failures; i++ {
		d *= 4
	}
	return d
}

// Worker identifies who is claiming.
type Worker struct {
	// Executor is builtin, agent, modal or runpod.
	Executor string
	// AgentID is set for PC agents.
	AgentID string
	// LocalOnly agents claim only jobs pinned to them.
	LocalOnly bool
	// Info is what the worker reported at claim (tool versions, encoders,
	// free disk), stored on the attempt as JSON.
	Info json.RawMessage
}

// Job describes work to enqueue.
type Job struct {
	ProjectID     string
	AssetID       string
	Kind          string
	PinnedAgentID string
}

// Claim is a successful claim. Token is shown to the worker once; only its
// hash is stored.
type Claim struct {
	Job     db.Job
	Attempt db.Attempt
	Output  db.Output
	Token   string
}

// Progress is what a heartbeat reports.
type Progress struct {
	Phase    string
	Progress *float32
}

// Verdict tells a worker whether to keep going.
type Verdict struct {
	Stop   bool
	Reason string
}

// Failure is what a worker reports when it gives up.
type Failure struct {
	// Permanent failures (a corrupt or unsupported source) are never retried.
	Permanent bool
	// Code is a stable snake_case error code.
	Code    string
	Message string
}

// Enqueue adds a job. Run it in the transaction that changes the asset; the
// one-active-job-per-asset index rejects a second mutating job.
func Enqueue(ctx context.Context, conn db.DBTX, j Job) (db.Job, error) {
	return db.New(conn).CreateJob(ctx, db.CreateJobParams{
		ID:            ids.New(ids.Job),
		ProjectID:     j.ProjectID,
		AssetID:       j.AssetID,
		Kind:          j.Kind,
		PinnedAgentID: text(j.PinnedAgentID),
	})
}

// Claim takes the next claimable job, if any, and creates its attempt, lease
// and fresh output. It returns nil when there is no work.
func (q *Queue) Claim(ctx context.Context, w Worker) (*Claim, error) {
	token := rand.Text() // 130 bits; only its hash is stored
	hash := hashToken(token)
	info := w.Info
	if len(info) == 0 {
		info = json.RawMessage(`{}`)
	}
	var claim *Claim
	err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		job, err := qs.LockNextClaimableJob(ctx, db.LockNextClaimableJobParams{
			ProjectID: q.ProjectID, AgentID: text(w.AgentID), LocalOnly: w.LocalOnly,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n, err := qs.NextAttemptNumber(ctx, job.ID)
		if err != nil {
			return err
		}
		output, err := qs.CreateOutput(ctx, db.CreateOutputParams{
			ID: ids.New(ids.Output), ProjectID: job.ProjectID, AssetID: job.AssetID, KeyVersion: q.KeyVersion,
		})
		if err != nil {
			return err
		}
		attempt, err := qs.CreateAttempt(ctx, db.CreateAttemptParams{
			ID: ids.New(ids.Attempt), ProjectID: job.ProjectID, JobID: job.ID, AssetID: job.AssetID,
			AttemptNumber: n, OutputID: output.ID, Executor: w.Executor, AgentID: text(w.AgentID),
			TokenHash: hash, WorkerInfo: info, LeaseSeconds: q.Lease.Seconds(),
		})
		if err != nil {
			return err
		}
		if err := qs.StartJob(ctx, db.StartJobParams{ID: job.ID, RoutedTo: text(w.Executor), RouteReason: text("claimed")}); err != nil {
			return err
		}
		if job, err = qs.LockJob(ctx, job.ID); err != nil {
			return err
		}
		claim = &Claim{Job: job, Attempt: attempt, Output: output, Token: token}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return claim, nil
}

// ClaimWait long-polls: it retries Claim every poll interval until it gets
// work, wait elapses or ctx ends. It returns nil, nil when no work arrived.
func (q *Queue) ClaimWait(ctx context.Context, w Worker, wait, poll time.Duration) (*Claim, error) {
	deadline := time.Now().Add(wait)
	for {
		c, err := q.Claim(ctx, w)
		if c != nil || err != nil {
			return c, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		timer := time.NewTimer(min(poll, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Authenticate returns the attempt a token belongs to.
func (q *Queue) Authenticate(ctx context.Context, token string) (db.Attempt, error) {
	a, err := db.New(q.pool).GetAttemptByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Attempt{}, ErrUnknownToken
	}
	return a, err
}

// locked is an attempt and its job, both locked, with the liveness verdict.
type locked struct {
	job     db.Job
	attempt db.Attempt
	verdict Verdict
}

// lockLive locks the attempt's job then the attempt, and applies the single
// liveness rule: an attempt is live while it is the latest attempt of a job
// that isn't cancelled (and hasn't ended).
func lockLive(ctx context.Context, qs *db.Queries, attemptID, jobID string) (locked, error) {
	job, err := qs.LockJob(ctx, jobID)
	if err != nil {
		return locked{}, err
	}
	row, err := qs.LockAttempt(ctx, attemptID)
	if err != nil {
		return locked{}, err
	}
	// A separate statement after the job lock is held, so it sees any
	// attempt a competing claim committed while we waited.
	latest, err := qs.LatestAttemptNumber(ctx, jobID)
	if err != nil {
		return locked{}, err
	}
	l := locked{job: job, attempt: row.Attempt}
	switch {
	case job.Status == "cancelled":
		l.verdict = Verdict{Stop: true, Reason: ReasonCancelled}
	case row.Attempt.AttemptNumber != latest:
		l.verdict = Verdict{Stop: true, Reason: ReasonSuperseded}
	case row.Attempt.Status == "committed" || row.Attempt.Status == "failed",
		job.Status == "succeeded" || job.Status == "failed":
		l.verdict = Verdict{Stop: true, Reason: ReasonFinished}
	}
	return l, nil
}

func (q *Queue) withLiveAttempt(ctx context.Context, token string, fn func(qs *db.Queries, l locked) (Verdict, error)) (Verdict, error) {
	a, err := q.Authenticate(ctx, token)
	if err != nil {
		return Verdict{}, err
	}
	var v Verdict
	err = pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		l, err := lockLive(ctx, qs, a.ID, a.JobID)
		if err != nil {
			return err
		}
		if l.verdict.Stop {
			v = l.verdict
			return nil
		}
		v, err = fn(qs, l)
		return err
	})
	return v, err
}

// Heartbeat extends a live attempt's lease and records progress. A slow
// worker whose lease was marked lost, but whose attempt is still the latest,
// returns to running.
func (q *Queue) Heartbeat(ctx context.Context, token string, p Progress) (Verdict, error) {
	return q.withLiveAttempt(ctx, token, func(qs *db.Queries, l locked) (Verdict, error) {
		progress := pgtype.Float4{}
		if p.Progress != nil {
			progress = pgtype.Float4{Float32: *p.Progress, Valid: true}
		}
		if err := qs.RenewLease(ctx, db.RenewLeaseParams{
			ID: l.attempt.ID, LeaseSeconds: q.Lease.Seconds(), Phase: text(p.Phase), Progress: progress,
		}); err != nil {
			return Verdict{}, err
		}
		return Verdict{}, qs.SetJobRunningProgress(ctx, db.SetJobRunningProgressParams{
			ID: l.job.ID, Phase: text(p.Phase), Progress: progress,
		})
	})
}

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Fail records a worker-reported failure. Transient failures retry with
// backoff until the failure budget is spent; permanent ones end the job.
func (q *Queue) Fail(ctx context.Context, token string, f Failure) (Verdict, error) {
	code := f.Code
	if !codePattern.MatchString(code) {
		code = CodeProcessingFailed
	}
	kind := "transient"
	if f.Permanent {
		kind = "permanent"
	}
	return q.withLiveAttempt(ctx, token, func(qs *db.Queries, l locked) (Verdict, error) {
		if err := qs.SetAttemptStatus(ctx, db.SetAttemptStatusParams{
			ID: l.attempt.ID, Status: "failed", FailureKind: text(kind), ErrorCode: text(code),
		}); err != nil {
			return Verdict{}, err
		}
		failures, err := qs.CountJobFailure(ctx, l.job.ID)
		if err != nil {
			return Verdict{}, err
		}
		switch {
		case f.Permanent:
			err = q.endJob(ctx, qs, l.job, code)
		case failures >= q.FailedBudget:
			err = q.endJob(ctx, qs, l.job, CodeProcessingFailed)
		default:
			err = qs.RequeueJob(ctx, db.RequeueJobParams{ID: l.job.ID, DelaySeconds: q.Backoff(failures).Seconds()})
		}
		return Verdict{Stop: true, Reason: ReasonFinished}, err
	})
}

// SweepLostLeases marks running attempts whose lease expired as lost and
// returns their jobs to the queue, or fails them once the lost budget is
// spent. It returns how many attempts it marked lost.
func (q *Queue) SweepLostLeases(ctx context.Context, limit int) (int, error) {
	expired, err := db.New(q.pool).ListExpiredLeases(ctx, int32(limit))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range expired {
		swept := false
		err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
			qs := db.New(tx)
			a, err := qs.GetAttempt(ctx, id)
			if err != nil {
				return err
			}
			job, err := qs.LockJob(ctx, a.JobID)
			if err != nil {
				return err
			}
			row, err := qs.LockAttempt(ctx, id)
			if err != nil {
				return err
			}
			if row.Attempt.Status != "running" || !row.LeaseExpired {
				return nil // heartbeated or finished since we listed it
			}
			if err := qs.SetAttemptStatus(ctx, db.SetAttemptStatusParams{ID: id, Status: "lost"}); err != nil {
				return err
			}
			swept = true
			latest, err := qs.LatestAttemptNumber(ctx, job.ID)
			if err != nil {
				return err
			}
			if job.Status != "running" || row.Attempt.AttemptNumber != latest {
				return nil // job ended or moved on; nothing to route
			}
			lost, err := qs.CountJobLoss(ctx, job.ID)
			if err != nil {
				return err
			}
			if lost >= q.LostBudget {
				return q.endJob(ctx, qs, job, CodeWorkerLost)
			}
			return qs.RequeueJob(ctx, db.RequeueJobParams{ID: job.ID})
		})
		if err != nil {
			return n, fmt.Errorf("sweep attempt %s: %w", id, err)
		}
		if swept {
			n++
		}
	}
	return n, nil
}

// endJob fails a job for good. A ready asset (a failed re-encode) keeps
// playing its current output; anything else becomes errored. Either way the
// app hears about it through the webhook outbox, in this transaction.
func (q *Queue) endJob(ctx context.Context, qs *db.Queries, job db.Job, code string) error {
	if err := qs.FailJob(ctx, db.FailJobParams{ID: job.ID, ErrorCode: text(code)}); err != nil {
		return err
	}
	asset, err := qs.LockAsset(ctx, job.AssetID)
	if err != nil {
		return err
	}
	event := outbox.AssetErrored
	switch asset.Status {
	case "deleted":
		return nil
	case "ready":
		event = outbox.AssetReencodeFailed
	default:
		if _, err = qs.MarkAssetErrored(ctx, db.MarkAssetErroredParams{ID: asset.ID, ErrorCode: text(code)}); err != nil {
			return err
		}
	}
	return outbox.AssetEvent(ctx, qs, asset.ID, event, q.BaseURL, &outbox.Job{ID: job.ID, Kind: job.Kind, ErrorCode: code})
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
