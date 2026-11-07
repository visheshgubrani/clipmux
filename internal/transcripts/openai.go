package transcripts

import (
	"encoding/json"
	"strings"
)

// OpenAIResponse is an OpenAI-compatible `verbose_json` transcription
// (POST /audio/transcriptions with timestamp_granularities segment and
// word), as OpenAI, Groq and self-hosted servers such as Speaches return it.
// Times are seconds from the start of the uploaded file.
type OpenAIResponse struct {
	Language string          `json:"language"`
	Duration json.Number     `json:"duration,omitempty"`
	Text     string          `json:"text"`
	Segments []OpenAISegment `json:"segments"`
	Words    []OpenAIWord    `json:"words"`
}

// OpenAISegment is one segment of a verbose_json response.
type OpenAISegment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// OpenAIWord is one word of a verbose_json response. Its text has no
// leading space and, from most providers, no punctuation.
type OpenAIWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Chunk is one uploaded piece of the audio and what the API answered for it:
// StartMS is where it begins on the output's timeline, SpanMS how long it is.
type Chunk struct {
	StartMS, SpanMS int64
	Response        OpenAIResponse
}

// FromOpenAI joins the chunks' responses into one canonical transcript.
// Each chunk's times are rounded, clamped to the chunk's span and moved by
// its start. Words are given to the segment holding their midpoint, and
// take the segment's own spelling (with punctuation) when the segment's text
// splits into exactly as many words; otherwise the segment is left without
// words and becomes one cue. The language is the first chunk's.
func FromOpenAI(chunks []Chunk, durationMS int64, model string) (*Transcript, error) {
	language := "und"
	if len(chunks) > 0 {
		language = LanguageCode(chunks[0].Response.Language)
	}
	b := newBuilder(language, durationMS, model)
	index := 0
	for _, c := range chunks {
		at := func(seconds float64) (int64, error) {
			ms, err := Milliseconds(seconds)
			if err != nil {
				return 0, invalid("%v", err)
			}
			return c.StartMS + min(ms, c.SpanMS), nil
		}
		type timed struct {
			text       string
			start, end int64
		}
		words := make([]timed, 0, len(c.Response.Words))
		for _, w := range c.Response.Words {
			start, err := at(w.Start)
			if err != nil {
				return nil, err
			}
			end, err := at(w.End)
			if err != nil {
				return nil, err
			}
			words = append(words, timed{w.Word, start, end})
		}
		next := 0
		for si, s := range c.Response.Segments {
			start, err := at(s.Start)
			if err != nil {
				return nil, err
			}
			end, err := at(s.End)
			if err != nil {
				return nil, err
			}
			// Words whose midpoint falls before this segment's end belong to
			// it (or, for the last segment, every word left).
			var mine []timed
			for next < len(words) && (si == len(c.Response.Segments)-1 || (words[next].start+words[next].end)/2 < end) {
				mine = append(mine, words[next])
				next++
			}
			var out []Word
			if spelled := strings.Fields(Sanitize(s.Text)); len(spelled) == len(mine) {
				for i, w := range mine {
					out = append(out, Word{Text: " " + spelled[i], StartMS: w.start, EndMS: w.end})
				}
			}
			b.add(index, s.Text, start, end, out)
			index++
		}
	}
	return &b.t, nil
}
