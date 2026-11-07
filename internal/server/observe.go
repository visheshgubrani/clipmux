package server

import (
	"context"
	"net/http"
	"net/http/pprof"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/chapters"
	"github.com/visheshgubrani/clipmux/internal/db"
	cm "github.com/visheshgubrani/clipmux/internal/metrics"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/speechapi"
	"github.com/visheshgubrani/clipmux/internal/sweeper"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// observer holds the server's metrics (D066). Request and loop metrics are
// counted as they happen; job, outbox, webhook and dispatch counts are read
// from Postgres when scraped, cached for dbCacheFor, so they are the same
// whichever replica answers.
type observer struct {
	reg *cm.Registry

	requests      *cm.Counter
	duration      *cm.Histogram
	inFlight      *cm.Gauge
	deliveryBytes *cm.Counter

	passSeconds *cm.Histogram
	passErrors  *cm.Counter
	passPanics  *cm.Counter
	passLast    *cm.Gauge
}

// dbCacheFor is how long counts read from Postgres are reused; a scrape
// every few seconds then costs a handful of queries every 10 s at most.
const dbCacheFor = 10 * time.Second

var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

var passBuckets = []float64{0.01, 0.1, 0.5, 1, 5, 15, 60, 300, 1800}

func newObserver(pool *pgxpool.Pool, leader *sweeper.Leader, version string) *observer {
	r := cm.New()
	o := &observer{
		reg: r,
		requests: r.Counter("clipmux_http_requests_total",
			"HTTP requests by route pattern and status.", "route", "code"),
		duration: r.Histogram("clipmux_http_request_duration_seconds",
			"HTTP request durations by route pattern, long-polls excluded.", durationBuckets, "route"),
		inFlight: r.Gauge("clipmux_http_in_flight",
			"HTTP requests being served."),
		deliveryBytes: r.Counter("clipmux_delivery_bytes_total",
			"Playback bytes sent, by playback ID policy.", "policy"),
		passSeconds: r.Histogram("clipmux_sweeper_pass_seconds",
			"Background loop pass durations.", passBuckets, "sweeper"),
		passErrors: r.Counter("clipmux_sweeper_errors_total",
			"Background loop passes that failed, panics included.", "sweeper"),
		passPanics: r.Counter("clipmux_sweeper_panics_total",
			"Background loop passes that panicked.", "sweeper"),
		passLast: r.Gauge("clipmux_sweeper_last_success_timestamp_seconds",
			"When each background loop last finished a pass without error.", "sweeper"),
	}
	r.Gauge("clipmux_build_info", "The build running, always 1.", "version").Set(1, version)
	start := float64(time.Now().Unix())
	r.Func("clipmux_process_start_time_seconds", "When serve started.", "gauge", nil,
		func() []cm.Sample { return []cm.Sample{{Value: start}} })
	r.Func("clipmux_leader", "1 if this replica runs the background loops.", "gauge", nil, func() []cm.Sample {
		v := 0.0
		if leader != nil && leader.IsLeader() {
			v = 1
		}
		return []cm.Sample{{Value: v}}
	})
	o.runtime()
	if pool != nil {
		o.pool(pool)
		o.database(pool)
	}
	return o
}

// request records one finished request.
func (o *observer) request(r *http.Request, status int, bytes int64, took time.Duration) {
	if o == nil {
		return
	}
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	o.requests.Inc(route, strconv.Itoa(status))
	if classify(r) != classLongPoll {
		o.duration.Observe(took.Seconds(), route)
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/v/"):
		o.deliveryBytes.Add(float64(bytes), "signed")
	case strings.HasPrefix(r.URL.Path, "/p/"):
		o.deliveryBytes.Add(float64(bytes), "public")
	}
}

// loop wraps a background task so each pass is measured. A panic is
// counted and passed on to sweeper.Every, which recovers it.
func (o *observer) loop(name string, task sweeper.Task) sweeper.Task {
	if o == nil {
		return task
	}
	return func(ctx context.Context) (n int, err error) {
		start := time.Now()
		defer func() {
			o.passSeconds.Observe(time.Since(start).Seconds(), name)
			if v := recover(); v != nil {
				o.passErrors.Inc(name)
				o.passPanics.Inc(name)
				panic(v)
			}
			if err != nil {
				o.passErrors.Inc(name)
			} else {
				o.passLast.Set(float64(time.Now().Unix()), name)
			}
		}()
		return task(ctx)
	}
}

// Handler serves /metrics, and pprof when asked: the metrics listener's
// whole mux. It is never mounted on the public listener.
func (o *observer) Handler(withPProf bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", o.reg.Handler())
	if withPProf {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	return mux
}

func (o *observer) runtime() {
	o.reg.Func("clipmux_go_goroutines", "Goroutines in serve.", "gauge", nil,
		func() []cm.Sample { return []cm.Sample{{Value: float64(runtime.NumGoroutine())}} })
	o.reg.Func("clipmux_go_heap_bytes", "Bytes in live and unswept heap objects.", "gauge", nil, func() []cm.Sample {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		metrics.Read(s)
		if s[0].Value.Kind() != metrics.KindUint64 {
			return nil
		}
		return []cm.Sample{{Value: float64(s[0].Value.Uint64())}}
	})
}

func (o *observer) pool(pool *pgxpool.Pool) {
	stat := func(f func(*pgxpool.Stat) float64) func() []cm.Sample {
		return func() []cm.Sample { return []cm.Sample{{Value: f(pool.Stat())}} }
	}
	o.reg.Func("clipmux_db_pool_acquired_conns", "Pool connections in use.", "gauge", nil,
		stat(func(s *pgxpool.Stat) float64 { return float64(s.AcquiredConns()) }))
	o.reg.Func("clipmux_db_pool_idle_conns", "Idle pool connections.", "gauge", nil,
		stat(func(s *pgxpool.Stat) float64 { return float64(s.IdleConns()) }))
	o.reg.Func("clipmux_db_pool_max_conns", "The pool's connection limit.", "gauge", nil,
		stat(func(s *pgxpool.Stat) float64 { return float64(s.MaxConns()) }))
	o.reg.Func("clipmux_db_pool_acquire_wait_seconds_total", "Time spent waiting for a pool connection.", "counter", nil,
		stat(func(s *pgxpool.Stat) float64 { return s.AcquireDuration().Seconds() }))
	o.reg.Func("clipmux_db_pool_empty_acquires_total", "Acquires that had to wait for a connection.", "counter", nil,
		stat(func(s *pgxpool.Stat) float64 { return float64(s.EmptyAcquireCount()) }))
}

// knownCodes bound the error_code label: a worker can send any snake_case
// code, and each would become a series of its own.
var knownCodes = map[string]bool{
	"": true, "unsupported_source": true, "source_download_failed": true, "transcode_failed": true,
	"upload_failed": true, "commit_failed": true, "output_invalid": true, "scratch_full": true,
	"job_timeout": true, "source_missing": true, "source_changed": true, "source_invalid": true,
	queue.CodeWorkerStopped: true, queue.CodeWorkerLost: true, queue.CodeProcessingFailed: true,
	queue.CodeAgentRevoked: true, "agent_unavailable": true, "wake_failed": true, "wake_timeout": true,
	// Tracks (P5 track B): a transcription's and its worker's codes.
	tracks.CodeNoAudio: true, tracks.CodeTranscribeFailed: true, tracks.CodeTranscriptInvalid: true,
	tracks.CodeTranscribeUnavailable: true, tracks.CodeSubtitlesFailed: true, "model_unavailable": true,
	speechapi.CodeUnavailable: true, speechapi.CodeRejected: true,
	// Chapters (P5-T17).
	chapters.CodeNotConnected: true, chapters.CodeRejected: true, chapters.CodeRefused: true,
	chapters.CodeInvalidOutput: true, chapters.CodeTranscriptTooLarge: true, chapters.CodeUnavailable: true,
}

func boundCode(code string) string {
	if knownCodes[code] {
		return code
	}
	return "other"
}

// dbCounts is one read of every Postgres-backed family.
type dbCounts struct {
	jobs, oldest, attempts, outbox, outboxAge, deliveries, dispatches, online []cm.Sample
}

func (o *observer) database(pool *pgxpool.Pool) {
	var mu sync.Mutex
	var at time.Time
	var cur dbCounts
	read := func() dbCounts {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) < dbCacheFor {
			return cur
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cur, at = readCounts(ctx, db.New(pool)), time.Now()
		return cur
	}
	family := func(name, help, typ string, labels []string, pick func(dbCounts) []cm.Sample) {
		o.reg.Func(name, help, typ, labels, func() []cm.Sample { return pick(read()) })
	}
	family("clipmux_jobs", "Queued and running jobs.", "gauge", []string{"kind", "status", "routed_to", "route_reason"},
		func(c dbCounts) []cm.Sample { return c.jobs })
	family("clipmux_job_queue_oldest_seconds", "How long the oldest due queued job of each kind has waited.", "gauge",
		[]string{"kind"}, func(c dbCounts) []cm.Sample { return c.oldest })
	family("clipmux_attempts_finished_total", "Attempts that ended, by executor, result and error code.", "counter",
		[]string{"executor", "result", "error_code"}, func(c dbCounts) []cm.Sample { return c.attempts })
	family("clipmux_outbox_pending", "Outbox rows not yet done, by kind.", "gauge", []string{"kind"},
		func(c dbCounts) []cm.Sample { return c.outbox })
	family("clipmux_outbox_oldest_pending_seconds", "Age of the oldest outbox row not yet done, by kind.", "gauge",
		[]string{"kind"}, func(c dbCounts) []cm.Sample { return c.outboxAge })
	family("clipmux_webhook_deliveries", "Webhook deliveries by status.", "gauge", []string{"status"},
		func(c dbCounts) []cm.Sample { return c.deliveries })
	family("clipmux_dispatches", "Serverless dispatches by provider and status.", "gauge", []string{"provider", "status"},
		func(c dbCounts) []cm.Sample { return c.dispatches })
	family("clipmux_workers_online", "Workers seen in the last minute: PC agents and the built-in worker.", "gauge",
		[]string{"kind"}, func(c dbCounts) []cm.Sample { return c.online })
}

// readCounts reads the Postgres-backed families. A query that fails leaves
// its family empty for this scrape rather than failing the others.
func readCounts(ctx context.Context, q *db.Queries) dbCounts {
	var c dbCounts
	if rows, err := q.MetricActiveJobs(ctx); err == nil {
		for _, r := range rows {
			c.jobs = append(c.jobs, cm.Sample{Labels: []string{r.Kind, r.Status, r.RoutedTo, r.RouteReason}, Value: float64(r.N)})
		}
	}
	if rows, err := q.MetricOldestQueued(ctx); err == nil {
		for _, r := range rows {
			c.oldest = append(c.oldest, cm.Sample{Labels: []string{r.Kind}, Value: r.Seconds})
		}
	}
	if rows, err := q.MetricFinishedAttempts(ctx); err == nil {
		sum := map[[3]string]float64{}
		for _, r := range rows {
			sum[[3]string{r.Executor, r.Status, boundCode(r.ErrorCode)}] += float64(r.N)
		}
		for k, n := range sum {
			c.attempts = append(c.attempts, cm.Sample{Labels: []string{k[0], k[1], k[2]}, Value: n})
		}
	}
	if rows, err := q.MetricPendingOutbox(ctx); err == nil {
		for _, r := range rows {
			c.outbox = append(c.outbox, cm.Sample{Labels: []string{r.Kind}, Value: float64(r.N)})
			c.outboxAge = append(c.outboxAge, cm.Sample{Labels: []string{r.Kind}, Value: r.OldestSeconds})
		}
	}
	if rows, err := q.MetricWebhookDeliveries(ctx); err == nil {
		for _, r := range rows {
			c.deliveries = append(c.deliveries, cm.Sample{Labels: []string{r.Status}, Value: float64(r.N)})
		}
	}
	if rows, err := q.MetricDispatches(ctx); err == nil {
		for _, r := range rows {
			c.dispatches = append(c.dispatches, cm.Sample{Labels: []string{r.Kind, r.Status}, Value: float64(r.N)})
		}
	}
	if w, err := q.MetricWorkersOnline(ctx, queue.OnlineWithin.Seconds()); err == nil {
		c.online = []cm.Sample{{Labels: []string{"agent"}, Value: float64(w.Agents)}, {Labels: []string{"builtin"}, Value: float64(w.Builtin)}}
	}
	return c
}
