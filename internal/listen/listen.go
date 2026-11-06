// Package listen relays an app's webhook events to a local URL, for a
// laptop that can't receive webhooks (docs/architecture.md, "Webhooks":
// `clipmux listen --forward-to localhost:3000/webhooks`). It long-polls
// GET /v1/events with an API key and POSTs each event exactly as the
// webhook publisher would, signed with a secret the developer configures in
// their app, so the app's webhook code runs unchanged.
package listen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

// overlap re-reads this much before the last event seen, because a
// transaction that began earlier can commit an event after a later one is
// visible. Events are deduplicated by ID.
const overlap = 30 * time.Second

// Relay forwards one app's events.
type Relay struct {
	Server    string // ClipMux's public URL
	Key       string // an API key with assets:read
	ForwardTo string // the local webhook URL
	Secret    []byte // signs forwarded events, like an endpoint's whsec_ secret
	// Types, when set, are the only event types forwarded.
	Types []string
	HTTP  *http.Client
	// Report is told about each forwarded event.
	Report func(eventType, id string, status int, err error)
	// Sleep waits between polls after an error; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

type event struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Body      json.RawMessage `json:"body"`
}

// Error is a refusal from the API.
type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message) }

func (r *Relay) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

func (r *Relay) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// poll long-polls for events at or after since.
// The server answers at once when an event is newer than after, else after
// up to 25 s.
func (r *Relay) poll(ctx context.Context, since, after time.Time) ([]event, error) {
	q := url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}, "wait": {"25"}}
	if !after.IsZero() {
		q.Set("after", after.UTC().Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(r.Server, "/")+"/v1/events?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Key)
	res, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e) // a non-JSON error still reports its status
		return nil, &Error{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	var body struct {
		Data []event `json:"data"`
	}
	return body.Data, json.Unmarshal(data, &body)
}

// Run relays events created from since on until ctx ends, or the API
// refuses the key.
func (r *Relay) Run(ctx context.Context, since time.Time) error {
	seen := map[string]time.Time{}
	var newest time.Time
	backoff := time.Second
	for ctx.Err() == nil {
		events, err := r.poll(ctx, since, newest)
		var apiErr *Error
		switch {
		case errors.As(err, &apiErr) && apiErr.Status < 500:
			return err
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			if r.Report != nil {
				r.Report("", "", 0, fmt.Errorf("polling %s: %w", r.Server, err))
			}
			if err := r.sleep(ctx, backoff); err != nil {
				return nil
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		latest := since
		for _, e := range events {
			latest = e.CreatedAt
			if e.CreatedAt.After(newest) {
				newest = e.CreatedAt
			}
			if _, ok := seen[e.ID]; ok {
				continue
			}
			seen[e.ID] = e.CreatedAt
			r.forward(ctx, e)
		}
		// Move on, keeping an overlap unless the page was full (then there
		// may be more at the same time, and an overlap could loop). With
		// nothing new, the window stays where it is.
		switch {
		case len(events) >= 100:
			since = latest
		case len(events) > 0 && latest.Add(-overlap).After(since):
			since = latest.Add(-overlap)
		}
		for id, at := range seen {
			if at.Before(since.Add(-overlap)) {
				delete(seen, id)
			}
		}
	}
	return nil
}

// forward POSTs one event to the local URL as the webhook publisher would
// (D018), retrying briefly while a dev server restarts.
func (r *Relay) forward(ctx context.Context, e event) {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(e.Body, &head) // the publisher wrote it; a bad body forwards with no type
	if len(r.Types) > 0 && !slices.Contains(r.Types, head.Type) {
		return
	}
	var status int
	var err error
	for try := range 4 {
		if try > 0 {
			if r.sleep(ctx, time.Duration(1<<(try-1))*time.Second) != nil {
				return
			}
		}
		status, err = r.post(ctx, e, head.Type)
		if err == nil && status < 500 {
			break
		}
	}
	if r.Report != nil {
		r.Report(head.Type, e.ID, status, err)
	}
}

func (r *Relay) post(ctx context.Context, e event, eventType string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ForwardTo, bytes.NewReader(e.Body))
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ClipMux-Listen/1.0")
	req.Header.Set("X-Webhook-Event", eventType)
	req.Header.Set("X-Webhook-Id", e.ID)
	req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Webhook-Signature", webhooks.Sign(r.Secret, now, e.Body))
	res, err := r.client().Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	res.Body.Close()
	return res.StatusCode, nil
}
