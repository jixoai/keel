//go:build !windows

package sidecar

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup makes a spawned runtime its own process-group leader,
// so a cancel or a crash can take the whole tree down.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateProcess asks a process group to exit, falling back to the direct
// process when the group is gone.
func terminateProcess(process *os.Process) error {
	if process == nil {
		return errors.New("oar sidecar: no process")
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}

// killProcessTree kills a process group, then the direct process as a fallback.
func killProcessTree(process *os.Process) error {
	if process == nil {
		return errors.New("oar sidecar: no process")
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err == nil {
		_ = process.Kill()
		return nil
	}
	return process.Kill()
}

// processGroupAlive reports whether any process is still in pid's group.
func processGroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(-pid, 0) == nil
}
