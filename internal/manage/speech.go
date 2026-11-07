package manage

import (
	"errors"
	"net/http"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/speechapi"
)

// The speech API (D075): its status, the speech mode, Connect and
// disconnecting. Audio leaves the owner's infrastructure only once they
// connect one and choose a mode that uses it.

func (m *API) getSpeech(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Speech == nil {
		return errNotFound
	}
	st, err := m.Speech.Status(r.Context())
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, st)
	return nil
}

func (m *API) setSpeechMode(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Speech == nil {
		return errNotFound
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := decode(w, r, &req); err != nil {
		return err
	}
	switch err := m.Speech.SetMode(r.Context(), req.Mode); {
	case errors.Is(err, speechapi.ErrNotConnected):
		return httpjson.Fail(http.StatusConflict, "speech_not_connected", "connect a speech API before choosing a mode that uses it")
	case errors.Is(err, speechapi.ErrInvalid):
		return httpjson.Fail(http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), speechapi.ErrInvalid.Error()+": "))
	case err != nil:
		return err
	}
	return m.getSpeech(w, r, o)
}

func (m *API) connectSpeech(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Speech == nil {
		return errNotFound
	}
	return connectWith(m.Speech.Connect, speechapi.ErrInvalid)(w, r, o)
}

func (m *API) disconnectSpeech(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Speech == nil {
		return errNotFound
	}
	switch err := m.Speech.Disconnect(r.Context()); {
	case errors.Is(err, speechapi.ErrNotConnected):
		return errNotFound
	case err != nil:
		return err
	}
	return m.getSpeech(w, r, o)
}
