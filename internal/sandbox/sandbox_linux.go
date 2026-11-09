package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	eacces = uint32(syscall.EACCES)
	eperm  = uint32(syscall.EPERM)

	prSetNoNewPrivs = 38
	prSetSeccomp    = 22
	seccompModeFilt = 2
	schedIdle       = 5
	oPath           = 0x200000 // O_PATH, the same on amd64 and arm64

	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446
)

// systemRead are the paths every tool may read: its libraries and their
// configuration. The data directory (and so the secrets key) is not among
// them.
// gpuRead are what GPU drivers read to find their device: sysfs and the
// NVIDIA driver's /proc entries.
var gpuRead = []string{"/sys", "/proc/driver"}

// memoryLimit is the resource the memory limit applies to. CUDA and Mesa
// reserve tens of GiB of address space up front, so a GPU tool is limited
// by its data segment instead, which still bounds what it can allocate.
func memoryLimit(gpu bool) int {
	if gpu {
		return syscall.RLIMIT_DATA
	}
	return syscall.RLIMIT_AS
}

var systemRead = []string{"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/etc", "/opt", "/dev/urandom", "/dev/zero", "/sys/devices/system/cpu", "/proc/cpuinfo"}

// apply confines this process. With argv it then execs the tool and only
// returns on failure; without it reports what holds (a probe).
func apply(p Policy, argv []string) (Status, error) {
	// Thread attributes (no_new_privs, seccomp, Landlock, scheduling) are
	// per thread; the exec happens on this one.
	runtime.LockOSThread()
	var st Status
	var bin string
	if len(argv) > 0 {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return st, err
		}
		if bin, err = filepath.EvalSymlinks(path); err != nil {
			return st, err
		}
	}

	limits := []struct {
		res int
		max uint64
	}{
		{memoryLimit(p.GPU), p.MemoryMB << 20},
		{syscall.RLIMIT_CPU, p.CPUSeconds},
		{syscall.RLIMIT_FSIZE, 64 << 30},
		{syscall.RLIMIT_NOFILE, 1024},
		{syscall.RLIMIT_CORE, 0},
	}
	st.Rlimits = true
	for _, l := range limits {
		if err := syscall.Setrlimit(l.res, &syscall.Rlimit{Cur: l.max, Max: l.max}); err != nil {
			return st, fmt.Errorf("limit %d: %w", l.res, err)
		}
	}

	param := int32(0)
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
	_, _, errno := syscall.RawSyscall(sysSchedSetscheduler, 0, schedIdle, uintptr(unsafe.Pointer(&param)))
	st.Idle = errno == 0

	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return st, fmt.Errorf("no_new_privs: %w", errno)
	}
	st.NoNewPrivs = true

	reads := append(append([]string{}, systemRead...), p.Read...)
	if p.GPU {
		reads = append(reads, gpuRead...)
	}
	if bin != "" {
		reads = append(reads, filepath.Dir(bin))
	}
	// Its own /proc entry (resolved now: the pid survives the exec), never
	// other processes', whose environments may hold secrets.
	self := fmt.Sprintf("/proc/%d", os.Getpid())
	reads = append(reads, self)
	// CUDA's cuInit writes the tool's own /proc entries and fails with
	// CUDA_ERROR_OPERATING_SYSTEM without (D096); only its own, never
	// /proc/driver or /proc/sys.
	var writeFiles []string
	if p.GPU {
		writeFiles = append(writeFiles, self)
	}
	abi, err := landlock(reads, p.Write, p.Devices, writeFiles)
	if err != nil {
		return st, fmt.Errorf("landlock: %w", err)
	}
	st.LandlockABI = abi

	if err := seccomp(p.GPU); err != nil {
		return st, fmt.Errorf("seccomp: %w", err)
	}
	st.Seccomp = true

	if bin == "" {
		return st, nil
	}
	env := append([]string{}, Env(p.Env)...)
	if p.Write != "" {
		env = append(env, "TMPDIR="+p.Write)
	}
	return st, syscall.Exec(bin, argv, env)
}

// --- Landlock ---

const (
	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeChar   = 1 << 6
	fsMakeDir    = 1 << 7
	fsMakeReg    = 1 << 8
	fsMakeSock   = 1 << 9
	fsMakeFifo   = 1 << 10
	fsMakeBlock  = 1 << 11
	fsMakeSym    = 1 << 12
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3
	fsIoctlDev   = 1 << 15 // ABI 5

	netBindTCP    = 1 << 0 // ABI 4
	netConnectTCP = 1 << 1

	scopeAbstractUnix = 1 << 0 // ABI 6
	scopeSignal       = 1 << 1

	fileRights = fsExecute | fsWriteFile | fsReadFile | fsTruncate | fsIoctlDev
)

