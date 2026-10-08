//go:build windows

package main

// withUmask runs fn unchanged. Windows has no umask, and Dapr skips pluggable
// components there entirely (local development needs WSL), so the socket
// permissions are set by Chmod alone.
func withUmask(_ int, fn func()) bool {
	fn()
	return false
}
