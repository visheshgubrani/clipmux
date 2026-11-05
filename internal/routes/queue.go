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

// QueueAppRefresh refreshes every live route of an app, after its disabled
// state changes: a disabled app's IDs are unpublished, an enabled app's
// republished.
func QueueAppRefresh(ctx context.Context, q *db.Queries, projectID, appID string) error {
	pids, err := q.ListActivePlaybackIDsForApp(ctx, appID)
	if err != nil {
		return err
	}
	for _, p := range pids {
		payload, _ := json.Marshal(map[string]any{"v": 1, "op": OpRefresh, "playback_id": p.ID, "app_id": appID, "asset_id": p.AssetID})
		if err := q.InsertOutbox(ctx, db.InsertOutboxParams{
			ID: ids.New(ids.Event), ProjectID: projectID, Kind: "route_sync",
			AppID: pgtype.Text{String: appID, Valid: true}, AssetID: pgtype.Text{String: p.AssetID, Valid: true},
			Payload: payload,
		}); err != nil {
			return err
		}
	}
	return nil
}
