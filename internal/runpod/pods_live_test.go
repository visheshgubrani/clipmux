package runpod_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/runpod"
)

// The real RunPod REST API v2 (D139). CLIPMUX_LIVE_RUNPOD=1 with
// RUNPOD_API_KEY lists the account's pods, stores and removes a registry
// credential, then starts one pod as a wake does, finds it in Running and
// terminates it. That rents a GPU for about a minute.
// CLIPMUX_LIVE_RUNPOD_IMAGE picks the image; the pod's worker has no key,
// so it never takes a job.
func TestLiveRunPodPods(t *testing.T) {
	key := os.Getenv("RUNPOD_API_KEY")
	if os.Getenv("CLIPMUX_LIVE_RUNPOD") == "" || key == "" {
		t.Skip("CLIPMUX_LIVE_RUNPOD or RUNPOD_API_KEY unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := runpod.NewClient(runpod.DefaultAPI, key)
	suffix := strings.ToLower(rand.Text()[:6])

	pods, err := client.ListPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the account has %d pods", len(pods))

	auth, err := client.CreateRegistryAuth(ctx, "clipmux-live-"+suffix, "clipmux-live", "not-a-password-"+rand.Text())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteRegistryAuth(ctx, auth); err != nil {
		t.Fatal(err)
	}

	ex := runpod.New(client, runpod.Config{
		Image:      cmp.Or(os.Getenv("CLIPMUX_LIVE_RUNPOD_IMAGE"), "ghcr.io/visheshgubrani/clipmux:v1.0.0-rc.1"),
		GPUTypeIDs: runpod.DefaultGPUTypes, CloudType: "SECURE", DiskGB: 20, Tag: "live" + suffix[:4],
	})
	name := dispatch.Name("dsp_live" + suffix)
	id, err := ex.Wake(ctx, dispatch.Wake{DispatchID: "dsp_live" + suffix, Name: name, Env: map[string]string{"CLIPMUX_SERVER": "https://clipmux.invalid"}})
	if id != "" {
		t.Cleanup(func() {
			if err := ex.Stop(context.Background(), id); err != nil {
				t.Errorf("terminate pod %s: %v; terminate it in RunPod's console", id, err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pod %s started", id)
	running, err := ex.Running(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(running, dispatch.Instance{ID: id, Name: name}) {
		t.Fatalf("Running = %+v, want pod %s named %s", running, id, name)
	}
	if err := ex.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	running, err = ex.Running(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(running, func(in dispatch.Instance) bool { return in.ID == id }) {
		t.Fatalf("pod %s is still listed after Stop: %+v", id, running)
	}
}
