package runpod

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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Connect RunPod (architecture "Optional upgrades"; D056): provider
// credentials, the image and its registry access, a concurrency limit and
// a monthly spend limit; then a setup test whose sample must encode on a
// RunPod pod before RunPod takes any other work.

// Request is what the owner enters.
type Request struct {
	APIKey string `json:"api_key"`
	// Image defaults to the worker image this server was built with.
	Image            string   `json:"image"`
	RegistryUsername string   `json:"registry_username"`
	RegistryPassword string   `json:"registry_password"`
	GPUTypeIDs       []string `json:"gpu_type_ids"`
	DiskGB           int      `json:"disk_gb"`
	MaxConcurrent    int32    `json:"max_concurrent"`
	// MonthlyLimitCents and RateCentsPerHour bound the estimated spend
	// (D055); the rate is what RunPod charges per pod-hour for these GPUs.
	MonthlyLimitCents int64 `json:"monthly_limit_cents"`
	RateCentsPerHour  int32 `json:"rate_cents_per_hour"`
}

// ErrInvalid wraps a request that can't be tried.
var ErrInvalid = errors.New("runpod: invalid request")

// Connector runs Connect RunPod.
type Connector struct {
	Pool      *pgxpool.Pool
	Box       *secrets.Box
	ProjectID string
	// API is RunPod's REST API base (DefaultAPI, or a fake).
	API string
	// GraphQL and Console are where browser approval runs (D109), and
	// Catalog where GPU prices come from (D111); empty means the defaults.
	GraphQL, Console, Catalog string
	// PublicURL is the URL pods reach the worker API on.
	PublicURL func() string
	// AllowPrivateURL skips the public-URL check, for development and the
	// exit test, whose pods run on the same machine.
	AllowPrivateURL bool
	// DefaultImage is the worker image this build pairs with (T06).
	DefaultImage string
	// Registry resolves the image's digest; nil means a default resolver.
	Registry *registry.Resolver
	// SetupTest queues the setup test through RunPod.
	SetupTest func(ctx context.Context) error
	Resolver  *net.Resolver
	Logger    *slog.Logger
}

var imageRef = regexp.MustCompile(`^[a-z0-9]+([._/:@-][a-zA-Z0-9_.-]+)+$`)

// CheckSettings refuses a request whose settings Connect would refuse,
// before a browser approval issues a key for it (D109).
func (c *Connector) CheckSettings(req Request) error {
	_, _, err := c.settings(req)
	return err
}

