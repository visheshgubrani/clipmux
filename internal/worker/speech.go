package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/transcripts"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// Transcription on workers (P5-T14; docs/plans/P5-production.md,
// "Transcription on workers"). A worker that can transcribe runs a second
// claim loop for transcribe jobs beside its media loop, so a long
// transcription never delays the next upload's encode.

// Failure codes of a transcription.
const (
	// codeTranscribe: whisper failed. It may be the file's fault, so it
	// bars the fallback (D053).
	codeTranscribe = "transcribe_failed"
	// codeTranscriptInvalid: serve refused the transcript (409); permanent.
	codeTranscriptInvalid = "transcript_invalid"
	// codeModelUnavailable: the speech model is missing here; a capacity
	// code, so the job may go to another worker or a fallback.
	codeModelUnavailable = "model_unavailable"
)

// SpeechSlot is how this worker transcribes, once its probe proved it:
// shared by both claim loops, which report it in every claim.
type SpeechSlot struct{ p atomic.Pointer[media.Speech] }

// Get returns the proven speech setup, or nil.
func (s *SpeechSlot) Get() *media.Speech {
	if s == nil {
		return nil
	}
	return s.p.Load()
}

// Set records the speech setup the probe proved.
func (s *SpeechSlot) Set(sp *media.Speech) { s.p.Store(sp) }

// SpeechMode is --speech: whether, and on what, this worker transcribes.
type SpeechMode string

// Speech modes.
const (
	// SpeechAuto transcribes on a GPU whose probe passes (CUDA, then
	// Vulkan), else on a CPU with at least AutoMinCPUs and AutoMinMemory.
	SpeechAuto   SpeechMode = "auto"
	SpeechOff    SpeechMode = "off"
	SpeechCPU    SpeechMode = "cpu"
	SpeechVulkan SpeechMode = "vulkan"
	SpeechCUDA   SpeechMode = "cuda"
)

// What auto asks of a machine before it transcribes on the CPU, where
// whisper takes half the threads beside the encoder (P5 open question 7).
const (
	AutoMinCPUs   = 4
	AutoMinMemory = 6 << 30
)

// ParseSpeechMode reads --speech.
func ParseSpeechMode(s string) (SpeechMode, error) {
	switch m := SpeechMode(s); m {
	case "", SpeechAuto:
		return SpeechAuto, nil
	case SpeechOff, SpeechCPU, SpeechVulkan, SpeechCUDA:
		return m, nil
	}
	return "", fmt.Errorf("unknown speech mode %q: use auto, off, cpu, vulkan or cuda", s)
}

// SpeechCandidates are the backends a worker probes, best first, on this
// machine (D131).
func SpeechCandidates(mode SpeechMode, whisper string) []media.SpeechBackend {
	return speechCandidates(mode, hasNVIDIA(), hasGPU(), hasCUDABackend(whisper), cpuCanTranscribe())
}

// speechCandidates: CUDA on an NVIDIA card when whisper-cli carries the CUDA
// backend (the image does, an agent doesn't), Vulkan on any GPU, then the
// CPU where the mode allows it.
func speechCandidates(mode SpeechMode, nvidia, gpu, cudaBuilt, cpuOK bool) []media.SpeechBackend {
	var out []media.SpeechBackend
	if (mode == SpeechAuto || mode == SpeechCUDA) && nvidia && cudaBuilt {
		out = append(out, media.SpeechCUDA)
	}
	if (mode == SpeechAuto || mode == SpeechVulkan) && gpu {
		out = append(out, media.SpeechVulkan)
	}
	if mode == SpeechCPU || (mode == SpeechAuto && cpuOK) {
		out = append(out, media.SpeechCPU)
	}
	return out
}

