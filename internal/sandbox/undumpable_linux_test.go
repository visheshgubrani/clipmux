package sandbox

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// undumpableHelper is the target of TestUndumpableHidesEnviron: it calls
// Undumpable when asked, prints its PID and waits for stdin to close.
func undumpableHelper(undumpable bool) {
	if undumpable {
		if err := Undumpable(); err != nil {
			os.Exit(2)
		}
	}
	os.Stdout.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// readEnviron starts the helper and returns what reading its environ gave.
func readEnviron(t *testing.T, undumpable bool) error {
	t.Helper()
	arg := "dumpable-helper"
	if undumpable {
		arg = "undumpable-helper"
	}
	cmd := exec.Command(os.Args[0], arg)
	cmd.Env = []string{"CLIPMUX_SECRETS_KEY=secret"}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("read helper pid: %v", err)
	}
	_, err = os.ReadFile("/proc/" + strings.TrimSpace(line) + "/environ")
	return err
}

// A media tool running as serve's user must not read serve's environment,
// which holds the secrets key, even where Landlock is missing (finding 1).
func TestUndumpableHidesEnviron(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any process's environ")
	}
	if err := readEnviron(t, false); err != nil {
		t.Skipf("a dumpable process's environ is unreadable here: %v", err)
	}
	if err := readEnviron(t, true); !os.IsPermission(err) {
		t.Fatalf("read an undumpable process's environ: err = %v, want permission denied", err)
	}
}
