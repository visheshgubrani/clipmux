package analytics

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Dataset is the Analytics Engine dataset the Worker writes to (D103). One
// serves every install on an account; each point names its project.
const Dataset = "clipmux_delivery"

// DefaultCloudflareAPI is Cloudflare's v4 API.
const DefaultCloudflareAPI = "https://api.cloudflare.com/client/v4"

// The sealed token's table, as secrets.Columns names it.
const edgeSealTable = "edge_analytics"

// A pass recomputes from an hour before the watermark, so points landing
// late still count, and covers at most a day, so catching up stays in
// small queries. History reaches back 13 whole days: hourly rows are kept
// 14 (D105), and a day's edge row is rebuilt from all of its hours.
const (
	edgeLate    = time.Hour
	edgeStep    = 24 * time.Hour
	edgeHistory = 13 * 24 * time.Hour
)

// Errors Connect returns for the owner to correct.
var (
	ErrNoCloudflare     = errors.New("analytics: Cloudflare delivery isn't connected")
	ErrTokenRejected    = errors.New("analytics: Cloudflare rejected the token; it needs Account Analytics: Read")
	ErrEdgeNotConnected = errors.New("analytics: edge analytics isn't connected")
)

// Edge pulls the Worker's data points into the analytics tables (P7-T05).
// The owner connects it with a read-only Cloudflare token, sealed in
// Postgres; a leader loop calls Pull.
type Edge struct {
	// API is Cloudflare's API base; tests point it at a fake.
	API    string
	HTTP   *http.Client
	Now    func() time.Time
	Logger *slog.Logger

	pool      *pgxpool.Pool
	box       *secrets.Box
	projectID string
}

// NewEdge returns the edge puller for a project.
func NewEdge(pool *pgxpool.Pool, box *secrets.Box, projectID string) *Edge {
	return &Edge{
		API: DefaultCloudflareAPI, HTTP: http.DefaultClient, Now: time.Now, Logger: slog.New(slog.DiscardHandler),
		pool: pool, box: box, projectID: projectID,
	}
}

