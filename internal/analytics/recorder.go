package analytics

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// Defaults for a Recorder.
const (
	// FlushInterval is how often counts reach Postgres; a crash loses at
	// most this much.
	FlushInterval = 15 * time.Second
	// MaxKeys bounds the hourly rows and viewers held between flushes.
	MaxKeys = 100_000
	// flushWait bounds one flush, and the last one at shutdown.
	flushWait = 10 * time.Second
)

// Recorder aggregates one replica's events in memory and flushes them as
// additive origin rows, so replicas never coordinate.
type Recorder struct {
	Now      func() time.Time
	Logger   *slog.Logger
	Interval time.Duration
	MaxKeys  int
	// OnFlush is told how many rows each flush wrote; OnDrop, each key
	// dropped because the recorder was full.
	OnFlush func(rows int)
	OnDrop  func()

	pool *pgxpool.Pool
	key  []byte
	full chan struct{} // a full recorder asks Run for an early flush

	mu      sync.Mutex
	hourly  map[hourKey]totals
	viewers map[viewerKey]string // the app ID
}

type hourKey struct {
	project, app, asset, playback string
	hour                          int64 // Unix seconds, on the hour
}

type totals struct{ views, watch, bytes int64 }

type viewerKey struct {
	project, asset string
	day            int64 // Unix seconds, on the UTC day
	viewer         [ViewerKeySize]byte
}

// NewRecorder returns a recorder whose viewer keys are made under key, the
// project's analytics key (LoadKey).
func NewRecorder(pool *pgxpool.Pool, key []byte) *Recorder {
	return &Recorder{
		Now: time.Now, Logger: slog.New(slog.DiscardHandler), Interval: FlushInterval, MaxKeys: MaxKeys,
		pool: pool, key: key, full: make(chan struct{}, 1),
		hourly: map[hourKey]totals{}, viewers: map[viewerKey]string{},
	}
}

// Record counts one event at the current time. It never blocks on
// Postgres; when the recorder is full, new keys are dropped.
func (r *Recorder) Record(e Event) {
	now := r.Now().UTC()
	hk := hourKey{e.ProjectID, e.AppID, e.AssetID, e.PlaybackID, now.Truncate(time.Hour).Unix()}
	var vk viewerKey
	if e.Vid != "" {
		vk = viewerKey{project: e.ProjectID, asset: e.AssetID, day: now.Truncate(24 * time.Hour).Unix()}
		copy(vk.viewer[:], ViewerKey(r.key, e.AppID, e.Vid))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.hourly[hk]; ok || r.room() {
		r.hourly[hk] = totals{t.views + e.Views, t.watch + e.WatchSeconds, t.bytes + e.Bytes}
	} else {
		r.dropped()
	}
	if e.Vid == "" {
		return
	}
	if _, ok := r.viewers[vk]; ok || r.room() {
		r.viewers[vk] = e.AppID
	} else {
		r.dropped()
	}
}

// room reports whether another key fits; it asks for a flush when one
// won't. Callers hold mu.
func (r *Recorder) room() bool {
	if len(r.hourly)+len(r.viewers) < r.MaxKeys {
		return true
	}
	select {
	case r.full <- struct{}{}:
	default:
	}
	return false
}

func (r *Recorder) dropped() {
	if r.OnDrop != nil {
		r.OnDrop()
	}
}

// Flush writes everything recorded so far in one transaction and returns
// the rows it wrote. On failure the counts are kept for the next flush.
func (r *Recorder) Flush(ctx context.Context) (int, error) {
	r.mu.Lock()
	hourly, viewers := r.hourly, r.viewers
	r.hourly, r.viewers = map[hourKey]totals{}, map[viewerKey]string{}
	r.mu.Unlock()
	if len(hourly) == 0 && len(viewers) == 0 {
		return 0, nil
	}
	n, err := r.write(ctx, hourly, viewers)
	if err != nil {
		r.restore(hourly, viewers)
		return 0, err
	}
	if r.OnFlush != nil {
		r.OnFlush(n)
	}
	return n, nil
}

func (r *Recorder) write(ctx context.Context, hourly map[hourKey]totals, viewers map[viewerKey]string) (int, error) {
	var h db.AddOriginPlaybackParams
	for k, t := range hourly {
		h.ProjectIds = append(h.ProjectIds, k.project)
		h.Hours = append(h.Hours, pgtype.Timestamptz{Time: time.Unix(k.hour, 0).UTC(), Valid: true})
		h.AppIds = append(h.AppIds, k.app)
		h.AssetIds = append(h.AssetIds, k.asset)
		h.PlaybackIds = append(h.PlaybackIds, k.playback)
		h.Views = append(h.Views, t.views)
		h.WatchSeconds = append(h.WatchSeconds, t.watch)
		h.Bytes = append(h.Bytes, t.bytes)
	}
	var v db.AddPlaybackViewersParams
	for k, app := range viewers {
		v.ProjectIds = append(v.ProjectIds, k.project)
		v.Days = append(v.Days, pgtype.Date{Time: time.Unix(k.day, 0).UTC(), Valid: true})
		v.AssetIds = append(v.AssetIds, k.asset)
		v.Viewers = append(v.Viewers, k.viewer[:])
		v.AppIds = append(v.AppIds, app)
	}
	var n int64
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		a, err := q.AddOriginPlayback(ctx, h)
		if err != nil {
			return fmt.Errorf("add hourly playback: %w", err)
		}
		b, err := q.AddOriginPlaybackDaily(ctx, db.AddOriginPlaybackDailyParams(h))
		if err != nil {
			return fmt.Errorf("add daily playback: %w", err)
		}
		c, err := q.AddPlaybackViewers(ctx, v)
		if err != nil {
			return fmt.Errorf("add playback viewers: %w", err)
		}
		d, err := q.AddPlaybackViewerDays(ctx, db.AddPlaybackViewerDaysParams{
			ProjectIds: v.ProjectIds, Days: v.Days, Viewers: v.Viewers, AppIds: v.AppIds,
		})
		if err != nil {
			return fmt.Errorf("add viewer days: %w", err)
		}
		n = a + b + c + d
		return nil
	})
	return int(n), err
}

