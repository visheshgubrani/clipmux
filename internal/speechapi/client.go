// Package speechapi sends transcriptions to a speech API the owner
// connects (D075; docs/plans/P5-production.md, "Speech API"): any
// OpenAI-compatible POST /audio/transcriptions that answers verbose_json
// with segment and word timestamps (OpenAI, Groq, Together, Fireworks,
// DeepInfra, or Speaches, LocalAI and vLLM on the owner's own network).
//
// serve runs these itself, as the executor class speech_api: the key never
// reaches a worker, and audio leaves the owner's infrastructure only when
// they connect an API and choose a speech mode that uses it.
package speechapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// Client is one connection's API.
type Client struct {
	// BaseURL is the API root, e.g. https://api.groq.com/openai/v1.
	BaseURL string
	Model   string
	// Key is the bearer token; empty for a keyless server.
	Key  string
	HTTP *http.Client
}

// Error is a failed call. Transient ones (408, 409, 429, 5xx, the network)
// are worth retrying; the rest mean the API refused the request.
type Error struct {
	Status     int
	Transient  bool
	RetryAfter time.Duration
	msg        string
}

func (e *Error) Error() string { return e.msg }

// Request is one chunk to transcribe.
type Request struct {
	Audio []byte
	// Language is ISO 639-1, or "" to let the API detect it.
	Language string
	// Prompt is text that came just before this chunk, for continuity.
	Prompt string
}

// requestTimeout bounds one call: 24 MiB up and the API's own work.
const requestTimeout = 10 * time.Minute

// Transcribe sends one chunk and returns the API's verbose_json answer.
func (c *Client) Transcribe(ctx context.Context, r Request) (transcripts.OpenAIResponse, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fields := [][2]string{
		{"model", c.Model}, {"response_format", "verbose_json"},
		{"timestamp_granularities[]", "segment"}, {"timestamp_granularities[]", "word"},
		{"temperature", "0"},
	}
	if r.Language != "" {
		fields = append(fields, [2]string{"language", r.Language})
	}
	if r.Prompt != "" {
		fields = append(fields, [2]string{"prompt", r.Prompt})
	}
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return transcripts.OpenAIResponse{}, err
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="chunk.m4a"`)
	h.Set("Content-Type", "audio/mp4")
	part, err := mw.CreatePart(h)
	if err != nil {
		return transcripts.OpenAIResponse{}, err
	}
	if _, err := part.Write(r.Audio); err != nil {
		return transcripts.OpenAIResponse{}, err
	}
	if err := mw.Close(); err != nil {
		return transcripts.OpenAIResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/audio/transcriptions"), &body)
	if err != nil {
		return transcripts.OpenAIResponse{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out transcripts.OpenAIResponse
	if err := c.do(req, &out); err != nil {
		return transcripts.OpenAIResponse{}, err
	}
	if out.Segments == nil {
		return transcripts.OpenAIResponse{}, &Error{Status: http.StatusOK, msg: "the answer has no segments: the API doesn't give verbose_json timestamps"}
	}
	return out, nil
}

// CheckCredentials lists the API's models, which every OpenAI-compatible
// server answers and which costs nothing. A server without the route (404)
// passes: the transcription step proves the rest.
func (c *Client) CheckCredentials(ctx context.Context) error {
	_, _, err := c.Models(ctx)
	return err
}

// Model IDs longer than Connect accepts are dropped, and a list is cut at
// maxModels: OpenAI's own is about a hundred.
const (
	maxModelLen = 200
	maxModels   = 1000
)

// Models lists the API's models from GET {base}/models, OpenAI's
// {"data": [{"id": …}]}, trimmed, deduplicated and sorted. listed is false
// for a server without the route (404) or whose answer isn't that list:
// the owner types the model's name instead.
func (c *Client) Models(ctx context.Context) (ids []string, listed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/models"), nil)
	if err != nil {
		return nil, false, err
	}
	body, err := c.send(req)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
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
	ids = make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		if id := strings.TrimSpace(m.ID); id != "" && len(id) <= maxModelLen {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > maxModels {
		ids = ids[:maxModels]
	}
	return ids, true, nil
}

func (c *Client) endpoint(path string) string { return strings.TrimSuffix(c.BaseURL, "/") + path }

// do sends a request with the key and decodes a JSON answer into out.
func (c *Client) do(req *http.Request, out any) error {
	body, err := c.send(req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &Error{Status: http.StatusOK, msg: "speech API: the answer isn't JSON: " + err.Error()}
	}
	return nil
}

// send sends a request with the key and returns a 2xx answer's body.
func (c *Client) send(req *http.Request) ([]byte, error) {
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, &Error{Transient: true, msg: "speech API: " + logging.RedactError(err).Error()}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, &Error{Transient: true, msg: "speech API: reading the answer: " + err.Error()}
	}
	if res.StatusCode/100 != 2 {
		return nil, apiError(res, body)
	}
	return body, nil
}

// apiError classifies a refusal, keeping only the API's own message (never
// the request, which holds the key).
func apiError(res *http.Response, body []byte) *Error {
	var detail struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := http.StatusText(res.StatusCode)
	if json.Unmarshal(body, &detail) == nil && detail.Error.Message != "" {
		msg = detail.Error.Message
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	e := &Error{Status: res.StatusCode, msg: fmt.Sprintf("speech API: HTTP %d: %s", res.StatusCode, msg)}
	switch {
	case res.StatusCode == http.StatusRequestTimeout, res.StatusCode == http.StatusConflict,
		res.StatusCode == http.StatusTooManyRequests, res.StatusCode >= 500:
		e.Transient = true
	}
	if secs, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && secs >= 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	return e
}

// Host is the API's host, for the transcript's model name and the logs.
func (c *Client) Host() string {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return "unknown"
	}
	return u.Host
}
