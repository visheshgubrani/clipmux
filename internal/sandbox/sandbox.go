// Package sandbox confines the media tools (ffmpeg, ffprobe, Shaka
// Packager), which parse hostile input (docs/architecture.md, "Worker
// sandbox"). `clipmux sandbox-exec` applies the confinement to itself and
// then execs the tool, so the tool starts already confined:
//
//   - resource limits: address space, CPU seconds, file size, open files, no
//     core dumps;
//   - nice 19 and SCHED_IDLE, so encoding gets only the CPU the API leaves;
//   - no_new_privs, so nothing it runs can gain privileges;
//   - a seccomp filter: socket() fails with EACCES (no network); ptrace,
//     bpf, io_uring, userfaultfd, perf, keyring and namespace calls (unshare,
//     setns, clone with CLONE_NEW* flags) with EPERM; clone3 with ENOSYS;
//   - Landlock, where the kernel has it: read-only system paths and the
//     tool's own directory, write access to one directory only, and on ABI 4+
//     no TCP; on ABI 6+ no signals or abstract sockets outside. A GPU tool
//     may also open the device nodes it is given and list /dev/dri, where
//     Mesa's Vulkan drivers find their GPU.
//
// It works inside Docker's default runtime, where new namespaces and cgroups
// aren't available to an unprivileged process. Limits, no_new_privs and
// seccomp are required; Landlock is reported, not required.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
)

// Policy is what a sandboxed tool may do.
type Policy struct {
	// Write is the one directory the tool may write (an attempt's).
	Write string
	// Read are extra readable paths beyond the system defaults.
	Read []string
	// Devices are GPU device nodes the tool may open read-write, such as
	// /dev/dri/renderD128 or /dev/nvidia0. Nothing else under /dev is.
	Devices []string
	// GPU swaps the address-space limit for a data-segment limit (GPU
	// drivers reserve far more address space than they use) and lets the
	// tool read /sys and /proc/driver, where drivers find their devices.
	GPU bool
	// Env are driver variables passed to the tool, such as
	// LIBVA_DRIVER_NAME; only names in EnvAllowed are kept.
	Env []string
	// MemoryMB bounds the address space; CPUSeconds the CPU time.
	MemoryMB, CPUSeconds uint64
}

// Defaults for a media tool: generous enough for a long 1080p encode.
const (
	DefaultMemoryMB   = 8 << 10
	DefaultCPUSeconds = 6 * 3600
)

// CPUBudget is the CPU time a tool may use: DefaultCPUSeconds for each CPU
// it can run on. RLIMIT_CPU counts every thread of the process, so a flat
// 6 hours would kill an 8-thread encode after 45 minutes of wall time; the
// wall clock itself is bounded by the attempt's own timeout.
func CPUBudget(cpus int) uint64 { return DefaultCPUSeconds * uint64(max(cpus, 1)) }

// Status is what the platform enforces.
type Status struct {
	Rlimits     bool `json:"rlimits"`
	NoNewPrivs  bool `json:"no_new_privs"`
	Seccomp     bool `json:"seccomp"`
	Idle        bool `json:"idle_priority"`
	LandlockABI int  `json:"landlock_abi"`
}

// Enforced reports whether the required parts hold.
func (s Status) Enforced() bool { return s.Rlimits && s.NoNewPrivs && s.Seccomp }

