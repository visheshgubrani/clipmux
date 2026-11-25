package setuptest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/settings"
)

// Inside compose the app can't resolve its own public hostname; the setup
// test dials Caddy instead and keeps the hostname for Host (and SNI).
func TestSelfDialKeepsTheURLsHostname(t *testing.T) {
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host = r.Host }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	r := New(nil, nil, nil, nil, nil, nil, "127.0.0.1")
	if status, _, err := r.get(context.Background(), "http://video.example.invalid:"+port+"/healthz"); err != nil || status != 200 || host != "video.example.invalid:"+port {
		t.Fatalf("bare host: status %d, %v, Host %q", status, err, host)
	}
	r = New(nil, nil, nil, nil, nil, nil, "127.0.0.1:"+port)
	if status, _, err := r.get(context.Background(), "http://video.example.invalid/healthz"); err != nil || status != 200 || host != "video.example.invalid" {
		t.Fatalf("host:port: status %d, %v, Host %q", status, err, host)
	}
}

func TestSelfDialSkipsExternalHostsAndWorkerMediaURL(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	var selfHits, extHits int
	selfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { selfHits++ }))
	defer selfSrv.Close()
	extSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { extHits++ }))
	defer extSrv.Close()

	_, selfPort, _ := net.SplitHostPort(selfSrv.Listener.Addr().String())
	_, extPort, _ := net.SplitHostPort(extSrv.Listener.Addr().String())

	urls, err := settings.Load(ctx, pool, "prj_default", config.Config{
		PublicURL: "http://app.example.invalid",
		MediaURL:  "http://media.example.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := urls.Set(ctx, "http://app.example.invalid", "http://media.example.invalid"); err != nil {
		t.Fatal(err)
	}

	r := New(pool, nil, nil, urls, nil, nil, "127.0.0.1:"+selfPort)

	// Built-in delivery: both public_url and media_url go through selfDial; unrelated hosts do not.
	if status, _, err := r.get(ctx, "http://app.example.invalid/healthz"); err != nil || status != 200 || selfHits != 1 {
		t.Fatalf("public_url under builtin: status=%d err=%v selfHits=%d", status, err, selfHits)
	}
	if status, _, err := r.get(ctx, "http://media.example.invalid/healthz"); err != nil || status != 200 || selfHits != 2 {
		t.Fatalf("media_url under builtin: status=%d err=%v selfHits=%d", status, err, selfHits)
	}
	if status, _, err := r.get(ctx, "http://127.0.0.1:"+extPort+"/bucket/part"); err != nil || status != 200 || extHits != 1 || selfHits != 2 {
		t.Fatalf("external host under builtin: status=%d err=%v extHits=%d selfHits=%d", status, err, extHits, selfHits)
	}

	// Worker delivery: public_url still self-dials, media_url dials directly without selfDial.
	if _, err := pool.Exec(ctx, `UPDATE projects SET delivery = 'worker', delivery_storage_id = 'sto_default' WHERE id = 'prj_default'`); err != nil {
		t.Fatal(err)
	}
	if status, _, err := r.get(ctx, "http://app.example.invalid/healthz"); err != nil || status != 200 || selfHits != 3 {
		t.Fatalf("public_url under worker: status=%d err=%v selfHits=%d", status, err, selfHits)
	}
	if shouldSelfDial(ctx, pool, urls, "media.example.invalid:443") {
		t.Fatal("media_url under worker delivery must not be rewritten to selfDial")
	}
	if !shouldSelfDial(ctx, pool, urls, "app.example.invalid:443") {
		t.Fatal("public_url under worker delivery must still use selfDial")
	}
}
