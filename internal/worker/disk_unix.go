//go:build unix

package worker

import "syscall"

// freeDisk is the space available to this user under dir, or 0 if unknown.
func freeDisk(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
