package webhooks

import "testing"

// The vector is the one the LMS verifier and clipmux-lms share
// (pipeline-auth.spec.ts), checked with
// `printf '1700000000.{"job_id":"abc","stage":"video"}' | openssl dgst -sha256 -hmac test-secret`.
func TestSignatureMatchesTheLMSVector(t *testing.T) {
	got := Sign([]byte("test-secret"), 1700000000, []byte(`{"job_id":"abc","stage":"video"}`))
	want := "sha256=fac6876fcc981593ebc1248ba3ce771b001862c52f3863ff0a728f73c6a44e2f"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
