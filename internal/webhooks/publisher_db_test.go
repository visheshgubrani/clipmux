package webhooks

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/db/dbtest"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

var t0 = time.Unix(1700000000, 0).UTC()

type rig struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *db.Queries
	box  *secrets.Box
	pub  *Publisher
	now  time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pool := dbtest.New(t)
	box, err := secrets.NewBox(1, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, pool: pool, q: db.New(pool), box: box, now: t0}
	r.pub = New(pool, box)
	r.pub.Now = func() time.Time { return r.now }
	return r
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), sql, args...); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) app(id string) string {
	r.exec(`INSERT INTO apps (id, project_id, name) VALUES ($1, 'prj_default', $1)`, id)
	return id
}

func (r *rig) endpoint(appID, url string) (string, string) {
	r.t.Helper()
	ep, secret, err := CreateEndpoint(context.Background(), r.q, r.box, "prj_default", appID, url)
	if err != nil {
		r.t.Fatal(err)
	}
	return ep.ID, secret
}

// event queues a ready event for a new asset of appID and returns its ID.
func (r *rig) event(appID, assetID string) string {
	r.t.Helper()
	r.exec(`INSERT INTO assets (id, project_id, app_id, status, storage_id) VALUES ($1, 'prj_default', $2, 'processing', 'sto_default')`, assetID, appID)
	if err := outbox.AssetEvent(context.Background(), r.q, assetID, outbox.AssetReady, nil); err != nil {
		r.t.Fatal(err)
	}
	var id string
	if err := r.pool.QueryRow(context.Background(), `SELECT id FROM outbox WHERE asset_id = $1 ORDER BY created_at DESC LIMIT 1`, assetID).Scan(&id); err != nil {
		r.t.Fatal(err)
	}
	return id
}

func (r *rig) run() int {
	r.t.Helper()
	n, err := r.pub.Run(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return n
}

type delivery struct {
	Endpoint, Status, LastError string
	Attempts, LastStatus        int32
	Next, GiveUp, Finished      time.Time
}

func (r *rig) deliveries() []delivery {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `
		SELECT endpoint_id, status, coalesce(last_error, ''), attempts, coalesce(last_status, 0),
		       coalesce(next_attempt_at, 'epoch'), give_up_at, coalesce(finished_at, 'epoch')
		FROM webhook_deliveries ORDER BY created_at, id`)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []delivery
	for rows.Next() {
		var d delivery
		if err := rows.Scan(&d.Endpoint, &d.Status, &d.LastError, &d.Attempts, &d.LastStatus, &d.Next, &d.GiveUp, &d.Finished); err != nil {
			r.t.Fatal(err)
		}
		d.Next, d.GiveUp, d.Finished = d.Next.UTC(), d.GiveUp.UTC(), d.Finished.UTC()
		out = append(out, d)
	}
	return out
}

// receiver records requests and answers with the next status (the last one
// repeats).
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	got      []recorded
}

type recorded struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	rc := &receiver{statuses: statuses}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, recorded{req.Header.Clone(), body})
		status := rc.statuses[min(len(rc.got), len(rc.statuses))-1]
		rc.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) requests() []recorded {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]recorded(nil), rc.got...)
}

