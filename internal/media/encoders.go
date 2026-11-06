package media

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Hardware encoding (P3-T03, D046). A PC agent is mostly worth having for a
// GPU, so the ladder can encode on one, with the same bitrates, keyframes
// and GOPs as libx264, and falls back stage by stage to the CPU when a GPU
// can't do it.

// Backend is a family of encoding stages.
type Backend string

// Backends, in the order a worker prefers them.
const (
	// NVENC decodes with CUDA, scales with scale_cuda and encodes with
	// h264_nvenc.
	NVENC Backend = "nvenc"
	// VAAPI is AMD (radeonsi) and Intel (iHD) on Linux: VAAPI decode,
	// scale_vaapi and h264_vaapi.
	VAAPI Backend = "vaapi"
	// CUDADecode is an NVIDIA card without NVENC: the GPU decodes and
	// libx264 encodes.
	CUDADecode Backend = "cuda-decode"
	// CPU is libx264, which every worker has.
	CPU Backend = "cpu"
)

// ParseBackend reads --encoder: auto (""), or a backend name.
func ParseBackend(s string) (Backend, error) {
	switch b := Backend(s); b {
	case "", "auto":
		return "", nil
	case NVENC, VAAPI, CUDADecode, CPU:
		return b, nil
	}
	return "", fmt.Errorf("unknown encoder %q: use auto, nvenc, vaapi, cuda-decode or cpu", s)
}

// Hardware is what a worker's GPUs proved they can do, by real two-frame
// encodes when it started. Listing an encoder proves nothing: ffmpeg lists
// h264_nvenc on machines with no NVIDIA card.
type Hardware struct {
	NVENC bool `json:"nvenc,omitempty"`
	// CUDA means a CUDA device opens, so the GPU can at least decode.
	CUDA bool `json:"cuda,omitempty"`
	// VAAPIDevice is the render node h264_vaapi encoded on, or "".
	VAAPIDevice string `json:"vaapi_device,omitempty"`
	// Prefer is the backend to try first (--encoder), or "" for auto.
	Prefer Backend `json:"prefer,omitempty"`
}

// Backends lists what this hardware offers, best first, always ending with
// the CPU.
func (h Hardware) Backends() []Backend {
	var out []Backend
	if h.NVENC {
		out = append(out, NVENC)
	}
	if h.VAAPIDevice != "" {
		out = append(out, VAAPI)
	}
	if h.CUDA && !h.NVENC {
		out = append(out, CUDADecode)
	}
	if h.Prefer != "" && slices.Contains(out, h.Prefer) {
		out = append([]Backend{h.Prefer}, slices.DeleteFunc(out, func(b Backend) bool { return b == h.Prefer })...)
	}
	if h.Prefer == CPU {
		out = nil
	}
	return append(out, CPU)
}

// Encoders names the H.264 encoders this hardware proved, for the claim.
func (h Hardware) Encoders() []string {
	var out []string
	if h.NVENC {
		out = append(out, "h264_nvenc")
	}
	if h.VAAPIDevice != "" {
		out = append(out, "h264_vaapi")
	}
	if h.CUDA {
		out = append(out, "cuda_decode")
	}
	return append(out, "libx264")
}

// Stage is one way to encode the ladder: a backend, decoding on the GPU or
// on the CPU (a hybrid, for sources the GPU can't decode).
type Stage struct {
	Backend  Backend
	HWDecode bool
}

// Name is how a stage appears in a manifest: nvenc, nvenc-hybrid, vaapi,
// vaapi-hybrid, cuda-decode or cpu.
func (s Stage) Name() string {
	if (s.Backend == NVENC || s.Backend == VAAPI) && !s.HWDecode {
		return string(s.Backend) + "-hybrid"
	}
	return string(s.Backend)
}

// gpu reports whether a stage touches a GPU.
func (s Stage) gpu() bool { return s.Backend != CPU }

