package e2e

// The operator drills of P6-T04 (D069–D071, D078), on the real binary:
// retention after backdating, a pg_dump and data-directory restore with the
// same secrets key, rotating the secrets key, the content key secret and a
// signing key, and a transcription routed to RunPod because the primary is
// offline. TOTP in a browser is e2e/web/tests/totp.spec.ts. Each runs in
// exit.yml with the other exit tests.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// secretsKey is a fresh CLIPMUX_SECRETS_KEY of that version.
func secretsKey(version string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "v" + version + ":" + hex.EncodeToString(b)
}

// setEnv sets (or with value "" removes) one variable for the next launch.
func (in *instance) setEnv(name, value string) {
	env := in.env[:0:0]
	for _, kv := range in.env {
		if !strings.HasPrefix(kv, name+"=") {
			env = append(env, kv)
		}
	}
	if value != "" {
		env = append(env, name+"="+value)
	}
	in.env = env
}

// refusesToBoot runs serve with the instance's environment, which must stop
// it before it listens, and returns what it printed.
func (in *instance) refusesToBoot() string {
	in.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve")
	cmd.Env = in.env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		in.t.Fatalf("serve kept running; want it to refuse to boot:\n%s", out)
	}
	if err == nil {
		in.t.Fatalf("serve exited 0; want it to refuse to boot:\n%s", out)
	}
	return string(out)
}

// plays checks that a signed playback ID decodes, with a token from kid.
func (in *instance) plays(kid, priv, pid string) {
	in.t.Helper()
	if video, _ := play(in.t, in.url+"/v/"+sign(in.t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8"); video < 50 {
		in.t.Fatalf("%s decoded %d frames, want at least 50", pid, video)
	}
}

// filesOf lists the stored files under the data directory whose path names
// the output.
func (in *instance) filesOf(output string) []string {
	in.t.Helper()
	var found []string
	_ = filepath.WalkDir(in.dataDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(p, string(filepath.Separator)+output+string(filepath.Separator)) {
			found = append(found, p)
		}
		return nil
	})
	return found
}

