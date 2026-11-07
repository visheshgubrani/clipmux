package speechapi

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// sample is 11 s of public-domain speech (JFK's inaugural address) exactly
// as a transcription sends it: the media pipeline's audio rendition, its
// init segment and decrypted segments joined into one fragmented MP4.
// Connect transcribes it, so an API that won't take that format fails at
// Connect, not on a job. Regenerate with
// `go test ./internal/speechapi -run TestSampleIsAnAudioChunk -make-sample`.
//
//go:embed sample.m4a
var sample []byte

// ErrInvalid is a Connect request that can't work.
var ErrInvalid = errors.New("invalid speech connection")

// ErrNotConnected means a mode that uses the API was chosen with none
// verified.
var ErrNotConnected = errors.New("no speech API is connected")

// Service is the owner's side: Connect, the speech mode, and disconnecting.
type Service struct {
	pool      *pgxpool.Pool
	box       *secrets.Box
	projectID string
	HTTP      *http.Client
	Logger    *slog.Logger
}

// NewService returns the speech settings for a project.
func NewService(pool *pgxpool.Pool, box *secrets.Box, projectID string) *Service {
	return &Service{pool: pool, box: box, projectID: projectID, HTTP: http.DefaultClient, Logger: slog.Default()}
}

// ConnectRequest is what the owner pastes. The key is optional, for a
// server on their own network that takes none.
type ConnectRequest struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
}

// Connect checks a speech API in steps (credentials, a real transcription
// of the sample, saved) and saves it, verified, with its key sealed. A
// failed step leaves any earlier connection as it was. The base URL isn't
// filtered for private addresses: a LAN speech server is a use, as for
// webhooks (D018).
func (s *Service) Connect(ctx context.Context, req ConnectRequest) (dispatch.Result, error) {
	req.BaseURL, req.Model = strings.TrimSpace(req.BaseURL), strings.TrimSpace(req.Model)
	u, err := url.Parse(req.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return dispatch.Result{}, fmt.Errorf("%w: base_url must be an http or https URL with no user or query, e.g. https://api.groq.com/openai/v1", ErrInvalid)
	}
	if req.Model == "" || len(req.Model) > 200 {
		return dispatch.Result{}, fmt.Errorf("%w: model is required, e.g. whisper-large-v3-turbo", ErrInvalid)
	}
	c := &Client{BaseURL: req.BaseURL, Model: req.Model, Key: req.APIKey, HTTP: s.HTTP}
	steps := dispatch.Steps{Provider: "speech API", Logger: s.Logger}
	steps.Do("credentials", func() (string, error) {
		if err := c.CheckCredentials(ctx); err != nil {
			return "", err
		}
		return "the API answered " + c.Host(), nil
	})
	steps.Do("transcription", func() (string, error) {
		res, err := c.Transcribe(ctx, Request{Audio: sample})
		if err != nil {
			return "", err
		}
		if len(res.Segments) == 0 {
			return "", errors.New("the API heard no speech in the sample: it may not read fragmented MP4 audio")
		}
		words := "with word timings"
		if len(res.Words) == 0 {
			words = "without word timings (one subtitle per segment)"
		}
		return fmt.Sprintf("transcribed the sample as %q, %d segments, %s", transcripts.LanguageCode(res.Language), len(res.Segments), words), nil
	})
	steps.Do("saved", func() (string, error) {
		return "", s.save(ctx, req)
	})
	return steps.Result, nil
}

// save stores the verified connection, its key sealed for the row it will
// update (the row ID is part of the associated data, D017).
func (s *Service) save(ctx context.Context, req ConnectRequest) error {
	q := db.New(s.pool)
	id := ids.New(ids.Connection)
	if row, err := q.GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: s.projectID, Purpose: "speech"}); err == nil {
		id = row.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var sealed []byte
	if req.APIKey != "" {
		sealed = s.box.Seal("ai_connections", id, []byte(req.APIKey))
	}
	_, err := q.UpsertAIConnection(ctx, db.UpsertAIConnectionParams{
		ID: id, ProjectID: s.projectID, Purpose: "speech", Provider: "openai_compatible",
		BaseUrl: pgtype.Text{String: req.BaseURL, Valid: true}, Model: req.Model, KeySealed: sealed,
	})
	return err
}

// Status is the speech settings as the dashboard shows them; never the key.
type Status struct {
	Mode       string      `json:"mode"`
	Connection *Connection `json:"connection"`
}

// Connection is a saved speech API.
type Connection struct {
	BaseURL    string    `json:"base_url"`
	Model      string    `json:"model"`
	HasKey     bool      `json:"has_key"`
	VerifiedAt time.Time `json:"verified_at"`
}

// Status reads the project's speech mode and connection.
func (s *Service) Status(ctx context.Context) (Status, error) {
	q := db.New(s.pool)
	mode, err := q.GetSpeechMode(ctx, s.projectID)
	if err != nil {
		return Status{}, err
	}
	st := Status{Mode: mode}
	row, err := q.GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: s.projectID, Purpose: "speech"})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Status{}, err
	default:
		st.Connection = &Connection{BaseURL: row.BaseUrl.String, Model: row.Model, HasKey: row.KeySealed != nil, VerifiedAt: row.VerifiedAt.Time}
	}
	return st, nil
}

// SetMode chooses where transcriptions go. api and local_then_api need a
// verified connection. The router moves queued jobs to match.
func (s *Service) SetMode(ctx context.Context, mode string) error {
	switch mode {
	case queue.SpeechLocal:
	case queue.SpeechAPI, queue.SpeechLocalThenAPI:
		row, err := db.New(s.pool).GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: s.projectID, Purpose: "speech"})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.VerifiedAt.Valid) {
			return ErrNotConnected
		}
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: mode must be local, api or local_then_api", ErrInvalid)
	}
	return db.New(s.pool).SetSpeechMode(ctx, db.SetSpeechModeParams{ID: s.projectID, SpeechMode: mode})
}

// Disconnect forgets the connection and returns the project to local
// speech, in one transaction; the router moves queued transcriptions back
// to the project's executors.
func (s *Service) Disconnect(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.DeleteAIConnection(ctx, db.DeleteAIConnectionParams{ProjectID: s.projectID, Purpose: "speech"})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotConnected
		}
		return q.SetSpeechMode(ctx, db.SetSpeechModeParams{ID: s.projectID, SpeechMode: queue.SpeechLocal})
	})
}
