package queue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/sqlc"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

// ErrUnknownToken means no attempt carries the presented token.
var ErrUnknownToken = errors.New("queue: unknown attempt token")

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
}

// New returns a queue with the architecture's defaults: 90 s leases, budgets
// of 5 lost leases and 3 failures, and 30 s then 2 min backoff.
func New(pool *pgxpool.Pool) *Queue {
	return &Queue{
		pool:         pool,
		ProjectID:    "prj_default",
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
	Job     sqlc.Job
	Attempt sqlc.Attempt
	Output  sqlc.Output
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
func Enqueue(ctx context.Context, db sqlc.DBTX, j Job) (sqlc.Job, error) {
	return sqlc.New(db).CreateJob(ctx, sqlc.CreateJobParams{
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
	token, hash, err := newToken()
	if err != nil {
		return nil, err
	}
	info := w.Info
	if len(info) == 0 {
		info = json.RawMessage(`{}`)
	}
	var claim *Claim
	err = pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		qs := sqlc.New(tx)
		job, err := qs.LockNextClaimableJob(ctx, sqlc.LockNextClaimableJobParams{
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
		output, err := qs.CreateOutput(ctx, sqlc.CreateOutputParams{
			ID: ids.New(ids.Output), ProjectID: job.ProjectID, AssetID: job.AssetID, KeyVersion: q.KeyVersion,
		})
		if err != nil {
			return err
		}
		attempt, err := qs.CreateAttempt(ctx, sqlc.CreateAttemptParams{
			ID: ids.New(ids.Attempt), ProjectID: job.ProjectID, JobID: job.ID, AssetID: job.AssetID,
			AttemptNumber: n, OutputID: output.ID, Executor: w.Executor, AgentID: text(w.AgentID),
			TokenHash: hash, WorkerInfo: info, LeaseSeconds: q.Lease.Seconds(),
		})
		if err != nil {
			return err
		}
		if err := qs.StartJob(ctx, sqlc.StartJobParams{ID: job.ID, RoutedTo: text(w.Executor), RouteReason: text("claimed")}); err != nil {
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
func (q *Queue) Authenticate(ctx context.Context, token string) (sqlc.Attempt, error) {
	a, err := sqlc.New(q.pool).GetAttemptByTokenHash(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Attempt{}, ErrUnknownToken
	}
	return a, err
}

// locked is an attempt and its job, both locked, with the liveness verdict.
type locked struct {
	job     sqlc.Job
	attempt sqlc.Attempt
	verdict Verdict
}

// lockLive locks the attempt's job then the attempt, and applies the single
// liveness rule: an attempt is live while it is the latest attempt of a job
// that isn't cancelled (and hasn't ended).
func lockLive(ctx context.Context, qs *sqlc.Queries, attemptID, jobID string) (locked, error) {
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

func (q *Queue) withLiveAttempt(ctx context.Context, token string, fn func(qs *sqlc.Queries, l locked) (Verdict, error)) (Verdict, error) {
	a, err := q.Authenticate(ctx, token)
	if err != nil {
		return Verdict{}, err
	}
	var v Verdict
	err = pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		qs := sqlc.New(tx)
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
	return q.withLiveAttempt(ctx, token, func(qs *sqlc.Queries, l locked) (Verdict, error) {
		progress := pgtype.Float4{}
		if p.Progress != nil {
			progress = pgtype.Float4{Float32: *p.Progress, Valid: true}
		}
		if err := qs.RenewLease(ctx, sqlc.RenewLeaseParams{
			ID: l.attempt.ID, LeaseSeconds: q.Lease.Seconds(), Phase: text(p.Phase), Progress: progress,
		}); err != nil {
			return Verdict{}, err
		}
		return Verdict{}, qs.SetJobRunningProgress(ctx, sqlc.SetJobRunningProgressParams{
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
	return q.withLiveAttempt(ctx, token, func(qs *sqlc.Queries, l locked) (Verdict, error) {
		if err := qs.SetAttemptStatus(ctx, sqlc.SetAttemptStatusParams{
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
			err = qs.RequeueJob(ctx, sqlc.RequeueJobParams{ID: l.job.ID, DelaySeconds: q.Backoff(failures).Seconds()})
		}
		return Verdict{Stop: true, Reason: ReasonFinished}, err
	})
}

// SweepLostLeases marks running attempts whose lease expired as lost and
// returns their jobs to the queue, or fails them once the lost budget is
// spent. It returns how many attempts it marked lost.
func (q *Queue) SweepLostLeases(ctx context.Context, limit int) (int, error) {
	expired, err := sqlc.New(q.pool).ListExpiredLeases(ctx, int32(limit))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range expired {
		swept := false
		err := pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
			qs := sqlc.New(tx)
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
			if err := qs.SetAttemptStatus(ctx, sqlc.SetAttemptStatusParams{ID: id, Status: "lost"}); err != nil {
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
			return qs.RequeueJob(ctx, sqlc.RequeueJobParams{ID: job.ID})
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
func (q *Queue) endJob(ctx context.Context, qs *sqlc.Queries, job sqlc.Job, code string) error {
	if err := qs.FailJob(ctx, sqlc.FailJobParams{ID: job.ID, ErrorCode: text(code)}); err != nil {
		return err
	}
	asset, err := qs.LockAsset(ctx, job.AssetID)
	if err != nil {
		return err
	}
	event, version := "video.asset.errored", asset.Version
	switch asset.Status {
	case "deleted":
		return nil
	case "ready":
		event = "video.asset.reencode_failed"
	default:
		if version, err = qs.MarkAssetErrored(ctx, sqlc.MarkAssetErroredParams{ID: asset.ID, ErrorCode: text(code)}); err != nil {
			return err
		}
	}
	return insertEvent(ctx, qs, asset, event, version, map[string]any{"job_id": job.ID, "error_code": code})
}

func insertEvent(ctx context.Context, qs *sqlc.Queries, asset sqlc.Asset, eventType string, version int64, extra map[string]any) error {
	id := ids.New(ids.Event)
	data := map[string]any{"asset_id": asset.ID, "version": version}
	if asset.Passthrough.Valid {
		data["passthrough"] = asset.Passthrough.String
	}
	for k, v := range extra {
		data[k] = v
	}
	payload, err := json.Marshal(map[string]any{
		"id": id, "type": eventType, "created_at": time.Now().UTC().Format(time.RFC3339), "data": data,
	})
	if err != nil {
		return err
	}
	return qs.InsertOutbox(ctx, sqlc.InsertOutboxParams{
		ID: id, ProjectID: asset.ProjectID, Kind: "webhook", AppID: text(asset.AppID), AssetID: text(asset.ID), Payload: payload,
	})
}

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}
