package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The test binary is its own helper: run with "sandbox-exec" first, it does
// what `clipmux sandbox-exec` does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(Main(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// sandboxed runs argv under p and returns its exit status, stdout and stderr.
func sandboxed(t *testing.T, p Policy, argv ...string) (int, string, string) {
	t.Helper()
	// The helper gets no PATH; callers pass the tool's absolute path.
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		t.Skipf("%s not installed", argv[0])
	}
	argv[0] = bin
	cmd := exec.Command(os.Args[0], Args(p, argv...)...)
	cmd.Env = []string{}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

func need(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}

func TestProbeReportsWhatIsEnforced(t *testing.T) {
	cmd := exec.Command(os.Args[0], "sandbox-exec", "--probe")
	cmd.Env = []string{}
	out, err := cmd.Output()
	var st Status
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		t.Fatalf("probe output %q: %v (%v)", out, jerr, err)
	}
	if !st.Enforced() || !st.Idle || err != nil {
		t.Fatalf("probe = %+v (%v); want rlimits, no_new_privs, seccomp and idle priority", st, err)
	}
	t.Logf("enforced: %s", st)
}

// A media tool has no network: socket() is refused, so it can't even try to
// connect (Permission denied, not Connection refused).
func TestSandboxedToolCannotOpenASocket(t *testing.T) {
	need(t, "bash")
	code, _, stderr := sandboxed(t, Policy{Write: t.TempDir()}, "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/9")
	if code == 0 || !strings.Contains(stderr, "Permission denied") {
		t.Fatalf("exit %d, stderr %q; want socket() refused", code, stderr)
	}
}

// Landlock confines the filesystem: the data directory (the secrets key)
// and other processes' /proc entries are out of reach; the work directory
// is writable.
func TestSandboxedToolReadsOnlyItsDirectories(t *testing.T) {
	need(t, "bash", "cat")
	if st := probe(t); st.LandlockABI == 0 {
		t.Skip("Landlock is not available on this kernel")
	}
	data := t.TempDir()
	secret := filepath.Join(data, "secrets.key")
	if err := os.WriteFile(secret, []byte("v1:secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	code, _, stderr := sandboxed(t, Policy{Write: work}, "cat", secret)
	if code == 0 || !strings.Contains(stderr, "Permission denied") {
		t.Fatalf("reading the secrets key: exit %d, %q; want Permission denied", code, stderr)
	}
	if code, _, stderr := sandboxed(t, Policy{Write: work}, "cat", "/proc/1/environ"); code == 0 {
		t.Fatalf("another process's environment was readable (%q)", stderr)
	}
	if code, _, stderr := sandboxed(t, Policy{Write: work}, "bash", "-c", "echo out > "+filepath.Join(work, "x")); code != 0 {
		t.Fatalf("writing the work directory: exit %d, %q", code, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "x")); string(b) != "out\n" {
		t.Fatalf("work file = %q", b)
	}
}

func TestMemoryLimitStopsARunawayAllocation(t *testing.T) {
	need(t, "python3")
	code, _, stderr := sandboxed(t, Policy{Write: t.TempDir(), MemoryMB: 256}, "python3", "-c", "x = bytearray(1024 * 1024 * 1024)")
	if code == 0 || !strings.Contains(stderr, "MemoryError") {
		t.Fatalf("1 GiB under a 256 MiB limit: exit %d, %q; want MemoryError", code, stderr)
	}
	if code, _, stderr := sandboxed(t, Policy{Write: t.TempDir(), MemoryMB: 256}, "python3", "-c", "x = bytearray(16 * 1024 * 1024)"); code != 0 {
		t.Fatalf("16 MiB under the limit: exit %d, %q", code, stderr)
	}
}

// Encoding runs at idle priority so it never starves the API.
func TestToolsRunAtIdlePriority(t *testing.T) {
	need(t, "cat")
	code, stdout, stderr := sandboxed(t, Policy{Write: t.TempDir()}, "cat", "/proc/self/stat")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Fields after "(comm) ": nice is field 19, policy field 41 (1-based).
	f := strings.Fields(stdout[strings.LastIndex(stdout, ")")+2:])
	nice, _ := strconv.Atoi(f[19-3])
	policy, _ := strconv.Atoi(f[41-3])
	if nice != 19 || policy != schedIdle {
		t.Fatalf("nice %d, policy %d; want 19 and SCHED_IDLE (%d)", nice, policy, schedIdle)
	}
}

func probe(t *testing.T) Status {
	t.Helper()
	cmd := exec.Command(os.Args[0], "sandbox-exec", "--probe")
	cmd.Env = []string{}
	out, _ := cmd.Output()
	var st Status
	_ = json.Unmarshal(out, &st)
	return st
}

// A GPU tool may make the AF_UNIX SOCK_SEQPACKET sockets CUDA uses
// internally, and nothing else: no network, no stream Unix sockets.
func TestGPUToolsGetCUDAsIPCSocketsAndNoOthers(t *testing.T) {
	need(t, "python3")
	try := func(gpu bool, family, kind string) int {
		code, _, _ := sandboxed(t, Policy{Write: t.TempDir(), GPU: gpu}, "python3", "-I", "-c",
			"import socket; socket.socket(socket."+family+", socket."+kind+")")
		return code
	}
	if code := try(true, "AF_UNIX", "SOCK_SEQPACKET"); code != 0 {
		t.Fatalf("a GPU tool couldn't open CUDA's AF_UNIX SOCK_SEQPACKET socket (exit %d)", code)
	}
	for _, c := range [][2]string{{"AF_INET", "SOCK_STREAM"}, {"AF_INET6", "SOCK_DGRAM"}, {"AF_UNIX", "SOCK_STREAM"}, {"AF_UNIX", "SOCK_DGRAM"}, {"AF_NETLINK", "SOCK_RAW"}} {
		if try(true, c[0], c[1]) == 0 {
			t.Errorf("a GPU tool opened %s %s", c[0], c[1])
		}
	}
	if try(false, "AF_UNIX", "SOCK_SEQPACKET") == 0 {
		t.Error("a CPU tool opened a socket")
	}
}
