// Package dispatch wakes serverless providers (docs/architecture.md,
// "Compute"; D054). When the router sends a job to a provider, a dispatch
// is created in the router's transaction with a one-time key; the publisher
// then calls the provider's Wake from an outbox row, and the machine it
// starts runs `clipmux worker --once`, whose key claims that job alone.
package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// KeyPrefix starts every dispatch key, so a worker knows it holds one.
const KeyPrefix = "cmxd_"

// MaxLifetime bounds a woken machine: the worker's 6 h job timeout plus a
// margin for the image pull and the upload.
const MaxLifetime = 6*time.Hour + 15*time.Minute

// Executor starts and stops one provider's machines. The core never
// branches on which provider it is (AGENTS.md, adapter boundaries).
type Executor interface {
	// Wake starts a machine for one dispatch and returns the provider's ID
	// for it. It must be safe to call again for the same dispatch: a retry
	// after a crash finds the machine named w.Name rather than start two.
	Wake(ctx context.Context, w Wake) (instanceID string, err error)
	// Stop ends a machine. Stopping one that is already gone succeeds.
	Stop(ctx context.Context, instanceID string) error
	// Running lists this project's ClipMux machines the provider is still
	// running (or starting), for the reconciler.
	Running(ctx context.Context) ([]Instance, error)
}

// Instance is one machine a provider reports.
type Instance struct {
	// ID is the provider's ID, as Wake returned it.
	ID string
	// Name is the name Wake gave it, clipmux-{dispatch_id}.
	Name string
}

// Wake is what a provider needs to start one machine.
type Wake struct {
	DispatchID string
	// Name is clipmux-{dispatch_id}, the provider-side name that makes Wake
	// idempotent.
	Name string
	// Env is the machine's whole environment for clipmux: the server URL
	// and the one-time key. It never goes in argv.
	Env map[string]string
	// Config is the provider's stored config (image, GPU, region).
	Config json.RawMessage
	// Lifetime is how long the provider may keep the machine at most.
	Lifetime time.Duration
}

// Service creates dispatches, authenticates their keys and publishes their
// wakes and stops.
type Service struct {
	pool *pgxpool.Pool
	box  *secrets.Box
	// ProjectID scopes the keys; v1 has one hidden project.
	ProjectID string
	// ServerURL is the public URL a machine reaches the worker API on.
	ServerURL func() string
	// Resolve opens the adapter for a provider's config.
	Resolve func(ctx context.Context, cfg db.ExecutorConfig) (Executor, error)
	Logger  *slog.Logger
	// billWarned holds the providers whose bill couldn't be read, so that is
	// said once (RefreshBills).
	billWarned sync.Map
	// CallTimeout bounds each provider call (a wake or a stop), so one stuck
	// provider fails its own call rather than holding its slot forever (D097,
	// D149). Modal's placement wait is 90 s per GPU type.
	CallTimeout time.Duration
	// MaxCalls caps provider calls running in parallel (default 20: two
	// providers at 10 machines each).
	MaxCalls int

	callsMu   sync.Mutex
	callsCtx  context.Context
	inFlight  int
	callsDone chan struct{}
}

// ErrPreparing is a wake the provider can't start yet but will, such as
// Modal still converting a new image or a provider with no GPU free right
// now: the dispatch stays waking and the wake is tried again with backoff,
// with no failure counted. The claim deadline still bounds it.
var ErrPreparing = errors.New("dispatch: the provider is still preparing the machine")

// New returns a service. Set Resolve before any provider is connected.
func New(pool *pgxpool.Pool, box *secrets.Box, serverURL func() string) *Service {
	return &Service{
		pool: pool, box: box, ProjectID: db.DefaultProjectID, ServerURL: serverURL,
		Resolve: func(context.Context, db.ExecutorConfig) (Executor, error) {
			return nil, errors.New("dispatch: no provider adapter is available")
		},
		Logger:      slog.New(slog.DiscardHandler),
		CallTimeout: 6 * time.Minute,
		MaxCalls:    20,
	}
}

