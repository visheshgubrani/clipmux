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
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/outputs"
)

// maxPlaybackIDs bounds an asset's live playback IDs (share links create
// one each).
const maxPlaybackIDs = 1000

// AddPlaybackIDRequest is the body of POST /v1/assets/{id}/playback-ids.
type AddPlaybackIDRequest struct {
	// Policy is "signed" (the default) or "public".
	Policy string `json:"policy"`
}

// AddPlaybackID gives an asset another public or signed handle. It plays as
// soon as its route is published; video.asset.updated says so.
func (s *Service) AddPlaybackID(ctx context.Context, key apikeys.Key, assetID string, req AddPlaybackIDRequest, idemKey string) (assets.PlaybackID, error) {
	if req.Policy == "" {
		req.Policy = "signed"
	}
	if req.Policy != "signed" && req.Policy != "public" {
		return assets.PlaybackID{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", `policy must be "signed" or "public"`)
	}
	body, _ := json.Marshal(req)
	var out assets.PlaybackID
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets/{id}/playback-ids "+assetID, body)
		if err != nil {
			return idempotency.Problem(err)
		}
		if claim != nil && claim.Replay != nil {
			return json.Unmarshal(claim.Replay, &out)
		}
		asset, pid, err := s.AddPlaybackIDIn(ctx, q, key, assetID, req.Policy)
		if err != nil {
			return err
		}
		out = assets.PlaybackID{ID: pid.ID, Policy: pid.Policy, Playable: asset.CurrentOutputID.Valid, Status: pid.Status}
		stored, _ := json.Marshal(out)
		return claim.Save(ctx, http.StatusCreated, stored)
	})
	if err != nil {
		return assets.PlaybackID{}, err
	}
	s.Publish(ctx, assetID)
	return out, nil
}

// AddPlaybackIDIn adds a playback ID to one of the key's app's assets in
// the caller's transaction, under AddPlaybackID's checks: the asset is
// locked, not deleted, and under the 1000-ID cap. Share links (D104) create
// theirs this way. The caller calls Publish once the transaction commits.
func (s *Service) AddPlaybackIDIn(ctx context.Context, q *db.Queries, key apikeys.Key, assetID, policy string) (db.Asset, db.PlaybackID, error) {
	asset, err := s.lockOwnAsset(ctx, q, key, assetID)
	if err != nil {
		return db.Asset{}, db.PlaybackID{}, err
	}
	if asset.Status == "deleted" {
		return db.Asset{}, db.PlaybackID{}, errAssetDeleted()
	}
	if n, err := q.CountLivePlaybackIDs(ctx, assetID); err != nil {
		return db.Asset{}, db.PlaybackID{}, err
	} else if n >= maxPlaybackIDs {
		return db.Asset{}, db.PlaybackID{}, httpjson.Fail(http.StatusConflict, "too_many_playback_ids", "an asset has at most 1000 playback IDs; revoke some first")
	}
	pid, err := q.CreatePlaybackID(ctx, db.CreatePlaybackIDParams{ID: ids.New(ids.PlaybackID), ProjectID: asset.ProjectID, AssetID: asset.ID, Policy: policy})
	if err != nil {
		return db.Asset{}, db.PlaybackID{}, err
	}
	// With no output yet, the commit publishes it with the others.
	if asset.CurrentOutputID.Valid {
		if err := outputs.QueueRoute(ctx, q, asset, pid, asset.CurrentOutputID.String); err != nil {
			return db.Asset{}, db.PlaybackID{}, err
		}
	}
	if _, err := q.BumpAssetVersion(ctx, assetID); err != nil {
		return db.Asset{}, db.PlaybackID{}, err
	}
	return asset, pid, nil
}

// RevokePlaybackID stops one of an asset's playback IDs. It reports whether
// the unpublish is confirmed; until then the ID reads revoking. Delivery
// refuses a revoking ID at once; the Phase 2 Worker within its route cache.
func (s *Service) RevokePlaybackID(ctx context.Context, key apikeys.Key, assetID, playbackID string) (assets.PlaybackID, bool, error) {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		asset, err := s.lockOwnAsset(ctx, q, key, assetID)
		if err != nil {
			return err
		}
		if _, err := q.GetPlaybackIDOfAsset(ctx, db.GetPlaybackIDOfAssetParams{ID: playbackID, AssetID: assetID}); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		n, err := q.MarkPlaybackIDRevoking(ctx, playbackID)
		if err != nil || n == 0 {
			return err // already revoking or revoked
		}
		if err := outputs.QueueUnpublish(ctx, q, asset, playbackID); err != nil {
			return err
		}
		_, err = q.BumpAssetVersion(ctx, assetID)
		return err
	})
	if err != nil {
		return assets.PlaybackID{}, false, err
	}
	s.Publish(ctx, assetID)
	p, err := db.New(s.pool).GetPlaybackIDOfAsset(ctx, db.GetPlaybackIDOfAssetParams{ID: playbackID, AssetID: assetID})
	if err != nil {
		return assets.PlaybackID{}, false, err
	}
	return assets.PlaybackID{ID: p.ID, Policy: p.Policy, Status: p.Status}, p.Status == "revoked", nil
}

// lockOwnAsset locks one of the key's app's assets.
func (s *Service) lockOwnAsset(ctx context.Context, q *db.Queries, key apikeys.Key, assetID string) (db.Asset, error) {
	if _, err := q.GetAsset(ctx, db.GetAssetParams{ID: assetID, AppID: key.AppID}); errors.Is(err, pgx.ErrNoRows) {
		return db.Asset{}, ErrNotFound
	} else if err != nil {
		return db.Asset{}, err
	}
	return q.LockAsset(ctx, assetID)
}

// Publish confirms pending routes right away when Inline allows; otherwise
// (or on failure) the route sweeper does.
func (s *Service) Publish(ctx context.Context, assetID string) {
	if s.Inline <= 0 {
		return
	}
	if _, err := s.routes.SyncRoutes(ctx, 1000); err != nil {
		s.Logger.Warn("route publication deferred", "asset", assetID, "err", err)
	}
}

func (s *Service) handleAddPlaybackID(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	var req AddPlaybackIDRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	p, err := s.AddPlaybackID(r.Context(), key, r.PathValue("id"), req, r.Header.Get("Idempotency-Key"))
	s.respond(w, "add_playback_id", http.StatusCreated, p, err)
}

// handleRevokePlaybackID answers 200 once the ID is unpublished, or 202 while
// it reads revoking.
func (s *Service) handleRevokePlaybackID(w http.ResponseWriter, r *http.Request) {
	key, ok := s.Auth.Require(w, r, apikeys.AssetsWrite)
	if !ok {
		return
	}
	p, done, err := s.RevokePlaybackID(r.Context(), key, r.PathValue("id"), r.PathValue("pid"))
	status := http.StatusAccepted
	if done {
		status = http.StatusOK
	}
	s.respond(w, "revoke_playback_id", status, p, err)
}
