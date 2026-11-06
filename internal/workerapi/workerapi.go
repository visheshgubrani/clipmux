package workerapi

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/stores"
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

// Auths tries each WorkerAuth in turn: the built-in worker's key, then
// agent keys.
type Auths []WorkerAuth

// AuthenticateWorker implements WorkerAuth.
func (as Auths) AuthenticateWorker(ctx context.Context, key string) (queue.Worker, bool) {
	for _, a := range as {
		if w, ok := a.AuthenticateWorker(ctx, key); ok {
			return w, true
		}
	}
	return queue.Worker{}, false
}

// AgentReporter records what PC agents report, and when they were last seen.
type AgentReporter interface {
	ReportClaim(r *http.Request, agentID string, info json.RawMessage, sandboxed bool)
	ReportBeat(r *http.Request, agentID string)
	// RegisterWorker adds the agent-only routes: pairing, and registering
	// local sources.
	RegisterWorker(mux *http.ServeMux)
	// LocalSource is the agent source an asset's file stays on, or "".
	LocalSource(ctx context.Context, assetID string) (id string, size int64, err error)
}

// Sandboxed reports whether a claim's sandbox status enforces what the
// sandbox requires (rlimits, no_new_privs and seccomp; D027). Landlock is
// reported, not required.
func Sandboxed(status string) bool {
	parts := strings.Split(status, ",")
	for _, need := range []string{"rlimits", "no_new_privs", "seccomp"} {
		if !slices.Contains(parts, need) {
			return false
		}
	}
	return true
}

// Source is where a worker downloads the asset's source from. URLs are
// minted at claim time or later, never at enqueue.
type Source struct {
	URL  string `json:"url,omitempty"`
	Size int64  `json:"size"`
	// AgentSourceID replaces URL for a job pinned to the agent that holds
	// the file (push --local); only that agent knows its path.
	AgentSourceID string `json:"agent_source_id,omitempty"`
}

// Encryption is the output's content key, handed over at claim time. It
// opens only this attempt's output.
type Encryption struct {
	Method     string `json:"method"`
	KeyVersion int32  `json:"key_version"`
	Key        string `json:"key"`    // hex
	KeyID      string `json:"key_id"` // hex
}

// Profile names the encoding profile.
type Profile struct {
	ID      string `json:"id"`
	Version int32  `json:"version"`
}

// Job is the claim payload (docs/architecture.md, "Job payload"). It holds
// no storage credentials.
type Job struct {
	ContractVersion  int        `json:"contract_version"`
	AttemptID        string     `json:"attempt_id"`
	AttemptToken     string     `json:"attempt_token"`
	LeaseSeconds     int        `json:"lease_seconds"`
	HeartbeatSeconds int        `json:"heartbeat_seconds"`
	JobID            string     `json:"job_id"`
	Kind             string     `json:"kind"`
	AssetID          string     `json:"asset_id"`
	OutputID         string     `json:"output_id"`
	Source           Source     `json:"source"`
	Profile          Profile    `json:"profile"`
	OutputFormat     int32      `json:"output_format"`
	Encryption       Encryption `json:"encryption"`
}

const (
	// SignBudget caps the files one attempt may sign.
	SignBudget = 20_000
	// MaxSignPaths caps the paths in one sign call.
	MaxSignPaths = 500
	putURLTTL    = 15 * time.Minute
	sourceURLTTL = time.Hour
)

// API serves /worker/v1.
type API struct {
	Queue  *queue.Queue
	Auth   WorkerAuth
	Stores *stores.Set
	Keys   contentkey.Ring
	Commit *outputs.Committer
	// Agents records agent reports and serves pairing; nil for none.
	Agents AgentReporter
	Logger *slog.Logger
}

// New returns the worker API.
func New(q *queue.Queue, auth WorkerAuth, set *stores.Set, keys contentkey.Ring, commit *outputs.Committer) *API {
	return &API{Queue: q, Auth: auth, Stores: set, Keys: keys, Commit: commit, Logger: slog.Default()}
}

