package agents

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
	"github.com/visheshgubrani/clipmux/internal/queue"
)

// Local sources: `clipmux push --local` hands a file to the PC's agent,
// which registers it here by size alone. The path never leaves the agent,
// so the server can't ask it for an arbitrary file (docs/architecture.md,
// threats table).

// maxSyncIDs bounds one sources/sync call.
const maxSyncIDs = 1000

// agent authenticates an agent key on a worker route.
func (s *Service) agent(w http.ResponseWriter, r *http.Request) (queue.Worker, bool) {
	wk, ok := s.AuthenticateWorker(r.Context(), httpjson.Bearer(r))
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "an agent key is required")
		return queue.Worker{}, false
	}
	s.touch(r, wk.AgentID)
	return wk, true
}

func (s *Service) handleSource(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agent(w, r)
	if !ok {
		return
	}
	var req struct {
		Size int64 `json:"size"`
		// UploadSource asks for the file to be uploaded to storage once it
		// has been encoded (push --local --upload-source, D059).
		UploadSource bool `json:"upload_source"`
	}
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if req.Size <= 0 {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "size must be positive")
		return
	}
	src, err := db.New(s.pool).CreateAgentSource(r.Context(), db.CreateAgentSourceParams{
		ID: ids.New(ids.Source), ProjectID: s.projectID, AgentID: wk.AgentID, Size: req.Size, UploadSource: req.UploadSource,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, map[string]any{"id": src.ID, "size": src.Size, "upload_source": src.UploadSource})
}

// handleAttach creates the asset on one of the agent's own sources, as the
// owner's upload (the Dashboard app): `clipmux push --local` on the owner's
// PC needs no API key. It runs after the agent has recorded the file, so
// the pinned job it queues always finds it.
func (s *Service) handleAttach(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agent(w, r)
	if !ok {
		return
	}
	var req lifecycle.ImportRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if s.Life == nil || req.URL != "" {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "attach takes playback_policy, passthrough and generate only")
		return
	}
	qs := db.New(s.pool)
	src, err := qs.GetAttachableAgentSource(r.Context(), db.GetAttachableAgentSourceParams{ProjectID: s.projectID, ID: r.PathValue("id")})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && src.AgentID != wk.AgentID) {
		httpjson.Error(w, http.StatusNotFound, "agent_source_not_found", "no such source of this agent, or it already belongs to an asset")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	app, err := qs.GetBuiltinApp(r.Context(), s.projectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	req.AgentSourceID = src.ID
	key := apikeys.Key{ID: "agent:" + wk.AgentID, ProjectID: s.projectID, AppID: app.ID, Scopes: apikeys.AllScopes()}
	view, err := s.Life.AttachLocal(r.Context(), key, req, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, view)
}

// handleSync tells an agent which of its sources it can forget: their
// assets were deleted, or they were never attached.
func (s *Service) handleSync(w http.ResponseWriter, r *http.Request) {
	wk, ok := s.agent(w, r)
	if !ok {
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if len(req.IDs) > maxSyncIDs {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "sync at most 1000 sources per call")
		return
	}
	gone, err := db.New(s.pool).ListGoneAgentSources(r.Context(), db.ListGoneAgentSourcesParams{Ids: req.IDs, AgentID: wk.AgentID})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"gone": gone})
}

// LocalSource implements workerapi.AgentReporter: the agent source an
// asset's file stays on, if any.
func (s *Service) LocalSource(ctx context.Context, assetID string) (string, int64, error) {
	src, err := db.New(s.pool).GetAssetAgentSource(ctx, assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return src.ID, src.Size, nil
}
