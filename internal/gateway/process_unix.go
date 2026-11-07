//go:build !windows

// Process-tree control for spawned providers.
//
// A provider runtime spawns children of its own (Node worker pools, DSH
// grandchildren), so signalling or killing only the direct child leaves the
// rest running as orphans. Every spawn is therefore placed in its own process
// group (Setpgid) and teardown targets the whole group. KillTree is the reaper
// primitive a supervisor uses to clean up a sidecar tree after the fact.

package gateway

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup makes the child a process-group leader (its own group), so
// everything it spawns can be signalled and killed together.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateProcessTree asks the provider's whole process group to exit. It
// falls back to the direct process when the group is gone.
func terminateProcessTree(process *os.Process) error {
	if process == nil {
		return errors.New("gateway: no process")
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGTERM); err == nil {
		return nil
	}
	return process.Signal(syscall.SIGTERM)
}

// killProcessTree kills the provider's whole process group, then the direct
// process as a belt-and-braces fallback.
func killProcessTree(process *os.Process) error {
	if process == nil {
		return errors.New("gateway: no process")
	}
	if err := syscall.Kill(-process.Pid, syscall.SIGKILL); err == nil {
		_ = process.Kill()
		return nil
	}
	return process.Kill()
}

// KillTree kills the process group of pid. It is the reaper primitive: a
// supervisor that recorded a sidecar pid can clean up the whole tree it left
// behind, and it is also what kills a group whose leader has already exited
// (the group id is the leader's pid, so the group outlives the leader).
func KillTree(pid int) error {
	if pid <= 0 {
		return errors.New("gateway: KillTree requires a positive pid")
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// processGroupExists reports whether any process is still in pid's process
// group. A group whose leader has been reaped still reports true while any
// descendant lives.
func processGroupExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(-pid, 0) == nil
}

// processExists reports whether pid is alive, for reamers and tests.
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
