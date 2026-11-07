package sandbox

import (
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
