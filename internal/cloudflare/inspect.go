package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
)

// Getting a token (D109): the dashboard opens Cloudflare's create-token form
// already holding the permissions a flow needs, the owner creates the token
// and pastes it back, and Inspect shows what it reaches so nothing is typed
// by hand. Real OAuth needs a ClipMux-owned public client and a hosted
// callback (docs/known-gaps.md).

// permission is one entry of a token template's permissionGroupKeys: a
// short key from Cloudflare's permission reference and an access type.
type permission struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

// Each flow asks for what its calls use and nothing more.
var (
	// Connect: the Worker and its custom domain (Workers Scripts; Workers
	// Routes on the zone), the bucket (R2), Workers Paid (Billing), and the
	// media hostname's zone and records.
	connectPermissions = []permission{
		{"workers_scripts", "edit"}, {"workers_routes", "edit"}, {"workers_r2", "edit"},
		{"billing", "read"}, {"zone", "read"}, {"dns", "edit"},
	}
	r2Permissions        = []permission{{"workers_r2", "edit"}}
	analyticsPermissions = []permission{{"account_analytics", "read"}}
)

// tokenURL is an account-owned token template link: the owner picks the
// account, Cloudflare names any key it couldn't apply, and the form is
// pre-filled with the name and permissions.
func tokenURL(name string, perms []permission) string {
	keys, _ := json.Marshal(perms) // a fixed slice of strings; it can't fail
	q := url.Values{"to": {"/:account/api-tokens"}, "permissionGroupKeys": {string(keys)}, "name": {name}}
	return "https://dash.cloudflare.com/?" + q.Encode()
}

// ConnectTokenURL opens Cloudflare's form for a Connect Cloudflare token.
func ConnectTokenURL() string { return tokenURL("ClipMux", connectPermissions) }

// R2TokenURL opens the form for a token that only manages R2 storage.
func R2TokenURL() string { return tokenURL("ClipMux storage", r2Permissions) }

// AnalyticsTokenURL opens the form for the read-only edge analytics token.
func AnalyticsTokenURL() string { return tokenURL("ClipMux analytics", analyticsPermissions) }

// ErrTokenRejected means Cloudflare doesn't accept the token.
var ErrTokenRejected = errors.New("cloudflare: token rejected")

// Inspection is what a token reaches. With several accounts and none
// chosen, only Accounts is filled: the owner picks one and inspects again.
type Inspection struct {
	Accounts []Account `json:"accounts"`
	// AccountID is the account the rest describes.
	AccountID   string   `json:"account_id,omitempty"`
	Zones       []string `json:"zones,omitempty"`
	Buckets     []string `json:"buckets,omitempty"`
	WorkersPaid bool     `json:"workers_paid"`
	// Unreadable names what the token can't list (a storage-only token has
	// no zones or billing); the flow that needs it says so when it runs.
	Unreadable []string `json:"unreadable,omitempty"`
}

// Inspect reads what token reaches, changing nothing. The token is used for
// this call only.
func (c *Connector) Inspect(ctx context.Context, token, accountID string) (Inspection, error) {
	if token == "" {
		return Inspection{}, fmt.Errorf("%w: a token is required", ErrInvalid)
	}
	cf := c.client(token)
	var in Inspection
	accounts, err := cf.Accounts(ctx)
	if err != nil {
		return in, rejected(err)
	}
	in.Accounts = accounts
	if accountID == "" && len(accounts) > 1 {
		return in, nil
	}
	var steps []Step
	_, in.AccountID, _ = credential(ctx, cf, accountID, stepper(&steps))
	if last := steps[len(steps)-1]; !last.OK {
		return in, fmt.Errorf("%w: %s", ErrTokenRejected, last.Detail)
	}
	if in.Zones, err = cf.ActiveZones(ctx, in.AccountID); err != nil {
		in.Unreadable = append(in.Unreadable, "zones")
	}
	if in.Buckets, err = cf.Buckets(ctx, in.AccountID); err != nil {
		in.Unreadable = append(in.Unreadable, "buckets")
	}
	if in.WorkersPaid, err = cf.WorkersPaid(ctx, in.AccountID); err != nil {
		in.Unreadable = append(in.Unreadable, "billing")
	}
	slices.Sort(in.Zones)
	slices.Sort(in.Buckets)
	return in, ctx.Err()
}

// rejected turns Cloudflare refusing the token into ErrTokenRejected; other
// failures (the network, a 5xx) stay as they are.
func rejected(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
		return fmt.Errorf("%w: %s", ErrTokenRejected, apiErr.Error())
	}
	return err
}
