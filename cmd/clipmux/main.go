// Command clipmux is the single ClipMux binary: the control plane, the worker,
// and the CLI clients. See docs/architecture.md, "First run and packaging".
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/visheshgubrani/clipmux/internal/config"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `clipmux - open-source, self-hosted video infrastructure

Usage:
  clipmux serve     API, dashboard, scheduler and built-in delivery
  clipmux worker    run a worker (agent mode; --once for serverless)
  clipmux push      upload files through the API
  clipmux listen    forward webhooks to a local URL
  clipmux admin     host-side administration (keys, webhooks, reset-password)
  clipmux version   print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "admin":
		return admin(args[1:], os.Getenv, os.Stdin, stdout, stderr)
	case "worker":
		return runWorker(args[1:], os.Getenv, stderr)
	case "healthcheck":
		// Not in the usage: the image's HEALTHCHECK, so it needs no curl.
		return healthcheck(os.Getenv, stderr)
	case "sandbox-exec":
		// Not in the usage: the worker runs each media tool through it.
		return sandbox.Main(args[1:], stdout, stderr)
	case "push":
		return runPush(args[1:], os.Getenv, stdout, stderr)
	case "listen":
		return runListen(args[1:], os.Getenv, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "clipmux: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func serve(stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	cfg, err := config.FromEnv(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

// healthcheck asks this host's serve for /healthz on its CLIPMUX_LISTEN port.
func healthcheck(getenv func(string) string, stderr io.Writer) int {
	_, port, err := net.SplitHostPort(orEnv(getenv("CLIPMUX_LISTEN"), config.DefaultListenAddr))
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "healthcheck: HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}
