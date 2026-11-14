package runpod_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
)

func executor(t *testing.T) (*runpod.Executor, *runpodtest.Fake) {
	t.Helper()
	fake := runpodtest.New(t)
	return runpod.New(runpod.NewClient(fake.URL, runpodtest.Key), runpod.Config{
		Image: "ghcr.io/visheshgubrani/clipmux:sha-abc", GPUTypeIDs: []string{"NVIDIA L4"}, CloudType: "SECURE",
		DiskGB: 50, RegistryAuthID: "cra1", Tag: "inst0001",
	}), fake
}

var wake = dispatch.Wake{
	DispatchID: "dsp_1", Name: "clipmux-dsp_1",
	Env: map[string]string{"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret"},
}

// The pod runs `clipmux worker --once` on the image's entrypoint, with
// only the dispatch's environment (plus the driver's video capability for
// NVENC), on Secure Cloud, exposing nothing.
func TestWakeCreatesOnePodWithTheDispatchsEnvironment(t *testing.T) {
	ex, fake := executor(t)
	id, err := ex.Wake(context.Background(), wake)
	if err != nil {
		t.Fatal(err)
	}
	pods := fake.Pods()
	if len(pods) != 1 || pods[0].ID != id {
		t.Fatalf("pods = %+v, want one with ID %s", pods, id)
	}
	got := pods[0].PodInput
	want := runpod.PodInput{
		Name: "clipmux-inst0001-dsp_1", ImageName: "ghcr.io/visheshgubrani/clipmux:sha-abc",
		ComputeType: "GPU", CloudType: "SECURE", GPUTypeIDs: []string{"NVIDIA L4"}, GPUTypePriority: "availability", GPUCount: 1,
		Env: map[string]string{
			"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret",
			"NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility,graphics",
		},
		DockerStartCmd: []string{"worker", "--once"}, ContainerDiskInGb: 50, VolumeInGb: 0, Ports: []string{},
		ContainerRegistryAuthID: "cra1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pod request =\n%+v\nwant\n%+v", got, want)
	}

	// A retried wake finds the pod by name rather than start a second.
	again, err := ex.Wake(context.Background(), wake)
	if err != nil || again != id || fake.Created() != 1 {
		t.Fatalf("retried wake = %s, %v with %d creates; want %s and one create", again, err, fake.Created(), id)
	}
}

func TestStopTerminatesAndRunningListsOnlyThisInstallsPods(t *testing.T) {
	ex, fake := executor(t)
	ctx := context.Background()
	id, _ := ex.Wake(ctx, wake)
	other := fake.AddPod("clipmux-inst9999-dsp_x") // another install on the same account
	mine := fake.AddPod("my-own-notebook")         // the owner's own pod
	running, err := ex.Running(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].ID != id || running[0].Name != dispatch.Name("dsp_1") {
		t.Fatalf("Running = %+v, want only %s named clipmux-dsp_1", running, id)
	}
	if err := ex.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := ex.Stop(ctx, id); err != nil {
		t.Fatalf("stopping a gone pod = %v, want nil", err)
	}
	if running, _ := ex.Running(ctx); len(running) != 0 {
		t.Fatalf("after Stop: %+v", running)
	}
	left := map[string]bool{}
	for _, p := range fake.Pods() {
		left[p.ID] = true
	}
	if !left[other] || !left[mine] || len(left) != 2 {
		t.Fatalf("pods left = %v, want only the other install's and the owner's", left)
	}
}

func TestErrorsNeverCarryTheKey(t *testing.T) {
	fake := runpodtest.New(t)
	ex := runpod.New(runpod.NewClient(fake.URL, "rpa_WRONG_KEY_1234"), runpod.Config{Tag: "t"})
	_, err := ex.Wake(context.Background(), wake)
	if err == nil || strings.Contains(err.Error(), "rpa_WRONG_KEY_1234") || strings.Contains(err.Error(), "cmxd_secret") {
		t.Fatalf("err = %v", err)
	}
}
