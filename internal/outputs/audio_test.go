package outputs

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// slowStore serves objects from memory after a delay, like a round trip to
// object storage, and records how many reads overlap.
type slowStore struct {
	storage.Storage // only Get is used
	objects         map[string][]byte
	delay           time.Duration
	fail            string // a key whose read fails
	inFlight, peak  atomic.Int32
}

func (s *slowStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	n := s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	for {
		p := s.peak.Load()
		if n <= p || s.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if key == s.fail {
		return nil, errors.New("storage unavailable")
	}
	b, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: not found", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// encrypt is Shaka's HLS AES-128: CBC with PKCS#7 padding.
func encrypt(t *testing.T, key, iv [16]byte, plain []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(buf, buf)
	return buf
}

// audioOf builds an n-segment rendition in a slow store and the bytes its
// stream must equal: the init segment, then each segment in order.
func audioOf(t *testing.T, n int, delay time.Duration) (*Audio, *slowStore, []byte) {
	t.Helper()
	key := [16]byte{1, 2, 3}
	st := &slowStore{objects: map[string][]byte{}, delay: delay}
	dir := "media/ast_1/out_1/audio/"
	want := []byte("init")
	st.objects[dir+"init.mp4"] = []byte("init")
	a := &Audio{store: st, dir: dir, key: key, Playlist: media.MediaPlaylist{Init: "init.mp4"}}
	for i := range n {
		uri := fmt.Sprintf("seg_%d.m4s", i+1)
		iv := [16]byte{15: byte(i + 1)}
		plain := []byte(strings.Repeat(fmt.Sprintf("segment %d;", i+1), 20))
		st.objects[dir+uri] = encrypt(t, key, iv, plain)
		a.Playlist.Segments = append(a.Playlist.Segments, media.PlaylistSegment{URI: uri, IV: iv, Duration: 6})
		want = append(want, plain...)
	}
	return a, st, want
}

func TestAudioStreamReadsSegmentsAheadAndWritesThemInOrder(t *testing.T) {
	// 64 segments at 50 ms each: 3.2 s one at a time, about 0.25 s with 16
	// in flight.
	a, st, want := audioOf(t, 64, 50*time.Millisecond)
	var got bytes.Buffer
	start := time.Now()
	n, err := a.Stream(context.Background(), &got)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) || n != int64(len(want)) {
		t.Fatalf("stream is %d bytes, not the %d of the rendition in order", n, len(want))
	}
	if took > time.Second {
		t.Errorf("took %v: segments weren't read ahead", took)
	}
	if p := st.peak.Load(); p < 8 || p > audioFetchWindow+1 {
		t.Errorf("%d reads overlapped; want about %d", p, audioFetchWindow)
	}
}

func TestAudioStreamStopsAtAFailingSegmentAndNamesIt(t *testing.T) {
	a, st, _ := audioOf(t, 40, 10*time.Millisecond)
	st.fail = a.dir + "seg_7.m4s"
	_, err := a.Stream(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "segment 7:") {
		t.Fatalf("err = %v, want segment 7's", err)
	}
}

func TestAudioStreamFailsWhenCancelledRatherThanEndShort(t *testing.T) {
	a, _, _ := audioOf(t, 200, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := a.Stream(ctx, io.Discard); err == nil {
		t.Fatal("a cancelled stream ended without an error")
	}
}
