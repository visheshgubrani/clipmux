package runpod_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

type connectRig struct {
	pool   *pgxpool.Pool
	fake   *runpodtest.Fake
	reg    *registrytest.Registry
	c      *runpod.Connector
	queued int
}

func newConnectRig(t *testing.T) *connectRig {
	t.Helper()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{5}, 32))
	r := &connectRig{pool: pool, fake: runpodtest.New(t), reg: registrytest.New(t)}
	r.c = &runpod.Connector{
		Pool: pool, Box: box, ProjectID: "prj_default", API: r.fake.URL, Catalog: r.fake.Catalog,
		PublicURL: func() string { return "https://93.184.215.14" }, Resolver: net.DefaultResolver,
		DefaultImage: r.reg.Image(), Registry: &registry.Resolver{HTTP: r.reg.Client}, Logger: slog.New(slog.DiscardHandler),
		SetupTest: func(context.Context) error { r.queued++; return nil },
	}
	return r
}

var request = runpod.Request{
	APIKey: runpodtest.Key, RegistryUsername: registrytest.Username, RegistryPassword: registrytest.Password,
	MaxConcurrent: 3, MonthlyLimitCents: 5000, RateCentsPerHour: 45,
}

func steps(res dispatch.Result) string {
	var names []string
	for _, s := range res.Steps {
		mark := "ok"
		if !s.OK {
			mark = "FAILED"
		}
		names = append(names, s.Name+":"+mark)
	}
	return strings.Join(names, " ")
}

