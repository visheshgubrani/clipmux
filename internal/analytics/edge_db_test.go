package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// dataPoint is one Analytics Engine data point as the Worker writes it (D103):
// blobs project, app, asset, playback ID, kind, viewer key; doubles views,
// watch seconds, bytes.
type dataPoint struct {
	at                  string
	sample              float64
	project, app, asset string
	pid, kind, viewer   string
	views, watch, bytes float64
}

// fakeSQL is Cloudflare's Analytics Engine SQL API for account acc_1. It
// answers the pull's two queries by aggregating its points, weighted by
// _sample_interval, and SHOW TABLES; sums come back as strings and hours
// as numbers, as ClickHouse's JSON may give either.
type fakeSQL struct {
	mu      sync.Mutex
	token   string
	dataset bool
	fail    bool
	points  []dataPoint
	queries []string
}

var (
	projectRE = regexp.MustCompile(`blob1 = '([^']*)'`)
	fromRE    = regexp.MustCompile(`timestamp >= toDateTime\('([^']*)'\)`)
	toRE      = regexp.MustCompile(`timestamp < toDateTime\('([^']*)'\)`)
)

func (f *fakeSQL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.URL.Path != "/accounts/acc_1/analytics_engine/sql" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"errors":[{"code":10000,"message":"Authentication error"}]}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	sql := string(body)
	f.queries = append(f.queries, sql)
	if strings.HasPrefix(sql, "SHOW TABLES") {
		data := []map[string]any{{"dataset": "other"}}
		if f.dataset {
			data = append(data, map[string]any{"dataset": "clipmux_delivery"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "rows": len(data)})
		return
	}
	if f.fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
		return
	}
	if !strings.Contains(sql, "FROM clipmux_delivery") || !strings.HasSuffix(sql, "FORMAT JSON") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	project := projectRE.FindStringSubmatch(sql)[1]
	from, _ := time.Parse(time.DateTime, fromRE.FindStringSubmatch(sql)[1])
	to, _ := time.Parse(time.DateTime, toRE.FindStringSubmatch(sql)[1])
	viewers := strings.Contains(sql, "blob6 AS viewer")
	type key struct {
		bucket                  int64
		app, asset, pid, viewer string
	}
	sums := map[key][3]float64{}
	var order []key
	for _, p := range f.points {
		at := mustTime(p.at)
		if p.project != project || at.Before(from) || !at.Before(to) {
			continue
		}
		k := key{bucket: at.Truncate(time.Hour).Unix(), app: p.app, asset: p.asset, pid: p.pid}
		if viewers {
			if p.viewer == "" {
				continue
			}
			k = key{bucket: at.Truncate(24 * time.Hour).Unix(), app: p.app, asset: p.asset, viewer: p.viewer}
		}
		if _, ok := sums[k]; !ok {
			order = append(order, k)
		}
		// Sampled points stand for _sample_interval points each; a query
		// that doesn't weight by it undercounts, as it would on Cloudflare.
		w := 1.0
		if strings.Contains(sql, "SUM(_sample_interval * double1)") {
			w = p.sample
		}
		s := sums[k]
		s[0] += w * p.views
		s[1] += w * p.watch
		s[2] += w * p.bytes
		sums[k] = s
	}
	data := []map[string]any{}
	for _, k := range order {
		if viewers {
			data = append(data, map[string]any{"day": k.bucket, "app_id": k.app, "asset_id": k.asset, "viewer": k.viewer})
			continue
		}
		s := sums[k]
		data = append(data, map[string]any{
			"hour": k.bucket, "app_id": k.app, "asset_id": k.asset, "playback_id": k.pid,
			"views": fmt.Sprint(s[0]), "watch_seconds": fmt.Sprint(s[1]), "bytes": fmt.Sprint(s[2]),
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": []any{}, "data": data, "rows": len(data)})
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

const (
	edgeToken = "cf-analytics-read-0123456789"
	viewerV1  = "0102030405060708090a0b0c0d0e0f10"
	viewerV2  = "1112131415161718191a1b1c1d1e1f20"
)

type edgeRig struct {
	edge *Edge
	fake *fakeSQL
	pool *pgxpool.Pool
	now  time.Time
}

func newEdgeRig(t *testing.T) *edgeRig {
	t.Helper()
	pool := dbtest.New(t)
	box, _ := secrets.NewBox(1, bytes.Repeat([]byte{9}, 32))
	fake := &fakeSQL{token: edgeToken, dataset: true}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	r := &edgeRig{fake: fake, pool: pool, now: mustTime("2026-11-10T09:30:00Z")}
	r.edge = NewEdge(pool, box, "prj_default")
	r.edge.API = api.URL
	r.edge.Now = func() time.Time { return r.now }
	return r
}

func (r *edgeRig) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (r *edgeRig) cloudflare(t *testing.T) {
	t.Helper()
	r.exec(t, `INSERT INTO cloudflare_connections (project_id, account_id, zone_id, hostname, bucket, script_name, token_id, storage_id, worker_version)
	           VALUES ('prj_default', 'acc_1', 'zone_1', 'media.example.com', 'clipmux-media', 'clipmux-media', 'tok_1', 'sto_default', 'v1')`)
}

// rows reads a table's rows as text, sorted, for literal comparison.
func (r *edgeRig) rows(t *testing.T, sql string) []string {
	t.Helper()
	rows, err := r.pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, v := range vals {
			switch v := v.(type) {
			case time.Time:
				parts = append(parts, v.UTC().Format(time.RFC3339))
			case []byte:
				parts = append(parts, fmt.Sprintf("%x", v))
			default:
				parts = append(parts, fmt.Sprint(v))
			}
		}
		out = append(out, strings.Join(parts, " "))
	}
	slices.Sort(out)
	return out
}

const (
	hourlySQL = `SELECT source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes FROM playback_hourly`
	dailySQL  = `SELECT source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes FROM playback_daily`
	viewerSQL = `SELECT day, asset_id, viewer, app_id FROM playback_viewers`
	vdaysSQL  = `SELECT day, app_id, viewer FROM playback_viewer_days`
)

func same(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestConnectingEdgeAnalyticsChecksTheToken(t *testing.T) {
	r := newEdgeRig(t)
	ctx := context.Background()
	if err := r.edge.Connect(ctx, edgeToken); !errors.Is(err, ErrNoCloudflare) {
		t.Fatalf("without Cloudflare delivery: %v, want ErrNoCloudflare", err)
	}
	r.cloudflare(t)
	if err := r.edge.Connect(ctx, "wrong"); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("a rejected token: %v, want ErrTokenRejected", err)
	}
	if err := r.edge.Connect(ctx, edgeToken); err != nil {
		t.Fatal(err)
	}
	// Sealed, and the first pass starts 13 days back, at a day's start, so
	// the hourly rows (kept 14 days) hold every hour of each day it rebuilds.
	same(t, "edge_analytics", r.rows(t, `SELECT account_id, pulled_through, token_sealed IS NOT NULL,
	        position(convert_to('`+edgeToken+`', 'UTF8') IN token_sealed) = 0 FROM edge_analytics`),
		[]string{"acc_1 2026-10-28T01:00:00Z true true"})
	st, err := r.edge.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Connected || st.AccountID != "acc_1" || !st.PulledThrough.Equal(mustTime("2026-10-28T01:00:00Z")) {
		t.Fatalf("status = %+v", st)
	}
	if err := r.edge.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.edge.Disconnect(ctx); !errors.Is(err, ErrEdgeNotConnected) {
		t.Fatalf("a second disconnect: %v, want ErrEdgeNotConnected", err)
	}
}

func TestAPassCatchesUpADayAtATime(t *testing.T) {
	r := newEdgeRig(t)
	ctx := context.Background()
	r.cloudflare(t)
	if err := r.edge.Connect(ctx, edgeToken); err != nil {
		t.Fatal(err)
	}
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	q := r.fake.queries[len(r.fake.queries)-1]
	if !strings.Contains(q, "timestamp >= toDateTime('2026-10-28 00:00:00')") || !strings.Contains(q, "timestamp < toDateTime('2026-10-29 01:00:00')") {
		t.Fatalf("the first pass asked for\n%s", q)
	}
	same(t, "watermark", r.rows(t, `SELECT pulled_through FROM edge_analytics`), []string{"2026-10-29T01:00:00Z"})

	// A pull that stalled for weeks resumes 13 days back, not where it was.
	r.exec(t, `UPDATE edge_analytics SET pulled_through = '2026-09-01T00:00:00Z'`)
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	same(t, "watermark", r.rows(t, `SELECT pulled_through FROM edge_analytics`), []string{"2026-10-29T01:00:00Z"})
}

func TestADatasetNotYetWrittenAdvancesTheWatermark(t *testing.T) {
	r := newEdgeRig(t)
	ctx := context.Background()
	r.cloudflare(t)
	r.fake.dataset = false
	if err := r.edge.Connect(ctx, edgeToken); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE edge_analytics SET pulled_through = '2026-11-10T08:00:00Z'`)
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(r.fake.queries); n != 2 || !strings.HasPrefix(r.fake.queries[1], "SHOW TABLES") {
		t.Fatalf("queries = %q, want the connect check and one SHOW TABLES", r.fake.queries)
	}
	same(t, "watermark", r.rows(t, `SELECT pulled_through FROM edge_analytics`), []string{"2026-11-10T09:30:00Z"})
}

// The pull replaces the edge rows of the hours it recomputes, rebuilds the
// edge daily rows of the days they touch, and adds the edge viewers. Origin
// rows are never touched, and a pull repeated changes nothing.
func TestThePullReplacesEdgeRowsAndChangesNothingTwice(t *testing.T) {
	r := newEdgeRig(t)
	ctx := context.Background()
	r.cloudflare(t)
	if err := r.edge.Connect(ctx, edgeToken); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE edge_analytics SET pulled_through = '2026-11-10T08:00:00Z'`)
	r.exec(t, `INSERT INTO apps (id, project_id, name) VALUES ('app_a', 'prj_default', 'a'), ('app_b', 'prj_default', 'b')`)
	r.exec(t, `INSERT INTO assets (id, project_id, app_id, status, storage_id, title) VALUES
	           ('ast_1', 'prj_default', 'app_a', 'processing', 'sto_default', 'Lesson 1'),
	           ('ast_2', 'prj_default', 'app_b', 'processing', 'sto_default', 'Lesson 2')`)
	// An earlier pass wrote 03:00 and a partial 08:00; origin counted 08:00.
	r.exec(t, `INSERT INTO playback_hourly (project_id, source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes) VALUES
	           ('prj_default', 'edge',   '2026-11-10T03:00:00Z', 'app_a', 'ast_1', 'pid_1', 2, 30, 4000),
	           ('prj_default', 'edge',   '2026-11-10T08:00:00Z', 'app_a', 'ast_1', 'pid_1', 5, 5, 5),
	           ('prj_default', 'origin', '2026-11-10T08:00:00Z', 'app_a', 'ast_1', 'pid_1', 1, 6, 100)`)
	r.exec(t, `INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes) VALUES
	           ('prj_default', 'edge',   '2026-11-10', 'app_a', 'ast_1', 'pid_1', 7, 35, 4005),
	           ('prj_default', 'origin', '2026-11-10', 'app_a', 'ast_1', 'pid_1', 1, 6, 100)`)
	r.fake.points = []dataPoint{
		{at: "2026-11-10T07:10:00Z", sample: 1, project: "prj_default", app: "app_a", asset: "ast_1", pid: "pid_1", kind: "master", viewer: viewerV1, views: 1, bytes: 150},
		{at: "2026-11-10T08:05:00Z", sample: 2, project: "prj_default", app: "app_a", asset: "ast_1", pid: "pid_1", kind: "segment", viewer: viewerV1, watch: 6, bytes: 1000},
		{at: "2026-11-10T09:20:00Z", sample: 1, project: "prj_default", app: "app_b", asset: "ast_2", pid: "pid_2", kind: "master", views: 1, bytes: 150},
		{at: "2026-11-10T08:00:00Z", sample: 1, project: "prj_other", app: "app_x", asset: "ast_x", pid: "pid_x", kind: "master", viewer: viewerV2, views: 1, bytes: 150},
		{at: "2026-11-10T08:10:00Z", sample: 1, project: "prj_default", app: "app_a", asset: "ast_1", pid: "pid_1", kind: "segment", viewer: "not-hex", watch: 0, bytes: 0},
	}
	wantHourly := []string{
		"edge 2026-11-10T03:00:00Z app_a ast_1 pid_1 2 30 4000",
		"edge 2026-11-10T07:00:00Z app_a ast_1 pid_1 1 0 150",
		"edge 2026-11-10T08:00:00Z app_a ast_1 pid_1 0 12 2000",
		"edge 2026-11-10T09:00:00Z app_b ast_2 pid_2 1 0 150",
		"origin 2026-11-10T08:00:00Z app_a ast_1 pid_1 1 6 100",
	}
	wantDaily := []string{
		"edge 2026-11-10T00:00:00Z app_a ast_1 pid_1 3 42 6150",
		"edge 2026-11-10T00:00:00Z app_b ast_2 pid_2 1 0 150",
		"origin 2026-11-10T00:00:00Z app_a ast_1 pid_1 1 6 100",
	}
	wantViewers := []string{"2026-11-10T00:00:00Z ast_1 " + viewerV1 + " app_a"}
	wantDays := []string{"2026-11-10T00:00:00Z app_a " + viewerV1}
	check := func(what string) {
		t.Helper()
		same(t, what+": hourly", r.rows(t, hourlySQL), wantHourly)
		same(t, what+": daily", r.rows(t, dailySQL), wantDaily)
		same(t, what+": viewers", r.rows(t, viewerSQL), wantViewers)
		same(t, what+": viewer days", r.rows(t, vdaysSQL), wantDays)
	}
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	check("first pull")
	same(t, "watermark", r.rows(t, `SELECT pulled_through FROM edge_analytics`), []string{"2026-11-10T09:30:00Z"})

	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	check("the same pull again")

	// A point that landed late in the hour before the watermark counts.
	r.fake.points = append(r.fake.points, dataPoint{at: "2026-11-10T08:50:00Z", sample: 1, project: "prj_default", app: "app_a", asset: "ast_1", pid: "pid_1", kind: "segment", viewer: viewerV2, watch: 6, bytes: 1000})
	r.now = mustTime("2026-11-10T09:35:00Z")
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	wantHourly[2] = "edge 2026-11-10T08:00:00Z app_a ast_1 pid_1 0 18 3000"
	wantDaily[0] = "edge 2026-11-10T00:00:00Z app_a ast_1 pid_1 3 48 7150"
	wantViewers = append(wantViewers, "2026-11-10T00:00:00Z ast_1 "+viewerV2+" app_a")
	wantDays = append(wantDays, "2026-11-10T00:00:00Z app_a "+viewerV2)
	slices.Sort(wantViewers)
	slices.Sort(wantDays)
	check("a late point")

	// The API reads both sources and says the edge is collected.
	win, _ := windowOf("24h", r.now)
	out, err := NewAPI(r.pool).read(ctx, scope{project: "prj_default"}, win)
	if err != nil {
		t.Fatal(err)
	}
	if out.Totals.Views != 5 || out.Totals.WatchSeconds != 54 || out.Totals.Bytes != 7400 || out.Collecting.Edge != "connected" {
		t.Fatalf("totals %+v, collecting %+v; want 5 views, 54 s, 7400 bytes, edge connected", out.Totals, out.Collecting)
	}
	if out.Totals.UniqueViewers == nil || *out.Totals.UniqueViewers != 2 {
		t.Fatalf("unique viewers = %v, want 2", out.Totals.UniqueViewers)
	}
}

func TestAFailedPullKeepsItsWatermarkAndSaysWhy(t *testing.T) {
	r := newEdgeRig(t)
	ctx := context.Background()
	r.cloudflare(t)
	if err := r.edge.Connect(ctx, edgeToken); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `UPDATE edge_analytics SET pulled_through = '2026-11-10T08:00:00Z'`)
	r.fake.fail = true
	if _, err := r.edge.Pull(ctx); err == nil {
		t.Fatal("a failed query pulled")
	}
	st, err := r.edge.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.PulledThrough.Equal(mustTime("2026-11-10T08:00:00Z")) || !strings.Contains(st.LastError, "500") || strings.Contains(st.LastError, edgeToken) {
		t.Fatalf("status = %+v; want the watermark kept and the status, never the token", st)
	}
	r.fake.fail = false
	if _, err := r.edge.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.edge.Status(ctx); st.LastError != "" {
		t.Fatalf("a good pull left last_error %q", st.LastError)
	}
}

// Rounding: sampled sums are doubles; a row gets the nearest integer.
func TestSampledSumsRoundToIntegers(t *testing.T) {
	var n flexNumber
	for _, c := range []struct {
		in   string
		want int64
	}{{`"2.9999999"`, 3}, {`12`, 12}, {`"1e3"`, 1000}} {
		if err := json.Unmarshal([]byte(c.in), &n); err != nil || n.int() != c.want {
			t.Errorf("%s = %d (%v), want %d", c.in, n.int(), err, c.want)
		}
	}
	if err := json.Unmarshal([]byte(`"x"`), &n); err == nil {
		t.Error(`"x" decoded`)
	}
}
