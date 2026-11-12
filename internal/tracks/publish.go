package tracks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/routes"
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
		store, err := p.Stores.MediaForIn(ctx, qs, asset.ID)
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
		if err := qs.BumpAssetForTrack(ctx, asset.ID); err != nil {
			return err
		}
		return publish(ctx, qs, asset, track.ID)
	}
}

// publish writes a stored track's route_sync rows; with no playback ID to
// publish, it is ready at once (as outputs.commit does for an output).
func publish(ctx context.Context, qs *db.Queries, asset db.Asset, trackID string) error {
	n, err := routes.QueueTrack(ctx, qs, asset.ProjectID, asset.AppID, asset.ID, trackID)
	if err != nil || n > 0 {
		return err
	}
	return FinishPublication(ctx, qs, trackID)
}

// FinishPublication makes a stored track ready once every route_sync row
// of it is confirmed, and only once its asset is ready, so video is always
// ready before its captions. It retires the asset's previous ready track of
// the kind (still servable for the retire window) and republishes the
// routes so the master stops listing it. SyncRoutes calls it, as does the
// asset becoming ready (FinishPublishing). Lock order: asset, then track.
func FinishPublication(ctx context.Context, qs *db.Queries, trackID string) error {
	track, err := qs.GetTrack(ctx, trackID)
	if err != nil {
		return err
	}
	asset, err := qs.LockAsset(ctx, track.AssetID)
	if err != nil {
		return err
	}
	if track, err = qs.LockTrack(ctx, trackID); err != nil {
		return err
	}
	if track.Status != Publishing || asset.Status != "ready" {
		return nil
	}
	pending, err := qs.CountPendingRoutesForTrack(ctx, db.CountPendingRoutesForTrackParams{AssetID: text(asset.ID), TrackID: trackID})
	if err != nil || pending > 0 {
		return err
	}
	// The old one first: at most one ready track per kind (tracks_one_ready).
	retired, err := qs.RetireReadyTracksOfKind(ctx, db.RetireReadyTracksOfKindParams{AssetID: asset.ID, Kind: track.Kind, Keep: trackID})
	if err != nil {
		return err
	}
	if _, err := qs.MarkTrackReady(ctx, trackID); err != nil {
		return err
	}
	if len(retired) > 0 {
		if err := routes.QueueAssetRefresh(ctx, qs, asset.ProjectID, asset.AppID, asset.ID); err != nil {
			return err
		}
	}
	if err := qs.BumpAssetForTrack(ctx, asset.ID); err != nil {
		return err
	}
	if err := outbox.TrackEvent(ctx, qs, asset.ID, outbox.TrackReady, outbox.Track{ID: trackID, Kind: track.Kind}, nil); err != nil {
		return err
	}
	// Ready subtitles start the chapters waiting for them.
	return QueueNext(ctx, qs, asset.ID)
}

// FinishPublishing finishes every track of an asset that was waiting for
// the asset to become ready. The asset is locked by the caller.
func FinishPublishing(ctx context.Context, qs *db.Queries, assetID string) error {
	ids, err := qs.ListPublishingTracks(ctx, assetID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := FinishPublication(ctx, qs, id); err != nil {
			return err
		}
	}
	return nil
}

func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// ErrStale means a chapters job's track or asset changed while the model
// answered (deleted, superseded): it commits nothing.
var ErrStale = errors.New("tracks: the track is no longer being made")

// ChaptersResult is what a chapters job made.
type ChaptersResult struct {
	// VTT is the chapters as WebVTT, one cue per chapter.
	VTT []byte
	// Chapters is the [{start_ms, end_ms, title}] the API shows.
	Chapters   json.RawMessage
	DurationMS int64
	// Language and OutputID are the transcript's.
	Language, OutputID string
}

// CommitChapters stores a chapters job's result, with the job locked by
// the caller: the VTT sealed and written under the lock (as a
// transcription's is, D054), then the track waits for its routes. A track
// that is no longer preparing, or a deleted asset, gives ErrStale.
func (p *Publisher) CommitChapters(ctx context.Context, qs *db.Queries, job db.Job, r ChaptersResult) error {
	if job.Kind != JobChapters || !job.TrackID.Valid {
		return fmt.Errorf("job %s doesn't make chapters", job.ID)
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
		return ErrStale
	}
	secret := p.Keys.Current()
	key := StorageKey(asset.ID, track.ID)
	sealed, err := contentkey.SealTrack(secret, asset.ID, track.ID, key, r.VTT)
	if err != nil {
		return err
	}
	store, err := p.Stores.MediaForIn(ctx, qs, asset.ID)
	if err != nil {
		return err
	}
	if err := store.Put(ctx, key, bytes.NewReader(sealed)); err != nil {
		return fmt.Errorf("store %s: %w", key, err)
	}
	if _, err := qs.PublishChaptersTrack(ctx, db.PublishChaptersTrackParams{
		ID: track.ID, Language: text(r.Language), Name: text("Chapters"),
		DurationMs: pgtype.Int8{Int64: r.DurationMS, Valid: true}, KeyVersion: pgtype.Int4{Int32: secret.Version, Valid: true},
		OutputID: text(r.OutputID), Chapters: r.Chapters,
	}); err != nil {
		return err
	}
	if err := qs.BumpAssetForTrack(ctx, asset.ID); err != nil {
		return err
	}
	return publish(ctx, qs, asset, track.ID)
}
