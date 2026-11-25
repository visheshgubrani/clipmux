package push_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/push"
)

func TestAURLIsImportedWithTheSameSettingsAndAStableKey(t *testing.T) {
	var bodies []map[string]any
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/assets" {
			t.Errorf("%s %s, want POST /v1/assets", r.Method, r.URL.Path)
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		bodies, keys = append(bodies, b), append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ast_1","status":"processing","playback_ids":[{"id":"pid_1","policy":"public"}]}`))
	}))
	t.Cleanup(srv.Close)
	c := &push.Client{Server: srv.URL, Key: "cmx_test", HTTP: srv.Client()}
	o := push.Options{Policies: []string{"public"}, Passthrough: "lesson_42", Renditions: []string{"720p"}}
	for range 2 {
		a, err := c.URL(context.Background(), "https://files.example.com/leds.mp4", o)
		if err != nil || a.ID != "ast_1" {
			t.Fatalf("URL() = %+v, %v", a, err)
		}
	}
	b := bodies[0]
	if b["url"] != "https://files.example.com/leds.mp4" || b["passthrough"] != "lesson_42" ||
		!slices.Equal(anyStrings(b["playback_policies"]), []string{"public"}) || !slices.Equal(anyStrings(b["renditions"]), []string{"720p"}) {
		t.Fatalf("body = %v", b)
	}
	if len(keys[0]) != len("push-url-")+32 || keys[0][:9] != "push-url-" || keys[1] != keys[0] {
		t.Fatalf("Idempotency-Keys = %q, want the same push-url-… key twice", keys)
	}
	if _, err := c.URL(context.Background(), "https://files.example.com/leds.mp4", push.Options{}); err != nil {
		t.Fatal(err)
	}
	if keys[2] == keys[0] || !slices.Equal(anyStrings(bodies[2]["playback_policies"]), []string{"signed"}) {
		t.Fatalf("other options gave key %q and policies %v; want a new key and [signed]", keys[2], bodies[2]["playback_policies"])
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}
