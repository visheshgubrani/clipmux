package dispatch

import "time"

// Approval is a provider's pending browser approval (D109): the owner opens
// URL, checks Code where the provider shows one, and approves; the server
// then receives the provider's credentials directly. Flow and Secret
// identify the approval to the provider and never reach the browser.
type Approval struct {
	URL     string
	Code    string
	Expires time.Time
	Flow    string
	Secret  string
}

// ApprovalStatus is where an approval stands.
type ApprovalStatus string

const (
	// ApprovalPending: the owner hasn't approved yet.
	ApprovalPending ApprovalStatus = "pending"
	// ApprovalApproved: the credentials came back and Connect ran with them.
	ApprovalApproved ApprovalStatus = "approved"
	// ApprovalDenied: the owner refused on the provider's page.
	ApprovalDenied ApprovalStatus = "denied"
	// ApprovalExpired: too late; start again.
	ApprovalExpired ApprovalStatus = "expired"
	// ApprovalShown: the provider showed the key on its own page for the
	// owner to paste, rather than handing it to this server (RunPod does).
	ApprovalShown ApprovalStatus = "shown"
)
