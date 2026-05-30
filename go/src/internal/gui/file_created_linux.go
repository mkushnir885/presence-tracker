//go:build linux

package gui

import (
	"time"

	"golang.org/x/sys/unix"
)

// fileCreatedAt returns the file's birth time via statx, falling back to
// modTime when the filesystem doesn't record one (the btime mask is unset).
func fileCreatedAt(path string, modTime time.Time) time.Time {
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &stx); err != nil {
		return modTime
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return modTime
	}
	return time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec))
}
