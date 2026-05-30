//go:build !linux

package gui

import "time"

// fileCreatedAt: non-Linux platforms have no portable birth time here, so the
// modification time stands in.
func fileCreatedAt(_ string, modTime time.Time) time.Time {
	return modTime
}
