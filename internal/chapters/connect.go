package chapters

import (
	"context"
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
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// Providers a chapters connection can use.
const (
	ProviderOpenAI    = "openai_compatible"
	ProviderAnthropic = "anthropic"
)

// purpose is the connection's row in ai_connections, beside the speech
// API's (D075).
const purpose = "chapters"

// ErrInvalid is a Connect request that can't work.
var ErrInvalid = errors.New("invalid chapters connection")

// ErrNotConnected means no LLM is connected for chapters.
var ErrNotConnected = errors.New("no LLM is connected for chapters")

// Service is the owner's side: Connect, the status, and disconnecting.
type Service struct {
	pool      *pgxpool.Pool
	box       *secrets.Box
	projectID string
	HTTP      *http.Client
	Logger    *slog.Logger
}

// NewService returns the chapters settings for a project.
func NewService(pool *pgxpool.Pool, box *secrets.Box, projectID string) *Service {
	return &Service{pool: pool, box: box, projectID: projectID, HTTP: http.DefaultClient, Logger: slog.Default()}
}

// ConnectRequest is what the owner pastes.
type ConnectRequest struct {
	// Provider is openai_compatible or anthropic.
	Provider string `json:"provider"`
	// BaseURL is the API root: required for openai_compatible, and for
	// anthropic only to reach something other than Anthropic's API.
	BaseURL string `json:"base_url"`
	// Model defaults to claude-opus-5-5 for anthropic.
	Model string `json:"model"`
	// APIKey is optional for openai_compatible (a keyless Ollama).
	APIKey string `json:"api_key"`
}

// sample is the fixed transcript Connect makes chapters of: three short
// segments on two topics.
var sample = transcripts.Transcript{
	Version: 1, Language: "en", DurationMS: 30000,
	Segments: []transcripts.Segment{
		{ID: "s0", Text: "Welcome to the course. Today we set up our tools.", StartMS: 0, EndMS: 9000},
		{ID: "s1", Text: "First, install the editor and open a new project.", StartMS: 9000, EndMS: 18000},
		{ID: "s2", Text: "Now a new topic: writing our first program and running it.", StartMS: 18000, EndMS: 30000},
	},
}

// Connect checks an LLM in steps (credentials, a real structured answer on
// a fixed transcript, saved) and saves it, verified, with its key sealed.
// For an OpenAI-compatible API it tries a strict JSON schema first and
// falls back to JSON mode, and remembers which worked. A failed step
// leaves any earlier connection as it was. The base URL isn't filtered
// for private addresses: Ollama on the owner's LAN is a use (D018).
func (s *Service) Connect(ctx context.Context, req ConnectRequest) (dispatch.Result, error) {
	req.BaseURL, req.Model = strings.TrimSpace(req.BaseURL), strings.TrimSpace(req.Model)
	switch req.Provider {
	case ProviderOpenAI:
		if req.Model == "" {
			return dispatch.Result{}, fmt.Errorf("%w: model is required, e.g. gpt-5-mini or llama3.3", ErrInvalid)
		}
		if err := checkBaseURL(req.BaseURL); err != nil {
			return dispatch.Result{}, err
		}
	case ProviderAnthropic:
		if req.APIKey == "" {
			return dispatch.Result{}, fmt.Errorf("%w: api_key is required for anthropic", ErrInvalid)
		}
		if req.Model == "" {
			req.Model = DefaultClaudeModel
		}
		if req.BaseURL != "" {
			if err := checkBaseURL(req.BaseURL); err != nil {
				return dispatch.Result{}, err
			}
		}
	default:
		return dispatch.Result{}, fmt.Errorf(`%w: provider must be "openai_compatible" or "anthropic"`, ErrInvalid)
	}
	if len(req.Model) > 200 {
		return dispatch.Result{}, fmt.Errorf("%w: model is too long", ErrInvalid)
	}
	conn := Connection{Provider: req.Provider, BaseURL: req.BaseURL, Model: req.Model}
	if req.Provider == ProviderOpenAI {
		conn.ResponseFormat = FormatJSONSchema
	}
	steps := dispatch.Steps{Provider: "chapters LLM", Logger: s.Logger}
	steps.Do("credentials", func() (string, error) {
		if err := s.model(conn, req.APIKey).(checker).CheckCredentials(ctx); err != nil {
			return "", err
		}
		return "the API accepted the key for " + req.Model, nil
	})
	steps.Do("structured_output", func() (string, error) {
		out, err := Generate(ctx, s.model(conn, req.APIKey), sample)
		var e *Error
		if req.Provider == ProviderOpenAI && errors.As(err, &e) && e.Code == CodeRejected {
			// No strict schemas here: try JSON mode.
			conn.ResponseFormat = FormatJSONObject
			out, err = Generate(ctx, s.model(conn, req.APIKey), sample)
		}
		if err != nil {
			return "", err
		}
		titles := make([]string, len(out))
		for i, c := range out {
			titles[i] = fmt.Sprintf("%q", c.Title)
		}
		how := ""
		if conn.ResponseFormat != "" {
			how = " with " + conn.ResponseFormat
		}
		return fmt.Sprintf("made %d chapters of a sample%s: %s", len(out), how, strings.Join(titles, ", ")), nil
	})
	steps.Do("saved", func() (string, error) {
		return "", s.save(ctx, conn, req.APIKey)
	})
	return steps.Result, nil
}

// Model IDs longer than Connect accepts are dropped, and a list is cut at
// maxModels: OpenAI's own is about a hundred.
const (
	maxModelLen = 200
	maxModels   = 1000
)

// ModelInfo is one model an API lists; Name is Claude's display name and
// empty for an OpenAI-compatible API.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ModelList is what an API lists. Listed is false when it has no list to
// read, and the owner types the model's name.
type ModelList struct {
	Listed bool        `json:"listed"`
	Data   []ModelInfo `json:"data"`
}

// ModelsRequest is the provider, URL and key the owner has typed so far.
type ModelsRequest struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

// Models lists an LLM's models for the Connect form, before anything is
// saved; the key is used for the one call and kept nowhere.
func (s *Service) Models(ctx context.Context, req ModelsRequest) (ModelList, error) {
	req.BaseURL, req.APIKey = strings.TrimSpace(req.BaseURL), strings.TrimSpace(req.APIKey)
	var lister interface {
		Models(context.Context) ([]ModelInfo, bool, error)
	}
	switch req.Provider {
	case ProviderOpenAI:
		if err := checkBaseURL(req.BaseURL); err != nil {
			return ModelList{}, err
		}
		lister = &OpenAI{BaseURL: req.BaseURL, Key: req.APIKey, HTTP: s.HTTP}
	case ProviderAnthropic:
		if req.APIKey == "" {
			return ModelList{}, fmt.Errorf("%w: api_key is required for anthropic", ErrInvalid)
		}
		if req.BaseURL != "" {
			if err := checkBaseURL(req.BaseURL); err != nil {
				return ModelList{}, err
			}
		}
		lister = NewClaude(req.APIKey, "", req.BaseURL, s.HTTP)
	default:
		return ModelList{}, fmt.Errorf(`%w: provider must be "openai_compatible" or "anthropic"`, ErrInvalid)
	}
	models, listed, err := lister.Models(ctx)
	if err != nil {
		return ModelList{}, err
	}
	if models == nil {
		models = []ModelInfo{}
	}
	return ModelList{Listed: listed, Data: models}, nil
}

type checker interface {
	CheckCredentials(ctx context.Context) error
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" {
		return fmt.Errorf("%w: base_url must be an http or https URL with no user or query, e.g. https://api.openai.com/v1", ErrInvalid)
	}
	return nil
}

