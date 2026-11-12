package analytics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// API serves GET /v1/analytics and /v1/analytics/assets/{id} (D100, D103).
// The owner's session sees every app, or the one app_id names; an API key
// with analytics:read sees its own app only.
type API struct {
	Auth   *apikeys.Authenticator
	Now    func() time.Time
	Logger *slog.Logger
	pool   *pgxpool.Pool
}

// NewAPI returns the analytics API; serve sets Auth.Session.
func NewAPI(pool *pgxpool.Pool) *API {
	return &API{Auth: apikeys.NewAuthenticator(pool), Now: time.Now, Logger: slog.New(slog.DiscardHandler), pool: pool}
}

// Register adds the analytics routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/analytics", a.project)
	mux.HandleFunc("GET /v1/analytics/assets/{id}", a.asset)
}

// window is a range in buckets: [start, stop) now, [prevStart, start)
// before it. The last bucket is the one now falls in, so it is partial.
type window struct {
	start, stop, prevStart time.Time
	step                   time.Duration
	buckets                int
}

const day = 24 * time.Hour

func windowOf(rng string, now time.Time) (window, bool) {
	now = now.UTC()
	step, n := day, 0
	switch rng {
	case "24h":
		step, n = time.Hour, 24
	case "7d":
		n = 7
	case "30d":
		n = 30
	case "90d":
		n = 90
	default:
		return window{}, false
	}
	stop := now.Truncate(step).Add(step)
	start := stop.Add(-time.Duration(n) * step)
	return window{start: start, stop: stop, prevStart: start.Add(-time.Duration(n) * step), step: step, buckets: n}, true
}

// Viewers are kept per UTC day, so unique viewers count the days a range
// touches: for 24h, yesterday and today.
func (w window) curFrom() time.Time  { return w.start.Truncate(day) }
func (w window) curTo() time.Time    { return w.stop.Add(-time.Nanosecond).Truncate(day) }
func (w window) prevFrom() time.Time { return w.prevStart.Truncate(day) }
func (w window) prevTo() time.Time   { return w.start.Add(-time.Nanosecond).Truncate(day) }

type totalsView struct {
	Views         int64  `json:"views"`
	WatchSeconds  int64  `json:"watch_seconds"`
	UniqueViewers *int64 `json:"unique_viewers"`
	Bytes         int64  `json:"bytes_delivered"`
}

type pointView struct {
	T            time.Time `json:"t"`
	Views        int64     `json:"views"`
	WatchSeconds int64     `json:"watch_seconds"`
	Bytes        int64     `json:"bytes_delivered"`
}

type assetView struct {
	AssetID      string `json:"asset_id"`
	Title        string `json:"title,omitempty"`
	Views        int64  `json:"views"`
	WatchSeconds int64  `json:"watch_seconds"`
	Bytes        int64  `json:"bytes_delivered"`
}

type collectingView struct {
	Builtin bool `json:"builtin"`
	// Edge is "not_used" without Cloudflare delivery, "not_connected" with
	// it and no analytics token (T05), or "connected".
	Edge string `json:"edge"`
}

type analyticsView struct {
	Totals     totalsView     `json:"totals"`
	Previous   totalsView     `json:"previous"`
	Series     []pointView    `json:"series"`
	TopAssets  *[]assetView   `json:"top_assets,omitempty"`
	Collecting collectingView `json:"collecting"`
}

// scope is what one request may see.
type scope struct {
	project string
	app     pgtype.Text // every app when not Valid
	asset   pgtype.Text
}

// begin authenticates r and reads its range; on failure it has answered.
func (a *API) begin(w http.ResponseWriter, r *http.Request) (scope, window, bool) {
	key, ok := a.Auth.Require(w, r, apikeys.AnalyticsRead)
	if !ok {
		return scope{}, window{}, false
	}
	win, ok := windowOf(r.URL.Query().Get("range"), a.Now())
	if !ok {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "range is 24h, 7d, 30d or 90d")
		return scope{}, window{}, false
	}
	s := scope{project: key.ProjectID}
	switch {
	case !key.Owner:
		s.app = pgtype.Text{String: key.AppID, Valid: true}
	case r.URL.Query().Get("app_id") != "":
		s.app = pgtype.Text{String: r.URL.Query().Get("app_id"), Valid: true}
	}
	return s, win, true
}

func (a *API) project(w http.ResponseWriter, r *http.Request) {
	s, win, ok := a.begin(w, r)
	if !ok {
		return
	}
	a.answer(w, r, s, win)
}

func (a *API) asset(w http.ResponseWriter, r *http.Request) {
	s, win, ok := a.begin(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	app, err := db.New(a.pool).GetAnalyticsAsset(r.Context(), db.GetAnalyticsAssetParams{ProjectID: s.project, ID: id})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && s.app.Valid && app != s.app.String) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	s.asset = pgtype.Text{String: id, Valid: true}
	a.answer(w, r, s, win)
}

