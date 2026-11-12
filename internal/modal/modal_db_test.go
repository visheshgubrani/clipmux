package modal

// Modal's SDK speaks gRPC to Modal, so these tests replace it at the api
// seam with a fake that records what the executor asks for. The SDK itself
// runs only in the real-account workflow (D057).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

const (
	tokenID     = "ak-TESTTOKENID"
	tokenSecret = "as-TESTTOKENSECRET"
)

type fakeModal struct {
	mu       sync.Mutex
	creates  []sandboxRequest
	running  map[string]sandbox
	opened   int
	closed   int
	n        int
	tokenSec string
	// converting: prepare reports the image still being converted.
	converting bool
	prepared   []string
	// full: GPU types Modal has no capacity for.
	full map[string]bool
}

func newFake() *fakeModal { return &fakeModal{running: map[string]sandbox{}} }

func (f *fakeModal) create(_ context.Context, req sandboxRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, req)
	if f.full[req.GPU] {
		return "", errNoCapacity
	}
	f.n++
	id := "sb-" + strconv.Itoa(f.n)
	f.running[id] = sandbox{ID: id, Tags: maps.Clone(req.Tags)}
	return id, nil
}

func (f *fakeModal) find(_ context.Context, _ string, tags map[string]string) ([]sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sandbox
	for _, s := range f.running {
		match := true
		for k, v := range tags {
			match = match && s.Tags[k] == v
		}
		if match {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeModal) terminate(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
	return nil
}

func (f *fakeModal) check(context.Context, string) error {
	if f.tokenSec != tokenSecret {
		// Modal's own message, which might echo what it was sent.
		return errors.New("Unauthenticated: invalid token secret " + f.tokenSec)
	}
	return nil
}

func (f *fakeModal) prepare(_ context.Context, req sandboxRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared = append(f.prepared, req.Image)
	return !f.converting, nil
}

func (f *fakeModal) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func executor(f *fakeModal) *Executor {
	return &Executor{api: f, cfg: Config{Image: "ghcr.io/visheshgubrani/clipmux:sha-abc", GPU: "L4", CPU: 4, MemoryMiB: 16384, Tag: "inst0001"},
		secret: Secret{RegistryUsername: "visheshgubrani", RegistryPassword: "ghp_REGISTRY"}}
}

var wake = dispatch.Wake{
	DispatchID: "dsp_1", Name: "clipmux-dsp_1", Lifetime: dispatch.MaxLifetime,
	Env: map[string]string{"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret"},
}

// A sandbox runs `clipmux worker --once` with only the dispatch's
// environment (plus the driver's video capability for NVENC), on the
// configured GPU, for at most the dispatch's lifetime, tagged so it can be
// found again.
func TestWakeStartsOneSandboxWithTheDispatchsEnvironment(t *testing.T) {
	f := newFake()
	ex := executor(f)
	id, err := ex.Wake(context.Background(), wake)
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxRequest{
		App: "clipmux-inst0001", Name: "clipmux-dsp_1", Image: "ghcr.io/visheshgubrani/clipmux:sha-abc",
		RegistryUsername: "visheshgubrani", RegistryPassword: "ghp_REGISTRY",
		// Modal appends the command to the image's entrypoint (tini --
		// clipmux), as RunPod does; a sandbox given "clipmux worker" ran
		// `clipmux clipmux worker` and exited at once (D097).
		Command: []string{"worker", "--once"},
		Env: map[string]string{
			"CLIPMUX_SERVER": "https://video.example.com", "CLIPMUX_WORKER_KEY": "cmxd_secret",
			"NVIDIA_DRIVER_CAPABILITIES": "compute,video,utility",
		},
		GPU: "L4", CPU: 4, MemoryMiB: 16384, Timeout: 6*time.Hour + 15*time.Minute,
		Tags: map[string]string{"clipmux": "inst0001", "clipmux_dispatch": "dsp_1"},
	}
	if len(f.creates) != 1 || !reflect.DeepEqual(f.creates[0], want) {
		t.Fatalf("sandbox request =\n%+v\nwant\n%+v", f.creates, want)
	}
	again, err := ex.Wake(context.Background(), wake)
	if err != nil || again != id || len(f.creates) != 1 {
		t.Fatalf("a replayed wake = %s, %v with %d creates; want %s and one create", again, err, len(f.creates), id)
	}
}

// Modal's L4 queue can stall for minutes (D051, D097): a wake takes the
// next NVENC card in the configured order, and with none free it is asked
// again later rather than failed.
func TestAWakeFallsBackToTheNextGPUWithCapacity(t *testing.T) {
	f := newFake()
	f.full = map[string]bool{"L4": true}
	e := executor(f)
	e.cfg.GPU = "L4,A10G,T4"
	id, err := e.Wake(context.Background(), wake)
	if err != nil || id != "sb-1" { // the fake numbers only sandboxes it starts
		t.Fatalf("wake = %q, %v", id, err)
	}
	if len(f.creates) != 2 || f.creates[0].GPU != "L4" || f.creates[1].GPU != "A10G" || f.creates[1].Name != "clipmux-dsp_1-a10g" {
		t.Fatalf("creates = %+v", f.creates)
	}

	f = newFake()
	f.full = map[string]bool{"L4": true, "A10G": true, "T4": true}
	e = executor(f)
	e.cfg.GPU = "L4,A10G,T4"
	if _, err := e.Wake(context.Background(), wake); !errors.Is(err, dispatch.ErrPreparing) || err.Error() != "modal has no L4, A10G or T4 free: dispatch: the provider is still preparing the machine" {
		t.Fatalf("with every GPU taken: %v", err)
	}
}

func TestStopAndRunningSeeOnlyThisInstallsSandboxes(t *testing.T) {
	f := newFake()
	ex := executor(f)
	ctx := context.Background()
	id, _ := ex.Wake(ctx, wake)
	f.running["sb-other"] = sandbox{ID: "sb-other", Tags: map[string]string{"clipmux": "inst9999", "clipmux_dispatch": "dsp_x"}}
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
	if running, _ := ex.Running(ctx); len(running) != 0 {
		t.Fatalf("after Stop: %+v", running)
	}
	if _, ok := f.running["sb-other"]; !ok {
		t.Fatal("stopped another install's sandbox")
	}
}

func connector(t *testing.T, f *fakeModal) (*Connector, *int) {
	t.Helper()
	pool := dbtest.New(t)
	reg := registrytest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{6}, 32))
	queued := 0
	c := &Connector{
		Pool: pool, Box: box, ProjectID: "prj_default", PublicURL: func() string { return "https://93.184.215.14" },
		Resolver: net.DefaultResolver, DefaultImage: reg.Image(), Registry: &registry.Resolver{HTTP: reg.Client},
		Logger:    slog.New(slog.DiscardHandler),
		SetupTest: func(context.Context) error { queued++; return nil },
		newAPI: func(_, sec, _ string) (checkedAPI, error) {
			f.mu.Lock()
			f.opened++
			f.tokenSec = sec
			f.mu.Unlock()
			return f, nil
		},
	}
	return c, &queued
}

