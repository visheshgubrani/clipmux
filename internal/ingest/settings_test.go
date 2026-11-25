package ingest

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/httpjson"
)

func TestSettingsDefaultToOneSignedPlaybackID(t *testing.T) {
	var s Settings
	got, err := s.Check()
	if err != nil || !slices.Equal(got.PlaybackPolicies, []string{"signed"}) {
		t.Fatalf("Check() = %v, %v; want [signed]", got.PlaybackPolicies, err)
	}
	if !slices.Equal(s.PlaybackPolicies, []string{"signed"}) {
		t.Fatalf("settings after Check = %v; the default must be filled in for the idempotency body", s.PlaybackPolicies)
	}
}

func TestSettingsTakeBothPoliciesInOrder(t *testing.T) {
	s := Settings{PlaybackPolicies: []string{"public", "signed"}, Passthrough: "lesson_42", Title: " Lesson 42 "}
	got, err := s.Check()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.PlaybackPolicies, []string{"public", "signed"}) || got.Passthrough.String != "lesson_42" || got.Title.String != "Lesson 42" {
		t.Fatalf("Check() = %+v", got)
	}
}

func TestSettingsRefuseBadValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Settings
		msg  string
	}{
		{"no policies", Settings{PlaybackPolicies: []string{}}, `playback_policies must name at least one; give "signed", "public" or both, or leave it out for one signed playback ID`},
		{"unknown policy", Settings{PlaybackPolicies: []string{"private"}}, `playback_policies can't include "private"; give "signed", "public" or both, or leave it out for one signed playback ID`},
		{"repeated policy", Settings{PlaybackPolicies: []string{"public", "public"}}, `playback_policies names public twice; give "signed", "public" or both, or leave it out for one signed playback ID`},
		{"long passthrough", Settings{Passthrough: strings.Repeat("a", 256)}, "passthrough is limited to 255 bytes"},
		{"bad rendition", Settings{Renditions: []string{"4320p"}}, ""},
		{"long title", Settings{Title: strings.Repeat("é", 201)}, "title is limited to 200 characters"},
	} {
		_, err := tc.s.Check()
		var p *httpjson.Problem
		if !errors.As(err, &p) || p.Status != http.StatusBadRequest || p.Code != "invalid_request" {
			t.Errorf("%s: Check() = %v, want a 400 invalid_request", tc.name, err)
			continue
		}
		if tc.msg != "" && p.Message != tc.msg {
			t.Errorf("%s: message = %q, want %q", tc.name, p.Message, tc.msg)
		}
	}
}
