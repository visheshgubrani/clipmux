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
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// TestPlayerFixture serves the browser exit suites (e2e/web): the real
// serve with an encoded 30 s, three-rung video and an owner account,
// backed by a fake RunPod and a fake private registry; and on
// another origin the built test pages plus:
//
//	GET  /config                 {base_url, playback_id, api, dashboard, setup_token,
//	                              ops, ops_email, ops_password, totp, runpod_key, registry_*,
//	                              s3?}
//	POST /verify-runpod          marks the connected RunPod verified, as a passed
//	                             setup test on a pod would
//	GET  /token?aud=v&ttl=SECS   a fresh playback token (text)
//	POST /uploads?size=BYTES     an upload created with the app's API key
//	POST /session?ttl=SECS&n=KEY&deny_after=CALLS
//	                             a session-url answer for {playback_id, native}, as
//	                             POST /s/{secret}/playback answers; 403 after
//	                             deny_after calls with the same n
//	POST /create-upload?deny=1   a createUrl answer for {size, name, type}: the
//	                             upload plus endpoint; 403 with deny
//
// It runs until interrupted. Playwright starts it as its web server.
func TestPlayerFixture(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_FIXTURE") != "1" {
		t.Skip("set CLIPMUX_E2E_FIXTURE=1 to serve the browser suites")
	}
	if os.Getenv("CLIPMUX_E2E_LIVE_CONFIG") != "" {
		serveLiveFixture(t)
		return
	}
	// The Workers suite connects RunPod: a fake RunPod API and a fake
	// private registry, which serve trusts through SSL_CERT_FILE.
	rp := runpodtest.New(t)
	reg := registrytest.New(t)
	certFile := filepath.Join(t.TempDir(), "registry.pem")
	if err := os.WriteFile(certFile, reg.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	in := start(t, "CLIPMUX_RUNPOD_API="+rp.URL, "CLIPMUX_RUNPOD_CATALOG="+rp.Catalog, "CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL=true",
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
	// The TOTP drill (P6-T04) turns on the owner's second factor, so it gets
	// an install of its own, whose owner exists already.
	totp := start(t)
	totp.owner()
	// The Delivery suite adds, rotates and removes a storage config: a bucket
	// of its own on the S3 test store, when there is one.
	var s3 map[string]string
	if os.Getenv("CLIPMUX_TEST_S3_ENDPOINT") != "" {
		b := s3test.New(t)
		s3 = map[string]string{"endpoint": b.Endpoint, "bucket": b.Name, "access_key_id": b.AccessKeyID, "secret_access_key": b.SecretAccessKey}
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
	files := http.FileServer(http.Dir(pages))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// ?csp=app serves the page under the Content-Security-Policy the SDK
		// READMEs tell an app to send: its own origin, plus ClipMux's URL,
		// storage and media origins (all in.url here).
		if r.URL.Query().Get("csp") == "app" {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"connect-src 'self' "+in.url+"; media-src blob: "+in.url+"; img-src 'self' "+in.url+"; object-src 'none'; base-uri 'none'")
		}
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base_url": in.url, "playback_id": pid, "api": in.url,
			"asset_id": asset, "app_id": appID, "subtitles_track": trks["subtitles"].ID, "chapters_track": trks["chapters"].ID,
			"cue_text": cueText, "cue_start": cueStart, "llm_url": ai.llm.URL + "/v1", "llm_key": llmKey,
			"speech_url": ai.speech.URL + "/v1", "speech_key": speechKey,
			"dashboard": fresh.url, "setup_token": setupToken[1], "totp": totp.url,
			"ops": in.url, "ops_email": "owner@example.test", "ops_password": "a long test password",
			"runpod_key": runpodtest.Key, "registry_username": registrytest.Username, "registry_password": registrytest.Password,
			"s3": s3,
		})
	})
	// The fake RunPod never runs a pod, so its setup test never passes. The
	// Workers suite stands in for a sample that encoded on one: RunPod is
	// verified, and the owner can choose it (D116).
	mux.HandleFunc("POST /verify-runpod", func(w http.ResponseWriter, r *http.Request) {
		if _, err := in.pool.Exec(r.Context(), `UPDATE executor_configs SET verified_at = now() WHERE kind = 'runpod'`); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	// What @clipmux/node's handler answers the browser packages with.
	var sessionsMu sync.Mutex
	sessions := map[string]int{}
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			PlaybackID string `json:"playback_id"`
			Native     *bool  `json:"native"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Native == nil {
			http.Error(w, "want {playback_id, native}", http.StatusBadRequest)
			return
		}
		if req.PlaybackID != pid {
			http.Error(w, "no such playback", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		sessionsMu.Lock()
		sessions[q.Get("n")]++
		calls := sessions[q.Get("n")]
		sessionsMu.Unlock()
		if after, err := strconv.Atoi(q.Get("deny_after")); err == nil && calls > after {
			http.Error(w, `{"error":{"code":"forbidden","message":"denied"}}`, http.StatusForbidden)
			return
		}
		ttl, _ := strconv.Atoi(q.Get("ttl"))
		if ttl <= 0 {
			ttl = 3600
		}
		now := time.Now()
		exp := now.Add(time.Duration(ttl) * time.Second)
		tok, err := tokens.Sign(signer, kid, tokens.Claims{Sub: pid, Aud: tokens.AudiencePlayback, Iat: now.Unix(), Exp: exp.Unix()})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// The poster's own token, as @clipmux/node's session() signs it.
		posterTok, err := tokens.Sign(signer, kid, tokens.Claims{Sub: pid, Aud: tokens.AudienceThumbnail, Iat: now.Unix(), Exp: exp.Unix()})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"playback_id": pid, "base_url": in.url, "token": tok, "expires_at": exp.UTC().Format(time.RFC3339),
			"poster_url": in.url + "/v/" + posterTok + "/" + pid + "/poster.jpg",
			"tracks":     []trackView{trks["subtitles"], trks["chapters"]},
		})
	})
	mux.HandleFunc("POST /create-upload", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Size int64  `json:"size"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Size <= 0 || req.Name == "" {
			http.Error(w, "want {size, name, type}", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("deny") == "1" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"you may not upload here"}}`))
			return
		}
		var created map[string]any
		status := in.call("POST", "/v1/uploads", key, map[string]any{"size": req.Size, "playback_policy": "signed", "title": req.Name}, &created)
		if status == http.StatusCreated {
			created["endpoint"] = in.url
		}
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
