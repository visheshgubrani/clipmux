// Package outbox writes side effects as rows in the same transaction as the
// state change that causes them (docs/architecture.md, "Webhooks"), so a
// crash can't lose one. Delivery of webhook rows lands in P1-T09 and of
// route_sync rows in P1-T07.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

// AssetEvent queues a webhook event about asset for its app. version is the
// asset version the event describes; extra adds fields to data.
func AssetEvent(ctx context.Context, q *db.Queries, asset db.Asset, eventType string, version int64, extra map[string]any) error {
	id := ids.New(ids.Event)
	data := map[string]any{"asset_id": asset.ID, "version": version}
	if asset.Passthrough.Valid {
		data["passthrough"] = asset.Passthrough.String
	}
	for k, v := range extra {
		data[k] = v
	}
	payload, err := json.Marshal(map[string]any{
		"id": id, "type": eventType, "created_at": time.Now().UTC().Format(time.RFC3339), "data": data,
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
