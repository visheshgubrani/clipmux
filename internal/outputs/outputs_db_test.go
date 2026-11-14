package outputs

// Real Postgres and filesystem storage, with hand-built output files so the
// commit checks run without media tools. Expected behaviour comes from
// docs/architecture.md, "Commit" and "Write path checks".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
)

const master = `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,URI="audio/index.m3u8",GROUP-ID="a",NAME="x"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360,AUDIO="a"
360p/index.m3u8
`

const mediaPL = `#EXTM3U
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="../key/1",IV=0x00000000000000000000000000000001
#EXTINF:6.000,
seg_00001.m4s
#EXTINF:2.000,
seg_00002.m4s
#EXT-X-ENDLIST
`

type fx struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *storage.Filesystem
	q     *queue.Queue
	c     *Committer
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pool := dbtest.New(t)
	store, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	q := queue.New(pool)
	c := New(pool, q, stores.Fixed(store))
	f := &fx{t: t, pool: pool, store: store, q: q, c: c}
	f.exec(`UPDATE projects SET media_url = 'https://media.example'`)
	return f
}

func (f *fx) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *fx) scalar(sql string, args ...any) any {
	f.t.Helper()
	var v any
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// asset creates an asset with playback IDs and queues a job of kind.
func (f *fx) asset(status string, pids int, kind string) {
	f.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, passthrough) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'lesson-7')`)
	if status == "ready" {
		f.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_old', 'prj_default', 'ast_1', 1, 'committed', '{}')`)
		f.exec(`UPDATE assets SET status = 'ready', current_output_id = 'out_old' WHERE id = 'ast_1'`)
	}
	for i := range pids {
		f.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', 'ast_1', 'signed')`, fmt.Sprintf("pid_%d", i))
	}
	if _, err := queue.Enqueue(context.Background(), f.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_1", Kind: kind}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) claim() *queue.Claim {
	f.t.Helper()
	c, err := f.q.Claim(context.Background(), queue.Worker{Executor: "builtin"})
	if err != nil || c == nil {
		f.t.Fatalf("claim: %v, %v", c, err)
	}
	return c
}

// write stores a complete output for the claim and returns its manifest.
// files overrides or removes (empty string) individual files.
func (f *fx) write(c *queue.Claim, files map[string]string) media.Manifest {
	f.t.Helper()
	all := map[string]string{
		"master.m3u8": master, "poster.jpg": "jpeg",
		"360p/index.m3u8": mediaPL, "360p/init.mp4": "init-v", "360p/seg_00001.m4s": "seg-v-1-cipher..", "360p/seg_00002.m4s": "seg-v-2-cipher..",
		"audio/index.m3u8": mediaPL, "audio/init.mp4": "init-a", "audio/seg_00001.m4s": "seg-a-1-cipher..", "audio/seg_00002.m4s": "seg-a-2-cipher..",
	}
	for k, v := range files {
		all[k] = v
	}
	m := media.Manifest{OutputFormat: 1, DurationSeconds: 8, Encryption: "aes-128", KeyVersion: 1, Audio: true,
		Renditions: []media.Rendition{{Name: "360p", Width: 640, Height: 360}}}
	prefix := "media/" + c.Job.AssetID + "/" + c.Output.ID + "/"
	for p, body := range all {
		if body == "" {
			continue
		}
		if err := f.store.Put(context.Background(), prefix+p, strings.NewReader(body)); err != nil {
			f.t.Fatal(err)
		}
		m.Files = append(m.Files, media.File{Path: p, Size: int64(len(body))})
	}
	raw, _ := json.Marshal(m)
	if err := f.store.Put(context.Background(), prefix+"manifest.json", strings.NewReader(string(raw))); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fx) events() []string {
	f.t.Helper()
	rows, _ := f.pool.Query(context.Background(), `SELECT payload->>'type' FROM outbox WHERE kind = 'webhook' ORDER BY created_at, id`)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func wantInvalid(t *testing.T, err error, name string) {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("%s: err = %v, want an InvalidError", name, err)
	}
}

