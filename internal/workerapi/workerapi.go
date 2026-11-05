package workerapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/queue"
)

// ContractVersion is the job payload / output contract this server speaks.
const ContractVersion = 1

// MinPackager is the oldest Shaka Packager that encrypts CMAF correctly.
var MinPackager = [3]int{3, 9, 1}

const (
	// HeartbeatInterval is how often workers should heartbeat.
	HeartbeatInterval = 15 * time.Second
	maxWait           = 30 * time.Second
	defaultWait       = 25 * time.Second
	pollInterval      = time.Second
	maxBody           = 64 << 10
)

// WorkerAuth decides who may claim work.
type WorkerAuth interface {
	// AuthenticateWorker returns the claiming worker for a bearer key, or
	// false when the key is not a worker key.
	AuthenticateWorker(ctx context.Context, key string) (queue.Worker, bool)
}

// BuiltinKey authenticates the built-in worker with a key minted per boot
// and handed to the child process. Agent and serverless keys arrive in
// Phases 3 and 4.
type BuiltinKey string

// AuthenticateWorker implements WorkerAuth.
func (k BuiltinKey) AuthenticateWorker(_ context.Context, key string) (queue.Worker, bool) {
	if k == "" || subtle.ConstantTimeCompare([]byte(k), []byte(key)) != 1 {
		return queue.Worker{}, false
	}
	return queue.Worker{Executor: "builtin"}, true
}

// Source and Encryption are added to the payload by providers that land
// with storage (P1-T04) and delivery keys (P1-T08).
type Source struct {
	URL           string `json:"url,omitempty"`
	Size          int64  `json:"size"`
	AgentSourceID string `json:"agent_source_id,omitempty"`
}

// Encryption is the per-output content key handed over at claim time.
type Encryption struct {
	Method     string `json:"method"`
	KeyVersion int32  `json:"key_version"`
	Key        string `json:"key"`
}

// API serves /worker/v1.
type API struct {
	Queue  *queue.Queue
	Auth   WorkerAuth
	Logger *slog.Logger
	// Optional payload providers; nil until their tasks land.
	Source     func(ctx context.Context, c *queue.Claim) (*Source, error)
	Encryption func(ctx context.Context, c *queue.Claim) (*Encryption, error)
}

// New returns the worker API.
func New(q *queue.Queue, auth WorkerAuth) *API {
	return &API{Queue: q, Auth: auth, Logger: slog.Default()}
}

// Handler returns the /worker/v1 routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/claim", a.claim)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/heartbeat", a.heartbeat)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/fail", a.fail)
	return mux
}

type claimRequest struct {
	ContractVersions []int    `json:"contract_versions"`
	FFmpegVersion    string   `json:"ffmpeg_version"`
	PackagerVersion  string   `json:"packager_version"`
	Encoders         []string `json:"encoders"`
	FreeDiskBytes    int64    `json:"free_disk_bytes"`
	WaitSeconds      *float64 `json:"wait_seconds"`
}

type profile struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
}

