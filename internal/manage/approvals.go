package manage

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/modal"
	"github.com/visheshgubrani/clipmux/internal/runpod"
)

// Browser approval for Modal and RunPod (D109): instead of pasting a key,
// the owner starts an approval with the settings they chose, approves on
// the provider's page, and the dashboard polls. Once approved, the
// provider's Connect runs with the credentials it issued; they go from the
// provider to this server and are sealed like pasted ones, never sent to
// the browser. Pending approvals live in memory: a restart only means
// starting again.

// maxPendingApprovals bounds the approvals one project has waiting.
const maxPendingApprovals = 3

type pendingApproval struct {
	project string
	kind    string
	expires time.Time
	// finish polls the provider once and connects when approved.
	finish func(ctx context.Context) (dispatch.ApprovalStatus, dispatch.Result, error)

	mu    sync.Mutex // one poll at a time, so Connect runs once
	final *approvalView
}

type approvalView struct {
	ID      string                  `json:"id"`
	URL     string                  `json:"url,omitempty"`
	Code    string                  `json:"code,omitempty"`
	Expires time.Time               `json:"expires_at"`
	Status  dispatch.ApprovalStatus `json:"status"`
	Result  *dispatch.Result        `json:"result,omitempty"`
}

// approvals holds the pending approvals; the zero value is ready.
type approvals struct {
	mu sync.Mutex
	m  map[string]*pendingApproval
}

// add keeps p unless its project already has the most waiting, and drops
// expired ones first.
func (a *approvals) add(p *pendingApproval, now time.Time) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]*pendingApproval{}
	}
	waiting := 0
	for id, q := range a.m {
		switch {
		case !now.Before(q.expires):
			delete(a.m, id)
		case q.project == p.project && q.finalView() == nil:
			waiting++
		}
	}
	if waiting >= maxPendingApprovals {
		return "", false
	}
	id := "apr_" + strings.ToLower(rand.Text()[:16])
	a.m[id] = p
	return id, true
}

func (a *approvals) get(id, project, kind string, now time.Time) *pendingApproval {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.m[id]
	if p == nil || p.project != project || p.kind != kind || !now.Before(p.expires) {
		return nil
	}
	return p
}

func (p *pendingApproval) finalView() *approvalView {
	if !p.mu.TryLock() {
		return nil // a poll is running; it isn't final yet
	}
	defer p.mu.Unlock()
	return p.final
}

// startApproval starts a provider's browser approval for the settings in
// the body, refusing settings its Connect would refuse first, so no key is
// issued for them.
func startApproval[R any](kind string, check func(R) error, begin func(context.Context) (dispatch.Approval, error),
	finish func(context.Context, dispatch.Approval, R) (dispatch.ApprovalStatus, dispatch.Result, error), invalid error, m *API,
) func(http.ResponseWriter, *http.Request, accounts.Owner) error {
	return func(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
		var req R
		if err := decode(w, r, &req); err != nil {
			return err
		}
		if err := check(req); err != nil {
			if errors.Is(err, invalid) {
				return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), invalid.Error()+": "))
			}
			return err
		}
		a, err := begin(r.Context())
		if err != nil {
			m.Logger.WarnContext(r.Context(), "browser approval refused", "provider", kind, "error", err)
			return httpjson.Fail(http.StatusBadGateway, "approval_unavailable", "couldn't start the approval at "+kind+"; paste a key instead")
		}
		p := &pendingApproval{
			project: o.ProjectID, kind: kind, expires: a.Expires,
			finish: func(ctx context.Context) (dispatch.ApprovalStatus, dispatch.Result, error) {
				return finish(ctx, a, req)
			},
		}
		id, ok := m.approvals.add(p, m.Now())
		if !ok {
			return httpjson.Fail(http.StatusTooManyRequests, "too_many_approvals", "three approvals are already waiting; finish one or wait for it to expire")
		}
		httpjson.Write(w, http.StatusCreated, approvalView{ID: id, URL: a.URL, Code: a.Code, Expires: a.Expires.UTC(), Status: dispatch.ApprovalPending})
		return nil
	}
}

// pollApproval checks a pending approval once. While another poll for it
// runs, it answers pending; once settled, every poll gets the same answer.
func (m *API) pollApproval(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	kind := r.PathValue("kind")
	p := m.approvals.get(r.PathValue("id"), o.ProjectID, kind, m.Now())
	if p == nil {
		return httpjson.Fail(http.StatusNotFound, "not_found", "no such approval; it may have expired, so start again")
	}
	view := approvalView{ID: r.PathValue("id"), Expires: p.expires.UTC(), Status: dispatch.ApprovalPending}
	if !p.mu.TryLock() {
		httpjson.Write(w, http.StatusOK, view)
		return nil
	}
	defer p.mu.Unlock()
	if p.final != nil {
		httpjson.Write(w, http.StatusOK, p.final)
		return nil
	}
	st, res, err := p.finish(r.Context())
	for _, invalid := range []error{modal.ErrInvalid, runpod.ErrInvalid} {
		if errors.Is(err, invalid) {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), invalid.Error()+": "))
		}
	}
	if err != nil {
		return err
	}
	view.Status = st
	if st == dispatch.ApprovalApproved {
		view.Result = &res
	}
	if st != dispatch.ApprovalPending {
		p.final = &view
		m.Logger.InfoContext(r.Context(), "browser approval settled", "provider", kind, "status", st, "connected", res.Connected)
	}
	httpjson.Write(w, http.StatusOK, view)
	return nil
}

func (m *API) startRunPodApproval(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.RunPod == nil {
		return errNotFound
	}
	return startApproval("runpod", m.RunPod.CheckSettings, m.RunPod.BeginApproval, m.RunPod.FinishApproval, runpod.ErrInvalid, m)(w, r, o)
}

func (m *API) startModalApproval(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Modal == nil {
		return errNotFound
	}
	return startApproval("modal", m.Modal.CheckSettings, m.Modal.BeginApproval, m.Modal.FinishApproval, modal.ErrInvalid, m)(w, r, o)
}