// Route is queue.Queue.Dispatch: in the router's transaction it creates a
// dispatch for a job routed to a provider, with its one-time key, and the
// outbox row that wakes a machine. The key is minted here, not in the
// publisher, so it is committed before any machine can present it, and a
// retried wake hands the same key to the same machine.
func (s *Service) Route(ctx context.Context, qs *db.Queries, job db.Job, kind string) error {
	// The provider's row lock serialises routers, so two can't both take
	// its last slot or the last of its monthly budget (D055). Room is
	// checked in a second statement, whose snapshot is taken after the
	// lock is granted and so sees what the previous holder committed.
	p, err := qs.LockExecutorConfigByKind(ctx, db.LockExecutorConfigByKindParams{ProjectID: job.ProjectID, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // disconnected since the router looked
	}
	if err != nil {
		return fmt.Errorf("lock provider: %w", err)
	}
	hasRoom, err := qs.ProviderHasRoom(ctx, db.ProviderHasRoomParams{ID: p.ID, AssetID: job.AssetID})
	if err != nil {
		return fmt.Errorf("provider room: %w", err)
	}
	if !hasRoom {
		return nil // full since the router looked; the next pass says why
	}
	id := ids.New(ids.Dispatch)
	key := KeyPrefix + rand.Text()
	if _, err := qs.CreateDispatch(ctx, db.CreateDispatchParams{
		ID: id, JobID: job.ID, ExecutorConfigID: p.ID, AssetID: job.AssetID,
		KeyHash: hash(key), KeySealed: s.box.Seal("dispatches", id, []byte(key)),
	}); err != nil {
		return fmt.Errorf("create dispatch: %w", err)
	}
	return queueOutbox(ctx, qs, job.ProjectID, job.AssetID, "wake", id)
}

func queueOutbox(ctx context.Context, qs *db.Queries, projectID, assetID, kind, dispatchID string) error {
	payload, _ := json.Marshal(map[string]string{"dispatch_id": dispatchID})
	return qs.InsertOutbox(ctx, db.InsertOutboxParams{
		ID: ids.New(ids.Event), ProjectID: projectID, Kind: kind,
		AssetID: pgtype.Text{String: assetID, Valid: assetID != ""}, Payload: payload,
	})
}

// AuthenticateWorker makes dispatch keys a workerapi.WorkerAuth. A key works
// while its dispatch is waking and its claim deadline hasn't passed; once
// it has claimed, the attempt token takes over and the key is spent.
func (s *Service) AuthenticateWorker(ctx context.Context, key string) (queue.Worker, bool) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return queue.Worker{}, false
	}
	d, err := db.New(s.pool).GetClaimableDispatchByKeyHash(ctx, hash(key))
	if err != nil || d.ProjectID != s.ProjectID {
		return queue.Worker{}, false
	}
	return queue.Worker{Executor: d.Kind, DispatchID: d.ID, JobID: d.JobID}, true
}

