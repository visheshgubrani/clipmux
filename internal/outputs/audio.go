package outputs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"

	"github.com/visheshgubrani/clipmux/internal/contentkey"
	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/storage"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// A transcription reads an output's audio rendition, not the source
// (P5 "Transcription on workers"): it works after the source is deleted,
// never pins a job to an offline PC, and moves about 1 MB a minute. serve
// decrypts it, so a worker never needs a content key for it.

// ErrNoAudio means the output has no audio rendition.
var ErrNoAudio = errors.New("outputs: the output has no audio")

// maxSegment bounds one audio segment read into memory: six seconds of
// 128 kb/s AAC is about 100 KB.
const maxSegment = 8 << 20

// Audio is an output's audio rendition: its playlist, and the key to
// decrypt its segments.
type Audio struct {
	store    storage.Storage
	dir      string // media/{asset}/{output}/audio/
	key      [16]byte
	Playlist media.MediaPlaylist
	// DurationMS is the output's duration, from its manifest.
	DurationMS int64
}

// OpenAudio reads an output's audio playlist from its store.
func OpenAudio(ctx context.Context, store storage.Storage, keys contentkey.Ring, out db.Output) (*Audio, error) {
	var m struct {
		Audio    bool    `json:"audio"`
		Duration float64 `json:"duration_seconds"`
	}
	if err := json.Unmarshal(out.Manifest, &m); err != nil {
		return nil, fmt.Errorf("output %s manifest: %w", out.ID, err)
	}
	if !m.Audio {
		return nil, ErrNoAudio
	}
	secret, ok := keys.Get(out.KeyVersion)
	if !ok {
		return nil, fmt.Errorf("no content key secret version %d", out.KeyVersion)
	}
	dir := out.Prefix.String + "audio/"
	body, err := readAll(ctx, store, dir+"index.m3u8", 1<<20)
	if err != nil {
		return nil, fmt.Errorf("audio playlist: %w", err)
	}
	pl, err := media.ParseMediaPlaylist(string(body))
	if err != nil {
		return nil, fmt.Errorf("audio playlist: %w", err)
	}
	ms, err := transcripts.Milliseconds(m.Duration)
	if err != nil {
		return nil, err
	}
	return &Audio{store: store, dir: dir, key: secret.Derive(out.AssetID, out.ID), Playlist: pl, DurationMS: ms}, nil
}

// Init reads the init segment, which is stored in the clear (Shaka issue
// 1616's workaround).
func (a *Audio) Init(ctx context.Context) ([]byte, error) {
	return readAll(ctx, a.store, a.object(a.Playlist.Init), maxSegment)
}

// Segment reads and decrypts segment i.
func (a *Audio) Segment(ctx context.Context, i int) ([]byte, error) {
	s := a.Playlist.Segments[i]
	enc, err := readAll(ctx, a.store, a.object(s.URI), maxSegment)
	if err != nil {
		return nil, err
	}
	return media.DecryptSegment(a.key, s.IV, enc)
}

// audioFetchWindow is how many segments Stream reads ahead of the one it
// is writing. Each read is a round trip to object storage (about 0.7 s
// from the VPS to R2), and an hour of audio is 600 of them: one at a
// time, a 76-minute transcription waited almost nine minutes for its
// audio (D132). The window bounds memory to this many segments.
const audioFetchWindow = 16

// Stream writes the whole rendition as one fragmented MP4: the init
// segment, then every segment decrypted, in playlist order. Up to
// audioFetchWindow segments are read and decrypted at once. It returns the
// bytes written; the first failure stops the rest.
func (a *Audio) Stream(ctx context.Context, w io.Writer) (int64, error) {
	init, err := a.Init(ctx)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(init)
	total := int64(n)
	if err != nil {
		return total, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		seg []byte
		err error
	}
	// Each segment has its own buffered channel, queued in order: the
	// fetcher hands them out up to the window ahead, the writer drains
	// them in turn.
	pending := make(chan chan result, audioFetchWindow)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		defer close(pending)
		for i := range a.Playlist.Segments {
			ch := make(chan result, 1)
			select {
			case pending <- ch:
			case <-ctx.Done():
				return
			}
			wg.Go(func() {
				seg, err := a.Segment(ctx, i)
				if err != nil {
					err = fmt.Errorf("segment %d: %w", i+1, err)
				}
				ch <- result{seg, err}
			})
		}
	})
	for ch := range pending {
		r := <-ch
		if r.err != nil {
			cancel()
			return total, r.err
		}
		n, err := w.Write(r.seg)
		total += int64(n)
		if err != nil {
			cancel()
			return total, err
		}
	}
	return total, ctx.Err()
}

// object is a playlist URI's storage key; the playlist checks at commit
// keep URIs relative and inside the rendition.
func (a *Audio) object(uri string) string { return path.Join(a.dir, uri) }

func readAll(ctx context.Context, store storage.Storage, key string, limit int64) ([]byte, error) {
	rc, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, limit)
	}
	return b, nil
}
