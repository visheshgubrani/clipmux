package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/worker"
	"github.com/visheshgubrani/clipmux/internal/workerapi"
)

// runDoctor is `clipmux worker doctor`: everything an agent needs, checked
// in the order a failure would stop it, so the first ✗ is the one to fix.
// With --offline it skips the connection, the server and the key, to check
// an install (such as the agent tarball) before it is paired (D061).
func runDoctor(args []string, getenv func(string) string, out io.Writer) int {
	offline := len(args) > 0 && args[0] == "--offline"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	failed := false
	check := func(ok bool, what, detail string) {
		mark := "✓"
		if !ok {
			mark, failed = "✗", true
		}
		fmt.Fprintf(out, "  %s %s", mark, what)
		if detail != "" {
			fmt.Fprintf(out, ": %s", detail)
		}
		fmt.Fprintln(out)
	}
	note := func(what, detail string) { fmt.Fprintf(out, "  ! %s: %s\n", what, detail) }

	if !offline && !doctorConnection(ctx, getenv, check, note) {
		return 1
	}

	tools := media.DefaultTools()
	tools.FFmpeg = orEnv(getenv("CLIPMUX_FFMPEG"), tools.FFmpeg)
	tools.FFprobe = orEnv(getenv("CLIPMUX_FFPROBE"), tools.FFprobe)
	tools.Packager = orEnv(getenv("CLIPMUX_PACKAGER"), tools.Packager)
	// The driver variables reach the sandboxed tools, as in the worker: a
	// tarball's VAAPI drivers are found through LIBVA_DRIVERS_PATH.
	for _, n := range sandbox.EnvAllowed {
		if v := getenv(n); v != "" {
			tools.Env = append(tools.Env, n+"="+v)
		}
	}
	v, err := tools.Versions(ctx)
	if err != nil {
		check(false, "ffmpeg and Shaka Packager", err.Error())
		return 1
	}
	pv, ok := workerapi.ParseVersion(v.Packager)
	check(ok && workerapi.CompareVersion(pv, workerapi.MinPackager) >= 0, "ffmpeg and Shaka Packager",
		fmt.Sprintf("ffmpeg %s, packager %s (3.9.1 or later needed)", v.FFmpeg, v.Packager))

	self, _ := os.Executable()
	if st, err := sandbox.ProbeExec(ctx, self); err != nil {
		note("sandbox", "doesn't hold here ("+err.Error()+"); with --unsandboxed this PC takes only its own local jobs and your uploads")
	} else {
		check(true, "sandbox", st.String())
		tools.Sandbox = &media.Sandbox{Exec: self}
	}

	work := filepath.Join(os.TempDir(), "clipmux-doctor")
	_ = os.MkdirAll(work, 0o750)
	defer os.RemoveAll(work)
	hw := media.DetectHardware(ctx, tools, work, getenv("CLIPMUX_VAAPI_DEVICE"), "")
	check(true, "encoders", hw.Describe()+" (each passed a test encode)")
	if len(hw.Backends()) == 1 {
		note("GPU", "none passed a test encode; in Docker, add --gpus all (NVIDIA) or --device /dev/dri with the render group (AMD, Intel)")
	}
	doctorSpeech(ctx, tools, getenv, work, check, note)
	if free := worker.FreeDisk(os.TempDir()); free > 0 {
		msg := fmt.Sprintf("%.1f GiB free in %s; a source needs about three times its size", float64(free)/(1<<30), os.TempDir())
		if free < 10<<30 {
			note("scratch disk", msg+" (use --work-dir for a bigger disk)")
		} else {
			check(true, "scratch disk", msg)
		}
	}
	if failed {
		return 1
	}
	fmt.Fprintln(out, "\n  This PC is ready to encode.")
	return 0
}

