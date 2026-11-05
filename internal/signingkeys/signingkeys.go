// Package signingkeys issues the ES256 keys playback tokens are signed with
// (docs/architecture.md, "Tokens"). An app's key pair is generated for its
// backend: ClipMux keeps only the public half. The server's own internal
// key, which signs short-lived tokens for dashboard preview, setup tests and
// share links, keeps its private half sealed with the secrets key.
package signingkeys

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

const sealedTable = "signing_keys"

// CreateForApp generates a key pair for an app, stores the public key and
// returns the kid and the private key, which is never stored or shown again.
func CreateForApp(ctx context.Context, conn db.DBTX, projectID, appID string) (string, string, error) {
	privatePEM, publicPEM, err := tokens.GenerateKey()
	if err != nil {
		return "", "", err
	}
	kid := ids.New(ids.SigningKey)
	q := db.New(conn)
	if _, err := q.CreateSigningKey(ctx, db.CreateSigningKeyParams{
		ID: kid, ProjectID: projectID, AppID: pgtype.Text{String: appID, Valid: true}, PublicKeyPem: publicPEM,
	}); err != nil {
		return "", "", fmt.Errorf("create signing key: %w", err)
	}
	// The Worker learns of the key through _keys.json.
	if err := routes.QueueKeys(ctx, q, projectID); err != nil {
		return "", "", fmt.Errorf("create signing key: %w", err)
	}
	return kid, privatePEM, nil
}

// EnsureInternal creates the project's internal key unless it has one.
func EnsureInternal(ctx context.Context, conn db.DBTX, box *secrets.Box, projectID string) error {
	q := db.New(conn)
	if _, err := q.GetInternalSigningKey(ctx, projectID); err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	privatePEM, publicPEM, err := tokens.GenerateKey()
	if err != nil {
		return err
	}
	kid := ids.New(ids.SigningKey)
	// A concurrent serve may win; the partial unique index keeps one.
	_, err = q.CreateInternalSigningKey(ctx, db.CreateInternalSigningKeyParams{
		ID: kid, ProjectID: projectID, PublicKeyPem: publicPEM,
		PrivateKeyEncrypted: box.Seal(sealedTable, kid, []byte(privatePEM)),
	})
	if err != nil {
		return fmt.Errorf("create internal signing key: %w", err)
	}
	return routes.QueueKeys(ctx, q, projectID)
}

// Signer signs playback tokens with one key.
type Signer struct {
	Kid string
	key *ecdsa.PrivateKey
}

// Sign returns a signed token.
func (s Signer) Sign(c tokens.Claims) (string, error) { return tokens.Sign(s.key, s.Kid, c) }

// Internal returns a signer for the project's internal key.
func Internal(ctx context.Context, conn db.DBTX, box *secrets.Box, projectID string) (Signer, error) {
	k, err := db.New(conn).GetInternalSigningKey(ctx, projectID)
	if err != nil {
		return Signer{}, fmt.Errorf("internal signing key: %w", err)
	}
	privatePEM, err := box.Open(sealedTable, k.ID, k.PrivateKeyEncrypted)
	if err != nil {
		return Signer{}, fmt.Errorf("open internal signing key: %w", err)
	}
	key, err := tokens.ParsePrivateKey(string(privatePEM))
	if err != nil {
		return Signer{}, err
	}
	return Signer{Kid: k.ID, key: key}, nil
}
