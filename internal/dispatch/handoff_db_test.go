package dispatch_test

// A machine woken for an encode goes on to the asset's transcription under
// a new dispatch instead of stopping, so a captioned video wakes one
// machine, not two (D128).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/queue"
)

// handOffRig has RunPod as the primary, so a transcription queued for an
// asset (speech mode local) is routed there as well.
func handOffRig(t *testing.T) *rig {
	r := newRig(t)
	r.exec(`UPDATE projects SET primary_executor = 'runpod', fallback_executor = NULL WHERE id = 'prj_default'`)
	return r
}

// encoding wakes a machine for asset's encode and claims it with that
// machine's key; followOn is whether the worker says it can take a
// transcription after it.
func (r *rig) encoding(assetID string, followOn bool) (db.Job, *queue.Claim) {
	r.t.Helper()
	job, key := r.woken(assetID)
	wk, ok := r.svc.AuthenticateWorker(context.Background(), key)
	if !ok {
		r.t.Fatal("the woken machine's key didn't authenticate")
	}
	if followOn {
		wk.Info = []byte(`{"follow_on":["transcribe"]}`)
	}
	c := r.claim(wk)
	if c == nil {
		r.t.Fatal("the woken machine claimed nothing")
	}
	return job, c
}

// transcription queues the transcription of asset's subtitles, as an
// output's commit does.
func (r *rig) transcription(assetID string) db.Job {
	r.t.Helper()
	ctx := context.Background()
	trk := ids.New(ids.Track)
	r.exec(`INSERT INTO tracks (id, project_id, asset_id, kind) VALUES ($1, 'prj_default', $2, 'subtitles')`, trk, assetID)
	j, err := db.New(r.pool).CreateJob(ctx, db.CreateJobParams{
		ID: ids.New(ids.Job), ProjectID: "prj_default", AssetID: assetID, Kind: "transcribe",
		TrackID: pgtype.Text{String: trk, Valid: true},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return j
}

// handOff runs HandOff in a transaction of its own, standing in for the
// commit's.
func (r *rig) handOff(c *queue.Claim, next db.Job) string {
	r.t.Helper()
	var key string
	err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		var err error
		key, err = r.svc.HandOff(context.Background(), db.New(tx), c.Attempt, next)
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return key
}

func (r *rig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func TestACommittedEncodeHandsItsMachineToTheTranscription(t *testing.T) {
	ctx := context.Background()
	r := handOffRig(t)
	enc, c := r.encoding("ast_1", true)
	tr := r.transcription("ast_1")

	key := r.handOff(c, tr)
	if !strings.HasPrefix(key, "cmxd_") {
		t.Fatalf("HandOff = %q, want a new one-time key", key)
	}
	from := r.ended(enc.ID)
	if from.Status != "ended" || from.EndReason.String != "handed_off" || !from.StoppedAt.Valid {
		t.Fatalf("the encode's dispatch = %s %s stopped %v, want ended handed_off and stopped",
			from.Status, from.EndReason.String, from.StoppedAt.Valid)
	}
	ds := r.dispatches(tr.ID)
	if len(ds) != 1 {
		t.Fatalf("%d dispatches for the transcription, want 1", len(ds))
	}
	next := ds[0]
	if next.Status != "waking" || next.InstanceID != from.InstanceID || !next.WokenAt.Valid || next.Kind != "runpod" {
		t.Fatalf("the transcription's dispatch = %+v, want waking on machine %s", next, from.InstanceID.String)
	}
	// The machine's time is counted once: the first dispatch stops when the
	// next one wakes, and the next must claim within the hand-off window.
	if !from.StoppedAt.Time.Equal(next.WokenAt.Time) {
		t.Fatalf("stopped %v, woken %v: the machine's time would be counted twice or lost", from.StoppedAt.Time, next.WokenAt.Time)
	}
	if left := time.Until(next.ClaimDeadline.Time); left <= 0 || left > 2*time.Minute {
		t.Fatalf("claim deadline in %v, want within the 2-minute hand-off window", left)
	}
	if n := r.count(`SELECT count(*) FROM dispatches WHERE status <> 'ended' OR (instance_id IS NOT NULL AND stopped_at IS NULL)`); n != 1 {
		t.Fatalf("%d machines counted against the provider's limit, want 1", n)
	}

	// No wake and no stop: the same machine goes on.
	if n := r.count(`SELECT count(*) FROM outbox WHERE kind IN ('wake', 'stop') AND done_at IS NULL`); n != 0 {
		t.Fatalf("%d wakes or stops queued by a hand-off", n)
	}
	r.route()
	if ds := r.dispatches(tr.ID); len(ds) != 1 {
		t.Fatalf("the router made %d dispatches for a handed-off job", len(ds))
	}

	// The new key claims the transcription, and only that.
	wk, ok := r.svc.AuthenticateWorker(ctx, key)
	if !ok || wk.JobID != tr.ID || wk.DispatchID != next.ID {
		t.Fatalf("AuthenticateWorker = %+v %v", wk, ok)
	}
	c2 := r.claim(wk)
	if c2 == nil || c2.Job.ID != tr.ID || c2.Attempt.DispatchID.String != next.ID {
		t.Fatalf("the handed-off key claimed %+v", c2)
	}
	r.publish()
	if len(r.prov.wakes) != 1 || len(r.prov.stops) != 0 {
		t.Fatalf("wakes %d stops %v: a captioned video should wake one machine", len(r.prov.wakes), r.prov.stops)
	}

	// Once the transcription ends, the machine stops as usual.
	if _, err := r.q.Fail(ctx, c2.Token, queue.Failure{Code: "transcribe_failed"}); err != nil {
		t.Fatal(err)
	}
	if n := r.reap(); n != 1 {
		t.Fatalf("reaped %d, want the transcription's dispatch", n)
	}
	r.publish()
	if len(r.prov.stops) != 1 || r.prov.stops[0] != from.InstanceID.String {
		t.Fatalf("stops = %v, want the one machine %s", r.prov.stops, from.InstanceID.String)
	}
}

// Without everything a hand-off needs, the encode's machine stops as before
// and the transcription is routed like any job.
func TestAHandOffNeedsAWillingMachineOnTheSameProviderWithRoom(t *testing.T) {
	cases := []struct {
		name     string
		followOn bool
		setup    func(r *rig, tr db.Job)
	}{
		{"the worker can't follow on", false, func(*rig, db.Job) {}},
		{"the transcription goes elsewhere", true, func(r *rig, tr db.Job) {
			r.exec(`UPDATE jobs SET routed_to = 'speech_api' WHERE id = $1`, tr.ID)
		}},
		{"the monthly limit is reached", true, func(r *rig, _ db.Job) {
			r.exec(`UPDATE executor_configs SET monthly_limit_cents = 0 WHERE id = 'exe_runpod'`)
		}},
		{"the provider was disconnected", true, func(r *rig, _ db.Job) {
			// Its row stays, as spend history, without credentials.
			r.exec(`UPDATE executor_configs SET secrets_encrypted = NULL WHERE id = 'exe_runpod'`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := handOffRig(t)
			enc, c := r.encoding("ast_1", tc.followOn)
			tr := r.transcription("ast_1")
			tc.setup(r, tr)
			tr, err := db.New(r.pool).GetJob(context.Background(), tr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if key := r.handOff(c, tr); key != "" {
				t.Fatalf("HandOff = %q, want none", key)
			}
			if d := r.ended(enc.ID); d.Status != "claimed" {
				t.Fatalf("the encode's dispatch = %s %s, want it left claimed", d.Status, d.EndReason.String)
			}
			if ds := r.dispatches(tr.ID); len(ds) != 0 {
				t.Fatalf("%d dispatches for the transcription", len(ds))
			}
		})
	}
}

// A machine that never takes the job it was handed (its answer was lost,
// or it crashed) is stopped at the end of the window, and the job is woken
// for like any other.
func TestAHandOffNobodyClaimsIsRoutedAgain(t *testing.T) {
	r := handOffRig(t)
	enc, c := r.encoding("ast_1", true)
	tr := r.transcription("ast_1")
	if r.handOff(c, tr) == "" {
		t.Fatal("no hand-off")
	}
	machine := r.ended(enc.ID).InstanceID.String
	r.exec(`UPDATE dispatches SET claim_deadline = now() - interval '1 second' WHERE job_id = $1`, tr.ID)
	if n := r.reap(); n != 1 {
		t.Fatalf("reaped %d, want the unclaimed hand-off", n)
	}
	if d := r.ended(tr.ID); d.EndReason.String != "wake_timeout" {
		t.Fatalf("the hand-off ended %s, want wake_timeout", d.EndReason.String)
	}
	r.publish()
	if len(r.prov.stops) != 1 || r.prov.stops[0] != machine {
		t.Fatalf("stops = %v, want %s", r.prov.stops, machine)
	}
	r.route()
	r.publish()
	if ds := r.dispatches(tr.ID); len(ds) != 2 || ds[1].Status != "waking" || len(r.prov.wakes) != 2 {
		t.Fatalf("%d dispatches and %d wakes, want the transcription woken for afresh", len(ds), len(r.prov.wakes))
	}
}
