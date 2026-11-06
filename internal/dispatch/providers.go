package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/publicnet"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Provider settings shared by every Connect (D056). The provider-specific
// steps (checking credentials, registry access) live in its own package.

// Settings are a provider's limits and machine size, as Connect saves them.
type Settings struct {
	MaxConcurrent     int32
	MonthlyLimitCents int64
	RateCentsPerHour  int32
	DiskBytes         int64
}

// Valid reports what is wrong with s, if anything.
func (s Settings) Valid() error {
	switch {
	case s.MaxConcurrent < 1 || s.MaxConcurrent > 100:
		return errors.New("max_concurrent must be between 1 and 100")
	case s.MonthlyLimitCents < 1:
		return errors.New("monthly_limit_cents must be set: a provider without a spend limit isn't connected")
	case s.RateCentsPerHour < 0:
		return errors.New("rate_cents_per_hour can't be negative")
	case s.DiskBytes < 20<<30:
		return errors.New("each machine needs at least 20 GB of disk")
	}
	return nil
}

// Existing returns a provider's stored row, if it was ever connected.
func Existing(ctx context.Context, pool *pgxpool.Pool, projectID, kind string) (db.ExecutorConfig, bool, error) {
	row, err := db.New(pool).GetExecutorConfigByKind(ctx, db.GetExecutorConfigByKindParams{ProjectID: projectID, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ExecutorConfig{}, false, nil
	}
	return row, err == nil, err
}

// Save stores a provider's config and sealed secret, connecting it or
// connecting it again (which rotates the secret). Either way it must pass a
// setup test before it takes work.
func Save(ctx context.Context, pool *pgxpool.Pool, box *secrets.Box, projectID, kind string, config any, secret any, s Settings) (db.ExecutorConfig, error) {
	cfg, err := json.Marshal(config)
	if err != nil {
		return db.ExecutorConfig{}, err
	}
	plain, err := json.Marshal(secret)
	if err != nil {
		return db.ExecutorConfig{}, err
	}
	// The row ID is part of the sealed value's associated data (D017), so
	// a reconnect seals for the row it will update.
	id := ids.New(ids.Executor)
	if row, ok, err := Existing(ctx, pool, projectID, kind); err != nil {
		return db.ExecutorConfig{}, err
	} else if ok {
		id = row.ID
	}
	return db.New(pool).UpsertExecutorConfig(ctx, db.UpsertExecutorConfigParams{
		ID: id, ProjectID: projectID, Kind: kind, Config: cfg,
		SecretsEncrypted: box.Seal("executor_configs", id, plain),
		MaxConcurrent:    s.MaxConcurrent, MonthlyLimitCents: s.MonthlyLimitCents,
		RateCentsPerHour: s.RateCentsPerHour, DiskBytes: s.DiskBytes,
	})
}

// Status is a provider as the dashboard shows it.
type Status struct {
	Kind              string          `json:"kind"`
	Connected         bool            `json:"connected"`
	Verified          bool            `json:"verified"`
	VerifiedAt        *time.Time      `json:"verified_at,omitempty"`
	Config            json.RawMessage `json:"config"`
	MaxConcurrent     int32           `json:"max_concurrent"`
	MonthlyLimitCents int64           `json:"monthly_limit_cents"`
	RateCentsPerHour  int32           `json:"rate_cents_per_hour"`
	DiskBytes         int64           `json:"disk_bytes"`
	MonthCents        float64         `json:"month_cents"`
	Machines          int64           `json:"machines"`
	BackoffUntil      *time.Time      `json:"backoff_until,omitempty"`
}

// Statuses lists every provider ever connected.
func Statuses(ctx context.Context, pool *pgxpool.Pool, projectID string) ([]Status, error) {
	rows, err := db.New(pool).ExecutorStatus(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(rows))
	for _, r := range rows {
		st := Status{
			Kind: r.Kind, Connected: r.Connected, Verified: r.Connected && r.VerifiedAt.Valid, Config: r.Config,
			MaxConcurrent: r.MaxConcurrent, MonthlyLimitCents: r.MonthlyLimitCents, RateCentsPerHour: r.RateCentsPerHour,
			DiskBytes: r.DiskBytes, MonthCents: r.MonthCents, Machines: r.Machines,
			VerifiedAt: optTime(r.VerifiedAt),
		}
		if r.BackoffUntil.Valid && r.BackoffUntil.Time.After(time.Now()) {
			st.BackoffUntil = &r.BackoffUntil.Time
		}
		out = append(out, st)
	}
	return out, nil
}

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// Disconnect refusals.
var (
	// ErrInUse: routing names the provider as primary or fallback.
	ErrInUse = errors.New("dispatch: the provider is the primary or the fallback")
	// ErrMachinesRunning: a dispatch is live or a machine isn't stopped.
	ErrMachinesRunning = errors.New("dispatch: the provider still runs machines")
	// ErrNotConnected: the provider isn't connected.
	ErrNotConnected = errors.New("dispatch: the provider isn't connected")
)

// Disconnect forgets a provider's credentials, keeping its row as spend
// history. It refuses while routing uses the provider or while it may run
// a machine, since nothing could stop that machine afterwards. It returns
// the row as it was, so the provider's package can clean up on its side.
func Disconnect(ctx context.Context, pool *pgxpool.Pool, projectID, kind string) (db.ExecutorConfig, error) {
	var was db.ExecutorConfig
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		qs := db.New(tx)
		row, err := qs.LockExecutorConfigByKind(ctx, db.LockExecutorConfigByKindParams{ProjectID: projectID, Kind: kind})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(row.SecretsEncrypted) == 0) {
			return ErrNotConnected
		}
		if err != nil {
			return err
		}
		routing, err := qs.GetRouting(ctx, projectID)
		if err != nil {
			return err
		}
		if routing.PrimaryExecutor == kind || routing.FallbackExecutor.String == kind {
			return ErrInUse
		}
		statuses, err := qs.ExecutorStatus(ctx, projectID)
		if err != nil {
			return err
		}
		for _, st := range statuses {
			if st.ID == row.ID && st.Machines > 0 {
				return ErrMachinesRunning
			}
		}
		was = row
		return qs.DisconnectExecutor(ctx, row.ID)
	})
	return was, err
}

