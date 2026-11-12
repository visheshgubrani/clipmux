package runpod

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The real browser approval (D109). CLIPMUX_LIVE_APPROVAL=1 starts one at
// RunPod and prints the page to open; CLIPMUX_LIVE_APPROVAL_WAIT=1 also
// waits for the owner to approve it, then reports only that a key came
// back. Approving creates a real API key in the owner's RunPod account.
func TestLiveRunPodApproval(t *testing.T) {
	if os.Getenv("CLIPMUX_LIVE_APPROVAL") == "" {
		t.Skip("CLIPMUX_LIVE_APPROVAL unset")
	}
	ctx := context.Background()
	c := &Connector{}
	a, err := c.BeginApproval(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.URL, DefaultConsole+"/flash/login?request=") {
		t.Fatalf("approval page = %q", a.URL)
	}
	t.Logf("open %s (expires %s)", a.URL, a.Expires.Format(time.RFC3339))
	q := `query flashAuthRequestStatus($flashAuthRequestId: String!) { flashAuthRequestStatus(flashAuthRequestId: $flashAuthRequestId) { id status expiresAt apiKey } }`
	for {
		var out struct {
			Status struct {
				Status string  `json:"status"`
				APIKey *string `json:"apiKey"`
			} `json:"flashAuthRequestStatus"`
		}
		if err := c.graphQL(ctx, "check approval", q, map[string]any{"flashAuthRequestId": a.Flow}, &out); err != nil {
			t.Fatal(err)
		}
		switch s := out.Status.Status; {
		case out.Status.APIKey != nil && *out.Status.APIKey != "":
			t.Logf("approved (%s): a key of %d characters came back", s, len(*out.Status.APIKey))
			return
		case s == "DENIED" || s == "EXPIRED":
			t.Fatalf("approval %s", s)
		case os.Getenv("CLIPMUX_LIVE_APPROVAL_WAIT") == "":
			if s != "PENDING" {
				t.Fatalf("a fresh request = %s, want PENDING", s)
			}
			return
		case !time.Now().Before(a.Expires):
			t.Fatal("not approved in time")
		}
		time.Sleep(2 * time.Second)
	}
}
