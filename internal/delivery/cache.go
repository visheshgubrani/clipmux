package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// cache keeps what every segment request reads, so a request costs no
// query (D072): each playback ID's route, each signing key, and each served
// output. Entries live RouteTTL (UnknownTTL for an ID that doesn't exist),
// and every change the Cloudflare Worker would have to see drops them at
// once, because it already goes through route publication: Invalidate runs
// when route_sync rows are confirmed, InvalidateKeys when _keys.json is.
// So on one node a revocation still takes effect immediately; the TTL only
// bounds what nothing published (another replica's changes).
type cache struct {
	now func() time.Time
	mu  sync.Mutex
	// gen moves on every invalidation; a load that started before one isn't
	// stored, so a slow read can't put back what was just dropped.
	gen     uint64
	routes  map[string]cached[db.GetRouteRow]
	keys    map[string]cached[db.SigningKey]
	outputs map[string]cached[servedOutput]
	// tracks are each asset's servable tracks, by asset ID.
	tracks map[string]cached[[]db.ListServableTracksRow]
	// onLookup, when set, is told each lookup's table and whether it hit
	// (clipmux_delivery_cache_lookups_total).
	onLookup func(table string, hit bool)
}

type cached[T any] struct {
	v     T
	found bool
	until time.Time
}

// TTLs and the cache's bound.
const (
	RouteTTL   = 30 * time.Second
	UnknownTTL = 5 * time.Second
	cacheMax   = 100_000
)

func newCache(now func() time.Time) *cache {
	c := &cache{now: now}
	c.clear()
	return c
}

func (c *cache) clear() {
	c.routes = map[string]cached[db.GetRouteRow]{}
	c.keys = map[string]cached[db.SigningKey]{}
	c.outputs = map[string]cached[servedOutput]{}
	c.tracks = map[string]cached[[]db.ListServableTracksRow]{}
}

// lookup returns a cached value, or loads it and keeps it unless an
// invalidation happened meanwhile. A load that fails isn't kept.
func lookup[T any](c *cache, table string, m func() map[string]cached[T], id string, load func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := m()[id]
	gen := c.gen
	report := c.onLookup
	c.mu.Unlock()
	hit := ok && c.now().Before(e.until)
	if report != nil {
		report(table, hit)
	}
	if hit {
		if !e.found {
			var zero T
			return zero, pgx.ErrNoRows
		}
		return e.v, nil
	}
	v, err := load()
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	ttl := RouteTTL
	if !found {
		ttl = UnknownTTL
	}
	c.mu.Lock()
	if c.gen == gen {
		if len(m()) >= cacheMax {
			c.clear()
		}
		m()[id] = cached[T]{v: v, found: found, until: c.now().Add(ttl)}
	}
	c.mu.Unlock()
	return v, err
}

func (c *cache) route(ctx context.Context, q *db.Queries, pid string) (db.GetRouteRow, error) {
	return lookup(c, "route", func() map[string]cached[db.GetRouteRow] { return c.routes }, pid,
		func() (db.GetRouteRow, error) { return q.GetRoute(ctx, pid) })
}

func (c *cache) signingKey(ctx context.Context, q *db.Queries, kid string) (db.SigningKey, error) {
	return lookup(c, "signing_key", func() map[string]cached[db.SigningKey] { return c.keys }, kid,
		func() (db.SigningKey, error) { return q.GetSigningKey(ctx, kid) })
}

// servedOutput is an output and the files its manifest lists, which are
// all delivery serves of it (D124).
type servedOutput struct {
	db.Output
	files map[string]bool
}

// lists reports whether the output's manifest lists rel.
func (o servedOutput) lists(rel string) bool { return o.files[rel] }

func (c *cache) output(ctx context.Context, q *db.Queries, id string) (servedOutput, error) {
	return lookup(c, "output", func() map[string]cached[servedOutput] { return c.outputs }, id,
		func() (servedOutput, error) {
			out, err := q.GetOutput(ctx, id)
			if err != nil {
				return servedOutput{}, err
			}
			// A writing output's manifest is empty, and a committed one was
			// verified; one that doesn't parse lists nothing.
			var m struct {
				Files []struct {
					Path string `json:"path"`
				} `json:"files"`
			}
			_ = json.Unmarshal(out.Manifest, &m)
			files := make(map[string]bool, len(m.Files))
			for _, f := range m.Files {
				files[f.Path] = true
			}
			return servedOutput{Output: out, files: files}, nil
		})
}

// tracksOf returns an asset's servable tracks, in ID order. An asset with
// none is cached as found and empty.
func (c *cache) tracksOf(ctx context.Context, q *db.Queries, assetID string) ([]db.ListServableTracksRow, error) {
	return lookup(c, "tracks", func() map[string]cached[[]db.ListServableTracksRow] { return c.tracks }, assetID,
		func() ([]db.ListServableTracksRow, error) { return q.ListServableTracks(ctx, assetID) })
}

// Invalidate drops the routes of these playback IDs, and every output and
// track list: a publication may follow a commit, a retirement, a purge or
// a track stored or made ready.
func (h *Handler) Invalidate(pids ...string) {
	c := h.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for _, p := range pids {
		delete(c.routes, p)
	}
	c.outputs = map[string]cached[servedOutput]{}
	c.tracks = map[string]cached[[]db.ListServableTracksRow]{}
}

// InvalidateKeys drops every signing key, after _keys.json changed.
func (h *Handler) InvalidateKeys() {
	c := h.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.keys = map[string]cached[db.SigningKey]{}
}

// OnCacheLookup reports every cache lookup, by table and whether it hit.
func (h *Handler) OnCacheLookup(fn func(table string, hit bool)) {
	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()
	h.cache.onLookup = fn
}
