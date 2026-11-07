package sandbox

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// dieWithParentHelper is the middle process of TestChildDiesWithItsParent:
// it starts sleep with DieWithParent, prints the child's PID and waits.
func dieWithParentHelper() {
	cmd := exec.Command("sleep", "60")
	DieWithParent(cmd)
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	os.Stdout.WriteString(strconv.Itoa(cmd.Process.Pid) + "\n")
	_ = cmd.Wait()
	os.Exit(0)
}

// A serve killed with SIGKILL (OOM, container runtime) must not leave its
// built-in worker, or a worker its ffmpeg, running unsupervised.
func TestChildDiesWithItsParent(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not installed")
	}
	parent := exec.Command(os.Args[0], "die-with-parent-helper")
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("child pid %q: %v", line, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive(child) {
			return
		}
	}
	t.Fatalf("child %d still running 5s after its parent was killed", child)
}

// alive reports whether pid runs; a zombie waiting for its reaper doesn't.
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(after, "Z")
}
