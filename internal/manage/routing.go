package manage

import (
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/accounts"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/queue"
)

// Routing settings (docs/architecture.md, "Routing and fallback"; D053):
// the primary executor, an optional fallback, and the opt-in queue-age rule.

// RoutingView is the project's routing, with the built-in worker's liveness
// so the dashboard can say whether "the server" is a working choice.
type RoutingView struct {
	Primary         string      `json:"primary_executor"`
	Fallback        *string     `json:"fallback_executor"`
	QueueAgeMinutes *int32      `json:"queue_age_fallback_minutes"`
	BuiltinWorker   builtinView `json:"builtin_worker"`
	// Providers is each provider routing could name: connected and verified
	// by its setup test.
	Providers []string `json:"providers_ready"`
}

type builtinView struct {
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Online     bool       `json:"online"`
}

func (m *API) getRouting(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	row, err := db.New(m.pool).GetRouting(r.Context(), o.ProjectID)
	if err != nil {
		return err
	}
	ready, err := m.readyProviders(r)
	if err != nil {
		return err
	}
	v := RoutingView{
		Primary:       row.PrimaryExecutor,
		BuiltinWorker: builtinView{LastSeenAt: optTime(row.BuiltinSeenAt)},
		Providers:     ready,
	}
	if row.FallbackExecutor.Valid {
		v.Fallback = &row.FallbackExecutor.String
	}
	if row.QueueAgeFallbackMinutes.Valid {
		v.QueueAgeMinutes = &row.QueueAgeFallbackMinutes.Int32
	}
	v.BuiltinWorker.Online = row.BuiltinSeenAt.Valid && time.Since(row.BuiltinSeenAt.Time) < queue.OnlineWithin
	httpjson.Write(w, http.StatusOK, v)
	return nil
}

func (m *API) saveRouting(w http.ResponseWriter, r *http.Request, o accounts.Owner) error {
	var body struct {
		Primary         string  `json:"primary_executor"`
		Fallback        *string `json:"fallback_executor"`
		QueueAgeMinutes *int32  `json:"queue_age_fallback_minutes"`
	}
	if err := decode(w, r, &body); err != nil {
		return err
	}
	fallback := ""
	if body.Fallback != nil {
		fallback = *body.Fallback
	}
	ready, err := m.readyProviders(r)
	if err != nil {
		return err
	}
	if err := validRouting(body.Primary, fallback, body.QueueAgeMinutes, ready); err != nil {
		return err
	}
	params := db.SetRoutingParams{ID: o.ProjectID, PrimaryExecutor: body.Primary, FallbackExecutor: optText(fallback)}
	if body.QueueAgeMinutes != nil {
		params.QueueAgeFallbackMinutes = pgtype.Int4{Int32: *body.QueueAgeMinutes, Valid: true}
	}
	if err := db.New(m.pool).SetRouting(r.Context(), params); err != nil {
		return err
	}
	m.Logger.Info("routing saved", "primary", body.Primary, "fallback", fallback)
	return m.getRouting(w, r, o)
}

// validRouting checks a routing choice. The schema enforces the same rules;
// this turns them into errors the dashboard can show.
func validRouting(primary, fallback string, queueAge *int32, ready []string) error {
	bad := func(msg string) error { return httpjson.Fail(http.StatusBadRequest, "invalid_request", msg) }
	notReady := func(kind string) error {
		return httpjson.Fail(http.StatusConflict, "executor_not_connected", kind+" isn't connected, or its setup test hasn't passed")
	}
	switch primary {
	case queue.RouteWorkers, queue.RouteAgents, queue.RouteBuiltin:
	case queue.RouteModal, queue.RouteRunPod:
		if !slices.Contains(ready, primary) {
			return notReady(primary)
		}
	default:
		return bad("primary_executor must be workers, agents, builtin, modal or runpod")
	}
	switch fallback {
	case "":
		if queueAge != nil {
			return bad("queue-age fallback needs a fallback_executor")
		}
		return nil
	case queue.RouteBuiltin:
		if primary == queue.RouteBuiltin || primary == queue.RouteWorkers {
			return bad("the built-in worker is already part of the primary")
		}
	case queue.RouteModal, queue.RouteRunPod:
		if fallback == primary {
			return bad("the fallback can't be the primary")
		}
		if !slices.Contains(ready, fallback) {
			return notReady(fallback)
		}
	default:
		return bad("fallback_executor must be null, builtin, modal or runpod")
	}
	if queueAge != nil && (*queueAge < 1 || *queueAge > 1440) {
		return bad("queue_age_fallback_minutes must be between 1 and 1440")
	}
	return nil
}

// readyProviders are the providers routing may name: connected, with a
// passed setup test.
func (m *API) readyProviders(r *http.Request) ([]string, error) {
	sts, err := dispatch.Statuses(r.Context(), m.pool, db.DefaultProjectID)
	if err != nil {
		return nil, err
	}
	ready := []string{}
	for _, s := range sts {
		if s.Verified {
			ready = append(ready, s.Kind)
		}
	}
	return ready, nil
}