// restore puts back what a failed flush took, under the same cap.
func (r *Recorder) restore(hourly map[hourKey]totals, viewers map[viewerKey]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range hourly {
		if c, ok := r.hourly[k]; ok || r.room() {
			r.hourly[k] = totals{c.views + t.views, c.watch + t.watch, c.bytes + t.bytes}
		} else {
			r.dropped()
		}
	}
	for k, app := range viewers {
		if _, ok := r.viewers[k]; ok || r.room() {
			r.viewers[k] = app
		} else {
			r.dropped()
		}
	}
}

// Run flushes every Interval, or sooner when the recorder fills, until ctx
// ends; then it flushes once more. serve cancels ctx only after the HTTP
// server has shut down, so that last flush holds every request's counts.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushWait)
			r.flush(last)
			cancel()
			return
		case <-t.C:
		case <-r.full:
		}
		pass, cancel := context.WithTimeout(ctx, flushWait)
		r.flush(pass)
		cancel()
	}
}

func (r *Recorder) flush(ctx context.Context) {
	if _, err := r.Flush(ctx); err != nil {
		r.Logger.Error("analytics flush", "err", err)
	}
}

// sealedTable is what the analytics key is sealed under (secrets.Columns).
const sealedTable = "projects_analytics"

// LoadKey returns a project's analytics key, creating it on first boot.
func LoadKey(ctx context.Context, conn db.DBTX, box *secrets.Box, projectID string) ([]byte, error) {
	q := db.New(conn)
	sealed, err := q.GetAnalyticsKey(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("read analytics key: %w", err)
	}
	if sealed == nil {
		key := make([]byte, 32)
		_, _ = rand.Read(key) // never fails (crypto/rand)
		if err := q.SetAnalyticsKeyIfUnset(ctx, db.SetAnalyticsKeyIfUnsetParams{
			ID: projectID, Sealed: box.Seal(sealedTable, projectID, key),
		}); err != nil {
			return nil, fmt.Errorf("store analytics key: %w", err)
		}
		// Another serve may have stored one first; read back what won.
		if sealed, err = q.GetAnalyticsKey(ctx, projectID); err != nil {
			return nil, fmt.Errorf("read analytics key: %w", err)
		}
	}
	key, err := box.Open(sealedTable, projectID, sealed)
	if err != nil {
		return nil, fmt.Errorf("open analytics key: %w", err)
	}
	return key, nil
}
