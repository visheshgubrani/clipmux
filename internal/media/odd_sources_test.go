package media

// Sources the 2026-11-18 bug hunt found failing (F5–F11, F17), each made the
// way its corpus file was, through the real tools on the CPU stage.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// transcodeFile probes and transcodes path on the CPU stage, or on this
// machine's GPU chain under the sandbox with CLIPMUX_GPU_TEST=1, returning
// the manifest and the output directory.
func transcodeFile(t *testing.T, path string) (Manifest, string, error) {
	t.Helper()
	tools := testTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if os.Getenv("CLIPMUX_GPU_TEST") == "1" {
		tools.Sandbox = &Sandbox{Exec: os.Args[0]}
		tools.HW = DetectHardware(ctx, tools, t.TempDir(), os.Getenv("CLIPMUX_VAAPI_DEVICE"), "")
		tools = tools.In(dir, path)
	}
	src, err := Probe(ctx, tools, path)
	if err != nil {
		return Manifest{}, "", err
	}
	out := filepath.Join(dir, "out")
	m, err := Transcode(ctx, tools, path, src, nil, filepath.Join(dir, "work"), out, testKey, nil)
	return m, out, err
}

// playlistSeconds adds up a media playlist's segment durations.
func playlistSeconds(t *testing.T, out, rendition string) float64 {
	t.Helper()
	pl, err := os.ReadFile(filepath.Join(out, rendition, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, m := range regexp.MustCompile(`#EXTINF:([0-9.]+)`).FindAllStringSubmatch(string(pl), -1) {
		v, _ := strconv.ParseFloat(m[1], 64)
		total += v
	}
	return total
}

func mustPoster(t *testing.T, out string) {
	t.Helper()
	if fi, err := os.Stat(filepath.Join(out, "poster.jpg")); err != nil || fi.Size() == 0 {
		t.Fatalf("no poster: %v", err)
	}
}

// F5: x264's default GOP (250 frames) in MPEG-TS. Input seeking landed
// between keyframes and decoded nothing, failing a finished encode.
func TestALongGOPTransportStreamGetsAPoster(t *testing.T) {
	testTools(t)
	src := filepath.Join(t.TempDir(), "obs.ts")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=6", "-c:v", "libx264", "-pix_fmt", "yuv420p", src)
	_, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	mustPoster(t, out)
}

// F9: one frame, so the poster's seek to half the duration passed it.
func TestASingleFrameVideoGetsAPoster(t *testing.T) {
	testTools(t)
	src := filepath.Join(t.TempDir(), "one.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-frames:v", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", src)
	_, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	mustPoster(t, out)
}

// F6: loudnorm turns under 3 s of digital silence into NaN, which AAC refuses.
func TestAShortClipOfSilenceEncodes(t *testing.T) {
	testTools(t)
	src := filepath.Join(t.TempDir(), "muted.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=2", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-t", "2", src)
	m, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Audio {
		t.Fatal("the silent track should still be an audio rendition")
	}
	if s := playlistSeconds(t, out, "audio"); s < 1.9 {
		t.Fatalf("audio lasts %.2fs, want 2", s)
	}
}

// F7: a declared audio track with no packets (a recorder with a dead mic).
// The video is fine; the output is video-only.
func TestAnAudioTrackWithNoPacketsIsLeftOut(t *testing.T) {
	testTools(t)
	src := filepath.Join(t.TempDir(), "deadmic.mkv")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=4", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-af", "atrim=end_sample=0", "-c:a", "aac", "-t", "4", src)
	m, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	if m.Audio {
		t.Fatal("manifest says audio, but the track had none")
	}
	if _, err := os.Stat(filepath.Join(out, "audio")); !os.IsNotExist(err) {
		t.Fatalf("an audio rendition was written: %v", err)
	}
	if s := playlistSeconds(t, out, "360p"); s < 3.9 {
		t.Fatalf("video lasts %.2fs, want 4", s)
	}
}

// F10: an audio file's cover art is an attached picture, not a video.
func TestCoverArtIsNotAVideo(t *testing.T) {
	tools := testTools(t)
	dir := t.TempDir()
	art, m4a := filepath.Join(dir, "art.png"), filepath.Join(dir, "song.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=600x600", "-frames:v", "1", art)
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-i", art, "-map", "0:a", "-map", "1:v",
		"-c:a", "aac", "-c:v", "mjpeg", "-disposition:v", "attached_pic", m4a)
	if _, err := Probe(context.Background(), tools, m4a); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("probe = %v, want ErrUnsupportedSource", err)
	}
}

// F17: a WebM written to a pipe, as MediaRecorder does, has no duration in
// its header.
func TestARecordingWithoutADurationEncodes(t *testing.T) {
	tools := testTools(t)
	src := filepath.Join(t.TempDir(), "recorded.webm")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4", "-c:v", "libvpx", "-deadline", "realtime", "-b:v", "1M",
		"-c:a", "libopus", "-f", "webm", "pipe:1")
	var stderr strings.Builder
	cmd.Stdout, cmd.Stderr = f, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, stderr.String())
	}
	f.Close()
	s, err := Probe(context.Background(), tools, src)
	if err != nil {
		t.Fatal(err)
	}
	if s.Duration < 3.9 || s.Duration > 4.1 {
		t.Fatalf("duration = %.3f, want 4", s.Duration)
	}
	m, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	if v := playlistSeconds(t, out, "360p"); v < 3.9 || !m.Audio {
		t.Fatalf("video lasts %.2fs (want 4), audio %v", v, m.Audio)
	}
}

// F11: 3 s at 720p, then 3 s at 360p, as a stitched capture. The renditions
// stopped at the change.
func TestAResolutionChangeKeepsTheWholeVideo(t *testing.T) {
	testTools(t)
	dir := t.TempDir()
	a, b, src := filepath.Join(dir, "a.ts"), filepath.Join(dir, "b.ts"), filepath.Join(dir, "both.ts")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mpegts", a)
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30:duration=3", "-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-output_ts_offset", "3", "-f", "mpegts", b)
	first, _ := os.ReadFile(a)
	second, _ := os.ReadFile(b)
	if err := os.WriteFile(src, append(first, second...), 0o600); err != nil {
		t.Fatal(err)
	}
	m, out, err := transcodeFile(t, src)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Renditions {
		if s := playlistSeconds(t, out, r.Name); s < 5.9 {
			t.Errorf("%s lasts %.2fs, want 6", r.Name, s)
		}
	}
}
