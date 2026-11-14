package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/idempotency"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// JobView is a job as the API returns it.
type JobView struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// ReencodeRequest is the body of POST /v1/assets/{id}/reencode.
type ReencodeRequest struct {
	// Supersede cancels the asset's active job instead of refusing with 409.
	Supersede bool `json:"supersede"`
	// Renditions replaces the asset's choice of rungs for this output and
	// the ones after it (D130); absent keeps the asset's choice.
	Renditions []string `json:"renditions,omitempty"`
}

func errAssetDeleted() error {
	return httpjson.Fail(http.StatusConflict, "asset_deleted", "the asset was deleted")
}

// Reencode queues a new output for one of the key's app's assets from its
// kept source. A ready asset keeps playing its current output until the new
// one commits; an errored one returns to processing. With another job
// active it answers 409 job_active, unless req.Supersede cancels that job.
func (s *Service) Reencode(ctx context.Context, key apikeys.Key, assetID string, req ReencodeRequest, idemKey string) (JobView, error) {
	renditions, err := assets.Renditions(req.Renditions)
	if err != nil {
		return JobView{}, err
	}
	q := db.New(s.pool)
	asset, err := q.GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		return JobView{}, ErrNotFound
	}
	if err != nil {
		return JobView{}, err
	}
	// Checked before any lock; the claim fails the job cleanly if the source
	// disappears after this.
	sourceKept := asset.SourceUri.Valid && asset.SourceKept
	// A source on a PC agent is that agent's to check, at claim.
	pinned, err := pinnedAgent(ctx, q, asset)
	if err != nil {
		return JobView{}, err
	}
	if sourceKept && !asset.AgentSourceID.Valid {
		store, err := s.stores.Get(ctx, asset.StorageID)
		if err != nil {
			return JobView{}, err
		}
		if _, err := store.Head(ctx, asset.SourceUri.String); errors.Is(err, storage.ErrNotFound) {
			sourceKept = false
		} else if err != nil {
			return JobView{}, err
		}
	}
	body, _ := json.Marshal(req)
	var out JobView
	err = errChanged
	for try := 0; errors.Is(err, errChanged) && try < 3; try++ {
		err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := db.New(tx)
			claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets/{id}/reencode "+assetID, body)
			if err != nil {
				return idempotency.Problem(err)
			}
			if claim != nil && claim.Replay != nil {
				return json.Unmarshal(claim.Replay, &out)
			}
			_, jobs, asset, err := lockForChange(ctx, q, assetID, tracks.OutputJobs)
			if err != nil {
				return err
			}
			switch {
			case asset.Status == "deleted":
				return errAssetDeleted()
			case asset.Status == "waiting_upload":
				return httpjson.Fail(http.StatusConflict, "asset_not_ready", "the asset has no source until its upload completes")
			case !sourceKept:
				return httpjson.Fail(http.StatusConflict, "source_missing", "the asset's source was not kept, so it can't be re-encoded")
			}
			if len(jobs) > 0 {
				job := jobs[0]
				if !req.Supersede {
					p := httpjson.Fail(http.StatusConflict, "job_active", "another job is running on this asset; pass supersede to cancel it")
					p.Details = map[string]string{"job_id": job.ID}
					return p
				}
				if err := q.CancelJob(ctx, job.ID); err != nil {
					return err
				}
			}
			if asset.Status == "errored" {
				if _, err := q.MarkAssetReprocessing(ctx, assetID); err != nil {
					return err
				}
			}
			if renditions != nil {
				if err := q.SetAssetRenditions(ctx, db.SetAssetRenditionsParams{ID: assetID, Renditions: renditions}); err != nil {
					return err
				}
			}
			j, err := queue.Enqueue(ctx, tx, queue.Job{ProjectID: asset.ProjectID, AssetID: asset.ID, Kind: "reencode", PinnedAgentID: pinned})
			if err != nil {
				return err
			}
			out = JobView{ID: j.ID, Kind: j.Kind, Status: j.Status}
			stored, _ := json.Marshal(out)
			return claim.Save(ctx, http.StatusAccepted, stored)
		})
	}
	return out, err
}

func (s *Service) handleReencode(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var req ReencodeRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	job, err := s.Reencode(r.Context(), key, r.PathValue("id"), req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "reencode", http.StatusAccepted, job, err)
}

// respond writes v, or the error as its API response.
func (s *Service) respond(w http.ResponseWriter, op string, status int, v any, err error) {
	switch {
	case err == nil:
		httpjson.Write(w, status, v)
	case errors.Is(err, ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "not_found", "not found")
	case httpjson.WriteProblem(w, err):
	default:
		s.Logger.Error("lifecycle api", "op", op, "request_id", w.Header().Get("X-Request-Id"), "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}
