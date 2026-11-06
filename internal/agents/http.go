package agents

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// RegisterWorker adds the agent routes under /worker/v1: pairing, which an
// agent calls before it holds a key, and its local sources.
func (s *Service) RegisterWorker(mux *http.ServeMux) {
	mux.HandleFunc("POST /worker/v1/pair", s.handleRedeem)
	mux.HandleFunc("POST /worker/v1/pairings", s.handleStart)
	mux.HandleFunc("POST /worker/v1/pairings/poll", s.handlePoll)
	mux.HandleFunc("POST /worker/v1/sources", s.handleSource)
	mux.HandleFunc("POST /worker/v1/sources/sync", s.handleSync)
	mux.HandleFunc("POST /worker/v1/sources/{id}/attach", s.handleAttach)
}

// Register adds the owner's agent routes. Every one needs the owner's
// session; no API key or agent key reaches them.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents", s.owned(s.handleList))
	mux.HandleFunc("PATCH /v1/agents/{id}", s.owned(s.handleUpdate))
	mux.HandleFunc("DELETE /v1/agents/{id}", s.owned(s.handleRevoke))
	mux.HandleFunc("POST /v1/agents/pairings", s.owned(s.handleInvite))
	mux.HandleFunc("GET /v1/agents/pairings/{code}", s.owned(s.handleGetPairing))
	mux.HandleFunc("POST /v1/agents/pairings/{code}/approve", s.owned(s.handleDecide(true)))
	mux.HandleFunc("POST /v1/agents/pairings/{code}/deny", s.owned(s.handleDecide(false)))
}

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		err = httpjson.Fail(http.StatusNotFound, "not_found", "no such agent")
	}
	if !httpjson.WriteProblem(w, err) {
		s.Logger.Error("agents api", "path", r.URL.Path, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func (s *Service) owned(h func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, err := s.accounts.Owner(r)
		if err == nil && o.ProjectID != s.projectID {
			err = httpjson.Fail(http.StatusNotFound, "not_found", "not found")
		}
		if err == nil {
			err = h(w, r)
		}
		if err != nil && !errors.Is(err, errWritten) {
			s.fail(w, r, err)
		}
	}
}

var errWritten = errors.New("agents: response already written")

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !httpjson.Decode(w, r, v) {
		return errWritten
	}
	return nil
}

// --- agent side ---

type redeemRequest struct {
	Code string `json:"code"`
	Description
}

func (s *Service) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	creds, err := s.Redeem(r.Context(), req.Code, req.Description, s.accounts.ClientIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, creds)
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	var d Description
	if !httpjson.Decode(w, r, &d) {
		return
	}
	req, err := s.Start(r.Context(), d, s.accounts.ClientIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, req)
}

func (s *Service) handlePoll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PollToken string `json:"poll_token"`
	}
	if !httpjson.Decode(w, r, &req) {
		return
	}
	res, err := s.Poll(r.Context(), req.PollToken, s.accounts.ClientIP(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, res)
}

// --- owner side ---

// View is an agent as the Workers page shows it.
type View struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	Scope        string          `json:"scope"`
	Online       bool            `json:"online"`
	Sandboxed    bool            `json:"sandboxed"`
	Hostname     string          `json:"hostname"`
	Platform     string          `json:"platform"`
	LastIP       string          `json:"last_ip,omitempty"`
	LastSeenAt   *time.Time      `json:"last_seen_at"`
	Capabilities json.RawMessage `json:"capabilities"`
	CreatedAt    time.Time       `json:"created_at"`
	RevokedAt    *time.Time      `json:"revoked_at,omitempty"`
}

// OnlineWithin is how recently an agent must have called to count as
// online: a claim long-polls for up to 30 s, and heartbeats come every 15 s.
const OnlineWithin = 60 * time.Second

func optTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func view(a db.Agent, now time.Time) View {
	v := View{
		ID: a.ID, Name: a.Name, Status: a.Status, Scope: a.Scope, Sandboxed: a.Sandboxed,
		Hostname: a.Hostname, Platform: a.Platform, LastSeenAt: optTime(a.LastSeenAt),
		Capabilities: a.Capabilities, CreatedAt: a.CreatedAt.Time, RevokedAt: optTime(a.RevokedAt),
	}
	if len(v.Capabilities) == 0 {
		v.Capabilities = json.RawMessage(`{}`)
	}
	if a.LastIp != nil {
		v.LastIP = a.LastIp.String()
	}
	v.Online = a.Status == "approved" && a.LastSeenAt.Valid && now.Sub(a.LastSeenAt.Time) < OnlineWithin
	return v
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) error {
	list, err := db.New(s.pool).ListAgents(r.Context(), s.projectID)
	if err != nil {
		return err
	}
	out := make([]View, 0, len(list))
	now := time.Now()
	for _, a := range list {
		out = append(out, view(a, now))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": out})
	return nil
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name  *string `json:"name"`
		Scope *string `json:"scope"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	p := db.UpdateAgentParams{ProjectID: s.projectID, ID: r.PathValue("id")}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" || len(n) > 100 {
			return httpjson.Fail(http.StatusBadRequest, "invalid_name", "a name is 1 to 100 characters")
		}
		p.Name = pgtype.Text{String: n, Valid: true}
	}
	if req.Scope != nil {
		if *req.Scope != "all" && *req.Scope != "local" {
			return httpjson.Fail(http.StatusBadRequest, "invalid_scope", `scope is "all" or "local"`)
		}
		p.Scope = pgtype.Text{String: *req.Scope, Valid: true}
	}
	a, err := db.New(s.pool).UpdateAgent(r.Context(), p)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, view(a, time.Now()))
	return nil
}

func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	qs := db.New(s.pool)
	a, fenced, err := s.Revoke(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Revoking twice is harmless: answer with the agent as it is.
		if a, err = qs.GetAgent(r.Context(), db.GetAgentParams{ProjectID: s.projectID, ID: id}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	outputs, err := qs.CountAgentOutputs(r.Context(), pgtype.Text{String: a.ID, Valid: true})
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"agent": view(a, time.Now()), "fenced_attempts": fenced, "current_outputs": outputs,
	})
	return nil
}

// Invitation is a dashboard-minted code with the commands that use it.
type Invitation struct {
	Code          string    `json:"code"`
	ExpiresAt     time.Time `json:"expires_at"`
	Server        string    `json:"server"`
	Command       string    `json:"command"`
	DockerCommand string    `json:"docker_command"`
}

func (s *Service) handleInvite(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if len(req.Name) > 100 {
		return httpjson.Fail(http.StatusBadRequest, "invalid_name", "a name is at most 100 characters")
	}
	code, p, err := s.Invite(r.Context(), req.Name)
	if err != nil {
		return err
	}
	server := strings.TrimRight(s.PublicURL(), "/")
	httpjson.Write(w, http.StatusCreated, Invitation{
		Code: code, ExpiresAt: p.ExpiresAt.Time, Server: server,
		Command:       "clipmux worker --server " + server + " --pair " + code,
		DockerCommand: dockerCommand(server, code),
	})
	return nil
}

// dockerCommand runs the image as an agent: as the host user, keeping its
// connection in the host's ~/.config/clipmux, so `clipmux push --local` on
// the host finds the agent's socket. GPUs need one more flag, which the
// Workers page explains.
func dockerCommand(server, code string) string {
	return `docker run -d --name clipmux-agent --restart unless-stopped --no-healthcheck --user "$(id -u):$(id -g)" ` +
		`-v "$HOME/.config/clipmux:/var/lib/clipmux-agent" -e CLIPMUX_AGENT_DIR=/var/lib/clipmux-agent ` +
		"ghcr.io/visheshgubrani/clipmux worker --server " + server + " --pair " + code
}

// PairingView is a pairing as the owner sees it: for an agent-started one,
// what the machine said about itself, so the owner knows what they approve.
type PairingView struct {
	Status       string    `json:"status"` // pending, approved, denied, consumed or expired
	AgentStarted bool      `json:"agent_started"`
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	Platform     string    `json:"platform"`
	IP           string    `json:"ip,omitempty"`
	AgentID      string    `json:"agent_id,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func pairingView(p db.AgentPairing, now time.Time) PairingView {
	v := PairingView{
		Status: p.Status, AgentStarted: p.PollTokenHash != nil, Name: p.Name, Hostname: p.Hostname,
		Platform: p.Platform, AgentID: p.AgentID.String, ExpiresAt: p.ExpiresAt.Time,
	}
	if p.Ip != nil {
		v.IP = p.Ip.String()
	}
	if (p.Status == "pending" || p.Status == "approved") && !now.Before(p.ExpiresAt.Time) {
		v.Status = "expired"
	}
	return v
}

func (s *Service) handleGetPairing(w http.ResponseWriter, r *http.Request) error {
	n := normalizeCode(r.PathValue("code"))
	if n == "" {
		return errNotFound
	}
	p, err := db.New(s.pool).GetPairingByCode(r.Context(), db.GetPairingByCodeParams{ProjectID: s.projectID, CodeHash: codeHash(n)})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, pairingView(p, time.Now()))
	return nil
}

func (s *Service) handleDecide(approve bool) func(w http.ResponseWriter, r *http.Request) error {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.Decide(r.Context(), r.PathValue("code"), approve)
		if err != nil {
			return err
		}
		httpjson.Write(w, http.StatusOK, pairingView(p, time.Now()))
		return nil
	}
}
