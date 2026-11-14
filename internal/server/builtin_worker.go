package server

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/visheshgubrani/clipmux/internal/logging"
	"github.com/visheshgubrani/clipmux/internal/media"
	"github.com/visheshgubrani/clipmux/internal/sandbox"
	"github.com/visheshgubrani/clipmux/internal/worker"
)

// builtinWorker runs `clipmux worker` as a child process over loopback
// (docs/architecture.md, "Compute"). The child holds only the worker key and
// gets attempt tokens per job; it has no database or storage credentials.
type builtinWorker struct {
	server  string // loopback URL of this serve
	key     string
	workDir string
	// modelsDir keeps the speech models across restarts (the work
	// directory is cleared at each one).
	modelsDir string
	tools     media.Tools
	logger    *slog.Logger
	// unsandboxed runs the tools unconfined (CLIPMUX_UNSANDBOXED_WORKER).
	unsandboxed bool
	// sandboxHolds is set when the probe found limits, no_new_privs and
	// seccomp: an owner who opted out only for want of Landlock keeps them.
	sandboxHolds bool
	// probe reports what the sandbox enforces; sandbox.ProbeExec when nil.
	probe func(ctx context.Context, self string) (sandbox.Status, error)
}

// loopbackURL is how the child reaches serve: the listener's own address, or
// 127.0.0.1 when serve listens on every interface.
func loopbackURL(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return "http://" + addr.String()
	}
	host := tcp.IP.String()
	if tcp.IP == nil || tcp.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(tcp.Port))
}

// childEnv is the child's whole environment: PATH to find the tools, the
// tool overrides, and the worker's own settings. Nothing else is inherited.
func (b *builtinWorker) childEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"CLIPMUX_WORKER_KEY=" + b.key,
		"CLIPMUX_SERVER=" + b.server,
		"CLIPMUX_FFMPEG=" + b.tools.FFmpeg,
		"CLIPMUX_FFPROBE=" + b.tools.FFprobe,
		"CLIPMUX_PACKAGER=" + b.tools.Packager,
		"CLIPMUX_WHISPER=" + b.tools.Whisper,
		"CLIPMUX_MODELS_DIR=" + b.modelsDir,
	}
	if mem := os.Getenv("CLIPMUX_TOOL_MEMORY_MB"); mem != "" {
		env = append(env, "CLIPMUX_TOOL_MEMORY_MB="+mem)
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" {
		env = append(env, "TMPDIR="+tmp)
	}
	// A GPU on the server host encodes for the built-in worker too.
	// The child logs like serve, to the same stream.
	for _, name := range []string{logging.EnvFormat, logging.EnvLevel} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	// CLIPMUX_BAKED_MODELS_DIR: the image's models (D118), so the built-in
	// worker never downloads them into the data volume.
	for _, name := range append([]string{"CLIPMUX_ENCODER", "CLIPMUX_VAAPI_DEVICE", "CLIPMUX_SPEECH", "CLIPMUX_BAKED_MODELS_DIR"}, sandbox.EnvAllowed...) {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// args are the child's arguments. It reaches storage through serve over
// loopback, never back through the public URL, and confines its tools
// unless the owner opted out on a host where the sandbox doesn't hold.
func (b *builtinWorker) args() []string {
	args := []string{"worker", "--work-dir", b.workDir, "--storage-via-server"}
	if !b.unsandboxed || b.sandboxHolds {
		args = append(args, "--sandbox")
	}
	return args
}

// run supervises the child until ctx ends, restarting it with backoff.
func (b *builtinWorker) run(ctx context.Context) {
	if _, err := b.tools.Versions(ctx); err != nil {
		b.logger.Error("built-in worker not started: media tools are missing; uploads will wait in processing",
			"err", err, "hint", "install ffmpeg and toolchain/install_packager.sh, or set CLIPMUX_PACKAGER")
		return
	}
	self, err := os.Executable()
	if err != nil {
		b.logger.Error("built-in worker not started", "err", err)
		return
	}
	if !b.sandboxAllows(ctx, self) {
		return
	}
	backoff := time.Second
	for ctx.Err() == nil {
		// A child that was killed couldn't remove its attempt directories;
		// only this supervisor's child uses them, so start each one clean.
		if err := worker.ClearDir(b.workDir); err != nil {
			b.logger.Warn("clear the worker's scratch directory", "err", err)
		}
		started := time.Now()
		cmd := exec.CommandContext(ctx, self, b.args()...)
		cmd.Env = b.childEnv()
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 30 * time.Second
		sandbox.DieWithParent(cmd)
		b.logger.Info("built-in worker starting", "server", b.server)
		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		b.logger.Error("built-in worker exited; restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// sandboxAllows reports whether the worker may start: the tools parse
// hostile uploads, so they need the sandbox and Landlock, unless the owner
// said the host isolates the worker another way. Without Landlock a tool
// could read any file serve's user can, the data directory's keys included
// (D120).
func (b *builtinWorker) sandboxAllows(ctx context.Context, self string) bool {
	probe := b.probe
	if probe == nil {
		probe = sandbox.ProbeExec
	}
	const hint = "set CLIPMUX_UNSANDBOXED_WORKER=true if the host isolates the worker another way, or run an agent or a provider instead"
	st, err := probe(ctx, self)
	b.sandboxHolds = err == nil
	switch {
	case err != nil && b.unsandboxed:
		b.logger.Warn("built-in worker runs its media tools unconfined (CLIPMUX_UNSANDBOXED_WORKER)", "err", err)
	case err != nil:
		b.logger.Error("built-in worker not started: the media tool sandbox doesn't hold on this host; uploads will wait in processing",
			"err", err, "hint", "run on Linux (or in Docker), or "+hint)
		return false
	case st.LandlockABI == 0 && b.unsandboxed:
		b.logger.Warn("built-in worker runs without Landlock (CLIPMUX_UNSANDBOXED_WORKER): its tools can read every file serve can", "enforced", st.String())
	case st.LandlockABI == 0:
		b.logger.Error("built-in worker not started: this kernel has no Landlock, so its media tools could read serve's keys; uploads will wait in processing",
			"enforced", st.String(), "hint", "use a kernel with Landlock (5.13+, enabled in the LSM list), or "+hint)
		return false
	default:
		b.logger.Info("built-in worker sandbox", "enforced", st.String())
	}
	return true
}

func workerDir(dataDir string) string { return filepath.Join(dataDir, "worker") }