// Handler returns the /worker/v1 routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/claim", a.claim)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/heartbeat", a.heartbeat)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/sign", a.sign)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/complete", a.complete)
	mux.HandleFunc("POST /worker/v1/attempts/{id}/fail", a.fail)
	if a.Agents != nil {
		a.Agents.RegisterWorker(mux)
	}
	return mux
}

func sourceKey(assetID string) string { return "uploads/" + assetID + "/source" }

// source signs a fresh download URL for an asset's source.
func (a *API) source(ctx context.Context, assetID string) (Source, error) {
	if a.Agents != nil {
		id, size, err := a.Agents.LocalSource(ctx, assetID)
		if err != nil || id != "" {
			return Source{AgentSourceID: id, Size: size}, err
		}
	}
	store, err := a.Stores.ForAsset(ctx, assetID)
	if err != nil {
		return Source{}, err
	}
	obj, err := store.Head(ctx, sourceKey(assetID))
	if err != nil {
		return Source{}, err
	}
	url, err := store.PresignGet(ctx, sourceKey(assetID), sourceURLTTL)
	return Source{URL: url, Size: obj.Size}, err
}

type claimRequest struct {
	ContractVersions []int    `json:"contract_versions"`
	FFmpegVersion    string   `json:"ffmpeg_version"`
	PackagerVersion  string   `json:"packager_version"`
	Encoders         []string `json:"encoders"`
	FreeDiskBytes    int64    `json:"free_disk_bytes"`
	// Hardware is what the worker's GPUs proved at start, for the Workers
	// page.
	Hardware    json.RawMessage `json:"hardware"`
	WaitSeconds *float64        `json:"wait_seconds"`
	// Sandbox is what confines the worker's tools, e.g.
	// "rlimits,no_new_privs,seccomp,idle,landlock_abi_6", or "none".
	Sandbox string `json:"sandbox"`
}

func (a *API) claim(w http.ResponseWriter, r *http.Request) {
	worker, ok := a.Auth.AuthenticateWorker(r.Context(), httpjson.Bearer(r))
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "a worker key is required")
		return
	}
	var req claimRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if !slices.Contains(req.ContractVersions, ContractVersion) {
		httpjson.Error(w, http.StatusConflict, "unsupported_contract", "this server needs contract version "+strconv.Itoa(ContractVersion))
		return
	}
	if v, ok := ParseVersion(req.PackagerVersion); !ok || CompareVersion(v, MinPackager) < 0 {
		httpjson.Error(w, http.StatusConflict, "packager_too_old", "Shaka Packager 3.9.1 or later is required")
		return
	}
	wait := defaultWait
	if req.WaitSeconds != nil {
		wait = min(max(time.Duration(*req.WaitSeconds*float64(time.Second)), 0), maxWait)
	}
	info, _ := json.Marshal(map[string]any{
		"ffmpeg_version": req.FFmpegVersion, "packager_version": req.PackagerVersion,
		"encoders": req.Encoders, "free_disk_bytes": req.FreeDiskBytes, "contract_versions": req.ContractVersions,
		"sandbox": req.Sandbox, "hardware": req.Hardware,
	})
	worker.Info = info
	worker.FreeDiskBytes = max(req.FreeDiskBytes, 0)
	if worker.AgentID != "" {
		// Sandbox status is the agent's own report: an agent that lies
		// exposes only its own machine to hostile uploads.
		sandboxed := Sandboxed(req.Sandbox)
		worker.Untrusted = !sandboxed
		if a.Agents != nil {
			a.Agents.ReportClaim(r, worker.AgentID, info, sandboxed)
		}
	}

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
	// The worker hung up while the claim was committing: it will never see
	// the token, so give the job back rather than burn a lease on it.
	if r.Context().Err() != nil {
		a.release(r.Context(), c.Token)
		return
	}
	src, err := a.source(r.Context(), c.Job.AssetID)
	if errors.Is(err, storage.ErrNotFound) {
		// The source is gone; no executor can ever run this job.
		if _, ferr := a.Queue.Fail(r.Context(), c.Token, queue.Failure{Permanent: true, Code: "source_missing"}); ferr != nil {
			a.Logger.Error("worker api", "op", "fail sourceless job", "err", ferr)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		a.release(r.Context(), c.Token)
		a.internal(w, "claim source", err)
		return
	}
	secret, ok := a.Keys.Get(c.Output.KeyVersion)
	if !ok {
		a.release(r.Context(), c.Token)
		a.internal(w, "claim key", fmt.Errorf("no content key secret version %d", c.Output.KeyVersion))
		return
	}
	key, kid := secret.Derive(c.Job.AssetID, c.Output.ID), contentkey.KeyID(c.Output.ID)
	httpjson.Write(w, http.StatusOK, Job{
		ContractVersion:  ContractVersion,
		AttemptID:        c.Attempt.ID,
		AttemptToken:     c.Token,
		LeaseSeconds:     int(a.Queue.Lease / time.Second),
		HeartbeatSeconds: int(HeartbeatInterval / time.Second),
		JobID:            c.Job.ID,
		Kind:             c.Job.Kind,
		AssetID:          c.Job.AssetID,
		OutputID:         c.Output.ID,
		Source:           src,
		Profile:          Profile{ID: c.Job.ProfileID, Version: c.Job.ProfileVersion},
		OutputFormat:     c.Output.OutputFormat,
		Encryption: Encryption{
			Method: "aes-128", KeyVersion: c.Output.KeyVersion,
			Key: hex.EncodeToString(key[:]), KeyID: hex.EncodeToString(kid[:]),
		},
	})
}

