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
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
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
}

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
