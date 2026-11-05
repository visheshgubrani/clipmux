package assets_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// An app that missed webhooks for longer than their 24 h retry window
// catches up with GET /v1/assets?updated_since= (docs/architecture.md,
// "Webhooks"): every change since a time, oldest first, deleted assets
// included, paged with an opaque cursor.
func TestCatchUpListsChangesSinceATimeInOrderWithACursor(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (id, project_id, name) VALUES ('app_other', 'prj_default', 'other');
		INSERT INTO assets (id, project_id, app_id, status, storage_id, updated_at) VALUES
		  ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', '2026-11-05T10:00:00Z'),
		  ('ast_4', 'prj_default', 'app_dashboard', 'processing', 'sto_default', '2026-11-05T10:01:00Z'),
		  ('ast_2', 'prj_default', 'app_dashboard', 'processing', 'sto_default', '2026-11-05T10:01:00Z'),
		  ('ast_3', 'prj_default', 'app_dashboard', 'deleted', 'sto_default', '2026-11-05T10:02:00.123456Z'),
		  ('ast_9', 'prj_default', 'app_other', 'processing', 'sto_default', '2026-11-05T10:05:00Z');`); err != nil {
		t.Fatal(err)
	}
	key, _, err := apikeys.Create(ctx, pool, "prj_default", "app_dashboard", "backend", []string{apikeys.AssetsRead})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	assets.New(pool, "https://media.example").Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	page := func(query url.Values) (int, []string, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/v1/assets?"+query.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body struct {
			Data []struct {
				ID        string `json:"id"`
				UpdatedAt string `json:"updated_at"`
			} `json:"data"`
			NextCursor string `json:"next_cursor"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		var got []string
		for _, a := range body.Data {
			got = append(got, a.ID+"@"+a.UpdatedAt)
		}
		return res.StatusCode, got, body.NextCursor
	}

	status, got, next := page(url.Values{"updated_since": {"2026-11-05T10:01:00Z"}, "limit": {"2"}})
	if want := []string{"ast_2@2026-11-05T10:01:00Z", "ast_4@2026-11-05T10:01:00Z"}; status != 200 || !reflect.DeepEqual(got, want) || next == "" {
		t.Fatalf("first page = %d %v next %q, want %v and a cursor", status, got, next, want)
	}
	status, got, next = page(url.Values{"updated_since": {"2026-11-05T10:01:00Z"}, "limit": {"2"}, "cursor": {next}})
	if want := []string{"ast_3@2026-11-05T10:02:00.123456Z"}; status != 200 || !reflect.DeepEqual(got, want) || next != "" {
		t.Fatalf("second page = %d %v next %q, want %v and no cursor", status, got, next, want)
	}
	if status, got, _ := page(url.Values{"updated_since": {"2026-11-05T09:00:00+00:00"}}); status != 200 || len(got) != 4 {
		t.Fatalf("since 09:00 = %d %v, want the app's 4 assets", status, got)
	}
	for _, bad := range []url.Values{
		{"updated_since": {"yesterday"}},
		{"updated_since": {"2026-11-05T10:01:00Z"}, "cursor": {"not-a-cursor"}},
		{"updated_since": {"2026-11-05T10:01:00Z"}, "limit": {"101"}},
		{"updated_since": {"2026-11-05T10:01:00Z"}, "passthrough": {"x"}},
		{},
	} {
		if status, _, _ := page(bad); status != http.StatusBadRequest {
			t.Errorf("GET /v1/assets?%s = %d, want 400", bad.Encode(), status)
		}
	}
}
