package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/idempotency"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outbox"
	"github.com/visheshgubrani/clipmux/internal/routes"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// Tracks through the integration API (P5-T16): regenerating one, and
// deleting one. Reading them is the assets API's.

// TrackRequest is the body of POST /v1/assets/{id}/tracks.
type TrackRequest struct {
	// Kind is subtitles or chapters.
	Kind string `json:"kind"`
	// Supersede cancels the asset's active transcription or chapters job
	// instead of refusing with 409 job_active.
	Supersede bool `json:"supersede,omitempty"`
}

// TrackJob answers a regeneration: the new track, being made, and its job.
type TrackJob struct {
	Track assets.Track `json:"track"`
	Job   JobView      `json:"job"`
}

// RegenerateTrack makes a new track of a kind for a ready asset. The old
// one, if any, keeps playing until the new one is ready (tracks are
// immutable). Subtitles need the video's audio; chapters need ready
// subtitles and a connected LLM.
func (s *Service) RegenerateTrack(ctx context.Context, key apikeys.Key, assetID string, req TrackRequest, idemKey string) (TrackJob, error) {
	if req.Kind != tracks.Subtitles && req.Kind != tracks.Chapters {
		return TrackJob{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", `kind must be "subtitles" or "chapters"`)
	}
	if _, err := db.New(s.pool).GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID}); errors.Is(err, pgx.ErrNoRows) {
		return TrackJob{}, errNotFound
	} else if err != nil {
		return TrackJob{}, err
	}
	body, _ := json.Marshal(req)
	var out TrackJob
	err := errChanged
	for try := 0; errors.Is(err, errChanged) && try < 3; try++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets/{id}/tracks "+assetID, body)
			if err != nil {
				return idempotency.Problem(err)
			}
			if claim != nil && claim.Replay != nil {
				return json.Unmarshal(claim.Replay, &out)
			}
			_, jobs, asset, err := lockForChange(ctx, q, assetID, tracks.TrackJobs)
			if err != nil {
				return err
			}
			switch {
			case asset.Status == "deleted":
				return errAssetDeleted()
			case asset.Status != "ready" || !asset.CurrentOutputID.Valid:
				return httpjson.Fail(http.StatusConflict, "asset_not_ready", "tracks are made from a ready asset's video")
			}
			for _, j := range jobs {
				// A move or delete is never superseded from here.
				if !req.Supersede || !tracks.IsTrackJob(j.Kind) {
					p := httpjson.Fail(http.StatusConflict, "job_active", "another track job is running on this asset; pass supersede to cancel it")
					p.Details = map[string]string{"job_id": j.ID}
					return p
				}
				if err := q.CancelJob(ctx, j.ID); err != nil {
					return err
				}
				// The track it was making gives way to the new one.
				if t, err := q.LockTrack(ctx, j.TrackID.String); err != nil {
					return err
				} else if t.Status == tracks.Preparing {
					if _, err := q.DeleteTrack(ctx, t.ID); err != nil {
						return err
					}
				}
			}
			track, job, err := s.queueTrack(ctx, q, asset, req.Kind)
			if err != nil {
				return err
			}
			if err := q.BumpAssetForTrack(ctx, asset.ID); err != nil {
				return err
			}
			out = TrackJob{Track: assets.TrackView(track), Job: JobView{ID: job.ID, Kind: job.Kind, Status: job.Status}}
			stored, _ := json.Marshal(out)
			return claim.Save(ctx, http.StatusAccepted, stored)
		})
	}
	return out, err
}

// queueTrack checks what a kind needs, then makes (or reuses a waiting)
// track of it and queues its job.
func (s *Service) queueTrack(ctx context.Context, q *db.Queries, asset db.Asset, kind string) (db.Track, db.Job, error) {
	switch kind {
	case tracks.Subtitles:
		out, err := q.GetOutput(ctx, asset.CurrentOutputID.String)
		if err != nil {
			return db.Track{}, db.Job{}, err
		}
		var m struct {
			Audio bool `json:"audio"`
		}
		if err := json.Unmarshal(out.Manifest, &m); err != nil || !m.Audio {
			return db.Track{}, db.Job{}, httpjson.Fail(http.StatusConflict, tracks.CodeNoAudio, "the video has no audio to transcribe")
		}
	case tracks.Chapters:
		if _, err := q.GetTrackOfKind(ctx, db.GetTrackOfKindParams{AssetID: asset.ID, Kind: tracks.Subtitles, Statuses: []string{tracks.Ready}}); errors.Is(err, pgx.ErrNoRows) {
			return db.Track{}, db.Job{}, httpjson.Fail(http.StatusConflict, "subtitles_not_ready", "chapters are made from ready subtitles")
		} else if err != nil {
			return db.Track{}, db.Job{}, err
		}
		conn, err := q.GetAIConnection(ctx, db.GetAIConnectionParams{ProjectID: asset.ProjectID, Purpose: "chapters"})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !conn.VerifiedAt.Valid) {
			return db.Track{}, db.Job{}, httpjson.Fail(http.StatusConflict, "chapters_not_connected", "connect an LLM for chapters first")
		} else if err != nil {
			return db.Track{}, db.Job{}, err
		}
	}
	// A track of the kind still waiting for its job is used as it is.
	track, err := q.GetTrackOfKind(ctx, db.GetTrackOfKindParams{AssetID: asset.ID, Kind: kind, Statuses: []string{tracks.Preparing}})
	if errors.Is(err, pgx.ErrNoRows) {
		track, err = q.CreateTrack(ctx, db.CreateTrackParams{ID: ids.New(ids.Track), ProjectID: asset.ProjectID, AssetID: asset.ID, Kind: kind})
	}
	if err != nil {
		return db.Track{}, db.Job{}, err
	}
	jobKind := tracks.JobTranscribe
	if kind == tracks.Chapters {
		jobKind = tracks.JobChapters
	}
	job, err := tracks.QueueJob(ctx, q, asset, jobKind, track.ID)
	return track, job, err
}