var request = Request{TokenID: tokenID, TokenSecret: tokenSecret, RegistryUsername: registrytest.Username, RegistryPassword: registrytest.Password,
	MaxConcurrent: 2, MonthlyLimitCents: 5000, RateCentsPerHour: 90}

func stepNames(res dispatch.Result) string {
	var out []string
	for _, s := range res.Steps {
		mark := "ok"
		if !s.OK {
			mark = "FAILED"
		}
		out = append(out, s.Name+":"+mark)
	}
	return strings.Join(out, " ")
}

func TestConnectModalSealsTheTokenAndQueuesItsSetupTest(t *testing.T) {
	f := newFake()
	c, queued := connector(t, f)
	ctx := context.Background()
	res, err := c.Connect(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if want := "credentials:ok public_url:ok image:ok image_conversion:ok saved:ok setup_test:ok"; stepNames(res) != want || !res.Connected || *queued != 1 {
		t.Fatalf("steps = %s (queued %d), want %s", stepNames(res), *queued, want)
	}
	// Connect starts Modal converting the pinned image, so the first wake
	// doesn't wait minutes for it (D097).
	if len(f.prepared) != 1 || !strings.Contains(f.prepared[0], "@sha256:") || res.Steps[3].Detail != "Modal has the image" {
		t.Fatalf("prepared %v, detail %q", f.prepared, res.Steps[3].Detail)
	}
	row, _, _ := dispatch.Existing(ctx, c.Pool, "prj_default", "modal")
	var cfg Config
	_ = json.Unmarshal(row.Config, &cfg)
	if !strings.Contains(cfg.Image, "@sha256:") || cfg.GPU != "L4,A10G,T4" || cfg.CPU != 4 || cfg.MemoryMiB != 16384 || len(cfg.Tag) != 8 || row.DiskBytes != 50<<30 || row.RateCentsPerHour != 90 {
		t.Fatalf("config = %+v, row %+v", cfg, row)
	}
	for _, secret := range []string{tokenSecret, registrytest.Password, tokenID} {
		if bytes.Contains(row.SecretsEncrypted, []byte(secret)) || strings.Contains(string(row.Config), secret) {
			t.Fatalf("%s is stored in the clear", secret[:3])
		}
	}

	// One client per stored row, replaced after a reconnect.
	first, err := c.Resolve(row)
	if err != nil {
		t.Fatal(err)
	}
	if same, _ := c.Resolve(row); same != first {
		t.Fatal("Resolve opened a second client for the same row")
	}
	if res, _ := c.Connect(ctx, request); !res.Connected {
		t.Fatal(stepNames(res))
	}
	again, _, _ := dispatch.Existing(ctx, c.Pool, "prj_default", "modal")
	var cfg2 Config
	_ = json.Unmarshal(again.Config, &cfg2)
	closedBefore := f.closed
	second, _ := c.Resolve(again)
	if second == first || cfg2.Tag != cfg.Tag || f.closed != closedBefore+1 {
		t.Fatalf("after reconnect: new executor %v, tag %s→%s, closed %d→%d", second != first, cfg.Tag, cfg2.Tag, closedBefore, f.closed)
	}
	// The resolved executor carries the sealed registry credentials.
	if _, err := second.Wake(ctx, wake); err != nil || f.creates[0].RegistryPassword != registrytest.Password || !strings.Contains(f.creates[0].Image, "@sha256:") {
		t.Fatalf("wake with the stored config: %v", err)
	}
}

// A conversion still running when Connect's wait ends is no failure: the
// setup test's wake waits for it.
func TestConnectModalLeavesALongConversionToTheSetupTest(t *testing.T) {
	f := newFake()
	f.converting = true
	c, _ := connector(t, f)
	res, err := c.Connect(context.Background(), request)
	if err != nil || !res.Connected {
		t.Fatalf("connect: %v %s", err, stepNames(res))
	}
	if want := "Modal is converting the image, which takes minutes the first time; the setup test waits for it"; res.Steps[3].Detail != want {
		t.Fatalf("detail = %q, want %q", res.Steps[3].Detail, want)
	}
}

func TestConnectModalRefusesABadTokenWithoutEchoingIt(t *testing.T) {
	f := newFake()
	c, _ := connector(t, f)
	bad := request
	bad.TokenSecret = "as-WRONGSECRET"
	res, err := c.Connect(context.Background(), bad)
	if err != nil || res.Connected || stepNames(res) != "credentials:FAILED" || strings.Contains(res.Steps[0].Detail, "as-WRONGSECRET") {
		t.Fatalf("bad token: %s %+v %v", stepNames(res), res.Steps, err)
	}
	noLimit := request
	noLimit.MonthlyLimitCents = 0
	if _, err := c.Connect(context.Background(), noLimit); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no monthly limit = %v, want ErrInvalid", err)
	}
}