// SetupSpeech decides how this worker transcribes: it uses the models baked
// into the image (bakedDir, D118) or fetches the pinned models by digest into
// modelsDir, then proves each candidate backend with a real transcription
// under the sandbox, GPU first. It returns nil when the worker won't
// transcribe, and says why in the log.
func SetupSpeech(ctx context.Context, tools media.Tools, mode SpeechMode, bakedDir, modelsDir, probeDir string, client *http.Client, log *slog.Logger) *media.Speech {
	if mode == SpeechOff {
		return nil
	}
	explicit := mode != SpeechAuto
	warn := log.Info
	if explicit {
		warn = log.Warn
	}
	if _, err := exec.LookPath(tools.Whisper); tools.Whisper == "" || err != nil {
		warn("speech: no whisper-cli, so this worker won't transcribe", "whisper", tools.Whisper)
		return nil
	}
	candidates := SpeechCandidates(mode, tools.Whisper)
	if len(candidates) == 0 {
		warn("speech: no backend this mode allows works here; set --speech cpu to transcribe on the CPU anyway",
			"mode", mode, "cpus", runtime.NumCPU(), "min_cpus", AutoMinCPUs, "min_memory_bytes", AutoMinMemory,
			"nvidia", hasNVIDIA(), "cuda_backend", hasCUDABackend(tools.Whisper))
		return nil
	}
	_, baked := BakedModel(bakedDir, media.SpeechModel)
	// The first start without baked models downloads about 0.9 GB, which
	// takes minutes; say so, or a missing subtitles track looks like a hang.
	if fi, err := os.Stat(filepath.Join(modelsDir, media.SpeechModel.FileName())); !baked && (err != nil || fi.Size() != media.SpeechModel.Size) {
		log.Info("speech: downloading the speech models; this worker transcribes once they are in place",
			"model", media.SpeechModel.ID(), "bytes", media.SpeechModel.Size+media.VADModel.Size)
	}
	model, err := findModel(ctx, client, bakedDir, modelsDir, media.SpeechModel)
	if err != nil {
		log.Warn("speech: couldn't fetch the speech model; will try again at the next start", "err", err)
		return nil
	}
	vad, err := findModel(ctx, client, bakedDir, modelsDir, media.VADModel)
	if err != nil {
		log.Warn("speech: couldn't fetch the VAD model; will try again at the next start", "err", err)
		return nil
	}
	for _, b := range candidates {
		sp := media.Speech{Backend: b, Model: model, VAD: vad, ModelName: media.SpeechModel.ID(), Threads: speechThreads(b)}
		dir := filepath.Join(probeDir, "speech-"+string(b))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			log.Warn("speech: probe dir", "err", err)
			return nil
		}
		start := time.Now()
		err := media.ProbeSpeech(ctx, tools, dir, sp)
		_ = os.RemoveAll(dir)
		if err == nil {
			log.Info("speech: this worker transcribes", "backend", b, "model", sp.ModelName, "threads", sp.Threads,
				"probe_ms", time.Since(start).Milliseconds())
			return &sp
		}
		log.Warn("speech: a backend failed its probe", "backend", b, "err", err)
	}
	return nil
}

// speechThreads: on the CPU the speech slot gets half the machine, beside
// the encoder; on a GPU whisper needs few.
func speechThreads(b media.SpeechBackend) int {
	if b.GPU() {
		return 4
	}
	return max(runtime.NumCPU()/2, 1)
}

// hasNVIDIA reports an NVIDIA card's device node.
func hasNVIDIA() bool {
	m, _ := filepath.Glob("/dev/nvidia[0-9]*") // the pattern is valid
	return len(m) > 0
}

// hasCUDABackend reports whether whisper-cli carries ggml's CUDA backend,
// which ggml loads from the executable's own directory (D077, D131).
func hasCUDABackend(whisper string) bool {
	path, err := exec.LookPath(whisper)
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	_, err = os.Stat(filepath.Join(filepath.Dir(path), "libggml-cuda.so"))
	return err == nil
}

// hasGPU reports a device Vulkan could use: a DRM render node or an
// NVIDIA card.
func hasGPU() bool {
	for _, pattern := range []string{"/dev/dri/renderD*", "/dev/nvidia[0-9]*"} {
		if m, _ := filepath.Glob(pattern); len(m) > 0 { // the patterns are valid
			return true
		}
	}
	return false
}

// cpuCanTranscribe applies auto's floor for CPU transcription. Memory it
// can't read counts as enough.
func cpuCanTranscribe() bool {
	if runtime.NumCPU() < AutoMinCPUs {
		return false
	}
	mem, ok := memTotal()
	return !ok || mem >= AutoMinMemory
}

// memTotal reads MemTotal from /proc/meminfo.
func memTotal() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb << 10, err == nil
		}
	}
	return 0, false
}

// BakedModel finds m among the models baked into the image (D118). The
// image build checked each against its sha256 and the image is pinned by
// digest, so the right size is enough here: re-hashing 0.9 GB would cost
// every serverless cold start seconds.
func BakedModel(dir string, m media.Model) (string, bool) {
	if dir == "" {
		return "", false
	}
	path := filepath.Join(dir, m.FileName())
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != m.Size {
		return "", false
	}
	return path, true
}

