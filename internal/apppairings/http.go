package apppairings

import (
	"errors"
	"net/http"
	"time"

	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/webhooks"
)

// Register adds the routes: starting and polling a pairing, which the CLI
// calls before it holds any key, and the owner's routes, which need the
// owner's session.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/app-pairings", s.handleStart)
	mux.HandleFunc("POST /v1/app-pairings/poll", s.handlePoll)
	mux.HandleFunc("GET /v1/app-pairings/{code}", s.owned(s.handleGet))
	mux.HandleFunc("POST /v1/app-pairings/{code}/approve", s.owned(s.handleApprove))
	mux.HandleFunc("POST /v1/app-pairings/{code}/deny", s.owned(s.handleDeny))
	mux.HandleFunc("POST /v1/apps/{id}/credentials", s.owned(s.handleCredentials))
}

var errWritten = errors.New("apppairings: response already written")

func (s *Service) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrPublicURLRequired):
		err = httpjson.Fail(http.StatusConflict, "public_url_required", err.Error())
	case errors.Is(err, webhooks.ErrInvalidURL):
		err = httpjson.Fail(http.StatusBadRequest, "invalid_request", webhooks.ErrInvalidURL.Error())
	}
	if !httpjson.WriteProblem(w, err) {
		s.Logger.ErrorContext(r.Context(), "app pairings api", "route", r.Pattern, "err", err)
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

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if !httpjson.Decode(w, r, v) {
		return errWritten
	}
	return nil
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	var d Description
	if !httpjson.Decode(w, r, &d) {
		return
	}
	ip := s.accounts.ClientIP(r)
	if !s.limits.Start(w, ip) {
		return
	}
	req, err := s.Start(r.Context(), d, ip)
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
	if !s.limits.Poll(w, s.accounts.ClientIP(r)) {
		return
	}
	res, err := s.Poll(r.Context(), req.PollToken)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpjson.Write(w, http.StatusOK, res)
}

func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Get(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, view(p, time.Now()))
	return nil
}

func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) error {
	var a Approval
	if err := decode(w, r, &a); err != nil {
		return err
	}
	p, err := s.Approve(r.Context(), r.PathValue("code"), a)
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, view(p, time.Now()))
	return nil
}

func (s *Service) handleDeny(w http.ResponseWriter, r *http.Request) error {
	p, err := s.Deny(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusOK, view(p, time.Now()))
	return nil
}

// handleCredentials issues an app's credentials for the dashboard's
// "Create credentials" dialog, which shows the .env block once.
func (s *Service) handleCredentials(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		WebhookURL string `json:"webhook_url"`
		Local      bool   `json:"local"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	c, err := s.IssueForApp(r.Context(), r.PathValue("id"), Options{
		PublicURL: s.urls.PublicURL(), Local: body.Local, WebhookURL: body.WebhookURL,
	})
	if err != nil {
		return err
	}
	httpjson.Write(w, http.StatusCreated, c)
	return nil
}
