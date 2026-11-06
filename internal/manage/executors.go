package manage

import (
	"errors"
	"net/http"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/runpod"
)

// Serverless providers (D056): their status, Connect RunPod, and
// disconnecting.

func (m *API) listExecutors(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	sts, err := dispatch.Statuses(r.Context(), m.pool, o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": sts})
	return nil
}

// connectRunPod runs Connect RunPod with the key the owner pastes. Every
// step's result comes back; a failed step can be fixed and connected again.
func (m *API) connectRunPod(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.RunPod == nil {
		return errNotFound
	}
	var req runpod.Request
	if err := decode(w, r, &req); err != nil {
		return err
	}
	res, err := m.RunPod.Connect(r.Context(), req)
	if errors.Is(err, runpod.ErrInvalid) {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), runpod.ErrInvalid.Error()+": "))
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, res)
	return nil
}

func (m *API) disconnectExecutor(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	var err error
	switch kind := r.PathValue("kind"); {
	case kind == "runpod" && m.RunPod != nil:
		err = m.RunPod.Disconnect(r.Context())
	default:
		return errNotFound
	}
	switch {
	case errors.Is(err, dispatch.ErrNotConnected):
		return errNotFound
	case errors.Is(err, dispatch.ErrInUse):
		return httpjson.Fail(http.StatusConflict, "executor_in_use", "routing names this provider; choose another primary and fallback first")
	case errors.Is(err, dispatch.ErrMachinesRunning):
		return httpjson.Fail(http.StatusConflict, "machines_running", "this provider still runs machines; wait for them to stop")
	case err != nil:
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
