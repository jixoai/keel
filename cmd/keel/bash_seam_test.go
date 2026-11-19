package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/jobqueue"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/tasklog"
)

// fakeBashJobManager is the command adapter's view of *jobqueue.Manager.
type fakeBashJobManager struct {
	views      []jobqueue.JobView
	listErr    error
	listTenant uuid.UUID
	listLimit  int
	runTenant  uuid.UUID
	runSession uuid.UUID
	runRequest jobqueue.RunRequest
	runResult  jobqueue.RunResult
}

func (f *fakeBashJobManager) List(_ context.Context, tenantID uuid.UUID, limit int) ([]jobqueue.JobView, error) {
	f.listTenant, f.listLimit = tenantID, limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.views, nil
}

func (f *fakeBashJobManager) Run(_ context.Context, tenantID, sessionID uuid.UUID, req jobqueue.RunRequest) (jobqueue.RunResult, error) {
	f.runTenant, f.runSession, f.runRequest = tenantID, sessionID, req
	return f.runResult, nil
}

// TestBashJobCommandsAdapterMapsJobManagerFacts pins that the sandbox view is
// the JobManager's persisted fact: ids, status, session, exit code and
// timestamps travel unchanged, and no host process is involved.
func TestBashJobCommandsAdapterMapsJobManagerFacts(t *testing.T) {
	tenantID, sessionID := uuid.New(), uuid.New()
	jobID := uuid.New()
	exitCode := 7
	created := time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC)
	fake := &fakeBashJobManager{views: []jobqueue.JobView{{
		JobID: jobID, SessionID: &sessionID, Command: "make test", Cwd: "/workspace",
		Status: store.BashJobExited, Reason: "killed", ExitCode: &exitCode,
		CreatedAt: created, Live: false,
	}}}
	adapter := bashJobCommands{manager: func() bashJobManager { return fake }}
	jobs, err := adapter.ListJobs(context.Background(), tenantID, 7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if fake.listTenant != tenantID || fake.listLimit != 7 {
		t.Fatalf("manager list args = %s/%d", fake.listTenant, fake.listLimit)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	got := jobs[0]
	if got.ID != jobID.String() || got.Command != "make test" || got.Cwd != "/workspace" ||
		got.Status != store.BashJobExited || got.Reason != "killed" || got.Live {
		t.Fatalf("job mapping = %+v", got)
	}
	if got.SessionID != sessionID.String() || got.ExitCode == nil || *got.ExitCode != exitCode || !got.CreatedAt.Equal(created) {
		t.Fatalf("job mapping fields = %+v", got)
	}

	// A manager that is not wired answers an error instead of pretending there
	// are no jobs.
	if _, err := (bashJobCommands{}).ListJobs(context.Background(), tenantID, 1); err == nil {
		t.Fatal("unwired manager listed jobs")
	}
}

// TestBashJobCommandsAdapterStartJobIsBackground pins nohup's contract: the
// manager is asked for a background job (positive timeout), not a synchronous
// execution, and the returned fact carries the durable job id.
func TestBashJobCommandsAdapterStartJobIsBackground(t *testing.T) {
	tenantID, sessionID := uuid.New(), uuid.New()
	jobID := uuid.New()
	fake := &fakeBashJobManager{runResult: jobqueue.RunResult{
		JobID: &jobID, Status: store.BashJobRunning, Cwd: "/workspace", TimedOut: true,
	}}
	adapter := bashJobCommands{manager: func() bashJobManager { return fake }}
	job, err := adapter.StartJob(context.Background(), tenantID, sessionID, "sleep 30")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if fake.runTenant != tenantID || fake.runSession != sessionID {
		t.Fatalf("manager run scope = %s/%s", fake.runTenant, fake.runSession)
	}
	if fake.runRequest.Command != "sleep 30" || fake.runRequest.Timeout <= 0 {
		t.Fatalf("manager run request = %+v, want a background timeout", fake.runRequest)
	}
	if job.ID != jobID.String() || job.Status != store.BashJobRunning || !job.Live || job.SessionID != sessionID.String() {
		t.Fatalf("start job = %+v", job)
	}
}

// TestBashJobCommandsAdapterMatchesTheToolSurfaceJobFacts is the consistency
// check against a real JobManager: a job started through the sandbox adapter
// (nohup's path) is visible through the adapter's list with exactly the facts
// the tool surface's jobs action reads from manager.List. Skipped without
// KEEL_TEST_DSN because job facts are Postgres rows.
func TestBashJobCommandsAdapterMatchesTheToolSurfaceJobFacts(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 4, MinConns: 1})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	tenantID, sessionID := uuid.New(), uuid.New()
	manager, err := jobqueue.New(jobqueue.Options{
		Store:         st,
		Bridge:        bashbridge.New(bashbridge.Config{}),
		TenantID:      tenantID,
		MaxGlobal:     4,
		MaxPerGroup:   2,
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("job manager: %v", err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer manager.Close()
	adapter := bashJobCommands{manager: func() bashJobManager { return manager }}

	started, err := adapter.StartJob(ctx, tenantID, sessionID, "sleep 30")
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if started.ID == "" {
		t.Fatal("the background job has no id")
	}
	jobID, err := uuid.Parse(started.ID)
	if err != nil {
		t.Fatalf("job id %q: %v", started.ID, err)
	}
	// The job stays running while both views are read, so the comparison is not
	// a race between a terminal transition and the second list.
	defer func() {
		if _, err := manager.Kill(context.Background(), tenantID, jobID); err != nil {
			t.Logf("kill test job: %v", err)
		}
	}()
	jobs, err := adapter.ListJobs(ctx, tenantID, 100)
	if err != nil {
		t.Fatalf("list adapter jobs: %v", err)
	}
	var sandboxFact *bashbridge.JobInfo
	for index := range jobs {
		if jobs[index].ID == started.ID {
			sandboxFact = &jobs[index]
		}
	}
	if sandboxFact == nil {
		t.Fatalf("job %s is missing from the sandbox view (%+v)", started.ID, jobs)
	}
	views, err := manager.List(ctx, tenantID, 100)
	if err != nil {
		t.Fatalf("list tool-surface jobs: %v", err)
	}
	var toolFact *jobqueue.JobView
	for index := range views {
		if views[index].JobID.String() == started.ID {
			toolFact = &views[index]
		}
	}
	if toolFact == nil {
		t.Fatalf("job %s is missing from the tool-surface view", started.ID)
	}
	if sandboxFact.Status != toolFact.Status || sandboxFact.Command != toolFact.Command ||
		sandboxFact.SessionID != sessionID.String() || sandboxFact.Live != toolFact.Live {
		t.Fatalf("sandbox fact %+v != tool fact %+v", sandboxFact, toolFact)
	}
}

type recordingBashBackend struct {
	ctx    context.Context
	action string
	args   json.RawMessage
}

func (b *recordingBashBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	b.ctx, b.action, b.args = ctx, action, args
	return agentcore.AgentToolResult{}, nil
}

// TestBashSessionBackendStampsRunIdentity pins the serve wiring that connects
// the task gate to the workspace provider: the run scope (tenant/session), the
// task-gate-stamped taskId and the ambient tool-call id reach the bridge context
// as one identity, unchanged.
func TestBashSessionBackendStampsRunIdentity(t *testing.T) {
	tenantID, sessionID := uuid.New(), uuid.New()
	ctx := agentext.WithRunScope(context.Background(), agentext.RunScope{
		TenantID: tenantID, SessionID: sessionID, RunID: "run-1",
	})
	ctx = tasklog.WithCallID(ctx, "call-bash-1")
	inner := &recordingBashBackend{}
	backend := bashSessionBackend{inner: inner}

	args := json.RawMessage(`{"command":"true","taskId":"task-9"}`)
	if _, err := backend.Invoke(ctx, "run", args); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if inner.action != "run" || string(inner.args) != string(args) {
		t.Fatalf("delegated action/args = %q/%s", inner.action, inner.args)
	}
	identity, ok := bashbridge.SessionIdentityFromContext(inner.ctx)
	if !ok {
		t.Fatal("the bridge context carries no session identity")
	}
	if identity.TenantID != tenantID || identity.SessionID != sessionID {
		t.Fatalf("identity scope = %s/%s, want %s/%s", identity.TenantID, identity.SessionID, tenantID, sessionID)
	}
	if identity.TaskID != "task-9" {
		t.Fatalf("identity taskId = %q, want the task-gate stamp", identity.TaskID)
	}
	if identity.CallID != "call-bash-1" {
		t.Fatalf("identity callId = %q, want the tool-call id", identity.CallID)
	}
}

// TestBashSessionBackendPassesThroughOutsideARun pins the fallback: a call with
// no run scope (the plugin UI bridge) is delegated untouched, so no identity is
// invented for it and the provider uses its throwaway workspace.
func TestBashSessionBackendPassesThroughOutsideARun(t *testing.T) {
	inner := &recordingBashBackend{}
	backend := bashSessionBackend{inner: inner}
	if _, err := backend.Invoke(context.Background(), "run", json.RawMessage(`{"command":"true"}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if _, ok := bashbridge.SessionIdentityFromContext(inner.ctx); ok {
		t.Fatal("an identity was invented outside a run")
	}
}
