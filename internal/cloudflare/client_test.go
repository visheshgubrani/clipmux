package cloudflare

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyAccountToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/staging-account/tokens/verify" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected verification request")
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"account-token-id","status":"active"}}`))
	}))
	defer server.Close()
	client := Client{Base: server.URL, Token: "test-token"}
	id, err := client.VerifyAccountToken(context.Background(), "staging-account")
	if err != nil || id != "account-token-id" {
		t.Fatalf("id = %q, error = %v", id, err)
	}
}

func TestDomainMatchesRequiresTheSameWorkerAndZone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/account1/workers/domains" || r.URL.Query().Get("hostname") != "media.example.com" {
			t.Error("unexpected domains lookup")
		}
		_, _ = w.Write([]byte(`{"success":true,"result":[{"hostname":"media.example.com","service":"clipmux-media","zone_id":"zone1"}]}`))
	}))
	defer server.Close()
	client := Client{Base: server.URL}
	for _, test := range []struct {
		zone, script string
		want         bool
	}{{"zone1", "clipmux-media", true}, {"other", "clipmux-media", false}, {"zone1", "other-worker", false}} {
		got, err := client.DomainMatches(context.Background(), "account1", test.zone, "media.example.com", test.script)
		if err != nil || got != test.want {
			t.Fatalf("domain match %q/%q: %v, %v", test.zone, test.script, got, err)
		}
	}
}
