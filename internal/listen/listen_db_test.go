package listen_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/listen"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

type received struct {
	id, eventType string
	verified      bool
}

func TestListenRelaysTheAppsEventsSignedAsWebhooks(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'lms'), ('app_site', 'prj_default', 'site')`)
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_mine', 'prj_default', 'app_lms', 'processing', 'sto_default'), ('ast_theirs', 'prj_default', 'app_site', 'processing', 'sto_default')`)
	key, _, err := apikeys.Create(ctx, pool, "prj_default", "app_lms", "dev", []string{apikeys.AssetsRead})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	assets.New(pool).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	secret := []byte("whsec_test")
	var mu sync.Mutex
	var got []received
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get("X-Webhook-Timestamp"), 10, 64)
		mu.Lock()
		got = append(got, received{
			id: r.Header.Get("X-Webhook-Id"), eventType: r.Header.Get("X-Webhook-Event"),
			verified: r.Header.Get("X-Webhook-Signature") == webhooks.Sign(secret, ts, body),
		})
		mu.Unlock()
	}))
	defer local.Close()

	event := func(asset, kind string) {
		t.Helper()
		if err := outbox.AssetEvent(ctx, db.New(pool), asset, kind, nil); err != nil {
			t.Fatal(err)
		}
	}
	event("ast_mine", outbox.AssetErrored) // before the relay started, but within --since
	ctx, cancel := context.WithCancel(ctx)
	r := &listen.Relay{Server: srv.URL, Key: key, ForwardTo: local.URL, Secret: secret, Types: []string{outbox.AssetErrored, outbox.AssetReady}}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, time.Now().Add(-time.Minute)) }()
	event("ast_theirs", outbox.AssetReady) // another app's: never relayed
	event("ast_mine", outbox.AssetDeleted) // filtered out by type
	event("ast_mine", outbox.AssetReady)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond) // a further poll must not repeat anything
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].eventType != outbox.AssetErrored || got[1].eventType != outbox.AssetReady {
		t.Fatalf("relayed %+v, want this app's errored then ready, once each", got)
	}
	for _, g := range got {
		if !g.verified {
			t.Fatalf("event %s doesn't verify with the listen secret", g.id)
		}
	}
}

func TestListenNeedsAKeyThatCanReadAssets(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	if _, err := pool.Exec(ctx, `INSERT INTO apps (id, project_id, name) VALUES ('app_lms', 'prj_default', 'lms')`); err != nil {
		t.Fatal(err)
	}
	key, _, _ := apikeys.Create(ctx, pool, "prj_default", "app_lms", "uploads only", []string{apikeys.UploadsWrite})
	mux := http.NewServeMux()
	assets.New(pool).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r := &listen.Relay{Server: srv.URL, Key: key, ForwardTo: "http://127.0.0.1:1"}
	err := r.Run(ctx, time.Now())
	if e, ok := err.(*listen.Error); !ok || e.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want 403", err)
	}
}
