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
// CLIPMUX_REQUIRE_MEDIA_TOOLS=1). The models are read from
// CLIPMUX_TEST_MODELS_DIR, under their upstream file names.
func speechTools(t *testing.T) (Tools, Speech) {
	t.Helper()
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
// words, the language and their times.
func TestWhisperTranscribesUnderTheSandbox(t *testing.T) {
	tools, sp := speechTools(t)
	tr := transcribe(t, tools, sp, audioRendition(t, 3*time.Second))
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

// Silence before the speech moves every time by exactly that much: the VAD
// skips the silence without shifting the timeline it reports.
func TestLeadingSilenceKeepsTheTimelineUnderVAD(t *testing.T) {
	tools, sp := speechTools(t)
	plain := transcribe(t, tools, sp, audioRendition(t, 10*time.Millisecond))
	late := transcribe(t, tools, sp, audioRendition(t, 3010*time.Millisecond))
	for _, w := range []string{"americans", "country"} {
		d := firstWord(t, late, w) - firstWord(t, plain, w)
		if d < 2700 || d > 3300 {
			t.Errorf("%q moved %d ms with 3 s of leading silence, want 3000 ± 300", w, d)
		}
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

func TestSpeechArgs(t *testing.T) {
	got := SpeechWAVArgs("in.mp4", "out.wav")
	if !slices.Contains(got, "-format_whitelist") || strings.Join(got[len(got)-14:], " ") != "-i in.mp4 -map 0:a:0 -vn -ac 1 -ar 16000 -c:a pcm_s16le -f wav out.wav" {
		t.Fatalf("wav args %q", got)
	}
	sp := Speech{Backend: SpeechCPU, Model: "/m/model.bin", VAD: "/m/vad.bin", Threads: 3}
	if got := strings.Join(WhisperArgs(sp, "a.wav", "out"), " "); got != "-m /m/model.bin -f a.wav -l auto --vad -vm /m/vad.bin -t 3 -ojf -of out -np -sns -ng" {
		t.Fatalf("cpu args %q", got)
	}
	sp.Backend = SpeechVulkan
	if got := WhisperArgs(sp, "a.wav", "out"); slices.Contains(got, "-ng") {
		t.Fatalf("vulkan args %q must use the GPU", got)
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
