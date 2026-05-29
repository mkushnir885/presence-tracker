//go:build linux

package gui

import (
	"time"

	"golang.org/x/sys/unix"
)

// fileCreatedAt returns the file's birth time (the "Created" column on
// the Meetings page). Plain ModTime would shift every time a file is
// rewritten — display-name PATCH rewrites the Parquet in place — which
// made the column behave like "last modified" instead of "created".
// Falls back to the supplied modTime when btime is unavailable (older
// kernels, filesystems without birth time).
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
