// Package outbox writes side effects as rows in the same transaction as the
// state change that causes them (docs/architecture.md, "Webhooks"), so a
// crash can't lose one. internal/webhooks delivers the webhook rows and
// internal/outputs publishes the route_sync rows.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

// Asset event types.
const (
	AssetReady          = "video.asset.ready"
	AssetUpdated        = "video.asset.updated"
	AssetErrored        = "video.asset.errored"
	AssetReencodeFailed = "video.asset.reencode_failed"
	AssetDeleted        = "video.asset.deleted"
)

// Job names the job behind an event and why it ended.
type Job struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	ErrorCode string `json:"error_code,omitempty"`
}

// event is a webhook body. Data is the asset exactly as GET
// /v1/assets/{id} returns it, at the version the event describes.
type event struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	CreatedAt string      `json:"created_at"`
	Data      assets.View `json:"data"`
	Job       *Job        `json:"job,omitempty"`
}

// AssetEvent queues a webhook event about an asset for its app. It reads the
// asset inside the caller's transaction, so call it after the state change
// the event reports. job is set for events a job caused.
func AssetEvent(ctx context.Context, q *db.Queries, assetID, eventType string, job *Job) error {
	asset, err := q.GetAssetByID(ctx, assetID)
	if err != nil {
		return err
	}
	view, err := assets.Load(ctx, q, asset)
	if err != nil {
		return err
	}
	id := ids.New(ids.Event)
	payload, err := json.Marshal(event{
		ID: id, Type: eventType, CreatedAt: time.Now().UTC().Format(time.RFC3339), Data: view, Job: job,
	})
	if err != nil {
		return err
	}
	return q.InsertOutbox(ctx, db.InsertOutboxParams{
		ID: id, ProjectID: asset.ProjectID, Kind: "webhook",
		AppID:   pgtype.Text{String: asset.AppID, Valid: true},
		AssetID: pgtype.Text{String: asset.ID, Valid: true},
		Payload: payload,
	})
}