// CheckPublicURL refuses a public URL a provider's machines couldn't reach:
// a private control plane can't use Modal or RunPod (architecture
// "Compatibility"). Its host must resolve only to public addresses. This
// is stricter than API keys need (settings.KeysAllowed accepts LAN
// addresses, for apps on the same network).
func CheckPublicURL(ctx context.Context, publicURL string, resolver *net.Resolver) error {
	u, err := url.Parse(publicURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("the public URL %q has no host", publicURL)
	}
	host := u.Hostname()
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else if addrs, err = resolver.LookupNetIP(ctx, "ip", host); err != nil {
		return fmt.Errorf("the public URL's host %s doesn't resolve: %w", host, err)
	}
	for _, a := range addrs {
		if !publicnet.Public(a) {
			return fmt.Errorf("the public URL's host %s is a private address (%s): a provider's machines couldn't reach the worker API", host, a)
		}
	}
	return nil
}

// Step is one step of a provider's Connect. Details never hold a key, a
// token or a password.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Result is a whole Connect run.
type Result struct {
	Connected bool   `json:"connected"`
	Steps     []Step `json:"steps"`
}

// Steps runs a Connect's steps in order, stopping at the first that fails:
// each step is safe to repeat, so the owner fixes what failed and connects
// again.
type Steps struct {
	Result   Result
	Provider string
	Logger   *slog.Logger
	failed   bool
}

// Do runs one step unless an earlier one failed, and reports whether every
// step so far succeeded. fn returns the step's detail.
func (s *Steps) Do(name string, fn func() (string, error)) bool {
	if s.failed {
		return false
	}
	detail, err := fn()
	st := Step{Name: name, OK: err == nil, Detail: detail}
	if err != nil {
		st.Detail = err.Error()
		s.failed = true
		s.Logger.Warn("connect "+s.Provider, "step", name, "err", err)
	}
	s.Result.Steps = append(s.Result.Steps, st)
	s.Result.Connected = !s.failed
	return !s.failed
}

// PinImage resolves a provider's worker image to its digest with the
// registry credentials its machines will pull with (D058). It proves those
// credentials can pull before any machine is woken, and pins the image so
// every machine runs exactly what was connected, even if the tag moves.
func PinImage(ctx context.Context, r *registry.Resolver, image, username, password string) (pinned, detail string, err error) {
	ref, err := r.Resolve(ctx, image, username, password)
	if errors.Is(err, registry.ErrDenied) {
		if username == "" {
			return "", "", fmt.Errorf("%s can't be pulled anonymously: give registry credentials that can read it", image)
		}
		return "", "", fmt.Errorf("%s can't be pulled with these registry credentials", image)
	}
	if err != nil {
		return "", "", err
	}
	return ref.Pinned(), "pinned to " + ref.Digest, nil
}
