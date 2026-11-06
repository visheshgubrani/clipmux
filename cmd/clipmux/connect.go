package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/visheshgubrani/clipmux/internal/worker"
)

// agentDir is where a connected PC keeps agent.json.
func agentDir(getenv func(string) string) string {
	if d := getenv("CLIPMUX_AGENT_DIR"); d != "" {
		return d
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "clipmux")
	}
	return ".clipmux"
}

// connectAgent returns this PC's agent config: the saved one, or a new one
// from pairing. With --pair it redeems a dashboard code; with only --server
// it asks for a code, opens the dashboard's connect page in this machine's
// browser, and waits for the owner to approve, so connecting the PC you're
// sitting at is one command and one click.
func connectAgent(ctx context.Context, dir, server string, serverSet bool, code, name string, out io.Writer) (worker.AgentConfig, int) {
	saved, err := worker.LoadAgentConfig(dir)
	switch {
	case err == nil && code == "":
		if serverSet && !sameServer(saved.Server, server) {
			fmt.Fprintf(out, "clipmux worker: this PC is connected to %s, not %s.\n"+
				"Remove %s to connect it elsewhere.\n", saved.Server, server, worker.ConfigPath(dir))
			return worker.AgentConfig{}, 2
		}
		return saved, 0
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(out, "clipmux worker: %v\n", err)
		return worker.AgentConfig{}, 1
	case !serverSet:
		fmt.Fprint(out, "clipmux worker: this PC isn't connected yet; give the server's URL with --server\n\n"+workerUsage)
		return worker.AgentConfig{}, 2
	}

	client := worker.DefaultHTTPClient()
	var cfg worker.AgentConfig
	if code != "" {
		cfg, err = worker.Redeem(ctx, client, server, code, name)
	} else {
		var p worker.PairingRequest
		if p, err = worker.StartPairing(ctx, client, server, name); err == nil {
			fmt.Fprintf(out, "\n  To connect this PC, approve code %s in the dashboard:\n\n      %s\n\n", p.Code, p.VerifyURL)
			if openBrowser(p.VerifyURL) {
				fmt.Fprintln(out, "  (opened in your browser; waiting for approval…)")
			} else {
				fmt.Fprintln(out, "  Waiting for approval…")
			}
			cfg, err = worker.AwaitApproval(ctx, client, server, p)
		}
	}
	if err != nil {
		fmt.Fprintf(out, "clipmux worker: couldn't connect to %s: %v\n", server, err)
		return worker.AgentConfig{}, 1
	}
	if err := worker.SaveAgentConfig(dir, cfg); err != nil {
		fmt.Fprintf(out, "clipmux worker: %v\n", err)
		return worker.AgentConfig{}, 1
	}
	fmt.Fprintf(out, "  Connected as %q. This PC now encodes for %s.\n\n", cfg.Name, cfg.Server)
	return cfg, 0
}

func sameServer(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// openBrowser opens url on a desktop, best effort. In a container or over
// SSH there is no browser, and the printed link is enough.
func openBrowser(url string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		cmd = exec.Command("xdg-open", url)
	default:
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	go func() { _ = cmd.Wait() }() // reap it; the browser outlives us
	return true
}
