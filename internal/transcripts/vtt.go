package transcripts

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Cue rules, from clipmux-lms (transcoding/video/transcript.py): a cue ends
// before a word that would take it past 84 characters, stretch it over 4 s,
// or follow a pause of more than 800 ms.
const (
	CueMaxChars = 84
	CueMaxMS    = 4000
	CueMaxGapMS = 800
)

// TimestampMap is the header line HLS reads to place cues on the media
// timeline. Our outputs start at zero, so cue time 0 is presentation time 0;
// browsers ignore the line.
const TimestampMap = "X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000"

// Cue is one WebVTT cue before ordering and rendering.
type Cue struct {
	StartMS, EndMS int64
	Text           string
}

// Cues builds the subtitle cues: one per segment without words, otherwise
// words grouped by the cue rules.
func (t *Transcript) Cues() []Cue {
	var cues []Cue
	for _, s := range t.Segments {
		if len(s.Words) == 0 {
			cues = append(cues, Cue{s.StartMS, s.EndMS, s.Text})
			continue
		}
		var cur []Word
		flush := func() {
			end, text := int64(0), ""
			for _, w := range cur {
				end = max(end, w.EndMS)
				text += w.Text
			}
			cues = append(cues, Cue{cur[0].StartMS, end, strings.TrimSpace(text)})
			cur = nil
		}
		for _, w := range s.Words {
			if len(cur) > 0 {
				joined := 0
				for _, c := range cur {
					joined += utf8.RuneCountInString(c.Text)
				}
				if joined+utf8.RuneCountInString(w.Text) > CueMaxChars ||
					w.EndMS-cur[0].StartMS > CueMaxMS ||
					w.StartMS-cur[len(cur)-1].EndMS > CueMaxGapMS {
					flush()
				}
			}
			cur = append(cur, w)
		}
		if len(cur) > 0 {
			flush()
		}
	}
	return cues
}

// VTT renders the transcript's subtitles as WebVTT.
func (t *Transcript) VTT() []byte { return RenderVTT(t.Cues(), t.DurationMS) }

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// RenderVTT writes cues as a WebVTT file: in order, each starting no earlier
// than the previous one ended, at least 1 ms long, clipped to the duration,
// numbered from 1, on one line, with &, < and > escaped (so neither markup
// nor a "-->" in the text can change the file's structure). Empty cues are
// left out; with none, the file is just its header.
func RenderVTT(cues []Cue, durationMS int64) []byte {
	var b strings.Builder
	b.WriteString("WEBVTT\n" + TimestampMap + "\n\n")
	prevEnd, n := int64(0), 0
	for _, c := range cues {
		start := max(c.StartMS, prevEnd)
		end := min(durationMS, max(c.EndMS, start+1))
		text := strings.TrimSpace(Sanitize(c.Text))
		if text == "" || start >= end {
			continue
		}
		n++
		b.WriteString(strconv.Itoa(n))
		b.WriteString("\n" + FormatMS(start) + " --> " + FormatMS(end) + "\n")
		b.WriteString(escaper.Replace(text))
		b.WriteString("\n\n")
		prevEnd = end
	}
	return []byte(b.String())
}
