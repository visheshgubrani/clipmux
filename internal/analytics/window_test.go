package analytics

import (
	"testing"
	"time"
)

// Worked examples at 09:30 UTC on 10 November 2026. The last bucket is the
// one now is in, so it is partial; previous is the same number of whole
// buckets just before.
func TestWindows(t *testing.T) {
	now := at("2026-11-10T09:30:00Z")
	for _, c := range []struct {
		rng                              string
		start, stop, prevStart           string
		buckets                          int
		curFrom, curTo, prevFrom, prevTo string // viewer days, inclusive
	}{
		{"24h", "2026-11-09T10:00:00Z", "2026-11-10T10:00:00Z", "2026-11-08T10:00:00Z", 24,
			"2026-11-09", "2026-11-10", "2026-11-08", "2026-11-09"},
		{"7d", "2026-11-04T00:00:00Z", "2026-11-11T00:00:00Z", "2026-10-28T00:00:00Z", 7,
			"2026-11-04", "2026-11-10", "2026-10-28", "2026-11-03"},
		{"30d", "2026-10-12T00:00:00Z", "2026-11-11T00:00:00Z", "2026-09-12T00:00:00Z", 30,
			"2026-10-12", "2026-11-10", "2026-09-12", "2026-10-11"},
		{"90d", "2026-08-13T00:00:00Z", "2026-11-11T00:00:00Z", "2026-05-15T00:00:00Z", 90,
			"2026-08-13", "2026-11-10", "2026-05-15", "2026-08-12"},
	} {
		w, ok := windowOf(c.rng, now)
		if !ok {
			t.Fatalf("%s: not a range", c.rng)
		}
		day := func(d time.Time) string { return d.Format(time.DateOnly) }
		if !w.start.Equal(at(c.start)) || !w.stop.Equal(at(c.stop)) || !w.prevStart.Equal(at(c.prevStart)) || w.buckets != c.buckets {
			t.Errorf("%s: start %v stop %v previous %v, %d buckets", c.rng, w.start, w.stop, w.prevStart, w.buckets)
		}
		if day(w.curFrom()) != c.curFrom || day(w.curTo()) != c.curTo || day(w.prevFrom()) != c.prevFrom || day(w.prevTo()) != c.prevTo {
			t.Errorf("%s: viewer days %s..%s, previous %s..%s", c.rng,
				day(w.curFrom()), day(w.curTo()), day(w.prevFrom()), day(w.prevTo()))
		}
	}
	for _, bad := range []string{"", "1d", "24H", "365d"} {
		if _, ok := windowOf(bad, now); ok {
			t.Errorf("%q is a range", bad)
		}
	}
}
