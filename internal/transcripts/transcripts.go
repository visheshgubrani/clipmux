// Package transcripts holds the canonical transcript (v1), its conversion
// from whisper.cpp and from OpenAI-compatible speech APIs, its validation,
// and the WebVTT it is rendered to (docs/plans/P5-production.md,
// "Transcripts and WebVTT").
//
// Times are integer milliseconds on the output's timeline, which starts at
// zero because the encode resets both streams (media.Pipeline), so nothing
// here shifts captions by an audio offset.
package transcripts

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version is the canonical transcript's format version.
const Version = 1

// Transcript is the canonical transcript: what a worker or the speech API
// produced, checked and normalised, and what subtitles are rendered from.
type Transcript struct {
	Version int `json:"version"`
	// Language is the detected language as ISO 639-1 (a few of Whisper's
	// languages only have a three-letter code), or "und".
	Language   string `json:"language"`
	DurationMS int64  `json:"duration_ms"`
	// Model names what transcribed it: "{name}@sha256:{hex}" for a pinned
	// local model, "{model}@{host}" for a speech API.
	Model string `json:"model"`
	// OutputID is the output whose audio was transcribed. serve sets it from
	// the attempt; a worker's value is never trusted.
	OutputID string    `json:"output_id"`
	Segments []Segment `json:"segments"`
}

// Segment is one recognised span. IDs are "s{n}" by the recogniser's own
// index, so a segment dropped as empty leaves a gap.
type Segment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Words   []Word `json:"words,omitempty"`
}

// Word is one timed word, "s{n}w{m}". Its text keeps the recogniser's
// leading space, so joining a segment's words gives back its text.
type Word struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
}

// Limits on what a transcript may hold. A six-hour lecture has about
// 60,000 words.
const (
	MaxSegments     = 100_000
	MaxWords        = 1_000_000
	MaxSegmentBytes = 4096
	MaxWordBytes    = 256
	MaxModelBytes   = 200
)

// Milliseconds rounds seconds to whole milliseconds as floor(x·1000+0.5),
// never below zero, the one rounding every stored time goes through.
func Milliseconds(seconds float64) (int64, error) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, errors.New("media time must be finite")
	}
	return max(0, int64(math.Floor(seconds*1000+0.5))), nil
}

// FormatMS writes milliseconds as a WebVTT timestamp, HH:MM:SS.mmm.
func FormatMS(ms int64) string {
	ms = max(ms, 0)
	h, rest := ms/3_600_000, ms%3_600_000
	m, rest := rest/60_000, rest%60_000
	s, milli := rest/1000, rest%1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, milli)
}

// InvalidError is a transcript that breaks a rule; serve answers it with
// 409 transcript_invalid.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return "transcript invalid: " + e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

var (
	languageRe  = regexp.MustCompile(`^[a-z]{2,3}$`)
	segmentIDRe = regexp.MustCompile(`^s(0|[1-9][0-9]{0,6})$`)
)

// Validate checks every rule a stored transcript keeps: the version, the
// language, valid UTF-8 without control characters, IDs in order, times
// in order and within the duration, and the size limits.
func (t *Transcript) Validate() error {
	if t.Version != Version {
		return invalid("version %d", t.Version)
	}
	if !languageRe.MatchString(t.Language) {
		return invalid("language %q", t.Language)
	}
	if t.DurationMS <= 0 {
		return invalid("duration %d", t.DurationMS)
	}
	if t.Model == "" || len(t.Model) > MaxModelBytes || !clean(t.Model) || strings.ContainsRune(t.Model, ' ') {
		return invalid("model")
	}
	if len(t.Segments) > MaxSegments {
		return invalid("%d segments", len(t.Segments))
	}
	words, lastIndex, lastStart := 0, -1, int64(0)
	for _, s := range t.Segments {
		m := segmentIDRe.FindStringSubmatch(s.ID)
		if m == nil {
			return invalid("segment id %q", s.ID)
		}
		index, _ := strconv.Atoi(m[1])
		if index <= lastIndex {
			return invalid("segment %s out of order", s.ID)
		}
		lastIndex = index
		if s.StartMS < lastStart || s.StartMS < 0 || s.EndMS <= s.StartMS || s.EndMS > t.DurationMS {
			return invalid("segment %s times %d-%d", s.ID, s.StartMS, s.EndMS)
		}
		lastStart = s.StartMS
		if strings.TrimSpace(s.Text) == "" || len(s.Text) > MaxSegmentBytes || !clean(s.Text) {
			return invalid("segment %s text", s.ID)
		}
		words += len(s.Words)
		if words > MaxWords {
			return invalid("%d words", words)
		}
		wordStart := int64(0)
		for i, w := range s.Words {
			if w.ID != s.ID+"w"+strconv.Itoa(i) {
				return invalid("word id %q in %s", w.ID, s.ID)
			}
			if w.StartMS < wordStart || w.EndMS < w.StartMS || w.EndMS > t.DurationMS {
				return invalid("word %s times %d-%d", w.ID, w.StartMS, w.EndMS)
			}
			wordStart = w.StartMS
			if w.Text == "" || len(w.Text) > MaxWordBytes || !clean(w.Text) {
				return invalid("word %s text", w.ID)
			}
		}
	}
	return nil
}

// clean reports whether s is valid UTF-8 with no control characters.
func clean(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return false
		}
	}
	return true
}

// Sanitize makes recogniser text safe to store: invalid UTF-8 is dropped,
// NUL and other control characters are removed, and line breaks and tabs
// become spaces, so every cue stays on one line.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == ' ' || r == ' ' || r == '\u0085':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// Text is the transcript as one string: the segments' text joined by single
// spaces, with runs of whitespace collapsed.
func (t *Transcript) Text() string {
	var b strings.Builder
	for _, s := range t.Segments {
		for _, f := range strings.Fields(s.Text) {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(f)
		}
	}
	return b.String()
}

// builder accumulates segments while keeping the invariants Validate checks,
// so every converter produces a transcript that validates.
type builder struct {
	t         Transcript
	lastStart int64
}

func newBuilder(language string, durationMS int64, model string) *builder {
	return &builder{t: Transcript{Version: Version, Language: language, DurationMS: durationMS, Model: model, Segments: []Segment{}}}
}

// add appends segment index with its words, clipped to the duration and
// moved forward where the recogniser went backwards. A segment that ends
// up empty or zero-length is dropped.
func (b *builder) add(index int, text string, start, end int64, words []Word) {
	if len(b.t.Segments) >= MaxSegments {
		return
	}
	text = truncate(strings.TrimSpace(Sanitize(text)), MaxSegmentBytes)
	start = min(max(start, b.lastStart, 0), b.t.DurationMS)
	end = min(end, b.t.DurationMS)
	if text == "" || end <= start || text == "[BLANK_AUDIO]" {
		return
	}
	id := "s" + strconv.Itoa(index)
	var kept []Word
	wordStart := int64(0)
	for _, w := range words {
		w.Text = truncate(Sanitize(w.Text), MaxWordBytes)
		if strings.TrimSpace(w.Text) == "" {
			continue
		}
		w.StartMS = min(max(w.StartMS, wordStart, 0), b.t.DurationMS)
		w.EndMS = min(max(w.EndMS, w.StartMS), b.t.DurationMS)
		wordStart = w.StartMS
		w.ID = id + "w" + strconv.Itoa(len(kept))
		kept = append(kept, w)
	}
	b.t.Segments = append(b.t.Segments, Segment{ID: id, Text: text, StartMS: start, EndMS: end, Words: kept})
	b.lastStart = start
}

// truncate cuts s to at most n bytes on a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