// Retention (D069, D070; P5 check 3): rows and files past their windows go
// at `clipmux admin prune --files`, the same rules serve runs hourly, and a
// dry run counts them without deleting. The windows are days long, so the
// drill backdates instead of waiting.
func TestDrillRetentionAfterBackdating(t *testing.T) {
	in := start(t)
	key, kid, priv := in.credentials("Retention")
	owner := in.owner()
	asset, pid := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, asset, 3*time.Minute)
	retired := in.currentOutput(asset)
	in.reencode(key, asset)
	if len(in.filesOf(retired)) == 0 {
		t.Fatalf("no stored files of output %s before the drill", retired)
	}

	// An upload retried with an Idempotency-Key leaves a key row behind.
	req, _ := http.NewRequest("POST", in.url+"/v1/uploads", strings.NewReader(`{"size": 1024, "playback_policy": "signed"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "drill-retention")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusCreated {
		t.Fatalf("create upload with an Idempotency-Key: %v %v", err, res)
	}
	res.Body.Close()
	in.admin("create-pairing")

	ctx := context.Background()
	for _, q := range []string{
		`UPDATE outputs SET retired_at = now() - interval '9 days' WHERE id = '` + retired + `'`,
		`UPDATE idempotency_keys SET created_at = now() - interval '2 days'`,
		`UPDATE agent_pairings SET expires_at = now() - interval '2 days'`,
		`UPDATE owner_sessions SET last_seen_at = now() - interval '8 days'`,
	} {
		if _, err := in.pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	counts := func() string {
		return in.scalar(`SELECT (SELECT count(*) FROM idempotency_keys) || ' ' || (SELECT count(*) FROM agent_pairings) || ' ' ||
			(SELECT count(*) FROM owner_sessions) || ' ' || (SELECT count(*) FROM outputs WHERE purged_at IS NOT NULL)`)
	}
	if got := counts(); got != "1 1 1 0" {
		t.Fatalf("before pruning: keys, pairings, sessions, purged outputs = %s, want 1 1 1 0", got)
	}

	dry := in.admin("prune", "--dry-run")
	t.Logf("prune --dry-run:\n%s", dry)
	if got := counts(); got != "1 1 1 0" {
		t.Fatalf("a dry run changed rows: %s", got)
	}
	out := in.admin("prune", "--files")
	t.Logf("prune --files:\n%s", out)
	if got := counts(); got != "0 0 0 1" {
		t.Fatalf("after pruning: keys, pairings, sessions, purged outputs = %s, want 0 0 0 1", got)
	}
	if in.scalar(`SELECT (purged_at IS NOT NULL)::text FROM outputs WHERE id = $1`, retired) != "true" {
		t.Fatal("the retired output isn't marked purged")
	}
	if left := in.filesOf(retired); len(left) != 0 {
		t.Fatalf("the retired output's files are still stored: %v", left)
	}
	// The idle session is gone, so the owner signs in again; the video plays
	// its current output.
	if s := ownerCall(t, owner, "GET", in.url+"/v1/session", nil, nil); s != http.StatusUnauthorized {
		t.Fatalf("the pruned session got %d, want 401", s)
	}
	in.plays(kid, priv, pid)
}

// pgTool runs pg_dump or pg_restore of Postgres 18: the host's if it is
// that version, else the postgres:18-alpine image's, on the host network.
func pgTool(t *testing.T, stdin io.Reader, stdout io.Writer, tool string, args ...string) {
	t.Helper()
	var cmd *exec.Cmd
	if v, err := exec.Command(tool, "--version").Output(); err == nil && regexp.MustCompile(` 18\.`).Match(v) {
		cmd = exec.Command(tool, args...)
	} else {
		cmd = exec.Command("docker", append([]string{"run", "--rm", "-i", "--network", "host", "postgres:18-alpine", tool}, args...)...)
	}
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", tool, err, stderr.String())
	}
}

// withDatabase is the URL with another database name.
func withDatabase(t *testing.T, raw, name string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Backup and restore (docs/operations.md, "Backups"; P5 check 7): a pg_dump
// and a copy of the data directory, restored into a new database on a new
// data directory with the same secrets key, serve the same owner session,
// API key, signing key and video, and encode new ones. A different secrets
// key refuses to boot on the restored database.
func TestDrillBackupAndRestore(t *testing.T) {
	secrets := secretsKey("1")
	in := start(t, "CLIPMUX_SECRETS_KEY="+secrets)
	owner := in.owner()
	key, kid, priv := in.credentials("Backup")
	asset, pid := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, asset, 3*time.Minute)

	// The backup, taken with serve running, as the operations doc's is.
	schema := in.scalar(`SELECT current_schema()`)
	dbURL := os.Getenv("TEST_DATABASE_URL")
	var dump bytes.Buffer
	pgTool(t, nil, &dump, "pg_dump", "--format=custom", "--no-owner", "--schema="+schema, dbURL)
	backup := filepath.Join(t.TempDir(), "data")
	if out, err := exec.Command("cp", "-a", in.dataDir, backup).CombinedOutput(); err != nil {
		t.Fatalf("copy the data directory: %v\n%s", err, out)
	}
	in.halt(os.Interrupt)

	// A new database and a new data directory, as on a replacement host.
	name := "clipmux_restore_" + strings.ToLower(hex.EncodeToString([]byte(schema))[:12])
	ctx := context.Background()
	if _, err := in.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	restoredURL := withDatabase(t, dbURL, name)
	t.Cleanup(func() { _, _ = in.pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	pgTool(t, &dump, io.Discard, "pg_restore", "--no-owner", "--exit-on-error", "--dbname="+restoredURL)
	pool, err := pgxpool.New(ctx, restoredURL+"&search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	restoredDir := filepath.Join(t.TempDir(), "restored")
	if out, err := exec.Command("cp", "-a", backup, restoredDir).CombinedOutput(); err != nil {
		t.Fatalf("restore the data directory: %v\n%s", err, out)
	}
	re := &instance{t: t, pool: pool, url: in.url, dataDir: restoredDir, env: append([]string(nil), in.env...)}
	re.setEnv("DATABASE_URL", restoredURL+"&search_path="+schema+"&pool_max_conns=8")
	re.setEnv("CLIPMUX_DATA_DIR", restoredDir)

	re.setEnv("CLIPMUX_SECRETS_KEY", secretsKey("1"))
	if out := re.refusesToBoot(); !strings.Contains(out, "secrets") {
		t.Fatalf("serve with another secrets key refused without naming the secrets:\n%s", out)
	}
	re.setEnv("CLIPMUX_SECRETS_KEY", secrets)
	t.Cleanup(func() {
		re.halt(os.Interrupt)
		checkLogs(t, "restored serve", re.log())
		if t.Failed() {
			t.Logf("restored serve logs:\n%s", re.log())
		}
	})
	re.launch()

	if s := ownerCall(t, owner, "GET", re.url+"/v1/session", nil, nil); s != http.StatusOK {
		t.Fatalf("the owner's session after the restore: %d", s)
	}
	var a struct{ Status string }
	if s := re.call("GET", "/v1/assets/"+asset, key, nil, &a); s != http.StatusOK || a.Status != "ready" {
		t.Fatalf("the asset after the restore: %d %+v", s, a)
	}
	re.plays(kid, priv, pid)
	second, secondPID := re.upload(key, fixture(t, 4, 320, 180))
	re.waitReady(key, second, 3*time.Minute)
	re.plays(kid, priv, secondPID)
}

// Rotating the secrets key (D071; P5 check 8): with only the new key serve
// refuses to boot; with the old one as CLIPMUX_SECRETS_KEY_PREVIOUS it runs,
// `clipmux admin reseal-secrets` reseals everything, and then the new key
// alone serves and encodes while the old one alone is refused.
func TestDrillSecretsKeyRotation(t *testing.T) {
	k1, k2 := secretsKey("1"), secretsKey("2")
	in := start(t, "CLIPMUX_SECRETS_KEY="+k1)
	key, kid, priv := in.credentials("Rotation")
	in.owner() // the owner exists; a TOTP secret would be sealed too
	asset, pid := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, asset, 3*time.Minute)
	in.halt(os.Interrupt)

	in.setEnv("CLIPMUX_SECRETS_KEY", k2)
	if out := in.refusesToBoot(); !strings.Contains(out, "CLIPMUX_SECRETS_KEY_PREVIOUS") {
		t.Fatalf("serve with only the new key refused without naming CLIPMUX_SECRETS_KEY_PREVIOUS:\n%s", out)
	}
	in.setEnv("CLIPMUX_SECRETS_KEY_PREVIOUS", k1)
	in.launch()
	in.plays(kid, priv, pid)
	if out := in.admin("reseal-secrets"); !strings.Contains(out, "every secret is sealed with key v2") {
		t.Fatalf("reseal-secrets:\n%s", out)
	}
	in.halt(os.Interrupt)

	in.setEnv("CLIPMUX_SECRETS_KEY_PREVIOUS", "")
	in.setEnv("CLIPMUX_SECRETS_KEY", k1)
	in.refusesToBoot()
	in.setEnv("CLIPMUX_SECRETS_KEY", k2)
	in.launch()
	in.plays(kid, priv, pid)
	second, secondPID := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, second, 3*time.Minute)
	in.plays(kid, priv, secondPID)
	if v := in.scalar(`SELECT string_agg(DISTINCT substring(secret_encrypted FROM 2 FOR 4)::text, ',') FROM content_key_secrets`); v != `\x00000002` {
		t.Fatalf("content key secrets are sealed with key versions %s, want only 2", v)
	}
}

// Rotating the content key secret (D071; P5 check 8) on built-in delivery:
// the new version is usable at once, new outputs use it, and the existing
// output keeps its version and plays.
func TestDrillContentKeyRotation(t *testing.T) {
	in := start(t)
	owner := in.owner()
	key, kid, priv := in.credentials("Content keys")
	first, firstPID := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, first, 3*time.Minute)
	var res struct {
		Version int  `json:"version"`
		Usable  bool `json:"usable"`
		Current int  `json:"current"`
	}
	if s := ownerCall(t, owner, "POST", in.url+"/v1/content-keys/rotate", map[string]any{}, &res); s != http.StatusOK || !res.Usable || res.Current != 2 {
		t.Fatalf("rotate: %d %+v, want 200, usable, current 2", s, res)
	}
	second, secondPID := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, second, 3*time.Minute)
	version := func(asset string) string {
		return in.scalar(`SELECT o.key_version::text FROM outputs o JOIN assets a ON a.current_output_id = o.id WHERE a.id = $1`, asset)
	}
	if v1, v2 := version(first), version(second); v1 != "1" || v2 != "2" {
		t.Fatalf("key versions: the first output %s, the second %s; want 1 and 2", v1, v2)
	}
	in.plays(kid, priv, firstPID)
	in.plays(kid, priv, secondPID)
	// A restart loads the ring from Postgres and still opens both.
	in.restart()
	in.plays(kid, priv, firstPID)
	in.plays(kid, priv, secondPID)
}

// Rotating a signing key (D071; P5 check 8): tokens from the retired key
// keep playing until its retirement, the new key plays at once, and
// revoking the old key refuses its tokens within the route cache's 30 s.
func TestDrillSigningKeyRotation(t *testing.T) {
	in := start(t)
	key, oldKid, oldPriv := in.credentials("Signing")
	asset, pid := in.upload(key, fixture(t, 4, 320, 180))
	in.waitReady(key, asset, 3*time.Minute)

	out := in.admin("rotate-signing-key", "--app", "Signing")
	newKid := regexp.MustCompile(`kid: (kid_\S+)`).FindStringSubmatch(out)[1]
	newPriv := out[strings.Index(out, "-----BEGIN"):]
	newPriv = newPriv[:strings.Index(newPriv, "-----END PRIVATE KEY-----")+len("-----END PRIVATE KEY-----")] + "\n"
	if !strings.Contains(out, "1 older key(s) keep working") {
		t.Fatalf("rotate-signing-key:\n%s", out)
	}
	list := in.admin("list-keys", "--app", "Signing")
	if !regexp.MustCompile(oldKid + `\s+created \S+\s+retires 20\d\d-`).MatchString(list) {
		t.Fatalf("list-keys doesn't show %s retiring:\n%s", oldKid, list)
	}
	in.plays(oldKid, oldPriv, pid)
	in.plays(newKid, newPriv, pid)

	in.admin("revoke-key", oldKid)
	master := func(kid, priv string) int {
		s, _ := get(t, in.url+"/v/"+sign(t, kid, priv, pid, time.Hour)+"/"+pid+"/master.m3u8")
		return s
	}
	deadline := time.Now().Add(35 * time.Second)
	for master(oldKid, oldPriv) != http.StatusForbidden {
		if time.Now().After(deadline) {
			t.Fatal("a token from the revoked key still plays after 35 s")
		}
		time.Sleep(time.Second)
	}
	in.plays(newKid, newPriv, pid)
}

// A transcription routed to RunPod (D078; P5 check 17): with "my PCs, else
// RunPod" and no PC online, the encode and then the transcription each wake
// a pod for primary_offline, and the pod transcribes with whisper.cpp.
func TestDrillTranscriptionOnRunPodWhenThePrimaryIsOffline(t *testing.T) {
	if os.Getenv("CLIPMUX_SKIP_SPEECH") == "1" {
		t.Skip("CLIPMUX_SKIP_SPEECH=1: no whisper.cpp here")
	}
	if os.Getenv("CLIPMUX_TEST_MODELS_DIR") == "" {
		t.Skip("set CLIPMUX_TEST_MODELS_DIR to the speech models (toolchain/fetch_speech_models.sh)")
	}
	c := newCloud(t, connectOpts{maxConcurrent: 4, monthlyLimit: 100_000, rateCentsPerHour: 50, speech: true})
	asset, _ := c.uploadWith(c.key, speechFixture(t), map[string]any{"generate": map[string]bool{"subtitles": true}})
	c.waitReady(c.key, asset, 5*time.Minute)
	trk := c.waitTracks(c.key, asset, 20*time.Minute)["subtitles"]
	if trk.Status != "ready" || trk.Language != "en" {
		t.Fatalf("subtitles = %+v", trk)
	}
	var tr struct {
		Text string `json:"text"`
	}
	c.call("GET", "/v1/assets/"+asset+"/tracks/"+trk.ID+"/transcript", c.key, nil, &tr)
	if !strings.Contains(strings.ToLower(tr.Text), "ask not what your country") {
		t.Fatalf("transcript = %q", tr.Text)
	}
	for _, kind := range []string{"transcode", "transcribe"} {
		r := c.ran(asset, kind)
		if len(r) != 1 || r[0].executor != "runpod" || r[0].reason != "primary_offline" || r[0].dispatchID == "" || r[0].status != "committed" {
			t.Fatalf("%s attempts %+v: want one committed on a RunPod pod, for primary_offline", kind, r)
		}
	}
	c.waitNoPods()
}
