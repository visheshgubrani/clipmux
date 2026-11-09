package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Config is what executor_configs.config holds for RunPod. Nothing here is
// secret; the API key is sealed separately.
type Config struct {
	// Image is the worker image every pod runs (T06 pins it to the server's
	// build).
	Image string `json:"image"`
	// GPUTypeIDs are the GPU types a pod may get, whichever has capacity.
	GPUTypeIDs []string `json:"gpu_type_ids"`
	// CloudType is SECURE: Community Cloud hosts are strangers' machines,
	// and a worker sees the plaintext source and the output's key (D056).
	CloudType string `json:"cloud_type"`
	// DiskGB is each pod's container disk, its scratch space.
	DiskGB int `json:"disk_gb"`
	// RegistryAuthID names RunPod's stored credentials for a private image.
	RegistryAuthID string `json:"registry_auth_id,omitempty"`
	// Tag marks this install's pods in the account, so the reconciler never
	// touches another install's or the owner's own pods.
	Tag string `json:"tag"`
}

// Secret is the sealed part of executor_configs for RunPod.
type Secret struct {
	APIKey string `json:"api_key"`
}

// DefaultGPUTypes have NVENC, so the worker encodes on the GPU; RunPod
// picks whichever has capacity. Data-centre cards without NVENC (A100,
// H100) would encode on the CPU and cost more for it, and so do RunPod's
// RTX 4090 hosts, which refuse NVENC sessions ("unsupported device") even
// outside the sandbox (D097). The L4 is proven on a real pod.
var DefaultGPUTypes = []string{"NVIDIA L4", "NVIDIA RTX A4000", "NVIDIA RTX A5000"}

// Executor starts one pod per dispatch. Pods run `clipmux worker --once`
// with the dispatch's environment; RunPod restarts a container that exits,
// so a pod lives until the reaper stops it (D055).
type Executor struct {
	client *Client
	cfg    Config
}

// New returns an executor for one account and config.
func New(client *Client, cfg Config) *Executor { return &Executor{client: client, cfg: cfg} }

// Open builds the executor for a stored config, unsealing its key.
func Open(row db.ExecutorConfig, box *secrets.Box, api string) (*Executor, error) {
	var cfg Config
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return nil, fmt.Errorf("runpod config: %w", err)
	}
	if len(row.SecretsEncrypted) == 0 {
		return nil, fmt.Errorf("runpod is disconnected")
	}
	plain, err := box.Open("executor_configs", row.ID, row.SecretsEncrypted)
	if err != nil {
		return nil, fmt.Errorf("runpod secret: %w", err)
	}
	var sec Secret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return nil, fmt.Errorf("runpod secret: %w", err)
	}
	return New(NewClient(api, sec.APIKey), cfg), nil
}

// podName is the RunPod name of a dispatch's pod: clipmux-{tag}-{dispatch}.
func (e *Executor) podName(name string) string {
	return "clipmux-" + e.cfg.Tag + "-" + strings.TrimPrefix(name, "clipmux-")
}

func live(p Pod) bool { return p.DesiredStatus != "TERMINATED" }

// Wake starts the dispatch's pod, or finds the one a crashed earlier call
// started under the same name.
func (e *Executor) Wake(ctx context.Context, w dispatch.Wake) (string, error) {
	name := e.podName(w.Name)
	existing, err := e.client.ListPods(ctx, name)
	if err != nil {
		return "", err
	}
	for _, p := range existing {
		if p.Name == name && live(p) {
			return p.ID, nil
		}
	}
	env := map[string]string{
		// NVENC needs the video capability of the injected driver.
		"NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility",
	}
	for k, v := range w.Env {
		env[k] = v
	}
	p, err := e.client.CreatePod(ctx, PodInput{
		Name:                    name,
		ImageName:               e.cfg.Image,
		ComputeType:             "GPU",
		CloudType:               e.cfg.CloudType,
		GPUTypeIDs:              e.cfg.GPUTypeIDs,
		GPUTypePriority:         "availability",
		GPUCount:                1,
		Env:                     env,
		DockerStartCmd:          []string{"worker", "--once"}, // the image's entrypoint is clipmux
		ContainerDiskInGb:       e.cfg.DiskGB,
		VolumeInGb:              0,
		Ports:                   []string{}, // nothing listens; workers only call out
		ContainerRegistryAuthID: e.cfg.RegistryAuthID,
	})
	return p.ID, err
}

// Stop terminates a pod.
func (e *Executor) Stop(ctx context.Context, instanceID string) error {
	return e.client.DeletePod(ctx, instanceID)
}

// Running lists this install's live pods, named as Wake was asked to name
// them.
func (e *Executor) Running(ctx context.Context) ([]dispatch.Instance, error) {
	pods, err := e.client.ListPods(ctx, "")
	if err != nil {
		return nil, err
	}
	prefix := "clipmux-" + e.cfg.Tag + "-"
	var out []dispatch.Instance
	for _, p := range pods {
		if rest, ok := strings.CutPrefix(p.Name, prefix); ok && live(p) {
			out = append(out, dispatch.Instance{ID: p.ID, Name: "clipmux-" + rest})
		}
	}
	return out, nil
}
