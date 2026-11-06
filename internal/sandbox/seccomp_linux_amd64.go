package sandbox

// auditArch is AUDIT_ARCH_X86_64; x32 syscalls (bit 30) are refused too.
const (
	sysSocket            = 41
	auditArch            = 0xc000003e
	x32Bit               = 0x40000000
	sysSchedSetscheduler = 144
)

// denied are the syscalls a media tool never needs: sockets (no network),
// and kernel interfaces that widen the attack surface.
var denied = map[uint32]uint32{
	41:  eacces, // socket
	101: eperm,  // ptrace
	165: eperm,  // mount
	166: eperm,  // umount2
	248: eperm,  // add_key
	249: eperm,  // request_key
	250: eperm,  // keyctl
	298: eperm,  // perf_event_open
	310: eperm,  // process_vm_readv
	311: eperm,  // process_vm_writev
	321: eperm,  // bpf
	323: eperm,  // userfaultfd
	425: eperm,  // io_uring_setup
	426: eperm,  // io_uring_enter
	427: eperm,  // io_uring_register
}