// hwDecodable is what every GPU here decodes: common codecs in 8-bit 4:2:0.
// Anything else (10-bit, 4:2:2, ProRes) decodes on the CPU and still
// encodes on the GPU.
func hwDecodable(src Source) bool {
	return slices.Contains([]string{"h264", "hevc", "vp9", "av1", "mpeg2video"}, src.Codec) &&
		slices.Contains([]string{"yuv420p", "yuvj420p", "nv12"}, src.PixFmt)
}

// Chain is the order a source is tried in: each GPU backend decoding on the
// GPU where it can, then decoding on the CPU, and the CPU last. A stage
// that fails hands the whole ladder to the next, so one attempt still
// encodes every rendition alike.
func (h Hardware) Chain(src Source) []Stage {
	var out []Stage
	for _, b := range h.Backends() {
		switch b {
		case NVENC, VAAPI:
			if hwDecodable(src) {
				out = append(out, Stage{Backend: b, HWDecode: true})
			}
			out = append(out, Stage{Backend: b})
		case CUDADecode:
			out = append(out, Stage{Backend: b, HWDecode: true})
		case CPU:
			out = append(out, Stage{Backend: CPU})
		}
	}
	return out
}

// EncodeArgs builds one ffmpeg run that decodes the source once and encodes
// every rung on stage's backend, keyframes forced on segment boundaries so
// all renditions align. Renditions carry no audio. dev is the VAAPI render
// node.
func EncodeArgs(stage Stage, dev string, src Source, input, encDir string, rungs []Rung) []string {
	gop := strconv.Itoa(int(math.Round(SegmentSeconds * src.FPS)))
	args := []string{"-hide_banner", "-nostdin", "-y", "-nostats", "-progress", "pipe:1"}
	switch {
	case stage.Backend == NVENC && stage.HWDecode:
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	case stage.Backend == VAAPI && stage.HWDecode:
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", dev, "-hwaccel_output_format", "vaapi")
	case stage.Backend == VAAPI:
		args = append(args, "-init_hw_device", "vaapi=va:"+dev, "-filter_hw_device", "va")
	case stage.Backend == CUDADecode:
		// Decoded frames come back to system memory; if the GPU can't
		// decode this source, ffmpeg decodes it on the CPU instead.
		args = append(args, "-hwaccel", "cuda")
	}
	args = append(args, inputGuard()...)
	args = append(args, "-i", input)
	graph := fmt.Sprintf("[0:v:0]setpts=PTS-STARTPTS,split=%d", len(rungs))
	for i := range rungs {
		graph += fmt.Sprintf("[s%d]", i)
	}
	for i, r := range rungs {
		w, h := r.Dimensions(src.Width, src.Height)
		var scale string
		switch {
		case stage.Backend == NVENC && stage.HWDecode:
			scale = fmt.Sprintf("scale_cuda=%d:%d:format=yuv420p", w, h)
		case stage.Backend == VAAPI && stage.HWDecode:
			scale = fmt.Sprintf("scale_vaapi=w=%d:h=%d:format=nv12", w, h)
		case stage.Backend == VAAPI:
			scale = fmt.Sprintf("scale=%d:%d,format=nv12,hwupload", w, h)
		default:
			scale = fmt.Sprintf("scale=%d:%d,format=yuv420p", w, h)
		}
		graph += fmt.Sprintf(";[s%d]%s[v%d]", i, scale, i)
	}
	args = append(args, "-filter_complex", graph)
	for i, r := range rungs {
		args = append(args, "-map", fmt.Sprintf("[v%d]", i))
		args = append(args, codecArgs(stage.Backend, r, gop)...)
		args = append(args,
			"-fps_mode", "cfr", "-r", src.FrameRate,
			"-g", gop,
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", SegmentSeconds),
			"-flags", "+cgop", "-an",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
			filepath.Join(encDir, "v_"+r.Label()+".mp4"))
	}
	return args
}

