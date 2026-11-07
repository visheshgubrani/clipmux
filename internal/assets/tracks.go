package assets

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/httpjson"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// Chapter is one chapter of a chapters track, in milliseconds.
type Chapter struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Title   string `json:"title"`
}

// TrackDetail is GET /v1/assets/{id}/tracks/{track_id}: the track, and a
// ready chapters track's chapters.
type TrackDetail struct {
	Track
	DurationMS *int64    `json:"duration_ms,omitempty"`
	Chapters   []Chapter `json:"chapters,omitempty"`
}

// TranscriptView is GET .../transcript: what was said, by segment, in
// milliseconds on the video's timeline.
type TranscriptView struct {
	TrackID    string              `json:"track_id"`
	Language   string              `json:"language"`
	DurationMS int64               `json:"duration_ms"`
	Text       string              `json:"text"`
	Segments   []TranscriptSegment `json:"segments"`
}

// TranscriptSegment is one segment, with its words when asked for.
type TranscriptSegment struct {
	ID      string           `json:"id"`
	StartMS int64            `json:"start_ms"`
	EndMS   int64            `json:"end_ms"`
	Text    string           `json:"text"`
	Words   []TranscriptWord `json:"words,omitempty"`
}

// TranscriptWord is one timed word.
type TranscriptWord struct {
	ID      string `json:"id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

// track loads one of the key's app's asset's tracks that the API shows,
// answering 404 for anything else.
func (a *API) track(w http.ResponseWriter, r *http.Request) (db.Track, bool) {
	key, ok := a.authorize(w, r)
	if !ok {
		return db.Track{}, false
	}
	q := db.New(a.pool)
	asset, err := q.GetAsset(r.Context(), db.GetAssetParams{ID: r.PathValue("id"), AppID: key.AppID})
	if errors.Is(err, pgx.ErrNoRows) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such asset")
		return db.Track{}, false
	}
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return db.Track{}, false
	}
	t, err := q.GetTrack(r.Context(), r.PathValue("track"))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (t.AssetID != asset.ID || t.Status == "retired" || t.Status == "deleted")) {
		httpjson.Error(w, http.StatusNotFound, "not_found", "no such track")
		return db.Track{}, false
	}
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return db.Track{}, false
	}
	return t, true
}

func (a *API) getTrack(w http.ResponseWriter, r *http.Request) {
	t, ok := a.track(w, r)
	if !ok {
		return
	}
	d := TrackDetail{Track: TrackView(t)}
	if t.DurationMs.Valid {
		d.DurationMS = &t.DurationMs.Int64
	}
	if t.Kind == "chapters" && t.Status == "ready" && t.Chapters != nil {
		if err := json.Unmarshal(t.Chapters, &d.Chapters); err != nil {
			httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
			return
		}
	}
	httpjson.Write(w, http.StatusOK, d)
}

// getTranscript serves a ready subtitles track's transcript, the moment the
// track reads ready; words only with ?words=true.
func (a *API) getTranscript(w http.ResponseWriter, r *http.Request) {
	t, ok := a.track(w, r)
	if !ok {
		return
	}
	if t.Kind != "subtitles" {
		httpjson.Error(w, http.StatusNotFound, "not_found", "only a subtitles track has a transcript")
		return
	}
	if t.Status != "ready" {
		httpjson.Error(w, http.StatusConflict, "track_not_ready", "the track is "+TrackView(t).Status)
		return
	}
	row, err := db.New(a.pool).GetTranscript(r.Context(), t.ID)
	if err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	var tr transcripts.Transcript
	if err := json.Unmarshal(row.Body, &tr); err != nil {
		httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	words := r.URL.Query().Get("words") == "true"
	v := TranscriptView{TrackID: t.ID, Language: tr.Language, DurationMS: tr.DurationMS, Text: tr.Text(), Segments: make([]TranscriptSegment, 0, len(tr.Segments))}
	for _, s := range tr.Segments {
		seg := TranscriptSegment{ID: s.ID, StartMS: s.StartMS, EndMS: s.EndMS, Text: s.Text}
		if words {
			for _, w := range s.Words {
				seg.Words = append(seg.Words, TranscriptWord{ID: w.ID, StartMS: w.StartMS, EndMS: w.EndMS, Text: w.Text})
			}
		}
		v.Segments = append(v.Segments, seg)
	}
	httpjson.Write(w, http.StatusOK, v)
}
