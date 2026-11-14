package tracks_test

// Publishing a track (P5-T15): a stored transcript waits for its routes,
// and for its asset, before it is ready; a newer track retires the old one.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tracks"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

type rig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	q      *queue.Queue
	commit *outputs.Committer
	pub    *tracks.Publisher
}

func newRig(t *testing.T, assetStatus string, playbackIDs int) *rig {
	t.Helper()
	pool := dbtest.New(t)
	fs, err := storage.OpenFilesystem(t.TempDir(), storage.FilesystemOptions{BaseURL: "http://unused"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	secret, _ := contentkey.FromHex(1, strings.Repeat("11", 32))
	q := queue.New(pool)
	r := &rig{t: t, pool: pool, q: q, commit: outputs.New(pool, q, stores.Fixed(fs)),
		pub: &tracks.Publisher{Stores: stores.Fixed(fs), Keys: contentkey.NewRing(secret)}}
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	r.exec(`INSERT INTO outputs (id, project_id, asset_id, key_version, status, manifest) VALUES ('out_1', 'prj_default', 'ast_1', 1, 'committed', '{"duration_seconds": 30, "audio": true}')`)
	r.exec(`UPDATE assets SET current_output_id = 'out_1', status = $1 WHERE id = 'ast_1'`, assetStatus)
	for i := range playbackIDs {
		r.exec(`INSERT INTO playback_ids (id, project_id, asset_id, policy) VALUES ($1, 'prj_default', 'ast_1', 'signed')`, "pid_"+string(rune('a'+i)))
	}
	return r
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// transcribe asks for subtitles and runs a transcription to its commit.
func (r *rig) transcribe(text string) string {
	r.t.Helper()
	ctx := context.Background()
	qs := db.New(r.pool)
	created, err := tracks.Create(ctx, qs, "prj_default", "ast_1", tracks.Generate{Subtitles: true})
	if err != nil {
		r.t.Fatal(err)
	}
	asset, _ := qs.GetAssetByID(ctx, "ast_1")
	if _, err := tracks.QueueTranscriptions(ctx, qs, asset, true); err != nil {
		r.t.Fatal(err)
	}
	c, err := r.q.Claim(ctx, queue.Worker{Executor: "builtin", Kinds: []string{"transcribe"}, Speech: true})
	if err != nil || c == nil {
		r.t.Fatalf("claim %+v, %v", c, err)
	}
	raw, _ := json.Marshal(transcripts.Transcript{Version: 1, Language: "hi", Model: "m@sha256:0",
		Segments: []transcripts.Segment{{ID: "s0", Text: text, StartMS: 1000, EndMS: 2000}}})
	if _, err := r.q.CommitAttempt(ctx, c.Token, r.pub.CommitTranscript(ctx, raw)); err != nil {
		r.t.Fatal(err)
	}
	return created[0].ID
}

func (r *rig) status(id string) string {
	r.t.Helper()
	var s string
	if err := r.pool.QueryRow(context.Background(), `SELECT status FROM tracks WHERE id = $1`, id).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) sync() {
	r.t.Helper()
	if _, err := r.commit.SyncRoutes(context.Background(), 100); err != nil {
		r.t.Fatal(err)
	}
}

func TestATrackIsReadyOnceItsRoutesAndItsVideoAre(t *testing.T) {
	// The output committed and its routes are still pending: the asset is
	// processing when the transcription lands.
	r := newRig(t, "processing", 2)
	ctx := context.Background()
	qs := db.New(r.pool)
	asset, _ := qs.GetAssetByID(ctx, "ast_1")
	pids, _ := qs.ListPlaybackIDs(ctx, "ast_1")
	for _, p := range pids {
		if err := outputs.QueueRoute(ctx, qs, asset, p, "out_1"); err != nil {
			t.Fatal(err)
		}
	}
	trk := r.transcribe("नमस्ते")
	if s := r.status(trk); s != "publishing" {
		t.Fatalf("after its commit the track is %s, want publishing", s)
	}
	var trackRows int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind = 'route_sync' AND payload->>'op' = 'track' AND payload->>'track_id' = $1`, trk).Scan(&trackRows); err != nil || trackRows != 2 {
		t.Fatalf("%d track route rows (%v), want one per playback ID", trackRows, err)
	}
	r.sync()
	var assetStatus string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM assets WHERE id = 'ast_1'`).Scan(&assetStatus); err != nil || assetStatus != "ready" {
		t.Fatalf("asset %s, %v", assetStatus, err)
	}
	if s := r.status(trk); s != "ready" {
		t.Fatalf("after the sync the track is %s, want ready", s)
	}
	rec, err := routes.Desired(ctx, qs, "pid_a", "sto_default")
	if err != nil || rec == nil {
		t.Fatalf("record %+v, %v", rec, err)
	}
	if got := rec.Tracks[trk]; got != (routes.Track{Kind: "subtitles", Language: "hi", Name: "Hindi", DurationMS: 30000, KeyVersion: 1, Listed: true}) {
		t.Fatalf("record track %+v", got)
	}

	// Regenerating: the new track is ready only after its own sync, and
	// then the old one is retired, still in the record, unlisted.
	next := r.transcribe("नमस्ते दोबारा")
	if r.status(trk) != "ready" || r.status(next) != "publishing" {
		t.Fatalf("before the sync: old %s, new %s", r.status(trk), r.status(next))
	}
	r.sync()
	if r.status(trk) != "retired" || r.status(next) != "ready" {
		t.Fatalf("after the sync: old %s, new %s", r.status(trk), r.status(next))
	}
	r.sync() // the refresh the retirement queued
	rec, _ = routes.Desired(ctx, qs, "pid_a", "sto_default")
	if !rec.Tracks[next].Listed || rec.Tracks[trk].Listed || len(rec.Tracks) != 2 {
		t.Fatalf("record tracks %+v", rec.Tracks)
	}
}

func TestATrackWithNoPlaybackIDIsReadyAtOnce(t *testing.T) {
	r := newRig(t, "ready", 0)
	trk := r.transcribe("hello")
	if s := r.status(trk); s != "ready" {
		t.Fatalf("track %s, want ready with nothing to publish", s)
	}
}
