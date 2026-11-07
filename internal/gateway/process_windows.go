//go:build windows

// Windows fallbacks for the process-tree helpers.
//
// Windows has no POSIX process groups; the direct process is what can be
// signalled and killed, and children are expected to exit on stdin EOF (the
// same contract the Unix path relies on as its first teardown step).

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on Windows.
func setProcessGroup(*exec.Cmd) {}

// terminateProcessTree reports that graceful group termination is unavailable,
// so the caller falls back to its next step.
func terminateProcessTree(*os.Process) error {
	return errors.New("gateway: process groups are not available on Windows")
}

// killProcessTree kills the direct process.
func killProcessTree(process *os.Process) error {
	if process == nil {
		return errors.New("gateway: no process")
	}
	return process.Kill()
}

// KillTree kills pid.
func KillTree(pid int) error {
	if pid <= 0 {
		return errors.New("gateway: KillTree requires a positive pid")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// processGroupExists always reports false on Windows: groups are not tracked.
func processGroupExists(int) bool { return false }

// processExists reports whether pid is alive.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(handle)
	return true
}
