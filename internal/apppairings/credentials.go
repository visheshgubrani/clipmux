package apppairings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
	"github.com/visheshgubrani/clipmux/internal/settings"
	"github.com/visheshgubrani/clipmux/internal/signingkeys"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

// ErrPublicURLRequired refuses credentials while the public URL is loopback
// and nobody confirmed they are for apps on this machine (D108).
var ErrPublicURLRequired = errors.New("set a public URL reachable from outside this machine, or confirm the credentials are for apps on this machine (local)")

// Options say how to issue an app's credentials.
type Options struct {
	// PublicURL is the server's, which the app calls (CLIPMUX_URL).
	PublicURL string
	// Local confirms the credentials are for apps on this machine, which a
	// loopback public URL needs (D108).
	Local bool
	// WebhookURL, if set, registers an endpoint for the app's events.
	WebhookURL string
	// KeyName names the API key; "integration" when blank.
	KeyName string
}

// Credentials are everything an app's backend needs, shown once: the
// secrets are not stored (the API key and signing key) or stored sealed for
// signing only (the webhook secret).
type Credentials struct {
	URL               string `json:"url"`
	AppID             string `json:"app_id"`
	AppName           string `json:"app_name"`
	APIKeyID          string `json:"api_key_id"`
	APIKey            string `json:"api_key"`
	SigningKid        string `json:"signing_kid"`
	SigningKey        string `json:"signing_key"`
	WebhookEndpointID string `json:"webhook_endpoint_id,omitempty"`
	WebhookSecret     string `json:"webhook_secret,omitempty"`
	// Env is the same as a .env block.
	Env string `json:"env"`
}

// Issue creates an app's credentials: an API key with the default scopes,
// a signing key, and with o.WebhookURL a webhook endpoint. conn should be a
// transaction, so a refused webhook URL leaves no key behind. box seals the
// webhook secret and may be nil without a webhook URL.
func Issue(ctx context.Context, conn db.DBTX, box *secrets.Box, app db.App, o Options) (Credentials, error) {
	if app.IsBuiltin {
		return Credentials{}, apikeys.ErrBuiltinApp
	}
	if !o.Local && !settings.KeysAllowed(o.PublicURL) {
		return Credentials{}, ErrPublicURLRequired
	}
	name := strings.TrimSpace(o.KeyName)
	if name == "" {
		name = "integration"
	}
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	c := Credentials{URL: strings.TrimRight(o.PublicURL, "/"), AppID: app.ID, AppName: app.Name}
	secret, key, err := apikeys.Create(ctx, conn, app.ProjectID, app.ID, name, apikeys.DefaultScopes())
	if err != nil {
		return Credentials{}, err
	}
	c.APIKeyID, c.APIKey = key.ID, secret
	if c.SigningKid, c.SigningKey, err = signingkeys.CreateForApp(ctx, conn, app.ProjectID, app.ID); err != nil {
		return Credentials{}, err
	}
	if o.WebhookURL != "" {
		if box == nil {
			return Credentials{}, errors.New("apppairings: a webhook needs the secrets key")
		}
		ep, whsec, err := webhooks.CreateEndpoint(ctx, db.New(conn), box, app.ProjectID, app.ID, strings.TrimSpace(o.WebhookURL))
		if err != nil {
			return Credentials{}, fmt.Errorf("create webhook endpoint: %w", err)
		}
		c.WebhookEndpointID, c.WebhookSecret = ep.ID, whsec
	}
	c.Env = c.env()
	return c, nil
}

// env renders the credentials as a .env block. The PEM is one double-quoted
// line with \n escapes, which dotenv, Next.js and Node's --env-file all
// expand back into a multi-line key.
func (c Credentials) env() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# ClipMux credentials for %s (%s)\n", oneLine(c.AppName), c.AppID)
	fmt.Fprintf(&b, "CLIPMUX_URL=%s\n", c.URL)
	fmt.Fprintf(&b, "CLIPMUX_API_KEY=%s\n", c.APIKey)
	fmt.Fprintf(&b, "CLIPMUX_SIGNING_KID=%s\n", c.SigningKid)
	fmt.Fprintf(&b, "CLIPMUX_SIGNING_KEY=\"%s\"\n", strings.ReplaceAll(strings.TrimRight(c.SigningKey, "\n"), "\n", `\n`))
	if c.WebhookSecret != "" {
		fmt.Fprintf(&b, "CLIPMUX_WEBHOOK_SECRET=%s\n", c.WebhookSecret)
	}
	return b.String()
}

// oneLine keeps an app name from breaking out of its comment.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}
