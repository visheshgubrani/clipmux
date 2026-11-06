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
	"testing"
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
