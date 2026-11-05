package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/storage/s3test"
)

// The Phase 2 exit test (docs/plans/P2-cloudflare-delivery.md) on the real
// binary: Connect Cloudflare against a fake Cloudflare API, with R2 played
// by the test S3 store and the Worker's request logic in front of it
// (worker/e2e/harness.ts). It proves, in order:
//
//  1. app code is unchanged on the Worker: the same key, signing key and
//     playback ID play, and a new upload lands in R2 and plays from it;
//  2. a revoked ID stops at the edge within a minute (the route cache);
//  3. the stored segment is ciphertext;
//  4. an old filesystem asset plays through origin, and after its move
//     plays from R2 with serve stopped.

// cfToken derives, in deploy/seaweedfs-dev.json, to an S3 identity.
const (
	cfToken   = "cf-test-token-value"
	cfTokenID = "cftesttokenid"
)

// cfFake records the Worker upload, whose bindings the harness needs.
type cfFake struct {
	mu       sync.Mutex
	bindings map[string]string
}

func (f *cfFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ok := func(v any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": v})
	}
	p := r.URL.Path
	switch {
	case p == "/user/tokens/verify":
		ok(map[string]string{"id": cfTokenID, "status": "active"})
	case p == "/accounts":
		ok([]map[string]string{{"id": "acct1", "name": "Exit test"}})
	case p == "/zones":
		if r.URL.Query().Get("name") == "example.test" {
			ok([]map[string]string{{"id": "zone1", "name": "example.test", "status": "active"}})
		} else {
			ok([]any{})
		}
	case strings.HasSuffix(p, "/subscriptions"):
		ok([]map[string]any{{"rate_plan": map[string]string{"id": "workers_paid", "public_name": "Workers Paid"}, "state": "Paid"}})
	case strings.Contains(p, "/r2/buckets/"):
		ok(map[string]any{}) // the test made the bucket
	case strings.Contains(p, "/workers/scripts/"):
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FormName() != "metadata" {
				continue
			}
			var meta struct {
				Bindings []struct {
					Type       string `json:"type"`
					Name       string `json:"name"`
					Text       string `json:"text"`
					BucketName string `json:"bucket_name"`
				} `json:"bindings"`
			}
			_ = json.NewDecoder(part).Decode(&meta)
			f.mu.Lock()
			f.bindings = map[string]string{}
			for _, b := range meta.Bindings {
				f.bindings[b.Name] = b.Text + b.BucketName
			}
			f.mu.Unlock()
		}
		ok(map[string]any{})
	case strings.HasSuffix(p, "/dns_records"):
		ok([]any{})
	default:
		ok(map[string]any{})
	}
}

// owner signs in as the owner with the setup token serve printed.
func (in *instance) owner() *http.Client {
	in.t.Helper()
	m := regexp.MustCompile(`setup_token=(\S+)`).FindStringSubmatch(in.log())
	if m == nil {
		in.t.Fatalf("no setup token in the logs")
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"setup_token": m[1], "email": "owner@example.test", "password": "a long test password"})
	res, err := c.Post(in.url+"/v1/setup/owner", "application/json", bytes.NewReader(body))
	if err != nil || res.StatusCode != http.StatusCreated {
		in.t.Fatalf("create owner: %v %v", err, res)
	}
	res.Body.Close()
	return c
}

func ownerCall(t *testing.T, c *http.Client, method, url string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	// Same-origin, as the dashboard's requests are.
	req.Header.Set("Origin", strings.SplitN(url, "/v1/", 2)[0])
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(raw, out)
	}
	return res.StatusCode
}

