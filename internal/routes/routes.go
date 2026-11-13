// Package routes publishes what delivery may serve (docs/architecture.md,
// "Route records"). The built-in handler reads Postgres directly, so for it
// publication is only confirmation. The Cloudflare Worker can't ask
// Postgres, so it reads a projection from its bucket:
//
//	_routes/{playback_id}.json  one record per servable playback ID
//	_keys.json                  the signing keys delivery accepts
//
// A record exists exactly when the built-in handler would serve the ID, and
// it carries every fact the handler checks that the Worker can't look up:
// which outputs pinned paths and keys may name, with their key versions.
// Records are always rebuilt from current state, so outbox rows confirmed
// out of order can't publish something stale.
package routes

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/tokens"
)

// Record is _routes/{playback_id}.json.
type Record struct {
	V       int    `json:"v"`
	AppID   string `json:"app_id"`
	AssetID string `json:"asset_id"`
	// OutputID is the current output: what master.m3u8 and the unpinned
	// poster are generated from.
	OutputID string `json:"output_id"`
	Policy   string `json:"policy"`
	// Backend is "r2" for assets in the Worker's bucket, or "origin" for
	// assets on other storage, which the Worker fetches from the built-in
	// handler until a move job brings them over.
	Backend string `json:"backend"`
	// Outputs maps each output pinned paths may name (committed or retired)
	// to its key version; anything else is 404.
	Outputs map[string]int32 `json:"outputs"`
	// Denied is set while the ID's app is disabled: delivery answers 403
	// rather than 404, as the built-in handler does (D016).
	Denied bool `json:"denied,omitempty"`
	// Tracks maps each track tracks/{id}.vtt may name (stored, ready, or
	// retired within the window) to how delivery serves it. Omitted when
	// empty, so records of assets without tracks, and their ETags, don't
	// change (P5-T15).
	Tracks map[string]Track `json:"tracks,omitempty"`
	// NoAudio lists the outputs with no audio rendition, in ID order: their
	// video segments carry the watch time (D103). Omitted when empty.
	NoAudio []string `json:"no_audio,omitempty"`
	// Share marks a share link's playback ID, which delivery counts even
	// under the internal key (D103, D104). Set from P7-T06.
	Share bool `json:"share,omitempty"`
}

// Track is one track in a route record.
type Track struct {
	Kind       string `json:"kind"`
	Language   string `json:"language"`
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	KeyVersion int32  `json:"key_version"`
	// Listed: the master names it (a subtitles track being published or
	// ready). Chapters and retired tracks are served but never listed.
	Listed bool `json:"listed"`
}

// TrackOf is a servable track's record entry: delivery lists exactly
// the tracks a record lists.
func TrackOf(kind, status string, language, name pgtype.Text, durationMS pgtype.Int8, keyVersion pgtype.Int4) Track {
	return Track{
		Kind: kind, Language: language.String, Name: name.String, DurationMS: durationMS.Int64, KeyVersion: keyVersion.Int32,
		Listed: kind == "subtitles" && (status == "publishing" || status == "ready"),
	}
}

// Backends.
const (
	BackendR2     = "r2"
	BackendOrigin = "origin"
)

// Keys is _keys.json.
type Keys struct {
	V                       int   `json:"v"`
	MaxTokenLifetimeSeconds int32 `json:"max_token_lifetime_seconds"`
	Keys                    []Key `json:"keys"`
}

// Key is one public key delivery accepts.
type Key struct {
	Kid string `json:"kid"`
	// AppID is null for the server's internal key, which signs for any app
	// of the project.
	AppID *string `json:"app_id"`
	JWK   JWK     `json:"jwk"`
	// NotAfter is when a retired key stops verifying (Unix seconds); null
	// while it is active.
	NotAfter *int64 `json:"not_after"`
}

// JWK is an ES256 public key as WebCrypto imports it.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// RouteKey and KeysKey are where the Worker reads.
func RouteKey(playbackID string) string { return "_routes/" + playbackID + ".json" }

// KeysKey is the object holding _keys.json.
const KeysKey = "_keys.json"

// Delivery is the publication side of a delivery adapter. Every method is
// idempotent.
type Delivery interface {
	Publish(ctx context.Context, playbackID string, r Record) error
	Unpublish(ctx context.Context, playbackID string) error
	PublishKeys(ctx context.Context, k Keys) error
	// Bucket is where the Worker reads, for the reconciler; nil for the
	// built-in handler, which needs no projection.
	Bucket() storage.Storage
}

// Builtin is the built-in handler's adapter: it reads Postgres, so there is
// nothing to write.
type Builtin struct{}

func (Builtin) Publish(context.Context, string, Record) error { return nil }
func (Builtin) Unpublish(context.Context, string) error       { return nil }
func (Builtin) PublishKeys(context.Context, Keys) error       { return nil }
func (Builtin) Bucket() storage.Storage                       { return nil }

