package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
)

// seeded analytics, at 09:30 UTC on 10 November 2026:
//
//	ast_1 (app_a, "Lesson 1"): 2026-11-10T09 origin 3/60/1000 + edge 2/30/500,
//	                           2026-11-09T09 origin 1/6/100, 2026-10-30T00 origin 9/0/90
//	ast_2 (app_a, untitled):   2026-11-08T12 origin 4/0/40
//	ast_3 (app_b):             2026-11-10T08 origin 1/0/10
//	ast_del (app_a, deleted):  2026-11-10T08 origin 7/0/70
//
// (views/watch seconds/bytes). Viewers: v1 on ast_1 on the 9th and the 10th,
// v2 on ast_3 on the 10th, v3 on ast_2 on the 3rd.
func seedAnalytics(t *testing.T) (*API, *http.ServeMux, func(sql string, args ...any)) {
	t.Helper()
	pool := dbtest.New(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO apps (id, project_id, name) VALUES ('app_a', 'prj_default', 'a'), ('app_b', 'prj_default', 'b')`)
	exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id, title) VALUES
	      ('ast_1', 'prj_default', 'app_a', 'processing', 'sto_default', 'Lesson 1'),
	      ('ast_2', 'prj_default', 'app_a', 'processing', 'sto_default', NULL),
	      ('ast_3', 'prj_default', 'app_b', 'processing', 'sto_default', NULL),
	      ('ast_del', 'prj_default', 'app_a', 'deleted', 'sto_default', NULL)`)
	exec(`INSERT INTO playback_hourly (project_id, source, hour, app_id, asset_id, playback_id, views, watch_seconds, bytes) VALUES
	      ('prj_default', 'origin', '2026-11-10T09:00:00Z', 'app_a', 'ast_1', 'pid_1', 3, 60, 1000),
	      ('prj_default', 'edge',   '2026-11-10T09:00:00Z', 'app_a', 'ast_1', 'pid_1', 2, 30, 500),
	      ('prj_default', 'origin', '2026-11-09T09:00:00Z', 'app_a', 'ast_1', 'pid_1', 1, 6, 100),
	      ('prj_default', 'origin', '2026-10-30T00:00:00Z', 'app_a', 'ast_1', 'pid_1', 9, 0, 90),
	      ('prj_default', 'origin', '2026-11-08T12:00:00Z', 'app_a', 'ast_2', 'pid_2', 4, 0, 40),
	      ('prj_default', 'origin', '2026-11-10T08:00:00Z', 'app_b', 'ast_3', 'pid_3', 1, 0, 10),
	      ('prj_default', 'origin', '2026-11-10T08:00:00Z', 'app_a', 'ast_del', 'pid_d', 7, 0, 70)`)
	exec(`INSERT INTO playback_viewers (project_id, day, asset_id, viewer, app_id) VALUES
	      ('prj_default', '2026-11-09', 'ast_1', '\x01000000000000000000000000000000', 'app_a'),
	      ('prj_default', '2026-11-10', 'ast_1', '\x01000000000000000000000000000000', 'app_a'),
	      ('prj_default', '2026-11-10', 'ast_3', '\x02000000000000000000000000000000', 'app_b'),
	      ('prj_default', '2026-11-03', 'ast_2', '\x03000000000000000000000000000000', 'app_a')`)
	// The rollups, as a flush writes them alongside.
	exec(`INSERT INTO playback_daily (project_id, source, day, app_id, asset_id, playback_id, views, watch_seconds, bytes)
	      SELECT project_id, source, (hour AT TIME ZONE 'UTC')::date, app_id, asset_id, playback_id, sum(views), sum(watch_seconds), sum(bytes)
	      FROM playback_hourly GROUP BY 1, 2, 3, 4, 5, 6`)
	exec(`INSERT INTO playback_viewer_days (project_id, day, app_id, viewer) SELECT DISTINCT project_id, day, app_id, viewer FROM playback_viewers`)

	auth := apikeys.NewAuthenticator(pool)
	// The owner's session, as ActAsApp answers it; X-No-Session is a
	// request with neither a session nor a key.
	auth.Session = func(r *http.Request) (apikeys.Key, error) {
		if r.Header.Get("X-No-Session") != "" {
			return apikeys.Key{}, apikeys.ErrInvalid
		}
		return apikeys.Key{ID: "owner:own_1", ProjectID: "prj_default", AppID: "app_dashboard", Scopes: apikeys.AllScopes(), Owner: true}, nil
	}
	a := NewAPI(pool)
	a.Auth = auth
	a.Now = func() time.Time { return at("2026-11-10T09:30:00Z") }
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux, exec
}

type totalsJSON struct {
	Views         int64  `json:"views"`
	WatchSeconds  int64  `json:"watch_seconds"`
	UniqueViewers *int64 `json:"unique_viewers"`
	Bytes         int64  `json:"bytes_delivered"`
}

type pointJSON struct {
	T            string `json:"t"`
	Views        int64  `json:"views"`
	WatchSeconds int64  `json:"watch_seconds"`
	Bytes        int64  `json:"bytes_delivered"`
}

