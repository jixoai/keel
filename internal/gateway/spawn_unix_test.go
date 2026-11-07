//go:build !windows

// Process-group (reaper) tests: they only make sense where POSIX process
// groups exist.

package gateway

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestKillTreeReapsTheGroupOfADeadLeader is the reaper contract a supervisor
// relies on: the leader is gone (crash), its children are alive, and the
// recorded pid still names the process group that has to be killed.
func TestKillTreeReapsTheGroupOfADeadLeader(t *testing.T) {
	stderr := &lockedBuffer{}
	transport, err := SpawnStdio(SpawnOptions{
		Command:    os.Args[0],
		Args:       []string{"-test.run=^TestGatewayHelperProcess$"},
		Env:        helperEnv(gatewayHelperOrphan + "=1"),
		Stderr:     stderr,
		CloseGrace: time.Second,
	})
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	orphan := pidFromStderr(t, stderr, "orphan child pid ")
	leader := transport.PID()
	if leader <= 0 {
		t.Fatal("the transport has no pid")
	}
	if !processGroupExists(leader) {
		t.Fatalf("the spawned provider is not its own process-group leader (pid %d)", leader)
	}
	if err := KillTree(0); err == nil {
		t.Error("KillTree(0) must be refused")
	}

	// The leader dies; the grandchild keeps the group alive.
	if err := transport.process.Kill(); err != nil {
		t.Fatalf("kill the leader: %v", err)
	}
	select {
	case <-transport.Done():
	case <-time.After(testTimeout):
		t.Fatal("the read loop did not end after the leader died")
	}
	if !waitFor(t, 2*time.Second, func() bool { return processGroupExists(leader) }) {
		t.Fatalf("expected the grandchild %d to keep group %d alive", orphan, leader)
	}
	if !processExists(orphan) {
		t.Fatalf("grandchild %d died with the leader; the test cannot prove reaping", orphan)
	}

	if err := KillTree(leader); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !processExists(orphan) }) {
		t.Fatalf("KillTree left the orphan %d alive", orphan)
	}
}
