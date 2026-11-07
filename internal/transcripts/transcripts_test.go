package transcripts

import (
	"errors"
	"math"
	"strings"
	"testing"
)

const header = "WEBVTT\n" + TimestampMap + "\n\n"

// fixture mirrors clipmux-lms's test transcript: segment i runs from
// i·10+1.234 s to i·10+3.456 s with one word ending at i·10+2.345 s, over
// a 30.789 s output.
func fixture(texts ...string) *Transcript {
	t := &Transcript{Version: 1, Language: "en", DurationMS: 30789, Model: "large-v3-turbo-q5_0@sha256:00", OutputID: "out_1"}
	for i, text := range texts {
		base := int64(i) * 10_000
		id := "s" + itoa(i)
		t.Segments = append(t.Segments, Segment{
			ID: id, Text: text, StartMS: base + 1234, EndMS: base + 3456,
			Words: []Word{{ID: id + "w0", Text: " " + text, StartMS: base + 1234, EndMS: base + 2345}},
		})
	}
	return t
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestRoundingCarriesAcrossMinutesAndHours(t *testing.T) {
	for _, c := range []struct {
		seconds float64
		want    string
	}{
		{59.9994, "00:00:59.999"},
		{59.9996, "00:01:00.000"},
		{3599.9996, "01:00:00.000"},
		{3661.234, "01:01:01.234"},
		{-0.2, "00:00:00.000"},
	} {
		ms, err := Milliseconds(c.seconds)
		if err != nil {
			t.Fatal(err)
		}
		if got := FormatMS(ms); got != c.want {
			t.Errorf("%v s = %s, want %s", c.seconds, got, c.want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Milliseconds(v); err == nil {
			t.Errorf("%v must be refused", v)
		}
	}
}

func TestCuesAreEscapedOrderedAndOnOneLine(t *testing.T) {
	tr := fixture("<script>Unsafe & text</script>", "Later.")
	// The recogniser went backwards: the second word overlaps the first cue.
	tr.Segments[1].Words[0].StartMS, tr.Segments[1].Words[0].EndMS = 1000, 4000
	want := header +
		"1\n00:00:01.234 --> 00:00:02.345\n&lt;script&gt;Unsafe &amp; text&lt;/script&gt;\n\n" +
		"2\n00:00:02.345 --> 00:00:04.000\nLater.\n\n"
	if got := string(tr.VTT()); got != want {
		t.Fatalf("vtt:\n%q\nwant\n%q", got, want)
	}
}

func TestArrowsInTextCannotStartACue(t *testing.T) {
	got := string(RenderVTT([]Cue{{0, 1000, "a --> b\nc"}}, 2000))
	if want := header + "1\n00:00:00.000 --> 00:00:01.000\na --&gt; b c\n\n"; got != want {
		t.Fatalf("vtt %q", got)
	}
}

func TestEmptyTranscriptAndSegmentsWithoutWords(t *testing.T) {
	tr := fixture("A whole sentence.")
	tr.Segments[0].Words = nil
	if got, want := string(tr.VTT()), header+"1\n00:00:01.234 --> 00:00:03.456\nA whole sentence.\n\n"; got != want {
		t.Fatalf("segment cue %q", got)
	}
	tr.Segments = nil
	if got := string(tr.VTT()); got != "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n" {
		t.Fatalf("empty transcript %q", got)
	}
}

func TestCueBreaksAtLengthSpanAndPause(t *testing.T) {
	w := func(text string, start, end int64) Word { return Word{Text: text, StartMS: start, EndMS: end} }
	long := strings.Repeat("x", 80)
	tr := &Transcript{Version: 1, Language: "en", DurationMS: 60_000, Segments: []Segment{{
		ID: "s0", Text: "t", StartMS: 0, EndMS: 20_000, Words: []Word{
			w(" "+long, 0, 500), w(" over", 600, 900), // 81+5 > 84 characters
			w(" a", 1000, 1200), w(" b", 4900, 5100), // 5100-600 > 4000 ms
			w(" c", 5200, 5300), w(" d", 6200, 6300), // a 900 ms pause
		},
	}}}
	cues := tr.Cues()
	var got []string
	for _, c := range cues {
		got = append(got, c.Text)
	}
	if want := []string{long, "over a", "b c", "d"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("cues %q, want %q", got, want)
	}
}

func TestCuesAreClippedToTheDuration(t *testing.T) {
	got := string(RenderVTT([]Cue{{29_000, 99_000, "end"}, {31_000, 32_000, "past"}}, 30_789))
	if want := header + "1\n00:00:29.000 --> 00:00:30.789\nend\n\n"; got != want {
		t.Fatalf("vtt %q", got)
	}
}

func TestMultilingualTextIsUntouched(t *testing.T) {
	texts := []string{"नमस्ते hello 42", "নমস্কার বন্ধু", "வணக்கம் உலகம்", "السلام علیکم", "你好世界"}
	tr := fixture(texts...)
	tr.DurationMS = 50_000
	if err := tr.Validate(); err != nil {
		t.Fatal(err)
	}
	vtt := string(tr.VTT())
	for _, text := range texts {
		if !strings.Contains(vtt, "\n"+text+"\n") {
			t.Errorf("%q changed in %q", text, vtt)
		}
	}
}

func TestTextJoinsSegmentsAndCollapsesSpace(t *testing.T) {
	tr := fixture("Hello   world.", " New topic. ")
	if got := tr.Text(); got != "Hello world. New topic." {
		t.Fatalf("text %q", got)
	}
}

func TestValidateRefusesWhatABadWorkerCouldSend(t *testing.T) {
	for name, mutate := range map[string]func(*Transcript){
		"version":           func(t *Transcript) { t.Version = 2 },
		"language":          func(t *Transcript) { t.Language = "English" },
		"no duration":       func(t *Transcript) { t.DurationMS = 0 },
		"model":             func(t *Transcript) { t.Model = "" },
		"segment id":        func(t *Transcript) { t.Segments[0].ID = "x0" },
		"segments reversed": func(t *Transcript) { t.Segments[0], t.Segments[1] = t.Segments[1], t.Segments[0] },
		"past the end":      func(t *Transcript) { t.Segments[1].EndMS = 30_790 },
		"zero length":       func(t *Transcript) { t.Segments[0].EndMS = t.Segments[0].StartMS },
		"nul":               func(t *Transcript) { t.Segments[0].Text = "a\x00b" },
		"newline":           func(t *Transcript) { t.Segments[0].Text = "a\nb" },
		"bad utf-8":         func(t *Transcript) { t.Segments[0].Text = "a\xffb" },
		"blank":             func(t *Transcript) { t.Segments[0].Text = "  " },
		"word id":           func(t *Transcript) { t.Segments[0].Words[0].ID = "s1w0" },
		"word backwards":    func(t *Transcript) { t.Segments[0].Words[0].EndMS = 1000 },
		"word too long":     func(t *Transcript) { t.Segments[0].Words[0].Text = strings.Repeat("a", 257) },
	} {
		tr := fixture("Hello world.", "New topic.")
		if err := tr.Validate(); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		mutate(tr)
		var inv *InvalidError
		if err := tr.Validate(); !errors.As(err, &inv) {
			t.Errorf("%s: Validate = %v, want InvalidError", name, err)
		}
	}
}

func TestSanitizeKeepsOneLine(t *testing.T) {
	if got := Sanitize("a\x00b\tc\nd\re f\x7fg\xffh"); got != "ab c d e fgh" {
		t.Fatalf("Sanitize = %q", got)
	}
}

func TestLanguageCodes(t *testing.T) {
	for in, want := range map[string]string{
		"en": "en", "Hindi": "hi", "english": "en", "haitian creole": "ht", "jw": "jv",
		"yue": "yue", "Mandarin": "zh", "klingon": "und", "": "und", "auto": "und",
	} {
		if got := LanguageCode(in); got != want {
			t.Errorf("LanguageCode(%q) = %q, want %q", in, got, want)
		}
	}
}