// DeleteTrack deletes one of an asset's tracks. It stops being served at
// once on the built-in handler; it reports whether every route has
// stopped listing it (the Cloudflare Worker reads published records), so
// the API answers 200 then, or 202 while unpublishing. Its job, if it is
// still being made, is cancelled, and chapters waiting on deleted
// subtitles error subtitles_failed.
func (s *Service) DeleteTrack(ctx context.Context, key apikeys.Key, assetID, trackID string) (assets.View, bool, error) {
	if _, err := db.New(s.pool).GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID}); errors.Is(err, pgx.ErrNoRows) {
		return assets.View{}, false, errNotFound
	} else if err != nil {
		return assets.View{}, false, err
	}
	var servable bool
	err := errChanged
	for try := 0; errors.Is(err, errChanged) && try < 3; try++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			_, jobs, asset, err := lockForChange(ctx, q, assetID, tracks.TrackJobs)
			if err != nil {
				return err
			}
			t, err := q.LockTrack(ctx, trackID)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && (t.AssetID != asset.ID || t.Status == tracks.Deleted)) {
				return errNotFound
			}
			if err != nil {
				return err
			}
			for _, j := range jobs {
				if j.TrackID.String == t.ID {
					if err := q.CancelJob(ctx, j.ID); err != nil {
						return err
					}
				}
			}
			servable = t.Status == tracks.Publishing || t.Status == tracks.Ready || t.Status == tracks.Retired
			if _, err := q.DeleteTrack(ctx, t.ID); err != nil {
				return err
			}
			if err := q.DeleteTranscript(ctx, t.ID); err != nil {
				return err
			}
			if t.Kind == tracks.Subtitles && (t.Status == tracks.Preparing || t.Status == tracks.Publishing) {
				if _, err := q.ErrorPreparingTracksOfKind(ctx, db.ErrorPreparingTracksOfKindParams{
					AssetID: asset.ID, Kind: tracks.Chapters, ErrorCode: pgtype.Text{String: tracks.CodeSubtitlesFailed, Valid: true},
				}); err != nil {
					return err
				}
			}
			if servable {
				if err := routes.QueueAssetRefresh(ctx, q, asset.ProjectID, asset.AppID, asset.ID); err != nil {
					return err
				}
			}
			if err := q.BumpAssetForTrack(ctx, asset.ID); err != nil {
				return err
			}
			return outbox.TrackEvent(ctx, q, asset.ID, outbox.TrackDeleted, outbox.Track{ID: t.ID, Kind: t.Kind}, nil)
		})
	}
	if err != nil {
		return assets.View{}, false, err
	}
	q := db.New(s.pool)
	if servable {
		store, err := s.stores.ForAsset(ctx, assetID)
		if err != nil {
			return assets.View{}, false, err
		}
		if err := store.Delete(ctx, tracks.StorageKey(assetID, trackID)); err != nil {
			return assets.View{}, false, err
		}
		if _, err := s.routes.SyncRoutes(ctx, 1000); err != nil {
			s.Logger.Warn("unpublishing a deleted track continues in the background", "track", trackID, "err", err)
		}
	}
	pending, err := q.CountPendingRoutesForAsset(ctx, pgtype.Text{String: assetID, Valid: true})
	if err != nil {
		return assets.View{}, false, err
	}
	asset, err := q.GetAssetByID(ctx, assetID)
	if err != nil {
		return assets.View{}, false, err
	}
	view, err := assets.Load(ctx, q, asset)
	return view, pending == 0, err
}

func (s *Service) handleRegenerateTrack(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var req TrackRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	out, err := s.RegenerateTrack(r.Context(), key, r.PathValue("id"), req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "regenerate track", http.StatusAccepted, out, err)
}

func (s *Service) handleDeleteTrack(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	view, done, err := s.DeleteTrack(r.Context(), key, r.PathValue("id"), r.PathValue("track"))
	status := http.StatusAccepted
	if done {
		status = http.StatusOK
	}
	s.respond(w, "delete track", status, view, err)
}
