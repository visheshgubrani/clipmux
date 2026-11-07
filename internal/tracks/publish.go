package tracks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/stores"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// StorageKey is where a track's sealed VTT lives: under the asset's media
// prefix, so deleting the asset deletes it, and outside every out_ prefix,
// so the orphan sweep leaves it alone (D070).
func StorageKey(assetID, trackID string) string {
	return "media/" + assetID + "/tracks/" + trackID + ".vtt"
}

// languageNames are the names players show for the LMS's languages; any
// other language shows its code.
var languageNames = map[string]string{
	"en": "English", "hi": "Hindi", "bn": "Bengali", "ta": "Tamil", "te": "Telugu", "mr": "Marathi",
	"gu": "Gujarati", "kn": "Kannada", "ml": "Malayalam", "pa": "Punjabi", "ur": "Urdu",
}

// LanguageName is how a track is named in a player's menu.
func LanguageName(code string) string {
	if n, ok := languageNames[code]; ok {
		return n
	}
	return code
}

// Publisher stores what track jobs produce: the transcript, and its WebVTT
// sealed into the asset's store.
type Publisher struct {
	Stores *stores.Set
	Keys   contentkey.Ring
}

// CommitTranscript is a transcription attempt's commit, for
// queue.CommitAttempt, so it runs under the job's liveness lock: a stale
// attempt never gets here and writes nothing. The transcript is checked
// against the output it was made from (its duration, which serve sets, as
// it sets the output), rendered to WebVTT, sealed and written to storage,
// then stored, and the track waits for its routes (publishing). A bad
// transcript is a *transcripts.InvalidError.
func (p *Publisher) CommitTranscript(ctx context.Context, raw json.RawMessage) func(qs *db.Queries, attempt db.Attempt, job db.Job) error {
	return func(qs *db.Queries, attempt db.Attempt, job db.Job) error {
		if job.Kind != JobTranscribe || !job.TrackID.Valid || !attempt.InputOutputID.Valid {
			return &transcripts.InvalidError{Reason: "the attempt doesn't transcribe"}
		}
		var t transcripts.Transcript
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&t); err != nil {
			return &transcripts.InvalidError{Reason: err.Error()}
		}
		asset, err := qs.LockAsset(ctx, job.AssetID)
		if err != nil {
			return err
		}
		track, err := qs.LockTrack(ctx, job.TrackID.String)
		if err != nil {
			return err
		}
		if asset.Status == "deleted" || track.Status != Preparing {
			return &transcripts.InvalidError{Reason: "the track is " + track.Status}
		}
		out, err := qs.GetOutput(ctx, attempt.InputOutputID.String)
		if err != nil {
			return err
		}
		var m struct {
			Duration float64 `json:"duration_seconds"`
		}
		if err := json.Unmarshal(out.Manifest, &m); err != nil {
			return fmt.Errorf("output %s manifest: %w", out.ID, err)
		}
		if t.DurationMS, err = transcripts.Milliseconds(m.Duration); err != nil {
			return err
		}
		t.OutputID = out.ID
		if err := t.Validate(); err != nil {
			return err
		}
		secret := p.Keys.Current()
		key := StorageKey(asset.ID, track.ID)
		sealed, err := contentkey.SealTrack(secret, asset.ID, track.ID, key, t.VTT())
		if err != nil {
			return err
		}
		store, err := p.Stores.ForAsset(ctx, asset.ID)
		if err != nil {
			return err
		}
		if err := store.Put(ctx, key, bytes.NewReader(sealed)); err != nil {
			return fmt.Errorf("store %s: %w", key, err)
		}
		body, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if err := qs.InsertTranscript(ctx, db.InsertTranscriptParams{
			TrackID: track.ID, ProjectID: track.ProjectID, AssetID: asset.ID, Body: body,
		}); err != nil {
			return err
		}
		if _, err := qs.PublishTrack(ctx, db.PublishTrackParams{
			ID: track.ID, Language: text(t.Language), Name: text(LanguageName(t.Language)),
			DurationMs: pgtype.Int8{Int64: t.DurationMS, Valid: true}, KeyVersion: pgtype.Int4{Int32: secret.Version, Valid: true},
			OutputID: text(out.ID),
		}); err != nil {
			return err
		}
		return qs.BumpAssetForTrack(ctx, asset.ID)
	}
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
