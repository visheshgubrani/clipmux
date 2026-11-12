package modal

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The real token flow (D109). CLIPMUX_LIVE_APPROVAL=1 starts one at Modal
// and prints the page to open; CLIPMUX_LIVE_APPROVAL_WAIT=1 also waits up
// to approvalLifetime for the owner to approve it, then prints only the
// token's ID. Approving creates a real token in the owner's workspace.
func TestLiveModalApproval(t *testing.T) {
	if os.Getenv("CLIPMUX_LIVE_APPROVAL") == "" {
		t.Skip("CLIPMUX_LIVE_APPROVAL unset")
	}
	ctx := context.Background()
	f := grpcFlows{}
	a, err := f.create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.URL, "https://") || a.Flow == "" || a.Secret == "" {
		t.Fatalf("approval = url %q, flow set %v, secret set %v", a.URL, a.Flow != "", a.Secret != "")
	}
	t.Logf("open %s (code %q)", a.URL, a.Code)
	_, _, done, err := f.wait(ctx, a, time.Second)
	if err != nil || done {
		t.Fatalf("a fresh flow = done %v, %v; want pending", done, err)
	}
	if os.Getenv("CLIPMUX_LIVE_APPROVAL_WAIT") == "" {
		return
	}
	for time.Now().Before(a.Expires) {
		id, secret, done, err := f.wait(ctx, a, approvalWait)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			if secret == "" {
				t.Fatal("approved without a secret")
			}
			t.Logf("approved: token %s", id)
			return
		}
	}
	t.Fatal("not approved in time")
}