func TestFirstCommitMakesTheAssetReadyAfterItsRoutesPublish(t *testing.T) {
	f := newFx(t)
	f.asset("processing", 2, "transcode")
	c := f.claim()
	m := f.write(c, nil)
	before := f.scalar(`SELECT version FROM assets`).(int64)

	if v, err := f.c.Commit(context.Background(), c.Token, m); err != nil || v.Stop {
		t.Fatalf("Commit = %+v, %v", v, err)
	}
	got := map[string]any{
		"asset":   f.scalar(`SELECT status FROM assets`),
		"current": f.scalar(`SELECT current_output_id FROM assets`),
		"output":  f.scalar(`SELECT status FROM outputs WHERE id = $1`, c.Output.ID),
		"attempt": f.scalar(`SELECT status FROM attempts`),
		"job":     f.scalar(`SELECT status FROM jobs`),
		"pending": f.scalar(`SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND done_at IS NULL`),
		"routes":  f.scalar(`SELECT count(*) FROM outbox WHERE kind = 'route_sync'`),
	}
	want := map[string]any{"asset": "ready", "current": c.Output.ID, "output": "committed", "attempt": "committed", "job": "succeeded", "pending": int64(0), "routes": int64(2)}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if v := f.scalar(`SELECT version FROM assets`).(int64); v != before+2 {
		t.Errorf("version = %d, want %d (pointer move, then ready)", v, before+2)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.ready" {
		t.Fatalf("events = %v, want one video.asset.ready", ev)
	}
	pids := f.scalar(`SELECT payload->'data'->'playback_ids' FROM outbox WHERE kind = 'webhook'`)
	if raw, _ := json.Marshal(pids); !strings.Contains(string(raw), "pid_0") || !strings.Contains(string(raw), "pid_1") {
		t.Fatalf("ready event playback_ids = %s", raw)
	}
	if b := f.scalar(`SELECT payload->'data'->>'base_url' FROM outbox WHERE kind = 'webhook'`); b != "https://media.example" {
		t.Fatalf("ready event base_url = %v", b)
	}
	if m := f.scalar(`SELECT manifest->>'key_version' FROM outputs WHERE id = $1`, c.Output.ID); m != "1" {
		t.Fatalf("stored manifest key_version = %v", m)
	}
}

func TestReencodeMovesThePointerAndRetiresTheOldOutputWithoutLeavingReady(t *testing.T) {
	f := newFx(t)
	f.asset("ready", 1, "reencode")
	c := f.claim()
	m := f.write(c, nil)
	if _, err := f.c.Commit(context.Background(), c.Token, m); err != nil {
		t.Fatal(err)
	}
	if s := f.scalar(`SELECT status FROM assets`); s != "ready" {
		t.Fatalf("asset = %v during and after a re-encode, want ready", s)
	}
	if s := f.scalar(`SELECT status FROM outputs WHERE id = 'out_old'`); s != "retired" {
		t.Fatalf("old output = %v, want retired", s)
	}
	if cur := f.scalar(`SELECT current_output_id FROM assets`); cur != c.Output.ID {
		t.Fatalf("current = %v", cur)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.updated" {
		t.Fatalf("events = %v, want one video.asset.updated", ev)
	}
}

func TestAnAssetWithoutPlaybackIDsIsReadyAtOnce(t *testing.T) {
	f := newFx(t)
	f.asset("processing", 0, "transcode")
	c := f.claim()
	if _, err := f.c.Commit(context.Background(), c.Token, f.write(c, nil)); err != nil {
		t.Fatal(err)
	}
	if s := f.scalar(`SELECT status FROM assets`); s != "ready" {
		t.Fatalf("asset = %v", s)
	}
}

// rung adds a rendition's files and its master entry to an output.
func rung(name string) map[string]string {
	return map[string]string{
		"master.m3u8":           master + "#EXT-X-STREAM-INF:BANDWIDTH=3000000,AUDIO=\"a\"\n" + name + "/index.m3u8\n",
		name + "/index.m3u8":    mediaPL,
		name + "/init.mp4":      "init-v",
		name + "/seg_00001.m4s": "seg-v-1-cipher..",
		name + "/seg_00002.m4s": "seg-v-2-cipher..",
	}
}

