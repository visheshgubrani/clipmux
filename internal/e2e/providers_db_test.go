package e2e

// Check 9 of the Phase 4 exit test (docs/plans/P4-serverless.md), on real
// accounts: Connect RunPod and Connect Modal each run the setup test
// through their provider, against serve behind a tunnel, encode on NVENC,
// and leave no machine running. With the provider as the primary, a clip of
// speech is then transcribed on its GPU and timed (P5-T19). exit-providers.yml
// runs it; without a provider's secrets that provider is skipped.
//
//	CLIPMUX_E2E_PROVIDERS=1
//	CLIPMUX_E2E_PUBLIC_URL     the tunnel's https URL, forwarding to CLIPMUX_E2E_SERVE_PORT
//	CLIPMUX_E2E_IMAGE          the worker image, e.g. ghcr.io/visheshgubrani/clipmux:sha-…
//	CLIPMUX_E2E_REGISTRY_USERNAME, CLIPMUX_E2E_REGISTRY_PASSWORD   a pull credential for it
//	RUNPOD_API_KEY
//	MODAL_TOKEN_ID, MODAL_TOKEN_SECRET

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/modal"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

func TestExitRealProviders(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_PROVIDERS") != "1" {
		t.Skip("set CLIPMUX_E2E_PROVIDERS=1 (exit-providers.yml) to run against real RunPod and Modal accounts")
	}
	publicURL := os.Getenv("CLIPMUX_E2E_PUBLIC_URL")
	if publicURL == "" || os.Getenv("CLIPMUX_E2E_SERVE_PORT") == "" {
		t.Fatal("CLIPMUX_E2E_PUBLIC_URL and CLIPMUX_E2E_SERVE_PORT name the tunnel and where it forwards")
	}
	image := os.Getenv("CLIPMUX_E2E_IMAGE")
	in := start(t, "CLIPMUX_PUBLIC_URL="+publicURL, "CLIPMUX_WORKER_IMAGE="+image)
	in.waitSetupTestsIdle(5 * time.Minute)
	// Through the tunnel, as the owner would: with an https public URL the
	// session cookie is Secure.
	owner := in.ownerAt(publicURL)
	registry := map[string]any{
		"registry_username": os.Getenv("CLIPMUX_E2E_REGISTRY_USERNAME"),
		"registry_password": os.Getenv("CLIPMUX_E2E_REGISTRY_PASSWORD"),
		"max_concurrent":    1,
		// A setup test runs for minutes; $20 bounds a run gone wrong.
		"monthly_limit_cents": 2000,
	}
	providers := []struct {
		kind   string
		secret []string
		req    map[string]any
	}{
		{"runpod", []string{"RUNPOD_API_KEY"}, map[string]any{
			"api_key": os.Getenv("RUNPOD_API_KEY"), "rate_cents_per_hour": 80,
		}},
		{"modal", []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"}, map[string]any{
			"token_id": os.Getenv("MODAL_TOKEN_ID"), "token_secret": os.Getenv("MODAL_TOKEN_SECRET"), "rate_cents_per_hour": 100,
		}},
	}
	for _, p := range providers {
		t.Run(p.kind, func(t *testing.T) {
			for _, s := range p.secret {
				if os.Getenv(s) == "" {
					t.Skipf("%s is not set", s)
				}
			}
			for k, v := range registry {
				p.req[k] = v
			}
			var res struct {
				Steps []struct {
					Name, Detail string
					OK           bool
				}
			}
			started := time.Now()
			if s := ownerCall(t, owner, "POST", publicURL+"/v1/executors/"+p.kind+"/connect", p.req, &res); s != 200 {
				t.Fatalf("connect %s: %d", p.kind, s)
			}
			// Disconnecting removes what Connect stored in the account, such
			// as RunPod's registry credentials.
			t.Cleanup(func() {
				if s := ownerCall(t, owner, "DELETE", publicURL+"/v1/executors/"+p.kind, nil, nil); s != 204 {
					t.Errorf("disconnect %s: %d", p.kind, s)
				}
			})
			// Pass or fail, nothing of this run may keep billing: the
			// database (and so the reconciler) goes with the test. Runs
			// before the disconnect, which an active dispatch refuses.
			t.Cleanup(func() { stopAll(t, in, p.kind) })
			for _, step := range res.Steps {
				t.Logf("%s: ok=%v %s", step.Name, step.OK, step.Detail)
				if !step.OK {
					t.Fatalf("connect %s failed at %s", p.kind, step.Name)
				}
			}
			// A cold start pulls the image (about 575 MB) onto a GPU host.
			deadline := time.Now().Add(25 * time.Minute)
			for {
				status := in.scalar(`SELECT status FROM setup_tests WHERE executor = $1 ORDER BY created_at DESC LIMIT 1`, p.kind)
				if status == "passed" {
					break
				}
				if status == "failed" || time.Now().After(deadline) {
					t.Fatalf("the setup test through %s is %s: %s", p.kind, status,
						in.scalar(`SELECT coalesce(checks::text, '') FROM setup_tests WHERE executor = $1 ORDER BY created_at DESC LIMIT 1`, p.kind))
				}
				time.Sleep(10 * time.Second)
			}
			t.Logf("the setup test through %s passed in %s (cold start: %s after the wake)", p.kind, time.Since(started).Round(time.Second),
				in.scalar(`SELECT coalesce((claimed_at - woken_at)::text, '') FROM dispatches WHERE kind = $1 ORDER BY created_at DESC LIMIT 1`, p.kind))
			if stage := in.scalar(`
				SELECT coalesce(o.manifest->'encoder'->>'stage', '') FROM setup_tests s
				JOIN outputs o ON o.asset_id = s.asset_id AND o.manifest IS NOT NULL
				WHERE s.executor = $1 ORDER BY s.created_at DESC LIMIT 1`, p.kind); stage != "nvenc" {
				t.Errorf("the sample encoded on %q, want nvenc", stage)
			}

			// P5-T19: with the provider as the primary, a clip of speech is
			// transcribed on its GPU (the image ships whisper; the machine
			// fetches the models by digest), and its real-time factor logged.
			speechOn(t, in, owner, publicURL, p.kind)

			// Every machine stopped, by the database and by the provider.
			deadline = time.Now().Add(3 * time.Minute)
			for in.scalar(`SELECT count(*)::text FROM dispatches WHERE kind = $1 AND instance_id IS NOT NULL AND stopped_at IS NULL`, p.kind) != "0" {
				if time.Now().After(deadline) {
					t.Fatal("a dispatch's machine was never stopped")
				}
				time.Sleep(5 * time.Second)
			}
			if running := machines(t, in, p.kind); len(running) != 0 {
				t.Fatalf("%s still runs %v", p.kind, running)
			}
		})
	}
}

