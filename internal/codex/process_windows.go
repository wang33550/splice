//go:build windows

package codex

import (
	"syscall"
)

// isProcessAlive on Windows uses OpenProcess with PROCESS_QUERY_LIMITED_INFORMATION
// to check whether the PID corresponds to a running process. Returning false
// here means the lock will be reclaimed; we err on the side of false positives
// (treating a process as dead when it might be alive) only when the OS itself
// can't enumerate it, which is the conservative direction.
func isProcessAlive(pid int) bool {
	return IsProcessAlive(pid)
}

// IsProcessAlive reports whether pid is currently running.
func IsProcessAlive(pid int) bool {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	// GetExitCodeProcess returns STILL_ACTIVE (259) for a running process.
	const STILL_ACTIVE = 259
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	return exitCode == STILL_ACTIVE
}
