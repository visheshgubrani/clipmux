// Package delivery is the Delivery adapter boundary and, from P1-T08, the
// built-in delivery handler (docs/architecture.md, "Delivery"). The built-in
// handler and the Cloudflare Worker (Phase 2) share URLs, tokens, key
// derivation and lookup rules, held together by shared JSON test vectors.
package delivery

import "context"

// Route is the record that makes a playback ID servable. It is a projection
// of Postgres, written through the route_sync outbox.
type Route struct {
	V          int    `json:"v"`
	AppID      string `json:"app_id"`
	AssetID    string `json:"asset_id"`
	OutputID   string `json:"output_id"`
	Policy     string `json:"policy"`  // "signed" or "public"
	Backend    string `json:"backend"` // "r2" or "origin"
	PlaybackID string `json:"-"`
}

// Caps describes a delivery adapter.
type Caps struct {
	Name      string
	EdgeCache bool
}

// Delivery is the narrow interface every delivery backend implements.
type Delivery interface {
	BaseURL() string
	// Publish makes a playback ID servable. Idempotent.
	Publish(ctx context.Context, r Route) error
	// Unpublish revokes a playback ID. Idempotent.
	Unpublish(ctx context.Context, playbackID string) error
	Capabilities() Caps
}
