package speechapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// Failure codes of a transcription through the API.
const (
	// CodeUnavailable: the API didn't answer, or kept refusing for now
	// (429, 5xx), or isn't connected any more. A capacity code: it says
	// nothing about the file.
	CodeUnavailable = "speech_api_unavailable"
	// CodeRejected: the API refused the request (a bad key, model or file).
	CodeRejected = "speech_api_rejected"
)

// Runner runs the transcriptions routed to speech_api, one at a time, as
// an executor would: it claims through the queue, heartbeats, and commits
// or fails the attempt, all in process.
type Runner struct {
	Pool      *pgxpool.Pool
	Queue     *queue.Queue
	Stores    *stores.Set
	Keys      contentkey.Ring
	Box       *secrets.Box
	Publisher *tracks.Publisher
	HTTP      *http.Client
	Logger    *slog.Logger
	// Drain, once done, ends a transcription in flight: its attempt goes
	// back as worker_stopped (D065), so the job runs again after a restart.
	Drain context.Context
	// ChunkBytes bounds an upload (default ChunkBytes).
	ChunkBytes int
	// OnAudio counts the seconds of audio sent (clipmux_speech_api_audio_seconds_total).
	OnAudio func(seconds float64)
	// Heartbeat is how often the attempt's lease is renewed (default 15 s).
	Heartbeat time.Duration
}

var speechWorker = queue.Worker{
	Executor: queue.RouteSpeechAPI, Kinds: []string{tracks.JobTranscribe}, Speech: true,
	Info: json.RawMessage(`{"speech_api":true}`),
}

// Run claims at most one transcription routed to the API and runs it. It
// returns how many it ran, for the background loop.
func (r *Runner) Run(ctx context.Context) (int, error) {
	if r.Drain != nil && r.Drain.Err() != nil {
		return 0, nil
	}
	c, err := r.Queue.Claim(ctx, speechWorker)
	if err != nil || c == nil {
		return 0, err
	}
	log := r.Logger.With("job", c.Job.ID, "attempt", c.Attempt.ID, "kind", c.Job.Kind)
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	if r.Drain != nil {
		defer context.AfterFunc(r.Drain, cancel)()
	}
	var told stopFlag
	var wg sync.WaitGroup
	wg.Go(func() { r.heartbeat(work, cancel, c.Token, &told, log) })
	tr, code, permanent, err := r.transcribe(work, c)
	cancel()
	wg.Wait()

	switch {
	case told.get():
		log.Info("speech API attempt stopped by the server")
		return 1, nil
	case r.Drain != nil && r.Drain.Err() != nil:
		hctx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		_, ferr := r.Queue.Fail(hctx, c.Token, queue.Failure{Code: queue.CodeWorkerStopped, Message: "serve is shutting down"})
		return 1, ferr
	case err != nil:
		log.Warn("speech API transcription failed", "code", code, "err", err)
		_, ferr := r.Queue.Fail(ctx, c.Token, queue.Failure{Permanent: permanent, Code: code, Message: truncate(err.Error(), 500)})
		return 1, ferr
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		return 1, err
	}
	_, err = r.Queue.CommitAttempt(ctx, c.Token, r.Publisher.CommitTranscript(ctx, raw))
	var inv *transcripts.InvalidError
	if errors.As(err, &inv) {
		log.Warn("speech API transcript refused", "reason", inv.Reason)
		_, err = r.Queue.Fail(ctx, c.Token, queue.Failure{Permanent: true, Code: tracks.CodeTranscriptInvalid, Message: truncate(inv.Reason, 500)})
	}
	return 1, err
}

// transcribe reads the attempt's input output and sends its audio. It
// returns the failure code with any error, and whether a retry can't help.
func (r *Runner) transcribe(ctx context.Context, c *queue.Claim) (*transcripts.Transcript, string, bool, error) {
	client, err := Load(ctx, r.Pool, r.Box, c.Job.ProjectID, r.HTTP)
	if err != nil {
		return nil, CodeUnavailable, false, err
	}
	out, err := db.New(r.Pool).GetOutput(ctx, c.Attempt.InputOutputID.String)
	if err != nil {
		return nil, "source_download_failed", false, err
	}
	store, err := r.Stores.ForAsset(ctx, c.Job.AssetID)
	if err != nil {
		return nil, "source_download_failed", false, err
	}
	audio, err := outputs.OpenAudio(ctx, store, r.Keys, out)
	if errors.Is(err, outputs.ErrNoAudio) {
		return nil, tracks.CodeNoAudio, true, err
	}
	if err != nil {
		return nil, "source_download_failed", false, err
	}
	maxBytes := r.ChunkBytes
	if maxBytes == 0 {
		maxBytes = ChunkBytes
	}
	tr, err := transcribeAudio(ctx, client, audio, maxBytes, r.OnAudio)
	var apiErr *Error
	var inv *transcripts.InvalidError
	switch {
	case errors.As(err, &apiErr) && apiErr.Transient:
		return nil, CodeUnavailable, false, err
	case errors.As(err, &apiErr):
		return nil, CodeRejected, true, err
	case errors.As(err, &inv):
		return nil, tracks.CodeTranscriptInvalid, true, err
	case err != nil:
		return nil, "source_download_failed", false, err
	}
	return tr, "", false, nil
}

// heartbeat renews the lease while the transcription runs, and cancels it
// when the server says stop (the job was cancelled or superseded).
func (r *Runner) heartbeat(ctx context.Context, cancel context.CancelFunc, token string, told *stopFlag, log *slog.Logger) {
	every := r.Heartbeat
	if every == 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		v, err := r.Queue.Heartbeat(ctx, token, queue.Progress{Phase: "transcribe"})
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("speech API heartbeat", "err", err)
			}
			continue
		}
		if v.Stop {
			told.set()
			cancel()
			return
		}
	}
}

type stopFlag struct {
	mu sync.Mutex
	v  bool
}

func (f *stopFlag) set() { f.mu.Lock(); f.v = true; f.mu.Unlock() }
func (f *stopFlag) get() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// errNotConnected means the project has no verified speech connection.
var errNotConnected = errors.New("speechapi: no speech API is connected")

// Load opens the project's speech connection, its key unsealed.
func Load(ctx context.Context, pool *pgxpool.Pool, box *secrets.Box, projectID string, client *http.Client) (*Client, error) {
	row, err := db.New(pool).GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: projectID, Purpose: "speech"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotConnected, err)
	}
	if !row.VerifiedAt.Valid {
		return nil, errNotConnected
	}
	c := &Client{BaseURL: row.BaseUrl.String, Model: row.Model, HTTP: client}
	if row.KeySealed != nil {
		key, err := box.Open("ai_connections", row.ID, row.KeySealed)
		if err != nil {
			return nil, fmt.Errorf("open the speech API key: %w", err)
		}
		c.Key = string(key)
	}
	return c, nil
}