func (a *API) answer(w http.ResponseWriter, r *http.Request, s scope, win window) {
	out, err := a.read(r.Context(), s, win)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	a.Logger.ErrorContext(r.Context(), "analytics api", "route", r.Pattern, "err", err)
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func date(t time.Time) pgtype.Date      { return pgtype.Date{Time: t, Valid: true} }

// point is one bucket's sums, from either granularity.
type point struct {
	t                    time.Time
	views, watch, nbytes int64
}

func (a *API) read(ctx context.Context, s scope, win window) (analyticsView, error) {
	q := db.New(a.pool)
	// One series over both ranges: the buckets before start are previous.
	points, err := a.series(ctx, q, s, win)
	if err != nil {
		return analyticsView{}, fmt.Errorf("read series: %w", err)
	}
	out := analyticsView{Series: make([]pointView, win.buckets), Collecting: collectingView{Builtin: true, Edge: "not_used"}}
	for i := range out.Series {
		out.Series[i].T = win.start.Add(time.Duration(i) * win.step)
	}
	for _, p := range points {
		t := &out.Totals
		if p.t.Before(win.start) {
			t = &out.Previous
		} else {
			b := &out.Series[int(p.t.Sub(win.start)/win.step)]
			b.Views, b.WatchSeconds, b.Bytes = p.views, p.watch, p.nbytes
		}
		t.Views += p.views
		t.WatchSeconds += p.watch
		t.Bytes += p.nbytes
	}

	cur, prev, err := a.viewers(ctx, q, s, win)
	if err != nil {
		return analyticsView{}, fmt.Errorf("count viewers: %w", err)
	}
	// No viewer key means no view carried a vid: unknown, not zero.
	if cur > 0 {
		out.Totals.UniqueViewers = &cur
	}
	if prev > 0 {
		out.Previous.UniqueViewers = &prev
	}

	if !s.asset.Valid {
		top, err := a.top(ctx, q, s, win)
		if err != nil {
			return analyticsView{}, fmt.Errorf("top assets: %w", err)
		}
		out.TopAssets = &top
	}

	edge, err := q.EdgeDeliveryOn(ctx, s.project)
	if err != nil {
		return analyticsView{}, fmt.Errorf("read edge delivery: %w", err)
	}
	if edge {
		out.Collecting.Edge = "not_connected"
		pulled, err := q.EdgeAnalyticsConnected(ctx, s.project)
		if err != nil {
			return analyticsView{}, fmt.Errorf("read edge analytics: %w", err)
		}
		if pulled {
			out.Collecting.Edge = "connected"
		}
	}
	return out, nil
}

// series reads [prevStart, stop): hourly rows for hourly buckets, daily
// rows otherwise.
func (a *API) series(ctx context.Context, q *db.Queries, s scope, win window) ([]point, error) {
	var out []point
	if win.step == time.Hour {
		start, stop := ts(win.prevStart), ts(win.stop)
		var rows []db.HourlySeriesRow
		var err error
		if s.asset.Valid {
			var ar []db.AssetHourlySeriesRow
			ar, err = q.AssetHourlySeries(ctx, db.AssetHourlySeriesParams{ProjectID: s.project, AssetID: s.asset.String, Start: start, Stop: stop})
			for _, r := range ar {
				rows = append(rows, db.HourlySeriesRow(r))
			}
		} else {
			rows, err = q.HourlySeries(ctx, db.HourlySeriesParams{ProjectID: s.project, Start: start, Stop: stop, AppID: s.app})
		}
		for _, r := range rows {
			out = append(out, point{r.Bucket.Time.UTC(), r.Views, r.WatchSeconds, r.Bytes})
		}
		return out, err
	}
	start, stop := date(win.prevStart), date(win.stop)
	var rows []db.DailySeriesRow
	var err error
	if s.asset.Valid {
		var ar []db.AssetDailySeriesRow
		ar, err = q.AssetDailySeries(ctx, db.AssetDailySeriesParams{ProjectID: s.project, AssetID: s.asset.String, Start: start, Stop: stop})
		for _, r := range ar {
			rows = append(rows, db.DailySeriesRow(r))
		}
	} else {
		rows, err = q.DailySeries(ctx, db.DailySeriesParams{ProjectID: s.project, Start: start, Stop: stop, AppID: s.app})
	}
	for _, r := range rows {
		out = append(out, point{r.Day.Time.UTC(), r.Views, r.WatchSeconds, r.Bytes})
	}
	return out, err
}

func (a *API) viewers(ctx context.Context, q *db.Queries, s scope, win window) (cur, prev int64, err error) {
	days := struct{ curFrom, curTo, prevFrom, prevTo pgtype.Date }{
		date(win.curFrom()), date(win.curTo()), date(win.prevFrom()), date(win.prevTo()),
	}
	if s.asset.Valid {
		v, err := q.CountAssetViewers(ctx, db.CountAssetViewersParams{
			ProjectID: s.project, AssetID: s.asset.String,
			CurFrom: days.curFrom, CurTo: days.curTo, PrevFrom: days.prevFrom, PrevTo: days.prevTo,
		})
		return v.Current, v.Previous, err
	}
	v, err := q.CountViewers(ctx, db.CountViewersParams{
		ProjectID: s.project, AppID: s.app,
		CurFrom: days.curFrom, CurTo: days.curTo, PrevFrom: days.prevFrom, PrevTo: days.prevTo,
	})
	return v.Current, v.Previous, err
}

func (a *API) top(ctx context.Context, q *db.Queries, s scope, win window) ([]assetView, error) {
	var rows []db.TopAssetsDailyRow
	var err error
	if win.step == time.Hour {
		var hr []db.TopAssetsHourlyRow
		hr, err = q.TopAssetsHourly(ctx, db.TopAssetsHourlyParams{ProjectID: s.project, Start: ts(win.start), Stop: ts(win.stop), AppID: s.app})
		for _, r := range hr {
			rows = append(rows, db.TopAssetsDailyRow(r))
		}
	} else {
		rows, err = q.TopAssetsDaily(ctx, db.TopAssetsDailyParams{ProjectID: s.project, Start: date(win.start), Stop: date(win.stop), AppID: s.app})
	}
	list := make([]assetView, 0, len(rows))
	for _, t := range rows {
		list = append(list, assetView{AssetID: t.AssetID, Title: t.Title.String, Views: t.Views, WatchSeconds: t.WatchSeconds, Bytes: t.Bytes})
	}
	return list, err
}
