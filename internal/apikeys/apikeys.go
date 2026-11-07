// Package apikeys issues and checks integration API keys
// (docs/architecture.md, "Accounts and tenancy"). Each key belongs to one app
// and carries scopes; no integration key can manage apps, keys, agents,
// adapters or settings. Secrets are shown once and stored only as SHA-256.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/ratelimit"
)

// Scopes an integration key can carry.
const (
	AssetsRead   = "assets:read"
	AssetsWrite  = "assets:write"
	UploadsWrite = "uploads:write"
)

var allScopes = []string{AssetsRead, AssetsWrite, UploadsWrite}

// ErrInvalid means the presented secret is not a usable key.
var ErrInvalid = errors.New("apikeys: invalid key")

// secretPrefix makes leaked keys easy for secret scanners to spot.
const secretPrefix = "cmx_"

// Key is an authenticated key.
type Key struct {
	ID        string
	ProjectID string
	AppID     string
	Scopes    []string
	// Owner is set when the dashboard owner acts as AppID.
	Owner bool
}

// AllScopes are every scope an integration key can carry.
func AllScopes() []string { return slices.Clone(allScopes) }

// Can reports whether the key carries scope.
func (k Key) Can(scope string) bool { return slices.Contains(k.Scopes, scope) }

// Create issues a key for an app and returns its secret, which is never
// stored or shown again.
func Create(ctx context.Context, conn db.DBTX, projectID, appID, name string, scopes []string) (string, db.APIKey, error) {
	if len(scopes) == 0 {
		return "", db.APIKey{}, errors.New("apikeys: at least one scope is required")
	}
	for _, s := range scopes {
		if !slices.Contains(allScopes, s) {
			return "", db.APIKey{}, fmt.Errorf("apikeys: unknown scope %q", s)
		}
	}
	secret := secretPrefix + rand.Text()
	k, err := db.New(conn).CreateAPIKey(ctx, db.CreateAPIKeyParams{
		ID: ids.New(ids.APIKey), ProjectID: projectID, AppID: appID, Name: name, KeyHash: hash(secret), Scopes: scopes,
	})
	if err != nil {
		return "", db.APIKey{}, fmt.Errorf("create api key: %w", err)
	}
	return secret, k, nil
}

// Authenticate resolves a secret to its key.
func Authenticate(ctx context.Context, conn db.DBTX, secret string) (Key, error) {
	if secret == "" {
		return Key{}, ErrInvalid
	}
	q := db.New(conn)
	row, err := q.GetAPIKeyByHash(ctx, hash(secret))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrInvalid
	}
	if err != nil {
		return Key{}, fmt.Errorf("authenticate api key: %w", err)
	}
	// Best effort, throttled to once a minute in SQL.
	_ = q.TouchAPIKey(ctx, row.ID)
	return Key{ID: row.ID, ProjectID: row.ProjectID, AppID: row.AppID, Scopes: row.Scopes}, nil
}

func hash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Authenticator decides which key a request acts with: its bearer API key,
// or, when Session is set and the request has no bearer token, the dashboard
// owner acting as one app. The dashboard has no private path; it calls the
// same endpoints with its session.
type Authenticator struct {
	conn db.DBTX
	// Session resolves a request without a bearer token; nil means only
	// API keys are accepted.
	Session func(r *http.Request) (Key, error)
	// Limits rate each key's requests; nil limits nothing. serve shares one
	// across the APIs, so a key's budget is the same wherever it calls.
	Limits *Limits
}

// Limits are the integration API's rates per key (D067): 20 requests a
// second with bursts of 200, and of those, 2 writes a second with bursts of
// 60 (creating uploads, imports, re-encodes, playback IDs, deletes). At
// most 4 event long-polls run at once per key.
type Limits struct {
	all, writes *ratelimit.Buckets
	events      *ratelimit.Slots
}

// Per-key limits.
const (
	ReadsPerSecond, ReadBurst   = 20, 200
	WritesPerSecond, WriteBurst = 2, 60
	EventPolls                  = 4
)

// NewLimits returns the shipped limits.
func NewLimits() *Limits {
	return &Limits{
		all:    ratelimit.NewBuckets(ReadsPerSecond, ReadBurst),
		writes: ratelimit.NewBuckets(WritesPerSecond, WriteBurst),
		events: ratelimit.NewSlots(EventPolls),
	}
}

// limitKey is what a key's requests count against: the API key, or the
// owner's session acting as an app.
func (k Key) limitKey() string {
	if k.ID != "" {
		return k.ID
	}
	return "session:" + k.AppID
}

// take spends one request of k's budget, writing the 429 if it is spent.
func (l *Limits) take(w http.ResponseWriter, r *http.Request, k Key) bool {
	if l == nil {
		return true
	}
	ok, wait := l.all.Take(k.limitKey())
	if ok && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		ok, wait = l.writes.Take(k.limitKey())
	}
	if !ok {
		ratelimit.Refuse(w, wait)
	}
	return ok
}

// EventPoll takes one of k's event long-poll slots; release gives it back.
// Refused, it has written the 429.
func (l *Limits) EventPoll(w http.ResponseWriter, k Key) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	release, ok = l.events.Acquire(k.limitKey())
	if !ok {
		ratelimit.Refuse(w, time.Second)
	}
	return release, ok
}

// NewAuthenticator accepts API keys only, until Session is set.
func NewAuthenticator(conn db.DBTX) *Authenticator { return &Authenticator{conn: conn} }

// Authenticate resolves r to a key.
func (a *Authenticator) Authenticate(r *http.Request) (Key, error) {
	if bearer := httpjson.Bearer(r); bearer != "" || a.Session == nil {
		return Authenticate(r.Context(), a.conn, bearer)
	}
	return a.Session(r)
}

// Require authenticates r and checks that its key carries scope. Otherwise
// it writes 401 unauthorized (or the session's own refusal) or 403
// insufficient_scope and returns false.
func (a *Authenticator) Require(w http.ResponseWriter, r *http.Request, scope string) (Key, bool) {
	key, err := a.Authenticate(r)
	if err != nil {
		if errors.Is(err, ErrInvalid) || !httpjson.WriteProblem(w, err) {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "a valid API key is required")
		}
		return Key{}, false
	}
	if !key.Can(scope) {
		httpjson.Error(w, http.StatusForbidden, "insufficient_scope", "this key lacks "+scope)
		return Key{}, false
	}
	if !a.Limits.take(w, r, key) {
		return Key{}, false
	}
	return key, true
}