// settings fills req's defaults and checks everything but the key.
func (c *Connector) settings(req Request) (Request, dispatch.Settings, error) {
	if req.Image == "" {
		req.Image = c.DefaultImage
	}
	if len(req.GPUTypeIDs) == 0 {
		req.GPUTypeIDs = DefaultGPUTypes
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
	case req.DiskGB > 2000:
		return req, settings, fmt.Errorf("%w: disk_gb must be at most 2000", ErrInvalid)
	}
	if err := settings.Valid(); err != nil {
		return req, settings, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return req, settings, nil
}

// Connect runs the steps in order and stops at the first that fails. Each
// step is safe to repeat, so connecting again rotates the key or changes
// the settings.
func (c *Connector) Connect(ctx context.Context, req Request) (dispatch.Result, error) {
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" {
		return dispatch.Result{}, fmt.Errorf("%w: api_key is required", ErrInvalid)
	}
	req, settings, err := c.settings(req)
	if err != nil {
		return dispatch.Result{}, err
	}

	steps := &dispatch.Steps{Provider: "runpod", Logger: c.Logger}
	step := steps.Do
	client := NewClient(c.API, req.APIKey)
	existing, had, err := dispatch.Existing(ctx, c.Pool, c.ProjectID, "runpod")
	if err != nil {
		return dispatch.Result{}, err
	}
	var old Config
	if had {
		_ = json.Unmarshal(existing.Config, &old)
	}
	cfg := Config{Image: req.Image, GPUTypeIDs: req.GPUTypeIDs, CloudType: "SECURE", DiskGB: req.DiskGB, Tag: old.Tag}
	if cfg.Tag == "" {
		cfg.Tag = strings.ToLower(rand.Text()[:8])
	}

	step("credentials", func() (string, error) {
		if _, err := client.ListPods(ctx, "clipmux-connect-check"); err != nil {
			var api *APIError
			if errors.As(err, &api) && (api.Status == 401 || api.Status == 403) {
				return "", errors.New("RunPod refused the API key")
			}
			return "", err
		}
		if settings.RateCentsPerHour > 0 {
			return "the API key works", nil
		}
		// No rate given: the dearest chosen GPU's price, from RunPod.
		gpus, err := c.GPUs(ctx, req.APIKey)
		if err != nil {
			return "", fmt.Errorf("the API key works, but RunPod's GPU prices couldn't be read: %w", err)
		}
		if settings.RateCentsPerHour = rateFor(gpus, req.GPUTypeIDs); settings.RateCentsPerHour == 0 {
			return "", errors.New("none of the chosen GPUs is for rent on Secure Cloud now; choose another")
		}
		return fmt.Sprintf("the API key works; up to $%d.%02d per machine-hour", settings.RateCentsPerHour/100, settings.RateCentsPerHour%100), nil
	})
	step("public_url", func() (string, error) {
		if c.AllowPrivateURL {
			return "skipped (CLIPMUX_EXECUTORS_ALLOW_PRIVATE_URL)", nil
		}
		if err := dispatch.CheckPublicURL(ctx, c.PublicURL(), c.Resolver); err != nil {
			return "", err
		}
		return "pods can reach " + c.PublicURL(), nil
	})
	step("image", func() (string, error) {
		// Before anything is stored in RunPod: wrong credentials fail here.
		pinned, detail, err := dispatch.PinImage(ctx, c.registry(), req.Image, req.RegistryUsername, req.RegistryPassword)
		cfg.Image = pinned
		return detail, err
	})
	step("registry", func() (string, error) {
		if req.RegistryUsername == "" {
			return "the image is public", nil
		}
		id, err := client.CreateRegistryAuth(ctx, "clipmux-"+cfg.Tag+"-"+strings.ToLower(rand.Text()[:6]), req.RegistryUsername, req.RegistryPassword)
		if err != nil {
			return "", err
		}
		cfg.RegistryAuthID = id
		return "pods pull with stored registry credentials", nil
	})
	step("saved", func() (string, error) {
		if _, err := dispatch.Save(ctx, c.Pool, c.Box, c.ProjectID, "runpod", cfg, Secret{APIKey: req.APIKey}, settings); err != nil {
			return "", err
		}
		// The previous registry credentials are replaced; removing them is
		// tidying, not correctness.
		if old.RegistryAuthID != "" && old.RegistryAuthID != cfg.RegistryAuthID {
			if err := client.DeleteRegistryAuth(ctx, old.RegistryAuthID); err != nil {
				c.Logger.Warn("connect runpod: remove the old registry credentials", "err", err)
			}
		}
		return "RunPod takes work once its setup test passes", nil
	})
	if c.SetupTest != nil {
		step("setup_test", func() (string, error) {
			if err := c.SetupTest(ctx); err != nil {
				return "", err
			}
			return "queued: a RunPod pod must encode and play the sample", nil
		})
	}
	return steps.Result, nil
}

// Disconnect forgets RunPod's key (dispatch.Disconnect refuses while
// routing uses it or a pod may still run) and removes its stored registry
// credentials from the account.
func (c *Connector) Disconnect(ctx context.Context) error {
	row, had, err := dispatch.Existing(ctx, c.Pool, c.ProjectID, "runpod")
	if err != nil || !had || len(row.SecretsEncrypted) == 0 {
		if err == nil {
			err = dispatch.ErrNotConnected
		}
		return err
	}
	ex, err := Open(row, c.Box, c.API)
	if err != nil {
		return err
	}
	if _, err := dispatch.Disconnect(ctx, c.Pool, c.ProjectID, "runpod"); err != nil {
		return err
	}
	if ex.cfg.RegistryAuthID != "" {
		if err := ex.client.DeleteRegistryAuth(ctx, ex.cfg.RegistryAuthID); err != nil {
			c.Logger.Warn("disconnect runpod: remove the registry credentials", "err", err)
		}
	}
	return nil
}

// Resolve opens the executor for a stored config: dispatch.Service.Resolve
// for RunPod rows.
func (c *Connector) Resolve(row db.ExecutorConfig) (dispatch.Executor, error) {
	return Open(row, c.Box, c.API)
}

func (c *Connector) registry() *registry.Resolver {
	if c.Registry != nil {
		return c.Registry
	}
	return registry.NewResolver()
}
