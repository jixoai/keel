//go:build windows

package sidecar

import (
	"errors"
	"os"
	"os/exec"
)

// configureProcessGroup is a no-op on Windows: the platform has no POSIX
// process groups, so the direct process is what can be signalled.
func configureProcessGroup(*exec.Cmd) {}

func terminateProcess(process *os.Process) error {
	if process == nil {
		return errors.New("oar sidecar: no process")
	}
	return process.Kill()
}

func killProcessTree(process *os.Process) error {
	if process == nil {
		return errors.New("oar sidecar: no process")
	}
	return process.Kill()
}

func processGroupAlive(int) bool { return false }
