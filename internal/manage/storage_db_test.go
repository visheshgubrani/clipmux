package manage_test

import (
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

// D099: storage configs, from adding a bucket (checked first) to removing
// it once nothing needs it.
func TestStorageConfigsFromAddToRemove(t *testing.T) {
	b := s3test.New(t)
	r := newRig(t, config.Config{})
	listed := func() map[string]map[string]any {
		t.Helper()
		got := r.call("GET", "/v1/storage", nil, "")
		if got.status != 200 {
			t.Fatalf("list = %d %v", got.status, got.body)
		}
		out := map[string]map[string]any{}
		for _, c := range got.list {
			m := c.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out
	}
	if got := listed(); len(got) != 1 || got["sto_default"]["kind"] != "fs" || got["sto_default"]["default"] != true || got["sto_default"]["asset_count"] != 0.0 {
		t.Fatalf("a fresh install's storage = %v, want the filesystem default", got)
	}

	good := map[string]any{"endpoint": b.Endpoint + "/", "bucket": b.Name, "access_key_id": b.AccessKeyID, "secret_access_key": b.SecretAccessKey}
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for key, val := range good {
			out[key] = val
		}
		out[k] = v
		return out
	}
	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{with("endpoint", "ftp://example.com"), "invalid_request"},
		{with("endpoint", "https://user:pass@example.com"), "invalid_request"},
		{with("bucket", "a/b"), "invalid_request"},
		{with("secret_access_key", ""), "invalid_request"},
		{with("secret_access_key", "wrong-"+b.SecretAccessKey), "storage_check_failed"},
	} {
		if got := r.call("POST", "/v1/storage", c.body, ""); got.status != 400 || code(got) != c.code {
			t.Errorf("add %v = %d %v, want 400 %s", c.body, got.status, got.body, c.code)
		}
	}
	if got := listed(); len(got) != 1 {
		t.Fatalf("refused adds saved %v", got)
	}
	added := r.call("POST", "/v1/storage", good, "")
	id, _ := added.body["id"].(string)
	if added.status != 201 || added.body["kind"] != "s3" || added.body["bucket"] != b.Name || added.body["endpoint"] != b.Endpoint || added.body["default"] != false {
		t.Fatalf("add = %d %v", added.status, added.body)
	}
	if raw := r.mustJSON(listed()); strings.Contains(raw, b.SecretAccessKey) || strings.Contains(raw, b.AccessKeyID) {
		t.Fatalf("the list shows credentials: %s", raw)
	}

	// Credentials: checked first, the old ones kept on a failure.
	if got := r.call("PUT", "/v1/storage/"+id+"/credentials", map[string]string{"access_key_id": b.AccessKeyID, "secret_access_key": "wrong"}, ""); got.status != 400 || code(got) != "storage_check_failed" {
		t.Fatalf("bad new credentials = %d %v, want 400 storage_check_failed", got.status, got.body)
	}
	if got := r.call("PUT", "/v1/storage/"+id+"/credentials", map[string]string{"access_key_id": b.AccessKeyID, "secret_access_key": b.SecretAccessKey}, ""); got.status != 204 {
		t.Fatalf("rotate = %d %v, want 204", got.status, got.body)
	}
	if got := r.call("PUT", "/v1/storage/sto_default/credentials", map[string]string{"access_key_id": "a", "secret_access_key": "b"}, ""); got.status != 409 || code(got) != "builtin_storage" {
		t.Fatalf("filesystem credentials = %d %v, want 409 builtin_storage", got.status, got.body)
	}
	if got := r.call("PUT", "/v1/storage/sto_none/credentials", map[string]string{"access_key_id": "a", "secret_access_key": "b"}, ""); got.status != 404 {
		t.Fatalf("unknown storage = %d, want 404", got.status)
	}

	// The default can't go; neither can the filesystem.
	if got := r.call("POST", "/v1/storage/"+id+"/default", nil, ""); got.status != 200 || len(got.list) != 2 {
		t.Fatalf("make default = %d %v", got.status, got.body)
	}
	if got := listed(); got[id]["default"] != true || got["sto_default"]["default"] != false {
		t.Fatalf("after make default = %v", got)
	}
	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 409 || code(got) != "storage_default" {
		t.Fatalf("remove the default = %d %v, want 409 storage_default", got.status, got.body)
	}
	r.call("POST", "/v1/storage/sto_default/default", nil, "")
	if got := r.call("DELETE", "/v1/storage/sto_default", nil, ""); got.status != 409 || code(got) != "builtin_storage" {
		t.Fatalf("remove the filesystem = %d %v, want 409 builtin_storage", got.status, got.body)
	}

	// What still needs it: an asset's files, a move, Cloudflare delivery.
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', $1)`, id)
	if got := listed(); got[id]["asset_count"] != 1.0 {
		t.Fatalf("asset_count = %v, want 1", got[id]["asset_count"])
	}
	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 409 || code(got) != "storage_in_use" || got.body["error"].(map[string]any)["asset_count"] != "1" {
		t.Fatalf("remove with an asset = %d %v, want 409 storage_in_use", got.status, got.body)
	}
	r.exec(`UPDATE assets SET status = 'deleted' WHERE id = 'ast_1'`)
	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 409 || code(got) != "storage_in_use" {
		t.Fatalf("remove while a deleted asset's files remain = %d %v, want 409", got.status, got.body)
	}
	r.exec(`UPDATE assets SET deleted_at = now() WHERE id = 'ast_1'`)
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_2', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	r.exec(`INSERT INTO jobs (id, project_id, asset_id, kind, source_storage_id, target_storage_id) VALUES ('job_m', 'prj_default', 'ast_2', 'move', 'sto_default', $1)`, id)
	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 409 || got.body["error"].(map[string]any)["active_moves"] != "1" {
		t.Fatalf("remove during a move = %d %v, want 409 with active_moves 1", got.status, got.body)
	}
	r.exec(`UPDATE jobs SET status = 'cancelled', finished_at = now() WHERE id = 'job_m'`)
	r.exec(`UPDATE projects SET delivery = 'worker', delivery_storage_id = $1`, id)
	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 409 || code(got) != "cloudflare_delivery" {
		t.Fatalf("remove Cloudflare's bucket = %d %v, want 409 cloudflare_delivery", got.status, got.body)
	}
	r.exec(`UPDATE projects SET delivery = 'builtin', delivery_storage_id = NULL`)

	if got := r.call("DELETE", "/v1/storage/"+id, nil, ""); got.status != 204 {
		t.Fatalf("remove = %d %v, want 204", got.status, got.body)
	}
	if got := listed(); len(got) != 1 {
		t.Fatalf("after remove = %v, want only the filesystem", got)
	}
	var sealed int
	_ = r.pool.QueryRow(t.Context(), `SELECT count(*) FROM storage_configs WHERE id = $1 AND removed_at IS NOT NULL AND secrets_encrypted IS NULL`, id).Scan(&sealed)
	if sealed != 1 {
		t.Fatal("a removed config keeps its row (history names it) but not its credentials")
	}
	for _, c := range [][2]string{{"DELETE", "/v1/storage/" + id}, {"POST", "/v1/storage/" + id + "/default"}, {"PUT", "/v1/storage/" + id + "/credentials"}} {
		if got := r.call(c[0], c[1], map[string]string{"access_key_id": b.AccessKeyID, "secret_access_key": b.SecretAccessKey}, ""); got.status != 404 {
			t.Errorf("%s %s after removal = %d %v, want 404", c[0], c[1], got.status, got.body)
		}
	}
}
