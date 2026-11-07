package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// TestPlayerFixture serves the browser exit suites (e2e/web): the real
// serve with an encoded 30 s, three-rung video, an owner account and a
// connected PC, backed by a fake RunPod and a fake private registry; and on
// another origin the built test pages plus:
//
//	GET  /config                 {base_url, playback_id, api, dashboard, setup_token,
//	                              ops, ops_email, ops_password, runpod_key, registry_*}
//	GET  /token?aud=v&ttl=SECS   a fresh playback token (text)
//	POST /uploads?size=BYTES     an upload created with the app's API key
//
// It runs until interrupted. Playwright starts it as its web server.
func TestPlayerFixture(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_FIXTURE") != "1" {
		t.Skip("set CLIPMUX_E2E_FIXTURE=1 to serve the browser suites")
	}
	// The Workers suite connects RunPod: a fake RunPod API and a fake
	// private registry, which serve trusts through SSL_CERT_FILE.
	rp := runpodtest.New(t)
	reg := registrytest.New(t)
	certFile := filepath.Join(t.TempDir(), "registry.pem")
	if err := os.WriteFile(certFile, reg.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	in := start(t, "CLIPMUX_RUNPOD_API="+rp.URL, "CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL=true",
		"SSL_CERT_FILE="+certFile, "CLIPMUX_WORKER_IMAGE="+reg.Image())
	key, kid, priv := in.credentials("Browser suites")
	// The owner connects a fake speech API and a fake LLM, so the video gets
	// subtitles and chapters (P5-T18) without whisper on this machine.
	ownerClient := in.owner()
	ai := newAIFakes(t)
	in.connectSpeech(ownerClient, ai, "api")
	in.connectLLM(ownerClient, ai)
	asset, pid := in.uploadWith(key, fixture(t, 30, 1280, 720), map[string]any{"generate": map[string]bool{"chapters": true}})
	in.waitReady(key, asset, 5*time.Minute)
	trks := in.waitTracks(key, asset, 2*time.Minute)
	if trks["subtitles"].Status != "ready" || trks["chapters"].Status != "ready" {
		t.Fatalf("tracks = %+v", trks)
	}
	var appID string
	if err := in.pool.QueryRow(context.Background(), `SELECT app_id FROM assets WHERE id = $1`, asset).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	// A second, fresh install for the dashboard's first-boot suite: its URLs
	// aren't pinned, so the owner chooses them in the wizard.
	fresh := startUnpinned(t)
	setupToken := setupTokenLine.FindStringSubmatch(fresh.log())
	if setupToken == nil {
		t.Fatalf("the fresh install printed no setup token:\n%s", fresh.log())
	}
	// The Workers suite signs in as the owner and finds a connected PC, one
	// that can transcribe.
	if _, err := in.pool.Exec(context.Background(), `INSERT INTO agents (id, project_id, name, key_hash, status, sandboxed, hostname, platform, capabilities, last_seen_at)
		VALUES ('agt_studio', 'prj_default', 'studio-pc', 'fixture', 'approved', true, 'studio', 'linux/amd64', '{"encoders": ["h264_vaapi", "libx264"], "speech": {"backends": ["vulkan", "cpu"], "model": "large-v3-turbo-q8_0"}}', now() + interval '1 day')`); err != nil {
		t.Fatal(err)
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base_url": in.url, "playback_id": pid, "api": in.url,
			"asset_id": asset, "app_id": appID, "subtitles_track": trks["subtitles"].ID, "chapters_track": trks["chapters"].ID,
			"cue_text": cueText, "cue_start": cueStart, "llm_url": ai.llm.URL + "/v1", "llm_key": llmKey,
			"dashboard": fresh.url, "setup_token": setupToken[1],
			"ops": in.url, "ops_email": "owner@example.test", "ops_password": "a long test password",
			"runpod_key": runpodtest.Key, "registry_username": registrytest.Username, "registry_password": registrytest.Password,
		})
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
