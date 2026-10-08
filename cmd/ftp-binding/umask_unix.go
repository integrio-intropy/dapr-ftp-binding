//go:build !windows

package main

import "syscall"

// withUmask runs fn with the process umask set to mask, restoring the previous
// value afterwards, and reports whether it could do so.
//
// The umask is process-global and not goroutine-safe, so this is only called
// once during startup, before any goroutine that could create files is running.
func withUmask(mask int, fn func()) bool {
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	fn()
	return true
}
