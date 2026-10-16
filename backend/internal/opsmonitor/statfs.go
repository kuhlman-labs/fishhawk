//go:build darwin || linux

package opsmonitor

import "syscall"

// statfsBytes returns the bytes available to an unprivileged user (f_bavail)
// and the volume's total bytes (f_blocks), both scaled by f_bsize.
func statfsBytes(path string) (avail, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// Field widths differ per platform (Bsize is int64 on linux, uint32 on
	// darwin), so every operand is widened explicitly.
	bsize := uint64(st.Bsize)
	return uint64(st.Bavail) * bsize, uint64(st.Blocks) * bsize, nil
}
