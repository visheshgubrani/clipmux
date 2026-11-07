package chapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/visheshgubrani/clipmux/internal/logging"
)

// DefaultClaudeModel is the model a Claude connection starts with; the
// owner can name another.
const DefaultClaudeModel = "claude-opus-5-5"

// claudeMaxTokens leaves room for the model's thinking as well as the
// small outline.
const claudeMaxTokens = 16000

// Claude makes chapters through Anthropic's Messages API with the official
// Go SDK, never an OpenAI-compatible shim: structured outputs at low
// effort, no thinking settings, sampling parameters or prefill (which
// Opus 5.5 refuses). The SDK neither retries nor reads serve's environment:
// the job's own budget governs retries, and only the owner's key is used.
type Claude struct {
	client anthropic.Client
	Model  string
}

// NewClaude returns a Claude client. baseURL is empty for Anthropic's API;
// tests point it at a fake.
func NewClaude(key, model, baseURL string, hc *http.Client) *Claude {
	opts := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(CallTimeout),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	if hc != nil {
		opts = append(opts, option.WithHTTPClient(hc))
	}
	if model == "" {
		model = DefaultClaudeModel
	}
	return &Claude{client: anthropic.NewClient(opts...), Model: model}
}

// Complete sends one message and returns the text of the answer.
func (c *Claude) Complete(ctx context.Context, p Prompt) (json.RawMessage, error) {
	msg, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.Model),
		MaxTokens: claudeMaxTokens,
		System:    []anthropic.TextBlockParam{{Text: p.System}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(p.User))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: p.Schema},
		},
	})
	if err != nil {
		return nil, claudeError(err)
	}
	// A refusal is the model's answer, not a fault: the owner's choice of
	// model is the one that runs, with no fallback (P5 open question 11).
	if msg.StopReason == anthropic.StopReasonRefusal {
		return nil, permanent(CodeRefused, "the model refused (%s)", msg.StopDetails.Category)
	}
	var text strings.Builder
	for _, block := range msg.Content {
		if b, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(b.Text)
		}
	}
	return json.RawMessage(strings.TrimSpace(text.String())), nil
}

// CheckCredentials reads the model, which proves the key and the model ID
// and costs nothing.
func (c *Claude) CheckCredentials(ctx context.Context) error {
	if _, err := c.client.Models.Get(ctx, c.Model, anthropic.ModelGetParams{}); err != nil {
		return claudeError(err)
	}
	return nil
}

// claudeError classifies an SDK error: rate limits, overloads, server
// errors and the network are transient; any other answer is a refusal of
// the request (a bad key, model or body).
func claudeError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if msg == "" {
			msg = http.StatusText(apiErr.StatusCode)
		}
		e := &Error{Code: CodeRejected, Status: apiErr.StatusCode, msg: fmt.Sprintf("Claude: HTTP %d: %s", apiErr.StatusCode, clip(msg, 300))}
		if transientStatus(apiErr.StatusCode) {
			e.Code, e.Transient = CodeUnavailable, true
		}
		return e
	}
	return &Error{Code: CodeUnavailable, Transient: true, msg: "Claude: " + clip(logging.RedactError(err).Error(), 300)}
}