type claimResponse struct {
	ContractVersion  int         `json:"contract_version"`
	AttemptID        string      `json:"attempt_id"`
	AttemptToken     string      `json:"attempt_token"`
	LeaseSeconds     int         `json:"lease_seconds"`
	HeartbeatSeconds int         `json:"heartbeat_seconds"`
	JobID            string      `json:"job_id"`
	Kind             string      `json:"kind"`
	AssetID          string      `json:"asset_id"`
	OutputID         string      `json:"output_id"`
	Source           *Source     `json:"source,omitempty"`
	Profile          profile     `json:"profile"`
	OutputFormat     int32       `json:"output_format"`
	Encryption       *Encryption `json:"encryption,omitempty"`
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	worker, ok := a.Auth.AuthenticateWorker(r.Context(), bearer(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a worker key is required")
		return
	}
	var req claimRequest
	if !decode(w, r, &req) {
		return
	}
	if !slices.Contains(req.ContractVersions, ContractVersion) {
		writeError(w, http.StatusConflict, "unsupported_contract", "this server needs contract version "+strconv.Itoa(ContractVersion))
		return
	}
	if v, ok := parseVersion(req.PackagerVersion); !ok || compareVersion(v, MinPackager) < 0 {
		writeError(w, http.StatusConflict, "packager_too_old", "Shaka Packager 3.9.1 or later is required")
		return
	}
	wait := defaultWait
	if req.WaitSeconds != nil {
		wait = min(max(time.Duration(*req.WaitSeconds*float64(time.Second)), 0), maxWait)
	}
	info, _ := json.Marshal(map[string]any{
		"ffmpeg_version": req.FFmpegVersion, "packager_version": req.PackagerVersion,
		"encoders": req.Encoders, "free_disk_bytes": req.FreeDiskBytes, "contract_versions": req.ContractVersions,
	})
	worker.Info = info

	c, err := a.Queue.ClaimWait(r.Context(), worker, wait, pollInterval)
	if err != nil {
		if r.Context().Err() != nil {
			return // the worker hung up
		}
		a.internal(w, "claim", err)
		return
	}
	if c == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resp := claimResponse{
		ContractVersion:  ContractVersion,
		AttemptID:        c.Attempt.ID,
		AttemptToken:     c.Token,
		LeaseSeconds:     int(a.Queue.Lease / time.Second),
		HeartbeatSeconds: int(HeartbeatInterval / time.Second),
		JobID:            c.Job.ID,
		Kind:             c.Job.Kind,
		AssetID:          c.Job.AssetID,
		OutputID:         c.Output.ID,
		Profile:          profile{ID: c.Job.ProfileID, Version: c.Job.ProfileVersion},
		OutputFormat:     c.Output.OutputFormat,
	}
	// URLs and keys are minted at claim time, never at enqueue.
	if a.Source != nil {
		if resp.Source, err = a.Source(r.Context(), c); err != nil {
			a.internal(w, "claim source", err)
			return
		}
	}
	if a.Encryption != nil {
		if resp.Encryption, err = a.Encryption(r.Context(), c); err != nil {
			a.internal(w, "claim encryption", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type verdictResponse struct {
	Action       string `json:"action"`
	Reason       string `json:"reason,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
}

func (a *API) verdict(w http.ResponseWriter, v queue.Verdict) {
	if v.Stop {
		writeJSON(w, http.StatusOK, verdictResponse{Action: "stop", Reason: v.Reason})
		return
	}
	writeJSON(w, http.StatusOK, verdictResponse{Action: "continue", LeaseSeconds: int(a.Queue.Lease / time.Second)})
}

// attemptToken checks the bearer is the token of the attempt in the path.
func (a *API) attemptToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "an attempt token is required")
		return "", false
	}
	att, err := a.Queue.Authenticate(r.Context(), token)
	if errors.Is(err, queue.ErrUnknownToken) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "unknown attempt token")
		return "", false
	}
	if err != nil {
		a.internal(w, "authenticate attempt", err)
		return "", false
	}
	if att.ID != r.PathValue("id") {
		writeError(w, http.StatusForbidden, "attempt_mismatch", "the token belongs to another attempt")
		return "", false
	}
	return token, true
}

var phasePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type heartbeatRequest struct {
	Phase    string   `json:"phase"`
	Progress *float32 `json:"progress"`
}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	token, ok := a.attemptToken(w, r)
	if !ok {
		return
	}
	var req heartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Phase != "" && !phasePattern.MatchString(req.Phase) {
		writeError(w, http.StatusBadRequest, "invalid_request", "phase must be snake_case")
		return
	}
	if req.Progress != nil && (*req.Progress < 0 || *req.Progress > 1) {
		writeError(w, http.StatusBadRequest, "invalid_request", "progress must be between 0 and 1")
		return
	}
	v, err := a.Queue.Heartbeat(r.Context(), token, queue.Progress{Phase: req.Phase, Progress: req.Progress})
	if err != nil {
		a.internal(w, "heartbeat", err)
		return
	}
	a.verdict(w, v)
}

type failRequest struct {
	Permanent bool   `json:"permanent"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

func (a *API) fail(w http.ResponseWriter, r *http.Request) {
	token, ok := a.attemptToken(w, r)
	if !ok {
		return
	}
	var req failRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := a.Queue.Fail(r.Context(), token, queue.Failure{Permanent: req.Permanent, Code: req.ErrorCode, Message: req.Message})
	if err != nil {
		a.internal(w, "fail", err)
		return
	}
	a.verdict(w, v)
}

func (a *API) internal(w http.ResponseWriter, op string, err error) {
	a.Logger.Error("worker api", "op", op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be a JSON object")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// parseVersion reads "v3.9.3", "3.9.3" or "v3.9.3-0a8ba4f-release".
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func compareVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
