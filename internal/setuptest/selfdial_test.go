package setuptest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
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
