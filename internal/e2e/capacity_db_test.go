package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/worker"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// T29.11 extends the P5 real-binary soak with paced control-plane traffic.
// Run on a dedicated VM with a scratch Postgres, never against a live stack.
func TestRecoveryCapacity(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_CAPACITY") != "1" {
		t.Skip("set CLIPMUX_E2E_CAPACITY=1 to run the capacity baseline")
	}
	duration := time.Minute
	if raw := os.Getenv("CLIPMUX_E2E_CAPACITY_DURATION"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration < time.Second || duration > 10*time.Minute || duration%time.Second != 0 {
			t.Fatal("CLIPMUX_E2E_CAPACITY_DURATION must be a whole number of seconds from 1s to 10m")
		}
	}

	in := start(t, "CLIPMUX_BUILTIN_WORKER=false", "CLIPMUX_LOG_LEVEL=info", "CLIPMUX_BACKUP_INTERVAL=0")
	owner := in.owner()
	key, _, _ := in.credentials("Capacity")
	keys := []string{key}
	for range 15 {
		out := in.admin("create-key", "--app", "Capacity", "--allow-loopback", "--scopes", "assets:read")
		keys = append(keys, regexp.MustCompile(`key:\s+(cmx_\S+)`).FindStringSubmatch(out)[1])
	}
	source := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(source, []byte("synthetic source for the simulated encoder"), 0600); err != nil {
		t.Fatal(err)
	}

	sink := newWebhookSink(t)
	appID := in.scalar(`SELECT id FROM apps WHERE name = 'Capacity'`)
	var endpoint struct{ ID, Secret string }
	if s := ownerCall(t, owner, "POST", in.url+"/v1/apps/"+appID+"/webhook-endpoints", map[string]any{"url": sink.srv.URL}, &endpoint); s != 201 {
		t.Fatalf("create webhook endpoint: %d", s)
	}
	sink.mu.Lock()
	sink.secret = endpoint.Secret
	sink.mu.Unlock()

	pairing := in.admin("create-pairing")
	code := regexp.MustCompile(`code: (\S+)`).FindStringSubmatch(pairing)[1]
	agent, err := worker.Redeem(t.Context(), http.DefaultClient, in.url, code, "Capacity encoder")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- capacityEncoder(ctx, in, agent.Key) }()
	defer func() {
		stop()
		if err := <-workerDone; err != nil {
			t.Error(err)
		}
	}()

	// Fill an ordinary 100-row list page before starting the ramp.
	var asset string
	for range 100 {
		asset, _ = in.upload(key, source)
		// Stay below the upload-create limit, including the measured stages.
		time.Sleep(550 * time.Millisecond)
	}
	paths := []string{"/v1/assets?updated_since=2000-01-01T00:00:00Z&limit=100", "/v1/assets/" + asset, "/v1/app"}
	for _, rate := range []int{10, 50, 100, 200} {
		t.Run(fmt.Sprintf("%d_rps", rate), func(t *testing.T) {
			runCapacityStage(t, in, duration, rate, paths, keys, source, endpoint.ID)
		})
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.bad != 0 {
		t.Errorf("%d webhook deliveries had invalid signatures", sink.bad)
	}
}

// The worker performs the public claim and failure calls. It finishes each
// synthetic job immediately so the measurements exclude encoder throughput.
func capacityEncoder(ctx context.Context, in *instance, key string) error {
	claim := fmt.Sprintf(`{"contract_versions":[%d],"packager_version":"%d.%d.%d","free_disk_bytes":107374182400,"sandbox":"rlimits,no_new_privs,seccomp","wait_seconds":1}`,
		workerapi.ContractVersion, workerapi.MinPackager[0], workerapi.MinPackager[1], workerapi.MinPackager[2])
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for ctx.Err() == nil {
		var job workerapi.Job
		status, err := capacityRequest(ctx, client, "POST", in.url+"/worker/v1/claim", key, claim, &job)
		if ctx.Err() != nil {
			break
		}
		if err != nil || (status != 200 && status != 204) {
			return fmt.Errorf("capacity worker claim: HTTP %d, %v", status, err)
		}
		if status == 204 {
			continue
		}
		status, err = capacityRequest(ctx, client, "POST", in.url+"/worker/v1/attempts/"+job.AttemptID+"/fail",
			job.AttemptToken, `{"permanent":true,"error_code":"unsupported_codec"}`, nil)
		if ctx.Err() == nil && (err != nil || status != 200) {
			return fmt.Errorf("capacity worker completion: HTTP %d, %v", status, err)
		}
	}
	return nil
}

func capacityRequest(ctx context.Context, client *http.Client, method, url, key, body string, response *workerapi.Job) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		// Transport errors can include a worker URL. Only report their type.
		return 0, fmt.Errorf("request failed (%T)", err)
	}
	defer res.Body.Close()
	if response != nil && res.StatusCode == 200 {
		return res.StatusCode, json.NewDecoder(res.Body).Decode(response)
	}
	_, err = io.Copy(io.Discard, res.Body)
	return res.StatusCode, err
}