func TestDisconnectModalClosesItsClients(t *testing.T) {
	f := newFake()
	c, _ := connector(t, f)
	ctx := context.Background()
	c.Connect(ctx, request)
	row, _, _ := dispatch.Existing(ctx, c.Pool, "prj_default", "modal")
	if _, err := c.Resolve(row); err != nil {
		t.Fatal(err)
	}
	closed := f.closed
	if err := c.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if f.closed != closed+1 {
		t.Fatalf("closed %d clients on disconnect, want 1", f.closed-closed)
	}
	if sts, _ := dispatch.Statuses(ctx, c.Pool, "prj_default"); len(sts) != 1 || sts[0].Connected {
		t.Fatalf("after disconnect: %+v", sts)
	}
}

// fakeFlows is Modal's token flow: pending until approved.
type fakeFlows struct{ approved bool }

func (f *fakeFlows) create(context.Context) (dispatch.Approval, error) {
	return dispatch.Approval{URL: "https://modal.com/token-flow/tf-1", Code: "AB12", Expires: time.Now().Add(time.Minute), Flow: "tf-1", Secret: "ws-1"}, nil
}

func (f *fakeFlows) wait(_ context.Context, a dispatch.Approval, _ time.Duration) (string, string, bool, error) {
	if a.Flow != "tf-1" || a.Secret != "ws-1" {
		return "", "", false, errors.New("unknown flow")
	}
	if !f.approved {
		return "", "", false, nil
	}
	return tokenID, tokenSecret, true, nil
}

// Browser approval (D109): the token Modal issues connects with the
// settings asked for, never passing through the browser.
func TestConnectModalByBrowserApproval(t *testing.T) {
	f := newFake()
	c, queued := connector(t, f)
	flows := &fakeFlows{}
	c.flows = flows
	ctx := context.Background()
	a, err := c.BeginApproval(ctx)
	if err != nil || a.URL != "https://modal.com/token-flow/tf-1" || a.Code != "AB12" {
		t.Fatalf("approval = %+v, %v", a, err)
	}
	settings := request
	settings.TokenID, settings.TokenSecret = "", ""
	if st, _, err := c.FinishApproval(ctx, a, settings); err != nil || st != dispatch.ApprovalPending {
		t.Fatalf("before approving = %s, %v; want pending", st, err)
	}
	flows.approved = true
	st, res, err := c.FinishApproval(ctx, a, settings)
	if err != nil || st != dispatch.ApprovalApproved || !res.Connected || *queued != 1 || f.tokenSec != tokenSecret {
		t.Fatalf("after approving = %s, %s, %v (queued %d); want connected with Modal's token", st, stepNames(res), err, *queued)
	}
	a.Expires = time.Now().Add(-time.Second)
	if st, _, _ := c.FinishApproval(ctx, a, settings); st != dispatch.ApprovalExpired {
		t.Fatalf("an expired approval = %s, want expired", st)
	}
}
