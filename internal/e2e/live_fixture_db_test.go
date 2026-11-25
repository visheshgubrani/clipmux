package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// liveFixture is private retained state for a real staging instance. Never
// expose this struct through /config: it includes the integration's keys.
type liveFixture struct {
	API            string `json:"api"`
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	KID            string `json:"kid"`
	PrivateKey     string `json:"private_key"`
	PlaybackID     string `json:"playback_id"`
	AssetID        string `json:"asset_id"`
	Source         string `json:"source"`
	SubtitlesTrack string `json:"subtitles_track"`
	CueText        string `json:"cue_text"`
}

// serveLiveFixture targets an existing install and never drops its database.
// Only the built browser pages and short-lived test tokens are served, on
// loopback. The Cloudflare token and owner session never enter this server.
func serveLiveFixture(t *testing.T) {
	t.Helper()
	path := os.Getenv("CLIPMUX_E2E_LIVE_CONFIG")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal("live fixture: private config unavailable")
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("live fixture: config must have permissions 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("live fixture: read private config")
	}
	var cfg liveFixture
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal("live fixture: invalid config JSON")
	}
	if cfg.API != "https://app-test.clipmux.com" || cfg.BaseURL != "https://media-test.clipmux.com" || cfg.APIKey == "" || cfg.KID == "" || cfg.PrivateKey == "" || cfg.PlaybackID == "" {
		t.Fatal("live fixture: incomplete staging config or unexpected hostnames")
	}
	if source, err := os.Stat(cfg.Source); err != nil || source.Size() != 70*1024*1024 {
		t.Fatal("live fixture: source unavailable")
	}
	in := &instance{t: t, url: cfg.API}
	mux := http.NewServeMux()
	pages := os.Getenv("CLIPMUX_E2E_PAGES")
	if pages == "" {
		pages = "../../e2e/web/dist"
	}
	mux.Handle("GET /", http.FileServer(http.Dir(pages)))
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"api": cfg.API, "base_url": cfg.BaseURL, "playback_id": cfg.PlaybackID, "source_url": "/source.mp4", "live": true, "subtitles_track": cfg.SubtitlesTrack, "cue_text": cfg.CueText})
	})
	mux.HandleFunc("GET /source.mp4", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, filepath.Clean(cfg.Source)) })
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		ttl, _ := strconv.Atoi(r.URL.Query().Get("ttl"))
		if ttl <= 0 {
			ttl = 3600
		}
		if ttl > 3600 {
			ttl = 3600
		}
		aud := r.URL.Query().Get("aud")
		if aud == "" {
			aud = "v"
		}
		// sign() implements playback audience; thumbnail audience needs the same
		// signer with the requested claim, so use the shared token implementation.
		tok, err := liveToken(cfg, ttl, aud)
		if err != nil {
			http.Error(w, "sign token failed", 500)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, tok)
	})
	mux.HandleFunc("POST /uploads", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		if size != 70*1024*1024 {
			http.Error(w, "invalid fixture size", 400)
			return
		}
		var out map[string]any
		status := in.call("POST", "/v1/uploads", cfg.APIKey, map[string]any{"size": size, "playback_policies": []string{"signed"}}, &out)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /asset", func(w http.ResponseWriter, r *http.Request) {
		// Only status is returned; no integration credentials reach the browser.
		var out struct {
			Status string `json:"status"`
		}
		id := r.URL.Query().Get("id")
		if len(id) < 5 || len(id) > 100 {
			http.Error(w, "invalid asset", 400)
			return
		}
		status := in.call("GET", "/v1/assets/"+id, cfg.APIKey, nil, &out)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	})
	// Reject non-loopback Host values, including browser DNS rebinding.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:8099" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: "127.0.0.1:8099", Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); <-ctx.Done(); _ = srv.Close() }()
	t.Log("live browser fixture listening on loopback; credentials withheld")
	err = srv.ListenAndServe()
	stop()
	<-done
	if err != nil && err != http.ErrServerClosed {
		t.Fatal("live fixture listener failed")
	}
}

func liveToken(cfg liveFixture, ttl int, aud string) (string, error) {
	key, err := tokens.ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	return tokens.Sign(key, cfg.KID, tokens.Claims{Sub: cfg.PlaybackID, Aud: aud, Iat: now, Exp: now + int64(ttl)})
}