// harness runs the Worker logic in front of a bucket and returns its URL.
func harness(t *testing.T, b s3test.Bucket, bindings map[string]string) string {
	t.Helper()
	port := freePort(t)
	sum := sha256.Sum256([]byte(cfToken))
	cmd := exec.Command("node", filepath.Join("..", "..", "worker", "e2e", "harness.ts"))
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HARNESS_PORT=%d", port), "S3_ENDPOINT="+b.Endpoint, "S3_BUCKET="+b.Name,
		"S3_ACCESS_KEY="+cfTokenID, "S3_SECRET_KEY="+hex.EncodeToString(sum[:]),
		"CONTENT_KEYS="+bindings["CONTENT_KEYS"], "ORIGIN_URL="+bindings["ORIGIN_URL"])
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Skipf("node is needed for the Worker harness: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan struct{})
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			if strings.HasPrefix(s.Text(), "listening") {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(20 * time.Second):
		t.Fatal("the Worker harness never started")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func status(t *testing.T, url string) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res.StatusCode
}

func (in *instance) scalar(sql string, args ...any) string {
	in.t.Helper()
	var v string
	if err := in.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		in.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func TestPhase2ExitAppsPlayUnchangedThroughTheWorker(t *testing.T) {
	b := s3test.New(t) // skips without the S3 test store, unless CI requires it
	fake := &cfFake{}
	api := httptest.NewServer(fake)
	defer api.Close()
	in := start(t,
		"CLIPMUX_MEDIA_URL=https://media.example.test",
		"CLIPMUX_CLOUDFLARE_API="+api.URL,
		"CLIPMUX_R2_ENDPOINT="+b.Endpoint,
	)
	ctx := context.Background()

	// Before: an app on built-in delivery, with a playing asset.
	key, kid, priv := in.credentials("LMS")
	oldAsset, oldPID := in.upload(key, fixture(t, 8, 854, 480))
	in.waitReady(key, oldAsset, 3*time.Minute)
	if v, _ := play(t, in.url+"/v/"+sign(t, kid, priv, oldPID, time.Hour)+"/"+oldPID+"/master.m3u8"); v < 190 {
		t.Fatalf("built-in playback decoded %d frames", v)
	}

	// Connect Cloudflare.
	owner := in.owner()
	var res struct {
		Connected bool `json:"connected"`
		Steps     []struct {
			Name, Detail string
			OK           bool
		} `json:"steps"`
	}
	if s := ownerCall(t, owner, http.MethodPost, in.url+"/v1/cloudflare/connect", map[string]string{
		"token": cfToken, "hostname": "media.example.test", "bucket": b.Name,
	}, &res); s != http.StatusOK || !res.Connected {
		t.Fatalf("connect: %d %+v", s, res.Steps)
	}
	fake.mu.Lock()
	bindings := fake.bindings
	fake.mu.Unlock()
	if bindings["ORIGIN_URL"] != in.url || bindings["MEDIA"] != b.Name {
		t.Fatalf("worker bindings = %v", bindings)
	}
	edge := harness(t, b, bindings)

	// 1. App code unchanged: the same token for the same ID plays at the edge
	// (the old asset, through origin). base_url didn't change either.
	if v, _ := play(t, edge+"/v/"+sign(t, kid, priv, oldPID, time.Hour)+"/"+oldPID+"/master.m3u8"); v < 190 {
		t.Fatalf("the old asset through the Worker decoded %d frames", v)
	}
	// A new upload, with the same API key, goes to R2 and plays from it.
	newAsset, newPID := in.upload(key, fixture(t, 8, 854, 480))
	in.waitReady(key, newAsset, 3*time.Minute)
	if kind := in.scalar(`SELECT s.kind FROM assets a JOIN storage_configs s ON s.id = a.storage_id WHERE a.id = $1`, newAsset); kind != "r2" {
		t.Fatalf("the new upload is on %s storage, want r2", kind)
	}
	newURL := edge + "/v/" + sign(t, kid, priv, newPID, time.Hour) + "/" + newPID + "/master.m3u8"
	if v, a := play(t, newURL); v < 190 || a == 0 {
		t.Fatalf("the new asset from R2 decoded %d video and %d audio frames", v, a)
	}

	// 3. The stored segment is ciphertext, not a plain fMP4 fragment.
	r2, err := storage.OpenS3(storage.S3Options{Endpoint: b.Endpoint, Bucket: b.Name, AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey})
	if err != nil {
		t.Fatal(err)
	}
	objs, _, err := r2.List(ctx, "media/"+newAsset+"/", "")
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".m4s") {
			continue
		}
		rc, err := r2.Get(ctx, o.Key)
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, 8)
		_, _ = io.ReadFull(rc, head)
		rc.Close()
		if box := string(head[4:8]); box == "moof" || box == "styp" || box == "sidx" {
			t.Fatalf("%s starts with a %q box: not encrypted", o.Key, box)
		}
		checked = true
		break
	}
	if !checked {
		t.Fatal("no segment in R2")
	}

	// 2. Revoke the new asset's ID: the Worker stops serving it within its
	// 60 s route cache.
	if s := in.call(http.MethodDelete, "/v1/assets/"+newAsset+"/playback-ids/"+newPID, key, nil, nil); s != http.StatusOK && s != http.StatusAccepted {
		t.Fatalf("revoke: %d", s)
	}
	revoked := time.Now()
	for status(t, newURL) != http.StatusNotFound {
		if time.Since(revoked) > 65*time.Second {
			t.Fatal("the revoked ID still played at the edge after 65 s")
		}
		time.Sleep(time.Second)
	}
	t.Logf("revocation reached the edge in %s", time.Since(revoked).Round(time.Second))

	// 4. Move the library to R2; the old asset then plays without serve.
	var moved struct{ Queued int }
	if s := ownerCall(t, owner, http.MethodPost, in.url+"/v1/storage/moves", nil, &moved); s != http.StatusAccepted || moved.Queued < 1 { // also the first-boot sample
		t.Fatalf("move library: %d, queued %d", s, moved.Queued)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		route, err := r2.Get(ctx, "_routes/"+oldPID+".json")
		if err == nil {
			body, _ := io.ReadAll(route)
			route.Close()
			if bytes.Contains(body, []byte(`"backend":"r2"`)) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the old asset's route never moved to r2")
		}
		time.Sleep(time.Second)
	}
	_ = in.cmd.Process.Signal(os.Interrupt)
	_ = in.cmd.Wait()
	// A fresh isolate (the edge's caches are per isolate), serve stopped.
	edge = harness(t, b, bindings)
	if v, _ := play(t, edge+"/v/"+sign(t, kid, priv, oldPID, time.Hour)+"/"+oldPID+"/master.m3u8"); v < 190 {
		t.Fatalf("the moved asset with serve stopped decoded %d frames", v)
	}
}