// Worker writes the projection into the bucket the Cloudflare Worker reads
// through its R2 binding.
type Worker struct{ Store storage.Storage }

func (w Worker) Publish(ctx context.Context, playbackID string, r Record) error {
	return w.Store.Put(ctx, RouteKey(playbackID), bytes.NewReader(encode(r)))
}

func (w Worker) Unpublish(ctx context.Context, playbackID string) error {
	return w.Store.Delete(ctx, RouteKey(playbackID))
}

func (w Worker) PublishKeys(ctx context.Context, k Keys) error {
	return w.Store.Put(ctx, KeysKey, bytes.NewReader(encode(k)))
}

func (w Worker) Bucket() storage.Storage { return w.Store }

// encode is the exact bytes written, so the reconciler can compare them
// with an object's MD5 ETag. Maps marshal with sorted keys.
func encode(v any) []byte {
	b, _ := json.Marshal(v) // plain structs; can't fail
	return b
}

// ForProject returns the project's delivery adapter.
func ForProject(ctx context.Context, q *db.Queries, set *stores.Set, projectID string) (Delivery, string, error) {
	p, err := q.GetProjectDelivery(ctx, projectID)
	if err != nil {
		return nil, "", fmt.Errorf("project delivery: %w", err)
	}
	if p.Delivery != "worker" {
		return Builtin{}, "", nil
	}
	st, err := set.Get(ctx, p.DeliveryStorageID.String)
	if err != nil {
		return nil, "", err
	}
	return Worker{Store: st}, p.DeliveryStorageID.String, nil
}

