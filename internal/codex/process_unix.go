//go:build !windows

package codex

import (
	"os"
	"syscall"
)

// isProcessAlive on Unix sends signal 0 (no-op) which the kernel uses solely
// to validate whether the target PID exists and is signalable.
func isProcessAlive(pid int) bool {
	return IsProcessAlive(pid)
}

// IsProcessAlive reports whether pid is currently running.
func IsProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
