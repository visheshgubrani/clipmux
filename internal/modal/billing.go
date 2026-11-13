package modal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	sdk "github.com/modal-labs/modal-client/go"
	pb "github.com/modal-labs/modal-client/go/proto/modal_proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Modal's bill for this install: its billing report for the clipmux-{tag}
// app, which is what Modal charges for every sandbox ClipMux starts,
// network egress included, before credits. ClipMux's estimate covers only
// the time since the last interval reported (00033).

// biller is the part of api that reads the bill; tests' fakes may lack it.
type biller interface {
	spend(ctx context.Context, app string, since time.Time) (cents int64, through time.Time, err error)
}

// MonthSpend is dispatch.Biller: what Modal billed this install since.
func (e *Executor) MonthSpend(ctx context.Context, since time.Time) (int64, time.Time, error) {
	b, ok := e.api.(biller)
	if !ok {
		return 0, time.Time{}, errors.New("modal: this client can't read the bill")
	}
	return b.spend(ctx, e.app(), since)
}

// billItem is one row of the report: an interval's cost, a decimal string
// in dollars.
type billItem struct {
	Start, End time.Time
	Cost       string
}

// sumBill adds the rows up, in cents rounded up, and says how far they
// reach: the end of the last interval reported, or since when there is
// none (nothing billed yet; the estimate covers the month).
func sumBill(items []billItem, since time.Time) (int64, time.Time, error) {
	total, through := new(big.Rat), since
	for _, it := range items {
		c, ok := new(big.Rat).SetString(it.Cost)
		if !ok || c.Sign() < 0 {
			return 0, time.Time{}, fmt.Errorf("modal bill: cost %q isn't a dollar amount", it.Cost)
		}
		total.Add(total, c)
		if it.End.After(through) {
			through = it.End
		}
	}
	cents := total.Mul(total, big.NewRat(100, 1))
	q, r := new(big.Int).QuoRem(cents.Num(), cents.Denom(), new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64(), through, nil
}

func (a *sdkAPI) spend(ctx context.Context, appName string, since time.Time) (int64, time.Time, error) {
	app, err := a.client.Apps.FromName(ctx, appName, &sdk.AppFromNameParams{Environment: a.env})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("modal app %s: %w", appName, err)
	}
	// Modal reports complete intervals only, and hourly reports span at most
	// seven days: whole days up to today, then today's whole hours.
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var items []billItem
	err = callModal(ctx, []string{"x-modal-token-id", a.tokenID, "x-modal-token-secret", a.tokenSecret},
		func(ctx context.Context, cl pb.ModalClientClient) error {
			report := func(from, to time.Time, resolution string, step time.Duration) error {
				stream, err := cl.WorkspaceBillingReport(ctx, pb.WorkspaceBillingReportRequest_builder{
					StartTimestamp: timestamppb.New(from), EndTimestamp: timestamppb.New(to),
					Resolution: resolution, AppIds: []string{app.AppID},
				}.Build())
				if err != nil {
					return err
				}
				for {
					it, err := stream.Recv()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					start := it.GetInterval().AsTime()
					items = append(items, billItem{Start: start, End: start.Add(step), Cost: it.GetCost()})
				}
			}
			if today.After(since) {
				if err := report(since, today, "d", 24*time.Hour); err != nil {
					return err
				}
			}
			return report(later(since, today), now, "h", time.Hour)
		})
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("modal billing report: %w", redact(err, a.tokenSecret))
	}
	return sumBill(items, since)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
