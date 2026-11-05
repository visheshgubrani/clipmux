package routes

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
)

// OpRefresh republishes a route from current state without the side effects
// a new output or a revocation has (no events, no state change). Disabling
// or enabling an app refreshes each of its routes.
const OpRefresh = "refresh"

// QueueKeys asks for _keys.json to be rewritten. Call it in the transaction
// that creates, retires or revokes a signing key or changes the project's
// maximum token lifetime.
func QueueKeys(ctx context.Context, q *db.Queries, projectID string) error {
	return q.InsertOutbox(ctx, db.InsertOutboxParams{
		ID: ids.New(ids.Event), ProjectID: projectID, Kind: "keys_sync", Payload: []byte("{}"),
	})
}

// QueueAssetRefresh refreshes every live route of one asset, after
// something every record of it carries changes, such as its store.
func QueueAssetRefresh(ctx context.Context, q *db.Queries, projectID, appID, assetID string) error {
	pids, err := q.ListPlaybackIDs(ctx, assetID)
	if err != nil {
		return err
	}
	for _, p := range pids {
		if p.Status != "active" {
			continue
		}
		if err := queueRefresh(ctx, q, projectID, appID, assetID, p.ID); err != nil {
			return err
		}
	}
	return nil
}

func queueRefresh(ctx context.Context, q *db.Queries, projectID, appID, assetID, playbackID string) error {
	payload, _ := json.Marshal(map[string]any{"v": 1, "op": OpRefresh, "playback_id": playbackID, "app_id": appID, "asset_id": assetID})
	return q.InsertOutbox(ctx, db.InsertOutboxParams{
		ID: ids.New(ids.Event), ProjectID: projectID, Kind: "route_sync",
		AppID: pgtype.Text{String: appID, Valid: true}, AssetID: pgtype.Text{String: assetID, Valid: true},
		Payload: payload,
	})
}

// QueueAppRefresh refreshes every live route of an app after its disabled
// state changes, so the Worker denies (or again serves) its IDs.
func QueueAppRefresh(ctx context.Context, q *db.Queries, projectID, appID string) error {
	pids, err := q.ListActivePlaybackIDsForApp(ctx, appID)
	if err != nil {
		return err
	}
	for _, p := range pids {
		if err := queueRefresh(ctx, q, projectID, appID, p.AssetID, p.ID); err != nil {
			return err
		}
	}
	return nil
}
