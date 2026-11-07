// Spawn-time security tests: the provider's environment is explicit and
// fail-closed, and its process tree is what teardown kills.

package gateway

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSpawnStdioFailsClosedWithoutEnv: a nil Env would hand the provider the
// whole host environment (database URL, model keys, tunnel tokens), so it is
// refused instead of silently inherited.
func TestSpawnStdioFailsClosedWithoutEnv(t *testing.T) {
	transport, err := SpawnStdio(SpawnOptions{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestGatewayHelperProcess$"},
	})
	if err == nil {
		_ = transport.Close()
		t.Fatal("SpawnStdio with a nil Env succeeded: the provider would inherit the host environment")
	}
	if transport != nil {
		t.Fatal("SpawnStdio returned a transport alongside its error")
	}
	if !strings.Contains(err.Error(), "explicit environment") {
		t.Errorf("error = %v, want it to explain the environment requirement", err)
	}
}

func TestSpawnStdioRejectsMalformedEnvEntry(t *testing.T) {
	_, err := SpawnStdio(SpawnOptions{
		Command: os.Args[0],
		Env:     []string{"PATH=/usr/bin", "NOT_AN_ASSIGNMENT"},
	})
	if err == nil || !strings.Contains(err.Error(), "NAME=value") {
		t.Fatalf("error = %v, want a malformed-entry error", err)
	}
}

func TestSpawnStdioAcceptsAnExplicitEmptyEnv(t *testing.T) {
	// An explicit empty environment is a decision, not an accident: it must be
	// allowed (and must not be confused with nil).
	if _, err := SpawnStdio(SpawnOptions{Command: "/nonexistent-binary", Env: []string{}}); err == nil {
		t.Fatal("expected a start failure for a nonexistent command")
	} else if strings.Contains(err.Error(), "explicit environment") {
		t.Fatalf("an explicit empty env was rejected as if it were nil: %v", err)
	}
}

// TestSpawnStdioDoesNotInheritHostSecrets: the child receives exactly the
// environment it was handed. A secret that lives in the host environment (and
// therefore in os.Environ()) must not reach the provider unless it is injected
// on purpose. The values are generated at run time so the assertion cannot be
// fooled by anything already present in the environment.
func TestSpawnStdioDoesNotInheritHostSecrets(t *testing.T) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	hostSecret := "keel-host-secret-" + suffix
	providerKey := "keel-provider-key-" + suffix + "-injected"
	t.Setenv("KEEL_TEST_HOST_SECRET", hostSecret)

	stderr := &lockedBuffer{}
	env := append(MinimalEnv(os.Environ()),
		gatewayHelperEnv+"=1",
		gatewayHelperEnvDump+"=1",
		"DSH_MODEL_KEY="+providerKey,
	)
	transport, err := SpawnStdio(SpawnOptions{
		Command:    os.Args[0],
		Args:       []string{"-test.run=^TestGatewayHelperProcess$"},
		Env:        env,
		Stderr:     stderr,
		CloseGrace: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })

	dump := waitForStderr(t, stderr, "ENV PATH=")
	if strings.Contains(dump, hostSecret) || strings.Contains(dump, "KEEL_TEST_HOST_SECRET") {
		t.Fatalf("the provider inherited the host secret:\n%s", dump)
	}
	if !strings.Contains(dump, "DSH_MODEL_KEY="+providerKey) {
		t.Errorf("the explicitly injected provider variable is missing:\n%s", dump)
	}
}

// TestSpawnStdioKillsTheGrandchildTree: a provider runtime spawns children of
// its own, and Close must reap the whole tree - killing only the direct child
// leaks orphans.
func TestSpawnStdioKillsTheGrandchildTree(t *testing.T) {
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

	pid := pidFromStderr(t, stderr, "orphan child pid ")
	if pid <= 0 {
		t.Fatalf("could not read the grandchild pid from:\n%s", stderr.String())
	}
	if !processExists(pid) {
		t.Fatalf("grandchild %d is not running before Close", pid)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !processExists(pid) }) {
		t.Fatalf("grandchild %d survived transport Close: the provider tree leaks orphans", pid)
	}
}

// TestSpawnStdioSeesEOFWhenProviderCrashesWithoutClose: the parent must not
// hold the provider's stdout write end, or a crashed provider would never
// produce EOF and the connection would hang until the request timeout.
func TestSpawnStdioSeesEOFWhenProviderCrashesWithoutClose(t *testing.T) {
	transport, err := SpawnStdio(helperSpawnOptions())
	if err != nil {
		t.Fatalf("SpawnStdio: %v", err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	if err := transport.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The provider dies without us closing anything.
	if err := transport.process.Kill(); err != nil {
		t.Fatalf("kill provider: %v", err)
	}
	select {
	case <-transport.Done():
	case <-time.After(testTimeout):
		t.Fatal("the read loop never saw EOF after the provider crashed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	_, receiveErr := transport.Receive(ctx)
	if receiveErr == nil {
		t.Fatal("Receive returned nil error after the provider crashed")
	}
	if !strings.Contains(receiveErr.Error(), "exited") {
		t.Errorf("terminal error = %v, want it to name the provider exit", receiveErr)
	}
}

// waitForStderr waits until the child wrote the substring, returning the dump.
func waitForStderr(t *testing.T, stderr *lockedBuffer, substring string) string {
	t.Helper()
	if !waitFor(t, testTimeout, func() bool { return strings.Contains(stderr.String(), substring) }) {
		t.Fatalf("the child never wrote %q; stderr so far:\n%s", substring, stderr.String())
	}
	return stderr.String()
}

// pidFromStderr parses "prefix <pid>" out of the child's output.
func pidFromStderr(t *testing.T, stderr *lockedBuffer, prefix string) int {
	t.Helper()
	waitForStderr(t, stderr, prefix)
	for _, line := range strings.Split(stderr.String(), "\n") {
		index := strings.Index(line, prefix)
		if index < 0 {
			continue
		}
		fields := strings.Fields(line[index+len(prefix):])
		if len(fields) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			return pid
		}
	}
	return 0
}
