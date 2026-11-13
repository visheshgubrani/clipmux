package sandbox

// auditArch is AUDIT_ARCH_AARCH64; arm64 has no second syscall ABI.
const (
	sysSocket            = 198
	auditArch            = 0xc00000b7
	x32Bit               = 0
	sysSchedSetscheduler = 119
	sysClone             = 220
)

// denied are the syscalls a media tool never needs: sockets (no network),
// kernel interfaces that widen the attack surface, and namespaces (clone
// with CLONE_NEW* flags is refused in filter).
var denied = map[uint32]uint32{
	198: eacces, // socket
	117: eperm,  // ptrace
	40:  eperm,  // mount
	39:  eperm,  // umount2
	217: eperm,  // add_key
	218: eperm,  // request_key
	219: eperm,  // keyctl
	241: eperm,  // perf_event_open
	270: eperm,  // process_vm_readv
	271: eperm,  // process_vm_writev
	280: eperm,  // bpf
	282: eperm,  // userfaultfd
	425: eperm,  // io_uring_setup
	426: eperm,  // io_uring_enter
	427: eperm,  // io_uring_register
	97:  eperm,  // unshare
	268: eperm,  // setns
	435: enosys, // clone3: its flags are in memory, out of seccomp's reach; glibc falls back to clone
}