type assetJSON struct {
	AssetID      string `json:"asset_id"`
	Title        string `json:"title,omitempty"`
	Views        int64  `json:"views"`
	WatchSeconds int64  `json:"watch_seconds"`
	Bytes        int64  `json:"bytes_delivered"`
}

type analyticsJSON struct {
	Totals     totalsJSON   `json:"totals"`
	Previous   totalsJSON   `json:"previous"`
	Series     []pointJSON  `json:"series"`
	TopAssets  *[]assetJSON `json:"top_assets"`
	Collecting struct {
		Builtin bool   `json:"builtin"`
		Edge    string `json:"edge"`
	} `json:"collecting"`
}

func n(v int64) *int64 { return &v }

func call(t *testing.T, mux *http.ServeMux, path string, header ...string) (int, analyticsJSON, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out analyticsJSON
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", rec.Body, err)
		}
	}
	return rec.Code, out, rec.Body.String()
}

func sameTotals(t *testing.T, what string, got, want totalsJSON) {
	t.Helper()
	viewers := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return strconv.FormatInt(*p, 10)
	}
	if got.Views != want.Views || got.WatchSeconds != want.WatchSeconds || got.Bytes != want.Bytes || viewers(got.UniqueViewers) != viewers(want.UniqueViewers) {
		t.Errorf("%s = %d views, %d s, %d bytes, %s viewers; want %d, %d, %d, %s", what,
			got.Views, got.WatchSeconds, got.Bytes, viewers(got.UniqueViewers),
			want.Views, want.WatchSeconds, want.Bytes, viewers(want.UniqueViewers))
	}
}

// sameSeries checks a zero-filled series: n buckets from first, step
// apart, zero except the buckets in nonzero.
func sameSeries(t *testing.T, got []pointJSON, first string, step time.Duration, buckets int, nonzero map[string][3]int64) {
	t.Helper()
	if len(got) != buckets {
		t.Fatalf("%d buckets, want %d", len(got), buckets)
	}
	for i, p := range got {
		wantT := at(first).Add(time.Duration(i) * step).Format(time.RFC3339)
		v := nonzero[wantT]
		if p.T != wantT || p.Views != v[0] || p.WatchSeconds != v[1] || p.Bytes != v[2] {
			t.Errorf("bucket %d = %+v, want %s %v", i, p, wantT, v)
		}
	}
}

func sameTop(t *testing.T, got *[]assetJSON, want []assetJSON) {
	t.Helper()
	if got == nil {
		t.Fatal("top_assets missing")
	}
	if len(*got) != len(want) {
		t.Fatalf("top_assets = %+v, want %+v", *got, want)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("top_assets[%d] = %+v, want %+v", i, (*got)[i], want[i])
		}
	}
}

func TestAnalyticsForTheProject(t *testing.T) {
	_, mux, _ := seedAnalytics(t)

	code, got, body := call(t, mux, "/v1/analytics?range=24h")
	if code != http.StatusOK {
		t.Fatalf("24h: %d %s", code, body)
	}
	// Every app; the deleted asset's history counts, both sources add.
	sameTotals(t, "24h totals", got.Totals, totalsJSON{13, 90, n(2), 1580})
	sameTotals(t, "24h previous", got.Previous, totalsJSON{5, 6, n(1), 140})
	sameSeries(t, got.Series, "2026-11-09T10:00:00Z", time.Hour, 24, map[string][3]int64{
		"2026-11-10T08:00:00Z": {8, 0, 80},
		"2026-11-10T09:00:00Z": {5, 90, 1500},
	})
	sameTop(t, got.TopAssets, []assetJSON{{"ast_1", "Lesson 1", 5, 90, 1500}, {"ast_3", "", 1, 0, 10}})
	if !got.Collecting.Builtin || got.Collecting.Edge != "not_used" {
		t.Errorf("collecting = %+v", got.Collecting)
	}

	code, got, body = call(t, mux, "/v1/analytics?range=7d")
	if code != http.StatusOK {
		t.Fatalf("7d: %d %s", code, body)
	}
	sameTotals(t, "7d totals", got.Totals, totalsJSON{18, 96, n(2), 1720})
	sameTotals(t, "7d previous", got.Previous, totalsJSON{9, 0, n(1), 90})
	sameSeries(t, got.Series, "2026-11-04T00:00:00Z", 24*time.Hour, 7, map[string][3]int64{
		"2026-11-08T00:00:00Z": {4, 0, 40},
		"2026-11-09T00:00:00Z": {1, 6, 100},
		"2026-11-10T00:00:00Z": {13, 90, 1580},
	})
	sameTop(t, got.TopAssets, []assetJSON{{"ast_1", "Lesson 1", 6, 96, 1600}, {"ast_2", "", 4, 0, 40}, {"ast_3", "", 1, 0, 10}})

	// One app.
	code, got, body = call(t, mux, "/v1/analytics?range=24h&app_id=app_a")
	if code != http.StatusOK {
		t.Fatalf("app_a: %d %s", code, body)
	}
	sameTotals(t, "app_a totals", got.Totals, totalsJSON{12, 90, n(1), 1570})
	sameTotals(t, "app_a previous", got.Previous, totalsJSON{5, 6, n(1), 140})
	sameTop(t, got.TopAssets, []assetJSON{{"ast_1", "Lesson 1", 5, 90, 1500}})

	// Nothing in range at all: zeros, a full zero series, no viewers.
	code, got, _ = call(t, mux, "/v1/analytics?range=24h&app_id=app_nobody")
	if code != http.StatusOK {
		t.Fatalf("app_nobody: %d", code)
	}
	sameTotals(t, "empty totals", got.Totals, totalsJSON{0, 0, nil, 0})
	sameSeries(t, got.Series, "2026-11-09T10:00:00Z", time.Hour, 24, nil)
	sameTop(t, got.TopAssets, nil)
}