// Desired builds the record for a playback ID, or nil when the built-in
// handler would answer 404 (unknown, revoking, revoked, deleted or not yet
// playable). A disabled app's ID keeps a record marked Denied, for its 403.
// bucketID is the Worker's storage config.
func Desired(ctx context.Context, q *db.Queries, playbackID, bucketID string) (*Record, error) {
	rt, err := q.GetRoute(ctx, playbackID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rt.PlaybackStatus != "active" || rt.AssetStatus == "deleted" || !rt.CurrentOutputID.Valid {
		return nil, nil
	}
	outs, err := q.ListServableOutputs(ctx, rt.AssetID)
	if err != nil {
		return nil, err
	}
	r := &Record{
		V: 1, AppID: rt.AppID, AssetID: rt.AssetID, OutputID: rt.CurrentOutputID.String, Policy: rt.Policy,
		Backend: backend(rt.MediaStorageID, bucketID), Outputs: make(map[string]int32, len(outs)), Denied: rt.AppDisabled,
		Share: rt.Share,
	}
	for _, o := range outs {
		r.Outputs[o.ID] = o.KeyVersion
		if o.NoAudio {
			r.NoAudio = append(r.NoAudio, o.ID)
		}
	}
	trks, err := q.ListServableTracks(ctx, rt.AssetID)
	if err != nil {
		return nil, err
	}
	for _, t := range trks {
		if r.Tracks == nil {
			r.Tracks = map[string]Track{}
		}
		r.Tracks[t.ID] = TrackOf(t.Kind, t.Status, t.Language, t.Name, t.DurationMs, t.KeyVersion)
	}
	return r, nil
}

// backend says where the Worker reads an asset's encoded files: from its
// own bucket when they are there, otherwise through origin (D036, D117).
func backend(mediaStorageID, bucketID string) string {
	if mediaStorageID == bucketID {
		return BackendR2
	}
	return BackendOrigin
}

// Apply publishes a playback ID's current state: its record, or its absence.
func Apply(ctx context.Context, q *db.Queries, d Delivery, bucketID, playbackID string) error {
	r, err := Desired(ctx, q, playbackID, bucketID)
	if err != nil {
		return err
	}
	if r == nil {
		return d.Unpublish(ctx, playbackID)
	}
	return d.Publish(ctx, playbackID, *r)
}

// DesiredKeys builds _keys.json from the keys delivery accepts now.
func DesiredKeys(ctx context.Context, q *db.Queries, projectID string) (Keys, error) {
	life, err := q.GetProjectTokenLifetime(ctx, projectID)
	if err != nil {
		return Keys{}, err
	}
	rows, err := q.ListPublishedKeys(ctx, projectID)
	if err != nil {
		return Keys{}, err
	}
	k := Keys{V: 1, MaxTokenLifetimeSeconds: life, Keys: make([]Key, 0, len(rows))}
	for _, row := range rows {
		jwk, err := toJWK(row.PublicKeyPem)
		if err != nil {
			return Keys{}, fmt.Errorf("key %s: %w", row.ID, err)
		}
		key := Key{Kid: row.ID, JWK: jwk}
		if row.AppID.Valid {
			key.AppID = &row.AppID.String
		}
		if row.RetiredAt.Valid {
			t := row.RetiredAt.Time.Unix()
			key.NotAfter = &t
		}
		k.Keys = append(k.Keys, key)
	}
	return k, nil
}

func toJWK(pemText string) (JWK, error) {
	pub, err := tokens.ParsePublicKey(pemText)
	if err != nil {
		return JWK{}, err
	}
	b, err := pub.Bytes() // 0x04 || X || Y
	if err != nil || len(b) != 65 {
		return JWK{}, fmt.Errorf("not a P-256 point: %v", err)
	}
	enc := base64.RawURLEncoding
	return JWK{Kty: "EC", Crv: "P-256", X: enc.EncodeToString(b[1:33]), Y: enc.EncodeToString(b[33:])}, nil
}

// Result counts what a reconcile pass changed.
type Result struct{ Written, Deleted int }

// Reconcile makes the bucket match Postgres: it deletes records for IDs that
// shouldn't be served, rewrites any whose bytes differ (by MD5 ETag) and
// writes missing ones, then rewrites _keys.json. It repairs drift a lost
// outbox row or a manual edit could cause.
func Reconcile(ctx context.Context, q *db.Queries, d Delivery, bucketID, projectID string) (Result, error) {
	var res Result
	bucket := d.Bucket()
	if bucket == nil {
		return res, nil
	}
	want, err := desiredAll(ctx, q, projectID, bucketID)
	if err != nil {
		return res, err
	}
	seen := make(map[string]bool, len(want))
	for cursor := ""; ; {
		objs, next, err := bucket.List(ctx, "_routes/", cursor)
		if err != nil {
			return res, err
		}
		for _, o := range objs {
			pid, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, "_routes/"), ".json")
			body, wanted := want[pid]
			switch {
			case !ok || !wanted:
				if err := bucket.Delete(ctx, o.Key); err != nil {
					return res, err
				}
				res.Deleted++
			case strings.Trim(o.ETag, `"`) != md5hex(body):
				if err := bucket.Put(ctx, o.Key, bytes.NewReader(body)); err != nil {
					return res, err
				}
				res.Written++
			}
			seen[pid] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for pid, body := range want {
		if seen[pid] {
			continue
		}
		if err := bucket.Put(ctx, RouteKey(pid), bytes.NewReader(body)); err != nil {
			return res, err
		}
		res.Written++
	}
	keys, err := DesiredKeys(ctx, q, projectID)
	if err != nil {
		return res, err
	}
	return res, d.PublishKeys(ctx, keys)
}

// desiredAll builds every record of the project in two queries.
func desiredAll(ctx context.Context, q *db.Queries, projectID, bucketID string) (map[string][]byte, error) {
	routes, err := q.ListServableRoutes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	outs, err := q.ListServableOutputsForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	byAsset := map[string]map[string]int32{}
	silent := map[string][]string{}
	for _, o := range outs {
		if byAsset[o.AssetID] == nil {
			byAsset[o.AssetID] = map[string]int32{}
		}
		byAsset[o.AssetID][o.ID] = o.KeyVersion
		if o.NoAudio {
			silent[o.AssetID] = append(silent[o.AssetID], o.ID)
		}
	}
	trks, err := q.ListServableTracksForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tracksOf := map[string]map[string]Track{}
	for _, t := range trks {
		if tracksOf[t.AssetID] == nil {
			tracksOf[t.AssetID] = map[string]Track{}
		}
		tracksOf[t.AssetID][t.ID] = TrackOf(t.Kind, t.Status, t.Language, t.Name, t.DurationMs, t.KeyVersion)
	}
	want := make(map[string][]byte, len(routes))
	for _, rt := range routes {
		outputs := byAsset[rt.AssetID]
		if outputs == nil {
			outputs = map[string]int32{}
		}
		want[rt.PlaybackID] = encode(Record{
			V: 1, AppID: rt.AppID, AssetID: rt.AssetID, OutputID: rt.CurrentOutputID.String, Policy: rt.Policy,
			Backend: backend(rt.MediaStorageID, bucketID), Outputs: outputs, Denied: rt.AppDisabled,
			Tracks: tracksOf[rt.AssetID], NoAudio: silent[rt.AssetID], Share: rt.Share,
		})
	}
	return want, nil
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// ReconcileProject runs Reconcile for a project through its current
// delivery adapter; with built-in delivery there is nothing to do.
func ReconcileProject(ctx context.Context, q *db.Queries, set *stores.Set, projectID string) (int, error) {
	d, bucketID, err := ForProject(ctx, q, set, projectID)
	if err != nil {
		return 0, err
	}
	res, err := Reconcile(ctx, q, d, bucketID, projectID)
	return res.Written + res.Deleted, err
}
