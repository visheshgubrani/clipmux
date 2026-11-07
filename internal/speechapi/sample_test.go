package speechapi

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/media"
)

var makeSample = flag.Bool("make-sample", false, "regenerate sample.m4a through the media pipeline")

// The embedded sample is one audio chunk as the runner sends it: the
// pipeline's AAC rendition, decrypted, init then segments. -make-sample
// rebuilds it from media/testdata/speech/jfk.ogg with the real ffmpeg and
// Shaka Packager.
func TestSampleIsAnAudioChunk(t *testing.T) {
	if *makeSample {
		regenerateSample(t)
	}
	if len(sample) == 0 || !bytes.Contains(sample[:64], []byte("ftyp")) || !bytes.Contains(sample, []byte("moof")) {
		t.Fatal("sample.m4a isn't a fragmented MP4")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	f := filepath.Join(t.TempDir(), "sample.m4a")
	if err := os.WriteFile(f, sample, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_name:format=duration", "-of", "default=noprint_wrappers=1", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "codec_name=aac") {
		t.Fatalf("ffprobe: %s", out)
	}
}

func regenerateSample(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tools := media.DefaultTools()
	if p := os.Getenv("CLIPMUX_PACKAGER"); p != "" {
		tools.Packager = p
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=black:size=320x180:rate=25",
		"-i", filepath.Join("..", "media", "testdata", "speech", "jfk.ogg"), "-t", "11", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest", src).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	probe, err := media.Probe(ctx, tools, src)
	if err != nil {
		t.Fatal(err)
	}
	key := media.Key{Version: 1, Key: [16]byte{1}, KeyID: [16]byte{2}}
	out := filepath.Join(dir, "out")
	if _, err := media.Transcode(ctx, tools, src, probe, filepath.Join(dir, "work"), out, key, func(string, float64) {}); err != nil {
		t.Fatal(err)
	}
	playlist, err := os.ReadFile(filepath.Join(out, "audio", "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	pl, err := media.ParseMediaPlaylist(string(playlist))
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := os.ReadFile(filepath.Join(out, "audio", pl.Init))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range pl.Segments {
		enc, err := os.ReadFile(filepath.Join(out, "audio", s.URI))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := media.DecryptSegment(key.Key, s.IV, enc)
		if err != nil {
			t.Fatal(err)
		}
		chunk = append(chunk, plain...)
	}
	if err := os.WriteFile("sample.m4a", chunk, 0o644); err != nil {
		t.Fatal(err)
	}
	sample = chunk
	t.Logf("wrote sample.m4a, %s bytes", strconv.Itoa(len(chunk)))
}
