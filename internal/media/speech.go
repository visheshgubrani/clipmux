package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Speech to text (P5-T11): whisper.cpp, sandboxed like every media tool. A
// transcription reads the committed output's audio rendition, which the
// encode put on the same zero-based timeline as the video, so its times
// need no offset.

// SpeechBackend is where whisper.cpp runs.
type SpeechBackend string

const (
	// SpeechCPU runs on the CPU, in whichever variant of ggml's CPU backend
	// the processor supports.
	SpeechCPU SpeechBackend = "cpu"
	// SpeechVulkan runs on a GPU through Vulkan: AMD and Intel with mesa,
	// NVIDIA with its driver's ICD.
	SpeechVulkan SpeechBackend = "vulkan"
	// SpeechCUDA runs on an NVIDIA GPU through ggml's CUDA backend: what a
	// serverless provider's card runs, since gVisor (Modal) offers CUDA but
	// not always the Vulkan ICD (D131). The image carries it, agents don't.
	SpeechCUDA SpeechBackend = "cuda"
)

// GPU reports whether a backend runs on a GPU.
func (b SpeechBackend) GPU() bool { return b == SpeechVulkan || b == SpeechCUDA }

// gpuLine is how whisper.cpp says which GPU it took, or that it found none
// and runs on the CPU, e.g. "whisper_backend_init_gpu: using CUDA0 backend".
var gpuLine = regexp.MustCompile(`whisper_backend_init_gpu: (using (\S+) backend|no GPU found)`)

// devicePrefix is how ggml names a backend's devices: CUDA0, Vulkan0.
var devicePrefix = map[SpeechBackend]string{SpeechCUDA: "CUDA", SpeechVulkan: "Vulkan"}

// Speech is how a worker transcribes: the backend its probe proved, the
// model files (absolute paths, verified by digest), and the threads it may
// use.
type Speech struct {
	Backend SpeechBackend
	// Model and VAD are the GGML model and the Silero VAD model.
	Model, VAD string
	// ModelName is what the transcript records: "{name}@sha256:{hex}".
	ModelName string
	// Threads for whisper; on the CPU the speech slot gets half the machine.
	Threads int
}

// SpeechWAVArgs make the 16 kHz mono PCM whisper.cpp reads from the first
// audio stream of in.
func SpeechWAVArgs(in, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	args = append(args, inputGuard()...)
	return append(args, "-i", in, "-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", "-f", "wav", out)
}

// WhisperArgs run whisper-cli on a WAV: the language detected (`-l auto`),
// speech found by the VAD model (so long silences produce no text and cost
// no decoding), no invented non-speech tokens, and full JSON with token
// offsets written to outBase.json.
// On the CPU the GPU is never touched (-ng).
func WhisperArgs(sp Speech, wav, outBase string) []string {
	args := []string{
		"-m", sp.Model, "-f", wav, "-l", "auto",
		"--vad", "-vm", sp.VAD,
		"-t", strconv.Itoa(max(sp.Threads, 1)),
		"-ojf", "-of", outBase, "-np",
		// Suppress non-speech tokens ("[Music]", "(laughs)"), as
		// clipmux-lms's faster-whisper did by default: Whisper invents them.
		"-sns",
	}
	if !sp.Backend.GPU() {
		args = append(args, "-ng")
	}
	return args
}

// Transcribe turns the audio file (any container the input allowlist takes)
// into whisper.cpp's full JSON, working in dir. The tools may read the audio
// and the two model files, and nothing else outside dir.
func (t Tools) Transcribe(ctx context.Context, dir, audio string, sp Speech) ([]byte, error) {
	if t.Whisper == "" {
		return nil, fmt.Errorf("transcribe: no whisper-cli configured")
	}
	tools := t.In(dir, append([]string{audio, sp.Model, sp.VAD}, t.readList()...)...)
	wav := filepath.Join(dir, "speech.wav")
	if err := tools.run(ctx, t.FFmpeg, SpeechWAVArgs(audio, wav), dir, io.Discard); err != nil {
		return nil, fmt.Errorf("transcribe: audio: %w", err)
	}
	if sp.Backend.GPU() {
		tools = tools.forSpeechGPU(sp.Backend)
	}
	out := filepath.Join(dir, "speech")
	if err := tools.run(ctx, t.Whisper, WhisperArgs(sp, wav, out), dir, io.Discard); err != nil {
		return nil, fmt.Errorf("transcribe: whisper: %w", err)
	}
	b, err := os.ReadFile(out + ".json")
	if err != nil {
		return nil, fmt.Errorf("transcribe: read result: %w", err)
	}
	return b, nil
}

// readList is what the sandbox already lets the tools read beyond their
// directory, kept when Transcribe adds the model files.
func (t Tools) readList() []string {
	if t.Sandbox == nil {
		return nil
	}
	return t.Sandbox.Read
}

