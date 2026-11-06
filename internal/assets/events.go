package assets

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// GET /v1/events serves `clipmux listen`: an app's webhook events from a
// time on, long-polled, so a laptop that can't receive webhooks still gets
// them. An event's body is the asset view, which assets:read already reads.

const (
	maxEvents       = 100
	eventsWait      = 25 * time.Second
	eventsPollEvery = time.Second
)

// Event is one webhook event: its ID, type and the exact body the
// webhook publisher sends.
type Event struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Body      json.RawMessage `json:"body"`
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	key, ok := a.Auth.Require(w, r, apikeys.AssetsRead)
	if !ok {
		return
	}
	since := time.Now()
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", "since must be an RFC 3339 time")
			return
		}
		since = t
	}
	// after is the newest event the caller has; the poll waits for a newer
	// one, while still returning everything from since (an overlap that
	// catches events committed out of order).
	var after time.Time
	if s := r.URL.Query().Get("after"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", "after must be an RFC 3339 time")
			return
		}
		after = t
	}
	wait := eventsWait
	if s := r.URL.Query().Get("wait"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > int(eventsWait/time.Second) {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", "wait is 0 to 25 seconds")
			return
		}
		wait = time.Duration(n) * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		rows, err := db.New(a.pool).ListAppEvents(r.Context(), db.ListAppEventsParams{
			AppID: pgtype.Text{String: key.AppID, Valid: true}, Since: pgtype.Timestamptz{Time: since, Valid: true}, MaxEvents: maxEvents,
		})
		if err != nil {
			if r.Context().Err() == nil {
				httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			}
			return
		}
		fresh := len(rows) == maxEvents
		for _, e := range rows {
			fresh = fresh || e.CreatedAt.Time.After(after)
		}
		if fresh || time.Now().After(deadline) {
			out := make([]Event, len(rows))
			for i, e := range rows {
				out[i] = Event{ID: e.ID, CreatedAt: e.CreatedAt.Time, Body: e.Payload}
			}
			httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(eventsPollEvery):
		}
	}
}