func TestAnalyticsForOneAsset(t *testing.T) {
	_, mux, _ := seedAnalytics(t)
	code, got, body := call(t, mux, "/v1/analytics/assets/ast_1?range=7d")
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	sameTotals(t, "ast_1 totals", got.Totals, totalsJSON{6, 96, n(1), 1600})
	// The 30 October row has no viewer: unique_viewers is null, not 0.
	sameTotals(t, "ast_1 previous", got.Previous, totalsJSON{9, 0, nil, 90})
	sameSeries(t, got.Series, "2026-11-04T00:00:00Z", 24*time.Hour, 7, map[string][3]int64{
		"2026-11-09T00:00:00Z": {1, 6, 100},
		"2026-11-10T00:00:00Z": {5, 90, 1500},
	})
	if got.TopAssets != nil {
		t.Errorf("an asset's analytics have top_assets: %+v", *got.TopAssets)
	}
	// A deleted asset keeps its history.
	if code, got, _ := call(t, mux, "/v1/analytics/assets/ast_del?range=24h"); code != http.StatusOK || got.Totals.Views != 7 {
		t.Errorf("deleted asset: %d, %d views", code, got.Totals.Views)
	}
	if code, _, _ := call(t, mux, "/v1/analytics/assets/ast_nope?range=24h"); code != http.StatusNotFound {
		t.Errorf("unknown asset: %d, want 404", code)
	}
}

func TestAnalyticsRefusals(t *testing.T) {
	_, mux, _ := seedAnalytics(t)
	for _, c := range []struct {
		path   string
		header []string
		want   int
	}{
		{"/v1/analytics?range=1y", nil, http.StatusBadRequest},
		{"/v1/analytics", nil, http.StatusBadRequest},
		{"/v1/analytics/assets/ast_1?range=", nil, http.StatusBadRequest},
		{"/v1/analytics?range=24h", []string{"X-No-Session", "1"}, http.StatusUnauthorized},
	} {
		if code, _, body := call(t, mux, c.path, c.header...); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.path, code, body, c.want)
		}
	}
}

func TestAnAnalyticsKeySeesItsOwnAppOnly(t *testing.T) {
	a, mux, _ := seedAnalytics(t)
	ctx := context.Background()
	reader, _, err := apikeys.Create(ctx, a.pool, "prj_default", "app_a", "lms", []string{apikeys.AnalyticsRead})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := apikeys.Create(ctx, a.pool, "prj_default", "app_a", "uploader", []string{apikeys.AssetsRead})
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(k string) []string { return []string{"Authorization", "Bearer " + k} }

	// app_id is ignored: the key's own app.
	code, got, body := call(t, mux, "/v1/analytics?range=24h&app_id=app_b", bearer(reader)...)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	sameTotals(t, "key totals", got.Totals, totalsJSON{12, 90, n(1), 1570})
	if code, _, _ := call(t, mux, "/v1/analytics/assets/ast_1?range=24h", bearer(reader)...); code != http.StatusOK {
		t.Errorf("own asset: %d", code)
	}
	if code, _, _ := call(t, mux, "/v1/analytics/assets/ast_3?range=24h", bearer(reader)...); code != http.StatusNotFound {
		t.Errorf("another app's asset: %d, want 404", code)
	}
	if code, _, body := call(t, mux, "/v1/analytics?range=24h", bearer(plain)...); code != http.StatusForbidden {
		t.Errorf("a key without analytics:read: %d %s, want 403", code, body)
	}
}

func TestCollectingSaysWhenTheEdgeIsNotCounted(t *testing.T) {
	_, mux, exec := seedAnalytics(t)
	exec(`INSERT INTO cloudflare_connections (project_id, account_id, zone_id, hostname, bucket, script_name, token_id, storage_id, worker_version)
	      VALUES ('prj_default', 'acc', 'zone', 'media.example.com', 'b', 'clipmux', 'tok', 'sto_default', '1')`)
	_, got, _ := call(t, mux, "/v1/analytics?range=24h")
	if !got.Collecting.Builtin || got.Collecting.Edge != "not_connected" {
		t.Errorf("collecting = %+v, want builtin and not_connected", got.Collecting)
	}
}