func withRung(m media.Manifest, name string) media.Manifest {
	m.Renditions = append([]media.Rendition{{Name: name}}, m.Renditions...)
	return m
}

func TestCommitAcceptsTheRenditionsTheAssetAskedFor(t *testing.T) {
	f := newFx(t)
	f.asset("processing", 0, "transcode")
	f.exec(`UPDATE assets SET renditions = '{720p,360p}'`)
	c := f.claim()
	if _, err := f.c.Commit(context.Background(), c.Token, withRung(f.write(c, rung("720p")), "720p")); err != nil {
		t.Fatalf("commit of the 720p and 360p asked for: %v", err)
	}
}

func TestCommitRejectsOutputsThatDontMatchStorageOrArentEncrypted(t *testing.T) {
	cases := map[string]func(f *fx, c *queue.Claim) media.Manifest{
		"a manifest file is missing from storage": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			m.Files = append(m.Files, media.File{Path: "360p/seg_00003.m4s", Size: 10})
			return m
		},
		"storage holds a file the manifest doesn't list": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			_ = f.store.Put(context.Background(), "media/ast_1/"+c.Output.ID+"/360p/seg_00099.m4s", strings.NewReader("extra"))
			return m
		},
		"a size differs": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			m.Files[0].Size++
			return m
		},
		"a media playlist is in the clear": func(f *fx, c *queue.Claim) media.Manifest {
			return f.write(c, map[string]string{"360p/index.m3u8": strings.Replace(mediaPL, "#EXT-X-KEY:METHOD=AES-128,URI=\"../key/1\",IV=0x00000000000000000000000000000001\n", "", 1)})
		},
		"a playlist names a segment that isn't stored": func(f *fx, c *queue.Claim) media.Manifest {
			return f.write(c, map[string]string{"audio/seg_00002.m4s": ""})
		},
		"the master points elsewhere": func(f *fx, c *queue.Claim) media.Manifest {
			return f.write(c, map[string]string{"master.m3u8": strings.Replace(master, "360p/index.m3u8", "https://evil.example/x.m3u8", 1)})
		},
		"the master lists a rendition that wasn't uploaded": func(f *fx, c *queue.Claim) media.Manifest {
			return f.write(c, map[string]string{"master.m3u8": master + "#EXT-X-STREAM-INF:BANDWIDTH=1\n720p/index.m3u8\n"})
		},
		"the key version is wrong": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			m.KeyVersion = 2
			return m
		},
		"the output format is unknown": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			m.OutputFormat = 2
			return m
		},
		// The renditions the asset asked for (D130).
		"a rendition below the largest was left out": func(f *fx, c *queue.Claim) media.Manifest {
			return withRung(f.write(c, rung("720p")), "720p") // the whole ladder, but no 480p
		},
		"a rendition that wasn't asked for": func(f *fx, c *queue.Claim) media.Manifest {
			f.exec(`UPDATE assets SET renditions = '{720p}'`)
			return withRung(f.write(c, rung("720p")), "720p")
		},
		"the manifest's renditions aren't the output's": func(f *fx, c *queue.Claim) media.Manifest {
			m := f.write(c, nil)
			m.Renditions = []media.Rendition{{Name: "480p", Width: 854, Height: 480}}
			return m
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			f.asset("processing", 1, "transcode")
			c := f.claim()
			_, err := f.c.Commit(context.Background(), c.Token, build(f, c))
			wantInvalid(t, err, name)
			if s := f.scalar(`SELECT status FROM attempts`); s != "running" {
				t.Fatalf("attempt = %v after a rejected commit, want running", s)
			}
			if cur := f.scalar(`SELECT current_output_id FROM assets`); cur != nil {
				t.Fatalf("pointer moved to %v", cur)
			}
		})
	}
}

