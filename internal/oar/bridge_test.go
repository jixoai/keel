//go:build !windows

package oar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/gateway"
)

// sidecarBin is the OAR sidecar built once for the package tests.
var sidecarBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "keel-oar-sidecar-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "temp dir:", err)
		os.Exit(1)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	bin := filepath.Join(dir, "keel-oar")
	build := exec.Command("go", "build", "-o", bin, "./internal/oar/cmd/keel-oar")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build OAR sidecar: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	sidecarBin = bin
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fakeHarness struct {
	bridge  *Bridge
	journal *Journal
	session Session
	dir     string
}

func startFake(t *testing.T, args ...string) *fakeHarness {
	t.Helper()
	dir := t.TempDir()
	journal, err := NewJournal(filepath.Join(dir, "journal"), JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	sidecarArgs := append([]string{"sidecar", "-runtime=fake"}, args...)
	bridge, err := Dial(context.Background(), SpawnOptions{
		Command:      sidecarBin,
		Args:         sidecarArgs,
		Runtime:      RuntimeFake,
		Journal:      journal,
		RecordBuffer: 512,
		Stderr:       os.Stderr,
	})
	if err != nil {
		journal.Close()
		t.Fatalf("dial fake sidecar: %v", err)
	}
	t.Cleanup(func() {
		_ = bridge.Close()
		_ = journal.Close()
	})
	session, err := bridge.Open(context.Background(), OpenRequest{
		Reference:     "ref-1",
		HostSessionID: "host-session-1",
		TaskID:        "task-card-1",
		Workdir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return &fakeHarness{bridge: bridge, journal: journal, session: session, dir: dir}
}

// collectUntil drains the live record channel until the predicate matches or
// the deadline expires.
func collectUntil(t *testing.T, bridge *Bridge, done func(Record) bool) []Record {
	t.Helper()
	deadline := time.After(15 * time.Second)
	var records []Record
	for {
		select {
		case record := <-bridge.Records():
			records = append(records, record)
			if done(record) {
				return records
			}
		case err := <-bridge.Errors():
			t.Fatalf("record contract error: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for the record stream; got %d records", len(records))
		}
	}
}

func isFinished(record Record) bool {
	return record.Kind == KindStatus && record.Name == "turn.finished" &&
		(record.Status == StatusCompleted || record.Status == StatusFailed || record.Status == StatusCancelled)
}

func TestBridgeFakeSidecarRecordStream(t *testing.T) {
	harness := startFake(t, "-usage")
	if _, err := harness.bridge.Send(context.Background(), harness.session.Ref, json.RawMessage(`{"text":"inspect the repo"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	records := collectUntil(t, harness.bridge, isFinished)
	if len(records) < 8 {
		t.Fatalf("record count = %d, want at least 8: %+v", len(records), records)
	}
	for index, record := range records {
		if record.Seq != int64(index+1) {
			t.Fatalf("record %d seq = %d, want %d", index, record.Seq, index+1)
		}
		if record.Attribution.SessionID != "host-session-1" || record.Attribution.TaskID != "task-card-1" {
			t.Fatalf("record %d attribution = %+v", index, record.Attribution)
		}
	}
	paths := map[string]bool{}
	calls := map[string]bool{}
	results := map[string]bool{}
	var message string
	for _, record := range records {
		paths[record.AgentPath] = true
		switch record.Kind {
		case KindCall:
			calls[record.CallID] = true
		case KindResult:
			results[record.CallID] = true
		case KindMessage:
			message = record.Message
		}
		raw, _ := json.Marshal(record)
		lower := strings.ToLower(string(raw))
		if strings.Contains(lower, "task card") || strings.Contains(lower, "任务卡") || strings.Contains(lower, "please record") {
			t.Fatalf("record contains a task-card injection: %s", raw)
		}
	}
	if !paths[RootAgentPath] || !paths["/main/worker-1"] {
		t.Fatalf("agent paths = %+v, want /main and /main/worker-1", paths)
	}
	for callID := range calls {
		if !results[callID] {
			t.Fatalf("call %s has no result", callID)
		}
	}
	if !strings.Contains(message, "inspect the repo") {
		t.Fatalf("message = %q, want the exact prompt echoed", message)
	}
	projection := BuildProjection(records)
	if len(projection.Agents) != 2 {
		t.Fatalf("projection agents = %+v", projection.Agents)
	}
	if projection.HostSessionID != "host-session-1" || projection.TaskID != "task-card-1" {
		t.Fatalf("projection attribution = %+v", projection)
	}
	if !projection.Simulated {
		t.Fatal("fake projection must be marked simulated")
	}

	journalPath := harness.journal.Path(harness.session.Ref)
	persisted, err := ReadJournal(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if len(persisted) != len(records) {
		t.Fatalf("journal has %d records, live view has %d", len(persisted), len(records))
	}
	if got := harness.bridge.Session(harness.session.Ref).LastSeq; got != int64(len(records)) {
		t.Fatalf("session last seq = %d, want %d", got, len(records))
	}
}

func TestBridgeRejectsMissingCapability(t *testing.T) {
	dir := t.TempDir()
	journal, err := NewJournal(dir, JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	_, err = Dial(context.Background(), SpawnOptions{
		Command:      sidecarBin,
		Args:         []string{"sidecar", "-runtime=fake", "-capabilities=records,agent_path,calls,results,status"},
		Runtime:      RuntimeFake,
		Required:     []Capability{CapabilityCancel},
		Journal:      journal,
		RecordBuffer: 16,
	})
	if !IsCode(err, CodeUnsupportedCapability) {
		t.Fatalf("dial = %v, want unsupported_capability", err)
	}
}

func TestBridgeRejectsSimulatedRuntimeForLiveModel(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	_, err = Dial(context.Background(), SpawnOptions{
		Command:      sidecarBin,
		Args:         []string{"sidecar", "-runtime=fake"},
		Runtime:      RuntimeFake,
		Required:     []Capability{CapabilityLiveModel},
		Journal:      journal,
		RecordBuffer: 16,
	})
	if !IsCode(err, CodeSimulatedRuntime) {
		t.Fatalf("dial = %v, want simulated_runtime", err)
	}
}

func TestBridgeRejectsRuntimeMismatch(t *testing.T) {
	journal, err := NewJournal(t.TempDir(), JournalOptions{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	_, err = Dial(context.Background(), SpawnOptions{
		Command:      sidecarBin,
		Args:         []string{"sidecar", "-runtime=fake"},
		Runtime:      RuntimeCodex,
		Journal:      journal,
		RecordBuffer: 16,
	})
	if !IsCode(err, CodeProtocol) || !strings.Contains(err.Error(), "declared runtime") {
		t.Fatalf("dial = %v, want a runtime mismatch protocol error", err)
	}
}

func TestBridgeReapsSidecarProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	harness := startFake(t, "-spawn-child", "-child-pid-file", childPIDFile, "-step-delay=25ms")
	if _, err := harness.bridge.Send(context.Background(), harness.session.Ref, json.RawMessage(`{"text":"hold open"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	childPID := waitPIDFile(t, childPIDFile)
	sidecarPID := harness.bridge.PID()
	if sidecarPID <= 0 {
		t.Fatal("bridge has no sidecar pid")
	}
	if !processExists(childPID) {
		t.Fatalf("child %d did not start", childPID)
	}
	if err := harness.bridge.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !waitProcessGone(childPID, 5*time.Second) {
		t.Fatalf("child %d survived the process-group shutdown", childPID)
	}
	if !waitProcessGone(sidecarPID, 5*time.Second) {
		t.Fatalf("sidecar %d survived Close", sidecarPID)
	}
}

func TestBridgeFakeSidecarCrashesCleanly(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "crash-child.pid")
	harness := startFake(t, "-crash-after=2", "-spawn-child", "-child-pid-file", childPIDFile, "-step-delay=10ms")
	childPID := waitPIDFile(t, childPIDFile)
	if _, err := harness.bridge.Send(context.Background(), harness.session.Ref, json.RawMessage(`{"text":"crash"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The sidecar exits; the connection closes and the bridge's consumer ends.
	select {
	case <-harness.bridge.conn.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("bridge did not observe the sidecar crash")
	}
	if err := harness.bridge.Close(); err != nil {
		t.Fatalf("close after crash: %v", err)
	}
	if !waitProcessGone(childPID, 5*time.Second) {
		t.Fatalf("sidecar child %d survived the crash teardown", childPID)
	}
}

// TestServiceCodexUnavailableIsTyped pins the real-adapter failure: a missing
// Codex binary is a typed runtime_unavailable before any sidecar is spawned.
func TestServiceCodexUnavailableIsTyped(t *testing.T) {
	service, err := NewService(Config{
		Runtime:     RuntimeCodex,
		SidecarPath: sidecarBin,
		JournalDir:  t.TempDir(),
		Codex:       CodexConfig{Path: filepath.Join(t.TempDir(), "definitely-not-codex")},
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	err = service.Start(context.Background())
	if !IsCode(err, CodeRuntimeUnavailable) {
		t.Fatalf("start = %v, want runtime_unavailable", err)
	}
	if service.PID() != 0 {
		t.Fatalf("a failed preflight left a sidecar process (pid %d)", service.PID())
	}
}

func TestServiceDisabled(t *testing.T) {
	_, err := NewService(Config{})
	if !errors.Is(err, ErrDisabled) || !IsDisabled(err) {
		t.Fatalf("NewService(empty) = %v, want ErrDisabled", err)
	}
	if _, err := NewService(Config{Runtime: RuntimeFake, RequireLiveModel: true}); !IsCode(err, CodeSimulatedRuntime) {
		t.Fatalf("fake + RequireLiveModel = %v, want simulated_runtime", err)
	}
}

func TestServiceMissingSidecarIsTyped(t *testing.T) {
	service, err := NewService(Config{
		Runtime:     RuntimeFake,
		SidecarPath: filepath.Join(t.TempDir(), "missing-keel-oar"),
		JournalDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	err = service.Start(context.Background())
	if !IsCode(err, CodeRuntimeUnavailable) {
		t.Fatalf("start = %v, want runtime_unavailable", err)
	}
}

// TestExternalBackendConfigTargetsExistingGateway pins the bridge to the
// existing externalgw: the OAR sidecar is an ordinary stdio agent_backend entry
// in host.ExternalConfig, not a second external-session model.
func TestExternalBackendConfigTargetsExistingGateway(t *testing.T) {
	provider, err := (Config{Runtime: RuntimeFake, SidecarPath: sidecarBin}).ExternalBackendConfig()
	if err != nil {
		t.Fatalf("ExternalBackendConfig: %v", err)
	}
	if provider.Transport != "stdio" || provider.Command != sidecarBin {
		t.Fatalf("provider = %+v", provider)
	}
	if len(provider.Args) < 2 || provider.Args[0] != "sidecar" || provider.Args[1] != "-runtime=fake" {
		t.Fatalf("args = %v", provider.Args)
	}
	transport, err := provider.TransportKind()
	if err != nil || transport != "stdio" {
		t.Fatalf("TransportKind = %q, %v", transport, err)
	}
}

func TestServiceCodexAdapterWithFakeCodex(t *testing.T) {
	fakeCodex := writeFakeCodex(t, false)
	service, err := NewService(Config{
		Runtime:     RuntimeCodex,
		SidecarPath: sidecarBin,
		JournalDir:  t.TempDir(),
		Workdir:     t.TempDir(),
		Codex:       CodexConfig{Path: fakeCodex, Timeout: 20 * time.Second},
		Stderr:      os.Stderr,
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer service.Stop(context.Background())
	if service.Declaration().Runtime != RuntimeCodex || service.Declaration().Simulated {
		t.Fatalf("declaration = %+v", service.Declaration())
	}
	if service.Declaration().RuntimeVersion != "codex-cli 9.9.9" {
		t.Fatalf("runtime version = %q, want the probed codex version", service.Declaration().RuntimeVersion)
	}
	session, err := service.Open(context.Background(), OpenRequest{
		Reference:     "codex-ref",
		HostSessionID: "host-codex",
		TaskID:        "task-codex",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := service.Send(context.Background(), session.Ref, json.RawMessage(`{"text":"say hello"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	records := collectUntil(t, service.Bridge(), isFinished)
	if len(records) < 6 {
		t.Fatalf("records = %d: %+v", len(records), records)
	}
	var sawCall, sawResult, sawMessage, sawUsage bool
	for _, record := range records {
		if record.Attribution.SessionID != "host-codex" || record.Attribution.TaskID != "task-codex" {
			t.Fatalf("attribution = %+v", record.Attribution)
		}
		switch record.Kind {
		case KindCall:
			sawCall = record.CallID != ""
		case KindResult:
			sawResult = record.CallID != ""
		case KindMessage:
			sawMessage = strings.Contains(record.Message, "codex answer")
		case KindUsage:
			sawUsage = record.Usage != nil && record.Usage.TotalTokens == 14
		}
	}
	if !sawCall || !sawResult || !sawMessage || !sawUsage {
		t.Fatalf("mapped records missing: call=%v result=%v message=%v usage=%v (%+v)",
			sawCall, sawResult, sawMessage, sawUsage, records)
	}
	if last := records[len(records)-1]; last.Kind != KindStatus || last.Status != StatusCompleted {
		t.Fatalf("last record = %+v, want turn.finished completed", last)
	}
}

func TestServiceCodexCancelReapsProcessGroup(t *testing.T) {
	childPIDFile := filepath.Join(t.TempDir(), "codex-child.pid")
	fakeCodex := writeFakeCodex(t, true)
	service, err := NewService(Config{
		Runtime:     RuntimeCodex,
		SidecarPath: sidecarBin,
		JournalDir:  t.TempDir(),
		Workdir:     t.TempDir(),
		Codex:       CodexConfig{Path: fakeCodex, Timeout: time.Minute},
		Env:         append(gateway.MinimalEnv(os.Environ()), "CODEX_CHILD_PID_FILE="+childPIDFile),
		Stderr:      os.Stderr,
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := service.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer service.Stop(context.Background())
	session, err := service.Open(context.Background(), OpenRequest{HostSessionID: "host-cancel"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := service.Send(context.Background(), session.Ref, json.RawMessage(`{"text":"hang"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	childPID := waitPIDFile(t, childPIDFile)
	if !processExists(childPID) {
		t.Fatalf("fake codex child %d did not start", childPID)
	}
	if err := service.Cancel(context.Background(), session.Ref, "", "test cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	records := collectUntil(t, service.Bridge(), isFinished)
	last := records[len(records)-1]
	if last.Status != StatusCancelled {
		t.Fatalf("last record = %+v, want cancelled", last)
	}
	if !waitProcessGone(childPID, 5*time.Second) {
		t.Fatalf("codex child %d survived cancellation", childPID)
	}
}

// writeFakeCodex writes a deterministic `codex` executable. When hang is true
// it spawns a child (pid written to $CODEX_CHILD_PID_FILE) and waits, which is
// the cancellation/reaping observable.
func writeFakeCodex(t *testing.T, hang bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	header := `#!/bin/sh
case "$1" in
  --version)
    echo "codex-cli 9.9.9"
    exit 0
    ;;
  exec)
    if [ "$2" = "--help" ]; then
      echo "  --json  Print events to stdout as JSONL"
      exit 0
    fi
`
	body := `    printf '%s\n' '{"type":"thread.started","thread_id":"thread-test"}'
    printf '%s\n' '{"type":"turn.started"}'
    printf '%s\n' '{"type":"item.started","item":{"id":"item-1","type":"command_execution","command":"echo hi"}}'
    printf '%s\n' '{"type":"item.completed","item":{"id":"item-1","type":"command_execution","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}'
    printf '%s\n' '{"type":"item.completed","item":{"id":"item-2","type":"agent_message","text":"codex answer"}}'
`
	if hang {
		// No turn.completed before the wait: the test is about cancellation and
		// process-group reaping, not a completed turn.
		body += `    sleep 300 &
    echo $! > "$CODEX_CHILD_PID_FILE"
    wait
    `
	} else {
		body += `    printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}'
    `
	}
	script := header + body + `    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &pid); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func processExists(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processExists(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
