package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/visheshgubrani/clipmux/internal/transcripts"
)

// speechTools returns the media tools with whisper-cli and the pinned
// models, sandboxed by this test binary, or skips (fails under
// CLIPMUX_REQUIRE_MEDIA_TOOLS=1, unless CLIPMUX_SKIP_SPEECH=1 asks to skip). The models are read from
// CLIPMUX_TEST_MODELS_DIR, under their upstream file names.
func speechTools(t *testing.T) (Tools, Speech) {
	t.Helper()
	if os.Getenv("CLIPMUX_SKIP_SPEECH") == "1" {
		t.Skip("speech tests skipped (CLIPMUX_SKIP_SPEECH=1); they run on the GPU runner")
	}
	tools := testTools(t)
	tools.Whisper = "whisper-cli"
	if p := os.Getenv("CLIPMUX_WHISPER"); p != "" {
		tools.Whisper = p
	}
	missing := func(what string, err error) {
		if os.Getenv("CLIPMUX_REQUIRE_MEDIA_TOOLS") == "1" {
			t.Fatalf("%s is required (CLIPMUX_REQUIRE_MEDIA_TOOLS=1): %v", what, err)
		}
		t.Skipf("%s: %v; skipping speech test", what, err)
	}
	if _, err := exec.LookPath(tools.Whisper); err != nil {
		missing("whisper-cli", err)
	}
	dir := os.Getenv("CLIPMUX_TEST_MODELS_DIR")
	sp := Speech{
		Backend:   SpeechCPU,
		Model:     filepath.Join(dir, "ggml-large-v3-turbo-q8_0.bin"),
		VAD:       filepath.Join(dir, "ggml-silero-v6.2.0.bin"),
		ModelName: "large-v3-turbo-q8_0@sha256:test",
		Threads:   max(runtime.NumCPU()/2, 1),
	}
	for _, f := range []string{sp.Model, sp.VAD} {
		if _, err := os.Stat(f); dir == "" || err != nil {
			missing("CLIPMUX_TEST_MODELS_DIR with "+filepath.Base(f), err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tools.Sandbox = &Sandbox{Exec: self}
	return tools, sp
}

// audioRendition makes what a transcription reads: the speech sample after
// `silence` of nothing, as the pipeline's audio rendition (AAC, 48 kHz
// stereo, fragmented MP4).
func audioRendition(t *testing.T, silence time.Duration) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "audio.mp4")
	ffmpeg(t, "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-i", filepath.Join("testdata", "speech", "jfk.ogg"),
		"-filter_complex", "[0]atrim=duration="+strconv.FormatFloat(silence.Seconds(), 'f', 3, 64)+"[s];[1]aresample=48000,aformat=channel_layouts=stereo[v];[s][v]concat=n=2:v=0:a=1",
		"-c:a", "aac", "-b:a", "128k", "-ac", "2", "-ar", "48000", "-movflags", "+frag_keyframe+empty_moov", out)
	return out
}

func transcribe(t *testing.T, tools Tools, sp Speech, audio string) *transcripts.Transcript {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	start := time.Now()
	raw, err := tools.Transcribe(ctx, dir, audio, sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s transcribed in %s", sp.Backend, time.Since(start).Round(time.Millisecond))
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", audio).Output()
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatal(err)
	}
	durationMS, _ := transcripts.Milliseconds(seconds)
	tr, err := transcripts.FromWhisperCPP(raw, durationMS, sp.ModelName)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Validate(); err != nil {
		t.Fatalf("whisper's transcript must validate: %v", err)
	}
	return tr
}

var notLetters = regexp.MustCompile(`[^a-z]+`)

func words(s string) string {
	return strings.TrimSpace(notLetters.ReplaceAllString(strings.ToLower(s), " "))
}

// firstWord finds the word whose letters are w, and returns when it starts.
func firstWord(t *testing.T, tr *transcripts.Transcript, w string) int64 {
	t.Helper()
	for _, s := range tr.Segments {
		for _, x := range s.Words {
			if words(x.Text) == w {
				return x.StartMS
			}
		}
	}
	t.Fatalf("no word %q in %q", w, tr.Text())
	return 0
}

// The golden transcription: the real model, under the full sandbox (no
// network, Landlock, seccomp), reads the audio rendition and finds the
// words, the language and their times. Silence before the speech moves
// every time by exactly that much: the VAD skips the silence without
// shifting the timeline it reports. Both checks share two transcriptions,
// each of which costs a CPU runner a minute or more.
func TestWhisperTranscribesUnderTheSandboxKeepingTheTimeline(t *testing.T) {
	tools, sp := speechTools(t)
	plain := transcribe(t, tools, sp, audioRendition(t, 10*time.Millisecond))
	tr := transcribe(t, tools, sp, audioRendition(t, 3010*time.Millisecond))
	for _, w := range []string{"americans", "country"} {
		d := firstWord(t, tr, w) - firstWord(t, plain, w)
		if d < 2700 || d > 3300 {
			t.Errorf("%q moved %d ms with 3 s of leading silence, want 3000 ± 300", w, d)
		}
	}
	if tr.Language != "en" {
		t.Fatalf("language %q", tr.Language)
	}
	if got := words(tr.Text()); !strings.Contains(got, "ask not what your country can do for you ask what you can do for your country") {
		t.Fatalf("text %q", got)
	}
	// "Americans" is said about 1.8 s into the clip, so 4.8 s in here.
	if at := firstWord(t, tr, "americans"); at < 4300 || at > 5600 {
		t.Fatalf("\"Americans\" at %d ms", at)
	}
	vtt := string(tr.VTT())
	if !strings.HasPrefix(vtt, "WEBVTT\n") || !strings.Contains(vtt, " --> ") {
		t.Fatalf("vtt %q", vtt)
	}
}

// whisper-cli is built without libcurl and runs on the CPU with -ng: it
// opens no network socket, so the sandbox's refusal is never what keeps it
// offline. (glibc may still try nscd's local AF_UNIX socket for a user
// lookup; the sandbox refuses that too, and it isn't the network.)
func TestCPUWhisperOpensNoSocket(t *testing.T) {
	tools, sp := speechTools(t)
	strace, err := exec.LookPath("strace")
	if err != nil {
		t.Skip("strace not installed")
	}
	dir := t.TempDir()
	wav := filepath.Join(dir, "speech.wav")
	ffmpeg(t, "-i", audioRendition(t, 10*time.Millisecond), "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", wav)
	trace := filepath.Join(dir, "trace")
	whisper, err := exec.LookPath(tools.Whisper)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(strace, append([]string{"-f", "-qq", "-e", "trace=socket,connect,socketpair", "-o", trace, whisper},
		WhisperArgs(sp, wav, filepath.Join(dir, "out"))...)...)
	cmd.Env = []string{}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("whisper under strace: %v\n%s", err, out)
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(line, "socket(") && !strings.Contains(line, "socket(AF_UNIX") {
			t.Fatalf("whisper opened a network socket:\n%s", b)
		}
		if strings.Contains(line, "connect(") && !strings.Contains(line, "sa_family=AF_UNIX") {
			t.Fatalf("whisper connected somewhere:\n%s", b)
		}
	}
}

