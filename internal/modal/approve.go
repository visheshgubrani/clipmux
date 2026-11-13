package modal

import (
	"context"
	"crypto/tls"
	"fmt"
	"strconv"
	"time"

	pb "github.com/modal-labs/modal-client/go/proto/modal_proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
)

// Browser approval (D109). Modal's CLI gets a token by having the owner
// approve it in the browser (`modal token new`); ClipMux drives the same
// flow from the server: TokenFlowCreate gives the page to open, and
// TokenFlowWait returns the token once the owner approves it. These RPCs
// are what Modal's own CLI uses, not a documented integration API, so
// pasting a token stays the fallback.

// approvalLifetime bounds a pending approval; Modal doesn't say how long a
// flow lasts, so ClipMux stops waiting after this.
const approvalLifetime = 10 * time.Minute

// approvalWait is how long one poll waits on Modal for the owner.
const approvalWait = 10 * time.Second

// flows is Modal's token flow; tests replace it.
type flows interface {
	create(ctx context.Context) (dispatch.Approval, error)
	// wait returns the token once approved; done is false while pending.
	wait(ctx context.Context, a dispatch.Approval, timeout time.Duration) (tokenID, tokenSecret string, done bool, err error)
}

// BeginApproval starts a browser approval for a new Modal token.
func (c *Connector) BeginApproval(ctx context.Context) (dispatch.Approval, error) {
	return c.tokenFlows().create(ctx)
}

// FinishApproval polls the approval once. When the owner has approved, it
// connects with the token Modal issued and the settings in req.
func (c *Connector) FinishApproval(ctx context.Context, a dispatch.Approval, req Request) (dispatch.ApprovalStatus, dispatch.Result, error) {
	if !time.Now().Before(a.Expires) {
		return dispatch.ApprovalExpired, dispatch.Result{}, nil
	}
	id, secret, done, err := c.tokenFlows().wait(ctx, a, approvalWait)
	if err != nil {
		return "", dispatch.Result{}, err
	}
	if !done {
		return dispatch.ApprovalPending, dispatch.Result{}, nil
	}
	req.TokenID, req.TokenSecret = id, secret
	res, err := c.Connect(ctx, req)
	return dispatch.ApprovalApproved, res, err
}

func (c *Connector) tokenFlows() flows {
	if c.flows != nil {
		return c.flows
	}
	return grpcFlows{}
}

// grpcFlows calls Modal's API without credentials, as `modal token new`
// does before it has a token.
type grpcFlows struct{}

const modalHost = "api.modal.com"

func (grpcFlows) call(ctx context.Context, fn func(context.Context, pb.ModalClientClient) error) error {
	return callModal(ctx, nil, fn)
}

// callModal calls Modal's API directly, for what the SDK doesn't wrap: the
// token flow (no credentials) and the billing report (the token's headers,
// as the SDK sends them).
func callModal(ctx context.Context, headers []string, fn func(context.Context, pb.ModalClientClient) error) error {
	conn, err := grpc.NewClient(modalHost+":443", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})))
	if err != nil {
		return fmt.Errorf("modal: %w", err)
	}
	defer conn.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, append([]string{
		"x-modal-client-type", strconv.Itoa(int(pb.ClientType_CLIENT_TYPE_LIBMODAL_GO)),
		"x-modal-client-version", "1.0.0",
		"x-modal-host", modalHost,
	}, headers...)...)
	return fn(ctx, pb.NewModalClientClient(conn))
}

func (g grpcFlows) create(ctx context.Context) (dispatch.Approval, error) {
	var a dispatch.Approval
	err := g.call(ctx, func(ctx context.Context, cl pb.ModalClientClient) error {
		// ExpiresInSeconds would be the issued token's lifetime; zero keeps
		// it until the owner revokes it, as `modal token new` does.
		resp, err := cl.TokenFlowCreate(ctx, pb.TokenFlowCreateRequest_builder{UtmSource: "clipmux"}.Build())
		if err != nil {
			return fmt.Errorf("modal token flow: %w", err)
		}
		if resp.GetWebUrl() == "" || resp.GetTokenFlowId() == "" {
			return fmt.Errorf("modal token flow: no page to open")
		}
		a = dispatch.Approval{
			URL: resp.GetWebUrl(), Code: resp.GetCode(), Expires: time.Now().Add(approvalLifetime),
			Flow: resp.GetTokenFlowId(), Secret: resp.GetWaitSecret(),
		}
		return nil
	})
	return a, err
}

func (g grpcFlows) wait(ctx context.Context, a dispatch.Approval, timeout time.Duration) (id, secret string, done bool, err error) {
	err = g.call(ctx, func(ctx context.Context, cl pb.ModalClientClient) error {
		ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
		defer cancel()
		resp, err := cl.TokenFlowWait(ctx, pb.TokenFlowWaitRequest_builder{
			TokenFlowId: a.Flow, WaitSecret: a.Secret, Timeout: float32(timeout.Seconds()),
		}.Build())
		if err != nil {
			return fmt.Errorf("modal token flow: %w", err)
		}
		if resp.GetTimeout() {
			return nil
		}
		id, secret, done = resp.GetTokenId(), resp.GetTokenSecret(), true
		if id == "" || secret == "" {
			return fmt.Errorf("modal token flow: approved without a token")
		}
		return nil
	})
	return id, secret, done, err
}