func TestConnectRunPodStoresASealedKeyAndQueuesItsSetupTest(t *testing.T) {
	r := newConnectRig(t)
	ctx := context.Background()
	res, err := r.c.Connect(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if want := "credentials:ok public_url:ok image:ok registry:ok saved:ok setup_test:ok"; steps(res) != want || !res.Connected {
		t.Fatalf("steps = %s, want %s", steps(res), want)
	}
	if r.queued != 1 {
		t.Fatalf("queued %d setup tests, want 1", r.queued)
	}
	row, ok, err := dispatch.Existing(ctx, r.pool, "prj_default", "runpod")
	if err != nil || !ok {
		t.Fatalf("no config row: %v", err)
	}
	var cfg runpod.Config
	_ = json.Unmarshal(row.Config, &cfg)
	auths := r.fake.Auths()
	// Pods run exactly the image that was connected, by digest.
	if cfg.Image != r.reg.Pinned() || cfg.CloudType != "SECURE" || cfg.DiskGB != 50 ||
		len(cfg.Tag) != 8 || auths[cfg.RegistryAuthID] == "" || len(cfg.GPUTypeIDs) != len(runpod.DefaultGPUTypes) {
		t.Fatalf("config = %+v, auths %v", cfg, auths)
	}
	if row.MaxConcurrent != 3 || row.MonthlyLimitCents != 5000 || row.RateCentsPerHour != 45 || row.DiskBytes != 50<<30 || row.VerifiedAt.Valid {
		t.Fatalf("row = %+v, want the request's limits and not yet verified", row)
	}
	// Neither secret is stored as itself.
	for _, secret := range []string{runpodtest.Key, registrytest.Password} {
		if bytes.Contains(row.SecretsEncrypted, []byte(secret)) || strings.Contains(string(row.Config), secret) {
			t.Fatalf("%s is stored in the clear", secret[:4])
		}
	}
	// The stored key opens the executor.
	ex, err := r.c.Resolve(row)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Running(ctx); err != nil {
		t.Fatalf("the stored key doesn't work: %v", err)
	}

	// Connecting again rotates the key's row in place, keeps the tag,
	// replaces the registry credentials and needs a new setup test.
	if _, err := r.pool.Exec(ctx, `UPDATE executor_configs SET verified_at = now()`); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.c.Connect(ctx, request); !res.Connected {
		t.Fatalf("reconnect: %s", steps(res))
	}
	again, _, _ := dispatch.Existing(ctx, r.pool, "prj_default", "runpod")
	var cfg2 runpod.Config
	_ = json.Unmarshal(again.Config, &cfg2)
	if again.ID != row.ID || cfg2.Tag != cfg.Tag || again.VerifiedAt.Valid || cfg2.RegistryAuthID == cfg.RegistryAuthID {
		t.Fatalf("reconnect: row %s→%s tag %s→%s verified %v auth %s→%s", row.ID, again.ID, cfg.Tag, cfg2.Tag,
			again.VerifiedAt.Valid, cfg.RegistryAuthID, cfg2.RegistryAuthID)
	}
	if _, left := r.fake.Auths()[cfg.RegistryAuthID]; left || len(r.fake.Auths()) != 1 {
		t.Fatalf("auths after reconnect = %v, want only the new one", r.fake.Auths())
	}
}

func TestConnectRunPodRefusesBadKeysPrivateURLsAndMissingLimits(t *testing.T) {
	ctx := context.Background()
	r := newConnectRig(t)
	bad := request
	bad.APIKey = "rpa_NOT_THE_KEY"
	res, err := r.c.Connect(ctx, bad)
	if err != nil || res.Connected || steps(res) != "credentials:FAILED" || strings.Contains(res.Steps[0].Detail, "rpa_NOT_THE_KEY") {
		t.Fatalf("bad key: %s %+v %v", steps(res), res.Steps, err)
	}

	r.c.PublicURL = func() string { return "https://192.168.1.20" }
	res, _ = r.c.Connect(ctx, request)
	if steps(res) != "credentials:ok public_url:FAILED" {
		t.Fatalf("private URL: %s", steps(res))
	}
	r.c.AllowPrivateURL = true
	if res, _ := r.c.Connect(ctx, request); !res.Connected {
		t.Fatalf("with the development override: %s", steps(res))
	}

	// Credentials that can't pull the image fail before RunPod stores them.
	r.c.AllowPrivateURL = false
	r.c.PublicURL = func() string { return "https://93.184.215.14" }
	wrongRegistry := request
	wrongRegistry.RegistryPassword = "ghp_WRONG"
	authsBefore := len(r.fake.Auths())
	res, _ = r.c.Connect(ctx, wrongRegistry)
	if steps(res) != "credentials:ok public_url:ok image:FAILED" || len(r.fake.Auths()) != authsBefore || strings.Contains(res.Steps[2].Detail, "ghp_WRONG") {
		t.Fatalf("wrong registry password: %s, %d auths stored", steps(res), len(r.fake.Auths())-authsBefore)
	}

	negative := request
	negative.RateCentsPerHour = -1
	if _, err := r.c.Connect(ctx, negative); !errors.Is(err, runpod.ErrInvalid) {
		t.Fatalf("a negative rate = %v, want ErrInvalid", err)
	}
	if r.fake.Created() != 0 {
		t.Fatal("connecting started a pod")
	}
}

func TestDisconnectRunPodWaitsForMachinesAndHandsWorkBack(t *testing.T) {
	ctx := context.Background()
	r := newConnectRig(t)
	if res, _ := r.c.Connect(ctx, request); !res.Connected {
		t.Fatal(steps(res))
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE executor_configs SET verified_at = now()`)
	exec(`UPDATE projects SET primary_executor = 'runpod', fallback_executor = 'builtin', queue_age_fallback_minutes = 10`)
	routing := func() string {
		t.Helper()
		var got string
		if err := r.pool.QueryRow(ctx, `SELECT primary_executor || '/' || coalesce(fallback_executor, '-') || '/' || coalesce(queue_age_fallback_minutes::text, '-')
			FROM projects WHERE id = 'prj_default'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default')`)
	exec(`INSERT INTO jobs (id, project_id, asset_id, kind, routed_to) VALUES ('job_1', 'prj_default', 'ast_1', 'transcode', 'runpod')`)
	exec(`INSERT INTO dispatches (id, project_id, job_id, executor_config_id, kind, key_hash, key_sealed, rate_cents_per_hour, claim_deadline)
		SELECT 'dsp_1', 'prj_default', 'job_1', id, 'runpod', 'h', 's', 45, now() + interval '10 minutes' FROM executor_configs`)
	if err := r.c.Disconnect(ctx); !errors.Is(err, dispatch.ErrMachinesRunning) {
		t.Fatalf("disconnect with a live dispatch = %v, want ErrMachinesRunning", err)
	}
	if got := routing(); got != "runpod/builtin/10" {
		t.Fatalf("routing after a refused disconnect = %s, want it unchanged (runpod/builtin/10)", got)
	}
	exec(`UPDATE dispatches SET status = 'ended', ended_at = now(), end_reason = 'job_gone'`)
	// Disconnecting the primary hands the work back to this server (D116).
	if err := r.c.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if got := routing(); got != "builtin/-/-" {
		t.Fatalf("routing after disconnecting the primary = %s, want builtin/-/-", got)
	}
	sts, _ := dispatch.Statuses(ctx, r.pool, "prj_default")
	if len(sts) != 1 || sts[0].Connected || sts[0].Verified || len(r.fake.Auths()) != 0 {
		t.Fatalf("after disconnect: %+v, auths %v", sts, r.fake.Auths())
	}
	// Disconnected providers are invisible to the reconciler.
	if rows, _ := db.New(r.pool).ListExecutorConfigs(ctx, "prj_default"); len(rows) != 0 {
		t.Fatalf("the reconciler would still see %d providers", len(rows))
	}
	if err := r.c.Disconnect(ctx); !errors.Is(err, dispatch.ErrNotConnected) {
		t.Fatalf("disconnecting twice = %v", err)
	}
}

// Browser approval (D109): the owner approves on RunPod's page, and the key
// RunPod issues connects with the settings asked for, never passing through
// the browser.
func TestConnectRunPodByBrowserApproval(t *testing.T) {
	r := newConnectRig(t)
	r.c.GraphQL, r.c.Console = r.fake.GraphQL, "https://console.example.test"
	ctx := context.Background()
	a, err := r.c.BeginApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a.URL != "https://console.example.test/flash/login?request="+a.Flow || a.Flow == "" {
		t.Fatalf("approval page = %q (request %q)", a.URL, a.Flow)
	}
	settings := request
	settings.APIKey = ""
	st, _, err := r.c.FinishApproval(ctx, a, settings)
	if err != nil || st != dispatch.ApprovalPending {
		t.Fatalf("before approving = %s, %v; want pending", st, err)
	}
	r.fake.Approve(a.Flow)
	st, res, err := r.c.FinishApproval(ctx, a, settings)
	if err != nil || st != dispatch.ApprovalApproved || !res.Connected {
		t.Fatalf("after approving = %s, %s, %v; want approved and connected", st, steps(res), err)
	}
	row, ok, err := dispatch.Existing(ctx, r.pool, "prj_default", "runpod")
	if err != nil || !ok || row.MaxConcurrent != 3 {
		t.Fatalf("config = %+v, %v, %v; want the settings asked for", row, ok, err)
	}
	// RunPod's console may show the key on its page instead: the owner
	// pastes it.
	shown, _ := r.c.BeginApproval(ctx)
	r.fake.Show(shown.Flow)
	if st, _, err := r.c.FinishApproval(ctx, shown, settings); err != nil || st != dispatch.ApprovalShown {
		t.Fatalf("a key shown on RunPod's page = %s, %v; want shown", st, err)
	}

	denied, _ := r.c.BeginApproval(ctx)
	r.fake.Deny(denied.Flow)
	if st, _, err := r.c.FinishApproval(ctx, denied, settings); err != nil || st != dispatch.ApprovalDenied {
		t.Fatalf("a refused approval = %s, %v; want denied", st, err)
	}
}

// Choosing GPUs (D111): the NVENC types on Secure Cloud with RunPod's price,
// the recommended one first; left alone, the limit is $50 a month and the
// rate the dearest chosen GPU's price.
func TestConnectRunPodPricesTheChosenGPUs(t *testing.T) {
	ctx := context.Background()
	r := newConnectRig(t)
	gpus, err := r.c.GPUs(ctx, runpodtest.Key)
	want := []runpod.GPU{
		{ID: "NVIDIA L4", Name: "L4", MemoryGB: 24, CentsPerHour: 43, Recommended: true},
		{ID: "NVIDIA RTX A4000", Name: "RTX A4000", MemoryGB: 16, CentsPerHour: 25},
	}
	if err != nil || !reflect.DeepEqual(gpus, want) {
		t.Fatalf("gpus = %+v, %v; want %+v", gpus, err, want)
	}
	if _, err := r.c.GPUs(ctx, "rpa_NOT_THE_KEY"); err == nil {
		t.Fatal("a wrong key listed GPUs")
	}

	plain := runpod.Request{APIKey: runpodtest.Key, RegistryUsername: registrytest.Username, RegistryPassword: registrytest.Password,
		GPUTypeIDs: []string{"NVIDIA RTX A4000", "NVIDIA L4"}}
	res, err := r.c.Connect(ctx, plain)
	if err != nil || !res.Connected || !strings.Contains(res.Steps[0].Detail, "$0.43 per machine-hour") {
		t.Fatalf("connect with defaults = %s %+v, %v", steps(res), res.Steps, err)
	}
	row, _, _ := dispatch.Existing(ctx, r.pool, "prj_default", "runpod")
	if row.MonthlyLimitCents != 5000 || row.RateCentsPerHour != 43 || row.MaxConcurrent != 10 {
		t.Fatalf("saved limit %d, rate %d, machines %d; want 5000, 43, 10", row.MonthlyLimitCents, row.RateCentsPerHour, row.MaxConcurrent)
	}

	gone := plain
	gone.GPUTypeIDs = []string{"NVIDIA RTX A5000"}
	if res, _ := r.c.Connect(ctx, gone); res.Connected || steps(res) != "credentials:FAILED" {
		t.Fatalf("only a GPU not for rent on Secure Cloud = %s, want credentials to fail", steps(res))
	}
}
