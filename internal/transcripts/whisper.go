package transcripts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// whisperFull is the part of whisper.cpp's full JSON (`whisper-cli -ojf`)
// we read. Offsets are milliseconds. Token text is kept raw, because a
// token can hold part of a multi-byte character, and decoding each token
// on its own would turn Devanagari or CJK into replacement characters.
type whisperFull struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets whisperOffsets  `json:"offsets"`
		Text    json.RawMessage `json:"text"`
		Tokens  []struct {
			Text    json.RawMessage `json:"text"`
			Offsets whisperOffsets  `json:"offsets"`
		} `json:"tokens"`
	} `json:"transcription"`
}

type whisperOffsets struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// FromWhisperCPP converts whisper.cpp's full JSON into the canonical
// transcript, clipped to the duration. Words are built from tokens: a token
// that starts with a space starts a word, and in scripts written without
// spaces (Han, kana, Thai…) every whole character sequence is a word.
// Special tokens ("[_BEG_]", "[_TT_150]") are skipped.
func FromWhisperCPP(raw []byte, durationMS int64, model string) (*Transcript, error) {
	var w whisperFull
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, invalid("whisper json: %v", err)
	}
	b := newBuilder(LanguageCode(w.Result.Language), durationMS, model)
	for i, seg := range w.Transcription {
		text, err := rawString(seg.Text)
		if err != nil {
			return nil, invalid("segment %d text: %v", i, err)
		}
		var words []Word
		var cur []byte
		var curStart, curEnd int64
		flush := func() {
			if len(cur) > 0 {
				words = append(words, Word{Text: string(cur), StartMS: curStart, EndMS: curEnd})
			}
			cur = nil
		}
		for _, tok := range seg.Tokens {
			t, err := rawString(tok.Text)
			if err != nil {
				return nil, invalid("segment %d token: %v", i, err)
			}
			if special(t) || len(t) == 0 {
				continue
			}
			if len(cur) == 0 || t[0] == ' ' || (utf8.Valid(cur) && (noSpaceScript(lastRune(cur)) || noSpaceScript(firstRune(t)))) {
				flush()
				curStart = tok.Offsets.From
			}
			cur = append(cur, t...)
			curEnd = max(tok.Offsets.To, curStart)
		}
		flush()
		b.add(i, string(text), seg.Offsets.From, seg.Offsets.To, words)
	}
	return &b.t, nil
}

// special reports a whisper.cpp control token, written as "[_NAME_]" or
// "[_TT_n]".
func special(t []byte) bool {
	return len(t) >= 4 && bytes.HasPrefix(t, []byte("[_")) && t[len(t)-1] == ']'
}

func lastRune(b []byte) rune  { r, _ := utf8.DecodeLastRune(b); return r }
func firstRune(b []byte) rune { r, _ := utf8.DecodeRune(b); return r }

// noSpaceScript reports a character from a script written without spaces
// between words, where each recognised token stands as a word.
func noSpaceScript(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai,
		unicode.Lao, unicode.Khmer, unicode.Myanmar, unicode.Tibetan)
}

// rawString decodes a JSON string without validating UTF-8, so the bytes of
// a split character survive until its tokens are joined. whisper.cpp
// escapes only quotes and backslashes; the other JSON escapes are handled
// for any writer that uses them.
func rawString(m json.RawMessage) ([]byte, error) {
	if len(m) < 2 || m[0] != '"' || m[len(m)-1] != '"' {
		return nil, errors.New("not a string")
	}
	in := m[1 : len(m)-1]
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(in) {
			return nil, errors.New("trailing backslash")
		}
		switch in[i] {
		case '"', '\\', '/':
			out = append(out, in[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, n, err := unicodeEscape(in[i+1:])
			if err != nil {
				return nil, err
			}
			out = utf8.AppendRune(out, r)
			i += n
		default:
			return nil, fmt.Errorf("escape \\%c", in[i])
		}
	}
	return out, nil
}

// unicodeEscape reads the hex after a \u, joining a surrogate pair. It
// returns the rune and how many bytes it consumed.
func unicodeEscape(b []byte) (rune, int, error) {
	hex := func(b []byte) (rune, error) {
		if len(b) < 4 {
			return 0, errors.New("short \\u escape")
		}
		v, err := strconv.ParseUint(string(b[:4]), 16, 16)
		return rune(v), err
	}
	r, err := hex(b)
	if err != nil {
		return 0, 0, err
	}
	if utf16.IsSurrogate(r) && len(b) >= 10 && b[4] == '\\' && b[5] == 'u' {
		if r2, err := hex(b[6:]); err == nil {
			if pair := utf16.DecodeRune(r, r2); pair != utf8.RuneError {
				return pair, 10, nil
			}
		}
	}
	return r, 4, nil
}
