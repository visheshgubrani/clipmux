package dispatch

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
)

// Disconnecting a provider takes it out of routing (D116): a primary gives
// way to the built-in worker, a fallback to none, and the queue-age rule
// leaves with its fallback.
func TestRoutingWithoutAProvider(t *testing.T) {
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	age := pgtype.Int4{Int32: 10, Valid: true}
	for _, c := range []struct {
		name                      string
		primary, fallback         string
		queueAge                  pgtype.Int4
		wantPrimary, wantFallback string
		wantQueueAge, wantChanged bool
	}{
		{"primary with the built-in fallback", "modal", "builtin", age, "builtin", "", false, true},
		{"primary with another provider behind it", "modal", "runpod", age, "builtin", "runpod", true, true},
		{"the fallback", "builtin", "modal", age, "builtin", "", false, true},
		{"another provider's fallback", "runpod", "modal", age, "runpod", "", false, true},
		{"not named", "workers", "", pgtype.Int4{}, "workers", "", false, false},
		{"another provider", "runpod", "builtin", age, "runpod", "builtin", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			next, changed := routingWithout(db.GetRoutingRow{PrimaryExecutor: c.primary, FallbackExecutor: text(c.fallback), QueueAgeFallbackMinutes: c.queueAge}, "modal")
			if changed != c.wantChanged || next.PrimaryExecutor != c.wantPrimary || next.FallbackExecutor.String != c.wantFallback ||
				next.FallbackExecutor.Valid != (c.wantFallback != "") || next.QueueAgeFallbackMinutes.Valid != c.wantQueueAge {
				t.Fatalf("routingWithout(%s, %s) = %+v, %v; want %s, %q, queue age %v, changed %v",
					c.primary, c.fallback, next, changed, c.wantPrimary, c.wantFallback, c.wantQueueAge, c.wantChanged)
			}
		})
	}
}

func TestWakeEnvTurnsSpeechOffForAnythingButTranscription(t *testing.T) {
	for _, tc := range []struct {
		kind, speech string
	}{
		{"transcode", "off"},
		{"reencode", "off"},
		{"transcribe", ""},
	} {
		env := WakeEnv("https://video.example.com", "cmxd_secret", tc.kind)
		if env["CLIPMUX_SERVER"] != "https://video.example.com" || env["CLIPMUX_WORKER_KEY"] != "cmxd_secret" {
			t.Errorf("%s: env = %v", tc.kind, env)
		}
		if got, set := env["CLIPMUX_SPEECH"]; got != tc.speech || set != (tc.speech != "") {
			t.Errorf("%s: CLIPMUX_SPEECH = %q (set %v), want %q", tc.kind, got, set, tc.speech)
		}
	}
}
