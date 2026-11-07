package transcripts

import (
	"math"
	"strings"
	"testing"
)

// whisperJSON builds whisper.cpp full JSON by hand, with token text spliced
// in as raw bytes so a token can end in the middle of a character, as
// whisper.cpp writes them.
func whisperJSON(lang string, segs ...string) []byte {
	return []byte(`{"systeminfo":"x","model":{"type":"large"},"params":{"language":"auto"},"result":{"language":"` + lang + `"},"transcription":[` + strings.Join(segs, ",") + `]}`)
}

func seg(from, to int, text string, toks ...string) string {
	return `{"timestamps":{"from":"x","to":"y"},"offsets":{"from":` + itoa3(from) + `,"to":` + itoa3(to) + `},"text":"` + text + `","tokens":[` + strings.Join(toks, ",") + `]}`
}

func tok(from, to int, text string) string {
	return `{"text":"` + text + `","timestamps":{"from":"x","to":"y"},"offsets":{"from":` + itoa3(from) + `,"to":` + itoa3(to) + `},"id":1,"p":0.9,"t_dlt":0}`
}

func itoa3(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func TestWhisperCPPBuildsWordsFromTokens(t *testing.T) {
	raw := whisperJSON("en",
		seg(0, 2000, ` Hello, world.`, tok(0, 0, "[_BEG_]"), tok(0, 400, " Hello"), tok(400, 500, ","), tok(600, 1500, " wor"), tok(1500, 1900, "ld."), tok(2000, 2000, "[_TT_100]")),
		seg(2000, 3000, ` [BLANK_AUDIO]`, tok(2000, 3000, " [BLANK_AUDIO]")),
		seg(3000, 5000, ` Say \"hi\" \\ bye`, tok(3000, 3500, " Say"), tok(3500, 4000, ` \"hi\"`), tok(4000, 4200, ` \\`), tok(4200, 4900, " bye")),
		seg(9000, 12000, ` Past the end.`, tok(9000, 9500, " Past"), tok(9500, 11800, " the end.")),
	)
	got, err := FromWhisperCPP(raw, 10_500, "large-v3-turbo-q5_0@sha256:ab")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("converted transcript must validate: %v", err)
	}
	if got.Language != "en" || got.DurationMS != 10_500 || got.Model != "large-v3-turbo-q5_0@sha256:ab" {
		t.Fatalf("header %+v", got)
	}
	var ids, texts []string
	for _, s := range got.Segments {
		ids = append(ids, s.ID)
		texts = append(texts, s.Text)
	}
	// s1 was only [BLANK_AUDIO], so the IDs keep whisper's index with a gap.
	if strings.Join(ids, ",") != "s0,s2,s3" || texts[1] != `Say "hi" \ bye` {
		t.Fatalf("segments %v %q", ids, texts)
	}
	w := got.Segments[0].Words
	if len(w) != 2 || w[0] != (Word{ID: "s0w0", Text: " Hello,", StartMS: 0, EndMS: 500}) || w[1] != (Word{ID: "s0w1", Text: " world.", StartMS: 600, EndMS: 1900}) {
		t.Fatalf("words %+v", w)
	}
	last := got.Segments[2]
	if last.StartMS != 9000 || last.EndMS != 10_500 || last.Words[1].EndMS != 10_500 {
		t.Fatalf("clipping %+v", last)
	}
}