// forSpeechGPU runs whisper on backend b's GPU: the other GPU backend's
// devices are hidden, since ggml takes the first GPU it finds (CUDA before
// Vulkan), and the sandbox lets it open every DRM render node (AMD, Intel)
// and NVIDIA's character devices.
func (t Tools) forSpeechGPU(b SpeechBackend) Tools {
	hide := "CUDA_VISIBLE_DEVICES"
	if b == SpeechCUDA {
		hide = "GGML_VK_VISIBLE_DEVICES"
	}
	env := slices.DeleteFunc(slices.Clone(t.Env), func(kv string) bool { return strings.HasPrefix(kv, hide+"=") })
	t.Env = append(env, hide+"=") // empty: no devices
	if t.Sandbox == nil {
		return t
	}
	sb := *t.Sandbox
	sb.GPU = true
	render, _ := filepath.Glob("/dev/dri/renderD*") // the pattern is valid
	sb.Devices = append(render, gpuDevices(NVENC, "")...)
	t.Sandbox = &sb
	return t
}

// deviceLines quotes what whisper and ggml said about devices and backends,
// for a probe's error: why a GPU wasn't taken is usually there.
func deviceLines(log string) string {
	var out []string
	for line := range strings.Lines(log) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "whisper_backend_init_gpu:") || strings.HasPrefix(line, "ggml_vulkan:") ||
			strings.HasPrefix(line, "ggml_cuda") || strings.HasPrefix(line, "load_backend:") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out[:min(len(out), 8)], "; ") + ")"
}

// Model is a file a speech tool needs, fetched by digest (versions.env).
type Model struct {
	Name, URL, SHA256 string
	Size              int64
}

// FileName is the model's file name, the one upstream uses.
func (m Model) FileName() string {
	return m.URL[strings.LastIndexByte(m.URL, '/')+1:]
}

// ID is how a transcript names the model: "{name}@sha256:{hex}".
func (m Model) ID() string { return m.Name + "@sha256:" + m.SHA256 }

// ProbeSpeech proves a speech backend works here, as DetectHardware proves
// encoders: whisper runs the real model on two seconds of a tone, under the
// sandbox jobs use, without VAD (which would skip the tone and prove
// nothing), and must write its JSON.
func ProbeSpeech(ctx context.Context, t Tools, dir string, sp Speech) error {
	if t.Whisper == "" {
		return fmt.Errorf("no whisper-cli configured")
	}
	tools := t.In(dir, append([]string{sp.Model}, t.readList()...)...)
	wav := filepath.Join(dir, "probe.wav")
	if err := tools.run(ctx, t.FFmpeg, []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=16000:duration=2",
		"-ac", "1", "-c:a", "pcm_s16le", "-f", "wav", wav}, dir, io.Discard); err != nil {
		return fmt.Errorf("probe audio: %w", err)
	}
	if sp.Backend.GPU() {
		tools = tools.forSpeechGPU(sp.Backend)
	}
	out := filepath.Join(dir, "probe")
	// Without -np, so whisper says which GPU it took: it falls back to the
	// CPU when it finds none and still succeeds, which proves nothing.
	args := []string{"-m", sp.Model, "-f", wav, "-l", "en", "-t", strconv.Itoa(max(sp.Threads, 1)), "-ojf", "-of", out}
	if !sp.Backend.GPU() {
		args = append(args, "-ng")
	}
	var log bytes.Buffer
	if err := tools.runLogged(ctx, t.Whisper, args, dir, io.Discard, &log); err != nil {
		return fmt.Errorf("probe %s: %w", sp.Backend, err)
	}
	if sp.Backend.GPU() {
		m := gpuLine.FindStringSubmatch(log.String())
		switch {
		case m == nil:
			return fmt.Errorf("probe %s: whisper didn't say which GPU it used, so it may run on the CPU%s", sp.Backend, deviceLines(log.String()))
		case m[2] == "":
			return fmt.Errorf("probe %s: whisper found no %s GPU and would run on the CPU%s", sp.Backend, sp.Backend, deviceLines(log.String()))
		case !strings.HasPrefix(m[2], devicePrefix[sp.Backend]):
			return fmt.Errorf("probe %s: whisper took %s instead of a %s GPU%s", sp.Backend, m[2], sp.Backend, deviceLines(log.String()))
		}
	}
	b, err := os.ReadFile(out + ".json")
	if err != nil {
		return fmt.Errorf("probe %s: %w", sp.Backend, err)
	}
	var result struct {
		Transcription json.RawMessage `json:"transcription"`
	}
	if err := json.Unmarshal(b, &result); err != nil || result.Transcription == nil {
		return fmt.Errorf("probe %s: whisper wrote no transcription", sp.Backend)
	}
	return nil
}
