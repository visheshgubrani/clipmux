package chapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// Runner runs chapters jobs in serve, one at a time, in a leader loop of
// its own: a model can take minutes, and deletes and moves shouldn't wait.
// The key is opened for each job and reaches nothing else.
type Runner struct {
	Pool      *pgxpool.Pool
	Box       *secrets.Box
	Publisher *tracks.Publisher
	HTTP      *http.Client
	Logger    *slog.Logger
	// Drain, once done, hands a job in flight back to run after a restart,
	// without spending a try.
	Drain context.Context
	// Lease outlasts one job: two calls and the commit (default 10 min).
	Lease time.Duration
	// Tries is how many transient failures a job may have (default 3);
	// Backoff is the wait after the nth (default 30 s, then 2 min).
	Tries   int32
	Backoff func(n int32) time.Duration
	// OpenModel replaces Load, for tests.
	OpenModel func(ctx context.Context, projectID string) (Model, error)
}

var chaptersKinds = []string{tracks.JobChapters}

// Run claims at most one due chapters job and runs it, returning how many
// it ran.
func (r *Runner) Run(ctx context.Context) (int, error) {
	if r.Drain != nil && r.Drain.Err() != nil {
		return 0, nil
	}
	lease := r.Lease
	if lease == 0 {
		lease = 10 * time.Minute
	}
	job, err := db.New(r.Pool).ClaimControlJob(ctx, db.ClaimControlJobParams{LeaseSeconds: lease.Seconds(), Kinds: chaptersKinds})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("claim chapters job: %w", err)
	}
	return 1, r.run(ctx, job)
}

func (r *Runner) run(ctx context.Context, job db.Job) error {
	log := r.logger().With("job", job.ID, "kind", job.Kind, "track", job.TrackID.String)
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	if r.Drain != nil {
		defer context.AfterFunc(r.Drain, cancel)()
	}
	res, err := r.make(work, job)
	if r.Drain != nil && r.Drain.Err() != nil && err != nil {
		// Back to the queue as it was: the job did nothing wrong.
		hctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		return db.New(r.Pool).DeferJob(hctx, db.DeferJobParams{ID: job.ID, DelaySeconds: 0})
	}
	var e *Error
	switch {
	case errors.Is(err, tracks.ErrStale):
		log.Info("chapters job has nothing left to make")
		return r.finish(ctx, job, "")
	case errors.As(err, &e) && e.Transient:
		return r.retry(ctx, job, e, log)
	case errors.As(err, &e):
		log.Warn("chapters failed", "code", e.Code, "err", err)
		return r.finish(ctx, job, e.Code)
	case err != nil:
		// Our own fault (the database, storage): the lease brings it back.
		return err
	}
	var committed bool
	err = pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		committed = false
		qs := db.New(tx)
		locked, err := qs.LockJob(ctx, job.ID)
		if err != nil || locked.Status != "running" {
			return err // cancelled meanwhile: commit nothing
		}
		err = r.Publisher.CommitChapters(ctx, qs, locked, res)
		if errors.Is(err, tracks.ErrStale) {
			return qs.CancelJob(ctx, job.ID)
		}
		if err != nil {
			return err
		}
		if err := qs.SucceedJob(ctx, job.ID); err != nil {
			return err
		}
		if err := tracks.QueueNext(ctx, qs, job.AssetID); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if err == nil && committed {
		r.Publisher.PublishInline(ctx, r.logger(), job.AssetID)
	}
	return err
}

// make reads the asset's ready transcript and asks the model.
func (r *Runner) make(ctx context.Context, job db.Job) (tracks.ChaptersResult, error) {
	q := db.New(r.Pool)
	track, err := q.GetTrack(ctx, job.TrackID.String)
	if err != nil {
		return tracks.ChaptersResult{}, err
	}
	if track.Status != tracks.Preparing {
		return tracks.ChaptersResult{}, tracks.ErrStale
	}
	subs, err := q.GetTrackOfKind(ctx, db.GetTrackOfKindParams{AssetID: job.AssetID, Kind: tracks.Subtitles, Statuses: []string{tracks.Ready}})
	if errors.Is(err, pgx.ErrNoRows) {
		return tracks.ChaptersResult{}, permanent(tracks.CodeSubtitlesFailed, "the asset has no ready subtitles")
	}
	if err != nil {
		return tracks.ChaptersResult{}, err
	}
	row, err := q.GetTranscript(ctx, subs.ID)
	if err != nil {
		return tracks.ChaptersResult{}, err
	}
	var t transcripts.Transcript
	if err := json.Unmarshal(row.Body, &t); err != nil {
		return tracks.ChaptersResult{}, fmt.Errorf("transcript %s: %w", subs.ID, err)
	}
	open := r.OpenModel
	if open == nil {
		open = func(ctx context.Context, projectID string) (Model, error) {
			return Load(ctx, r.Pool, r.Box, projectID, r.HTTP)
		}
	}
	m, err := open(ctx, job.ProjectID)
	if errors.Is(err, ErrNotConnected) {
		return tracks.ChaptersResult{}, permanent(CodeNotConnected, "connect an LLM for chapters")
	}
	if err != nil {
		return tracks.ChaptersResult{}, err
	}
	out, err := Generate(ctx, m, t)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) && ctx.Err() == nil {
			// Anything an adapter didn't classify is the network's.
			err = &Error{Code: CodeUnavailable, Transient: true, msg: clip(err.Error(), 300)}
		}
		return tracks.ChaptersResult{}, err
	}
	body, err := json.Marshal(out)
	if err != nil {
		return tracks.ChaptersResult{}, err
	}
	return tracks.ChaptersResult{
		VTT: VTT(out, t.DurationMS), Chapters: body, DurationMS: t.DurationMS,
		Language: t.Language, OutputID: t.OutputID,
	}, nil
}

// retry counts a transient failure and tries again after the backoff, or
// ends the track once every try is spent.
func (r *Runner) retry(ctx context.Context, job db.Job, e *Error, log *slog.Logger) error {
	tries := r.Tries
	if tries == 0 {
		tries = 3
	}
	n, err := db.New(r.Pool).CountJobFailure(ctx, job.ID)
	if err != nil {
		return err
	}
	if n >= tries {
		log.Warn("chapters failed for now on every try", "tries", n, "err", e)
		return r.finish(ctx, job, e.Code)
	}
	backoff := r.Backoff
	if backoff == nil {
		backoff = func(n int32) time.Duration { return 30 * time.Second << (2 * (n - 1)) }
	}
	log.Info("chapters will be tried again", "tries", n, "err", e)
	return db.New(r.Pool).DeferJob(ctx, db.DeferJobParams{ID: job.ID, DelaySeconds: backoff(n).Seconds()})
}

// finish ends a job that made nothing: with a code it errors the track
// (tracks.Fail), and the next track job may start.
func (r *Runner) finish(ctx context.Context, job db.Job, code string) error {
	return pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		locked, err := qs.LockJob(ctx, job.ID)
		if err != nil || locked.Status != "running" {
			return err
		}
		if code == "" {
			if err := qs.CancelJob(ctx, job.ID); err != nil {
				return err
			}
		} else {
			if err := qs.FailJob(ctx, db.FailJobParams{ID: job.ID, ErrorCode: pgtype.Text{String: code, Valid: true}}); err != nil {
				return err
			}
			if err := tracks.Fail(ctx, qs, locked, code); err != nil {
				return err
			}
		}
		return tracks.QueueNext(ctx, qs, job.AssetID)
	})
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}
