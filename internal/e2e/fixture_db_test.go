package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// TestPlayerFixture serves the browser exit suites (e2e/web): the real
// serve with an encoded 30 s, three-rung video, and on another origin the
// built test pages plus:
//
//	GET  /config                 {base_url, playback_id, api}
//	GET  /token?aud=v&ttl=SECS   a fresh playback token (text)
//	POST /uploads?size=BYTES     an upload created with the app's API key
//
// It runs until interrupted. Playwright starts it as its web server.
func TestPlayerFixture(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_FIXTURE") != "1" {
		t.Skip("set CLIPMUX_E2E_FIXTURE=1 to serve the browser suites")
	}
	in := start(t)
	key, kid, priv := in.credentials("Browser suites")
	asset, pid := in.upload(key, fixture(t, 30, 1280, 720))
	in.waitReady(key, asset, 5*time.Minute)
	signer, err := tokens.ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	pages := os.Getenv("CLIPMUX_E2E_PAGES")
	if pages == "" {
		pages = "../../e2e/web/dist"
	}
	mux.Handle("GET /", http.FileServer(http.Dir(pages)))
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"base_url": in.url, "playback_id": pid, "api": in.url})
	})
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		ttl, _ := strconv.Atoi(r.URL.Query().Get("ttl"))
		if ttl <= 0 {
			ttl = 3600
		}
		aud := r.URL.Query().Get("aud")
		if aud == "" {
			aud = tokens.AudiencePlayback
		}
		now := time.Now().Unix()
		tok, err := tokens.Sign(signer, kid, tokens.Claims{Sub: pid, Aud: aud, Iat: now, Exp: now + int64(ttl)})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(tok))
	})
	mux.HandleFunc("POST /uploads", func(w http.ResponseWriter, r *http.Request) {
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		var created map[string]any
		status := in.call("POST", "/v1/uploads", key, map[string]any{"size": size, "playback_policy": "signed"}, &created)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(created)
	})
	port := os.Getenv("CLIPMUX_FIXTURE_PORT")
	if port == "" {
		port = "8099"
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Printf("fixture ready: pages http://127.0.0.1:%s, media %s, playback ID %s\n", port, in.url, pid)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
