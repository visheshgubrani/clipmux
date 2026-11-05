package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/secrets"
)

// retryDelays is the wait after each failed try; the last one repeats. With
// the 24 h window that is 11 tries: at 0, 30 s, 2½ min, 12½ min, 42½ min,
// 1 h 42½ min, 3 h 42½ min, 7 h 42½ min, 15 h 42½ min, 23 h 42½ min and 24 h.
var retryDelays = []time.Duration{
	30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
}

// Publisher fans webhook events out to endpoints and delivers them. Run is
// safe to call concurrently and repeatedly (SKIP LOCKED, leases, fenced
// results), so a crash anywhere only delays a delivery.
type Publisher struct {
	pool   *pgxpool.Pool
	box    *secrets.Box
	client *http.Client

	// Timeout bounds one request, from connecting to the response headers.
	Timeout time.Duration
	// Window is how long after fan-out an event is retried.
	Window time.Duration
	// Lease is how long a claimed delivery is held before another pass may
	// send it again; it must outlast Timeout.
	Lease time.Duration
	// Batch is how many deliveries one claim takes; Concurrency how many
	// are in flight at once.
	Batch, Concurrency int
	// Now is the clock for timestamps and schedules.
	Now    func() time.Time
	Logger *slog.Logger
}

// New returns a publisher with the defaults recorded in docs/decisions.md.
func New(pool *pgxpool.Pool, box *secrets.Box) *Publisher {
	return &Publisher{
		pool: pool, box: box,
		// A redirect is a failure, never followed: the owner registered this
		// URL, not wherever it points.
		client:  &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Timeout: 10 * time.Second, Window: 24 * time.Hour, Lease: 60 * time.Second,
		Batch: 32, Concurrency: 8,
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
	}
}

// Run fans out new events, then sends due deliveries until a claim comes up
// short. It returns how many deliveries it attempted.
func (p *Publisher) Run(ctx context.Context) (int, error) {
	if _, err := p.Fanout(ctx, 500); err != nil {
		return 0, err
	}
	sent := 0
	for range 10 {
		n, err := p.Deliver(ctx)
		sent += n
		if err != nil || n < p.Batch {
			return sent, err
		}
	}
	return sent, nil
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// Fanout turns up to limit pending webhook events into one delivery per
// enabled endpoint of the event's app, due now. An event whose app has no
// endpoints is done; apps catch up with GET /v1/assets?updated_since=.
func (p *Publisher) Fanout(ctx context.Context, limit int) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		events, err := q.LockPendingWebhookEvents(ctx, int32(limit))
		if err != nil {
			return err
		}
		now := p.Now()
		for _, e := range events {
			if e.AppID.Valid {
				endpoints, err := q.ListEnabledEndpointsForApp(ctx, e.AppID.String)
				if err != nil {
					return err
				}
				for _, ep := range endpoints {
					if err := q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{
						ID: ids.New(ids.Delivery), ProjectID: e.ProjectID, EventID: e.ID, EndpointID: ep,
						Now: ts(now), GiveUpAt: ts(now.Add(p.Window)),
					}); err != nil {
						return err
					}
				}
			}
			if err := q.MarkOutboxDone(ctx, e.ID); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("fan out webhook events: %w", err)
	}
	return n, nil
}

// Deliver claims one batch of due deliveries and sends them, at most
// Concurrency at a time. It returns once every send has finished.
func (p *Publisher) Deliver(ctx context.Context) (int, error) {
	claimed, err := p.claim(ctx)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, max(p.Concurrency, 1))
	var wg sync.WaitGroup
	for _, d := range claimed {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			p.deliver(ctx, d)
		})
	}
	wg.Wait()
	return len(claimed), nil
}

func (p *Publisher) claim(ctx context.Context) ([]db.ClaimWebhookDeliveriesRow, error) {
	now := p.Now()
	rows, err := db.New(p.pool).ClaimWebhookDeliveries(ctx, db.ClaimWebhookDeliveriesParams{
		Now: ts(now), LeaseUntil: ts(now.Add(p.Lease)), MaxDeliveries: int32(p.Batch),
	})
	if err != nil {
		return nil, fmt.Errorf("claim webhook deliveries: %w", err)
	}
	return rows, nil
}