// EdgeStatus is what the Delivery page shows.
type EdgeStatus struct {
	Connected     bool       `json:"connected"`
	AccountID     string     `json:"account_id,omitempty"`
	PulledThrough *time.Time `json:"pulled_through,omitempty"`
	LastPullAt    *time.Time `json:"last_pull_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// Connect checks token against the Cloudflare connection's account and
// keeps it sealed. A new token for a connected project keeps the watermark.
func (e *Edge) Connect(ctx context.Context, token string) error {
	q := db.New(e.pool)
	account, err := q.GetCloudflareAccount(ctx, e.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoCloudflare
	}
	if err != nil {
		return fmt.Errorf("read cloudflare account: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ErrTokenRejected
	}
	check, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := e.tables(check, account, token); err != nil {
		var se *sqlError
		if errors.As(err, &se) && (se.status == http.StatusUnauthorized || se.status == http.StatusForbidden) {
			return ErrTokenRejected
		}
		return err
	}
	start := e.Now().UTC().Truncate(24 * time.Hour).Add(-edgeHistory).Add(edgeLate)
	err = q.SaveEdgeAnalytics(ctx, db.SaveEdgeAnalyticsParams{
		ProjectID: e.projectID, AccountID: account, TokenSealed: e.box.Seal(edgeSealTable, e.projectID, []byte(token)),
		PulledThrough: ts(start),
	})
	if err != nil {
		return fmt.Errorf("save edge analytics: %w", err)
	}
	return nil
}

// Disconnect forgets the token. Rows already pulled stay.
func (e *Edge) Disconnect(ctx context.Context) error {
	n, err := db.New(e.pool).DeleteEdgeAnalytics(ctx, e.projectID)
	if err != nil {
		return fmt.Errorf("delete edge analytics: %w", err)
	}
	if n == 0 {
		return ErrEdgeNotConnected
	}
	return nil
}

// Status reports the connection and the last pass.
func (e *Edge) Status(ctx context.Context) (EdgeStatus, error) {
	row, err := db.New(e.pool).GetEdgeAnalytics(ctx, e.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EdgeStatus{}, nil
	}
	if err != nil {
		return EdgeStatus{}, fmt.Errorf("read edge analytics: %w", err)
	}
	st := EdgeStatus{Connected: true, AccountID: row.AccountID, LastError: row.LastError.String}
	through := row.PulledThrough.Time.UTC()
	st.PulledThrough = &through
	if row.LastPullAt.Valid {
		at := row.LastPullAt.Time.UTC()
		st.LastPullAt = &at
	}
	return st, nil
}

// Pull runs one pass: it recomputes the hours from an hour before the
// watermark to now (at most a day), replaces their edge rows, rebuilds the
// edge daily rows of the days they touch, and adds the edge viewers. It
// returns the rows written. A failure is kept for the status page and
// leaves the watermark where it was.
func (e *Edge) Pull(ctx context.Context) (int, error) {
	q := db.New(e.pool)
	row, err := q.GetEdgeAnalytics(ctx, e.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read edge analytics: %w", err)
	}
	n, err := e.pull(ctx, row)
	if err != nil {
		if mErr := q.MarkEdgeFailed(context.WithoutCancel(ctx), db.MarkEdgeFailedParams{
			ProjectID: e.projectID, LastError: pgtype.Text{String: err.Error(), Valid: true},
		}); mErr != nil {
			e.Logger.ErrorContext(ctx, "mark edge pull failed", "err", mErr)
		}
		return 0, err
	}
	return n, nil
}

func (e *Edge) pull(ctx context.Context, row db.EdgeAnalytic) (int, error) {
	token, err := e.box.Open(edgeSealTable, e.projectID, row.TokenSealed)
	if err != nil {
		return 0, fmt.Errorf("open edge token: %w", err)
	}
	now := e.Now().UTC()
	through := row.PulledThrough.Time.UTC()
	if oldest := now.Truncate(24 * time.Hour).Add(-edgeHistory).Add(edgeLate); through.Before(oldest) {
		through = oldest
	}
	from := through.Add(-edgeLate).Truncate(time.Hour)
	to := through.Add(edgeStep)
	if now.Before(to) {
		to = now
	}
	if !from.Before(to) {
		return 0, nil
	}
	tables, err := e.tables(ctx, row.AccountID, string(token))
	if err != nil {
		return 0, err
	}
	q := db.New(e.pool)
	if !tables[Dataset] {
		// No Worker has written a point yet: there is nothing to pull.
		return 0, q.MarkEdgePulled(ctx, db.MarkEdgePulledParams{ProjectID: e.projectID, PulledThrough: ts(to)})
	}
	var totals []struct {
		Hour       flexNumber `json:"hour"`
		AppID      string     `json:"app_id"`
		AssetID    string     `json:"asset_id"`
		PlaybackID string     `json:"playback_id"`
		Views      flexNumber `json:"views"`
		Watch      flexNumber `json:"watch_seconds"`
		Bytes      flexNumber `json:"bytes"`
	}
	where, err := e.where(from, to)
	if err != nil {
		return 0, err
	}
	if err := e.query(ctx, row.AccountID, string(token), `SELECT toUnixTimestamp(toStartOfHour(timestamp)) AS hour, `+
		`blob2 AS app_id, blob3 AS asset_id, blob4 AS playback_id, `+
		`SUM(_sample_interval * double1) AS views, SUM(_sample_interval * double2) AS watch_seconds, `+
		`SUM(_sample_interval * double3) AS bytes FROM `+Dataset+` WHERE `+where+
		` GROUP BY hour, app_id, asset_id, playback_id FORMAT JSON`, &totals); err != nil {
		return 0, err
	}
	var viewers []struct {
		Day     flexNumber `json:"day"`
		AppID   string     `json:"app_id"`
		AssetID string     `json:"asset_id"`
		Viewer  string     `json:"viewer"`
	}
	if err := e.query(ctx, row.AccountID, string(token), `SELECT toUnixTimestamp(toStartOfDay(timestamp)) AS day, `+
		`blob2 AS app_id, blob3 AS asset_id, blob6 AS viewer FROM `+Dataset+` WHERE `+where+
		` AND blob6 != '' GROUP BY day, app_id, asset_id, viewer FORMAT JSON`, &viewers); err != nil {
		return 0, err
	}

	// The hourly primary key has no app: a playback ID belongs to one.
	type hourKey struct {
		hour       int64
		asset, pid string
	}
	merged := map[hourKey]int{}
	var h db.AddEdgeHourlyParams
	h.ProjectID = e.projectID
	for _, t := range totals {
		k := hourKey{t.Hour.int(), t.AssetID, t.PlaybackID}
		if i, ok := merged[k]; ok {
			h.Views[i] += t.Views.int()
			h.WatchSeconds[i] += t.Watch.int()
			h.Bytes[i] += t.Bytes.int()
			continue
		}
		merged[k] = len(h.Hours)
		h.Hours = append(h.Hours, ts(time.Unix(k.hour, 0).UTC()))
		h.AppIds = append(h.AppIds, t.AppID)
		h.AssetIds = append(h.AssetIds, t.AssetID)
		h.PlaybackIds = append(h.PlaybackIds, t.PlaybackID)
		h.Views = append(h.Views, t.Views.int())
		h.WatchSeconds = append(h.WatchSeconds, t.Watch.int())
		h.Bytes = append(h.Bytes, t.Bytes.int())
	}
	var v db.AddPlaybackViewersParams
	for _, r := range viewers {
		key, err := hex.DecodeString(r.Viewer)
		if err != nil || len(key) != ViewerKeySize {
			continue // not a key the Worker writes
		}
		v.ProjectIds = append(v.ProjectIds, e.projectID)
		v.Days = append(v.Days, date(time.Unix(r.Day.int(), 0).UTC()))
		v.AssetIds = append(v.AssetIds, r.AssetID)
		v.Viewers = append(v.Viewers, key)
		v.AppIds = append(v.AppIds, r.AppID)
	}

	var n int64
	err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.DeleteEdgeHourly(ctx, db.DeleteEdgeHourlyParams{ProjectID: e.projectID, Start: ts(from), Stop: ts(to)}); err != nil {
			return fmt.Errorf("delete edge hours: %w", err)
		}
		a, err := q.AddEdgeHourly(ctx, h)
		if err != nil {
			return fmt.Errorf("add edge hours: %w", err)
		}
		days := db.DeleteEdgeDailyParams{ProjectID: e.projectID, Start: date(from.Truncate(24 * time.Hour)), Stop: date(to.Truncate(24 * time.Hour).Add(24 * time.Hour))}
		if _, err := q.DeleteEdgeDaily(ctx, days); err != nil {
			return fmt.Errorf("delete edge days: %w", err)
		}
		b, err := q.AddEdgeDaily(ctx, db.AddEdgeDailyParams(days))
		if err != nil {
			return fmt.Errorf("add edge days: %w", err)
		}
		c, err := q.AddPlaybackViewers(ctx, v)
		if err != nil {
			return fmt.Errorf("add edge viewers: %w", err)
		}
		d, err := q.AddPlaybackViewerDays(ctx, db.AddPlaybackViewerDaysParams{
			ProjectIds: v.ProjectIds, Days: v.Days, Viewers: v.Viewers, AppIds: v.AppIds,
		})
		if err != nil {
			return fmt.Errorf("add edge viewer days: %w", err)
		}
		n = a + b + c + d
		return q.MarkEdgePulled(ctx, db.MarkEdgePulledParams{ProjectID: e.projectID, PulledThrough: ts(to)})
	})
	return int(n), err
}

var projectIDRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// where filters a pass's points: this project, [from, to). The project ID
// is checked before it goes into the SQL text.
func (e *Edge) where(from, to time.Time) (string, error) {
	if !projectIDRE.MatchString(e.projectID) {
		return "", fmt.Errorf("analytics: project ID %q can't be queried", e.projectID)
	}
	return fmt.Sprintf("blob1 = '%s' AND timestamp >= toDateTime('%s') AND timestamp < toDateTime('%s')",
		e.projectID, from.Format(time.DateTime), to.Format(time.DateTime)), nil
}

// tables lists the account's datasets; it is also the token check.
func (e *Edge) tables(ctx context.Context, account, token string) (map[string]bool, error) {
	var rows []map[string]any
	if err := e.query(ctx, account, token, "SHOW TABLES FORMAT JSON", &rows); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		for _, v := range r {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
	}
	return out, nil
}

// sqlError is a failed SQL API call: its status and the start of the body,
// never the token.
type sqlError struct {
	status int
	body   string
}

func (e *sqlError) Error() string {
	return fmt.Sprintf("analytics engine sql: status %d: %s", e.status, e.body)
}

// query runs one statement against the account's Analytics Engine SQL API
// and decodes its JSON rows into out.
func (e *Edge) query(ctx context.Context, account, token, sql string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	base := strings.TrimRight(e.API, "/")
	if base == "" {
		base = DefaultCloudflareAPI
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/accounts/"+account+"/analytics_engine/sql", strings.NewReader(sql))
	if err != nil {
		return fmt.Errorf("analytics engine sql: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")
	res, err := e.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("analytics engine sql: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("analytics engine sql: read: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return &sqlError{status: res.StatusCode, body: msg}
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("analytics engine sql: decode: %w", err)
	}
	if len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("analytics engine sql: decode rows: %w", err)
	}
	return nil
}

// flexNumber is a JSON number ClickHouse may quote (64-bit integers and
// sums often come as strings).
type flexNumber float64

func (n *flexNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("analytics: %s is not a number", b)
	}
	*n = flexNumber(f)
	return nil
}

// int rounds a sampled sum to the nearest integer.
func (n flexNumber) int() int64 { return int64(math.Round(float64(n))) }
