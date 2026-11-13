package modal

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Connect Modal (architecture "Optional upgrades"; D057): a token, the
// image and its registry access, a concurrency limit and a monthly spend
// limit; then a setup test whose sample must encode in a Modal sandbox
// before Modal takes any other work.

// Request is what the owner enters.
type Request struct {
	TokenID     string `json:"token_id"`
	TokenSecret string `json:"token_secret"`
	Environment string `json:"environment"`
	// Image defaults to the worker image this server was built with.
	Image            string  `json:"image"`
	RegistryUsername string  `json:"registry_username"`
	RegistryPassword string  `json:"registry_password"`
	GPU              string  `json:"gpu"`
	CPU              float64 `json:"cpu"`
	MemoryMiB        int     `json:"memory_mib"`
	DiskGB           int     `json:"disk_gb"`
	MaxConcurrent    int32   `json:"max_concurrent"`
	// MonthlyLimitCents and RateCentsPerHour bound the estimated spend
	// (D055); the rate is what Modal charges per sandbox-hour for this GPU,
	// CPU and memory.
	MonthlyLimitCents int64 `json:"monthly_limit_cents"`
	RateCentsPerHour  int32 `json:"rate_cents_per_hour"`
}

// ErrInvalid wraps a request that can't be tried.
var ErrInvalid = errors.New("modal: invalid request")

// Connector runs Connect Modal and opens executors for stored configs.
type Connector struct {
	Pool      *pgxpool.Pool
	Box       *secrets.Box
	ProjectID string
	// PublicURL is the URL sandboxes reach the worker API on.
	PublicURL func() string
	// AllowPrivateURL skips the public-URL check (development only).
	AllowPrivateURL bool
	// DefaultImage is the worker image this build pairs with (T06).
	DefaultImage string
	// Registry resolves the image's digest; nil means a default resolver.
	Registry *registry.Resolver
	// SetupTest queues the setup test through Modal.
	SetupTest func(ctx context.Context) error
	Resolver  *net.Resolver
	Logger    *slog.Logger

	// newAPI opens Modal with a token; tests replace it.
	newAPI func(tokenID, tokenSecret, environment string) (checkedAPI, error)
	// flows is Modal's browser approval; nil means Modal's API (D109).
	flows flows

	mu    sync.Mutex
	cache map[string]cached
}

// checkedAPI is api plus what Connect and the cache need.
type checkedAPI interface {
	api
	check(ctx context.Context, app string) error
	// prepare starts Modal converting the request's image and reports
	// whether it is ready, after a bounded wait.
	prepare(ctx context.Context, req sandboxRequest) (bool, error)
	close()
}

type cached struct {
	updated time.Time
	ex      *Executor
	api     checkedAPI
}

func (c *Connector) open(tokenID, tokenSecret, environment string) (checkedAPI, error) {
	if c.newAPI != nil {
		return c.newAPI(tokenID, tokenSecret, environment)
	}
	return newSDK(tokenID, tokenSecret, environment)
}

var imageRef = regexp.MustCompile(`^[a-z0-9]+([._/:@-][a-zA-Z0-9_.-]+)+$`)

// CheckSettings refuses a request whose settings Connect would refuse,
// before a browser approval issues a token for it (D109).
func (c *Connector) CheckSettings(req Request) error {
	_, _, err := c.settings(req)
	return err
}

