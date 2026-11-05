package webhooks

import (
	"context"
	"crypto/rand"
	"errors"
	"net/url"

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
