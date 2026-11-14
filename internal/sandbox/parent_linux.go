package sandbox

import (
	"fmt"
	"os/exec"
	"syscall"
)

// DieWithParent makes the kernel SIGKILL cmd's process when the process
// that started it dies, however it dies: a serve killed by the OOM killer
// or a container runtime takes its built-in worker with it, and a worker its
// media tools, rather than leaving them encoding unsupervised. The setting
// survives `sandbox-exec` exec-ing the tool. It is tied to the OS thread
// that starts cmd, and Go keeps that thread unless a goroutine exits while
// locked to it, which nothing that starts these processes does.
func DieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// Undumpable marks this process not dumpable (PR_SET_DUMPABLE 0): another
// process of the same user, such as an exploited media tool on a host
// without Landlock, can then neither read its /proc/<pid>/environ, which
// holds the secrets key and the database password, nor ptrace it. Children
// start dumpable again after exec, so this protects only the caller.
func Undumpable() error {
	const prSetDumpable = 4
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("prctl PR_SET_DUMPABLE: %w", errno)
	}
	return nil
}
