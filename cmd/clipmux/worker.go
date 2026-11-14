package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/visheshgubrani/clipmux/internal/dispatch"
	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

const workerUsage = `Usage:
  clipmux worker --server URL              connect this PC (approve it in the dashboard)
  clipmux worker --server URL --pair CODE  connect with a code from the Workers page
  clipmux worker                           run, once this PC is connected
  clipmux worker doctor [--offline]        check everything this PC needs (--offline: before pairing)
  CLIPMUX_WORKER_KEY=KEY clipmux worker --server URL [--once]

A serverless machine's key (cmxd_...) claims its one job and exits; its tools
are always sandboxed.

Claims and runs media jobs. A connected PC keeps its server and key in
agent.json under CLIPMUX_AGENT_DIR (default: your config directory), so the
key never shows up in a process listing. Tools are found on PATH, or set with
CLIPMUX_FFMPEG, CLIPMUX_FFPROBE, CLIPMUX_PACKAGER and CLIPMUX_WHISPER.
`

// runWorker runs `clipmux worker`.
func runWorker(args []string, getenv func(string) string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(args[1:], getenv, stderr)
	}
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, workerUsage) }
	server := fs.String("server", orEnv(getenv("CLIPMUX_SERVER"), "http://127.0.0.1:8080"), "ClipMux server URL")
	workDir := fs.String("work-dir", filepath.Join(os.TempDir(), "clipmux-worker"), "scratch directory for attempts")
	once := fs.Bool("once", false, "run at most one job, then exit (serverless)")
	viaServer := fs.Bool("storage-via-server", false, "fetch and upload signed storage URLs through --server (the built-in worker)")
	confine := fs.Bool("sandbox", false, "confine ffmpeg and the packager (Linux; agents always do unless --unsandboxed)")
	unconfined := fs.Bool("unsandboxed", false, "run an agent's tools unconfined: it then gets only its own local jobs and the owner's uploads")
	pair := fs.String("pair", "", "connect this PC with a code from the dashboard's Workers page")
	name := fs.String("name", "", "what to call this PC on the Workers page (default: its hostname)")
	encoder := fs.String("encoder", orEnv(getenv("CLIPMUX_ENCODER"), "auto"), "first encoder to try: auto, nvenc, vaapi, cuda-decode or cpu; the CPU always backs it up")
	speechFlag := fs.String("speech", orEnv(getenv("CLIPMUX_SPEECH"), "auto"), "transcribe subtitles: auto (a GPU, or a CPU with 4+ cores and 6 GiB), off, cpu, vulkan or cuda")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	serverSet := getenv("CLIPMUX_SERVER") != ""
	fs.Visit(func(f *flag.Flag) { serverSet = serverSet || f.Name == "server" })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	key := getenv("CLIPMUX_WORKER_KEY")
	agent := key == ""
	// A provider's machine (D054): one job, always confined, because it
	// takes public uploads.
	dispatched := strings.HasPrefix(key, dispatch.KeyPrefix)
	if dispatched {
		if *unconfined {
			fmt.Fprintln(stderr, "clipmux worker: a serverless worker can't run --unsandboxed")
			return 2
		}
		*once, *confine = true, true
	}
	var roots []string
	if agent {
		// Agent mode: this PC's own key, from pairing.
		cfg, code := connectAgent(ctx, agentDir(getenv), *server, serverSet, *pair, *name, stderr)
		if code != 0 {
			return code
		}
		*server, key, roots = cfg.Server, cfg.Key, cfg.Roots
		*confine = !*unconfined
	}
	tools := media.DefaultTools()
	tools.FFmpeg = orEnv(getenv("CLIPMUX_FFMPEG"), tools.FFmpeg)
	tools.FFprobe = orEnv(getenv("CLIPMUX_FFPROBE"), tools.FFprobe)
	tools.Packager = orEnv(getenv("CLIPMUX_PACKAGER"), tools.Packager)
	tools.Whisper = orEnv(getenv("CLIPMUX_WHISPER"), tools.Whisper)

	logger, err := logging.FromEnv(getenv, stderr)
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		return 1
	}
	// The worker's key is in its environment; its tools run as its user.
	if err := sandbox.Undumpable(); err != nil {
		logger.Warn("the worker stays dumpable", "err", err)
	}
	// Each tool runs from its attempt's directory with paths beneath it, so
	// a relative work dir (serve's default ./data) would name the wrong file.
	if *workDir, err = filepath.Abs(*workDir); err != nil {
		logger.Error("work dir", "err", err)
		return 1
	}
	if err := os.MkdirAll(*workDir, 0o750); err != nil {
		logger.Error("work dir", "err", err)
		return 1
	}
	w := &worker.Worker{
		Server: *server, Key: key, WorkDir: *workDir, Tools: tools, Logger: logger,
		HTTP: worker.DefaultHTTPClient(), StorageViaServer: *viaServer,
	}
	if *confine {
		self, err := os.Executable()
		if err != nil {
			logger.Error("sandbox", "err", err)
			return 1
		}
		st, err := sandbox.ProbeExec(ctx, self)
		if err != nil {
			logger.Error("the sandbox doesn't hold here; refusing to run hostile media unconfined. "+
				"Run the agent in Docker, WSL2 or a VM, or pass --unsandboxed to take only your own jobs", "err", err)
			return 1
		}
		var mem uint64
		if v := getenv("CLIPMUX_TOOL_MEMORY_MB"); v != "" {
			if mem, err = strconv.ParseUint(v, 10, 64); err != nil || mem == 0 {
				logger.Error("invalid configuration", "err", fmt.Sprintf("CLIPMUX_TOOL_MEMORY_MB %q must be a positive number of MiB", v))
				return 1
			}
		}
		w.Tools.Sandbox = &media.Sandbox{Exec: self, MemoryMB: mem}
		w.Sandbox = st.String()
		logger.Info("media tools are sandboxed", "sandbox", w.Sandbox)
	}
	if _, err := tools.Versions(ctx); err != nil {
		logger.Error("media tools are missing", "err", err)
		return 1
	}
	prefer, err := media.ParseBackend(*encoder)
	if err != nil {
		logger.Error("encoder", "err", err)
		return 2
	}
	speechMode, err := worker.ParseSpeechMode(*speechFlag)
	if err != nil {
		logger.Error("speech", "err", err)
		return 2
	}
	modelsDir := orEnv(getenv("CLIPMUX_MODELS_DIR"), filepath.Join(agentDir(getenv), "models"))
	// The image bakes the models in (D118); a PC agent has none and fetches.
	bakedModels := getenv("CLIPMUX_BAKED_MODELS_DIR")
	// Prove each GPU path with a real encode, under the same sandbox jobs
	// use, before offering it.
	var driverEnv []string
	for _, n := range sandbox.EnvAllowed {
		if v := getenv(n); v != "" {
			driverEnv = append(driverEnv, n+"="+v)
		}
	}
	w.Tools.Env = driverEnv
	probeDir := filepath.Join(*workDir, ".probe")
	if err := os.MkdirAll(probeDir, 0o750); err != nil {
		logger.Error("work dir", "err", err)
		return 1
	}
	w.Tools.HW = media.DetectHardware(ctx, w.Tools, probeDir, getenv("CLIPMUX_VAAPI_DEVICE"), prefer)
	_ = os.RemoveAll(probeDir)
	if prefer != "" && prefer != media.CPU && w.Tools.HW.Backends()[0] != prefer {
		logger.Warn("the requested encoder failed its test encode; using the others", "encoder", prefer)
	}
	logger.Info("encoders", "order", w.Tools.HW.Describe())
	for b, why := range w.Tools.HW.Failed {
		logger.Info("a GPU backend failed its test encode", "backend", b, "err", why)
	}
	// Each claim loop clears its own scratch directory at start, so the
	// media and speech loops never share one.
	speech := &worker.SpeechSlot{}
	w.Speech = speech
	sw := *w
	sw.WorkDir, sw.Kinds = filepath.Join(*workDir, "speech"), []string{"transcribe"}
	w.WorkDir = filepath.Join(*workDir, "media")
	for _, d := range []string{w.WorkDir, sw.WorkDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			logger.Error("work dir", "err", err)
			return 1
		}
	}
	setupSpeech := func(ctx context.Context) *media.Speech {
		sp := worker.SetupSpeech(ctx, w.Tools, speechMode, bakedModels, modelsDir, *workDir, w.HTTP, logger)
		speech.Set(sp)
		return sp
	}
	if *once {
		// One job, of any kind this machine can run: prove speech first.
		if setupSpeech(ctx) != nil {
			w.Kinds = append(append([]string{}, queue.MediaKinds...), "transcribe")
		}
	}
	// Woken for an encode with speech off (D118), a machine may still be
	// handed the asset's transcription next (D128): it proves speech then,
	// in the mode CLIPMUX_FOLLOW_ON_SPEECH names.
	if v := getenv("CLIPMUX_FOLLOW_ON_SPEECH"); dispatched && v != "" {
		mode, err := worker.ParseSpeechMode(v)
		if err != nil {
			logger.Warn("CLIPMUX_FOLLOW_ON_SPEECH", "err", err)
		}
		if err == nil && mode != worker.SpeechOff {
			w.FollowOn = []string{"transcribe"}
			w.FollowOnSpeech = func(ctx context.Context) *media.Speech {
				if sp := speech.Get(); sp != nil {
					return sp
				}
				sp := worker.SetupSpeech(ctx, w.Tools, mode, bakedModels, modelsDir, *workDir, w.HTTP, logger)
				speech.Set(sp)
				return sp
			}
		}
	}
	logger.Info("clipmux worker started", "server", *server, "once", *once)
	if agent {
		// push --local hands this agent files over a socket only this user
		// can open; they stay where they are.
		dir := agentDir(getenv)
		local, err := worker.OpenSources(dir)
		if err != nil {
			logger.Error("local sources", "err", err)
			return 1
		}
		w.Local = local
		ctl := &worker.Control{Worker: w, Sources: local, Roots: roots, Logger: logger}
		var wg sync.WaitGroup
		defer wg.Wait()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		wg.Go(func() {
			if err := ctl.Serve(ctx, dir); err != nil {
				logger.Warn("push --local is unavailable", "err", err)
			}
		})
		wg.Go(func() { syncSources(ctx, w, local, logger) })
		if !*once {
			wg.Go(func() { runSpeech(ctx, &sw, setupSpeech, logger) })
		}
		return runLoop(ctx, w, *once, logger, getenv)
	}
	if dispatched {
		if err := w.RunDispatched(ctx); err != nil {
			logger.Error("worker", "err", err)
			return 1
		}
		return 0
	}
	if !*once {
		var wg sync.WaitGroup
		defer wg.Wait()
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		wg.Go(func() { runSpeech(ctx, &sw, setupSpeech, logger) })
		return runLoop(ctx, w, *once, logger, getenv)
	}
	return runLoop(ctx, w, *once, logger, getenv)
}