// doctorSpeech checks transcription (P5-T14). It is optional, so only a
// probe that runs and fails is a ✗; anything missing is a note.
func doctorSpeech(ctx context.Context, tools media.Tools, getenv func(string) string, work string, check func(bool, string, string), note func(string, string)) {
	tools.Whisper = orEnv(getenv("CLIPMUX_WHISPER"), tools.Whisper)
	if _, err := exec.LookPath(tools.Whisper); err != nil {
		note("speech", "no whisper-cli, so this PC won't transcribe subtitles")
		return
	}
	dir := orEnv(getenv("CLIPMUX_MODELS_DIR"), filepath.Join(agentDir(getenv), "models"))
	if _, ok := worker.BakedModel(getenv("CLIPMUX_BAKED_MODELS_DIR"), media.SpeechModel); ok {
		dir = getenv("CLIPMUX_BAKED_MODELS_DIR") // the image's own (D118)
	}
	model, vad := filepath.Join(dir, media.SpeechModel.FileName()), filepath.Join(dir, media.VADModel.FileName())
	for _, m := range []struct {
		path string
		want media.Model
	}{{model, media.SpeechModel}, {vad, media.VADModel}} {
		if fi, err := os.Stat(m.path); err != nil || fi.Size() != m.want.Size {
			note("speech", fmt.Sprintf("the %s model isn't in %s yet (%d MB); the worker downloads it at start", m.want.Name, dir, m.want.Size>>20))
			return
		}
	}
	backends := []media.SpeechBackend{media.SpeechCPU}
	if nodes, _ := filepath.Glob("/dev/dri/renderD*"); len(nodes) > 0 {
		backends = append([]media.SpeechBackend{media.SpeechVulkan}, backends...)
	}
	var failures []string
	for _, b := range backends {
		sp := media.Speech{Backend: b, Model: model, VAD: vad, Threads: max(runtime.NumCPU()/2, 1)}
		if err := media.ProbeSpeech(ctx, tools, work, sp); err != nil {
			failures = append(failures, string(b)+": "+err.Error())
			continue
		}
		for _, f := range failures {
			note("speech", "failed its probe on "+f)
		}
		check(true, "speech", fmt.Sprintf("whisper ran %s on %s", media.SpeechModel.Name, b))
		return
	}
	check(false, "speech", strings.Join(failures, "; "))
}

// doctorConnection checks the saved connection, the server and the key.
func doctorConnection(ctx context.Context, getenv func(string) string, check func(bool, string, string), note func(string, string)) bool {
	dir := agentDir(getenv)
	cfg, err := worker.LoadAgentConfig(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		check(false, "connected", "not yet; run clipmux worker --server URL")
		return false
	case err != nil:
		check(false, "connected", err.Error())
		return false
	}
	check(true, "connected", fmt.Sprintf("as %q to %s", cfg.Name, cfg.Server))

	client := worker.DefaultHTTPClient()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(cfg.Server, "/")+"/readyz", nil)
	res, err := client.Do(req)
	if err != nil {
		check(false, "server reachable", err.Error())
		return false
	}
	res.Body.Close()
	check(res.StatusCode == http.StatusOK, "server reachable", res.Status)
	if d, err := http.ParseTime(res.Header.Get("Date")); err == nil {
		skew := time.Since(d).Round(time.Second)
		if skew < -time.Minute || skew > time.Minute {
			note("clock", fmt.Sprintf("%s off the server's; fix NTP or signed URLs may look expired", skew))
		}
	}

	body := bytes.NewReader([]byte(`{"ids":[]}`))
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(cfg.Server, "/")+"/worker/v1/sources/sync", body)
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	req.Header.Set("Content-Type", "application/json")
	if res, err = client.Do(req); err != nil {
		check(false, "key accepted", err.Error())
		return false
	}
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		check(false, "key accepted", "the server refused it: this PC was revoked; remove "+worker.ConfigPath(dir)+" and connect again")
		return false
	}
	check(res.StatusCode == http.StatusOK, "key accepted", "")

	return true
}
