package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestExampleFixture serves launch exit check 8 (e2e/web/exit8): the real
// serve with an owner account and nothing else, so examples/nextjs-lms
// connects to it with `npx @clipmux/node init` as a new app would. It
// answers on CLIPMUX_FIXTURE_PORT:
//
//	GET  /config                    {ops, ops_email, ops_password, video}: video is
//	                                the path of a 45 s clip to upload
//	POST /disable-webhooks?app=ID   disables the app's webhook endpoints as the
//	                                owner would, so later events reach the app only
//	                                through `listen`
//
// It runs until interrupted. Playwright starts it as its web server.
func TestExampleFixture(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_EXAMPLE_FIXTURE") != "1" {
		t.Skip("set CLIPMUX_E2E_EXAMPLE_FIXTURE=1 to serve the exit-8 suite")
	}
	in := start(t)
	owner := in.owner()
	// Longer than the suite's 30 s token, so playback crosses a refresh.
	video := fixture(t, 45, 640, 360)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ops": in.url, "ops_email": "owner@example.test", "ops_password": "a long test password",
			"video": video,
		})
	})
	mux.HandleFunc("POST /disable-webhooks", func(w http.ResponseWriter, r *http.Request) {
		n, err := disableEndpoints(r.Context(), owner, in.url, r.URL.Query().Get("app"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{"disabled": n})
	})

	port := os.Getenv("CLIPMUX_FIXTURE_PORT")
	if port == "" {
		port = "8097"
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	fmt.Printf("example fixture ready: config http://127.0.0.1:%s/config, ClipMux %s\n", port, in.url)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

// disableEndpoints disables every enabled webhook endpoint of an app through
// the management API, as the dashboard's Disable does, and returns how many.
func disableEndpoints(ctx context.Context, owner *http.Client, base, app string) (int, error) {
	call := func(method, path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Origin", base) // same-origin, as the dashboard's requests are
		res, err := owner.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		defer res.Body.Close()
		if res.StatusCode >= 300 {
			return fmt.Errorf("%s %s: %s", method, path, res.Status)
		}
		if out != nil {
			return json.NewDecoder(res.Body).Decode(out)
		}
		return nil
	}
	var list struct {
		Data []struct {
			ID       string     `json:"id"`
			Disabled *time.Time `json:"disabled_at"`
		} `json:"data"`
	}
	if err := call(http.MethodGet, "/v1/apps/"+app+"/webhook-endpoints", &list); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range list.Data {
		if e.Disabled != nil {
			continue
		}
		if err := call(http.MethodDelete, "/v1/webhook-endpoints/"+e.ID, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
