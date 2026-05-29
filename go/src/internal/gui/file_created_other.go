//go:build !linux

package gui

import "time"

func fileCreatedAt(_ string, modTime time.Time) time.Time {
	return modTime
}