// End ends a dispatch for reason and, if its machine was started, queues
// the stop. Call it in a transaction; ending an ended dispatch does nothing.
func End(ctx context.Context, qs *db.Queries, dispatchID, reason string) error {
	d, err := qs.EndDispatch(ctx, db.EndDispatchParams{ID: dispatchID, EndReason: pgtype.Text{String: reason, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("end dispatch: %w", err)
	}
	if !d.InstanceID.Valid {
		return nil // never started: a pending wake sees it ended and skips
	}
	return queueOutbox(ctx, qs, d.ProjectID, "", "stop", d.ID)
}

// End reasons.
const (
	// EndJobGone: the job was cancelled, ended or rerouted before the
	// machine claimed it.
	EndJobGone = "job_gone"
	// EndWakeFailed: the provider refused or failed to start the machine.
	EndWakeFailed = "wake_failed"
	// EndWakeTimeout: the machine didn't claim before the claim deadline.
	EndWakeTimeout = "wake_timeout"
	// EndAttemptEnded: the machine's attempt committed, failed or lost its
	// lease, so the machine has nothing left to do.
	EndAttemptEnded = "attempt_ended"
	// EndMaxLifetime: the dispatch outlived MaxLifetime.
	EndMaxLifetime = "max_lifetime"
	// EndHandedOff: the machine's job committed and the machine went on to
	// the asset's next job under a new dispatch (HandOff), so nothing was
	// stopped.
	EndHandedOff = "handed_off"
)

// HandOffWindow is how long a machine handed its next job has to claim it.
// It claims at once, with the key the commit's answer carried; if that
// answer was lost, the dispatch ends as a wake timeout after this, the
// machine is stopped, and the job is routed like any other.
const HandOffWindow = 2 * time.Minute

// HandOff is outputs.Committer.HandOff: in the commit of attempt a, it hands
// a's machine the job just queued for the same asset, rather than stop that
// machine and wake another for the job (D128). That happens only when the
// worker said at its claim that it can take a job of that kind next, the
// job is routed to the machine's own provider, and the provider is still
// connected with room under its limits. a's dispatch ends handed off, with
// no stop, and a new dispatch for the job, already woken on the same
// machine, takes its place. HandOff returns the new dispatch's one-time key
// for the commit to answer with, or "" when there is no hand-off.
func (s *Service) HandOff(ctx context.Context, qs *db.Queries, a db.Attempt, job db.Job) (string, error) {
	if !a.DispatchID.Valid || job.Status != "queued" || !followsOn(a.WorkerInfo, job.Kind) {
		return "", nil
	}
	d, err := qs.LockDispatch(ctx, a.DispatchID.String)
	if err != nil {
		return "", fmt.Errorf("lock dispatch: %w", err)
	}
	if d.Status != "claimed" || d.AttemptID.String != a.ID || !d.InstanceID.Valid || job.RoutedTo.String != d.Kind {
		return "", nil
	}
	// The provider's lock serialises this with routers, as in Route.
	p, err := qs.LockExecutorConfigByKind(ctx, db.LockExecutorConfigByKindParams{ProjectID: job.ProjectID, Kind: d.Kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lock provider: %w", err)
	}
	if p.ID != d.ExecutorConfigID || p.SecretsEncrypted == nil {
		return "", nil // disconnected, or connected again as another row
	}
	room, err := qs.ProviderHasRoomForHandOff(ctx, db.ProviderHasRoomForHandOffParams{ID: p.ID, FromID: d.ID, AssetID: job.AssetID})
	if err != nil {
		return "", fmt.Errorf("provider room: %w", err)
	}
	if !room {
		return "", nil
	}
	if _, err := qs.HandOffDispatch(ctx, db.HandOffDispatchParams{ID: d.ID, AttemptID: a.ID}); err != nil {
		return "", fmt.Errorf("hand off dispatch: %w", err)
	}
	id := ids.New(ids.Dispatch)
	key := KeyPrefix + rand.Text()
	if _, err := qs.CreateHandOffDispatch(ctx, db.CreateHandOffDispatchParams{
		ID: id, JobID: job.ID, ExecutorConfigID: p.ID, AssetID: job.AssetID, InstanceID: d.InstanceID,
		KeyHash: hash(key), KeySealed: s.box.Seal("dispatches", id, []byte(key)),
		ClaimWindowSeconds: HandOffWindow.Seconds(),
	}); err != nil {
		return "", fmt.Errorf("create hand-off dispatch: %w", err)
	}
	s.Logger.Info("handed a machine on to the next job", "from", d.ID, "dispatch", id, "job", job.ID, "kind", job.Kind, "provider", d.Kind)
	return key, nil
}

// followsOn reports whether the worker's claim said it can take a job of
// kind after its own (workerapi's follow_on).
func followsOn(workerInfo []byte, kind string) bool {
	var w struct {
		FollowOn []string `json:"follow_on"`
	}
	return json.Unmarshal(workerInfo, &w) == nil && slices.Contains(w.FollowOn, kind)
}

// Start sets the parent context under which Publish launches provider calls.
// Call Wait after ctx is cancelled so serve returns only after every call
// in flight stops (AGENTS.md, D149).
func (s *Service) Start(ctx context.Context) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	s.callsCtx = ctx
}

// Wait blocks until every provider call started by Publish has returned.
func (s *Service) Wait() {
	for {
		s.callsMu.Lock()
		done := s.callsDone
		s.callsMu.Unlock()
		if done == nil {
			return
		}
		<-done
	}
}

// ReleaseLeases clears any outbox leases left by a previous process at boot,
// so wakes and stops whose call was cut off by a crash replay at once.
func (s *Service) ReleaseLeases(ctx context.Context) error {
	return db.New(s.pool).ReleaseDispatchOutboxLeases(ctx)
}

func (s *Service) releaseSlots(n int) {
	s.callsMu.Lock()
	defer s.callsMu.Unlock()
	s.inFlight -= n
	if s.inFlight == 0 && s.callsDone != nil {
		close(s.callsDone)
		s.callsDone = nil
	}
}

// Publish leases pending wake and stop outbox rows up to limit (and free
// MaxCalls slots), starts each provider call in its own goroutine without
// holding a database transaction, and returns at once (D149). Stops are
// leased before wakes.
func (s *Service) Publish(ctx context.Context, limit int) (int, error) {
	maxCalls := s.MaxCalls
	if maxCalls <= 0 {
		maxCalls = 20
	}
	s.callsMu.Lock()
	callsCtx := s.callsCtx
	if callsCtx != nil && callsCtx.Err() != nil {
		s.callsMu.Unlock()
		return 0, nil
	}
	want := min(limit, maxCalls-s.inFlight)
	if want <= 0 {
		s.callsMu.Unlock()
		return 0, nil
	}
	if s.inFlight == 0 {
		s.callsDone = make(chan struct{})
	}
	s.inFlight += want
	s.callsMu.Unlock()

	if callsCtx == nil {
		callsCtx = context.Background()
	}

	lease := (s.CallTimeout + time.Minute).Seconds()
	rows, err := db.New(s.pool).LeaseDispatchOutbox(ctx, db.LeaseDispatchOutboxParams{
		LeaseSeconds: lease,
		MaxRows:      int32(want),
	})
	if err != nil {
		s.releaseSlots(want)
		return 0, err
	}
	if callsCtx.Err() != nil {
		s.releaseSlots(want)
		return 0, nil
	}
	if unused := want - len(rows); unused > 0 {
		s.releaseSlots(unused)
	}
	for _, row := range rows {
		go func() {
			defer s.releaseSlots(1)
			defer func() {
				if v := recover(); v != nil {
					s.Logger.Error("dispatch call panicked", "outbox", row.ID, "kind", row.Kind, "panic", v)
				}
			}()
			s.runRow(callsCtx, row)
		}()
	}
	return len(rows), nil
}

// WakeEnv is what a woken machine is told: where serve is, its one-time
// key, and, for any job but a transcription, to skip speech before it,
// since proving speech (loading the 0.9 GB model) would only delay an
// encode (D118). Such a machine may still be handed the asset's
// transcription after its encode (HandOff, D128):
// CLIPMUX_FOLLOW_ON_SPEECH tells it to prove speech then. A worker from
// before hand-offs ignores it and keeps speech off.
func WakeEnv(server, key, jobKind string) map[string]string {
	env := map[string]string{"CLIPMUX_SERVER": server, "CLIPMUX_WORKER_KEY": key}
	if jobKind != "transcribe" {
		env["CLIPMUX_SPEECH"] = "off"
		env["CLIPMUX_FOLLOW_ON_SPEECH"] = "auto"
	}
	return env
}

func (s *Service) runRow(ctx context.Context, row db.Outbox) {
	var p struct {
		DispatchID string `json:"dispatch_id"`
	}
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		s.Logger.Error("decode dispatch outbox payload", "outbox", row.ID, "kind", row.Kind, "err", err)
		return
	}
	if row.Kind == "wake" {
		s.runWake(ctx, row, p.DispatchID)
		return
	}
	s.runStop(ctx, row, p.DispatchID)
}

func (s *Service) runWake(ctx context.Context, row db.Outbox, dispatchID string) {
	var (
		skip bool
		d    db.Dispatch
		cfg  db.ExecutorConfig
		req  Wake
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		var err error
		d, err = qs.LockDispatch(ctx, dispatchID)
		if err != nil {
			return err
		}
		if d.Status == "ended" || (d.Status == "claimed" && d.InstanceID.Valid) {
			skip = true
			return qs.MarkOutboxDone(ctx, row.ID)
		}
		job, err := qs.GetJob(ctx, d.JobID)
		if err != nil {
			return err
		}
		if d.Status == "waking" && (job.Status != "queued" || job.RoutedTo.String != d.Kind) {
			if err := End(ctx, qs, d.ID, EndJobGone); err != nil {
				return err
			}
			skip = true
			return qs.MarkOutboxDone(ctx, row.ID)
		}
		cfg, err = qs.GetExecutorConfig(ctx, d.ExecutorConfigID)
		if err != nil {
			return err
		}
		key, err := s.box.Open("dispatches", d.ID, d.KeySealed)
		if err != nil {
			return fmt.Errorf("open dispatch key: %w", err)
		}
		req = Wake{
			DispatchID: d.ID, Name: Name(d.ID), Config: cfg.Config, Lifetime: MaxLifetime,
			Env: WakeEnv(s.ServerURL(), string(key), job.Kind),
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			s.retryRow(ctx, row, err)
		}
		return
	}
	if skip {
		return
	}

	ex, err := s.Resolve(ctx, cfg)
	if err != nil {
		if ctx.Err() == nil {
			s.retryRow(ctx, row, err)
		}
		return
	}

	call, cancel := context.WithTimeout(ctx, s.CallTimeout)
	instance, wakeErr := ex.Wake(call, req)
	cancel()
	if ctx.Err() != nil {
		return // shutting down: write nothing; the lease brings the row back
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		cur, err := qs.LockDispatch(ctx, d.ID)
		if err != nil {
			return err
		}
		if wakeErr != nil {
			if cur.Status != "waking" {
				return qs.MarkOutboxDone(ctx, row.ID)
			}
			if errors.Is(wakeErr, ErrPreparing) {
				// Not a failure: the same dispatch and key, asked again later.
				s.Logger.Info("a provider is still preparing a machine", "dispatch", cur.ID, "provider", cur.Kind, "err", wakeErr)
				return s.retry(ctx, qs, row, wakeErr)
			}
			// The job returns to routing (its dispatch ends) without spending a
			// budget, and three such failures in a row back the provider off
			// for 5 minutes. A machine the provider started anyway is found by
			// the reconciler.
			s.Logger.Warn("a provider failed to wake a machine", "dispatch", cur.ID, "provider", cur.Kind, "err", wakeErr)
			if err := qs.RecordWakeFailure(ctx, cfg.ID); err != nil {
				return err
			}
			if err := End(ctx, qs, cur.ID, EndWakeFailed); err != nil {
				return err
			}
			return qs.MarkOutboxDone(ctx, row.ID)
		}
		if cur.Status == "ended" {
			if !cur.InstanceID.Valid {
				s.Logger.Info("woke a machine for an ended dispatch; stopping it", "dispatch", cur.ID, "provider", cur.Kind, "instance", instance)
				if err := qs.RecordWakeSuccess(ctx, cfg.ID); err != nil {
					return err
				}
				if err := qs.SetDispatchWoken(ctx, db.SetDispatchWokenParams{ID: cur.ID, InstanceID: pgtype.Text{String: instance, Valid: true}}); err != nil {
					return err
				}
				if err := queueOutbox(ctx, qs, cur.ProjectID, "", "stop", cur.ID); err != nil {
					return err
				}
			}
			return qs.MarkOutboxDone(ctx, row.ID)
		}
		s.Logger.Info("woke a machine", "dispatch", cur.ID, "job", cur.JobID, "provider", cur.Kind, "instance", instance)
		if err := qs.RecordWakeSuccess(ctx, cfg.ID); err != nil {
			return err
		}
		if err := qs.SetDispatchWoken(ctx, db.SetDispatchWokenParams{ID: cur.ID, InstanceID: pgtype.Text{String: instance, Valid: true}}); err != nil {
			return err
		}
		return qs.MarkOutboxDone(ctx, row.ID)
	})
	if err != nil && ctx.Err() == nil {
		s.Logger.Error("record dispatch wake", "dispatch", d.ID, "outbox", row.ID, "err", err)
	}
}

