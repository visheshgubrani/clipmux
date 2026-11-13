package dispatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
)

// billedProvider is a provider that reports its own bill (Modal).
type billedProvider struct {
	*fakeProvider
	cents   int64
	through time.Time
}

func (b billedProvider) MonthSpend(context.Context, time.Time) (int64, time.Time, error) {
	return b.cents, b.through, nil
}

// The monthly limit counts the provider's bill, plus ClipMux's estimate only
// for the time since it (00033): an hour the estimate prices at $60 that
// Modal billed $10 leaves room for another hour under $100.
func TestTheMonthlyLimitCountsTheProvidersBill(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	// $60 an hour against a $100 month, with room for 10 machines.
	r.exec(`UPDATE executor_configs SET rate_cents_per_hour = 6000, monthly_limit_cents = 10000, max_concurrent = 10`)
	first := r.job("ast_1")
	r.route()
	r.exec(`UPDATE dispatches SET status = 'ended', end_reason = 'attempt_ended', woken_at = now() - interval '61 minutes',
		ended_at = now() - interval '1 minute', stopped_at = now() - interval '1 minute', instance_id = 'pod-1' WHERE job_id = $1`, first.ID)
	r.exec(`UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1`, first.ID)
	routes := func(id string) string {
		j, _ := db.New(r.pool).GetJob(ctx, id)
		return j.RoutedTo.String + "/" + j.RouteReason.String
	}
	second := r.job("ast_2")
	r.route()
	if got := routes(second.ID); got != "agents/fallback_limit" {
		t.Fatalf("on the estimate ($60 spent + $60 reserved > $100) the second routed %s, want fallback_limit", got)
	}

	// Modal billed $10 up to a minute ago.
	billed := billedProvider{fakeProvider: r.prov, cents: 1000, through: time.Now().Add(-time.Minute)}
	r.svc.Resolve = func(context.Context, db.ExecutorConfig) (dispatch.Executor, error) { return billed, nil }
	if n, err := r.svc.RefreshBills(ctx); err != nil || n != 1 {
		t.Fatalf("RefreshBills = %d, %v", n, err)
	}
	st, err := dispatch.Statuses(ctx, r.pool, "prj_default")
	if err != nil || len(st) != 1 || st[0].BilledCents == nil || *st[0].BilledCents != 1000 || st[0].MonthCents < 1000 || st[0].MonthCents > 1001 {
		t.Fatalf("status = %+v, %v; want $10 billed and counted", st, err)
	}
	r.route()
	if got := routes(second.ID); got != "runpod/primary_offline" {
		t.Fatalf("with the bill ($10 + $60 reserved <= $100) the waiting job routed %s", got)
	}

	// Last month's bill doesn't count this month.
	r.exec(`UPDATE executor_configs SET billed_through = date_trunc('month', now()) - interval '1 hour'`)
	st, _ = dispatch.Statuses(ctx, r.pool, "prj_default")
	if st[0].BilledCents != nil || st[0].MonthCents < 6000 {
		t.Fatalf("a stale bill: status %+v, want the estimate alone", st[0])
	}
}
