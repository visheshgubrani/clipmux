package metrics

import (
	"math"
	"net/http/httptest"
	"testing"
)

func TestExpositionIsSortedEscapedAndCumulative(t *testing.T) {
	r := New()
	c := r.Counter("clipmux_http_requests_total", "Requests by route.", "route", "code")
	c.Inc("GET /v1/assets/{id}", "200")
	c.Add(2, "GET /v1/assets/{id}", "200")
	c.Inc(`a "quoted"\ path`+"\n", "404")
	c.Add(-5, "GET /v1/assets/{id}", "200") // ignored: counters don't go down
	g := r.Gauge("clipmux_leader", "Whether this replica runs the loops.\nOne at a time.")
	g.Set(1)
	h := r.Histogram("clipmux_http_request_duration_seconds", "Durations.", []float64{0.1, 1}, "route")
	h.Observe(0.05, "GET /x")
	h.Observe(0.5, "GET /x")
	h.Observe(3, "GET /x")
	r.Func("clipmux_jobs", "Jobs by state.", "gauge", []string{"kind", "status"}, func() []Sample {
		return []Sample{{Labels: []string{"transcode", "queued"}, Value: 2}, {Labels: []string{"move", "running"}, Value: 1}}
	})

	want := `# HELP clipmux_http_request_duration_seconds Durations.
# TYPE clipmux_http_request_duration_seconds histogram
clipmux_http_request_duration_seconds_bucket{route="GET /x",le="0.1"} 1
clipmux_http_request_duration_seconds_bucket{route="GET /x",le="1"} 2
clipmux_http_request_duration_seconds_bucket{route="GET /x",le="+Inf"} 3
clipmux_http_request_duration_seconds_sum{route="GET /x"} 3.55
clipmux_http_request_duration_seconds_count{route="GET /x"} 3
# HELP clipmux_http_requests_total Requests by route.
# TYPE clipmux_http_requests_total counter
clipmux_http_requests_total{route="GET /v1/assets/{id}",code="200"} 3
clipmux_http_requests_total{route="a \"quoted\"\\ path\n",code="404"} 1
# HELP clipmux_jobs Jobs by state.
# TYPE clipmux_jobs gauge
clipmux_jobs{kind="move",status="running"} 1
clipmux_jobs{kind="transcode",status="queued"} 2
# HELP clipmux_leader Whether this replica runs the loops.\nOne at a time.
# TYPE clipmux_leader gauge
clipmux_leader 1
`
	if got := string(r.Expose()); got != want {
		t.Fatalf("exposition\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" || rec.Body.String() != want {
		t.Fatalf("handler: %q", rec.Header().Get("Content-Type"))
	}
}

func TestNumbersAndMisuse(t *testing.T) {
	for v, want := range map[float64]string{1e21: "1e+21", 0.25: "0.25", math.Inf(1): "+Inf", 7: "7"} {
		if got := number(v); got != want {
			t.Errorf("number(%v) = %q, want %q", v, got, want)
		}
	}
	r := New()
	c := r.Counter("x_total", "x", "a")
	defer func() {
		if recover() == nil {
			t.Fatal("a wrong number of label values must panic: it is a programming error")
		}
	}()
	c.Inc("1", "2")
}
