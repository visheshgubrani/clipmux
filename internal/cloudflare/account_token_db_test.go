package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/settings"
)

func TestConnectVerifiesAccountOwnedTokenBeforeCheckingPaid(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	urls, err := settings.Load(ctx, pool, db.DefaultProjectID, config.Config{PublicURL: "https://app.example.com", MediaURL: "https://media.example.com", URLsPinned: true})
	if err != nil {
		t.Fatal(err)
	}
	verified := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any
		switch r.URL.Path {
		case "/user/tokens/verify":
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"success":false}`))
			return
		case "/accounts":
			result = []Account{{ID: "account1", Name: "Staging"}}
		case "/accounts/account1/tokens/verify":
			verified = true
			result = map[string]string{"id": "account-token", "status": "active"}
		case "/zones":
			result = []map[string]any{{"id": "zone1", "name": "example.com", "status": "active"}}
		case "/accounts/account1/subscriptions":
			result = []any{}
		default:
			t.Errorf("unexpected Cloudflare path %s", r.URL.Path)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}))
	defer api.Close()
	connector := Connector{Pool: pool, URLs: urls, API: api.URL}
	result, err := connector.Connect(ctx, Request{Token: "account-token-value", Hostname: "media.example.com"})
	if err != nil || !verified || result.Connected || len(result.Steps) != 4 || !result.Steps[0].OK || result.Steps[3].Name != "workers_paid" || result.Steps[3].OK {
		t.Fatalf("connect result = %+v, error = %v", result, err)
	}
}
