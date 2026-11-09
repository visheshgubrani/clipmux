package runpod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refused call keeps RunPod's reason (the real-provider run saw bare
// "HTTP 500"s, D097), but never anything key-like it might echo back.
func TestARefusedCallSaysWhyWithoutEchoingSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "create pod: There are no longer any instances available with the requested specifications. env CLIPMUX_WORKER_KEY=cmxd_ABCDEFGHIJKLMNOPQRSTUVWXYZ234567 auth ghp_0123456789abcdefghijklmnopqrstuvwxyz"}`))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "rpa_KEY").CreatePod(context.Background(), PodInput{Name: "x"})
	const want = "runpod create pod: HTTP 500: create pod: There are no longer any instances available with the requested specifications. env CLIPMUX_WORKER_KEY=… auth …"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant  %s", err, want)
	}
	if strings.Contains(err.Error(), "ABCDEFGHIJ") || strings.Contains(err.Error(), "0123456789abc") {
		t.Fatal("a secret came back in the error")
	}
}
