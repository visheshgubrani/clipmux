package chapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
)

// How an OpenAI-compatible API is asked for JSON: a strict schema, or JSON
// mode for providers without one (Connect finds out which).
const (
	FormatJSONSchema = "json_schema"
	FormatJSONObject = "json_object"
)

// OpenAI is any OpenAI-compatible chat completions API: OpenAI, Ollama,
// vLLM, Groq, OpenRouter. On the standard library.
type OpenAI struct {
	// BaseURL is the API root, e.g. https://api.openai.com/v1 or
	// http://ollama.lan:11434/v1.
	BaseURL string
	Model   string
	// Key is the bearer token; empty for a keyless server.
	Key string
	// Format is FormatJSONSchema (the default) or FormatJSONObject.
	Format string
	HTTP   *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Complete sends one chat completion and returns the message's content.
func (o *OpenAI) Complete(ctx context.Context, p Prompt) (json.RawMessage, error) {
	format := map[string]any{"type": FormatJSONObject}
	if o.Format != FormatJSONObject {
		format = map[string]any{
			"type":        FormatJSONSchema,
			"json_schema": map[string]any{"name": "chapter_outline", "strict": true, "schema": p.Schema},
		}
	}
	body, err := json.Marshal(map[string]any{
		"model":           o.Model,
		"messages":        []chatMessage{{"system", p.System}, {"user", p.User}},
		"response_format": format,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content *string `json:"content"`
				Refusal *string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := o.do(req, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return json.RawMessage{}, nil
	}
	msg := out.Choices[0].Message
	if msg.Refusal != nil && *msg.Refusal != "" {
		return nil, permanent(CodeRefused, "the model refused: %s", clip(*msg.Refusal, 200))
	}
	if msg.Content == nil {
		return json.RawMessage{}, nil
	}
	// Some local models wrap JSON in a Markdown fence even in JSON mode.
	content := strings.TrimSpace(*msg.Content)
	content = strings.TrimPrefix(strings.TrimPrefix(content, "```json"), "```")
	return json.RawMessage(strings.TrimSpace(strings.TrimSuffix(content, "```"))), nil
}

// CheckCredentials lists the API's models, which costs nothing. A server
// without the route (404) passes: the structured-output step proves the
// rest.
func (o *OpenAI) CheckCredentials(ctx context.Context) error {
	_, _, err := o.Models(ctx)
	return err
}

// Models lists the API's models from GET {base}/models, OpenAI's
// {"data": [{"id": …}]}, trimmed, deduplicated and sorted. listed is false
// for a server without the route (404) or whose answer isn't that list:
// the owner types the model's name instead.
func (o *OpenAI) Models(ctx context.Context) (models []ModelInfo, listed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint("/models"), nil)
	if err != nil {
		return nil, false, err
	}
	body, err := o.send(req)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &list) != nil || list.Data == nil {
		return nil, false, nil
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if id := strings.TrimSpace(m.ID); id != "" && len(id) <= maxModelLen {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	models = make([]ModelInfo, 0, min(len(ids), maxModels))
	for _, id := range ids[:min(len(ids), maxModels)] {
		models = append(models, ModelInfo{ID: id})
	}
	return models, true, nil
}

func (o *OpenAI) endpoint(path string) string { return strings.TrimSuffix(o.BaseURL, "/") + path }

func (o *OpenAI) do(req *http.Request, out any) error {
	body, err := o.send(req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{Code: CodeUnavailable, Transient: true, msg: "LLM: the answer isn't JSON: " + clip(err.Error(), 200)}
	}
	return nil
}

// send sends a request with the key and returns a 2xx answer's body.
func (o *OpenAI) send(req *http.Request) ([]byte, error) {
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Transient: true, msg: "LLM: " + logging.RedactError(err).Error()}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, &Error{Code: CodeUnavailable, Transient: true, msg: "LLM: reading the answer: " + err.Error()}
	}
	if res.StatusCode/100 != 2 {
		return nil, statusError(res.StatusCode, body)
	}
	return body, nil
}

// statusError classifies a refusal by status, keeping only the API's own
// message (never the request, which holds the key and the transcript).
func statusError(status int, body []byte) *Error {
	var detail struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := http.StatusText(status)
	if json.Unmarshal(body, &detail) == nil && detail.Error.Message != "" {
		msg = detail.Error.Message
	}
	e := &Error{Code: CodeRejected, Status: status, msg: fmt.Sprintf("LLM: HTTP %d: %s", status, clip(msg, 300))}
	if transientStatus(status) {
		e.Code, e.Transient = CodeUnavailable, true
	}
	return e
}

// transientStatus is a status worth trying again later: a timeout, a
// conflict, a rate limit or the server's own failure (529 is Anthropic's
// overload).
func transientStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusConflict ||
		status == http.StatusTooManyRequests || status >= 500
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