// maxManifest bounds a complete body: a manifest lists every file.
const maxManifest = 8 << 20

// complete submits the manifest for commit. A rejected output is 409
// output_invalid; the worker reports it as a transient failure, so the job
// retries on a fresh attempt and output.
func (a *API) complete(w http.ResponseWriter, r *http.Request) {
	token, ok := a.attemptToken(w, r)
	if !ok {
		return
	}
	var req struct {
		Manifest media.Manifest `json:"manifest"`
	}
	if !httpjson.DecodeLimit(w, r, &req, maxManifest) {
		return
	}
	v, err := a.Commit.Commit(r.Context(), token, req.Manifest)
	var inv *outputs.InvalidError
	switch {
	case errors.As(err, &inv):
		httpjson.Error(w, http.StatusConflict, "output_invalid", inv.Reason)
	case err != nil:
		a.internal(w, "complete", err)
	case v.Stop:
		a.verdict(w, v)
	default:
		httpjson.Write(w, http.StatusOK, verdictResponse{Action: "done"})
	}
}

// release gives an undelivered claim back to the queue.
func (a *API) release(ctx context.Context, token string) {
	if err := a.Queue.Release(context.WithoutCancel(ctx), token); err != nil {
		a.Logger.Error("worker api", "op", "release undelivered claim", "err", err)
	}
}

type signRequest struct {
	Paths  []string `json:"paths"`
	Source bool     `json:"source"`
}