// stopAll stops every machine of this install the provider still runs.
func stopAll(t *testing.T, in *instance, kind string) {
	t.Helper()
	ex := executor(t, in, kind)
	for _, m := range machines(t, in, kind) {
		if err := ex.Stop(context.Background(), m.ID); err != nil {
			t.Errorf("stop %s machine %s: %v", kind, m.Name, err)
		} else {
			t.Logf("stopped %s machine %s, left running by the run", kind, m.Name)
		}
	}
}

// machines asks the provider itself which ClipMux machines of this install
// are running, through the stored credentials.
func machines(t *testing.T, in *instance, kind string) []dispatch.Instance {
	t.Helper()
	running, err := executor(t, in, kind).Running(context.Background())
	if err != nil {
		t.Fatalf("list %s machines: %s", kind, strings.ReplaceAll(err.Error(), os.Getenv("RUNPOD_API_KEY"), "…"))
	}
	return running
}

// executor opens the provider with the credentials Connect stored.
func executor(t *testing.T, in *instance, kind string) dispatch.Executor {
	t.Helper()
	ctx := context.Background()
	box, err := secrets.Load(in.dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.New(in.pool).GetExecutorConfigByKind(ctx, db.GetExecutorConfigByKindParams{ProjectID: db.DefaultProjectID, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	var ex dispatch.Executor
	switch kind {
	case "runpod":
		ex, err = runpod.Open(row, box, runpod.DefaultAPI)
	case "modal":
		ex, err = (&modal.Connector{Box: box}).Resolve(row)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// speechOn routes to kind, transcribes 11 s of English there, checks what
// came back, logs how long the transcription took, and routes back.
func speechOn(t *testing.T, in *instance, owner *http.Client, publicURL, kind string) {
	t.Helper()
	routing := func(primary string) {
		if s := ownerCall(t, owner, "PUT", publicURL+"/v1/settings/routing", map[string]any{"primary_executor": primary}, nil); s != 200 {
			t.Fatalf("route to %s: %d", primary, s)
		}
	}
	routing(kind)
	defer routing("workers")
	key, _, _ := in.credentials("Speech on " + kind)
	asset, _ := in.uploadWith(key, speechFixture(t), map[string]any{"generate": map[string]bool{"subtitles": true}})
	in.waitReady(key, asset, 25*time.Minute)
	trk := in.waitTracks(key, asset, 30*time.Minute)["subtitles"]
	if trk.Status != "ready" || trk.Language != "en" {
		t.Fatalf("subtitles on %s = %+v", kind, trk)
	}
	var tr struct {
		Text string `json:"text"`
	}
	in.call("GET", "/v1/assets/"+asset+"/tracks/"+trk.ID+"/transcript", key, nil, &tr)
	if !strings.Contains(strings.ToLower(tr.Text), "ask not what your country") {
		t.Fatalf("transcript on %s = %q", kind, tr.Text)
	}
	t.Logf("transcription on %s: %s", kind, in.scalar(`
		SELECT a.executor || ', ' || round(extract(epoch FROM a.finished_at - a.started_at)::numeric, 1) || ' s for 11 s, worker ' || coalesce(a.worker_info::text, '')
		FROM attempts a JOIN jobs j ON j.id = a.job_id
		WHERE j.asset_id = $1 AND j.kind = 'transcribe' AND a.status = 'committed'`, asset))
}
