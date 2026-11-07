// Package tracks keeps an asset's subtitles and chapters (P5-T13;
// docs/plans/P5-production.md, "Tracks"): their rows and states, and the
// jobs that make them. A track belongs to the asset, not to an output, so
// it survives a re-encode: the timeline is the source's, and every output
// starts at zero.
//
// Tracks are immutable like outputs. Regenerating makes a new track and
// retires the old one only once the new one is ready, so a stale job can't
// overwrite one and viewers never see a gap.
package tracks

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
)

// Track kinds.
const (
	Subtitles = "subtitles"
	Chapters  = "chapters"
)

// Track states. The API shows publishing as preparing and hides retired
// tracks.
const (
	Preparing  = "preparing"
	Publishing = "publishing"
	Ready      = "ready"
	Retired    = "retired"
	Errored    = "errored"
	Deleted    = "deleted"
)

// Job kinds that make tracks.
const (
	// JobTranscribe makes a subtitles track from an output's audio, on a
	// worker (or the speech API, D075).
	JobTranscribe = "transcribe"
	// JobChapters makes a chapters track from a ready subtitles track, in
	// serve.
	JobChapters = "chapters"
)

// The two job families (P5 "Two job families"): at most one active job of
// each per asset. Move and delete are in both, so they exclude everything.
var (
	OutputJobs = []string{"import", "transcode", "reencode", "upload_source", "move", "delete"}
	TrackJobs  = []string{JobTranscribe, JobChapters, "move", "delete"}
	AllJobs    = []string{"import", "transcode", "reencode", "upload_source", "move", "delete", JobTranscribe, JobChapters}
)

// IsTrackJob reports whether a job kind makes a track.
func IsTrackJob(kind string) bool { return kind == JobTranscribe || kind == JobChapters }

// Error codes a track can end with.
const (
	// CodeNoAudio: the output has no audio, so there is nothing to transcribe.
	CodeNoAudio = "no_audio"
	// CodeTranscribeFailed: the transcription's retry budget was spent.
	CodeTranscribeFailed = "transcribe_failed"
	// CodeTranscriptInvalid: what came back broke the transcript's rules.
	CodeTranscriptInvalid = "transcript_invalid"
	// CodeTranscribeUnavailable: no worker that can transcribe claimed the
	// job within SpeechWait.
	CodeTranscribeUnavailable = "transcribe_unavailable"
	// CodeSubtitlesFailed: chapters can't be made because their subtitles
	// failed.
	CodeSubtitlesFailed = "subtitles_failed"
)

// shownCodes are the codes a failed transcription keeps on its track;
// anything else a worker or the queue reported (the budget spent, a lost
// worker, a crash) is transcribe_failed.
var shownCodes = map[string]bool{
	CodeTranscriptInvalid: true, CodeTranscribeUnavailable: true, CodeNoAudio: true,
	"speech_api_rejected": true,
}

// Generate is what an upload, import or attach asks for. Chapters imply
// subtitles, since they are made from the transcript.
type Generate struct {
	Subtitles bool `json:"subtitles,omitempty"`
	Chapters  bool `json:"chapters,omitempty"`
}

// Kinds lists the track kinds to create, subtitles first.
func (g Generate) Kinds() []string {
	switch {
	case g.Chapters:
		return []string{Subtitles, Chapters}
	case g.Subtitles:
		return []string{Subtitles}
	}
	return nil
}

