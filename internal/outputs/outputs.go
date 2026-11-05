// Package outputs commits what a worker wrote and makes it playable
// (docs/architecture.md, "Commit"):
//
//  1. Verify, outside any lock: the manifest must match a listing of the
//     output prefix exactly, the master may reference only the output's own
//     media playlists, and every media playlist must be AES-128 encrypted
//     with the expected key URI and name only stored segments. Worker output
//     is untrusted, so nothing here relies on the worker's own checks.
//  2. One transaction, under the attempt's liveness lock: the output is
//     committed, current_output_id moves (the only pointer that does), the
//     previous output is retired, and a route_sync row is written for each
//     active playback ID.
//  3. Routes are published from those rows. When the last route of the
//     current output is confirmed, a second transaction moves the asset to
//     ready with video.asset.ready (or emits video.asset.updated for a
//     re-encode). A crash anywhere resumes from the outbox.
package outputs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// InvalidError means the output failed verification and was not committed.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "output rejected: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

const maxPlaylistBytes = 1 << 20

// Committer commits outputs and publishes their routes.
type Committer struct {
	pool   *pgxpool.Pool
	queue  *queue.Queue
	store  storage.Storage
	Logger *slog.Logger

	// publishInline publishes routes right after the commit; tests turn it
	// off to stand in for a crash between the two steps.
	publishInline bool
}

// New returns a Committer.
func New(pool *pgxpool.Pool, q *queue.Queue, store storage.Storage) *Committer {
	return &Committer{pool: pool, queue: q, store: store, Logger: slog.Default(), publishInline: true}
}

// Commit verifies and commits the output of the attempt holding token.
func (c *Committer) Commit(ctx context.Context, token string, m media.Manifest) (queue.Verdict, error) {
	att, err := c.queue.Authenticate(ctx, token)
	if err != nil {
		return queue.Verdict{}, err
	}
	if err := c.verify(ctx, att, m); err != nil {
		return queue.Verdict{}, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return queue.Verdict{}, err
	}
	v, err := c.queue.CommitAttempt(ctx, token, func(qs *db.Queries, a db.Attempt, _ db.Job) error {
		return commit(ctx, qs, a, raw)
	})
	if err != nil || v.Stop {
		return v, err
	}
	if c.publishInline {
		if _, err := c.SyncRoutes(ctx, 1000); err != nil {
			// Committed; the route sweeper retries publication.
			c.Logger.Warn("route publication deferred", "asset", att.AssetID, "err", err)
		}
	}
	return v, nil
}