// landlock restricts the filesystem to reads beneath reads and full access
// beneath write, and on newer kernels TCP, abstract sockets and signals. It
// returns the kernel's ABI version, 0 where Landlock is missing or disabled.
// writeFiles may have their existing files read and written beneath them,
// with nothing created or removed.
func landlock(reads []string, write string, devices, writeFiles []string) (int, error) {
	v, _, errno := syscall.RawSyscall(sysLandlockCreateRuleset, 0, 0, 1 /* LANDLOCK_CREATE_RULESET_VERSION */)
	if errno != 0 {
		return 0, nil // no Landlock: reported, not required
	}
	abi := int(v)
	handled := uint64(fsExecute | fsWriteFile | fsReadFile | fsReadDir | fsRemoveDir | fsRemoveFile |
		fsMakeChar | fsMakeDir | fsMakeReg | fsMakeSock | fsMakeFifo | fsMakeBlock | fsMakeSym)
	if abi >= 2 {
		handled |= fsRefer
	}
	if abi >= 3 {
		handled |= fsTruncate
	}
	if abi >= 5 {
		handled |= fsIoctlDev
	}
	attr := [3]uint64{handled, 0, 0}
	size := uintptr(8)
	if abi >= 4 {
		attr[1] = netBindTCP | netConnectTCP // handled with no rule: no TCP at all
		size = 16
	}
	if abi >= 6 {
		attr[2] = scopeAbstractUnix | scopeSignal
		size = 24
	}
	fd, _, errno := syscall.RawSyscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), size, 0)
	if errno != 0 {
		return abi, fmt.Errorf("create ruleset: %w", errno)
	}
	defer syscall.Close(int(fd))
	read := uint64(fsExecute | fsReadFile | fsReadDir)
	for _, path := range reads {
		if err := allow(int(fd), path, read&handled); err != nil {
			return abi, err
		}
	}
	if err := allow(int(fd), "/dev/null", uint64(fsReadFile|fsWriteFile|fsTruncate)&handled); err != nil {
		return abi, err
	}
	for _, path := range writeFiles {
		if err := allow(int(fd), path, uint64(fsReadFile|fsReadDir|fsWriteFile|fsTruncate)&handled); err != nil {
			return abi, err
		}
	}
	if write != "" {
		if err := allow(int(fd), write, handled); err != nil {
			return abi, err
		}
	}
	// A GPU node is a character device: read, write and (ABI 5+) its
	// ioctls, which is how drivers talk to the GPU.
	for _, d := range devices {
		if err := allow(int(fd), d, uint64(fsReadFile|fsWriteFile|fsIoctlDev)&handled); err != nil {
			return abi, err
		}
	}
	if _, _, errno := syscall.RawSyscall(sysLandlockRestrictSelf, fd, 0, 0); errno != 0 {
		return abi, fmt.Errorf("restrict self: %w", errno)
	}
	return abi, nil
}

// allow adds a path-beneath rule; a missing path is skipped. Files take only
// file rights.
func allow(ruleset int, path string, access uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		access &= fileRights
	}
	// struct landlock_path_beneath_attr is packed: u64 allowed_access, s32 fd.
	var rule [12]byte
	*(*uint64)(unsafe.Pointer(&rule[0])) = access
	*(*int32)(unsafe.Pointer(&rule[8])) = int32(fd)
	if _, _, errno := syscall.RawSyscall6(sysLandlockAddRule, uintptr(ruleset), 1 /* LANDLOCK_RULE_PATH_BENEATH */, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0); errno != 0 {
		return fmt.Errorf("allow %s: %w", path, errno)
	}
	return nil
}

// --- seccomp ---

type sockFilter struct {
	code   uint16
	jt, jf uint8
	k      uint32
}

type sockFprog struct {
	len    uint16
	filter *sockFilter
}

const (
	bpfLdWAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJgeK   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K
	bpfAndK   = 0x54 // BPF_ALU | BPF_AND | BPF_K

	afUnix        = 1
	sockSeqpacket = 5
	sockTypeMask  = 0xf // the type without SOCK_NONBLOCK and SOCK_CLOEXEC

	retKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	retErrno = 0x00050000
	retAllow = 0x7fff0000
)

// filter is the seccomp program: kill on a foreign architecture (and x32 on
// x86-64), return an errno for each denied syscall, allow the rest.
//
// A GPU tool may also open AF_UNIX SOCK_SEQPACKET sockets: the CUDA driver
// makes them for its own IPC during cuInit, which fails without them. No
// network family is allowed, nor stream or datagram Unix sockets, the kind
// local services (Docker, D-Bus) listen on.
func filter(gpu bool) []sockFilter {
	prog := []sockFilter{
		{code: bpfLdWAbs, k: 4}, // seccomp_data.arch
		{code: bpfJeqK, jt: 1, k: auditArch},
		{code: bpfRetK, k: retKill},
		{code: bpfLdWAbs, k: 0}, // seccomp_data.nr
	}
	if x32Bit != 0 {
		prog = append(prog, sockFilter{code: bpfJgeK, jt: 0, jf: 1, k: x32Bit}, sockFilter{code: bpfRetK, k: retKill})
	}
	if gpu {
		prog = append(prog,
			sockFilter{code: bpfJeqK, jt: 0, jf: 7, k: sysSocket},
			sockFilter{code: bpfLdWAbs, k: 16}, // seccomp_data.args[0], low word: domain
			sockFilter{code: bpfJeqK, jt: 0, jf: 4, k: afUnix},
			sockFilter{code: bpfLdWAbs, k: 24}, // args[1], low word: type
			sockFilter{code: bpfAndK, k: sockTypeMask},
			sockFilter{code: bpfJeqK, jt: 0, jf: 1, k: sockSeqpacket},
			sockFilter{code: bpfRetK, k: retAllow},
			sockFilter{code: bpfRetK, k: retErrno | eacces},
		)
	}
	for nr, errno := range denied {
		prog = append(prog, sockFilter{code: bpfJeqK, jt: 0, jf: 1, k: nr}, sockFilter{code: bpfRetK, k: retErrno | errno})
	}
	return append(prog, sockFilter{code: bpfRetK, k: retAllow})
}

func seccomp(gpu bool) error {
	prog := filter(gpu)
	fprog := sockFprog{len: uint16(len(prog)), filter: &prog[0]}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilt, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return errno
	}
	runtime.KeepAlive(prog)
	return nil
}