func (s *Service) runStop(ctx context.Context, row db.Outbox, dispatchID string) {
	var (
		skip       bool
		instanceID string
		kind       string
		cfg        db.ExecutorConfig
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		d, err := qs.LockDispatch(ctx, dispatchID)
		if err != nil {
			return err
		}
		if !d.InstanceID.Valid || d.StoppedAt.Valid {
			skip = true
			return qs.MarkOutboxDone(ctx, row.ID)
		}
		cfg, err = qs.GetExecutorConfig(ctx, d.ExecutorConfigID)
		if err != nil {
			return err
		}
		instanceID = d.InstanceID.String
		kind = d.Kind
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			s.retryRow(ctx, row, err)
		}
		return
	}
	if skip {
		return
	}

	ex, err := s.Resolve(ctx, cfg)
	if err != nil {
		if ctx.Err() == nil {
			s.retryRow(ctx, row, err)
		}
		return
	}

	call, cancel := context.WithTimeout(ctx, s.CallTimeout)
	stopErr := ex.Stop(call, instanceID)
	cancel()
	if ctx.Err() != nil {
		return // shutting down: write nothing; the lease brings the row back
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		if stopErr != nil {
			return s.retry(ctx, qs, row, fmt.Errorf("stop %s: %w", kind, stopErr))
		}
		s.Logger.Info("stopped a machine", "dispatch", dispatchID, "provider", kind, "instance", instanceID)
		if err := qs.SetDispatchStopped(ctx, dispatchID); err != nil {
			return err
		}
		return qs.MarkOutboxDone(ctx, row.ID)
	})
	if err != nil && ctx.Err() == nil {
		s.Logger.Error("record dispatch stop", "dispatch", dispatchID, "outbox", row.ID, "err", err)
	}
}

