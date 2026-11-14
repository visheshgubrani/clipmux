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
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// Local sources (docs/architecture.md, "Local sources"): `clipmux push
// --local` leaves the file on the PC agent, which registered it as an
// AgentSource. The asset records agent://{agent_id}/{source_id}, and its
// jobs are pinned to that agent: no other executor can read the file, so
// fallback never applies.

func agentSourceURI(agentID, sourceID string) string { return "agent://" + agentID + "/" + sourceID }

var errSourceUnavailable = httpjson.Fail(http.StatusConflict, "source_unavailable",
	"the source stays on a PC agent that was revoked; push it again from a connected PC")

// AttachLocal creates a processing asset on an agent's registered source,
// with a transcode job pinned to that agent. It is POST /v1/assets with
// agent_source_id, and what a PC agent calls for the owner's own
// `push --local` (as the Dashboard app).
func (s *Service) AttachLocal(ctx context.Context, key apikeys.Key, req ImportRequest, idemKey string) (assets.View, error) {
	if req.PlaybackPolicy == "" {
		req.PlaybackPolicy = "signed"
	}
	switch {
	case req.URL != "":
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "give url or agent_source_id, not both")
	case req.PlaybackPolicy != "signed" && req.PlaybackPolicy != "public":
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", `playback_policy must be "signed" or "public"`)
	case len(req.Passthrough) > 255:
		return assets.View{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "passthrough is limited to 255 bytes")
	}
	title, err := assets.Title(req.Title)
	if err != nil {
		return assets.View{}, err
	}
	renditions, err := assets.Renditions(req.Renditions)
	if err != nil {
		return assets.View{}, err
	}
	body, _ := json.Marshal(req)
	var out assets.View
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		claim, err := idempotency.Begin(ctx, q, key.ProjectID, key.AppID, idemKey, "POST /v1/assets", body)
		if err != nil {
			return idempotency.Problem(err)
		}
		if claim != nil && claim.Replay != nil {
			return json.Unmarshal(claim.Replay, &out)
		}
		src, err := q.GetAttachableAgentSource(ctx, db.GetAttachableAgentSourceParams{ProjectID: key.ProjectID, ID: req.AgentSourceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return httpjson.Fail(http.StatusNotFound, "agent_source_not_found", "no such agent source, or it already belongs to an asset")
		}
		if err != nil {
			return err
		}
		if src.AgentStatus == "revoked" {
			return errSourceUnavailable
		}
		st, err := q.GetDefaultStorage(ctx, key.ProjectID)
		if err != nil {
			return err
		}
		asset, err := q.CreateLocalAsset(ctx, db.CreateLocalAssetParams{
			ID: ids.New(ids.Asset), ProjectID: key.ProjectID, AppID: key.AppID, StorageID: st.ID,
			SourceUri:     pgtype.Text{String: agentSourceURI(src.AgentID, src.ID), Valid: true},
			AgentSourceID: pgtype.Text{String: src.ID, Valid: true},
			Passthrough:   pgtype.Text{String: req.Passthrough, Valid: req.Passthrough != ""},
			Title:         title,
			Renditions:    renditions,
		})
		if err != nil {
			return err
		}
		if _, err := q.CreatePlaybackID(ctx, db.CreatePlaybackIDParams{
			ID: ids.New(ids.PlaybackID), ProjectID: key.ProjectID, AssetID: asset.ID, Policy: req.PlaybackPolicy,
		}); err != nil {
			return err
		}
		if _, err := tracks.Create(ctx, q, key.ProjectID, asset.ID, req.Generate); err != nil {
			return err
		}
		if _, err := queue.Enqueue(ctx, tx, queue.Job{ProjectID: key.ProjectID, AssetID: asset.ID, Kind: "transcode", PinnedAgentID: src.AgentID}); err != nil {
			return err
		}
		if out, err = assets.Load(ctx, q, asset); err != nil {
			return err
		}
		stored, _ := json.Marshal(out)
		return claim.Save(ctx, http.StatusCreated, stored)
	})
	return out, err
}

// pinnedAgent is the agent an asset's re-encode must run on: its source's,
// or none for a source in storage.
func pinnedAgent(ctx context.Context, q *db.Queries, asset db.Asset) (string, error) {
	if !asset.AgentSourceID.Valid {
		return "", nil
	}
	src, err := q.GetAssetAgentSource(ctx, asset.ID)
	if err != nil {
		return "", err
	}
	if src.AgentStatus == "revoked" {
		return "", errSourceUnavailable
	}
	return src.AgentID, nil
}