// SignResponse answers sign: PUT URLs keyed by output path, and a fresh
// source URL when asked for one.
type SignResponse struct {
	Action    string            `json:"action"`
	Reason    string            `json:"reason,omitempty"`
	URLs      map[string]string `json:"urls,omitempty"`
	Source    *Source           `json:"source,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// sign returns PUT URLs for output paths. Paths must match the output
// grammar; the server adds the attempt's own prefix, so an attempt can't
// write outside its output.
func (a *API) sign(w http.ResponseWriter, r *http.Request) {
	token, ok := a.attemptToken(w, r)
	if !ok {
		return
	}
	var req signRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	paths := slices.Compact(slices.Sorted(slices.Values(req.Paths)))
	if len(paths) > MaxSignPaths || (len(paths) == 0 && !req.Source) {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "sign 1 to 500 paths, or ask for the source")
		return
	}
	for _, p := range paths {
		if !media.ValidOutputPath(p) {
			httpjson.Error(w, http.StatusBadRequest, "invalid_path", fmt.Sprintf("%q is not a name the output format allows", p))
			return
		}
	}
	v, att, err := a.Queue.ReserveSigns(r.Context(), token, len(paths), SignBudget)
	if errors.Is(err, queue.ErrSignBudget) {
		httpjson.Error(w, http.StatusConflict, "sign_budget_exceeded", fmt.Sprintf("an attempt may sign at most %d files", SignBudget))
		return
	}
	if err != nil {
		a.internal(w, "sign", err)
		return
	}
	if v.Stop {
		a.verdict(w, v)
		return
	}
	resp := SignResponse{Action: "continue", URLs: make(map[string]string, len(paths)), ExpiresAt: time.Now().Add(putURLTTL).UTC()}
	store, err := a.Stores.ForAsset(r.Context(), att.AssetID)
	if err != nil {
		a.internal(w, "storage", err)
		return
	}
	prefix := "media/" + att.AssetID + "/" + att.OutputID + "/"
	for _, p := range paths {
		url, err := store.PresignPut(r.Context(), prefix+p, putURLTTL)
		if err != nil {
			a.internal(w, "sign put", err)
			return
		}
		resp.URLs[p] = url
	}
	if req.Source {
		src, err := a.source(r.Context(), att.AssetID)
		if err != nil {
			a.internal(w, "sign source", err)
			return
		}
		resp.Source = &src
	}
	httpjson.Write(w, http.StatusOK, resp)
}

type verdictResponse struct {
	Action       string `json:"action"`
	Reason       string `json:"reason,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
}

func (a *API) verdict(w http.ResponseWriter, v queue.Verdict) {
	if v.Stop {
		httpjson.Write(w, http.StatusOK, verdictResponse{Action: "stop", Reason: v.Reason})
		return
	}
	httpjson.Write(w, http.StatusOK, verdictResponse{Action: "continue", LeaseSeconds: int(a.Queue.Lease / time.Second)})
}

// attemptToken checks the bearer is the token of the attempt in the path.
func (a *API) attemptToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, _, ok := a.attempt(w, r)
	return token, ok
}

// attempt authenticates an attempt token for the attempt in the path.
func (a *API) attempt(w http.ResponseWriter, r *http.Request) (string, db.Attempt, bool) {
	token := httpjson.Bearer(r)
	if token == "" {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "an attempt token is required")
		return "", db.Attempt{}, false
	}
	att, err := a.Queue.Authenticate(r.Context(), token)
	if errors.Is(err, queue.ErrUnknownToken) {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "unknown attempt token")
		return "", db.Attempt{}, false
	}
	if err != nil {
		a.internal(w, "authenticate attempt", err)
		return "", db.Attempt{}, false
	}
	if att.ID != r.PathValue("id") {
		httpjson.Error(w, http.StatusForbidden, "attempt_mismatch", "the token belongs to another attempt")
		return "", db.Attempt{}, false
	}
	return token, att, true
}

var phasePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type heartbeatRequest struct {
	Phase    string   `json:"phase"`
	Progress *float32 `json:"progress"`
}

func (a *API) heartbeat(w http.ResponseWriter, r *http.Request) {
	token, att, ok := a.attempt(w, r)
	if !ok {
		return
	}
	if att.AgentID.Valid && a.Agents != nil {
		a.Agents.ReportBeat(r, att.AgentID.String)
	}
	var req heartbeatRequest
	if !httpjson.Decode(w, r, &req) {
		return
	}
	if req.Phase != "" && !phasePattern.MatchString(req.Phase) {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "phase must be snake_case")
		return
	}
	if req.Progress != nil && (*req.Progress < 0 || *req.Progress > 1) {
		httpjson.Error(w, http.StatusBadRequest, "invalid_request", "progress must be between 0 and 1")
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
	if !httpjson.Decode(w, r, &req) {
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
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}

// ParseVersion reads "v3.9.3", "3.9.3" or "v3.9.3-0a8ba4f-release".
func ParseVersion(s string) ([3]int, bool) {
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

// CompareVersion orders two versions like strings.Compare.
func CompareVersion(a, b [3]int) int {
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