// runSpeech proves speech off the start path (fetching the models can take
// minutes the first time, and the media loop shouldn't wait for it), then
// runs the speech claim loop until ctx ends.
func runSpeech(ctx context.Context, sw *worker.Worker, setup func(context.Context) *media.Speech, logger *slog.Logger) {
	if setup(ctx) == nil {
		return
	}
	if err := sw.Run(ctx); err != nil && !errors.Is(err, worker.ErrKeyRefused) {
		logger.Error("speech worker", "err", err)
	}
}

// syncSources forgets local sources whose assets were deleted: at start,
// then hourly.
func syncSources(ctx context.Context, w *worker.Worker, local *worker.Sources, logger *slog.Logger) {
	for {
		if err := w.SyncSources(ctx, local); err != nil && ctx.Err() == nil {
			logger.Warn("sync local sources", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func runLoop(ctx context.Context, w *worker.Worker, once bool, logger *slog.Logger, getenv func(string) string) int {
	if once {
		if _, err := w.RunOnce(ctx); err != nil {
			logger.Error("worker", "err", err)
			return 1
		}
		return 0
	}
	if err := w.Run(ctx); errors.Is(err, worker.ErrKeyRefused) && getenv("CLIPMUX_WORKER_KEY") == "" {
		logger.Error("this PC was disconnected from the server (revoked on its Workers page). " +
			"Remove " + worker.ConfigPath(agentDir(getenv)) + " and run clipmux worker --server URL to connect it again")
		return 1
	} else if err != nil {
		logger.Error("worker", "err", err)
		return 1
	}
	return 0
}

func orEnv(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