type capacitySample struct {
	route   int
	latency time.Duration
	status  int
}

func runCapacityStage(t *testing.T, in *instance, duration time.Duration, rate int, paths, keys []string, source, endpoint string) {
	t.Helper()
	total := int(duration/time.Second) * rate
	samples := make(chan capacitySample, total)
	ctx, stop := context.WithCancel(t.Context())
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()

	start := time.Now()
	var requests sync.WaitGroup
	requests.Go(func() {
		slots := make(chan struct{}, 64)
		var calls sync.WaitGroup
		defer calls.Wait()
		for i := range total {
			due := start.Add(time.Duration(i) * time.Second / time.Duration(rate))
			timer := time.NewTimer(time.Until(due))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			select {
			case slots <- struct{}{}:
				calls.Go(func() {
					defer func() { <-slots }()
					route := i % len(paths)
					status, err := capacityRequest(ctx, client, "GET", in.url+paths[route], keys[i%len(keys)], "", nil)
					if err != nil {
						status = 0
					}
					samples <- capacitySample{route, time.Since(due), status}
				})
			default:
				// Retain the offered rate when the server falls behind.
			}
		}
	})
	defer func() {
		stop()
		requests.Wait()
	}()

	var assets []string
	var uploads [3][]time.Duration
	for i := range int(duration / time.Second) {
		time.Sleep(time.Until(start.Add(time.Duration(i) * time.Second)))
		step := 0
		asset, _ := in.uploadThrough(keys[0], source, nil, func(method, path, bearer string, body, out any) int {
			before := time.Now()
			status := in.call(method, path, bearer, body, out)
			uploads[step] = append(uploads[step], time.Since(before))
			step++
			return status
		})
		assets = append(assets, asset)
	}
	requests.Wait()
	close(samples)
	reads := make([][]time.Duration, len(paths))
	var completed, rejected, failed int
	failures := map[int]int{}
	for s := range samples {
		completed++
		switch s.status {
		case 200:
			reads[s.route] = append(reads[s.route], s.latency)
		case 429:
			rejected++
		default:
			failed++
			failures[s.status]++
		}
	}
	t.Logf("GET/s offered=%d; duration=%s; responses=%d; 429=%d; failures=%d; client drops=%d", rate, duration, completed, rejected, failed, total-completed)
	if failed > 0 {
		t.Logf("failures by status (0 = transport error): %v", failures)
	}
	for i, route := range []string{"GET /v1/assets?updated_since=…&limit=100", "GET /v1/assets/{id}", "GET /v1/app"} {
		logCapacity(t, route, reads[i])
	}
	for i, route := range []string{"POST /v1/uploads", "POST /v1/uploads/{id}/parts", "POST /v1/uploads/{id}/complete"} {
		logCapacity(t, route, uploads[i])
	}

	// Measure from the persisted enqueue and event timestamps, including
	// polling and outbox scheduling. Require every probe to finish.
	wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		rows, err := in.pool.Query(wait, `
			SELECT extract(epoch FROM j.started_at - j.created_at)::float8,
			       extract(epoch FROM d.finished_at - o.created_at)::float8
			FROM jobs j
			JOIN outbox o ON o.asset_id = j.asset_id AND o.payload->>'type' = 'video.asset.errored'
			JOIN webhook_deliveries d ON d.event_id = o.id
			WHERE j.asset_id = ANY($1) AND j.kind = 'transcode' AND d.endpoint_id = $2 AND d.status = 'delivered'`,
			assets, endpoint)
		if err != nil {
			t.Fatalf("wait for claims and webhook deliveries: %v", err)
		}
		var claims, deliveries []time.Duration
		for rows.Next() {
			var claim, delivery float64
			if err := rows.Scan(&claim, &delivery); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			claims = append(claims, time.Duration(claim*float64(time.Second)))
			deliveries = append(deliveries, time.Duration(delivery*float64(time.Second)))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(claims) == len(assets) {
			logCapacity(t, "enqueue to claim", claims)
			logCapacity(t, "event to webhook delivery", deliveries)
			break
		}
		if wait.Err() != nil {
			t.Fatalf("only %d of %d probes were claimed and delivered", len(claims), len(assets))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func logCapacity(t *testing.T, label string, sample []time.Duration) {
	t.Helper()
	if len(sample) == 0 {
		t.Errorf("%s has no successful samples", label)
		return
	}
	slices.Sort(sample)
	p95 := int(math.Ceil(0.95*float64(len(sample)))) - 1
	p50 := (len(sample) - 1) / 2
	t.Logf("%s n=%d p50=%s p95=%s", label, len(sample), sample[p50].Round(time.Microsecond), sample[p95].Round(time.Microsecond))
}