// codecArgs are each encoder's settings at parity: the ladder's bitrate,
// maxrate and buffer, high profile, and no keyframes but the forced ones,
// so every backend cuts segments in the same places.
func codecArgs(b Backend, r Rung, gop string) []string {
	rate := []string{"-b:v", r.Bitrate, "-maxrate", r.Maxrate, "-bufsize", r.Bufsize}
	switch b {
	case NVENC:
		return append([]string{"-c:v", "h264_nvenc", "-preset", "p4", "-tune", "hq", "-profile:v", "high", "-rc", "vbr",
			"-forced-idr", "1", "-no-scenecut", "1", "-strict_gop", "1"}, rate...)
	case VAAPI:
		return append([]string{"-c:v", "h264_vaapi", "-profile:v", "high", "-rc_mode", "VBR"}, rate...)
	}
	return append(append([]string{"-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high"}, rate...),
		"-keyint_min", gop, "-sc_threshold", "0")
}

// gpuDevices are the device nodes a stage's tool may open.
func gpuDevices(b Backend, vaapiDev string) []string {
	switch b {
	case VAAPI:
		return []string{vaapiDev}
	case NVENC, CUDADecode:
		nodes, _ := filepath.Glob("/dev/nvidia*") // the pattern is valid
		var out []string
		for _, n := range nodes {
			if fi, err := os.Stat(n); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}

// forStage returns the tools for one stage: a GPU stage may open its device
// nodes and read sysfs, under a data-segment limit instead of an
// address-space one.
func (t Tools) forStage(s Stage) Tools {
	if !s.gpu() || t.Sandbox == nil {
		return t
	}
	sb := *t.Sandbox
	sb.GPU = true
	sb.Devices = gpuDevices(s.Backend, t.HW.VAAPIDevice)
	t.Sandbox = &sb
	return t
}

// probeArgs encode two synthetic frames on a backend; success proves the
// whole path (driver, device, encoder, sandbox) works.
func probeArgs(b Backend, dev string) []string {
	common := []string{"-hide_banner", "-nostdin", "-loglevel", "error"}
	src := []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-frames:v", "2"}
	switch b {
	case NVENC:
		return append(append(common, src...), "-c:v", "h264_nvenc", "-f", "null", "-")
	case VAAPI:
		return append(append(append(common, "-init_hw_device", "vaapi=va:"+dev, "-filter_hw_device", "va"), src...),
			"-vf", "format=nv12,hwupload", "-c:v", "h264_vaapi", "-f", "null", "-")
	case CUDADecode:
		return append(append(append(common, "-init_hw_device", "cuda=cu", "-filter_hw_device", "cu"), src...),
			"-vf", "format=nv12,hwupload,hwdownload,format=nv12", "-f", "null", "-")
	}
	return nil
}

// DetectHardware probes each GPU backend with a real encode, in dir, under
// the same sandbox jobs use. A backend that fails here is never offered.
// vaapiDev pins the render node; "" tries each one present.
func DetectHardware(ctx context.Context, t Tools, dir, vaapiDev string, prefer Backend) Hardware {
	h := Hardware{Prefer: prefer}
	if prefer == CPU {
		return h
	}
	try := func(b Backend, dev string) bool {
		probe := t
		probe.HW.VAAPIDevice = dev
		probe = probe.In(dir).forStage(Stage{Backend: b})
		return probe.run(ctx, t.FFmpeg, probeArgs(b, dev), dir, io.Discard) == nil
	}
	h.NVENC = try(NVENC, "")
	h.CUDA = h.NVENC || try(CUDADecode, "")
	devs := []string{vaapiDev}
	if vaapiDev == "" {
		devs, _ = filepath.Glob("/dev/dri/renderD*") // the pattern is valid
	}
	for _, d := range devs {
		if try(VAAPI, d) {
			h.VAAPIDevice = d
			break
		}
	}
	return h
}

// Describe is a one-line summary for logs, e.g. "vaapi (/dev/dri/renderD128), cpu".
func (h Hardware) Describe() string {
	var parts []string
	for _, b := range h.Backends() {
		if b == VAAPI {
			parts = append(parts, fmt.Sprintf("vaapi (%s)", h.VAAPIDevice))
			continue
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ", ")
}
