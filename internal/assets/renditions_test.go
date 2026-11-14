package assets

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

func TestRenditionsAreLadderNamesInLadderOrder(t *testing.T) {
	got, err := Renditions(nil)
	if err != nil || got != nil {
		t.Fatalf("absent = %v, %v; want nil: every rung up to the source", got, err)
	}
	got, err = Renditions([]string{"480p", "2160p", "1080p"})
	if err != nil || !slices.Equal(got, []string{"2160p", "1080p", "480p"}) {
		t.Fatalf("Renditions = %v, %v; want ladder order", got, err)
	}
	for _, bad := range [][]string{{}, {"4320p"}, {"1080"}, {"720p", "720p"}, {"audio"}} {
		_, err := Renditions(bad)
		var p *httpjson.Problem
		if !errors.As(err, &p) || p.Status != http.StatusBadRequest || p.Code != "invalid_request" {
			t.Errorf("Renditions(%q) = %v, want a 400 invalid_request", bad, err)
		}
	}
}
