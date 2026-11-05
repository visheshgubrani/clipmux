package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

const workerUsage = `Usage:
  CLIPMUX_WORKER_KEY=KEY clipmux worker --server URL [--work-dir DIR] [--once]

Claims and runs media jobs. The key comes from CLIPMUX_WORKER_KEY so it never
shows up in a process listing. Tools are found on PATH, or set with
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	key := getenv("CLIPMUX_WORKER_KEY")
	if key == "" {
		fmt.Fprint(stderr, "clipmux worker: CLIPMUX_WORKER_KEY is required\n\n"+workerUsage)
		return 2
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
		HTTP: worker.DefaultHTTPClient(),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	_ = w.Run(ctx)
	return 0
}

func orEnv(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