func (s *Service) retryRow(ctx context.Context, row db.Outbox, cause error) {
	if err := s.retry(ctx, db.New(s.pool), row, cause); err != nil && ctx.Err() == nil {
		s.Logger.Error("schedule dispatch outbox retry", "outbox", row.ID, "kind", row.Kind, "err", err)
	}
}

// retry keeps the row pending, backing off from 5 s to 5 minutes (or 30 s
// when the provider has no GPU free right now). The stored error is a code:
// a provider's message may hold a URL or a key.
func (s *Service) retry(ctx context.Context, qs *db.Queries, row db.Outbox, cause error) error {
	s.Logger.Warn("dispatch publication failed; will retry", "outbox", row.ID, "kind", row.Kind, "attempts", row.Attempts+1, "err", cause)
	maxDelay := 300.0
	if errors.Is(cause, ErrPreparing) {
		maxDelay = 30
	}
	delay := min(5*float64(int64(1)<<min(row.Attempts, 6)), maxDelay)
	return qs.RetryOutbox(ctx, db.RetryOutboxParams{
		ID: row.ID, LastError: pgtype.Text{String: row.Kind + "_failed", Valid: true}, DelaySeconds: delay,
	})
}

// Name is the provider-side name of a dispatch's machine.
func Name(dispatchID string) string { return "clipmux-" + dispatchID }