func (p *Publisher) deliver(ctx context.Context, d db.ClaimWebhookDeliveriesRow) {
	if d.AppDisabled || d.EndpointDisabled {
		code := "endpoint_disabled"
		if d.AppDisabled {
			code = "app_disabled"
		}
		p.finish(ctx, d, "failed", 0, code)
		return
	}
	secret, err := p.box.Open(sealedTable, d.EndpointID, d.SecretEncrypted)
	if err != nil {
		// serve checks its key at boot, so this row's secret is damaged.
		p.finish(ctx, d, "failed", 0, "secret_unreadable")
		return
	}
	status, code := p.send(ctx, d, secret)
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and another pass resends
	}
	p.record(ctx, d, status, code)
}

// send POSTs the stored event bytes, signed over a fresh timestamp. It
// returns the response status (0 if there was none) and, for a failure, a
// short code such as http_500 or timeout.
func (p *Publisher) send(ctx context.Context, d db.ClaimWebhookDeliveriesRow, secret []byte) (int, string) {
	reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.Url, bytes.NewReader(d.Payload))
	if err != nil {
		return 0, "invalid_url"
	}
	now := p.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ClipMux-Webhooks/1.0")
	req.Header.Set("X-Webhook-Event", d.EventType)
	req.Header.Set("X-Webhook-Id", d.EventID)
	req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(now, 10))
	req.Header.Set("X-Webhook-Signature", Sign(secret, now, d.Payload))
	res, err := p.client.Do(req)
	if err != nil {
		var nerr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()) {
			return 0, "timeout"
		}
		return 0, "connection_failed"
	}
	// Drain a little so the connection can be reused; never keep the body.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, "http_" + strconv.Itoa(res.StatusCode)
	}
	return res.StatusCode, ""
}

// record stores one try's result: delivered, a retry on the schedule, or
// failed once the window has passed. It reports whether the result was kept;
// a sender whose lease ran out and was overtaken records nothing.
func (p *Publisher) record(ctx context.Context, d db.ClaimWebhookDeliveriesRow, status int, code string) bool {
	if code == "" {
		return p.finish(ctx, d, "delivered", status, "")
	}
	now := p.Now()
	giveUp := d.GiveUpAt.Time
	if !now.Before(giveUp) {
		return p.finish(ctx, d, "failed", status, code)
	}
	next := now.Add(retryDelays[min(int(d.Attempts), len(retryDelays))-1])
	if next.After(giveUp) {
		next = giveUp
	}
	n, err := db.New(p.pool).RetryWebhookDelivery(ctx, db.RetryWebhookDeliveryParams{
		ID: d.ID, Attempts: d.Attempts, NextAttemptAt: ts(next),
		LastStatus: optStatus(status), LastError: pgtype.Text{String: code, Valid: true},
	})
	if err != nil {
		p.Logger.Error("record webhook retry", "delivery", d.ID, "err", err)
		return false
	}
	p.Logger.Warn("webhook delivery failed", "delivery", d.ID, "endpoint", d.EndpointID, "attempt", d.Attempts, "code", code)
	return n == 1
}

func (p *Publisher) finish(ctx context.Context, d db.ClaimWebhookDeliveriesRow, status string, httpStatus int, code string) bool {
	n, err := db.New(p.pool).FinishWebhookDelivery(ctx, db.FinishWebhookDeliveryParams{
		ID: d.ID, Attempts: d.Attempts, Status: status, Now: ts(p.Now()),
		LastStatus: optStatus(httpStatus), LastError: pgtype.Text{String: code, Valid: code != ""},
	})
	if err != nil {
		p.Logger.Error("record webhook delivery", "delivery", d.ID, "err", err)
		return false
	}
	if status == "failed" {
		p.Logger.Warn("webhook delivery gave up", "delivery", d.ID, "endpoint", d.EndpointID, "attempts", d.Attempts, "code", code)
	}
	return n == 1
}

func optStatus(s int) pgtype.Int4 { return pgtype.Int4{Int32: int32(s), Valid: s != 0} }
