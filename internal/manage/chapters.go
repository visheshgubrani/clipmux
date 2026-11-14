package manage

import (
	"errors"
	"net/http"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/chapters"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

// The chapters LLM (P5-T17): its status, Connect and disconnecting.
// Chapters send transcript text to the connected provider.

func (m *API) getChapters(w http.ResponseWriter, r *http.Request, _ accounts.Owner) error {
	if m.Chapters == nil {
		return errNotFound
	}
	st, err := m.Chapters.Status(r.Context())
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, st)
	return nil
}

func (m *API) connectChapters(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Chapters == nil {
		return errNotFound
	}
	return connectWith(m.Chapters.Connect, chapters.ErrInvalid)(w, r, o)
}

func (m *API) chaptersModels(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Chapters == nil {
		return errNotFound
	}
	return modelsWith(m.Chapters.Models, chapters.ErrInvalid, func(err error) bool {
		var e *chapters.Error
		return errors.As(err, &e) && keyStatus(e.Status)
	})(w, r, o)
}

func (m *API) disconnectChapters(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	if m.Chapters == nil {
		return errNotFound
	}
	switch err := m.Chapters.Disconnect(r.Context()); {
	case errors.Is(err, chapters.ErrNotConnected):
		return errNotFound
	case err != nil:
		return err
	}
	return m.getChapters(w, r, o)
}
