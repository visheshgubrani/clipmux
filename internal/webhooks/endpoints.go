package webhooks

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// sealedTable is the table endpoint secrets are sealed for.
const sealedTable = "webhook_endpoints"

// ErrInvalidURL means an endpoint URL was refused.
var ErrInvalidURL = errors.New("webhooks: the endpoint must be an absolute http or https URL of at most 2048 characters, with no credentials or fragment")

// CreateEndpoint registers rawURL to receive an app's events. It returns the
// endpoint and its signing secret, which is shown once and stored sealed.
// Only the owner creates endpoints (the dashboard, `clipmux admin`), never an
// integration key, so the URL is not filtered for private addresses: a
// self-hosted receiver often lives on the same private network.
func CreateEndpoint(ctx context.Context, q *db.Queries, box *secrets.Box, projectID, appID, rawURL string) (db.WebhookEndpoint, string, error) {
	u, err := checkURL(rawURL)
	if err != nil {
		return db.WebhookEndpoint{}, "", err
	}
	id := ids.New(ids.Webhook)
	secret := "whsec_" + rand.Text()
	ep, err := q.CreateWebhookEndpoint(ctx, db.CreateWebhookEndpointParams{
		ID: id, ProjectID: projectID, AppID: appID, Url: u,
		SecretEncrypted: box.Seal(sealedTable, id, []byte(secret)),
	})
	return ep, secret, err
}

// checkURL accepts an absolute http or https URL with a host and no
// credentials or fragment.
func checkURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil, len(raw) > 2048,
		u.Scheme != "http" && u.Scheme != "https", u.Host == "",
		u.User != nil, u.Fragment != "" || u.RawFragment != "" || u.ForceQuery:
		return "", ErrInvalidURL
	}
	return u.String(), nil
}

// pendingTable seals a secret waiting to be activated, so it can't be
// copied into secret_encrypted without being opened and sealed again.
const pendingTable = "webhook_endpoints_pending"

// Rotating a secret is two steps (D069): RotateSecret makes a new secret and
// returns it once, while deliveries go on being signed with the old one;
// the owner gives it to the receiver, then ActivateSecret switches signing
// to it. Receivers check one signature, so the switch is the moment the
// receiver must accept the new one; a receiver that accepts both during
// the change loses nothing.
var (
	ErrNoEndpoint      = errors.New("webhooks: no such endpoint")
	ErrNoPendingSecret = errors.New("webhooks: the endpoint has no new secret to activate")
)

// RotateSecret makes a new secret for the endpoint, replacing any pending
// one, and returns it. It doesn't sign anything until ActivateSecret.
func RotateSecret(ctx context.Context, q *db.Queries, box *secrets.Box, projectID, id string) (string, error) {
	if ep, err := q.GetWebhookEndpoint(ctx, id); err != nil || ep.ProjectID != projectID {
		return "", ErrNoEndpoint
	}
	secret := "whsec_" + rand.Text()
	n, err := q.SetPendingWebhookSecret(ctx, db.SetPendingWebhookSecretParams{ID: id, PendingSecretEncrypted: box.Seal(pendingTable, id, []byte(secret))})
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", ErrNoEndpoint // disabled
	}
	return secret, nil
}

// ActivateSecret makes the pending secret the one deliveries are signed
// with, from the next try on.
func ActivateSecret(ctx context.Context, q *db.Queries, box *secrets.Box, projectID, id string) error {
	ep, err := q.GetWebhookEndpoint(ctx, id)
	if err != nil || ep.ProjectID != projectID {
		return ErrNoEndpoint
	}
	if ep.PendingSecretEncrypted == nil {
		return ErrNoPendingSecret
	}
	secret, err := box.Open(pendingTable, id, ep.PendingSecretEncrypted)
	if err != nil {
		return err
	}
	n, err := q.ActivateWebhookSecret(ctx, db.ActivateWebhookSecretParams{ID: id, SecretEncrypted: box.Seal(sealedTable, id, secret)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNoPendingSecret // activated meanwhile
	}
	return nil
}

// RedeliverWindow is how long a redelivered event is retried: a fresh
// 24 hours, as for a new one.
const RedeliverWindow = 24 * time.Hour
