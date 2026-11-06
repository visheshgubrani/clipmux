package media

// Golden test with the real tools: ffmpeg and Shaka Packager encode,
// encrypt and package a generated clip, and the test decrypts what came out.
// Ported from clipmux-lms transcoding/tests/test_packaging.py.
//
// It skips when the tools are missing, unless CLIPMUX_REQUIRE_MEDIA_TOOLS=1
// (set in CI), which turns a missing tool into a failure.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTools returns the media tools for tests, or skips.
func testTools(t *testing.T) Tools {
	t.Helper()
	tools := DefaultTools()
	if p := os.Getenv("CLIPMUX_PACKAGER"); p != "" {
		tools.Packager = p
	}
	for _, bin := range []string{tools.FFmpeg, tools.FFprobe, tools.Packager} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
				t.Fatalf("%s is required (CLIPMUX_REQUIRE_MEDIA_TOOLS=1): %v", bin, err)
			}
			t.Skipf("%s not found; skipping media test", bin)
		}
	}
	return tools
}

func ffmpeg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("ffmpeg", append([]string{"-v", "error", "-y"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// decrypt reverses Shaka's AES-128: CBC over the whole segment, PKCS#7.
func decrypt(t *testing.T, playlist string, key [16]byte, segment []byte) []byte {
	t.Helper()
	m := regexp.MustCompile(`IV=0x([0-9a-fA-F]{32})`).FindStringSubmatch(playlist)
	if m == nil {
		t.Fatal("playlist has no IV")
	}
	iv, _ := hex.DecodeString(m[1])
	if len(segment)%aes.BlockSize != 0 {
		t.Fatalf("segment of %d bytes is not whole AES blocks: not encrypted", len(segment))
	}
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, len(segment))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, segment)
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize {
		t.Fatal("bad padding: wrong key or not AES-128")
	}
	return plain[:len(plain)-pad]
}

// probeStream returns the codec of the single stream in init+segment.
func probeStream(t *testing.T, init, segment []byte) (codec string, packets int) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "joined.mp4")
	if err := os.WriteFile(f, append(append([]byte{}, init...), segment...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-count_packets", "-show_entries",
		"stream=codec_name,nb_read_packets", "-of", "json", f).Output()
	if err != nil {
		t.Fatalf("ffprobe of decrypted segment: %v", err)
	}
	var p struct {
		Streams []struct {
			CodecName     string `json:"codec_name"`
			NbReadPackets string `json:"nb_read_packets"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &p); err != nil || len(p.Streams) != 1 {
		t.Fatalf("ffprobe: %s", out)
	}
	n, _ := strconv.Atoi(p.Streams[0].NbReadPackets)
	return p.Streams[0].CodecName, n
}

func TestTranscodeEncryptsEveryRenditionAndDecryptsBack(t *testing.T) {
	transcodeAndCheck(t, false, Hardware{})
}

// The same pipeline with every tool confined as the built-in worker runs it:
// no network, writes only in the attempt directory, limits, idle priority.
func TestGoldenTranscodePassesUnderTheSandbox(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox is Linux only")
	}
	transcodeAndCheck(t, true, Hardware{})
}

// testKey is the content key the golden tests encrypt with.
var testKey = Key{Version: 1, Key: [16]byte{0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe, 1, 2, 3, 4, 5, 6, 7, 8}, KeyID: [16]byte{0xaa}}

// transcodeAndCheck encodes a generated clip on hw (the CPU for the zero
// value), checks the output, and returns its manifest and directory.
func transcodeAndCheck(t *testing.T, sandboxed bool, hw Hardware) (Manifest, string) {
	tools := testTools(t)
	tools.HW = hw
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if sandboxed {
		tools.Sandbox = &Sandbox{Exec: os.Args[0]}
		tools = tools.In(dir)
	}
	source := filepath.Join(dir, "source.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "testsrc2=size=854x480:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100",
		"-t", "13", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", source)

	src, err := Probe(ctx, tools, source)
	if err != nil {
		t.Fatal(err)
	}
	if src.Width != 854 || src.Height != 480 || !src.HasAudio || src.FPS != 25 {
		t.Fatalf("probe = %+v", src)
	}

	key := testKey
	work, out := filepath.Join(dir, "work"), filepath.Join(dir, "out")
	var phases []string
	m, err := Transcode(ctx, tools, source, src, work, out, key, func(phase string, _ float64) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(phases, ",") != "transcode,package" {
		t.Errorf("phases = %v", phases)
	}

	// The output holds only grammar names, and never a key.
	if names, _ := Inventory(out); len(names) == 0 {
		t.Fatal("empty output")
	}
	_ = filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(out, p)
			if !ValidOutputPath(filepath.ToSlash(rel)) {
				t.Errorf("output contains %q, outside the grammar", rel)
			}
			if data, _ := os.ReadFile(p); bytes.Contains(data, key.Key[:]) {
				t.Errorf("%s contains the raw content key", rel)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(work, "encryption.flags")); !os.IsNotExist(err) {
		t.Error("the flag file holding the key must be removed after packaging")
	}

	if len(m.Renditions) != 2 || m.Renditions[0].Name != "480p" || m.Renditions[1].Width != 640 || !m.Audio || m.KeyVersion != 1 {
		t.Fatalf("manifest = %+v", m)
	}

	master, _ := os.ReadFile(filepath.Join(out, "master.m3u8"))
	for _, want := range []string{"480p/index.m3u8", "360p/index.m3u8", "audio/index.m3u8", "TYPE=AUDIO"} {
		if !bytes.Contains(master, []byte(want)) {
			t.Errorf("master.m3u8 lacks %q:\n%s", want, master)
		}
	}

	for dir, codec := range map[string]string{"480p": "h264", "360p": "h264", "audio": "aac"} {
		pl, _ := os.ReadFile(filepath.Join(out, dir, "index.m3u8"))
		if _, err := CheckMediaPlaylist(string(pl), 1); err != nil {
			t.Fatalf("%s: %v\n%s", dir, err, pl)
		}
		durations := regexp.MustCompile(`#EXTINF:([0-9.]+)`).FindAllStringSubmatch(string(pl), -1)
		if len(durations) != 3 {
			t.Errorf("%s: %d segments, want 3 for 13 s at 6 s", dir, len(durations))
		}
		for i, d := range durations[:len(durations)-1] {
			if v, _ := strconv.ParseFloat(d[1], 64); v < 5.9 || v > 6.1 {
				t.Errorf("%s segment %d lasts %.3fs, want 6 (aligned keyframes)", dir, i+1, v)
			}
		}
		init, _ := os.ReadFile(filepath.Join(out, dir, "init.mp4"))
		seg, _ := os.ReadFile(filepath.Join(out, dir, "seg_00001.m4s"))
		if bytes.Contains(seg[:min(64, len(seg))], []byte("moof")) {
			t.Fatalf("%s segment is stored in the clear", dir)
		}
		plain := decrypt(t, string(pl), key.Key, seg)
		got, packets := probeStream(t, init, plain)
		if got != codec || packets == 0 {
			t.Errorf("%s: decrypted segment is %q with %d packets, want %s", dir, got, packets, codec)
		}
	}

	var stored Manifest
	raw, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Files) != len(m.Files) {
		t.Fatalf("manifest.json: %v", err)
	}
	if stored.Encoder == nil || stored.Encoder.Stage != hw.Chain(src)[0].Name() {
		t.Fatalf("manifest encoder = %+v, want %s", stored.Encoder, hw.Chain(src)[0].Name())
	}
	return m, out
}

func TestProbeRefusesNonVideoAndDisguisedPlaylists(t *testing.T) {
	tools := testTools(t)
	ctx := context.Background()
	dir := t.TempDir()

	audioOnly := filepath.Join(dir, "a.m4a")
	ffmpeg(t, "-f", "lavfi", "-i", "sine=frequency=440", "-t", "2", "-c:a", "aac", audioOnly)
	// An HLS playlist renamed to .mp4: the classic local-file-read vector.
	disguised := filepath.Join(dir, "evil.mp4")
	if err := os.WriteFile(disguised, []byte("#EXTM3U\n#EXTINF:1,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "x.mp4")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte{0xde, 0xad}, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{audioOnly, disguised, garbage} {
		if _, err := Probe(ctx, tools, p); err == nil {
			t.Errorf("Probe(%s) accepted it", filepath.Base(p))
		}
	}
}
