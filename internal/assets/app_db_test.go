package assets_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// GET /v1/app (P6-T23) tells any valid key about its own app: what the
// SDK's doctor checks and what it fills in (base_url, the token cap).
func TestAppDescribesTheKeysOwnApp(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, `
		UPDATE projects SET public_url = 'https://video.example.test', media_url = 'https://media.example.test',
		  max_token_lifetime_seconds = 86400;
		INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'LMS'), ('app_other', 'prj_default', 'Other');
		INSERT INTO signing_keys (id, project_id, app_id, public_key_pem, created_at, retired_at, revoked_at) VALUES
		  ('kid_new', 'prj_default', 'app_lms', 'PEM-NEW', '2026-11-12T10:00:00Z', NULL, NULL),
		  ('kid_retiring', 'prj_default', 'app_lms', 'PEM-OLD', '2026-11-01T10:00:00Z', '2099-01-01T00:00:00Z', NULL),
		  ('kid_retired', 'prj_default', 'app_lms', 'PEM-GONE', '2026-10-01T10:00:00Z', '2026-10-02T00:00:00Z', NULL),
		  ('kid_revoked', 'prj_default', 'app_lms', 'PEM-REVOKED', '2026-11-02T10:00:00Z', NULL, now()),
		  ('kid_theirs', 'prj_default', 'app_other', 'PEM-THEIRS', '2026-11-03T10:00:00Z', NULL, NULL);`); err != nil {
		t.Fatal(err)
	}
	// A key with one scope still reads its app: there is no scope for it.
	key, _, err := apikeys.Create(ctx, pool, "prj_default", "app_lms", "backend", []string{apikeys.UploadsWrite})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := assets.New(pool)
	api.UploadOrigins = func(context.Context, string) ([]string, error) {
		return []string{"https://acct.r2.cloudflarestorage.com"}, nil
	}
	api.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(bearer string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/v1/app", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}

	status, got := get(key)
	want := map[string]any{
		"id": "app_lms", "name": "LMS", "scopes": []any{"uploads:write"},
		"public_url": "https://video.example.test", "base_url": "https://media.example.test",
		"max_token_lifetime_seconds": float64(86400),
		// Where a browser sends upload parts: the server (filesystem
		// storage, /_storage) and the buckets' public endpoints. With
		// base_url, these are what an app's CSP must allow.
		"upload_origins": []any{"https://video.example.test", "https://acct.r2.cloudflarestorage.com"},
		"signing_keys": []any{
			map[string]any{"kid": "kid_new", "public_key_pem": "PEM-NEW", "created_at": "2026-11-12T10:00:00Z"},
			map[string]any{"kid": "kid_retiring", "public_key_pem": "PEM-OLD", "created_at": "2026-11-01T10:00:00Z", "retired_at": "2099-01-01T00:00:00Z"},
		},
	}
	if status != http.StatusOK || !reflect.DeepEqual(got, want) {
		t.Fatalf("GET /v1/app = %d\n%v\nwant\n%v", status, got, want)
	}
	for _, bad := range []string{"", "cmx_nope"} {
		if status, out := get(bad); status != http.StatusUnauthorized || out["error"].(map[string]any)["code"] != "unauthorized" {
			t.Errorf("GET /v1/app with %q: %d %v", bad, status, out)
		}
	}
}