// findModel prefers the baked copy of m and fetches it into modelsDir only
// when the image has none.
func findModel(ctx context.Context, client *http.Client, bakedDir, modelsDir string, m media.Model) (string, error) {
	if path, ok := BakedModel(bakedDir, m); ok {
		return path, nil
	}
	return FetchModel(ctx, client, modelsDir, m)
}

// FetchModel makes sure dir holds model, downloaded by the worker (never by
// the sandboxed tool) and checked against its size and sha256 before it is
// used. A file already there is checked the same way.
func FetchModel(ctx context.Context, client *http.Client, dir string, m media.Model) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, m.FileName())
	if err := verifyModel(path, m); err == nil {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, m.FileName()+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// A stalled download is cut after a minute without bytes, like every
	// transfer a worker makes; a slow one takes as long as it needs.
	reqCtx, watch := idle(ctx, time.Minute)
	defer watch.stop()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, m.URL, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", m.Name, logging.RedactError(err))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d", m.Name, res.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(watch.reader(res.Body), m.Size+1))
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", m.Name, err)
	}
	if n != m.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return "", fmt.Errorf("fetch %s: got %d bytes that don't match its pinned size and sha256", m.Name, n)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func verifyModel(path string, m media.Model) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() != m.Size {
		return fmt.Errorf("%s: wrong size", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return fmt.Errorf("%s: wrong sha256", path)
	}
	return nil
}

// transcribe runs a transcription attempt: fetch the output's audio from
// serve, run whisper, and turn its JSON into the canonical transcript.
func (w *Worker) transcribe(ctx context.Context, job *workerapi.Job, dir string, report media.Progress, ph *phases) (*transcripts.Transcript, string, error) {
	sp := w.Speech.Get()
	if sp == nil || job.Speech == nil {
		return nil, codeModelUnavailable, errors.New("this worker doesn't transcribe")
	}
	for _, f := range []string{sp.Model, sp.VAD} {
		if _, err := os.Stat(f); err != nil {
			return nil, codeModelUnavailable, err
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, codeScratchFull, err
	}
	report("download", 0)
	audio := filepath.Join(dir, "audio.mp4")
	start := time.Now()
	if err := w.fetchAudio(ctx, job, audio); err != nil {
		return nil, codeDownload, err
	}
	ph.since("audio_fetch", start)
	report("transcribe", 0)
	start = time.Now()
	raw, err := w.Tools.Transcribe(ctx, dir, audio, *sp)
	if err != nil {
		return nil, codeTranscribe, err
	}
	took := time.Since(start)
	ph.add("backend", sp.Backend, "threads", sp.Threads, "duration_s", job.Speech.DurationSeconds, "transcribe_ms", took.Milliseconds())
	if job.Speech.DurationSeconds > 0 {
		// Wall-clock seconds per second of audio: well under 0.1 on a GPU.
		ph.add("realtime_factor", math.Round(took.Seconds()/job.Speech.DurationSeconds*1000)/1000)
	}
	durationMS, err := transcripts.Milliseconds(job.Speech.DurationSeconds)
	if err != nil {
		return nil, codeTranscribe, err
	}
	tr, err := transcripts.FromWhisperCPP(raw, durationMS, sp.ModelName)
	if err != nil {
		return nil, codeTranscribe, err
	}
	return tr, "", nil
}

// fetchAudio downloads the transcription's input, decrypted by serve, with
// the attempt token. A stalled stream is cut by the idle watchdog.
func (w *Worker) fetchAudio(ctx context.Context, job *workerapi.Job, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	reqCtx, watch := idle(ctx, w.idleTimeout())
	defer watch.stop()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, w.Server+"/worker/v1/attempts/"+job.AttemptID+"/audio", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+job.AttemptToken)
	res, err := w.httpClient().Do(req)
	if err != nil {
		return logging.RedactError(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		if strings.Contains(string(body), "attempt_stopped") {
			return fmt.Errorf("audio: %w", errStopped)
		}
		return fmt.Errorf("audio: HTTP 409: %s", truncate(string(body), 200))
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("audio: HTTP %d", res.StatusCode)
	}
	if _, err := io.Copy(f, watch.reader(res.Body)); err != nil {
		return fmt.Errorf("audio: %w", err)
	}
	return f.Close()
}
