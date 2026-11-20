package manage

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/apikeys"
	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/lifecycle"
)

// The Jobs page (docs/architecture.md, "Routing and fallback"): where each
// job ran, each attempt, and why it ended as it did.

// AttemptView is one executor's run of a job.
type AttemptView struct {
	ID          string `json:"id"`
	Number      int32  `json:"number"`
	Executor    string `json:"executor"`
	RouteReason string `json:"route_reason,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
	Status      string `json:"status"`
	Failure     string `json:"failure_kind,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	// ErrorMessage is what the worker said went wrong (queue.FailureMessage).
	ErrorMessage string          `json:"error_message,omitempty"`
	Phase        string          `json:"phase,omitempty"`
	Progress     *float32        `json:"progress,omitempty"`
	Encoder      json.RawMessage `json:"encoder,omitempty"`
	StartedAt    time.Time       `json:"started_at"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	LastBeatAt   *time.Time      `json:"last_heartbeat_at,omitempty"`
}

// JobView is a job with its attempts.
type JobView struct {
	ID            string        `json:"id"`
	AssetID       string        `json:"asset_id"`
	AppID         string        `json:"app_id"`
	Passthrough   string        `json:"passthrough,omitempty"`
	Kind          string        `json:"kind"`
	Status        string        `json:"status"`
	Phase         string        `json:"phase,omitempty"`
	Progress      *float32      `json:"progress,omitempty"`
	PinnedAgentID string        `json:"pinned_agent_id,omitempty"`
	LostCount     int32         `json:"lost_count"`
	FailedCount   int32         `json:"failed_count"`
	ErrorCode     string        `json:"error_code,omitempty"`
	RoutedTo      string        `json:"routed_to,omitempty"`
	RouteReason   string        `json:"route_reason,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
	Attempts      []AttemptView `json:"attempts"`
}

func optFloat(f pgtype.Float4) *float32 {
	if !f.Valid {
		return nil
	}
	return &f.Float32
}

func jobView(j db.ListJobsRow) JobView {
	return JobView{
		ID: j.ID, AssetID: j.AssetID, AppID: j.AppID, Passthrough: j.Passthrough.String, Kind: j.Kind, Status: j.Status,
		Phase: j.Phase.String, Progress: optFloat(j.Progress), PinnedAgentID: j.PinnedAgentID.String,
		LostCount: j.LostCount, FailedCount: j.FailedCount, ErrorCode: j.ErrorCode.String, RoutedTo: j.RoutedTo.String,
		RouteReason: j.RouteReason.String, CreatedAt: j.CreatedAt.Time, StartedAt: optTime(j.StartedAt), FinishedAt: optTime(j.FinishedAt),
		Attempts: []AttemptView{},
	}
}

// withAttempts fills in each job's attempts with one query.
func (m *API) withAttempts(r *http.Request, jobs []JobView) error {
	if len(jobs) == 0 {
		return nil
	}
	idx := make(map[string]int, len(jobs))
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		idx[j.ID], jobIDs[i] = i, j.ID
	}
	rows, err := db.New(m.pool).ListJobAttempts(r.Context(), jobIDs)
	if err != nil {
		return err
	}
	for _, a := range rows {
		v := AttemptView{
			ID: a.ID, Number: a.AttemptNumber, Executor: a.Executor, RouteReason: a.RouteReason.String, AgentID: a.AgentID.String, AgentName: a.AgentName,
			Status: a.Status, Failure: a.FailureKind.String, ErrorCode: a.ErrorCode.String, ErrorMessage: a.ErrorMessage.String, Phase: a.Phase.String,
			Progress: optFloat(a.Progress), StartedAt: a.StartedAt.Time, FinishedAt: optTime(a.FinishedAt),
			LastBeatAt: optTime(a.LastHeartbeatAt),
		}
		if len(a.Encoder) > 0 && string(a.Encoder) != "null" {
			v.Encoder = a.Encoder
		}
		j := &jobs[idx[a.JobID]]
		j.Attempts = append(j.Attempts, v)
	}
	return nil
}

func (m *API) listJobs(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	query := r.URL.Query()
	before, beforeID := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "~"
	if c := query.Get("cursor"); c != "" {
		var err error
		if before, beforeID, err = assets.DecodeCursor(c); err != nil {
			return httpjson.Fail(http.StatusBadRequest, "invalid_cursor", "cursor is not one this API returned")
		}
	}
	limit := 50
	if l := query.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxPage {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
		}
		limit = n
	}
	rows, err := db.New(m.pool).ListJobs(r.Context(), db.ListJobsParams{
		ProjectID: o.ProjectID, Status: optText(query.Get("status")), AssetID: optText(query.Get("asset_id")),
		AgentID:    optText(query.Get("agent_id")),
		BeforeTime: pgtype.Timestamptz{Time: before, Valid: true}, BeforeID: beforeID, MaxJobs: int32(limit + 1),
	})
	if err != nil {
		return err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = assets.EncodeCursor(rows[limit-1].CreatedAt.Time, rows[limit-1].ID)
	}
	out := make([]JobView, len(rows))
	for i, j := range rows {
		out[i] = jobView(j)
	}
	if err := m.withAttempts(r, out); err != nil {
		return err
	}
	body := map[string]any{"data": out}
	if next != "" {
		body["next_cursor"] = next
	}
	httpjson.Write(w, http.StatusOK, body)
	return nil
}

func (m *API) getJob(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	j, err := db.New(m.pool).GetJobInProject(r.Context(), db.GetJobInProjectParams{ProjectID: o.ProjectID, ID: r.PathValue("id")})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	out := []JobView{jobView(db.ListJobsRow(j))}
	if err := m.withAttempts(r, out); err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, out[0])
	return nil
}

// reencodeAgentOutputs queues a re-encode of every ready asset whose current
// output a given agent produced, as the owner would after revoking a PC it
// no longer trusts. Assets whose source stays on that PC can't be
// re-encoded elsewhere and are reported, not queued.
func (m *API) reencodeAgentOutputs(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Life == nil {
		return errNotFound
	}
	q := db.New(m.pool)
	list, err := q.ListAgentCurrentAssets(r.Context(), db.ListAgentCurrentAssetsParams{
		AgentID: pgtype.Text{String: r.PathValue("id"), Valid: true}, ProjectID: o.ProjectID,
	})
	if err != nil {
		return err
	}
	queued, skipped := []string{}, map[string]string{}
	for _, id := range list {
		asset, err := q.GetAssetInProject(r.Context(), db.GetAssetInProjectParams{ProjectID: o.ProjectID, ID: id})
		if err != nil {
			return err
		}
		key := apikeys.Key{ID: "owner:" + o.ID, ProjectID: o.ProjectID, AppID: asset.AppID, Scopes: apikeys.AllScopes(), Owner: true}
		if _, err := m.Life.Reencode(r.Context(), key, id, lifecycle.ReencodeRequest{}, ""); err != nil {
			var p *httpjson.Problem
			if !errors.As(err, &p) {
				return err
			}
			skipped[id] = p.Code
			continue
		}
		queued = append(queued, id)
	}
	httpjson.Write(w, http.StatusAccepted, map[string]any{"queued": queued, "skipped": skipped})
	return nil
}
