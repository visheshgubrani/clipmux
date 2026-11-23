//go:build unix

package server

import "syscall"

// diskSpace is the space available to this user on the filesystem holding dir,
// and its size; zeros if unknown.
func diskSpace(dir string) (free, total float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	return float64(st.Bavail) * float64(st.Bsize), float64(st.Blocks) * float64(st.Bsize)
}
