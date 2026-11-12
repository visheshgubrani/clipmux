package analytics

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// testKey is vectors.json's analytics_key_hex: 0x20 to 0x3f.
var testKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(0x20 + i)
	}
	return k
}()

// stu8f is ViewerKey(testKey, "app_a", "stu_8f3a2c"), from vectors.json.
const stu8f = "4687a687f4ac9c03299afb2103b29a7c"

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func master(pid, vid string) Event {
	return Event{ProjectID: "prj_default", AppID: "app_a", AssetID: "ast_v", PlaybackID: pid, Kind: KindMaster, Bytes: 150, Views: 1, Vid: vid}
}

func audioSegment(pid, vid string) Event {
	return Event{ProjectID: "prj_default", AppID: "app_a", AssetID: "ast_v", PlaybackID: pid, Kind: KindSegment, Bytes: 9, WatchSeconds: 6, Vid: vid}
}

// hourly reads playback_hourly as "source hour asset pid views watch bytes".
func hourly(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT format('%s %s %s %s %s %s %s', source, to_char(hour AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24'), asset_id, playback_id, views, watch_seconds, bytes)
		FROM playback_hourly ORDER BY hour, playback_id`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// daily reads playback_daily as "source day asset pid views watch bytes".
func daily(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return strings1(t, pool, `
		SELECT format('%s %s %s %s %s %s %s', source, day, asset_id, playback_id, views, watch_seconds, bytes)
		FROM playback_daily ORDER BY day, playback_id`)
}

// viewerDays reads playback_viewer_days as "day app viewer".
func viewerDays(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return strings1(t, pool, `SELECT format('%s %s %s', day, app_id, encode(viewer, 'hex')) FROM playback_viewer_days ORDER BY day, viewer`)
}

func strings1(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// viewers reads playback_viewers as "day asset app viewer".
func viewers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT format('%s %s %s %s', day, asset_id, app_id, encode(viewer, 'hex')) FROM playback_viewers ORDER BY day, viewer`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", what, strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
}

func clock(s string) (func() time.Time, func(string)) {
	var mu sync.Mutex
	now := at(s)
	return func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		func(s string) { mu.Lock(); defer mu.Unlock(); now = at(s) }
}

func TestAFlushWritesHourlyTotalsAndViewers(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	r := NewRecorder(pool, testKey)
	now, set := clock("2026-11-10T09:59:58Z")
	r.Now = now

	r.Record(master("pid_signed", "stu_8f3a2c"))
	r.Record(audioSegment("pid_signed", "stu_8f3a2c"))
	r.Record(audioSegment("pid_signed", ""))
	set("2026-11-10T10:00:01Z") // the next hour
	r.Record(audioSegment("pid_signed", "stu_8f3a2c"))
	r.Record(master("pid_public", ""))

	n, err := r.Flush(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 { // three hourly rows, two daily, a viewer and a viewer day
		t.Errorf("Flush wrote %d rows, want 7", n)
	}
	equal(t, "hourly", hourly(t, pool), []string{
		"origin 2026-11-10T09 ast_v pid_signed 1 12 168",
		"origin 2026-11-10T10 ast_v pid_public 1 0 150",
		"origin 2026-11-10T10 ast_v pid_signed 0 6 9",
	})
	equal(t, "daily", daily(t, pool), []string{
		"origin 2026-11-10 ast_v pid_public 1 0 150",
		"origin 2026-11-10 ast_v pid_signed 1 18 177",
	})
	// One viewer that day, seen in two hours.
	equal(t, "viewers", viewers(t, pool), []string{"2026-11-10 ast_v app_a " + stu8f})
	equal(t, "viewer days", viewerDays(t, pool), []string{"2026-11-10 app_a " + stu8f})

	// Nothing recorded since: a flush writes nothing.
	if n, err := r.Flush(ctx); err != nil || n != 0 {
		t.Errorf("empty Flush = %d, %v", n, err)
	}
}

func TestReplicasAddUp(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	now, _ := clock("2026-11-10T09:30:00Z")
	for range 2 { // two replicas, or one replica flushing twice
		r := NewRecorder(pool, testKey)
		r.Now = now
		r.Record(master("pid_signed", "stu_8f3a2c"))
		if _, err := r.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	equal(t, "hourly", hourly(t, pool), []string{"origin 2026-11-10T09 ast_v pid_signed 2 0 300"})
	equal(t, "daily", daily(t, pool), []string{"origin 2026-11-10 ast_v pid_signed 2 0 300"})
	equal(t, "viewers", viewers(t, pool), []string{"2026-11-10 ast_v app_a " + stu8f})
	equal(t, "viewer days", viewerDays(t, pool), []string{"2026-11-10 app_a " + stu8f})
}

func TestViewersAreKeptPerUTCDay(t *testing.T) {
	pool := dbtest.New(t)
	r := NewRecorder(pool, testKey)
	now, set := clock("2026-11-10T23:59:59Z")
	r.Now = now
	r.Record(master("pid_signed", "stu_8f3a2c"))
	set("2026-11-11T00:00:00Z")
	r.Record(audioSegment("pid_signed", "stu_8f3a2c"))
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	equal(t, "viewers", viewers(t, pool), []string{
		"2026-11-10 ast_v app_a " + stu8f,
		"2026-11-11 ast_v app_a " + stu8f,
	})
	equal(t, "daily", daily(t, pool), []string{
		"origin 2026-11-10 ast_v pid_signed 1 0 150",
		"origin 2026-11-11 ast_v pid_signed 0 6 9",
	})
	equal(t, "viewer days", viewerDays(t, pool), []string{
		"2026-11-10 app_a " + stu8f,
		"2026-11-11 app_a " + stu8f,
	})
}

func TestAFailedFlushKeepsItsCounts(t *testing.T) {
	pool := dbtest.New(t)
	r := NewRecorder(pool, testKey)
	r.Now, _ = clock("2026-11-10T09:30:00Z")
	r.Record(master("pid_signed", "stu_8f3a2c"))
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Flush(dead); err == nil {
		t.Fatal("Flush on a cancelled context succeeded")
	}
	r.Record(master("pid_signed", ""))
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	equal(t, "hourly", hourly(t, pool), []string{"origin 2026-11-10T09 ast_v pid_signed 2 0 300"})
	equal(t, "viewers", viewers(t, pool), []string{"2026-11-10 ast_v app_a " + stu8f})
}

func TestAFullRecorderDropsNewKeysAndAsksForAFlush(t *testing.T) {
	pool := dbtest.New(t)
	r := NewRecorder(pool, testKey)
	r.Now, _ = clock("2026-11-10T09:30:00Z")
	r.MaxKeys = 2
	dropped := 0
	r.OnDrop = func() { dropped++ }
	r.Record(master("pid_a", "stu_8f3a2c")) // two keys: an hourly row and a viewer
	r.Record(master("pid_a", ""))           // an existing key still counts
	r.Record(master("pid_b", ""))           // a new key is dropped
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	select {
	case <-r.full:
	default:
		t.Error("a full recorder didn't ask for a flush")
	}
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	equal(t, "hourly", hourly(t, pool), []string{"origin 2026-11-10T09 ast_v pid_a 2 0 300"})
	r.Record(master("pid_b", "")) // room again
	if _, err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	equal(t, "hourly", hourly(t, pool), []string{
		"origin 2026-11-10T09 ast_v pid_a 2 0 300",
		"origin 2026-11-10T09 ast_v pid_b 1 0 150",
	})
}

func TestRunFlushesOnceMoreWhenStopped(t *testing.T) {
	pool := dbtest.New(t)
	r := NewRecorder(pool, testKey)
	r.Now, _ = clock("2026-11-10T09:30:00Z")
	r.Interval = time.Hour // only the last flush runs
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	r.Record(master("pid_signed", ""))
	stop()
	<-done
	equal(t, "hourly", hourly(t, pool), []string{"origin 2026-11-10T09 ast_v pid_signed 1 0 150"})
}

func TestTheAnalyticsKeyIsMadeOnceAndSealed(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	box, err := secrets.NewBox(1, bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	k1, err := LoadKey(ctx, pool, box, "prj_default")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadKey(ctx, pool, box, "prj_default")
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 || !bytes.Equal(k1, k2) {
		t.Fatalf("keys %x and %x: want one 32-byte key", k1, k2)
	}
	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT analytics_key_sealed FROM projects WHERE id = 'prj_default'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, k1) {
		t.Fatal("the analytics key is stored in the clear")
	}
	other, _ := secrets.NewBox(1, bytes.Repeat([]byte{6}, 32))
	if _, err := LoadKey(ctx, pool, other, "prj_default"); err == nil {
		t.Fatal("another secrets key opened the analytics key")
	}
}