func TestALateCompletionFromAStaleAttemptCommitsNothing(t *testing.T) {
	f := newFx(t)
	f.asset("processing", 1, "transcode")
	stale := f.claim()
	m := f.write(stale, nil)
	f.exec(`UPDATE attempts SET lease_expires_at = now() - interval '1 second'`)
	if _, err := f.q.SweepLostLeases(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	fresh := f.claim()

	v, err := f.c.Commit(context.Background(), stale.Token, m)
	if err != nil || !v.Stop || v.Reason != queue.ReasonSuperseded {
		t.Fatalf("stale commit = %+v, %v", v, err)
	}
	if cur := f.scalar(`SELECT current_output_id FROM assets`); cur != nil {
		t.Fatalf("a stale attempt moved the pointer to %v", cur)
	}
	if _, err := f.c.Commit(context.Background(), fresh.Token, f.write(fresh, nil)); err != nil {
		t.Fatal(err)
	}
	if cur := f.scalar(`SELECT current_output_id FROM assets`); cur != fresh.Output.ID {
		t.Fatalf("current = %v, want the fresh attempt's output", cur)
	}
}

func TestPendingRoutesFinishAfterACrash(t *testing.T) {
	f := newFx(t)
	f.asset("processing", 2, "transcode")
	c := f.claim()
	f.c.publishInline = false // the process died after the commit transaction
	if _, err := f.c.Commit(context.Background(), c.Token, f.write(c, nil)); err != nil {
		t.Fatal(err)
	}
	if s := f.scalar(`SELECT status FROM assets`); s != "processing" {
		t.Fatalf("asset = %v before routes publish, want processing", s)
	}
	if n, err := f.c.SyncRoutes(context.Background(), 100); err != nil || n != 2 {
		t.Fatalf("SyncRoutes = %d, %v", n, err)
	}
	if s := f.scalar(`SELECT status FROM assets`); s != "ready" {
		t.Fatalf("asset = %v after the sweep, want ready", s)
	}
	if ev := f.events(); len(ev) != 1 || ev[0] != "video.asset.ready" {
		t.Fatalf("events = %v, want exactly one ready", ev)
	}
	if n, _ := f.c.SyncRoutes(context.Background(), 100); n != 0 {
		t.Fatalf("a second sweep published %d routes again", n)
	}
}

// Revoking a playback ID unpublishes its route through the same outbox, so a
// failed write is retried rather than lost; the ID reads revoked only once
// the unpublish is confirmed.
func TestUnpublishRowsRevokeTheirPlaybackIDOnConfirmation(t *testing.T) {
	f := newFx(t)
	f.asset("ready", 2, "reencode")
	ctx := context.Background()
	f.exec(`UPDATE playback_ids SET status = 'revoking' WHERE id = 'pid_1'`)
	if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		asset, err := qs.LockAsset(ctx, "ast_1")
		if err != nil {
			return err
		}
		return QueueUnpublish(ctx, qs, asset, "pid_1")
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.scalar(`SELECT payload->>'op' FROM outbox WHERE kind = 'route_sync'`); got != "unpublish" {
		t.Fatalf("route_sync op = %v, want unpublish", got)
	}
	if got := f.scalar(`SELECT status FROM playback_ids WHERE id = 'pid_1'`); got != "revoking" {
		t.Fatalf("before publication pid_1 = %v, want revoking", got)
	}
	if n, err := f.c.SyncRoutes(ctx, 100); err != nil || n != 1 {
		t.Fatalf("SyncRoutes = %d, %v", n, err)
	}
	got := map[string]any{
		"pid_0":   f.scalar(`SELECT status FROM playback_ids WHERE id = 'pid_0'`),
		"pid_1":   f.scalar(`SELECT status FROM playback_ids WHERE id = 'pid_1'`),
		"revoked": f.scalar(`SELECT revoked_at IS NOT NULL FROM playback_ids WHERE id = 'pid_1'`),
		"pending": f.scalar(`SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND done_at IS NULL`),
		"status":  f.scalar(`SELECT status FROM assets`),
	}
	want := map[string]any{"pid_0": "active", "pid_1": "revoked", "revoked": true, "pending": int64(0), "status": "ready"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