func TestWhisperCPPJoinsCharactersSplitAcrossTokens(t *testing.T) {
	// "नमस्ते" is 18 bytes; whisper can split its tokens mid-character.
	nm := "नमस्ते"
	hindi := seg(0, 1000, " "+nm+" दोस्त", tok(0, 300, " "+nm[:4]), tok(300, 600, nm[4:]), tok(600, 1000, " दोस्त"))
	// Han: every character stands as a word, even without spaces.
	han := seg(1000, 2000, " 你好世界", tok(1000, 1200, " 你"), tok(1200, 1400, "好"), tok(1400, 1700, "世界"))
	got, err := FromWhisperCPP(whisperJSON("hi", hindi, han), 5000, "m@sha256:00")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if w := got.Segments[0].Words; len(w) != 2 || w[0].Text != " "+nm || w[0].StartMS != 0 || w[0].EndMS != 600 {
		t.Fatalf("hindi words %+v", w)
	}
	var han3 []string
	for _, w := range got.Segments[1].Words {
		han3 = append(han3, w.Text)
	}
	if strings.Join(han3, "|") != " 你|好|世界" {
		t.Fatalf("han words %q", han3)
	}
	if got.Language != "hi" || got.Segments[0].Text != nm+" दोस्त" {
		t.Fatalf("language %q text %q", got.Language, got.Segments[0].Text)
	}
}

func TestWhisperCPPRefusesWhatIsNotItsJSON(t *testing.T) {
	for _, raw := range []string{`not json`, `{"transcription":[{"text":5}]}`} {
		if _, err := FromWhisperCPP([]byte(raw), 1000, "m"); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}

func TestOpenAIChunksAreShiftedByTheirStart(t *testing.T) {
	first := OpenAIResponse{
		Language: "english",
		Segments: []OpenAISegment{{Start: 0.5, End: 2.25, Text: " Hello, world."}, {Start: 3, End: 4, Text: " 你好"}},
		Words:    []OpenAIWord{{"Hello", 0.5, 1.0}, {"world", 1.2, 2.25}, {"你", 3, 3.4}, {"好", 3.4, 4}},
	}
	second := OpenAIResponse{
		Language: "english",
		// The API ran past the chunk: 1500.5 s of a 1500 s span is clipped.
		Segments: []OpenAISegment{{Start: 59.9996, End: 1500.5, Text: " Back again."}},
		Words:    []OpenAIWord{{"Back", 59.9996, 61}, {"again", 61, 1500.5}},
	}
	got, err := FromOpenAI([]Chunk{
		{StartMS: 0, SpanMS: 1_500_000, Response: first},
		{StartMS: 1_500_000, SpanMS: 1_500_000, Response: second},
	}, 3_000_000, "whisper-large-v3-turbo@api.groq.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	if got.Language != "en" || len(got.Segments) != 3 {
		t.Fatalf("got %+v", got)
	}
	s0 := got.Segments[0]
	if s0.ID != "s0" || s0.StartMS != 500 || s0.EndMS != 2250 || len(s0.Words) != 2 || s0.Words[0].Text != " Hello," || s0.Words[1].Text != " world." {
		t.Fatalf("first segment %+v", s0)
	}
	// "你好" doesn't split into two words by spaces, so it is one cue.
	if s1 := got.Segments[1]; s1.Words != nil || s1.Text != "你好" {
		t.Fatalf("second segment %+v", s1)
	}
	s2 := got.Segments[2]
	if s2.ID != "s2" || s2.StartMS != 1_560_000 || s2.EndMS != 3_000_000 || s2.Words[0].StartMS != 1_560_000 || s2.Words[1].Text != " again." {
		t.Fatalf("shifted segment %+v", s2)
	}
	if !strings.Contains(string(got.VTT()), "00:26:00.000 --> ") {
		t.Fatalf("vtt %s", got.VTT())
	}
}

func TestOpenAIWithoutChunksOrLanguage(t *testing.T) {
	got, err := FromOpenAI(nil, 1000, "m@h")
	if err != nil || got.Language != "und" || len(got.Segments) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := FromOpenAI([]Chunk{{SpanMS: 1000, Response: OpenAIResponse{Segments: []OpenAISegment{{Start: 0, End: 1, Text: "x"}}, Words: []OpenAIWord{{"x", 0, math.Inf(1)}}}}}, 1000, "m"); err == nil {
		t.Fatal("an infinite time must be refused")
	}
}
