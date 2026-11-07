package speechapi

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/visheshgubrani/clipmux/internal/outputs"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// ChunkBytes bounds one upload. OpenAI and Groq's free tier take 25 MB; the
// audio rendition is 128 kb/s, so a chunk holds about 25 minutes.
const ChunkBytes = 24 << 20

// promptRunes is how much of the previous chunk's text goes with the next.
const promptRunes = 200

// transcribeAudio sends an output's audio to the API in chunks of at most
// maxBytes, each the init segment and whole media segments, so every chunk
// is a valid fragmented MP4 that starts at a known time and no ffmpeg runs
// in serve. Chunks are built as segments are read, so memory stays near
// one chunk. The first chunk's detected language is asked of the rest, so
// a Hinglish lecture keeps one script, and each chunk gets the end of the
// previous one's text as its prompt. The answers are joined into one
// canonical transcript.
func transcribeAudio(ctx context.Context, c *Client, a *outputs.Audio, maxBytes int, onChunk func(seconds float64)) (*transcripts.Transcript, error) {
	init, err := a.Init(ctx)
	if err != nil {
		return nil, err
	}
	var results []transcripts.Chunk
	language, prompt := "", ""
	body := append([]byte{}, init...)
	startSec, spanSec := 0.0, 0.0
	send := func() error {
		res, err := withRetries(ctx, func() (transcripts.OpenAIResponse, error) {
			return c.Transcribe(ctx, Request{Audio: body, Language: language, Prompt: prompt})
		})
		if err != nil {
			return err
		}
		start, end := ms(startSec), ms(startSec+spanSec)
		if onChunk != nil {
			onChunk(spanSec)
		}
		if language == "" {
			if code := transcripts.LanguageCode(res.Language); code != "und" {
				language = code
			}
		}
		prompt = lastRunes(res.Text, promptRunes)
		results = append(results, transcripts.Chunk{StartMS: start, SpanMS: end - start, Response: res})
		startSec, spanSec = startSec+spanSec, 0
		body = append(body[:0], init...)
		return nil
	}
	for i, s := range a.Playlist.Segments {
		seg, err := a.Segment(ctx, i)
		if err != nil {
			return nil, err
		}
		if len(body) > len(init) && len(body)+len(seg) > maxBytes {
			if err := send(); err != nil {
				return nil, err
			}
		}
		body = append(body, seg...)
		spanSec += s.Duration
	}
	if len(body) > len(init) {
		if err := send(); err != nil {
			return nil, err
		}
	}
	return transcripts.FromOpenAI(results, a.DurationMS, c.Model+"@"+c.Host())
}

func ms(sec float64) int64 { return int64(math.Floor(sec*1000 + 0.5)) }

// Retries of one chunk: 4 tries, waiting 2, 4 and 8 s, or what Retry-After
// asks up to a minute.
var retryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

const maxRetryAfter = time.Minute

func withRetries(ctx context.Context, call func() (transcripts.OpenAIResponse, error)) (transcripts.OpenAIResponse, error) {
	for try := 0; ; try++ {
		res, err := call()
		var apiErr *Error
		if err == nil || !errors.As(err, &apiErr) || !apiErr.Transient || try == len(retryDelays) {
			return res, err
		}
		wait := retryDelays[try]
		if apiErr.RetryAfter > 0 {
			wait = min(apiErr.RetryAfter, maxRetryAfter)
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
