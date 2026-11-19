package jobqueue

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
	"github.com/jixoai/keel/internal/store"
)

// TestBackgroundJobSettlesCaptureAfterParentRunFinished is the production-shape
// proof of the job settlement lane: a real JobManager runs a command that writes
// a file, the parent run lease is settled BEFORE the job finishes, and the
// job's workspace close still enters layerfs as one fs.capture with its jobId.
// It is the case that previously produced only fs.capture_failed (the lease was
// already gone), losing the job's writes.
func TestBackgroundJobSettlesCaptureAfterParentRunFinished(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "w2-jobcap"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.EnqueueRun(ctx, tenant, session); err != nil {
		t.Fatalf("enqueue run: %v", err)
	}
	// Registered first so it runs after manager.Close (LIFO cleanup).
	t.Cleanup(func() { cleanupJobcapTenant(t, st, tenant) })

	fileStore, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatalf("open layerfs: %v", err)
	}
	t.Cleanup(func() { _ = fileStore.Close() })
	manifest, err := fileplugin.NewPostgresManifest(st.Pool())
	if err != nil {
		t.Fatalf("postgres manifest: %v", err)
	}
	kernelLog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatalf("kernel pg store: %v", err)
	}
	kern := kernel.New(kernelLog, kernel.Options{})
	scope := kernel.Scope{TenantID: tenant, SessionID: session}
	parentLease, err := kern.ClaimScope(ctx, scope, "w2-jobcap-run", time.Minute)
	if err != nil {
		t.Fatalf("claim parent run: %v", err)
	}

	var service *fileplugin.Service
	sandbox, err := bashbridge.NewLayerWorkspaces(bashbridge.LayerWorkspaceOptions{
		Store:   fileStore,
		Service: func() *fileplugin.Service { return service },
	})
	if err != nil {
		t.Fatalf("layer workspace provider: %v", err)
	}
	service, err = fileplugin.New(fileplugin.Options{
		Store:          fileStore,
		Manifest:       manifest,
		Committer:      kern,
		Leases:         kern,
		SessionSurface: sandbox,
		SnapshotSource: sandbox,
	})
	if err != nil {
		t.Fatalf("file service: %v", err)
	}
	manager, err := New(Options{
		Store:         st,
		Bridge:        bashbridge.New(bashbridge.Config{Workspace: sandbox}),
		TenantID:      tenant,
		MaxGlobal:     4,
		MaxPerGroup:   2,
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("job manager: %v", err)
	}
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start job manager: %v", err)
	}
	t.Cleanup(manager.Close)

	// The parent attempt ends before the job does (timeout-to-background): from
	// here on no run holds the fence.
	if err := kern.Finish(ctx, kernel.FinishRequest{Lease: parentLease, Cursor: 0, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish parent run: %v", err)
	}
	state, err := kern.LeaseState(ctx, scope)
	if err != nil {
		t.Fatalf("lease state: %v", err)
	}
	if state.Found || state.Live {
		t.Fatalf("parent lease is still live: found=%v live=%v", state.Found, state.Live)
	}

	jobCtx := bashbridge.WithSessionIdentity(ctx, bashbridge.SessionIdentity{
		TenantID: tenant, SessionID: session, TaskID: "task-jobcap", CallID: "call-jobcap",
	})
	result, err := manager.Run(jobCtx, tenant, session, RunRequest{
		Command: `mkdir -p /workspace/job && printf 'job-output\n' > /workspace/job/out.txt`,
		Timeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("start background job: %v", err)
	}
	if result.JobID == nil {
		t.Fatalf("background job has no id: %#v", result)
	}
	view := waitTerminal(t, manager, tenant, *result.JobID, 10*time.Second)
	if view.Status != store.BashJobExited {
		t.Fatalf("job terminal status = %q reason=%q", view.Status, view.Reason)
	}

	events, err := kern.LoadEvents(ctx, scope, 0, 1000)
	if err != nil {
		t.Fatalf("load session events: %v", err)
	}
	var captures []kernel.Event
	failed := 0
	for _, event := range events {
		switch event.Kind {
		case kernel.KindFSCapture:
			captures = append(captures, event)
		case kernel.KindFSCaptureFailed:
			failed++
		}
	}
	if failed != 0 {
		t.Fatalf("fs.capture_failed events = %d, want 0 (the job must settle on the system lane)", failed)
	}
	if len(captures) != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1", len(captures))
	}
	var payload struct {
		SessionID string `json:"sessionId"`
		JobID     string `json:"jobId"`
		TaskID    string `json:"taskId"`
		Count     int    `json:"count"`
		TreeHash  string `json:"treeHash"`
	}
	if err := json.Unmarshal(captures[0].Payload, &payload); err != nil {
		t.Fatalf("decode capture payload: %v", err)
	}
	if payload.JobID != result.JobID.String() {
		t.Fatalf("capture jobId = %q, want %s", payload.JobID, result.JobID)
	}
	if payload.SessionID != session.String() || payload.TaskID != "task-jobcap" {
		t.Fatalf("capture attribution = session %q task %q", payload.SessionID, payload.TaskID)
	}
	if payload.Count != 1 {
		t.Fatalf("capture count = %d, want the job's one file", payload.Count)
	}

	readCtx := fileplugin.WithCallID(fileplugin.WithScope(ctx, fileplugin.Scope{
		TenantID: tenant, SessionID: session,
	}), "call-jobcap-read")
	read, err := service.Read(readCtx, json.RawMessage(`{"path":"job/out.txt"}`))
	if err != nil {
		t.Fatalf("read the job's captured file from the manifest: %v", err)
	}
	if read.Content != "job-output\n" {
		t.Fatalf("captured content = %q", read.Content)
	}
	tree, ok, err := manifest.Snapshot(ctx, tenant, session, captures[0].Seq)
	if err != nil || !ok {
		t.Fatalf("session_snapshot at seq %d = %s/%v/%v", captures[0].Seq, tree, ok, err)
	}
	if tree.Prefixed() != payload.TreeHash || !tree.Valid() {
		t.Fatalf("session_snapshot tree = %q, event tree = %q", tree.Prefixed(), payload.TreeHash)
	}
	if _, err := fileStore.Tree(tree); err != nil {
		t.Fatalf("read capture tree object: %v", err)
	}
}

// cleanupJobcapTenant removes the rows this test created. It is best-effort:
// a cleanup failure is logged, not failed, because the assertions already ran.
func cleanupJobcapTenant(t *testing.T, st *store.Store, tenant uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, statement := range []string{
		`DELETE FROM bash_jobs WHERE tenant_id = $1`,
		`DELETE FROM file_manifest WHERE tenant_id = $1`,
		`DELETE FROM session_snapshot WHERE tenant_id = $1`,
		`DELETE FROM blob_refs WHERE tenant_id = $1`,
		`DELETE FROM blob_chunks WHERE tenant_id = $1`,
		`DELETE FROM blobs WHERE tenant_id = $1`,
		`DELETE FROM events WHERE tenant_id = $1`,
		`DELETE FROM runs WHERE tenant_id = $1`,
		`DELETE FROM sessions WHERE tenant_id = $1`,
		`DELETE FROM tenants WHERE tenant_id = $1`,
	} {
		if _, err := st.Pool().Exec(ctx, statement, tenant); err != nil {
			t.Logf("cleanup %q: %v", statement, err)
		}
	}
}