func TestEventsFanOutToEachEnabledEndpointOfTheirAppOnly(t *testing.T) {
	r := newRig(t)
	lms := r.app("app_lms")
	r.app("app_quiet")
	live, _ := r.endpoint(lms, "https://lms.example/hooks")
	off, _ := r.endpoint(lms, "https://lms.example/old")
	r.exec(`UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, off)
	r.endpoint("app_dashboard", "https://dash.example/hooks")

	r.event(lms, "ast_1")
	quiet := r.event("app_quiet", "ast_2")
	for range 2 { // a second pass must not duplicate anything
		if _, err := r.pub.Fanout(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
	}
	got := r.deliveries()
	if len(got) != 1 || got[0].Endpoint != live || got[0].Status != "pending" || !got[0].Next.Equal(t0) || !got[0].GiveUp.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("deliveries = %+v, want one pending for %s, due now, giving up in 24 h", got, live)
	}
	var pending int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE kind = 'webhook' AND done_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("undone events = %d, %v; an event with no endpoints (%s) is done too", pending, err, quiet)
	}
}

func TestDeliveryIsSignedWithAFreshTimestampAndTheEventID(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, http.StatusInternalServerError, http.StatusNoContent)
	ep, secret := r.endpoint("app_dashboard", rc.URL+"/hooks")
	evt := r.event("app_dashboard", "ast_1")
	var stored []byte
	if err := r.pool.QueryRow(context.Background(), `SELECT payload FROM outbox WHERE id = $1`, evt).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	r.run()
	r.now = t0.Add(30 * time.Second)
	r.run()

	reqs := rc.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (a 500, then the retry)", len(reqs))
	}
	for i, ts := range []string{"1700000000", "1700000030"} {
		h, body := reqs[i].header, reqs[i].body
		want := map[string]string{
			"Content-Type": "application/json", "User-Agent": "ClipMux-Webhooks/1.0",
			"X-Webhook-Event": "video.asset.ready", "X-Webhook-Id": evt, "X-Webhook-Timestamp": ts,
			"X-Webhook-Signature": Sign([]byte(secret), t0.Unix()+int64(30*i), body),
		}
		for k, v := range want {
			if h.Get(k) != v {
				t.Errorf("try %d: %s = %q, want %q", i+1, k, h.Get(k), v)
			}
		}
		if string(body) != string(stored) {
			t.Errorf("try %d: body differs from the stored event:\n%s\n%s", i+1, body, stored)
		}
	}
	if reqs[0].header.Get("X-Webhook-Signature") == reqs[1].header.Get("X-Webhook-Signature") {
		t.Error("each try is signed over its own timestamp")
	}
	d := r.deliveries()
	if len(d) != 1 || d[0].Endpoint != ep || d[0].Status != "delivered" || d[0].Attempts != 2 || d[0].LastStatus != 204 || d[0].LastError != "" || !d[0].Finished.Equal(r.now) {
		t.Fatalf("delivery = %+v, want delivered on the second try with 204", d)
	}
}

func TestFailedDeliveriesBackOffForADayThenStop(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, http.StatusInternalServerError)
	r.endpoint("app_dashboard", rc.URL)
	r.event("app_dashboard", "ast_1")

	want := []string{"30s", "2m30s", "12m30s", "42m30s", "1h42m30s", "3h42m30s", "7h42m30s", "15h42m30s", "23h42m30s", "24h0m0s"}
	var offsets []string
	for range 20 {
		r.run()
		d := r.deliveries()[0]
		if d.Status != "pending" {
			break
		}
		if !d.Next.After(r.now) {
			// Due but not tried: by its tenth failure the endpoint's breaker
			// is open for 32 minutes, so the last try waits for it to close.
			r.now = r.now.Add(time.Hour)
			continue
		}
		offsets = append(offsets, d.Next.Sub(t0).String())
		r.now = d.Next
	}
	if len(offsets) != len(want) {
		t.Fatalf("retry offsets = %v, want %v", offsets, want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Fatalf("retry offsets = %v, want %v", offsets, want)
		}
	}
	d := r.deliveries()[0]
	if d.Status != "failed" || d.Attempts != 11 || d.LastStatus != 500 || d.LastError != "http_500" || len(rc.requests()) != 11 {
		t.Fatalf("after a day: %+v with %d requests, want failed after 11 tries with http_500", d, len(rc.requests()))
	}
}

func TestRedirectsAreFailuresAndNeverFollowed(t *testing.T) {
	r := newRig(t)
	target := newReceiver(t, http.StatusOK)
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	r.endpoint("app_dashboard", redirect.URL)
	r.event("app_dashboard", "ast_1")
	r.run()
	if n := len(target.requests()); n != 0 {
		t.Fatalf("the redirect target got %d requests, want 0", n)
	}
	if d := r.deliveries()[0]; d.Status != "pending" || d.LastStatus != 302 || d.LastError != "http_302" {
		t.Fatalf("delivery = %+v, want a retry recorded as http_302", d)
	}
}

func TestASlowReceiverTimesOut(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	r.pub.Timeout = 50 * time.Millisecond
	r.endpoint("app_dashboard", slow.URL)
	r.event("app_dashboard", "ast_1")
	r.run()
	if d := r.deliveries()[0]; d.Status != "pending" || d.LastError != "timeout" || d.LastStatus != 0 || !d.Next.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("delivery = %+v, want a timeout retried in 30 s", d)
	}
}

func TestDisabledEndpointsAndAppsGetNothing(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, http.StatusOK)
	ep, _ := r.endpoint("app_dashboard", rc.URL)
	off := r.app("app_off")
	r.endpoint(off, rc.URL)
	r.event("app_dashboard", "ast_1")
	r.event(off, "ast_2")
	if _, err := r.pub.Fanout(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE webhook_endpoints SET disabled_at = now() WHERE id = $1`, ep)
	r.exec(`UPDATE apps SET disabled_at = now() WHERE id = $1`, off)
	r.run()
	if n := len(rc.requests()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
	got := r.deliveries()
	if len(got) != 2 || got[0].Status != "failed" || got[0].LastError != "endpoint_disabled" || got[1].Status != "failed" || got[1].LastError != "app_disabled" {
		t.Fatalf("deliveries = %+v, want endpoint_disabled and app_disabled", got)
	}
}

func TestAClaimedDeliveryIsResentAfterItsLease(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, http.StatusOK)
	r.endpoint("app_dashboard", rc.URL)
	r.event("app_dashboard", "ast_1")
	ctx := context.Background()
	if _, err := r.pub.Fanout(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// A sender claims the delivery and dies before recording anything.
	claimed, err := r.pub.claim(ctx)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %d, %v", len(claimed), err)
	}
	r.run()
	if n := len(rc.requests()); n != 0 {
		t.Fatalf("a leased delivery was sent again before its lease ended (%d requests)", n)
	}
	r.now = t0.Add(61 * time.Second)
	r.run()
	d := r.deliveries()[0]
	if len(rc.requests()) != 1 || d.Status != "delivered" || d.Attempts != 2 {
		t.Fatalf("after the lease: %d requests, %+v; want it sent once more and delivered", len(rc.requests()), d)
	}
	// The dead sender's late result must not overwrite the newer try.
	if r.pub.record(ctx, claimed[0], http.StatusInternalServerError, "http_500") {
		t.Fatal("a stale sender recorded a result")
	}
	if d := r.deliveries()[0]; d.Status != "delivered" {
		t.Fatalf("stale result changed the delivery: %+v", d)
	}
}

