package bashbridge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/bashbridge"
)

// fakeJobBackend is the JobManager seam without a database: it records what the
// sandbox commands asked for and returns configured job facts.
type fakeJobBackend struct {
	jobs         []bashbridge.JobInfo
	listErr      error
	listTenant   uuid.UUID
	listLimit    int
	started      int
	startedAt    []string
	startTenant  uuid.UUID
	startSession uuid.UUID
	startCallID  string
	startResult  bashbridge.JobInfo
	startErr     error
}

func (f *fakeJobBackend) ListJobs(_ context.Context, tenantID uuid.UUID, limit int) ([]bashbridge.JobInfo, error) {
	f.listTenant, f.listLimit = tenantID, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]bashbridge.JobInfo(nil), f.jobs...), nil
}

func (f *fakeJobBackend) StartJob(ctx context.Context, tenantID, sessionID uuid.UUID, command string) (bashbridge.JobInfo, error) {
	f.started++
	f.startedAt = append(f.startedAt, command)
	f.startTenant, f.startSession = tenantID, sessionID
	identity, _ := bashbridge.SessionIdentityFromContext(ctx)
	f.startCallID = identity.CallID
	return f.startResult, f.startErr
}

func jobCommandSession(t *testing.T) (uuid.UUID, uuid.UUID, context.Context) {
	t.Helper()
	tenantID, sessionID := uuid.New(), uuid.New()
	ctx := bashbridge.WithSessionIdentity(context.Background(), bashbridge.SessionIdentity{
		TenantID: tenantID, SessionID: sessionID, TaskID: "task-jobs", CallID: "call-parent",
	})
	return tenantID, sessionID, ctx
}

// runBash executes one script in a fresh bridge session and returns its terminal
// fact plus the streamed output.
func runBash(t *testing.T, bridge *bashbridge.Bridge, ctx context.Context, script string) (bashbridge.Result, string, string) {
	t.Helper()
	session, err := bridge.NewSession(ctx)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	result, err := session.Run(ctx, bashbridge.RunSpec{Command: script, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		t.Fatalf("run %q: %v", script, err)
	}
	return result, stdout.String(), stderr.String()
}

// TestSandboxJobCommandsRenderKeelJobs pins the model-visible job views: `jobs`
// lists this session's background jobs, `ps` lists the tenant's, both render
// keel job ids and the persisted JobManager status, and neither prints a host
// pid. `jobs` must actually execute (the upstream name exists without an
// implementation, so only the injected override keeps it from answering 127).
func TestSandboxJobCommandsRenderKeelJobs(t *testing.T) {
	tenantID, sessionID, ctx := jobCommandSession(t)
	otherSession := uuid.New()
	exitCode := 0
	backend := &fakeJobBackend{jobs: []bashbridge.JobInfo{
		{
			ID: "job-current", Command: "echo current", Cwd: "/workspace",
			Status: "exited", SessionID: sessionID.String(), ExitCode: &exitCode,
			CreatedAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		},
		{
			ID: "job-other", Command: "sleep 100", Cwd: "/workspace",
			Status: "running", SessionID: otherSession.String(), Live: true,
		},
	}}
	bridge := bashbridge.New(bashbridge.Config{Commands: bashbridge.NewJobCommands(backend)})

	result, stdout, stderr := runBash(t, bridge, ctx, `jobs`)
	if result.ExitCode != 0 {
		t.Fatalf("jobs exit = %d (stderr %q)", result.ExitCode, stderr)
	}
	if !strings.Contains(stdout, "job-current") || !strings.Contains(stdout, "status=exited") || !strings.Contains(stdout, "exitCode=0") {
		t.Fatalf("jobs output = %q", stdout)
	}
	if strings.Contains(stdout, "job-other") {
		t.Fatalf("jobs leaked another session's job: %q", stdout)
	}
	if backend.listTenant != tenantID {
		t.Fatalf("jobs listed tenant %s, want %s", backend.listTenant, tenantID)
	}

	result, stdout, stderr = runBash(t, bridge, ctx, `ps`)
	if result.ExitCode != 0 {
		t.Fatalf("ps exit = %d (stderr %q)", result.ExitCode, stderr)
	}
	for _, want := range []string{"job-current", "job-other", "session=" + otherSession.String(), "status=running", "live=true"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("ps output %q misses %q", stdout, want)
		}
	}
	if strings.Contains(strings.ToLower(stdout), "pid") {
		t.Fatalf("ps printed a host-pid-shaped field: %q", stdout)
	}

	// fg/bg are not registered by the contract: they stay command-not-found.
	for _, name := range []string{"fg", "bg"} {
		result, _, stderr = runBash(t, bridge, ctx, name)
		if result.ExitCode != 127 || !strings.Contains(stderr, "command not found") {
			t.Fatalf("%s = exit %d stderr %q, want 127 command not found", name, result.ExitCode, stderr)
		}
	}
}

