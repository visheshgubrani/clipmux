package speechapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The API's models come back trimmed, deduplicated and sorted, with an ID
// longer than Connect accepts dropped; the key goes as a bearer token only
// when there is one.
func TestModelsListsTheAPIsModels(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/openai/v1/models" {
			http.NotFound(w, r)
			return
		}
		auth = append(auth, r.Header.Get("Authorization"))
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"whisper-large-v3-turbo"},{"id":" whisper-large-v3 "},`+
			`{"id":"whisper-large-v3-turbo"},{"id":""},{"id":%q},{"id":"distil-whisper-large-v3-en"}]}`, strings.Repeat("x", 201))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/openai/v1/", Key: "k1"}
	got, listed, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"distil-whisper-large-v3-en", "whisper-large-v3", "whisper-large-v3-turbo"}
	if !listed || !slices.Equal(got, want) {
		t.Fatalf("Models = %q, listed %v; want %q, listed", got, listed, want)
	}
	c.Key = ""
	if _, _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(auth, []string{"Bearer k1", ""}) {
		t.Fatalf("Authorization headers = %q", auth)
	}
}

// A server without the route, or whose answer isn't OpenAI's list, lists
// nothing and isn't an error: Connect still accepts it.
func TestModelsWithoutAListAreUnlisted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"no route", http.StatusNotFound, `{"error":{"message":"not found"}}`},
		{"not JSON", http.StatusOK, "ok"},
		{"another shape", http.StatusOK, `{"models":[{"name":"whisper"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			got, listed, err := (&Client{BaseURL: srv.URL}).Models(context.Background())
			if err != nil || listed || len(got) != 0 {
				t.Fatalf("Models = %q, listed %v, %v; want nothing, unlisted, no error", got, listed, err)
			}
		})
	}
}

// A refused key is a refusal and a server error is transient, so the
// dashboard can tell the owner which it was.
func TestModelsClassifiesRefusals(t *testing.T) {
	for _, tc := range []struct {
		status    int
		transient bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusServiceUnavailable, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, `{"error":{"message":"no"}}`)
		}))
		_, _, err := (&Client{BaseURL: srv.URL, Key: "k1"}).Models(context.Background())
		srv.Close()
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status || apiErr.Transient != tc.transient {
			t.Fatalf("HTTP %d: err = %#v; want status %d, transient %v", tc.status, err, tc.status, tc.transient)
		}
	}
}

// The service checks the URL as Connect does before calling anything.
func TestServiceModelsChecksTheURL(t *testing.T) {
	s := NewService(nil, nil, "prj_test")
	for _, u := range []string{"", "ftp://example.com", "https://user:pw@example.com/v1", "https://example.com/v1?x=1"} {
		if _, err := s.Models(context.Background(), ModelsRequest{BaseURL: u}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Models(%q) = %v; want ErrInvalid", u, err)
		}
	}
}
