// Package modal is the Modal executor (P4-T05, D057): a Modal Sandbox
// started for one dispatch each through Modal's Go SDK
// (github.com/modal-labs/modal-client/go), and the Connect Modal steps.
package modal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Config is what executor_configs.config holds for Modal. Nothing here is
// secret; the token and the registry credentials are sealed separately.
type Config struct {
	// Image is the worker image every sandbox runs.
	Image string `json:"image"`
	// ImageChosen is set when the owner entered the image; otherwise it is
	// this build's, and moves with the server's upgrades (dispatch.Repin).
	ImageChosen bool `json:"image_chosen,omitempty"`
	// GPU is Modal's GPU type, e.g. "L4" (NVENC, D051).
	GPU string `json:"gpu"`
	// CPU (physical cores) and MemoryMiB size each sandbox.
	CPU       float64 `json:"cpu"`
	MemoryMiB int     `json:"memory_mib"`
	// Tag names this install's Modal app (clipmux-{tag}) and is set on
	// every sandbox, so the reconciler sees only this install's machines.
	Tag string `json:"tag"`
	// Environment is the Modal environment; empty means the account's
	// default.
	Environment string `json:"environment,omitempty"`
}

// Secret is the sealed part of executor_configs for Modal.
type Secret struct {
	TokenID          string `json:"token_id"`
	TokenSecret      string `json:"token_secret"`
	RegistryUsername string `json:"registry_username,omitempty"`
	RegistryPassword string `json:"registry_password,omitempty"`
}

// DefaultGPU are the GPU types a sandbox may take, in order: each encodes
// with NVENC (D051), and the L4's queue can stall for minutes, so a wake
// takes the next with capacity (D097).
const DefaultGPU = "L4,A10G,T4"

// errNoCapacity is a create Modal couldn't schedule on that GPU type in
// time; the sandbox was terminated, and the next type may be tried.
var errNoCapacity = errors.New("modal: no capacity for that GPU now")

// Tag names on every sandbox ClipMux starts.
const (
	tagInstall  = "clipmux"
	tagDispatch = "clipmux_dispatch"
)

// sandboxRequest is everything a sandbox is started with: the seam between
// the executor's choices and Modal's SDK, so tests can check the choices
// without Modal's gRPC API.
type sandboxRequest struct {
	App, Name, Image string
	RegistryUsername string
	RegistryPassword string
	Command          []string
	Env              map[string]string
	GPU              string
	CPU              float64
	MemoryMiB        int
	Timeout          time.Duration
	Tags             map[string]string
}

// sandbox is one running sandbox, as find reports it.
type sandbox struct {
	ID   string
	Tags map[string]string
}

// api is the part of Modal ClipMux uses. sdkAPI implements it with the
// SDK; tests use a fake.
type api interface {
	create(ctx context.Context, req sandboxRequest) (string, error)
	// find lists the app's running sandboxes carrying all of tags.
	find(ctx context.Context, app string, tags map[string]string) ([]sandbox, error)
	// terminate stops a sandbox; one that is already gone is fine.
	terminate(ctx context.Context, id string) error
}

// Executor starts one sandbox per dispatch. A sandbox runs `clipmux worker
// --once` and ends when it exits; Modal also ends it at Lifetime.
type Executor struct {
	api    api
	cfg    Config
	secret Secret
}

func (e *Executor) app() string { return "clipmux-" + e.cfg.Tag }

// Wake starts the dispatch's sandbox, or finds the one a crashed earlier
// call started.
func (e *Executor) Wake(ctx context.Context, w dispatch.Wake) (string, error) {
	tags := map[string]string{tagInstall: e.cfg.Tag, tagDispatch: w.DispatchID}
	found, err := e.api.find(ctx, e.app(), tags)
	if err != nil {
		return "", err
	}
	if len(found) > 0 {
		return found[0].ID, nil
	}
	env := map[string]string{
		// NVENC needs the video capability of the injected driver, CUDA
		// whisper compute, and Vulkan whisper graphics (the Vulkan ICD),
		// where gVisor's operator allows it (D131).
		"NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility,graphics",
	}
	maps.Copy(env, w.Env)
	var gpus []string
	for g := range strings.SplitSeq(e.cfg.GPU, ",") {
		if g = strings.TrimSpace(g); g != "" {
			gpus = append(gpus, g)
		}
	}
	for i, gpu := range gpus {
		name := w.Name
		if i > 0 { // the name of a sandbox Modal just terminated may linger
			name += "-" + strings.ToLower(gpu)
		}
		id, err := e.api.create(ctx, sandboxRequest{
			App: e.app(), Name: name, Image: e.cfg.Image,
			RegistryUsername: e.secret.RegistryUsername, RegistryPassword: e.secret.RegistryPassword,
			// Modal appends the command to the image's entrypoint (tini --
			// clipmux), as RunPod does (D097).
			Command: []string{"worker", "--once"},
			Env:     env, GPU: gpu, CPU: e.cfg.CPU, MemoryMiB: e.cfg.MemoryMiB,
			Timeout: w.Lifetime, Tags: tags,
		})
		if !errors.Is(err, errNoCapacity) {
			return id, err
		}
	}
	list := strings.Join(gpus, ", ")
	if i := strings.LastIndex(list, ", "); i >= 0 {
		list = list[:i] + " or " + list[i+2:]
	}
	return "", fmt.Errorf("modal has no %s free: %w", list, dispatch.ErrPreparing)
}

// Stop terminates a sandbox.
func (e *Executor) Stop(ctx context.Context, instanceID string) error {
	return e.api.terminate(ctx, instanceID)
}

// Running lists this install's running sandboxes, named as Wake was asked
// to name them.
func (e *Executor) Running(ctx context.Context) ([]dispatch.Instance, error) {
	found, err := e.api.find(ctx, e.app(), map[string]string{tagInstall: e.cfg.Tag})
	if err != nil {
		return nil, err
	}
	out := make([]dispatch.Instance, 0, len(found))
	for _, s := range found {
		out = append(out, dispatch.Instance{ID: s.ID, Name: dispatch.Name(s.Tags[tagDispatch])})
	}
	return out, nil
}

// open reads a stored config and its sealed secret.
func open(row db.ExecutorConfig, box *secrets.Box) (Config, Secret, error) {
	var cfg Config
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return cfg, Secret{}, fmt.Errorf("modal config: %w", err)
	}
	if len(row.SecretsEncrypted) == 0 {
		return cfg, Secret{}, fmt.Errorf("modal is disconnected")
	}
	plain, err := box.Open("executor_configs", row.ID, row.SecretsEncrypted)
	if err != nil {
		return cfg, Secret{}, fmt.Errorf("modal secret: %w", err)
	}
	var sec Secret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return cfg, Secret{}, fmt.Errorf("modal secret: %w", err)
	}
	return cfg, sec, nil
}
