package runpod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refused call keeps RunPod's reason, its RFC 9457 detail and the
// validation errors under it (the real-provider run saw bare "HTTP 500"s,
// D097), but never anything key-like it might echo back.
func TestARefusedCallSaysWhyWithoutEchoingSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"title": "Unprocessable Entity", "status": 422, "detail": "The request body is invalid.", "errors": ["$.env.CLIPMUX_WORKER_KEY: cmxd_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567 is too long", "$: additional properties 'auth' not allowed (ghp_0123456789abcdefghijklmnopqrstuvwxyz)"]}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "rpa_KEY").CreatePod(context.Background(), PodInput{Name: "x"})
	const want = "runpod create pod: HTTP 422: The request body is invalid.; $.env.CLIPMUX_WORKER_KEY: … is too long; $: additional properties 'auth' not allowed (…)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant  %s", err, want)
	}
	if strings.Contains(err.Error(), "ABCDEFGHIJ") || strings.Contains(err.Error(), "0123456789abc") {
		t.Fatal("a secret came back in the error")
	}
}