// verify checks the stored output against the manifest and the rules above.
func (c *Committer) verify(ctx context.Context, att db.Attempt, m media.Manifest) error {
	out, err := db.New(c.pool).GetOutput(ctx, att.OutputID)
	if err != nil {
		return err
	}
	switch {
	case m.OutputFormat != media.OutputFormat:
		return invalid("output_format %d, want %d", m.OutputFormat, media.OutputFormat)
	case m.Encryption != "aes-128":
		return invalid("encryption %q, want aes-128", m.Encryption)
	case m.KeyVersion != out.KeyVersion:
		return invalid("key_version %d, want %d", m.KeyVersion, out.KeyVersion)
	}
	want := make(map[string]int64, len(m.Files))
	for _, f := range m.Files {
		if !media.ValidOutputPath(f.Path) || f.Path == "manifest.json" {
			return invalid("manifest lists %q", f.Path)
		}
		if _, dup := want[f.Path]; dup {
			return invalid("manifest lists %q twice", f.Path)
		}
		want[f.Path] = f.Size
	}

	prefix := "media/" + att.AssetID + "/" + att.OutputID + "/"
	stored := map[string]int64{}
	for cursor := ""; ; {
		objs, next, err := c.store.List(ctx, prefix, cursor)
		if err != nil {
			return err
		}
		for _, o := range objs {
			stored[strings.TrimPrefix(o.Key, prefix)] = o.Size
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if _, ok := stored["manifest.json"]; !ok {
		return invalid("manifest.json was not uploaded")
	}
	for p, size := range want {
		got, ok := stored[p]
		switch {
		case !ok:
			return invalid("%s is in the manifest but not in storage", p)
		case got != size:
			return invalid("%s is %d bytes in storage, %d in the manifest", p, got, size)
		}
	}
	for p := range stored {
		if _, ok := want[p]; !ok && p != "manifest.json" {
			return invalid("storage holds %s, which the manifest doesn't list", p)
		}
	}

	text, err := c.read(ctx, prefix+"master.m3u8")
	if err != nil {
		return err
	}
	dirs, err := media.CheckMaster(text)
	if err != nil {
		return invalid("%v", err)
	}
	if got := renditionDirs(want); !slices.Equal(got, dirs) {
		return invalid("master references %v, the output holds %v", dirs, got)
	}
	for _, d := range dirs {
		text, err := c.read(ctx, prefix+d+"/index.m3u8")
		if err != nil {
			return err
		}
		segments, err := media.CheckMediaPlaylist(text, out.KeyVersion)
		if err != nil {
			return invalid("%s: %v", d, err)
		}
		for _, name := range append(segments, "init.mp4") {
			if _, ok := want[d+"/"+name]; !ok {
				return invalid("%s/index.m3u8 references %s, which isn't in the output", d, name)
			}
		}
	}
	return nil
}

// renditionDirs lists the sorted directories holding a media playlist.
func renditionDirs(files map[string]int64) []string {
	set := map[string]bool{}
	for p := range files {
		if dir, name, ok := strings.Cut(p, "/"); ok && name == "index.m3u8" {
			set[dir] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func (c *Committer) read(ctx context.Context, key string) (string, error) {
	rc, err := c.store.Get(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return "", invalid("%s is missing", key)
	}
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxPlaylistBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxPlaylistBytes {
		return "", invalid("%s is larger than a playlist can be", key)
	}
	return string(b), nil
}

// route is a route_sync row's payload: the record that makes a playback ID
// servable (docs/architecture.md, "Route records").
type route struct {
	V          int    `json:"v"`
	PlaybackID string `json:"playback_id"`
	AppID      string `json:"app_id"`
	AssetID    string `json:"asset_id"`
	OutputID   string `json:"output_id"`
	Policy     string `json:"policy"`
	Backend    string `json:"backend"`
}

// commit is step 2, inside the liveness transaction.
func commit(ctx context.Context, qs *db.Queries, a db.Attempt, manifest []byte) error {
	asset, err := qs.LockAsset(ctx, a.AssetID)
	if err != nil {
		return err
	}
	if asset.Status == "deleted" {
		return invalid("the asset was deleted")
	}
	n, err := qs.CommitOutput(ctx, db.CommitOutputParams{ID: a.OutputID, Manifest: manifest})
	if err != nil {
		return err
	}
	if n != 1 {
		return invalid("output %s is not being written", a.OutputID)
	}
	previous := asset.CurrentOutputID
	if asset, err = qs.MoveCurrentOutput(ctx, db.MoveCurrentOutputParams{ID: asset.ID, CurrentOutputID: pgtype.Text{String: a.OutputID, Valid: true}}); err != nil {
		return err
	}
	if previous.Valid {
		if err := qs.RetireOutput(ctx, previous.String); err != nil {
			return err
		}
	}
	pids, err := activePlaybackIDs(ctx, qs, asset.ID)
	if err != nil {
		return err
	}
	for _, p := range pids {
		payload, _ := json.Marshal(route{
			V: 1, PlaybackID: p.ID, AppID: asset.AppID, AssetID: asset.ID, OutputID: a.OutputID, Policy: p.Policy,
			// The built-in handler serves every asset from origin storage;
			// "r2" arrives with the Cloudflare Worker in Phase 2.
			Backend: "origin",
		})
		if err := qs.InsertOutbox(ctx, db.InsertOutboxParams{
			ID: ids.New(ids.Event), ProjectID: asset.ProjectID, Kind: "route_sync",
			AppID: pgtype.Text{String: asset.AppID, Valid: true}, AssetID: pgtype.Text{String: asset.ID, Valid: true},
			Payload: payload,
		}); err != nil {
			return err
		}
	}
	if len(pids) == 0 {
		return finishPublication(ctx, qs, asset.ID, a.OutputID)
	}
	return nil
}

func activePlaybackIDs(ctx context.Context, qs *db.Queries, assetID string) ([]db.PlaybackID, error) {
	all, err := qs.ListPlaybackIDs(ctx, assetID)
	return slices.DeleteFunc(all, func(p db.PlaybackID) bool { return p.Status != "active" }), err
}

// SyncRoutes publishes pending route records, then finishes publication for
// every output whose routes are all confirmed. It returns how many routes it
// published. Safe to run concurrently (SKIP LOCKED) and repeatedly.
func (c *Committer) SyncRoutes(ctx context.Context, limit int) (int, error) {
	published := 0
	err := pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		rows, err := qs.LockPendingRouteSyncs(ctx, int32(limit))
		if err != nil {
			return err
		}
		type key struct{ asset, output string }
		touched := map[key]bool{}
		for _, row := range rows {
			var r route
			if err := json.Unmarshal(row.Payload, &r); err != nil {
				return fmt.Errorf("route_sync %s: %w", row.ID, err)
			}
			// The built-in delivery handler reads routes straight from
			// Postgres, so confirming the row is the whole publication. The
			// Phase 2 Worker adapter writes _routes/{playback_id}.json here,
			// and a failed write is retried through RetryOutbox.
			if err := qs.MarkOutboxDone(ctx, row.ID); err != nil {
				return err
			}
			touched[key{r.AssetID, r.OutputID}] = true
			published++
		}
		for k := range touched {
			if err := finishPublication(ctx, qs, k.asset, k.output); err != nil {
				return err
			}
		}
		return nil
	})
	return published, err
}

// finishPublication is step 4. It locks the asset before counting pending
// routes, so of two concurrent publishers the second sees the first's work
// and exactly one of them finishes.
func finishPublication(ctx context.Context, qs *db.Queries, assetID, outputID string) error {
	asset, err := qs.LockAsset(ctx, assetID)
	if err != nil {
		return err
	}
	if asset.CurrentOutputID.String != outputID {
		return nil // superseded by a newer output, which finishes on its own
	}
	pending, err := qs.CountPendingRoutesForOutput(ctx, db.CountPendingRoutesForOutputParams{AssetID: pgtype.Text{String: assetID, Valid: true}, OutputID: outputID})
	if err != nil || pending > 0 {
		return err
	}
	pids, err := activePlaybackIDs(ctx, qs, assetID)
	if err != nil {
		return err
	}
	list := make([]map[string]string, 0, len(pids))
	for _, p := range pids {
		list = append(list, map[string]string{"id": p.ID, "policy": p.Policy})
	}
	extra := map[string]any{"output_id": outputID, "playback_ids": list}
	switch asset.Status {
	case "processing":
		version, err := qs.MarkAssetReady(ctx, assetID)
		if err != nil {
			return err
		}
		return outbox.AssetEvent(ctx, qs, asset, outbox.AssetReady, version, extra)
	case "ready":
		return outbox.AssetEvent(ctx, qs, asset, outbox.AssetUpdated, asset.Version, extra)
	}
	return nil
}
