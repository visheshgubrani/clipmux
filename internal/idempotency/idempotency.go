// Package idempotency makes retried POSTs safe (docs/architecture.md,
// "Integration API"): a request sent again with the same Idempotency-Key
// returns the first response instead of acting twice. Keys belong to an app
// and to one route; reusing a key with a different request is refused.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// MaxKey is the longest Idempotency-Key accepted.
const MaxKey = 255

var (
	// ErrTooLong means the key exceeds MaxKey bytes.
	ErrTooLong = errors.New("idempotency: Idempotency-Key is limited to 255 bytes")
	// ErrReused means the key was used with a different request (422).
	ErrReused = errors.New("idempotency: this Idempotency-Key was used with a different request")
	// ErrInProgress means the first request with the key hasn't finished (409).
	ErrInProgress = errors.New("idempotency: a request with this Idempotency-Key is still running")
)

// Claim is one request's hold on its key, inside the request's transaction.
type Claim struct {
	q          *db.Queries
	appID, key string
	// Replay is the first response's body when the request was already
	// answered; the caller returns it (and Status) instead of acting.
	Replay []byte
	Status int
}

// Begin claims key for a request to route (e.g. "POST /v1/uploads") whose
// canonical body is body. An empty key returns a nil Claim: nothing to
// remember. A key seen before returns its stored response in Replay, or
// ErrReused or ErrInProgress. Concurrent requests with one key wait for the
// first to commit.
func Begin(ctx context.Context, q *db.Queries, projectID, appID, key, route string, body []byte) (*Claim, error) {
	if key == "" {
		return nil, nil
	}
	if len(key) > MaxKey {
		return nil, ErrTooLong
	}
	sum := sha256.Sum256(append([]byte(route+"\n"), body...))
	c := &Claim{q: q, appID: appID, key: key}
	_, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{ProjectID: projectID, AppID: appID, Key: key, RequestHash: sum[:]})
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	prev, err := q.GetIdempotencyKey(ctx, db.GetIdempotencyKeyParams{AppID: appID, Key: key})
	if err != nil {
		return nil, err
	}
	switch {
	case !bytes.Equal(prev.RequestHash, sum[:]):
		return nil, ErrReused
	case len(prev.ResponseBody) == 0:
		return nil, ErrInProgress
	}
	c.Replay, c.Status = prev.ResponseBody, int(prev.ResponseStatus.Int32)
	return c, nil
}

// Save stores the response to replay. A nil Claim saves nothing.
func (c *Claim) Save(ctx context.Context, status int, body []byte) error {
	if c == nil {
		return nil
	}
	return c.q.SaveIdempotentResponse(ctx, db.SaveIdempotentResponseParams{
		AppID: c.appID, Key: c.key, ResponseStatus: pgtype.Int4{Int32: int32(status), Valid: true}, ResponseBody: body,
	})
}

// Problem maps Begin's refusals to their API errors and passes any other
// error through.
func Problem(err error) error {
	switch {
	case errors.Is(err, ErrTooLong):
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "Idempotency-Key is limited to 255 bytes")
	case errors.Is(err, ErrReused):
		return httpjson.Fail(http.StatusUnprocessableEntity, "idempotency_key_reused", "this Idempotency-Key was used with a different request")
	case errors.Is(err, ErrInProgress):
		return httpjson.Fail(http.StatusConflict, "idempotency_in_progress", "a request with this Idempotency-Key is still running")
	}
	return err
}
