package delivery

import (
	"context"
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
	outputs map[string]cached[db.Output]
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
	c.outputs = map[string]cached[db.Output]{}
}

// lookup returns a cached value, or loads it and keeps it unless an
// invalidation happened meanwhile. A load that fails isn't kept.
func lookup[T any](c *cache, m func() map[string]cached[T], id string, load func() (T, error)) (T, error) {
	c.mu.Lock()
	e, ok := m()[id]
	gen := c.gen
	c.mu.Unlock()
	if ok && c.now().Before(e.until) {
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
	return lookup(c, func() map[string]cached[db.GetRouteRow] { return c.routes }, pid,
		func() (db.GetRouteRow, error) { return q.GetRoute(ctx, pid) })
}

func (c *cache) signingKey(ctx context.Context, q *db.Queries, kid string) (db.SigningKey, error) {
	return lookup(c, func() map[string]cached[db.SigningKey] { return c.keys }, kid,
		func() (db.SigningKey, error) { return q.GetSigningKey(ctx, kid) })
}

func (c *cache) output(ctx context.Context, q *db.Queries, id string) (db.Output, error) {
	return lookup(c, func() map[string]cached[db.Output] { return c.outputs }, id,
		func() (db.Output, error) { return q.GetOutput(ctx, id) })
}

// Invalidate drops the routes of these playback IDs, and every output:
// a publication may follow a commit, a retirement or a purge.
func (h *Handler) Invalidate(pids ...string) {
	c := h.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for _, p := range pids {
		delete(c.routes, p)
	}
	c.outputs = map[string]cached[db.Output]{}
}

// InvalidateKeys drops every signing key, after _keys.json changed.
func (h *Handler) InvalidateKeys() {
	c := h.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.keys = map[string]cached[db.SigningKey]{}
}
