// Package settings holds the project settings the owner chooses at first
// boot (docs/architecture.md, "First boot"): the public URL that browsers
// and workers reach the app at, and the media URL playback is served from.
// They live in Postgres; a change applies at once, without a restart.
package settings

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/db"
)

// ErrPinned means the environment sets the URLs (CLIPMUX_PUBLIC_URL).
var ErrPinned = errors.New("settings: the URLs are set by CLIPMUX_PUBLIC_URL and CLIPMUX_MEDIA_URL")

// URLs are the project's origins.
type URLs struct {
	Public string `json:"public_url"`
	Media  string `json:"media_url"`
	// Confirmed is false until the owner (or the environment) sets them.
	Confirmed bool `json:"confirmed"`
	// Pinned means the environment sets them and they can't be changed here.
	Pinned bool `json:"pinned"`
}

// Store is the current settings of one project.
type Store struct {
	pool      *pgxpool.Pool
	projectID string
	pinned    bool
	cur       atomic.Pointer[URLs]

	mu    sync.Mutex // serialises Set and its hooks
	hooks []func(URLs)
}

// Load reads the project's URLs. When the environment pins them they are
// written to the database first, so base_url, which is read from there,
// matches; otherwise a first boot stores the unconfirmed defaults.
func Load(ctx context.Context, pool *pgxpool.Pool, projectID string, cfg config.Config) (*Store, error) {
	q := db.New(pool)
	var err error
	if cfg.URLsPinned {
		err = q.SetProjectURLs(ctx, db.SetProjectURLsParams{ID: projectID, PublicUrl: text(cfg.PublicURL), MediaUrl: text(cfg.MediaURL)})
	} else {
		err = q.InitProjectURLs(ctx, db.InitProjectURLsParams{ID: projectID, PublicUrl: text(cfg.PublicURL), MediaUrl: text(cfg.MediaURL)})
	}
	if err != nil {
		return nil, fmt.Errorf("store project URLs: %w", err)
	}
	row, err := q.GetProjectURLs(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project URLs: %w", err)
	}
	s := &Store{pool: pool, projectID: projectID, pinned: cfg.URLsPinned}
	s.cur.Store(&URLs{Public: row.PublicUrl.String, Media: row.MediaUrl.String, Confirmed: row.UrlsConfirmedAt.Valid, Pinned: cfg.URLsPinned})
	return s, nil
}

// URLs returns the current settings.
func (s *Store) URLs() URLs { return *s.cur.Load() }

// PublicURL is where browsers and workers reach the app.
func (s *Store) PublicURL() string { return s.cur.Load().Public }

// MediaURL is where playback is served from.
func (s *Store) MediaURL() string { return s.cur.Load().Media }

// KeysAllowed reports whether API keys may be created yet.
func (s *Store) KeysAllowed() bool { return KeysAllowed(s.PublicURL()) }

// OnChange registers fn to run after every successful Set, for the parts
// that keep a URL in memory (filesystem storage signs with the public URL).
func (s *Store) OnChange(fn func(URLs)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = append(s.hooks, fn)
}

// Set saves new URLs; an empty media URL means the public URL.
func (s *Store) Set(ctx context.Context, public, media string) (URLs, error) {
	if s.pinned {
		return URLs{}, ErrPinned
	}
	p, err := config.Origin(public)
	if err != nil {
		return URLs{}, fmt.Errorf("public URL: %w", err)
	}
	m := p
	if media != "" {
		if m, err = config.Origin(media); err != nil {
			return URLs{}, fmt.Errorf("media URL: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := db.New(s.pool).SetProjectURLs(ctx, db.SetProjectURLsParams{ID: s.projectID, PublicUrl: text(p), MediaUrl: text(m)}); err != nil {
		return URLs{}, fmt.Errorf("save project URLs: %w", err)
	}
	u := URLs{Public: p, Media: m, Confirmed: true}
	s.cur.Store(&u)
	for _, fn := range s.hooks {
		fn(u)
	}
	return u, nil
}

// ServesHost reports whether host (no port) is the public or media URL's
// hostname, so Caddy may obtain a certificate for it. Loopback names and IP
// addresses never get one.
func (s *Store) ServesHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || loopback(host) || net.ParseIP(host) != nil {
		return false
	}
	u := s.URLs()
	for _, raw := range []string{u.Public, u.Media} {
		if parsed, err := url.Parse(raw); err == nil && strings.EqualFold(parsed.Hostname(), host) {
			return true
		}
	}
	return false
}

// KeysAllowed reports whether publicURL is reachable from outside this
// machine. While it is loopback, signed storage URLs and base_url would point
// at the owner's own machine, so no API key may be created.
func KeysAllowed(publicURL string) bool {
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return !loopback(strings.ToLower(u.Hostname()))
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