func TestAFailingEndpointIsSkippedUntilItsBreakerCloses(t *testing.T) {
	r := newRig(t)
	dead := newReceiver(t, 500, 500, 500, 500, 500, 200)
	fine := newReceiver(t, 200)
	deadID, _ := r.endpoint("app_dashboard", dead.URL)
	r.endpoint("app_dashboard", fine.URL)
	r.pub.Concurrency = 1 // tries in order, so the fifth failure is the fifth try
	for i := range BreakerAfter {
		r.event("app_dashboard", "ast_"+string(rune('a'+i)))
	}
	r.run()
	if n := len(dead.requests()); n != BreakerAfter {
		t.Fatalf("dead endpoint tried %d times, want %d", n, BreakerAfter)
	}
	var failures int
	var open time.Time
	if err := r.pool.QueryRow(context.Background(), `SELECT consecutive_failures, circuit_open_until FROM webhook_endpoints WHERE id = $1`, deadID).Scan(&failures, &open); err != nil {
		t.Fatal(err)
	}
	if failures != BreakerAfter || !open.Equal(r.now.Add(time.Minute)) {
		t.Fatalf("breaker: %d failures, open until %v; want %d and now + 1 min", failures, open, BreakerAfter)
	}

	r.event("app_dashboard", "ast_late")
	r.run()
	if n := len(dead.requests()); n != BreakerAfter {
		t.Fatalf("an open breaker was tried: %d requests", n)
	}
	if n := len(fine.requests()); n != BreakerAfter+1 {
		t.Fatalf("the healthy endpoint got %d, want %d: one endpoint's breaker must not hold up another", n, BreakerAfter+1)
	}
	var charged int
	if err := r.pool.QueryRow(context.Background(), `SELECT d.attempts FROM webhook_deliveries d JOIN outbox e ON e.id = d.event_id WHERE d.endpoint_id = $1 AND e.asset_id = 'ast_late'`, deadID).Scan(&charged); err != nil {
		t.Fatal(err)
	}
	if charged != 0 {
		t.Fatalf("a skipped delivery was charged %d tries", charged)
	}

	r.now = r.now.Add(time.Minute + time.Second) // the breaker has closed; the probe succeeds
	r.run()
	if err := r.pool.QueryRow(context.Background(), `SELECT consecutive_failures, circuit_open_until IS NULL FROM webhook_endpoints WHERE id = $1`, deadID).Scan(&failures, new(bool)); err != nil {
		t.Fatal(err)
	}
	if failures != 0 {
		t.Fatalf("a success left %d failures counted", failures)
	}
}