// Create inserts a preparing track for each kind asked for, in the asset's
// creation transaction. Nothing runs until the asset's first output
// commits (QueueTranscriptions).
func Create(ctx context.Context, qs *db.Queries, projectID, assetID string, g Generate) ([]db.Track, error) {
	var out []db.Track
	for _, kind := range g.Kinds() {
		t, err := qs.CreateTrack(ctx, db.CreateTrackParams{ID: ids.New(ids.Track), ProjectID: projectID, AssetID: assetID, Kind: kind})
		if err != nil {
			return nil, fmt.Errorf("create %s track: %w", kind, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// QueueTranscriptions runs in an output's commit, with the asset locked:
// each subtitles track still being made, with no transcription queued,
// gets one, reading that output; or, when the output has no audio, ends
// with no_audio and no job. A track job already active on the asset (a
// chapters regeneration) leaves them to be queued when it ends.
func QueueTranscriptions(ctx context.Context, qs *db.Queries, asset db.Asset, hasAudio bool) error {
	todo, err := qs.ListTracksToTranscribe(ctx, asset.ID)
	if err != nil || len(todo) == 0 {
		return err
	}
	if !hasAudio {
		for _, t := range todo {
			if err := fail(ctx, qs, t.ID, CodeNoAudio, nil); err != nil {
				return err
			}
		}
		return nil
	}
	active, err := qs.GetActiveJobForAsset(ctx, db.GetActiveJobForAssetParams{AssetID: asset.ID, Kinds: TrackJobs})
	if err != nil || len(active) > 0 {
		return err
	}
	// At most one subtitles track is preparing per asset (tracks_one_preparing).
	_, err = QueueJob(ctx, qs, asset, JobTranscribe, todo[0].ID)
	return err
}

// QueueJob queues the job that makes a track: a transcription (routed like
// any worker job) or chapters (serve's own). Call it with the asset locked
// and no track job active.
func QueueJob(ctx context.Context, qs *db.Queries, asset db.Asset, kind, trackID string) (db.Job, error) {
	return qs.CreateJob(ctx, db.CreateJobParams{
		ID: ids.New(ids.Job), ProjectID: asset.ProjectID, AssetID: asset.ID, Kind: kind,
		TrackID: pgtype.Text{String: trackID, Valid: true},
	})
}

// Fail ends the track a job was making, for good: a failed transcription
// leaves the asset as it is and errors its subtitles track, and the
// asset's chapters still being made with it (subtitles_failed).
func Fail(ctx context.Context, qs *db.Queries, job db.Job, code string) error {
	if !job.TrackID.Valid {
		return fmt.Errorf("job %s makes no track", job.ID)
	}
	if job.Kind == JobTranscribe && !shownCodes[code] {
		code = CodeTranscribeFailed
	}
	return fail(ctx, qs, job.TrackID.String, code, &outbox.Job{ID: job.ID, Kind: job.Kind, ErrorCode: code})
}

// fail errors a track still being made, and its chapters with it, and
// tells the app about each.
func fail(ctx context.Context, qs *db.Queries, trackID, code string, job *outbox.Job) error {
	t, err := qs.ErrorTrack(ctx, db.ErrorTrackParams{ID: trackID, ErrorCode: pgtype.Text{String: code, Valid: true}})
	if err != nil {
		return fmt.Errorf("error track %s: %w", trackID, err)
	}
	if err := qs.BumpAssetForTrack(ctx, t.AssetID); err != nil {
		return err
	}
	if err := outbox.TrackEvent(ctx, qs, t.AssetID, outbox.TrackErrored, outbox.Track{ID: t.ID, Kind: t.Kind, ErrorCode: code}, job); err != nil {
		return err
	}
	if t.Kind != Subtitles {
		return nil
	}
	chapters, err := qs.ErrorPreparingTracksOfKind(ctx, db.ErrorPreparingTracksOfKindParams{
		AssetID: t.AssetID, Kind: Chapters, ErrorCode: pgtype.Text{String: CodeSubtitlesFailed, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("cascade to chapters: %w", err)
	}
	for _, c := range chapters {
		if err := outbox.TrackEvent(ctx, qs, c.AssetID, outbox.TrackErrored, outbox.Track{ID: c.ID, Kind: c.Kind, ErrorCode: CodeSubtitlesFailed}, nil); err != nil {
			return err
		}
	}
	return nil
}

// DeleteAll runs in an asset's delete: every track is deleted and every
// transcript removed at once; the delete job removes the VTT objects with
// the rest of media/{asset}/.
func DeleteAll(ctx context.Context, qs *db.Queries, assetID string) error {
	if _, err := qs.MarkTracksDeleted(ctx, assetID); err != nil {
		return fmt.Errorf("delete tracks: %w", err)
	}
	if _, err := qs.DeleteTranscriptsForAsset(ctx, assetID); err != nil {
		return fmt.Errorf("delete transcripts: %w", err)
	}
	return nil
}
