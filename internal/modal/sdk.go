package modal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	sdk "github.com/modal-labs/modal-client/go"
)

// sdkAPI is api on Modal's Go SDK. The SDK speaks gRPC to Modal, so it is
// exercised only by the real-account run (exit-providers.yml); the
// executor's choices are tested through the api seam.
type sdkAPI struct {
	client *sdk.Client
	env    string
}

// newSDK opens a client with the token. It reads no other credentials: the
// token wins over any ~/.modal.toml profile of the user serve runs as.
func newSDK(tokenID, tokenSecret, environment string) (*sdkAPI, error) {
	c, err := sdk.NewClientWithOptions(&sdk.ClientParams{
		TokenID: tokenID, TokenSecret: tokenSecret, Environment: environment,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return nil, fmt.Errorf("modal client: %w", err)
	}
	return &sdkAPI{client: c, env: environment}, nil
}

func (a *sdkAPI) close() { a.client.Close() }

func (a *sdkAPI) appID(ctx context.Context, name string) (*sdk.App, error) {
	app, err := a.client.Apps.FromName(ctx, name, &sdk.AppFromNameParams{Environment: a.env, CreateIfMissing: true})
	if err != nil {
		return nil, fmt.Errorf("modal app %s: %w", name, err)
	}
	return app, nil
}

func (a *sdkAPI) create(ctx context.Context, req sandboxRequest) (string, error) {
	app, err := a.appID(ctx, req.App)
	if err != nil {
		return "", err
	}
	var img *sdk.ImageFromRegistryParams
	if req.RegistryUsername != "" {
		// An ephemeral secret, never stored in Modal by name: the
		// credentials live sealed in ClipMux's database.
		sec, err := a.client.Secrets.FromMap(ctx, map[string]string{
			"REGISTRY_USERNAME": req.RegistryUsername, "REGISTRY_PASSWORD": req.RegistryPassword,
		}, &sdk.SecretFromMapParams{Environment: a.env})
		if err != nil {
			return "", fmt.Errorf("modal registry secret: %w", err)
		}
		img = &sdk.ImageFromRegistryParams{Secret: sec}
	}
	sb, err := a.client.Sandboxes.Create(ctx, app, a.client.Images.FromRegistry(req.Image, img), &sdk.SandboxCreateParams{
		Name: req.Name, Command: req.Command, Env: req.Env, GPU: req.GPU, CPU: req.CPU, MemoryMiB: req.MemoryMiB,
		Timeout: req.Timeout, Tags: req.Tags,
	})
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
