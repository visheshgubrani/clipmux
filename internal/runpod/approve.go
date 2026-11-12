package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
)

// Browser approval (D109). RunPod's Flash CLI gets an API key by having the
// owner approve it in RunPod's console (`flash login`); ClipMux drives the
// same flow from the server: createFlashAuthRequest gives the page to open,
// and flashAuthRequestStatus returns the key once the owner approves. This
// is what RunPod's own CLI uses, not a documented integration API, so
// pasting a key stays the fallback. The approval page names Flash, and the
// key it issues can be restricted or deleted in RunPod's settings.

// DefaultGraphQL is RunPod's GraphQL API and DefaultConsole its console,
// where the owner approves.
const (
	DefaultGraphQL = "https://api.runpod.io/graphql"
	DefaultConsole = "https://console.runpod.io"
)

// approvalLifetime bounds a pending approval when RunPod gives no expiry.
const approvalLifetime = 10 * time.Minute

// BeginApproval starts a browser approval for a new RunPod API key.
func (c *Connector) BeginApproval(ctx context.Context) (dispatch.Approval, error) {
	var out struct {
		Create struct {
			ID        string `json:"id"`
			ExpiresAt string `json:"expiresAt"`
		} `json:"createFlashAuthRequest"`
	}
	if err := c.graphQL(ctx, "start approval", `mutation createFlashAuthRequest { createFlashAuthRequest { id status expiresAt } }`, nil, &out); err != nil {
		return dispatch.Approval{}, err
	}
	if out.Create.ID == "" {
		return dispatch.Approval{}, fmt.Errorf("runpod start approval: no request")
	}
	expires := time.Now().Add(approvalLifetime)
	if t, err := time.Parse(time.RFC3339, out.Create.ExpiresAt); err == nil && t.Before(expires) {
		expires = t
	}
	console := c.Console
	if console == "" {
		console = DefaultConsole
	}
	return dispatch.Approval{
		URL:     strings.TrimRight(console, "/") + "/flash/login?request=" + url.QueryEscape(out.Create.ID),
		Expires: expires, Flow: out.Create.ID,
	}, nil
}

// FinishApproval polls the approval once. When the owner has approved, it
// connects with the key RunPod issued and the settings in req.
func (c *Connector) FinishApproval(ctx context.Context, a dispatch.Approval, req Request) (dispatch.ApprovalStatus, dispatch.Result, error) {
	if !time.Now().Before(a.Expires) {
		return dispatch.ApprovalExpired, dispatch.Result{}, nil
	}
	var out struct {
		Status struct {
			Status string  `json:"status"`
			APIKey *string `json:"apiKey"`
		} `json:"flashAuthRequestStatus"`
	}
	q := `query flashAuthRequestStatus($flashAuthRequestId: String!) { flashAuthRequestStatus(flashAuthRequestId: $flashAuthRequestId) { id status expiresAt apiKey } }`
	if err := c.graphQL(ctx, "check approval", q, map[string]any{"flashAuthRequestId": a.Flow}, &out); err != nil {
		return "", dispatch.Result{}, err
	}
	key := ""
	if out.Status.APIKey != nil {
		key = *out.Status.APIKey
	}
	switch s := out.Status.Status; {
	case key != "" && (s == "APPROVED" || s == "CONSUMED"):
		req.APIKey = key
		res, err := c.Connect(ctx, req)
		return dispatch.ApprovalApproved, res, err
	case s == "DENIED":
		return dispatch.ApprovalDenied, dispatch.Result{}, nil
	case s == "CONSUMED":
		// Consumed without a key: RunPod's page showed the key for the
		// owner to paste, as it did on a real run (D109).
		return dispatch.ApprovalShown, dispatch.Result{}, nil
	case s == "EXPIRED":
		return dispatch.ApprovalExpired, dispatch.Result{}, nil
	}
	return dispatch.ApprovalPending, dispatch.Result{}, nil
}

// graphQL posts one unauthenticated query; the approval flow runs before
// there is a key.
func (c *Connector) graphQL(ctx context.Context, op, query string, vars map[string]any, out any) error {
	base := c.GraphQL
	if base == "" {
		base = DefaultGraphQL
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "clipmux")
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	if res.StatusCode != http.StatusOK {
		return &APIError{Op: op, Status: res.StatusCode}
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	if len(env.Errors) > 0 {
		return &APIError{Op: op, Status: res.StatusCode, Message: tokenLike.ReplaceAllString(env.Errors[0].Message, "…")}
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("runpod %s: %w", op, err)
	}
	return nil
}