// model builds the adapter for a connection.
func (s *Service) model(c Connection, key string) Model {
	return open(c, key, s.HTTP)
}

func open(c Connection, key string, hc *http.Client) Model {
	if c.Provider == ProviderAnthropic {
		return NewClaude(key, c.Model, c.BaseURL, hc)
	}
	return &OpenAI{BaseURL: c.BaseURL, Model: c.Model, Key: key, Format: c.ResponseFormat, HTTP: hc}
}

// save stores the verified connection, its key sealed for the row it will
// update (the row ID is part of the associated data, D017).
func (s *Service) save(ctx context.Context, c Connection, key string) error {
	q := db.New(s.pool)
	id := ids.New(ids.Connection)
	if row, err := q.GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: s.projectID, Purpose: purpose}); err == nil {
		id = row.ID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var sealed []byte
	if key != "" {
		sealed = s.box.Seal("ai_connections", id, []byte(key))
	}
	_, err := q.UpsertAIConnection(ctx, db.UpsertAIConnectionParams{
		ID: id, ProjectID: s.projectID, Purpose: purpose, Provider: c.Provider,
		BaseUrl: pgtype.Text{String: c.BaseURL, Valid: c.BaseURL != ""}, Model: c.Model,
		ResponseFormat: pgtype.Text{String: c.ResponseFormat, Valid: c.ResponseFormat != ""}, KeySealed: sealed,
	})
	return err
}

// Connection is a saved LLM, as the dashboard shows it; never the key.
type Connection struct {
	Provider       string    `json:"provider"`
	BaseURL        string    `json:"base_url,omitempty"`
	Model          string    `json:"model"`
	ResponseFormat string    `json:"response_format,omitempty"`
	HasKey         bool      `json:"has_key"`
	VerifiedAt     time.Time `json:"verified_at"`
}

// Status is the chapters settings.
type Status struct {
	Connection *Connection `json:"connection"`
}

// Status reads the project's chapters connection.
func (s *Service) Status(ctx context.Context) (Status, error) {
	row, err := db.New(s.pool).GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: s.projectID, Purpose: purpose})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Status{}, nil
	case err != nil:
		return Status{}, err
	}
	return Status{Connection: &Connection{
		Provider: row.Provider, BaseURL: row.BaseUrl.String, Model: row.Model, ResponseFormat: row.ResponseFormat.String,
		HasKey: row.KeySealed != nil, VerifiedAt: row.VerifiedAt.Time,
	}}, nil
}

// Disconnect forgets the connection. Chapters already queued then end
// llm_not_connected; ready ones keep playing.
func (s *Service) Disconnect(ctx context.Context) error {
	n, err := db.New(s.pool).DeleteAIConnection(ctx, db.DeleteAIConnectionParams{ProjectID: s.projectID, Purpose: purpose})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotConnected
	}
	return nil
}

// Load opens the project's chapters model, its key unsealed.
func Load(ctx context.Context, pool *pgxpool.Pool, box *secrets.Box, projectID string, hc *http.Client) (Model, error) {
	row, err := db.New(pool).GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: projectID, Purpose: purpose})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.VerifiedAt.Valid) {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	var key string
	if row.KeySealed != nil {
		k, err := box.Open("ai_connections", row.ID, row.KeySealed)
		if err != nil {
			return nil, fmt.Errorf("open the chapters LLM key: %w", err)
		}
		key = string(k)
	}
	c := Connection{Provider: row.Provider, BaseURL: row.BaseUrl.String, Model: row.Model, ResponseFormat: row.ResponseFormat.String}
	return open(c, key, hc), nil
}
