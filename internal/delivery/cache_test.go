package delivery

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
)

func TestALoadThatRacesAnInvalidationIsntKept(t *testing.T) {
	h := &Handler{Now: time.Now}
	h.cache = newCache(h.Now)
	loads := 0
	get := func(during func()) {
		_, _ = lookup(h.cache, func() map[string]cached[db.Output] { return h.cache.outputs }, "out_1", func() (db.Output, error) {
			loads++
			if during != nil {
				during()
			}
			return db.Output{ID: "out_1", Status: "writing"}, nil
		})
	}
	get(func() { h.Invalidate("pid_1") }) // the output commits while it loads
	get(nil)
	if loads != 2 {
		t.Fatalf("loads = %d: a value read before an invalidation was cached", loads)
	}
	get(nil)
	if loads != 2 {
		t.Fatalf("loads = %d: a clean load wasn't cached", loads)
	}
}

func TestUnknownIDsAreRememberedBriefly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	h := &Handler{Now: func() time.Time { return now }}
	h.cache = newCache(h.Now)
	loads := 0
	miss := func() error {
		_, err := lookup(h.cache, func() map[string]cached[db.GetRouteRow] { return h.cache.routes }, "pid_new", func() (db.GetRouteRow, error) {
			loads++
			return db.GetRouteRow{}, pgx.ErrNoRows
		})
		return err
	}
	_ = miss()
	if err := miss(); !errors.Is(err, pgx.ErrNoRows) || loads != 1 {
		t.Fatalf("a cached miss: %v after %d loads", err, loads)
	}
	now = now.Add(UnknownTTL + time.Millisecond)
	_ = miss()
	if loads != 2 {
		t.Fatal("an unknown ID was remembered past UnknownTTL")
	}
}
