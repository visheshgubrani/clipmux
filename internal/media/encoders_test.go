package media

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var hd = Source{Width: 1280, Height: 720, FrameRate: "30", FPS: 30, Duration: 60, Codec: "h264", PixFmt: "yuv420p"}

func names(chain []Stage) []string {
	var out []string
	for _, s := range chain {
		out = append(out, s.Name())
	}
	return out
}

func TestEachGPUTriesItsOwnDecodeThenTheCPUs(t *testing.T) {
	tenBit := hd
	tenBit.PixFmt = "yuv420p10le"
	cases := []struct {
		name string
		hw   Hardware
		src  Source
		want []string
	}{
		{"no GPU", Hardware{}, hd, []string{"cpu"}},
		{"NVIDIA with NVENC", Hardware{NVENC: true, CUDA: true}, hd, []string{"nvenc", "nvenc-hybrid", "cpu"}},
		{"NVIDIA without NVENC", Hardware{CUDA: true}, hd, []string{"cuda-decode", "cpu"}},
		{"AMD or Intel", Hardware{VAAPIDevice: "/dev/dri/renderD128"}, hd, []string{"vaapi", "vaapi-hybrid", "cpu"}},
		{"10-bit source decodes on the CPU", Hardware{VAAPIDevice: "/dev/dri/renderD128"}, tenBit, []string{"vaapi-hybrid", "cpu"}},
		{"both GPUs", Hardware{NVENC: true, CUDA: true, VAAPIDevice: "/d"}, hd, []string{"nvenc", "nvenc-hybrid", "vaapi", "vaapi-hybrid", "cpu"}},
		{"--encoder vaapi first", Hardware{NVENC: true, CUDA: true, VAAPIDevice: "/d", Prefer: VAAPI}, hd, []string{"vaapi", "vaapi-hybrid", "nvenc", "nvenc-hybrid", "cpu"}},
		{"--encoder cpu", Hardware{NVENC: true, CUDA: true, Prefer: CPU}, hd, []string{"cpu"}},
	}
	for _, c := range cases {
		if got := names(c.hw.Chain(c.src)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: chain = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEveryBackendKeepsTheLaddersRatesAndKeyframes(t *testing.T) {
	rungs := []Rung{{Height: 720, Bitrate: "3000k", Maxrate: "3500k", Bufsize: "6000k"}}
	common := []string{
		"-b:v 3000k -maxrate 3500k -bufsize 6000k", "-profile:v high", "-fps_mode cfr -r 30", "-g 180",
		"-force_key_frames expr:gte(t,n_forced*6)", "-flags +cgop -an", "/w/enc/v_720p.mp4",
		"-format_whitelist " + allowedDemuxers,
	}
	cases := map[string]struct {
		stage Stage
		want  []string
	}{
		"nvenc": {Stage{NVENC, true}, []string{
			"-hwaccel cuda -hwaccel_output_format cuda", "scale_cuda=1280:720:format=yuv420p",
			"-c:v h264_nvenc -preset p4 -tune hq", "-rc vbr -forced-idr 1 -no-scenecut 1 -strict_gop 1",
		}},
		"nvenc-hybrid": {Stage{NVENC, false}, []string{"[s0]scale=1280:720,format=yuv420p[v0]", "-c:v h264_nvenc"}},
		"vaapi": {Stage{VAAPI, true}, []string{
			"-hwaccel vaapi -hwaccel_device /dev/dri/renderD128 -hwaccel_output_format vaapi",
			"scale_vaapi=w=1280:h=720:format=nv12", "-c:v h264_vaapi -profile:v high -rc_mode VBR",
		}},
		"vaapi-hybrid": {Stage{VAAPI, false}, []string{
			"-init_hw_device vaapi=va:/dev/dri/renderD128 -filter_hw_device va", "scale=1280:720,format=nv12,hwupload", "-c:v h264_vaapi",
		}},
		"cuda-decode": {Stage{CUDADecode, true}, []string{"-hwaccel cuda -format_whitelist", "scale=1280:720,format=yuv420p", "-c:v libx264 -preset veryfast", "-keyint_min 180 -sc_threshold 0"}},
		"cpu":         {Stage{CPU, false}, []string{"-c:v libx264 -preset veryfast", "-keyint_min 180 -sc_threshold 0"}},
	}
	for name, c := range cases {
		args := strings.Join(EncodeArgs(c.stage, "/dev/dri/renderD128", hd, "/w/source", "/w/enc", rungs), " ")
		for _, want := range append(c.want, common...) {
			if !strings.Contains(args, want) {
				t.Errorf("%s: missing %q in\n%s", name, want, args)
			}
		}
		if name != "cpu" && name != "cuda-decode" && strings.Contains(args, "libx264") {
			t.Errorf("%s encodes with libx264", name)
		}
	}
}

// fakeFFmpeg fails any run that uses one of the given encoders.
func fakeFFmpeg(t *testing.T, failing ...string) string {
	t.Helper()
	script := "#!/bin/sh\n"
	for _, enc := range failing {
		script += "case \"$*\" in *" + enc + "*) echo 'Cannot load " + enc + "' >&2; exit 1;; esac\n"
	}
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte(script+"exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAFailingGPUHandsTheWholeLadderToTheNextStage(t *testing.T) {
	dir := t.TempDir()
	tools := Tools{FFmpeg: fakeFFmpeg(t, "h264_nvenc"), HW: Hardware{NVENC: true, CUDA: true}}
	used, err := encodeLadder(context.Background(), tools, hd, "/src", dir, dir, SelectLadder(1280, 720), func(string, float64) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := (EncoderUsed{Stage: "cpu", Fallbacks: []string{"nvenc", "nvenc-hybrid"}}); !reflect.DeepEqual(used, want) {
		t.Fatalf("used = %+v, want %+v", used, want)
	}

	tools.FFmpeg = fakeFFmpeg(t)
	used, err = encodeLadder(context.Background(), tools, hd, "/src", dir, dir, SelectLadder(1280, 720), func(string, float64) {})
	if err != nil || used.Stage != "nvenc" || used.Fallbacks != nil {
		t.Fatalf("a working GPU: used = %+v, %v", used, err)
	}
}

func TestTheCPUsFailureIsFinal(t *testing.T) {
	dir := t.TempDir()
	tools := Tools{FFmpeg: fakeFFmpeg(t, "h264_vaapi", "libx264"), HW: Hardware{VAAPIDevice: "/dev/dri/renderD128"}}
	_, err := encodeLadder(context.Background(), tools, hd, "/src", dir, dir, SelectLadder(1280, 720), func(string, float64) {})
	if err == nil || !strings.Contains(err.Error(), "(cpu)") {
		t.Fatalf("err = %v, want the CPU stage's failure", err)
	}
}

func TestACancelledEncodeDoesntFallBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	tools := Tools{FFmpeg: fakeFFmpeg(t), HW: Hardware{NVENC: true}}
	used, err := encodeLadder(ctx, tools, hd, "/src", dir, dir, SelectLadder(1280, 720), func(string, float64) {})
	if err == nil || len(used.Fallbacks) != 0 {
		t.Fatalf("cancelled: used = %+v, err = %v; want a stop with no fallback", used, err)
	}
}

func TestDetectionTrustsOnlyARealEncode(t *testing.T) {
	// This ffmpeg "lists" every encoder but can only run libx264.
	tools := Tools{FFmpeg: fakeFFmpeg(t, "h264_nvenc", "h264_vaapi", "cuda=cu")}
	h := DetectHardware(context.Background(), tools, t.TempDir(), "/dev/dri/renderD128", "")
	if h.NVENC || h.CUDA || h.VAAPIDevice != "" {
		t.Fatalf("detected %+v from encoders that can't run", h)
	}
	if got := h.Encoders(); !reflect.DeepEqual(got, []string{"libx264"}) {
		t.Fatalf("encoders = %v", got)
	}
	// Why each failed travels with the capabilities, so an operator (and the
	// real-provider run) can see why a GPU isn't used.
	want := map[Backend]string{
		NVENC:      "ffmpeg: exit status 1: Cannot load h264_nvenc",
		CUDADecode: "ffmpeg: exit status 1: Cannot load cuda=cu",
		VAAPI:      "/dev/dri/renderD128: ffmpeg: exit status 1: Cannot load h264_vaapi",
	}
	if !reflect.DeepEqual(h.Failed, want) {
		t.Fatalf("failed = %v, want %v", h.Failed, want)
	}
	ok := Tools{FFmpeg: fakeFFmpeg(t, "h264_nvenc", "cuda=cu")}
	if h := DetectHardware(context.Background(), ok, t.TempDir(), "/dev/dri/renderD128", ""); h.VAAPIDevice != "/dev/dri/renderD128" {
		t.Fatalf("a working VAAPI encode wasn't detected: %+v", h)
	}
}
