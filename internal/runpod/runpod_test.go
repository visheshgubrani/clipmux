package runpod_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
)

func executorFor(t *testing.T, gpus ...string) (*runpod.Executor, *runpodtest.Fake) {
	t.Helper()
	fake := runpodtest.New(t)
	return runpod.New(runpod.NewClient(fake.URL, runpodtest.Key), runpod.Config{
		Image: "ghcr.io/visheshgubrani/clipmux:sha-abc", GPUTypeIDs: gpus, CloudType: "SECURE",
		DiskGB: 50, RegistryAuthID: "cra1", Tag: "inst0001",
	}), fake
}

func executor(t *testing.T) (*runpod.Executor, *runpodtest.Fake) {
	t.Helper()
	return executorFor(t, "NVIDIA L4")
}

var wake = dispatch.Wake{
	DispatchID: "dsp_1", Name: "clipmux-dsp_1",
	Env: map[string]string{"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret"},
}

// The pod runs `clipmux worker --once` on the image's entrypoint, with
// only the dispatch's environment (plus the driver's video capability for
// NVENC), on one GPU on Secure Cloud, exposing nothing and with no volume.
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
		Name: "clipmux-inst0001-dsp_1", Image: "ghcr.io/visheshgubrani/clipmux:sha-abc",
		Cloud: "SECURE", GPU: &runpod.PodGPU{ID: "NVIDIA L4", Count: 1},
		Env: map[string]string{
			"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret",
			"NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility,graphics",
		},
		Cmd: []string{"worker", "--once"}, Disk: 50, Ports: []string{}, Registry: "cra1",
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

// RunPod's v2 API places one GPU type per pod and answers 400 when it has
// none free, so a wake tries the chosen types in order (D139).
func TestWakeTakesTheNextGPUWhenOneHasNoCapacity(t *testing.T) {
	ex, fake := executorFor(t, "NVIDIA L4", "NVIDIA RTX A4000", "NVIDIA RTX A5000")
	fake.NoCapacity = map[string]bool{"NVIDIA L4": true}
	id, err := ex.Wake(context.Background(), wake)
	if err != nil {
		t.Fatal(err)
	}
	pods := fake.Pods()
	if len(pods) != 1 || pods[0].ID != id || pods[0].GPU.ID != "NVIDIA RTX A4000" || fake.Created() != 2 {
		t.Fatalf("pods = %+v after %d creates; want one on the RTX A4000 after 2", pods, fake.Created())
	}
}

// With every chosen type full the wake isn't a failure: the dispatch asks
// again later, until its claim deadline.
func TestWakeWithEveryGPUFullIsStillPreparing(t *testing.T) {
	ex, fake := executorFor(t, "NVIDIA L4", "NVIDIA RTX A4000")
	fake.NoCapacity = map[string]bool{"NVIDIA L4": true, "NVIDIA RTX A4000": true}
	_, err := ex.Wake(context.Background(), wake)
	if !errors.Is(err, dispatch.ErrPreparing) || !strings.Contains(err.Error(), "runpod has no NVIDIA L4, NVIDIA RTX A4000 free") {
		t.Fatalf("err = %v, want ErrPreparing naming both GPUs", err)
	}
	if len(fake.Pods()) != 0 || fake.Created() != 2 {
		t.Fatalf("%d pods after %d creates; want none after 2", len(fake.Pods()), fake.Created())
	}
}

// Any other refusal stops the wake: a create whose outcome is unknown is
// never repeated on another GPU, which could leave two pods running.
func TestWakeStopsAtARefusalThatIsntCapacity(t *testing.T) {
	ex, fake := executorFor(t, "NVIDIA L4", "NVIDIA RTX A4000")
	fake.FailCreates = 1
	_, err := ex.Wake(context.Background(), wake)
	if err == nil || errors.Is(err, dispatch.ErrPreparing) || fake.Created() != 1 {
		t.Fatalf("err = %v after %d creates; want a failure after 1", err, fake.Created())
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

// RunPod lists pods a page at a time, and a pod whose container exited or
// failed still bills until it is terminated, so the reaper must see it.
func TestRunningReadsEveryPageAndKeepsExitedPods(t *testing.T) {
	ex, fake := executor(t)
	fake.PageSize = 2
	want := map[string]bool{}
	for _, d := range []string{"dsp_a", "dsp_b", "dsp_c"} {
		want[fake.AddPod("clipmux-inst0001-"+d)] = true
	}
	fake.AddPod("my-own-notebook")
	exited := fake.AddPod("clipmux-inst0001-dsp_d")
	fake.SetStatus(exited, "EXITED")
	want[exited] = true
	failed := fake.AddPod("clipmux-inst0001-dsp_e")
	fake.SetStatus(failed, "ERROR")
	want[failed] = true

	running, err := ex.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, in := range running {
		got[in.ID] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Running = %v, want %v", got, want)
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