// Reap ends live dispatches whose machine has nothing left to do, queueing
// a stop for each one that was started, and returns how many it ended
// (D055). A dispatch ends when its attempt is no longer running (committed,
// failed or lost), when its job is no longer queued before the claim, when
// its claim deadline passes, or at MaxLifetime. Ending makes the job
// routable again; a wake timeout spends no retry budget, because the job
// never ran.
func (s *Service) Reap(ctx context.Context, limit int) (int, error) {
	maxLife := MaxLifetime.Seconds()
	candidates, err := db.New(s.pool).ListDispatchesToEnd(ctx, db.ListDispatchesToEndParams{MaxLifetimeSeconds: maxLife, MaxRows: int32(limit)})
	if err != nil {
		return 0, fmt.Errorf("list dispatches to end: %w", err)
	}
	n := 0
	for _, id := range candidates {
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			qs := db.New(tx)
			if _, err := qs.LockDispatch(ctx, id); err != nil {
				return err
			}
			// Decided again under the lock: a claim may have won meanwhile.
			reason, err := qs.DispatchEndReason(ctx, db.DispatchEndReasonParams{ID: id, MaxLifetimeSeconds: maxLife})
			if err != nil || reason == "" {
				return err
			}
			n++
			s.Logger.Info("dispatch ended", "dispatch", id, "reason", reason)
			return End(ctx, qs, id, reason)
		})
		if err != nil {
			return n, fmt.Errorf("end dispatch %s: %w", id, err)
		}
	}
	return n, nil
}

