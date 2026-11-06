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
	"syscall"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

const workerUsage = `Usage:
  clipmux worker --server URL              connect this PC (approve it in the dashboard)
  clipmux worker --server URL --pair CODE  connect with a code from the Workers page
  clipmux worker                           run, once this PC is connected
  CLIPMUX_WORKER_KEY=KEY clipmux worker --server URL [--once]

Claims and runs media jobs. A connected PC keeps its server and key in
agent.json under CLIPMUX_AGENT_DIR (default: your config directory), so the
key never shows up in a process listing. Tools are found on PATH, or set with
CLIPMUX_FFMPEG, CLIPMUX_FFPROBE and CLIPMUX_PACKAGER.
`

// runWorker runs `clipmux worker`.
func runWorker(args []string, getenv func(string) string, stderr io.Writer) int {
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	serverSet := getenv("CLIPMUX_SERVER") != ""
	fs.Visit(func(f *flag.Flag) { serverSet = serverSet || f.Name == "server" })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	key := getenv("CLIPMUX_WORKER_KEY")
	if key == "" {
		// Agent mode: this PC's own key, from pairing.
		cfg, code := connectAgent(ctx, agentDir(getenv), *server, serverSet, *pair, *name, stderr)
		if code != 0 {
			return code
		}
		*server, key = cfg.Server, cfg.Key
		*confine = !*unconfined
	}
	tools := media.DefaultTools()
	tools.FFmpeg = orEnv(getenv("CLIPMUX_FFMPEG"), tools.FFmpeg)
	tools.FFprobe = orEnv(getenv("CLIPMUX_FFPROBE"), tools.FFprobe)
	tools.Packager = orEnv(getenv("CLIPMUX_PACKAGER"), tools.Packager)

	logger := slog.New(slog.NewTextHandler(stderr, nil))
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
		mem, _ := strconv.ParseUint(getenv("CLIPMUX_TOOL_MEMORY_MB"), 10, 64)
		w.Tools.Sandbox = &media.Sandbox{Exec: self, MemoryMB: mem}
		w.Sandbox = st.String()
		logger.Info("media tools are sandboxed", "sandbox", w.Sandbox)
	}
	if _, err := tools.Versions(ctx); err != nil {
		logger.Error("media tools are missing", "err", err)
		return 1
	}
	logger.Info("clipmux worker started", "server", *server, "once", *once)
	if *once {
		if _, err := w.RunOnce(ctx); err != nil {
			logger.Error("worker", "err", err)
			return 1
		}
		return 0
	}
	if err := w.Run(ctx); errors.Is(err, worker.ErrKeyRefused) && key != getenv("CLIPMUX_WORKER_KEY") {
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