func (s Status) String() string {
	parts := []string{}
	for _, f := range []struct {
		name string
		ok   bool
	}{{"rlimits", s.Rlimits}, {"no_new_privs", s.NoNewPrivs}, {"seccomp", s.Seccomp}, {"idle", s.Idle}} {
		if f.ok {
			parts = append(parts, f.name)
		}
	}
	if s.LandlockABI > 0 {
		parts = append(parts, fmt.Sprintf("landlock_abi_%d", s.LandlockABI))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

// Args returns the sandbox-exec arguments that run argv under p.
func Args(p Policy, argv ...string) []string {
	args := []string{"sandbox-exec"}
	if p.Write != "" {
		args = append(args, "--write", p.Write)
	}
	for _, r := range p.Read {
		args = append(args, "--read", r)
	}
	for _, d := range p.Devices {
		args = append(args, "--device", d)
	}
	if p.GPU {
		args = append(args, "--gpu")
	}
	for _, e := range p.Env {
		args = append(args, "--env", e)
	}
	if p.MemoryMB > 0 {
		args = append(args, "--memory-mb", fmt.Sprint(p.MemoryMB))
	}
	if p.CPUSeconds > 0 {
		args = append(args, "--cpu-seconds", fmt.Sprint(p.CPUSeconds))
	}
	return append(append(args, "--"), argv...)
}

// EnvAllowed are the driver variables a tool may receive: they pick a VAAPI
// driver, a Vulkan driver (whisper.cpp) or a GPU, and carry no secrets.
var EnvAllowed = []string{
	"LIBVA_DRIVER_NAME", "LIBVA_DRIVERS_PATH", "CUDA_VISIBLE_DEVICES", "CUDA_DEVICE_ORDER",
	"VK_ICD_FILENAMES", "VK_DRIVER_FILES", "MESA_VK_DEVICE_SELECT", "GGML_VK_VISIBLE_DEVICES",
}

// Env keeps the NAME=VALUE pairs whose name is in EnvAllowed.
func Env(pairs []string) []string {
	var out []string
	for _, kv := range pairs {
		if name, _, ok := strings.Cut(kv, "="); ok && slices.Contains(EnvAllowed, name) {
			out = append(out, kv)
		}
	}
	return out
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// Main is `clipmux sandbox-exec [--write DIR] [--read PATH]… [--memory-mb N]
// [--cpu-seconds N] -- TOOL ARGS…`, or `--probe`, which applies everything
// and prints the Status as JSON. On success it never returns: the tool
// replaces it.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var p Policy
	var reads, devices, env multi
	fs.StringVar(&p.Write, "write", "", "the one writable directory")
	fs.Var(&reads, "read", "an extra readable path (repeatable)")
	fs.Var(&devices, "device", "a GPU device node the tool may open read-write (repeatable)")
	fs.Var(&env, "env", "NAME=VALUE for a GPU driver variable (repeatable; see EnvAllowed)")
	fs.BoolVar(&p.GPU, "gpu", false, "limit the data segment instead of the address space, and allow reading /sys")
	fs.Uint64Var(&p.MemoryMB, "memory-mb", DefaultMemoryMB, "address space limit")
	fs.Uint64Var(&p.CPUSeconds, "cpu-seconds", DefaultCPUSeconds, "CPU time limit")
	probe := fs.Bool("probe", false, "apply the sandbox and report what holds")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p.Read, p.Devices = reads, devices
	p.Env = Env(env)
	if *probe {
		st, err := apply(p, nil)
		if err != nil {
			fmt.Fprintf(stderr, "sandbox-exec: %v\n", err)
		}
		_ = json.NewEncoder(stdout).Encode(st)
		if !st.Enforced() {
			return 1
		}
		return 0
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "sandbox-exec: no tool to run")
		return 2
	}
	if _, err := apply(p, argv); err != nil {
		fmt.Fprintf(stderr, "sandbox-exec: %v\n", err)
		return 126
	}
	// apply execs the tool on success.
	fmt.Fprintln(stderr, "sandbox-exec: exec returned")
	return 126
}

// ProbeExec runs `self sandbox-exec --probe` and returns what holds.
func ProbeExec(ctx context.Context, self string) (Status, error) {
	cmd := exec.CommandContext(ctx, self, "sandbox-exec", "--probe")
	cmd.Env = []string{}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	var st Status
	if err := json.Unmarshal(out, &st); err != nil {
		return Status{}, fmt.Errorf("sandbox probe: %v: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	if !st.Enforced() {
		return st, fmt.Errorf("sandbox probe: only %s holds: %s", st, strings.TrimSpace(stderr.String()))
	}
	return st, nil
}

// ErrUnsupported means this platform has no sandbox.
var ErrUnsupported = errors.New("sandbox: only Linux confines the media tools; run agents in Docker, WSL2 or a VM")