// Reconcile asks each connected provider which ClipMux machines it is
// running and stops any that no unstopped dispatch accounts for, so a
// crash between starting a machine and recording it can't leave a paid
// machine running. A dispatch accounts for a machine by its instance ID,
// or, while its wake may still be in flight, by name. It returns how many
// machines it stopped.
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	cfgs, err := db.New(s.pool).ListExecutorConfigs(ctx, s.ProjectID)
	if err != nil {
		return 0, fmt.Errorf("list providers: %w", err)
	}
	stopped := 0
	for _, cfg := range cfgs {
		ex, err := s.Resolve(ctx, cfg)
		if err != nil {
			return stopped, fmt.Errorf("open %s: %w", cfg.Kind, err)
		}
		running, err := ex.Running(ctx)
		if err != nil {
			return stopped, fmt.Errorf("list %s machines: %w", cfg.Kind, err)
		}
		known, err := db.New(s.pool).ListProviderMachines(ctx, cfg.ID)
		if err != nil {
			return stopped, err
		}
		byID, pending := map[string]bool{}, map[string]bool{}
		for _, k := range known {
			if k.InstanceID.Valid {
				byID[k.InstanceID.String] = true
			} else {
				pending[Name(k.ID)] = true
			}
		}
		for _, m := range running {
			if byID[m.ID] || pending[m.Name] {
				continue
			}
			if err := ex.Stop(ctx, m.ID); err != nil {
				return stopped, fmt.Errorf("stop orphaned %s machine %s: %w", cfg.Kind, m.ID, err)
			}
			s.Logger.Warn("stopped a machine no dispatch accounts for", "provider", cfg.Kind, "instance", m.ID, "name", m.Name)
			stopped++
		}
	}
	return stopped, nil
}

func hash(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