// On a machine with a Vulkan GPU, the same transcription runs on it under
// the GPU sandbox and finds the same words.
func TestWhisperOnVulkan(t *testing.T) {
	tools, sp := speechTools(t)
	if nodes, _ := filepath.Glob("/dev/dri/renderD*"); len(nodes) == 0 {
		t.Skip("no render node")
	}
	sp.Backend, sp.Threads = SpeechVulkan, 4
	tr := transcribe(t, tools, sp, audioRendition(t, 3*time.Second))
	if got := words(tr.Text()); !strings.Contains(got, "ask what you can do for your country") {
		t.Fatalf("text %q", got)
	}
}

// On an NVIDIA machine whose whisper-cli carries the CUDA backend (the
// image, D131), the transcription runs on CUDA under the GPU sandbox.
func TestWhisperOnCUDA(t *testing.T) {
	tools, sp := speechTools(t)
	if nodes, _ := filepath.Glob("/dev/nvidia[0-9]*"); len(nodes) == 0 {
		t.Skip("no NVIDIA card")
	}
	sp.Backend, sp.Threads = SpeechCUDA, 4
	if err := ProbeSpeech(t.Context(), tools, t.TempDir(), sp); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	tr := transcribe(t, tools, sp, audioRendition(t, 3*time.Second))
	if got := words(tr.Text()); !strings.Contains(got, "ask what you can do for your country") {
		t.Fatalf("text %q", got)
	}
	t.Logf("CUDA transcription of 14 s took %s", time.Since(start).Round(time.Millisecond))
}