// TestSandboxNohupStartsBackgroundJob pins nohup's semantics: it hands the
// command to the JobManager (no host process, no real nohup), returns the keel
// job id immediately, and gives the background run its own capture identity.
func TestSandboxNohupStartsBackgroundJob(t *testing.T) {
	_, sessionID, ctx := jobCommandSession(t)
	backend := &fakeJobBackend{startResult: bashbridge.JobInfo{
		ID: "job-nohup-1", Status: "running", Command: "sleep 30", Cwd: "/workspace",
	}}
	bridge := bashbridge.New(bashbridge.Config{Commands: bashbridge.NewJobCommands(backend)})

	result, stdout, stderr := runBash(t, bridge, ctx, `nohup sleep 30`)
	if result.ExitCode != 0 {
		t.Fatalf("nohup exit = %d (stderr %q)", result.ExitCode, stderr)
	}
	if !strings.Contains(stdout, "job job-nohup-1") || !strings.Contains(stdout, "status=running") {
		t.Fatalf("nohup output = %q", stdout)
	}
	if backend.started != 1 || backend.startedAt[0] != "sleep 30" {
		t.Fatalf("started = %d %v", backend.started, backend.startedAt)
	}
	if backend.startSession != sessionID {
		t.Fatalf("nohup session = %s, want %s", backend.startSession, sessionID)
	}
	if !strings.HasPrefix(backend.startCallID, "call-parent:nohup:") || backend.startCallID == "call-parent" {
		t.Fatalf("nohup call id = %q, want a per-run suffix over the parent call id", backend.startCallID)
	}

	// argv with whitespace travels as one quoted argument, not re-split.
	result, _, stderr = runBash(t, bridge, ctx, `nohup printf %s 'a b'`)
	if result.ExitCode != 0 {
		t.Fatalf("quoted nohup exit = %d (stderr %q)", result.ExitCode, stderr)
	}
	if len(backend.startedAt) != 2 || backend.startedAt[1] != `printf %s 'a b'` {
		t.Fatalf("quoted command = %v", backend.startedAt)
	}

	// A command with no arguments is a usage error, not a silent success.
	result, _, stderr = runBash(t, bridge, ctx, `nohup`)
	if result.ExitCode != 2 || !strings.Contains(stderr, "usage: nohup") {
		t.Fatalf("nohup (no args) = exit %d stderr %q", result.ExitCode, stderr)
	}

	// Outside a keel run there is no session to own the job: fail loud.
	result, _, stderr = runBash(t, bridge, context.Background(), `nohup sleep 1`)
	if result.ExitCode == 0 || !strings.Contains(stderr, "keel session") {
		t.Fatalf("nohup outside a run = exit %d stderr %q", result.ExitCode, stderr)
	}
}

// TestSandboxJobCommandsReportBackendFailure pins the error vocabulary: a
// manager failure is a diagnostic + nonzero exit, never a command-not-found.
func TestSandboxJobCommandsReportBackendFailure(t *testing.T) {
	_, _, ctx := jobCommandSession(t)
	backend := &fakeJobBackend{listErr: errors.New("database is down")}
	bridge := bashbridge.New(bashbridge.Config{Commands: bashbridge.NewJobCommands(backend)})
	result, _, stderr := runBash(t, bridge, ctx, `jobs`)
	if result.ExitCode != 1 || !strings.Contains(stderr, "database is down") {
		t.Fatalf("jobs failure = exit %d stderr %q", result.ExitCode, stderr)
	}
}
