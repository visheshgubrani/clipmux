package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This opt-in test intentionally retains the live staging install and data.
// Browser tests must run first: rotation changes the content key version.
func TestCloudflareLiveAcceptance(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_LIVE_ACCEPTANCE") != "1" {
		t.Skip("live acceptance is opt-in")
	}
	path := os.Getenv("CLIPMUX_E2E_LIVE_CONFIG")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("private live config unavailable")
	}
	var cfg liveFixture
	if json.Unmarshal(raw, &cfg) != nil || cfg.API != "https://app-test.clipmux.com" || cfg.BaseURL != "https://media-test.clipmux.com" {
		t.Fatal("unexpected live staging config")
	}
	state := filepath.Dir(path)
	database := os.Getenv("CLIPMUX_E2E_LIVE_DATABASE_URL")
	if database != "postgres://clipmux:clipmux@127.0.0.1:5445/clipmux?sslmode=disable" {
		t.Fatal("dedicated staging database required")
	}
	pool, err := pgxpool.New(t.Context(), database)
	if err != nil {
		t.Fatal("staging database unavailable")
	}
	defer pool.Close()
	in := &instance{t: t, url: cfg.API, pool: pool}
	raw, err = os.ReadFile(filepath.Join(state, "owner.json"))
	if err != nil {
		t.Fatal("owner unavailable")
	}
	var ownerBody map[string]string
	if json.Unmarshal(raw, &ownerBody) != nil {
		t.Fatal("invalid private owner config")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := &http.Client{Jar: jar, Timeout: 3 * time.Minute}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/session", ownerBody, nil); s != 200 {
		t.Fatalf("owner login HTTP %d", s)
	}
	token, err := os.ReadFile(filepath.Join(state, "cloudflare-token"))
	if err != nil {
		t.Fatal("Cloudflare token unavailable")
	}
	media := func(pid string) string {
		return cfg.BaseURL + "/v/" + sign(t, cfg.KID, cfg.PrivateKey, pid, time.Hour) + "/" + pid + "/"
	}
	get := func(url string) (int, []byte) {
		req, err := http.NewRequestWithContext(t.Context(), "GET", url, nil)
		if err != nil {
			t.Fatal("invalid delivery request")
		}
		res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("delivery transport failed; URL withheld")
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		if err != nil {
			t.Fatal("delivery body failed")
		}
		return res.StatusCode, body
	}
	waitStatus := func(url string, want int) time.Duration {
		begin := time.Now()
		for {
			status, _ := get(url)
			if status == want {
				return time.Since(begin)
			}
			if time.Since(begin) > 65*time.Second {
				t.Fatalf("delivery HTTP %d after 65 seconds, want %d", status, want)
			}
			time.Sleep(time.Second)
		}
	}
	if kind := in.scalar(`SELECT s.kind FROM assets a JOIN storage_configs s ON s.id=a.storage_id WHERE a.id=$1`, cfg.AssetID); kind != "fs" {
		t.Fatal("baseline must still use filesystem storage")
	}
	if v, a := play(t, media(cfg.PlaybackID)+"master.m3u8"); v < 700 || a == 0 {
		t.Fatal("origin fallback did not decode baseline audio/video")
	}
	t.Log("existing filesystem asset plays through Worker origin fallback")
	// Only the speech provider is a deterministic fake. Storage, projections,
	// Cloudflare API calls, Worker execution and delivery are all real.
	ai := newAIFakes(t)
	in.connectSpeech(owner, ai, "api")

	if s := in.call("POST", "/v1/assets/"+cfg.AssetID+"/tracks", cfg.APIKey, map[string]string{"kind": "subtitles"}, nil); s != 202 {
		t.Fatalf("create captions HTTP %d", s)
	}
	trks := in.waitTracks(cfg.APIKey, cfg.AssetID, 3*time.Minute)
	track := trks["subtitles"]
	if track.Status != "ready" {
		t.Fatal("captions were not ready")
	}
	captionPath := "tracks/" + track.ID + ".vtt"
	waitStatus(media(cfg.PlaybackID)+captionPath, http.StatusOK)
	if status, body := get(media(cfg.PlaybackID) + captionPath); status != 200 || !strings.Contains(string(body), cueText) {
		t.Fatal("origin captions failed")
	}
	t.Log("captions generated through deterministic speech fixture and delivered through Worker")
	if s := ownerCall(t, owner, "DELETE", cfg.API+"/v1/settings/speech/connect", nil, nil); s != 200 {
		t.Fatalf("disconnect speech fixture HTTP %d", s)
	}
	// Warm an ID and its track before revocation so cached bytes cannot bypass it.
	var pid struct {
		ID string `json:"id"`
	}
	if s := in.call("POST", "/v1/assets/"+cfg.AssetID+"/playback-ids", cfg.APIKey, map[string]string{"policy": "signed"}, &pid); s != 201 {
		t.Fatalf("create playback ID HTTP %d", s)
	}
	revokedURL := media(pid.ID) + captionPath
	waitStatus(revokedURL, 200)
	if s := in.call("DELETE", "/v1/assets/"+cfg.AssetID+"/playback-ids/"+pid.ID, cfg.APIKey, nil, nil); s != 200 && s != 202 {
		t.Fatalf("revoke playback ID HTTP %d", s)
	}
	t.Logf("playback ID and cached track denied after %s", waitStatus(revokedURL, 404).Round(time.Second))
	var app struct {
		AppID string `json:"app_id"`
	}
	in.call("GET", "/v1/assets/"+cfg.AssetID, cfg.APIKey, nil, &app)
	var signing struct {
		KID     string `json:"kid"`
		Private string `json:"private_key_pem"`
	}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/apps/"+app.AppID+"/signing-keys", nil, &signing); s != 201 {
		t.Fatalf("create signing key HTTP %d", s)
	}
	keyURL := cfg.BaseURL + "/v/" + sign(t, signing.KID, signing.Private, cfg.PlaybackID, time.Hour) + "/" + cfg.PlaybackID + "/master.m3u8"
	waitStatus(keyURL, 200)
	if s := ownerCall(t, owner, "DELETE", cfg.API+"/v1/signing-keys/"+signing.KID, nil, nil); s != http.StatusNoContent {
		t.Fatalf("revoke signing key HTTP %d", s)
	}
	t.Logf("signing key denied after %s", waitStatus(keyURL, 403).Round(time.Second))
	var rotated struct {
		Usable  bool  `json:"usable"`
		Current int32 `json:"current"`
	}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/content-keys/rotate", map[string]string{"cloudflare_token": strings.TrimSpace(string(token))}, &rotated); s != 200 || !rotated.Usable {
		t.Fatalf("content key rotation HTTP %d", s)
	}
	if v, _ := play(t, media(cfg.PlaybackID)+"master.m3u8"); v < 700 {
		t.Fatal("old output stopped after rotation")
	}
	newAsset, newPID := in.upload(cfg.APIKey, fixture(t, 8, 854, 480))
	in.waitReady(cfg.APIKey, newAsset, 3*time.Minute)
	if kind := in.scalar(`SELECT s.kind FROM assets a JOIN storage_configs s ON s.id=a.storage_id WHERE a.id=$1`, newAsset); kind != "r2" {
		t.Fatal("new upload did not use R2")
	}
	var keyVersion int32
	if err := pool.QueryRow(t.Context(), `SELECT o.key_version FROM assets a JOIN outputs o ON o.id=a.current_output_id WHERE a.id=$1`, newAsset).Scan(&keyVersion); err != nil {
		t.Fatal(err)
	}
	if keyVersion != rotated.Current {
		t.Fatal("new output did not use rotated key version")
	}
	// Resolve a variant and segment as a player does, then exercise HEAD and
	// Range on an R2 object that has not yet been fetched by playback.
	masterURL := media(newPID) + "master.m3u8"
	_, masterBody := get(masterURL)
	var variant string
	for _, line := range strings.Split(string(masterBody), "\n") {
		if !strings.HasPrefix(line, "#") && strings.HasSuffix(line, ".m3u8") {
			variant = line
			break
		}
	}
	if variant == "" {
		t.Fatal("master has no variant")
	}
	root, err := url.Parse(masterURL)
	if err != nil {
		t.Fatal("invalid master URL")
	}
	rel, err := url.Parse(variant)
	if err != nil {
		t.Fatal("invalid variant URL")
	}
	playlistURL := root.ResolveReference(rel)
	_, playlistBody := get(playlistURL.String())
	var segment string
	for _, line := range strings.Split(string(playlistBody), "\n") {
		if !strings.HasPrefix(line, "#") && strings.HasSuffix(line, ".m4s") {
			segment = line
			break
		}
	}
	if segment == "" {
		t.Fatal("variant has no segment")
	}
	rel, err = url.Parse(segment)
	if err != nil {
		t.Fatal("invalid segment URL")
	}
	segmentURL := playlistURL.ResolveReference(rel).String()
	var objectSize int
	for _, method := range []string{"HEAD", "GET"} {
		req, err := http.NewRequestWithContext(t.Context(), method, segmentURL, nil)
		if err != nil {
			t.Fatal("invalid media request")
		}
		if method == "GET" {
			req.Header.Set("Range", "bytes=0-7")
		}
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal("media request failed; URL withheld")
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal("media read failed")
		}
		if response.Header.Get("Access-Control-Allow-Origin") != "*" || response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("media CORS or nosniff missing")
		}
		if method == "HEAD" {
			objectSize, _ = strconv.Atoi(response.Header.Get("Content-Length"))
			if response.StatusCode != 200 || len(body) != 0 || objectSize <= 8 {
				t.Fatal("invalid HEAD response")
			}
		} else {
			if response.StatusCode != 206 || len(body) != 8 || response.Header.Get("Content-Range") != "bytes 0-7/"+strconv.Itoa(objectSize) {
				t.Fatal("invalid byte range response")
			}
			if strings.Contains("styp moof sidx", string(body[4:8])) {
				t.Fatal("segment is plain MP4 instead of ciphertext")
			}
		}
	}
	t.Log("real R2 HEAD, byte range, CORS and encrypted segment checks passed")
	if v, a := play(t, media(newPID)+"master.m3u8"); v < 190 || a == 0 {
		t.Fatal("new R2 output did not decode")
	}
	t.Log("content key rotation preserves old output and decrypts new R2 output")
	// Re-encode the filesystem baseline before moving it. The new immutable
	// output is never fetched before shutdown, proving cold R2 delivery.
	oldOutput := in.scalar(`SELECT current_output_id FROM assets WHERE id=$1`, cfg.AssetID)
	if s := in.call("POST", "/v1/assets/"+cfg.AssetID+"/reencode", cfg.APIKey, map[string]bool{}, nil); s != 202 {
		t.Fatalf("reencode HTTP %d", s)
	}
	deadline := time.Now().Add(4 * time.Minute)
	for in.scalar(`SELECT current_output_id FROM assets WHERE id=$1`, cfg.AssetID) == oldOutput {
		if time.Now().After(deadline) {
			t.Fatal("reencode timed out")
		}
		time.Sleep(time.Second)
	}
	for {
		var active bool
		if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM jobs WHERE asset_id=$1 AND status IN ('queued','running'))`, cfg.AssetID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reencode publication timed out")
		}
		time.Sleep(time.Second)
	}
	if s := ownerCall(t, owner, "POST", cfg.API+"/v1/storage/moves", nil, nil); s != 202 {
		t.Fatalf("move library HTTP %d", s)
	}
	deadline = time.Now().Add(4 * time.Minute)
	for in.scalar(`SELECT s.kind FROM assets a JOIN storage_configs s ON s.id=a.storage_id WHERE a.id=$1`, cfg.AssetID) != "r2" {
		if time.Now().After(deadline) {
			t.Fatal("move timed out")
		}
		time.Sleep(time.Second)
	}
	// Wait past the shipped route cache before stopping the control plane.
	time.Sleep(65 * time.Second)
	stop := exec.CommandContext(context.Background(), "python3", "../../deploy/cloudflare-staging.py", "stop")
	if err := stop.Run(); err != nil {
		t.Fatal("stop staging app failed")
	}
	if v, a := play(t, media(cfg.PlaybackID)+"master.m3u8"); v < 700 || a == 0 {
		t.Fatal("cold moved output failed with serve stopped")
	}
	if status, body := get(media(cfg.PlaybackID) + captionPath); status != 200 || !strings.Contains(string(body), cueText) {
		t.Fatal("moved captions failed with serve stopped")
	}
	t.Log("fresh moved output and captions play from real R2 with serve stopped")
}
