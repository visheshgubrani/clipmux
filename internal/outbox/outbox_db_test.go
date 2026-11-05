package outbox_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outbox"
)

// An event carries the asset exactly as GET /v1/assets/{id} returns it, at
// the version the event describes, so a receiver can ignore anything older
// than what it stored. Job-caused events also say which job and why.
func TestEventsCarryTheFullAssetAtTheirVersion(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	q := db.New(pool)
	if _, err := pool.Exec(ctx, `
		INSERT INTO assets (id, project_id, app_id, status, error_code, version, storage_id, passthrough, created_at, updated_at)
		VALUES ('ast_1', 'prj_default', 'app_dashboard', 'errored', 'upload_cancelled', 2, 'sto_default', 'lesson-42',
		        '2026-11-05T10:00:00Z', '2026-11-05T10:05:00Z');
		INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ('pid_1', 'prj_default', 'ast_1', 'signed');
		UPDATE projects SET media_url = 'https://media.example';`); err != nil {
		t.Fatal(err)
	}

	if err := outbox.AssetEvent(ctx, q, "ast_1", outbox.AssetErrored, nil); err != nil {
		t.Fatal(err)
	}
	if err := outbox.AssetEvent(ctx, q, "ast_1", outbox.AssetReencodeFailed,
		&outbox.Job{ID: "job_1", Kind: "reencode", ErrorCode: "transcode_failed"}); err != nil {
		t.Fatal(err)
	}

	rows, err := pool.Query(ctx, `SELECT id, app_id, asset_id, payload FROM outbox WHERE kind = 'webhook' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []map[string]any
	for rows.Next() {
		var id, app, asset string
		var raw []byte
		if err := rows.Scan(&id, &app, &asset, &raw); err != nil {
			t.Fatal(err)
		}
		var ev map[string]any
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		if ev["id"] != id || app != "app_dashboard" || asset != "ast_1" {
			t.Fatalf("row %s (app %s, asset %s) holds event %v", id, app, asset, ev["id"])
		}
		delete(ev, "id")
		if _, ok := ev["created_at"].(string); !ok {
			t.Fatalf("event has no created_at: %v", ev)
		}
		delete(ev, "created_at")
		events = append(events, ev)
	}
	asset := map[string]any{
		"id": "ast_1", "status": "errored", "version": float64(2), "passthrough": "lesson-42",
		"error_code": "upload_cancelled", "base_url": "https://media.example",
		"created_at": "2026-11-05T10:00:00Z", "updated_at": "2026-11-05T10:05:00Z",
		"playback_ids": []any{map[string]any{"id": "pid_1", "policy": "signed", "playable": false, "status": "active"}},
	}
	want := []map[string]any{
		{"type": "video.asset.errored", "data": asset},
		{"type": "video.asset.reencode_failed", "data": asset,
			"job": map[string]any{"id": "job_1", "kind": "reencode", "error_code": "transcode_failed"}},
	}
	if !reflect.DeepEqual(events, want) {
		got, _ := json.MarshalIndent(events, "", "  ")
		t.Fatalf("events =\n%s", got)
	}
}
