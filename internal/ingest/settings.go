// Package ingest is what every way a video gets in shares: the new
// asset's settings (D150), checked once and stored the same way.
package ingest

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/visheshgubrani/clipmux/internal/assets"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/ids"
	"github.com/visheshgubrani/clipmux/internal/tracks"
)

// Settings are what every way a video gets in takes for its new asset
// (D150): an upload, an import from a URL and an agent source alike. Each
// adds only its source (size, url or agent_source_id).
type Settings struct {
	// PlaybackPolicies make one playback ID each; absent is ["signed"].
	PlaybackPolicies []string `json:"playback_policies,omitempty"`
	Passthrough      string   `json:"passthrough,omitempty"`
	// Title is an optional name for people (D099), up to 200 characters.
	Title string `json:"title,omitempty"`
	// Generate asks for subtitles and chapters (P5-T16); chapters imply
	// subtitles. They are made after the video is ready.
	Generate tracks.Generate `json:"generate,omitzero"`
	// Renditions are the ladder rungs to encode, e.g. ["1080p", "480p"]
	// (D130); absent is every rung up to the source.
	Renditions []string `json:"renditions,omitempty"`
}

// MaxPassthrough is the longest passthrough, in bytes.
const MaxPassthrough = 255

// Checked are Settings ready to store.
type Checked struct {
	PlaybackPolicies []string
	Passthrough      pgtype.Text
	Title            pgtype.Text
	Generate         tracks.Generate
	Renditions       []string
}

// Check validates the settings and fills in the default policy, so a
// request's idempotency body names what it created.
func (s *Settings) Check() (Checked, error) {
	if s.PlaybackPolicies == nil {
		s.PlaybackPolicies = []string{"signed"}
	}
	if err := checkPolicies(s.PlaybackPolicies); err != nil {
		return Checked{}, err
	}
	if len(s.Passthrough) > MaxPassthrough {
		return Checked{}, httpjson.Fail(http.StatusBadRequest, "invalid_request", "passthrough is limited to 255 bytes")
	}
	title, err := assets.Title(s.Title)
	if err != nil {
		return Checked{}, err
	}
	renditions, err := assets.Renditions(s.Renditions)
	if err != nil {
		return Checked{}, err
	}
	return Checked{
		PlaybackPolicies: s.PlaybackPolicies,
		Passthrough:      pgtype.Text{String: s.Passthrough, Valid: s.Passthrough != ""},
		Title:            title,
		Generate:         s.Generate,
		Renditions:       renditions,
	}, nil
}

func checkPolicies(policies []string) error {
	refuse := func(why string) error {
		return httpjson.Fail(http.StatusBadRequest, "invalid_request",
			`playback_policies `+why+`; give "signed", "public" or both, or leave it out for one signed playback ID`)
	}
	if len(policies) == 0 {
		return refuse("must name at least one")
	}
	for i, p := range policies {
		switch {
		case p != "signed" && p != "public":
			return refuse(fmt.Sprintf("can't include %q", p))
		case slices.Contains(policies[:i], p):
			return refuse(fmt.Sprintf("names %s twice", p))
		}
	}
	return nil
}

// Create makes the new asset's playback IDs, one per policy, and
// the tracks it asked for, in the caller's transaction.
func (c Checked) Create(ctx context.Context, q *db.Queries, projectID, assetID string) ([]assets.PlaybackID, error) {
	out := make([]assets.PlaybackID, 0, len(c.PlaybackPolicies))
	for _, policy := range c.PlaybackPolicies {
		pid, err := q.CreatePlaybackID(ctx, db.CreatePlaybackIDParams{
			ID: ids.New(ids.PlaybackID), ProjectID: projectID, AssetID: assetID, Policy: policy,
		})
		if err != nil {
			return nil, fmt.Errorf("create %s playback ID: %w", policy, err)
		}
		out = append(out, assets.PlaybackID{ID: pid.ID, Policy: pid.Policy, Status: pid.Status})
	}
	if _, err := tracks.Create(ctx, q, projectID, assetID, c.Generate); err != nil {
		return nil, err
	}
	return out, nil
}
