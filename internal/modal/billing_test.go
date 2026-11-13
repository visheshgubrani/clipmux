package modal

import (
	"testing"
	"time"
)

// Modal reports dollars as decimal strings per day and per hour; the bill is their sum
// in cents, rounded up, and reaches the end of the last hour reported.
func TestSumBill(t *testing.T) {
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cents, through, err := sumBill([]billItem{
		{Start: since, End: since.Add(24 * time.Hour), Cost: "0.09533340"},
		{Start: since.Add(24 * time.Hour), End: since.Add(25 * time.Hour), Cost: "0.15866660"},
		{Start: since.Add(26 * time.Hour), End: since.Add(27 * time.Hour), Cost: "1.5"},
	}, since)
	// 0.0953334 + 0.1586666 + 1.5 = $1.754 → 175.4 ¢ → 176 ¢.
	if err != nil || cents != 176 || !through.Equal(since.Add(27*time.Hour)) {
		t.Fatalf("sumBill = %d, %v, %v; want 176 through %v", cents, through, err, since.Add(27*time.Hour))
	}
	if cents, through, err := sumBill(nil, since); err != nil || cents != 0 || !through.Equal(since) {
		t.Fatalf("an empty report = %d, %v, %v; want 0 through since", cents, through, err)
	}
	if cents, _, _ := sumBill([]billItem{{Start: since, End: since.Add(time.Hour), Cost: "2.00"}}, since); cents != 200 {
		t.Fatalf("whole cents = %d, want 200", cents)
	}
	if _, _, err := sumBill([]billItem{{Start: since, End: since.Add(time.Hour), Cost: "NaN"}}, since); err == nil {
		t.Fatal("a cost that isn't a number must fail")
	}
}
