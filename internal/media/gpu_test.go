package media

// Real GPU encodes, for the self-hosted GPU runner and Modal (P3-T03). They
// run only with CLIPMUX_GPU_TEST=1, and then a GPU must be present.

import (
	"bytes"
	"context"
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

// minSSIM is how close a GPU encode must stay to the CPU's at the same
// bitrate, on the first 480p segment of the golden clip.
const minSSIM = 0.95

func TestGPUBackendsEncodeAtParity(t *testing.T) {
	if os.Getenv("CLIPMUX_GPU_TEST") != "1" {
		t.Skip("set CLIPMUX_GPU_TEST=1 on a machine with a GPU")
	}
	if runtime.GOOS != "linux" {
		t.Skip("GPU encoding runs under the Linux sandbox")
	}
	tools := testTools(t)
	tools.Sandbox = &Sandbox{Exec: os.Args[0]}
	hw := DetectHardware(context.Background(), tools, t.TempDir(), os.Getenv("CLIPMUX_VAAPI_DEVICE"), "")
	t.Logf("detected: %s", hw.Describe())
	backends := hw.Backends()
	if len(backends) == 1 {
		t.Fatal("CLIPMUX_GPU_TEST is set but no GPU passed its test encode")
	}

	_, cpuOut := transcodeAndCheck(t, true, Hardware{})
	// A 10-bit source can't decode on these GPUs, so it takes each one's
	// hybrid stage: CPU decode, one upload, every rung scaled on the GPU.
	_, cpuTenBit := transcodeClip(t, true, Hardware{}, "yuv420p10le")
	for _, b := range backends[:len(backends)-1] {
		t.Run(string(b), func(t *testing.T) {
			only := Hardware{Prefer: CPU}
			switch b {
			case NVENC:
				only = Hardware{NVENC: true, CUDA: true}
			case VAAPI:
				only = Hardware{VAAPIDevice: hw.VAAPIDevice}
			case CUDADecode:
				only = Hardware{CUDA: true}
			}
			_, out := transcodeAndCheck(t, true, only)
			if s := segmentSSIM(t, out, cpuOut, "480p"); s < minSSIM {
				t.Errorf("SSIM against the CPU encode = %.3f, want at least %.2f", s, minSSIM)
			} else {
				t.Logf("SSIM against the CPU encode: %.3f", s)
			}
			if b == CUDADecode {
				return // its only stage decodes on the GPU or falls back by itself
			}
			_, hybrid := transcodeClip(t, true, only, "yuv420p10le")
			if s := segmentSSIM(t, hybrid, cpuTenBit, "480p"); s < minSSIM {
				t.Errorf("%s-hybrid: SSIM against the CPU encode = %.3f, want at least %.2f", b, s, minSSIM)
			} else {
				t.Logf("%s-hybrid: SSIM against the CPU encode: %.3f", b, s)
			}
		})
	}
}

// A 4K source keeps the whole ladder, 2160p down, on its GPU stage,
// decode included: a 4K AV1 upload once fell back to nvenc-hybrid, CPU
// decode and all (D129). Each source logs its timings and any fallback.
func TestGPU4KLadderStaysOnTheGPU(t *testing.T) {
	if os.Getenv("CLIPMUX_GPU_TEST") != "1" {
		t.Skip("set CLIPMUX_GPU_TEST=1 on a machine with a GPU")
	}
	tools := testTools(t)
	tools.Sandbox = &Sandbox{Exec: os.Args[0]}
	hw := DetectHardware(context.Background(), tools, t.TempDir(), os.Getenv("CLIPMUX_VAAPI_DEVICE"), "")
	if !hw.NVENC && hw.VAAPIDevice == "" {
		t.Skip("no GPU encoder passed its test encode")
	}
	t.Logf("detected: %s", hw.Describe())
	for _, enc := range [][]string{
		{"h264.mp4", "-c:v", "libx264", "-preset", "ultrafast"},
		{"av1.webm", "-c:v", "libaom-av1", "-usage", "realtime", "-cpu-used", "8", "-b:v", "8M"},
	} {
		t.Run(enc[0], func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, enc[0])
			ffmpeg(t, append(append([]string{"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=30", "-t", "2"}, enc[1:]...), "-pix_fmt", "yuv420p", source)...)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			run := tools.In(dir)
			run.HW = hw
			src, err := Probe(ctx, run, source)
			if err != nil {
				t.Fatal(err)
			}
			m, err := Transcode(ctx, run, source, src, nil, filepath.Join(dir, "work"), filepath.Join(dir, "out"), testKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: stage %s, took %+v, fallbacks %v", src.Codec, m.Encoder.Stage, m.Took, m.Encoder.Failed)
			if want := hw.Chain(src)[0].Name(); m.Encoder.Stage != want {
				t.Errorf("encoded on %s, want %s; it fell back: %v", m.Encoder.Stage, want, m.Encoder.Failed)
			}
			var names []string
			for _, r := range m.Renditions {
				names = append(names, r.Name)
			}
			if got := strings.Join(names, ","); got != "2160p,1440p,1080p,720p,480p,360p" {
				t.Errorf("renditions %s", got)
			}
		})
	}
}

// segmentSSIM decrypts the first segment of a rendition in two outputs and
// compares them frame by frame.
func segmentSSIM(t *testing.T, a, b, rendition string) float64 {
	t.Helper()
	dir := t.TempDir()
	clear := func(out, name string) string {
		pl, _ := os.ReadFile(filepath.Join(out, rendition, "index.m3u8"))
		init, _ := os.ReadFile(filepath.Join(out, rendition, "init.mp4"))
		seg, _ := os.ReadFile(filepath.Join(out, rendition, "seg_00001.m4s"))
		p := filepath.Join(dir, name+".mp4")
		if err := os.WriteFile(p, append(init, decrypt(t, string(pl), testKey.Key, seg)...), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-i", clear(a, "a"), "-i", clear(b, "b"), "-lavfi", "ssim", "-f", "null", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ssim: %v\n%s", err, stderr.String())
	}
	m := regexp.MustCompile(`All:([0-9.]+)`).FindSubmatch(stderr.Bytes())
	if m == nil {
		t.Fatalf("no SSIM in:\n%s", stderr.String())
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}
