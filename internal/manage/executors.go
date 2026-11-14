package manage

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/modal"
	"github.com/visheshgubrani/clipmux/internal/runpod"
)

// Serverless providers (D056, D057): their status, Connect RunPod and
// Connect Modal, and disconnecting.

func (m *API) listExecutors(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	sts, err := dispatch.Statuses(r.Context(), m.pool, o.ProjectID)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": sts})
	return nil
}

// connectWith runs a provider's Connect with the credentials the owner
// pastes. Every step's result comes back; a failed step can be fixed and
// connected again. Credentials are used for this request and stored sealed.
func connectWith[R any](connect func(context.Context, R) (dispatch.Result, error), invalid error) func(http.ResponseWriter, *http.Request, accounts.Owner) error {
	return func(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
		var req R
		if err := decode(w, r, &req); err != nil {
			return err
		}
		res, err := connect(r.Context(), req)
		if errors.Is(err, invalid) {
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), invalid.Error()+": "))
		}
		if err != nil {
			return err
		}
		httpjson.Write(w, http.StatusOK, res)
		return nil
	}
}

// modelsWith lists an API's models with the URL and key the owner has typed
// so far, for a Connect form's model field. Nothing is kept. refused says
// whether the API turned the key away (401 or 403), which the form names as
// such; any other failure carries the API's own message.
func modelsWith[R, L any](list func(context.Context, R) (L, error), invalid error, refused func(error) bool) func(http.ResponseWriter, *http.Request, accounts.Owner) error {
	return func(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
		var req R
		if err := decode(w, r, &req); err != nil {
			return err
		}
		res, err := list(r.Context(), req)
		switch {
		case errors.Is(err, invalid):
			return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), invalid.Error()+": "))
		case refused(err):
			return httpjson.Fail(http.StatusBadRequest, "key_rejected", "the API refused the key")
		case err != nil:
			return httpjson.Fail(http.StatusBadGateway, "models_unavailable", "couldn't list the models: "+err.Error())
		}
		httpjson.Write(w, http.StatusOK, res)
		return nil
	}
}

// keyStatus is an API's answer that refuses a key.
func keyStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func (m *API) connectRunPod(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.RunPod == nil {
		return errNotFound
	}
	return connectWith(m.RunPod.Connect, runpod.ErrInvalid)(w, r, o)
}

func (m *API) connectModal(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Modal == nil {
		return errNotFound
	}
	return connectWith(m.Modal.Connect, modal.ErrInvalid)(w, r, o)
}

func (m *API) disconnectExecutor(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	var err error
	switch kind := r.PathValue("kind"); {
	case kind == "runpod" && m.RunPod != nil:
		err = m.RunPod.Disconnect(r.Context())
	case kind == "modal" && m.Modal != nil:
		err = m.Modal.Disconnect(r.Context())
	default:
		return errNotFound
	}
	switch {
	case errors.Is(err, dispatch.ErrNotConnected):
		return errNotFound
	case errors.Is(err, dispatch.ErrMachinesRunning):
		return httpjson.Fail(http.StatusConflict, "machines_running", "this provider still runs machines; wait for them to stop")
	case err != nil:
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// modalGPUs lists the GPUs a Modal sandbox can encode on, with Modal's
// published price per hour for a default sandbox (D111).
func (m *API) modalGPUs(w http.ResponseWriter, _ *http.Request, _ accounts.Owner) error {
	if m.Modal == nil {
		return errNotFound
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": modal.GPUs(4, 16384)})
	return nil
}

// runPodGPUs lists the NVENC GPUs RunPod rents on Secure Cloud, priced from
// its catalog with the key the owner pasted (D111). The key isn't kept.
func (m *API) runPodGPUs(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.RunPod == nil {
		return errNotFound
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.APIKey) == "" {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", "api_key is required")
	}
	gpus, err := m.RunPod.GPUs(r.Context(), strings.TrimSpace(req.APIKey))
	var api *runpod.APIError
	if errors.As(err, &api) && (api.Status == http.StatusUnauthorized || api.Status == http.StatusForbidden) {
		return httpjson.Fail(http.StatusBadRequest, "key_rejected", "RunPod refused the API key")
	}
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"data": gpus})
	return nil
}
