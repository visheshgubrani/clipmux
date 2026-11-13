package modal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	sdk "github.com/modal-labs/modal-client/go"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
)

// sdkAPI is api on Modal's Go SDK. The SDK speaks gRPC to Modal, so it is
// exercised only by the real-account run (exit-providers.yml); the
// executor's choices are tested through the api seam.
type sdkAPI struct {
	client *sdk.Client
	env    string
	// The token, for the billing report the SDK doesn't wrap.
	tokenID, tokenSecret string
}

// imageWait is how long a wake waits for Modal to have the image, and
// scheduleWait how long for a sandbox to be placed on one GPU type; a wake
// tries up to three types, inside dispatch's three-minute bound on a
// provider call.
const (
	imageWait    = 30 * time.Second
	scheduleWait = 45 * time.Second
)

// newSDK opens a client with the token. It reads no other credentials: the
// token wins over any ~/.modal.toml profile of the user serve runs as.
func newSDK(tokenID, tokenSecret, environment string) (*sdkAPI, error) {
	// The SDK always reads a config file, from MODAL_CONFIG_PATH or
	// ~/.modal.toml, and fails where there is no home directory; its Config
	// option can't be set from outside the package. An empty file keeps the
	// user's profiles (and their server URL) out of serve.
	if err := os.Setenv("MODAL_CONFIG_PATH", os.DevNull); err != nil {
		return nil, fmt.Errorf("modal client: %w", err)
	}
	c, err := sdk.NewClientWithOptions(&sdk.ClientParams{
		TokenID: tokenID, TokenSecret: tokenSecret, Environment: environment,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return nil, fmt.Errorf("modal client: %w", err)
	}
	return &sdkAPI{client: c, env: environment, tokenID: tokenID, tokenSecret: tokenSecret}, nil
}

func (a *sdkAPI) close() { a.client.Close() }

func (a *sdkAPI) appID(ctx context.Context, name string) (*sdk.App, error) {
	app, err := a.client.Apps.FromName(ctx, name, &sdk.AppFromNameParams{Environment: a.env, CreateIfMissing: true})
	if err != nil {
		return nil, fmt.Errorf("modal app %s: %w", name, err)
	}
	return app, nil
}

// image returns the request's image built on Modal, waiting at most
// imageWait. Modal converts a registry image on its first use, which takes
// minutes for ours, and keeps converting after the wait ends; a later call
// finds the same build (D097). A wait that ends first is ErrPreparing.
func (a *sdkAPI) image(ctx context.Context, app *sdk.App, req sandboxRequest) (*sdk.Image, error) {
	var params *sdk.ImageFromRegistryParams
	if req.RegistryUsername != "" {
		// An ephemeral secret, never stored in Modal by name: the
		// credentials live sealed in ClipMux's database.
		sec, err := a.client.Secrets.FromMap(ctx, map[string]string{
			"REGISTRY_USERNAME": req.RegistryUsername, "REGISTRY_PASSWORD": req.RegistryPassword,
		}, &sdk.SecretFromMapParams{Environment: a.env})
		if err != nil {
			return nil, fmt.Errorf("modal registry secret: %w", err)
		}
		params = &sdk.ImageFromRegistryParams{Secret: sec}
	}
	build, cancel := context.WithTimeout(ctx, imageWait)
	defer cancel()
	image, err := a.client.Images.FromRegistry(req.Image, params).Build(build, app, nil)
	if err != nil {
		if ctx.Err() == nil && errors.Is(build.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("modal is still converting %s, which takes minutes on its first use: %w", req.Image, dispatch.ErrPreparing)
		}
		return nil, fmt.Errorf("modal image %s: %w", req.Image, err)
	}
	return image, nil
}

// prepare starts Modal converting the image and reports whether it is ready.
func (a *sdkAPI) prepare(ctx context.Context, req sandboxRequest) (bool, error) {
	app, err := a.appID(ctx, req.App)
	if err != nil {
		return false, err
	}
	if _, err := a.image(ctx, app, req); errors.Is(err, dispatch.ErrPreparing) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (a *sdkAPI) create(ctx context.Context, req sandboxRequest) (string, error) {
	app, err := a.appID(ctx, req.App)
	if err != nil {
		return "", err
	}
	image, err := a.image(ctx, app, req)
	if err != nil {
		return "", err
	}
	// Create also waits, up to 21 minutes, for Modal to place the sandbox
	// on a GPU; past scheduleWait the SDK terminates it and the next GPU
	// type is tried (D097).
	sched, cancel := context.WithTimeout(ctx, scheduleWait)
	defer cancel()
	sb, err := a.client.Sandboxes.Create(sched, app, image, &sdk.SandboxCreateParams{
		Name: req.Name, Command: req.Command, Env: req.Env, GPU: req.GPU, CPU: req.CPU, MemoryMiB: req.MemoryMiB,
		Timeout: req.Timeout, Tags: req.Tags,
	})
	var full sdk.ResourceExhaustedError
	if err != nil && ctx.Err() == nil && (errors.Is(sched.Err(), context.DeadlineExceeded) || errors.As(err, &full)) {
		return "", fmt.Errorf("%s: %w", req.GPU, errNoCapacity)
	}
	if err != nil {
		return "", fmt.Errorf("modal create sandbox: %w", err)
	}
	return sb.SandboxID, nil
}

func (a *sdkAPI) find(ctx context.Context, appName string, tags map[string]string) ([]sandbox, error) {
	app, err := a.appID(ctx, appName)
	if err != nil {
		return nil, err
	}
	seq, err := a.client.Sandboxes.List(ctx, &sdk.SandboxListParams{AppID: app.AppID, Tags: tags, Environment: a.env})
	if err != nil {
		return nil, fmt.Errorf("modal list sandboxes: %w", err)
	}
	var out []sandbox
	for sb, err := range seq {
		if err != nil {
			return nil, fmt.Errorf("modal list sandboxes: %w", err)
		}
		t, err := sb.GetTags(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("modal sandbox tags: %w", err)
		}
		out = append(out, sandbox{ID: sb.SandboxID, Tags: t})
	}
	return out, nil
}

func (a *sdkAPI) terminate(ctx context.Context, id string) error {
	sb, err := a.client.Sandboxes.FromID(ctx, id, nil)
	var nf sdk.NotFoundError
	if errors.As(err, &nf) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("modal sandbox %s: %w", id, err)
	}
	if _, err := sb.Terminate(ctx, nil); err != nil && !errors.As(err, &nf) {
		return fmt.Errorf("modal terminate %s: %w", id, err)
	}
	return nil
}

// check proves the token works by resolving (creating if needed) the app.
func (a *sdkAPI) check(ctx context.Context, appName string) error {
	_, err := a.appID(ctx, appName)
	return err
}
