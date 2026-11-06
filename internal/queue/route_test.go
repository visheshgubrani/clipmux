package queue

import (
	"testing"
	"time"
)

// The routing table (docs/architecture.md, "Routing and fallback"; D053),
// one case per rule, with the expected route written out.
func TestDecideRouteFollowsTheRoutingTable(t *testing.T) {
	const pc, srv = "agents", "builtin"
	cases := []struct {
		name       string
		facts      routeFacts
		to, reason string
	}{
		{"the primary is online", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true, FallbackUp: true, FallbackEligible: true},
			"agents", "primary"},
		{"no heartbeat from the primary for 60 s: the fallback", routeFacts{Primary: pc, Fallback: srv, FallbackUp: true, FallbackEligible: true},
			"builtin", "primary_offline"},
		{"queued past the queue-age setting while the primary is busy", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true, FallbackUp: true, FallbackEligible: true,
			QueueAge: 10 * time.Minute, Waited: 11 * time.Minute}, "builtin", "queue_age"},
		{"not yet past the queue-age setting", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true, FallbackUp: true, FallbackEligible: true,
			QueueAge: 10 * time.Minute, Waited: 9 * time.Minute}, "agents", "primary"},
		{"queue-age fallback off (the default) waits however long", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true, FallbackUp: true, FallbackEligible: true,
			Waited: 3 * time.Hour}, "agents", "primary"},
		{"queue age never sends a failing file to the fallback", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true, FallbackUp: true,
			QueueAge: time.Minute, Waited: time.Hour}, "agents", "primary"},
		{"queue age with the fallback offline stays", routeFacts{Primary: pc, Fallback: srv, PrimaryUp: true,
			FallbackEligible: true, QueueAge: time.Minute, Waited: time.Hour}, "agents", "primary"},
		{"the primary offline and no fallback set: wait", routeFacts{Primary: pc, FallbackEligible: true},
			"agents", "no_fallback"},
		{"a failed file waits for the primary", routeFacts{Primary: pc, Fallback: srv, FallbackUp: true},
			"agents", "fallback_refused"},
		{"both offline: wait for the primary", routeFacts{Primary: pc, Fallback: srv, FallbackEligible: true},
			"agents", "fallback_offline"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			to, reason := decideRoute(c.facts)
			if to != c.to || reason != c.reason {
				t.Fatalf("decideRoute = (%s, %s), want (%s, %s)", to, reason, c.to, c.reason)
			}
		})
	}
}

func TestEachWorkerClaimsOnlyItsClasses(t *testing.T) {
	cases := map[string]string{"builtin": "workers,builtin", "agent": "workers,agents", "modal": "modal", "runpod": "runpod"}
	for executor, want := range cases {
		got := ""
		for i, c := range classes(executor) {
			if i > 0 {
				got += ","
			}
			got += c
		}
		if got != want {
			t.Errorf("classes(%s) = %s, want %s", executor, got, want)
		}
	}
}