func TestSpeechArgs(t *testing.T) {
	got := SpeechWAVArgs("in.mp4", "out.wav")
	if !slices.Contains(got, "-format_whitelist") || strings.Join(got[len(got)-14:], " ") != "-i in.mp4 -map 0:a:0 -vn -ac 1 -ar 16000 -c:a pcm_s16le -f wav out.wav" {
		t.Fatalf("wav args %q", got)
	}
	sp := Speech{Backend: SpeechCPU, Model: "/m/model.bin", VAD: "/m/vad.bin", Threads: 3}
	if got := strings.Join(WhisperArgs(sp, "a.wav", "out"), " "); got != "-m /m/model.bin -f a.wav -l auto --vad -vm /m/vad.bin -t 3 -ojf -of out -np -sns -ng" {
		t.Fatalf("cpu args %q", got)
	}
	for _, b := range []SpeechBackend{SpeechVulkan, SpeechCUDA} {
		sp.Backend = b
		if got := WhisperArgs(sp, "a.wav", "out"); slices.Contains(got, "-ng") {
			t.Fatalf("%s args %q must use the GPU", b, got)
		}
	}
}

// The owner's recordings (testdata/speech/README.md): English comes back
// "en", Hindi "hi", and Hinglish either, with text. They are recorded by
// the owner rather than generated, so detection is tested on real speech.
func TestDetectsTheOwnersLanguages(t *testing.T) {
	tools, sp := speechTools(t)
	for _, c := range []struct {
		file string
		want []string
	}{
		{"en.m4a", []string{"en"}},
		{"hi.m4a", []string{"hi"}},
		{"hinglish.m4a", []string{"hi", "en"}},
	} {
		t.Run(c.file, func(t *testing.T) {
			path := filepath.Join("testdata", "speech", c.file)
			if _, err := os.Stat(path); err != nil {
				t.Skipf("%s isn't recorded yet (testdata/speech/README.md)", path)
			}
			tr := transcribe(t, tools, sp, path)
			if !slices.Contains(c.want, tr.Language) || strings.TrimSpace(tr.Text()) == "" {
				t.Fatalf("language %q, text %q; want one of %v with text", tr.Language, tr.Text(), c.want)
			}
		})
	}
}

// fakeWhisper logs line as whisper.cpp does when it picks (or finds no)
// GPU, writes an empty transcription, and fails unless hidden, the other
// GPU backend's device variable, is set and empty.
func fakeWhisper(t *testing.T, line, hidden string) string {
	t.Helper()
	check := ""
	if hidden != "" {
		check = `[ "${` + hidden + `-unset}" = "" ] || { echo "` + hidden + ` isn't hidden" >&2; exit 1; }` + "\n"
	}
	return script(t, "whisper-cli", check+`out=""
while [ $# -gt 0 ]; do [ "$1" = "-of" ] && out="$2"; shift; done
echo "whisper_backend_init_gpu: device 0: CPU (type: 0)" >&2
echo "`+line+`" >&2
echo '{"transcription": []}' > "$out.json"
`)
}

// The probe proves the backend it names: whisper.cpp falls back to the CPU
// when it finds no GPU and still succeeds, which is how a Modal L4 once
// "passed" Vulkan and then transcribed at CPU speed.
func TestTheSpeechProbeProvesTheGPUItNames(t *testing.T) {
	cases := []struct {
		backend      SpeechBackend
		line, hidden string
		ok           bool
	}{
		{SpeechCUDA, "whisper_backend_init_gpu: using CUDA0 backend", "GGML_VK_VISIBLE_DEVICES", true},
		{SpeechVulkan, "whisper_backend_init_gpu: using Vulkan0 backend", "CUDA_VISIBLE_DEVICES", true},
		{SpeechVulkan, "whisper_backend_init_gpu: no GPU found", "", false},
		{SpeechCUDA, "whisper_backend_init_gpu: no GPU found", "", false},
		{SpeechCUDA, "whisper_backend_init_gpu: using Vulkan0 backend", "", false},
		{SpeechCPU, "whisper_backend_init_gpu: no GPU found", "", true},
	}
	for _, c := range cases {
		tools := Tools{FFmpeg: script(t, "ffmpeg", "exit 0\n"), Whisper: fakeWhisper(t, c.line, c.hidden)}
		sp := Speech{Backend: c.backend, Model: "/m/model.bin", Threads: 4}
		err := ProbeSpeech(context.Background(), tools, t.TempDir(), sp)
		if (err == nil) != c.ok {
			t.Errorf("%s probe logging %q: err = %v, want ok %v", c.backend, c.line, err, c.ok)
		}
		if strings.Contains(c.line, "no GPU") && c.backend.GPU() && err != nil && !strings.Contains(err.Error(), "CPU") {
			t.Errorf("%s probe error %q should say whisper would run on the CPU", c.backend, err)
		}
	}
}