func TestASecretSignsOnlyOnceActivated(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, 200)
	id, old := r.endpoint("app_dashboard", rc.URL)
	ctx := context.Background()
	if err := ActivateSecret(ctx, r.q, r.box, "prj_default", id); !errors.Is(err, ErrNoPendingSecret) {
		t.Fatalf("activate with nothing pending: %v", err)
	}
	fresh, err := RotateSecret(ctx, r.q, r.box, "prj_default", id)
	if err != nil || !strings.HasPrefix(fresh, "whsec_") || fresh == old {
		t.Fatalf("rotate: %q %v", fresh, err)
	}
	if _, err := RotateSecret(ctx, r.q, r.box, "prj_other", id); !errors.Is(err, ErrNoEndpoint) {
		t.Fatal("another project rotated this endpoint")
	}
	signedWith := func() string {
		got := rc.requests()
		h := got[len(got)-1].header
		ts, _ := strconv.ParseInt(h.Get("X-Webhook-Timestamp"), 10, 64)
		for _, s := range []string{old, fresh} {
			if Sign([]byte(s), ts, got[len(got)-1].body) == h.Get("X-Webhook-Signature") {
				return s
			}
		}
		return "neither"
	}
	r.event("app_dashboard", "ast_1")
	r.run()
	if signedWith() != old {
		t.Fatal("a pending secret signed a delivery")
	}
	if err := ActivateSecret(ctx, r.q, r.box, "prj_default", id); err != nil {
		t.Fatal(err)
	}
	r.event("app_dashboard", "ast_2")
	r.run()
	if signedWith() != fresh {
		t.Fatal("the activated secret doesn't sign")
	}
}

func TestOnlyAGivenUpDeliveryCanBeRedelivered(t *testing.T) {
	r := newRig(t)
	rc := newReceiver(t, 200)
	r.endpoint("app_dashboard", rc.URL)
	r.event("app_dashboard", "ast_1")
	r.run()
	var id string
	if err := r.pool.QueryRow(context.Background(), `SELECT id FROM webhook_deliveries`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	params := db.RedeliverWebhookParams{ID: id, ProjectID: "prj_default", Now: ts(r.now), GiveUpAt: ts(r.now.Add(RedeliverWindow))}
	if _, err := r.q.RedeliverWebhook(context.Background(), params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("redelivering a delivered event: %v, want refused", err)
	}
	r.exec(`UPDATE webhook_deliveries SET status = 'failed' WHERE id = $1`, id)
	if _, err := r.q.RedeliverWebhook(context.Background(), db.RedeliverWebhookParams{ID: id, ProjectID: "prj_other", Now: params.Now, GiveUpAt: params.GiveUpAt}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("another project redelivered it")
	}
	if _, err := r.q.RedeliverWebhook(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	r.run()
	if d := r.deliveries()[0]; d.Status != "delivered" || d.Attempts != 1 || !d.GiveUp.Equal(r.now.Add(RedeliverWindow)) || len(rc.requests()) != 2 {
		t.Fatalf("after redelivery: %+v with %d requests", d, len(rc.requests()))
	}
}
