package manage_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/backup"
	"github.com/visheshgubrani/clipmux/internal/config"
)

func TestTheBackupsRoutesShowStatusTakeRequestsAndRecordTheRecoveryKit(t *testing.T) {
	r := newRig(t, config.Config{})
	key, _, _ := apikeys.Create(t.Context(), r.pool, "prj_default", "app_dashboard", "backend", apikeys.AllScopes())
	for _, c := range [][2]string{{"GET", "/v1/settings/backups"}, {"POST", "/v1/settings/backups/run"}, {"PUT", "/v1/settings/backups/recovery-kit"}} {
		if got := r.call(c[0], c[1], map[string]bool{"confirmed": true}, key); got.status != http.StatusUnauthorized {
			t.Errorf("%s %s with an API key = %d, want 401", c[0], c[1], got.status)
		}
	}

	// Backups off: say so, and don't queue a request nothing will answer.
	r.m.BackupConfig = backup.Config{Local: true}
	got := r.call("GET", "/v1/settings/backups", nil, "")
	if got.status != 200 || got.body["enabled"] != false || got.body["off_site"] != false || got.body["last"] != nil || got.body["fence"] != nil {
		t.Fatalf("status with backups off = %d %v", got.status, got.body)
	}
	if got := r.call("POST", "/v1/settings/backups/run", nil, ""); got.status != http.StatusConflict || code(got) != "backups_off" {
		t.Errorf("run with backups off = %d %v, want 409 backups_off", got.status, got.body)
	}

	// Backups on, one taken and checked, a source waiting, the fence up.
	cfg := backup.Config{URL: "file://" + t.TempDir(), Interval: time.Hour}
	target, err := backup.OpenTarget(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	r.m.Backups = backup.New(r.pool, r.box, target, cfg, "", "v-test")
	r.m.BackupConfig = cfg
	r.exec(`INSERT INTO backups (id, project_id, kind, object_key, size_bytes, sha256, clipmux_version, schema_version, postgres_version, secrets_key_version, created_at, verified_at, verify_mode, verify_error)
		VALUES ('bak_1', 'prj_default', 'scheduled', 'db/hourly/a', 4096, 'x', 'v', 1, '18', 1, '2026-11-02T10:00:00Z', '2026-11-02T11:00:00Z', 'integrity', 'a table is short')`)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'uploads/ast_1/source')`)
	r.exec(`INSERT INTO restore_fence (project_id, reason, detail) VALUES ('prj_default', 'restored', 'restored from bak_1')`)
	r.exec(`UPDATE projects SET backup_error = 'the target refused the write', backup_error_at = '2026-11-02T12:00:00Z' WHERE id = 'prj_default'`)

	got = r.call("GET", "/v1/settings/backups", nil, "")
	last, _ := got.body["last"].(map[string]any)
	ver, _ := got.body["verified"].(map[string]any)
	errv, _ := got.body["error"].(map[string]any)
	fence, _ := got.body["fence"].(map[string]any)
	if got.status != 200 || got.body["enabled"] != true || got.body["off_site"] != true || got.body["interval_seconds"] != float64(3600) ||
		got.body["target"] != target.Desc || got.body["immutable"] != nil || got.body["sources_pending"] != float64(1) || got.body["requested"] != false {
		t.Fatalf("status = %d %v", got.status, got.body)
	}
	if last["id"] != "bak_1" || last["kind"] != "scheduled" || last["size_bytes"] != float64(4096) || last["created_at"] != "2026-11-02T10:00:00Z" {
		t.Errorf("last = %v", last)
	}
	if ver["backup_id"] != "bak_1" || ver["mode"] != "integrity" || ver["error"] != "a table is short" {
		t.Errorf("verified = %v", ver)
	}
	if errv["message"] != "the target refused the write" || fence["reason"] != "restored" || fence["detail"] != "restored from bak_1" {
		t.Errorf("error = %v, fence = %v", errv, fence)
	}

	// "Back up now" waits for the leader's next pass, and says so until one is taken.
	if got := r.call("POST", "/v1/settings/backups/run", nil, ""); got.status != http.StatusAccepted || got.body["requested"] != true {
		t.Fatalf("run = %d %v", got.status, got.body)
	}
	if got := r.call("GET", "/v1/settings/backups", nil, ""); got.body["requested"] != true {
		t.Errorf("requested = %v after asking", got.body["requested"])
	}

	// The owner's word that the kit is kept elsewhere is kept, and can be withdrawn.
	got = r.call("PUT", "/v1/settings/backups/recovery-kit", map[string]bool{"confirmed": true}, "")
	if got.status != 200 || got.body["recovery_kit_confirmed_at"] == nil {
		t.Fatalf("confirm = %d %v", got.status, got.body)
	}
	if got := r.call("GET", "/v1/settings/backups", nil, ""); got.body["recovery_kit_confirmed_at"] == nil {
		t.Error("the confirmation isn't in the status")
	}
	got = r.call("PUT", "/v1/settings/backups/recovery-kit", map[string]bool{"confirmed": false}, "")
	if got.status != 200 || got.body["recovery_kit_confirmed_at"] != nil {
		t.Errorf("withdraw = %d %v", got.status, got.body)
	}
}
