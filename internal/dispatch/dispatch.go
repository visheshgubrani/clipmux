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
	"strings"
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
}

// New returns a service. Set Resolve before any provider is connected.
func New(pool *pgxpool.Pool, box *secrets.Box, serverURL func() string) *Service {
	return &Service{
		pool: pool, box: box, ProjectID: db.DefaultProjectID, ServerURL: serverURL,
		Resolve: func(context.Context, db.ExecutorConfig) (Executor, error) {
			return nil, errors.New("dispatch: no provider adapter is available")
		},
		Logger: slog.New(slog.DiscardHandler),
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
)

// Publish wakes and stops machines for pending outbox rows, up to limit,
// and returns how many it handled. Like the route publisher, it calls the
// provider while holding the row, rebuilds from current state rather than
// the payload, and retries a failure with backoff.
func (s *Service) Publish(ctx context.Context, limit int) (int, error) {
	done := 0
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		rows, err := qs.LockPendingDispatchOutbox(ctx, int32(limit))
		if err != nil {
			return err
		}
		for _, row := range rows {
			var p struct {
				DispatchID string `json:"dispatch_id"`
			}
			if err := json.Unmarshal(row.Payload, &p); err != nil {
				return fmt.Errorf("%s %s: %w", row.Kind, row.ID, err)
			}
			var err error
			if row.Kind == "wake" {
				err = s.wake(ctx, qs, p.DispatchID)
			} else {
				err = s.stop(ctx, qs, p.DispatchID)
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err := s.retry(ctx, qs, row, err); err != nil {
					return err
				}
				continue
			}
			if err := qs.MarkOutboxDone(ctx, row.ID); err != nil {
				return err
			}
			done++
		}
		return nil
	})
	return done, err
}

func (s *Service) wake(ctx context.Context, qs *db.Queries, dispatchID string) error {
	d, err := qs.LockDispatch(ctx, dispatchID)
	if err != nil {
		return err
	}
	if d.Status != "waking" {
		return nil // ended before it was woken, or woken and claimed already
	}
	job, err := qs.GetJob(ctx, d.JobID)
	if err != nil {
		return err
	}
	if job.Status != "queued" || job.RoutedTo.String != d.Kind {
		return End(ctx, qs, d.ID, EndJobGone)
	}
	ex, cfg, err := s.executor(ctx, qs, d)
	if err != nil {
		return err
	}
	key, err := s.box.Open("dispatches", d.ID, d.KeySealed)
	if err != nil {
		return fmt.Errorf("open dispatch key: %w", err)
	}
	instance, err := ex.Wake(ctx, Wake{
		DispatchID: d.ID, Name: Name(d.ID), Config: cfg.Config, Lifetime: MaxLifetime,
		Env: map[string]string{"CLIPMUX_SERVER": s.ServerURL(), "CLIPMUX_WORKER_KEY": string(key)},
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // shutting down: wake on the next pass, same key
		}
		// The job returns to routing (its dispatch ends) without spending a
		// budget, and three such failures in a row back the provider off
		// for 5 minutes. A machine the provider started anyway is found by
		// the reconciler.
		s.Logger.Warn("a provider failed to wake a machine", "dispatch", d.ID, "provider", d.Kind, "err", err)
		if err := qs.RecordWakeFailure(ctx, cfg.ID); err != nil {
			return err
		}
		return End(ctx, qs, d.ID, EndWakeFailed)
	}
	s.Logger.Info("woke a machine", "dispatch", d.ID, "job", d.JobID, "provider", d.Kind, "instance", instance)
	if err := qs.RecordWakeSuccess(ctx, cfg.ID); err != nil {
		return err
	}
	return qs.SetDispatchWoken(ctx, db.SetDispatchWokenParams{ID: d.ID, InstanceID: pgtype.Text{String: instance, Valid: true}})
}

func (s *Service) stop(ctx context.Context, qs *db.Queries, dispatchID string) error {
	d, err := qs.LockDispatch(ctx, dispatchID)
	if err != nil {
		return err
	}
	if !d.InstanceID.Valid || d.StoppedAt.Valid {
		return nil
	}
	ex, _, err := s.executor(ctx, qs, d)
	if err != nil {
		return err
	}
	if err := ex.Stop(ctx, d.InstanceID.String); err != nil {
		return fmt.Errorf("stop %s: %w", d.Kind, err)
	}
	s.Logger.Info("stopped a machine", "dispatch", d.ID, "provider", d.Kind, "instance", d.InstanceID.String)
	return qs.SetDispatchStopped(ctx, d.ID)
}

func (s *Service) executor(ctx context.Context, qs *db.Queries, d db.Dispatch) (Executor, db.ExecutorConfig, error) {
	cfg, err := qs.GetExecutorConfig(ctx, d.ExecutorConfigID)
	if err != nil {
		return nil, cfg, err
	}
	ex, err := s.Resolve(ctx, cfg)
	return ex, cfg, err
}

// retry keeps the row pending, backing off from 5 s to 5 minutes. The
// stored error is a code: a provider's message may hold a URL or a key.
func (s *Service) retry(ctx context.Context, qs *db.Queries, row db.Outbox, cause error) error {
	s.Logger.Warn("dispatch publication failed; will retry", "outbox", row.ID, "kind", row.Kind, "attempts", row.Attempts+1, "err", cause)
	delay := min(5*float64(int64(1)<<min(row.Attempts, 6)), 300)
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
