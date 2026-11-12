package manage_test

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
	"github.com/visheshgubrani/clipmux/internal/runpod"
	"github.com/visheshgubrani/clipmux/internal/runpod/runpodtest"
)

// Browser approval over HTTP (D109): the owner starts it with the settings
// chosen, opens the provider's page, and polls; once approved, Connect runs
// with the key the provider issued, which never reaches the browser.
func TestConnectRunPodByApprovingInTheBrowser(t *testing.T) {
	r := newRig(t, config.Config{})
	fake := runpodtest.New(t)
	reg := registrytest.New(t)
	r.m.RunPod = &runpod.Connector{
		Pool: r.pool, Box: r.box, ProjectID: "prj_default", API: fake.URL, GraphQL: fake.GraphQL,
		Console: "https://console.example.test", PublicURL: func() string { return "https://93.184.215.14" },
		Resolver: net.DefaultResolver, DefaultImage: reg.Image(), Registry: &registry.Resolver{HTTP: reg.Client},
		Logger: slog.New(slog.DiscardHandler),
	}
	settings := map[string]any{
		"registry_username": registrytest.Username, "registry_password": registrytest.Password,
		"max_concurrent": 2, "monthly_limit_cents": 5000, "rate_cents_per_hour": 45,
	}

	if got := r.call("POST", "/v1/executors/runpod/authorize", map[string]any{"max_concurrent": 2}, ""); got.status != http.StatusBadRequest || code(got) != "invalid_request" {
		t.Fatalf("settings Connect would refuse = %d %v, want 400 before any approval starts", got.status, got.body)
	}
	started := r.call("POST", "/v1/executors/runpod/authorize", settings, "")
	id, _ := started.body["id"].(string)
	url, _ := started.body["url"].(string)
	if started.status != http.StatusCreated || id == "" || !strings.HasPrefix(url, "https://console.example.test/flash/login?request=") {
		t.Fatalf("start = %d %v", started.status, started.body)
	}
	request := strings.TrimPrefix(url, "https://console.example.test/flash/login?request=")
	if strings.Contains(id, request) {
		t.Fatal("the dashboard's id names RunPod's request; it is ClipMux's own")
	}
	if got := r.call("GET", "/v1/executors/runpod/authorize/"+id, nil, ""); got.status != 200 || got.body["status"] != "pending" {
		t.Fatalf("before approving = %d %v, want pending", got.status, got.body)
	}
	if got := r.call("GET", "/v1/executors/modal/authorize/"+id, nil, ""); got.status != http.StatusNotFound {
		t.Fatalf("the id under another provider = %d, want 404", got.status)
	}

	fake.Approve(request)
	done := r.call("GET", "/v1/executors/runpod/authorize/"+id, nil, "")
	result, _ := done.body["result"].(map[string]any)
	if done.status != 200 || done.body["status"] != "approved" || result["connected"] != true {
		t.Fatalf("after approving = %d %v, want approved and connected", done.status, done.body)
	}
	if raw := r.mustJSON(done.body); strings.Contains(raw, runpodtest.Key) {
		t.Fatal("the issued key reached the browser")
	}
	// A repeated poll gets the same answer, and Connect ran once.
	if again := r.call("GET", "/v1/executors/runpod/authorize/"+id, nil, ""); again.body["status"] != "approved" {
		t.Fatalf("polling again = %v, want the same result", again.body)
	}
	creates := 0
	for _, c := range fake.Calls() {
		if c.Method == "POST" && c.Path == "/v1/containerregistryauth" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("Connect ran %d times, want 1", creates)
	}
	if got := r.call("GET", "/v1/executors", nil, ""); !strings.Contains(r.mustJSON(got.body), `"connected":true`) {
		t.Fatalf("executors = %v, want RunPod connected", got.body)
	}

	// At most three wait at once.
	for i := range 3 {
		if got := r.call("POST", "/v1/executors/runpod/authorize", settings, ""); got.status != http.StatusCreated {
			t.Fatalf("approval %d = %d %v", i+2, got.status, got.body)
		}
	}
	if got := r.call("POST", "/v1/executors/runpod/authorize", settings, ""); got.status != http.StatusTooManyRequests {
		t.Fatalf("a fifth approval with three waiting = %d %v, want 429", got.status, got.body)
	}
}