// settings fills req's defaults and checks everything but the token.
func (c *Connector) settings(req Request) (Request, dispatch.Settings, error) {
	if req.Image == "" {
		req.Image = c.DefaultImage
	}
	if req.GPU == "" {
		req.GPU = DefaultGPU
	}
	if req.CPU == 0 {
		req.CPU = 4
	}
	if req.MemoryMiB == 0 {
		req.MemoryMiB = 16384
	}
	if req.DiskGB == 0 {
		req.DiskGB = 50
	}
	if req.MaxConcurrent == 0 {
		req.MaxConcurrent = 1
	}
	if req.MonthlyLimitCents == 0 {
		req.MonthlyLimitCents = dispatch.DefaultMonthlyLimitCents
	}
	rate, err := rateFor(req.GPU, req.CPU, req.MemoryMiB)
	if err != nil {
		return req, dispatch.Settings{}, err
	}
	if req.RateCentsPerHour == 0 {
		req.RateCentsPerHour = rate
	}
	settings := dispatch.Settings{
		MaxConcurrent: req.MaxConcurrent, MonthlyLimitCents: req.MonthlyLimitCents,
		RateCentsPerHour: req.RateCentsPerHour, DiskBytes: int64(req.DiskGB) << 30,
	}
	switch {
	case req.Image == "":
		return req, settings, fmt.Errorf("%w: this server has no default worker image (it was not built by CI); set CLIPMUX_WORKER_IMAGE or enter an image under Advanced", ErrInvalid)
	case !imageRef.MatchString(req.Image):
		return req, settings, fmt.Errorf("%w: image must be an image reference such as ghcr.io/owner/clipmux:tag", ErrInvalid)
	case (req.RegistryUsername == "") != (req.RegistryPassword == ""):
		return req, settings, fmt.Errorf("%w: registry_username and registry_password go together", ErrInvalid)
	case req.CPU < 1 || req.CPU > 64 || req.MemoryMiB < 4096 || req.MemoryMiB > 262144:
		return req, settings, fmt.Errorf("%w: cpu must be 1 to 64 cores and memory_mib 4096 to 262144", ErrInvalid)
	case req.DiskGB > 2000:
		return req, settings, fmt.Errorf("%w: disk_gb must be at most 2000", ErrInvalid)
	}
	if err := settings.Valid(); err != nil {
		return req, settings, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return req, settings, nil
}

// Connect runs the steps in order and stops at the first that fails. Each
// step is safe to repeat, so connecting again rotates the token or changes
// the settings.
func (c *Connector) Connect(ctx context.Context, req Request) (dispatch.Result, error) {
	req.TokenID, req.TokenSecret = strings.TrimSpace(req.TokenID), strings.TrimSpace(req.TokenSecret)
	if req.TokenID == "" || req.TokenSecret == "" {
		return dispatch.Result{}, fmt.Errorf("%w: token_id and token_secret are required", ErrInvalid)
	}
	chosen := req.Image != ""
	req, settings, err := c.settings(req)
	if err != nil {
		return dispatch.Result{}, err
	}
	existing, had, err := dispatch.Existing(ctx, c.Pool, c.ProjectID, "modal")
	if err != nil {
		return dispatch.Result{}, err
	}
	var old Config
	if had {
		_ = json.Unmarshal(existing.Config, &old)
	}
	cfg := Config{Image: req.Image, ImageChosen: chosen, GPU: req.GPU, CPU: req.CPU, MemoryMiB: req.MemoryMiB, Tag: old.Tag, Environment: req.Environment}
	if cfg.Tag == "" {
		cfg.Tag = strings.ToLower(rand.Text()[:8])
	}

	steps := &dispatch.Steps{Provider: "modal", Logger: c.Logger}
	steps.Do("credentials", func() (string, error) {
		a, err := c.open(req.TokenID, req.TokenSecret, req.Environment)
		if err != nil {
			return "", err
		}
		defer a.close()
		if err := a.check(ctx, "clipmux-"+cfg.Tag); err != nil {
			return "", fmt.Errorf("modal refused the token: %w", redact(err, req.TokenSecret))
		}
		return "the token works; sandboxes run in the app clipmux-" + cfg.Tag, nil
	})
	steps.Do("public_url", func() (string, error) {
		if c.AllowPrivateURL {
			return "skipped (CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL)", nil
		}
		if err := dispatch.CheckPublicURL(ctx, c.PublicURL(), c.Resolver); err != nil {
			return "", err
		}
		return "sandboxes can reach " + c.PublicURL(), nil
	})
	steps.Do("image", func() (string, error) {
		pinned, detail, err := dispatch.PinImage(ctx, c.registry(), req.Image, req.RegistryUsername, req.RegistryPassword)
		cfg.Image = pinned
		return detail, err
	})
	steps.Do("image_conversion", func() (string, error) {
		a, err := c.open(req.TokenID, req.TokenSecret, req.Environment)
		if err != nil {
			return "", err
		}
		defer a.close()
		ready, err := a.prepare(ctx, sandboxRequest{App: "clipmux-" + cfg.Tag, Image: cfg.Image,
			RegistryUsername: req.RegistryUsername, RegistryPassword: req.RegistryPassword})
		if err != nil {
			return "", redact(err, req.TokenSecret)
		}
		if !ready {
			return "Modal is converting the image, which takes minutes the first time; the setup test waits for it", nil
		}
		return "Modal has the image", nil
	})
	steps.Do("saved", func() (string, error) {
		sec := Secret{TokenID: req.TokenID, TokenSecret: req.TokenSecret, RegistryUsername: req.RegistryUsername, RegistryPassword: req.RegistryPassword}
		if _, err := dispatch.Save(ctx, c.Pool, c.Box, c.ProjectID, "modal", cfg, sec, settings); err != nil {
			return "", err
		}
		return "Modal encodes new videos once a sample encodes on it", nil
	})
	if c.SetupTest != nil {
		steps.Do("setup_test", func() (string, error) {
			if err := c.SetupTest(ctx); err != nil {
				return "", err
			}
			return "queued: a Modal sandbox must encode and play the sample", nil
		})
	}
	return steps.Result, nil
}

// redact removes a secret from an error's text, in case Modal echoes it.
func redact(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

// Disconnect forgets Modal's token (dispatch.Disconnect refuses while
// routing uses it or a sandbox may still run).
func (c *Connector) Disconnect(ctx context.Context) error {
	_, err := dispatch.Disconnect(ctx, c.Pool, c.ProjectID, "modal")
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.cache {
		e.api.close()
		delete(c.cache, id)
	}
	return err
}

// Resolve opens the executor for a stored config: dispatch.Service.Resolve
// for Modal rows. One client is kept per row and replaced when the row
// changes (a reconnect), because each holds a gRPC connection.
func (c *Connector) Resolve(row db.ExecutorConfig) (dispatch.Executor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.cache[row.ID]; ok && e.updated.Equal(row.UpdatedAt.Time) {
		return e.ex, nil
	}
	cfg, sec, err := open(row, c.Box)
	if err != nil {
		return nil, err
	}
	a, err := c.open(sec.TokenID, sec.TokenSecret, cfg.Environment)
	if err != nil {
		return nil, err
	}
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	if old, ok := c.cache[row.ID]; ok {
		old.api.close()
	}
	ex := &Executor{api: a, cfg: cfg, secret: sec}
	c.cache[row.ID] = cached{updated: row.UpdatedAt.Time, ex: ex, api: a}
	return ex, nil
}

// Repin moves Modal to this build's worker image after an upgrade
// (dispatch.Repin), then starts Modal converting it, so the next job doesn't
// wait minutes for the conversion. It is a sweeper task: it returns how
// many providers moved.
func (c *Connector) Repin(ctx context.Context) (int, error) {
	row, ok, err := dispatch.Existing(ctx, c.Pool, c.ProjectID, "modal")
	if err != nil || !ok || len(row.SecretsEncrypted) == 0 || c.DefaultImage == "" {
		return 0, err
	}
	cfg, sec, err := open(row, c.Box)
	if err != nil {
		return 0, err
	}
	pinned, err := dispatch.Repin(ctx, c.Pool, c.registry(), row, c.DefaultImage, sec.RegistryUsername, sec.RegistryPassword)
	if err != nil || pinned == "" {
		return 0, err
	}
	c.Logger.Info("re-pinned the worker image to this build's", "provider", "modal", "image", pinned)
	a, err := c.open(sec.TokenID, sec.TokenSecret, cfg.Environment)
	if err != nil {
		return 1, err
	}
	defer a.close()
	if _, err := a.prepare(ctx, sandboxRequest{App: "clipmux-" + cfg.Tag, Image: pinned,
		RegistryUsername: sec.RegistryUsername, RegistryPassword: sec.RegistryPassword}); err != nil {
		return 1, fmt.Errorf("modal: convert the new image: %w", redact(err, sec.TokenSecret))
	}
	return 1, nil
}

func (c *Connector) registry() *registry.Resolver {
	if c.Registry != nil {
		return c.Registry
	}
	return registry.NewResolver()
}
