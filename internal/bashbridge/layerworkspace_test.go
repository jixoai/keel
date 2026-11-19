package bashbridge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/bashbridge"
	"github.com/jixoai/keel/internal/fileplugin"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/layerfs"
)

// seamHarness is the production wiring under test: one layerfs store, one file
// service whose execution space is the bash bridge's workspace provider, and a
// bridge configured with that provider. The only pieces replaced are the
// transport of the facts (in-memory kernel + in-memory manifest), not the seam.
type seamHarness struct {
	store    *layerfs.Store
	mem      *kernel.MemStore
	kernel   *kernel.Kernel
	manifest *fileplugin.MemoryManifest
	service  *fileplugin.Service
	provider *bashbridge.LayerWorkspaces
	bridge   *bashbridge.Bridge
	scope    fileplugin.Scope
	ctx      context.Context
	lease    kernel.Lease
}

func newSeamHarness(t *testing.T) *seamHarness {
	t.Helper()
	store, err := layerfs.Open(t.TempDir(), layerfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mem := kernel.NewMemStore()
	tenantID, sessionID := uuid.New(), uuid.New()
	mem.CreateSession(tenantID, sessionID)
	mem.EnqueueRun(tenantID, sessionID)
	ctx := context.Background()
	kern := kernel.New(mem, kernel.Options{})
	// One hour keeps the fence live across the test without weakening the
	// production check (capture commits as the owner, not as a system fact).
	lease, err := kern.ClaimScope(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, "seam-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fileplugin.NewMemoryManifest()
	var service *fileplugin.Service
	provider, err := bashbridge.NewLayerWorkspaces(bashbridge.LayerWorkspaceOptions{
		Store:   store,
		Service: func() *fileplugin.Service { return service },
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err = fileplugin.New(fileplugin.Options{
		Store:          store,
		Manifest:       manifest,
		Committer:      kern,
		Leases:         kern,
		SessionSurface: provider,
		SnapshotSource: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &seamHarness{
		store: store, mem: mem, kernel: kern, manifest: manifest, service: service,
		provider: provider,
		bridge:   bashbridge.New(bashbridge.Config{Workspace: provider}),
		scope:    fileplugin.Scope{TenantID: tenantID, SessionID: sessionID},
		ctx:      ctx, lease: lease,
	}
}

func (h *seamHarness) fileCtx(callID string) context.Context {
	return fileplugin.WithCallID(fileplugin.WithScope(h.ctx, h.scope), callID)
}

func (h *seamHarness) bashCtx(callID string) context.Context {
	return bashbridge.WithSessionIdentity(h.ctx, bashbridge.SessionIdentity{
		TenantID:  h.scope.TenantID,
		SessionID: h.scope.SessionID,
		TaskID:    "task-seam",
		CallID:    callID,
	})
}

// bashJobCtx is a background job's execution identity: the job id makes the
// workspace settlement a system fact.
func (h *seamHarness) bashJobCtx(callID, jobID string) context.Context {
	return bashbridge.WithSessionIdentity(h.ctx, bashbridge.SessionIdentity{
		TenantID:  h.scope.TenantID,
		SessionID: h.scope.SessionID,
		TaskID:    "task-seam",
		CallID:    callID,
		JobID:     jobID,
	})
}

// releaseRunLease settles the parent run attempt, exactly as the runtime does
// when a timeout turns a tool call into a background job.
func (h *seamHarness) releaseRunLease(t *testing.T) {
	t.Helper()
	if err := h.kernel.Release(h.ctx, h.lease); err != nil {
		t.Fatalf("release the run lease: %v", err)
	}
	state, err := h.kernel.LeaseState(h.ctx, kernel.Scope{TenantID: h.scope.TenantID, SessionID: h.scope.SessionID})
	if err != nil {
		t.Fatalf("read lease state: %v", err)
	}
	if state.Live {
		t.Fatal("the run lease is still live; the test cannot prove the system lane")
	}
}

func (h *seamHarness) events(t *testing.T) []kernel.Event {
	t.Helper()
	events, err := h.kernel.LoadEvents(h.ctx, kernel.Scope{TenantID: h.scope.TenantID, SessionID: h.scope.SessionID}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func (h *seamHarness) captures(t *testing.T, kind kernel.EventKind) []kernel.Event {
	t.Helper()
	out := make([]kernel.Event, 0)
	for _, event := range h.events(t) {
		if event.Kind == kind {
			out = append(out, event)
		}
	}
	return out
}

func mustArgs(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestProductionWiringWriteThenBashRead is the assembly-level proof that the
// commit→write-through chain and the bash mount are one view: a file-tool write
// (committed fs.write, written through the session workspace) is readable by the
// very next bash call, and close settles it as one fs.capture. The workspace is
// the provider's, not a hand-built fixture.
func TestProductionWiringWriteThenBashRead(t *testing.T) {
	h := newSeamHarness(t)
	written, err := h.service.Write(h.fileCtx("call-w1"), mustArgs(t, map[string]any{
		"path": "src/a.txt", "content": "tool-wrote-this",
	}))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !written.Created {
		t.Fatalf("write = %+v", written)
	}

	session, err := h.bridge.NewSession(h.bashCtx("call-b1"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	var stdout strings.Builder
	result, err := session.Run(h.bashCtx("call-b1"), bashbridge.RunSpec{
		Command: `cat src/a.txt`,
		Stdout:  &stdout,
	})
	if err != nil {
		session.Close()
		t.Fatalf("bash run: %v", err)
	}
	if result.ExitCode != 0 || stdout.String() != "tool-wrote-this" {
		session.Close()
		t.Fatalf("bash read after write-through = %q (exit %d)", stdout.String(), result.ExitCode)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close (capture): %v", err)
	}

	captures := h.captures(t, kernel.KindFSCapture)
	if len(captures) != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1", len(captures))
	}
	var payload map[string]any
	if err := json.Unmarshal(captures[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["count"].(float64) != 1 {
		t.Fatalf("capture payload = %v, want the one tool-written file", payload)
	}
	if payload["taskId"] != "task-seam" {
		t.Fatalf("capture taskId = %v", payload["taskId"])
	}
	if payload["baseSeq"].(float64) != 0 {
		t.Fatalf("capture baseSeq = %v", payload["baseSeq"])
	}
}

// TestBashWriteCapturedIntoManifestAndTree proves the reverse direction: a bash
// write lands in the same workspace index, and closing the run commits one
// fs.capture whose count/tree hash the manifest and session_snapshot agree on.
// The sandbox layout (/bin, /usr/bin, /tmp, virtual command stubs) stays out of
// the capture.
func TestBashWriteCapturedIntoManifestAndTree(t *testing.T) {
	h := newSeamHarness(t)
	session, err := h.bridge.NewSession(h.bashCtx("call-b2"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	result, err := session.Run(h.bashCtx("call-b2"), bashbridge.RunSpec{
		Command: `mkdir -p /workspace/out && printf 'from-bash\n' > /workspace/out/b.txt`,
	})
	if err != nil {
		session.Close()
		t.Fatalf("bash run: %v", err)
	}
	if result.ExitCode != 0 {
		session.Close()
		t.Fatalf("bash write exit = %d", result.ExitCode)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close (capture): %v", err)
	}

	read, err := h.service.Read(h.fileCtx("call-r1"), mustArgs(t, map[string]any{"path": "out/b.txt"}))
	if err != nil {
		t.Fatalf("read captured bash file: %v", err)
	}
	if read.Content != "from-bash\n" {
		t.Fatalf("captured content = %q", read.Content)
	}

	captures := h.captures(t, kernel.KindFSCapture)
	if len(captures) != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1", len(captures))
	}
	var payload struct {
		Count    int    `json:"count"`
		TreeHash string `json:"treeHash"`
		Seq      int64  `json:"seq"`
	}
	if err := json.Unmarshal(captures[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Count != 1 {
		t.Fatalf("capture count = %d, want 1 (no sandbox layout may leak into layerfs)", payload.Count)
	}
	tree, ok, err := h.manifest.Snapshot(h.ctx, h.scope.TenantID, h.scope.SessionID, captures[0].Seq)
	if err != nil || !ok {
		t.Fatalf("manifest snapshot at seq %d = %s/%v/%v", captures[0].Seq, tree, ok, err)
	}
	if tree.Prefixed() != payload.TreeHash {
		t.Fatalf("session_snapshot tree = %s, event tree = %s", tree.Prefixed(), payload.TreeHash)
	}
	if !tree.Valid() {
		t.Fatalf("capture tree %q is not a layerfs hash", payload.TreeHash)
	}
	// The captured tree is resolvable from the CAS (the event points at a real
	// tree object, not just a hash string).
	entries, err := h.store.Tree(tree)
	if err != nil {
		t.Fatalf("read capture tree object: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "out" || entries[0].Kind != layerfs.TreeDir {
		t.Fatalf("tree root entries = %+v", entries)
	}
}

// TestManifestSeedsSandboxView proves the manifest is the sandbox's path index:
// a file that exists in the manifest before the first bash mount is readable by
// bash without any tool write-through, which is what makes a session view
// survive a process restart.
func TestManifestSeedsSandboxView(t *testing.T) {
	h := newSeamHarness(t)
	put, err := h.store.PutStatus(strings.NewReader("seeded-from-manifest"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.manifest.Upsert(h.ctx, h.scope.TenantID, h.scope.SessionID, []fileplugin.Entry{{
		Path: "seed.txt", Hash: put.Hash, Mode: 0o644, Size: put.Size, UpdatedSeq: 1, LastTaskID: "task-old",
	}}); err != nil {
		t.Fatal(err)
	}
	session, err := h.bridge.NewSession(h.bashCtx("call-b3"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	var stdout strings.Builder
	result, err := session.Run(h.bashCtx("call-b3"), bashbridge.RunSpec{
		Command: `cat /workspace/seed.txt`,
		Stdout:  &stdout,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("bash read seeded file = %+v/%v (stdout %q)", result, err, stdout.String())
	}
	if stdout.String() != "seeded-from-manifest" {
		t.Fatalf("seeded content = %q", stdout.String())
	}
}

// TestSessionsGetIndependentWorkspaces pins the per-session key: a write in one
// session's workspace is not visible to another session's bash, even though both
// share the process-wide layerfs store and provider.
func TestSessionsGetIndependentWorkspaces(t *testing.T) {
	h := newSeamHarness(t)
	otherSession := uuid.New()
	h.mem.CreateSession(h.scope.TenantID, otherSession)
	h.mem.EnqueueRun(h.scope.TenantID, otherSession)
	if _, err := h.kernel.ClaimScope(h.ctx, kernel.Scope{TenantID: h.scope.TenantID, SessionID: otherSession}, "seam-other", time.Hour); err != nil {
		t.Fatalf("claim other session: %v", err)
	}
	otherScope := fileplugin.Scope{TenantID: h.scope.TenantID, SessionID: otherSession}
	otherCtx := fileplugin.WithCallID(fileplugin.WithScope(h.ctx, otherScope), "call-w-other")
	if _, err := h.service.Write(otherCtx, mustArgs(t, map[string]any{
		"path": "other-session.txt", "content": "not-for-you",
	})); err != nil {
		t.Fatalf("write in other session: %v", err)
	}

	session, err := h.bridge.NewSession(h.bashCtx("call-b-iso"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	var stdout, stderr strings.Builder
	result, err := session.Run(h.bashCtx("call-b-iso"), bashbridge.RunSpec{
		Command: `cat /workspace/other-session.txt`,
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatalf("bash run: %v", err)
	}
	if result.ExitCode == 0 || stdout.Len() != 0 {
		t.Fatalf("another session's file leaked into this workspace: exit %d stdout %q", result.ExitCode, stdout.String())
	}
}

// TestCaptureFailureIsOneSystemEvent pins the failure path: a workspace that
// cannot become a canonical tree (a name the tree form refuses) yields exactly
// one fs.capture_failed system event and no fs.capture, and Close reports it.
func TestCaptureFailureIsOneSystemEvent(t *testing.T) {
	h := newSeamHarness(t)
	if _, err := h.service.Write(h.fileCtx("call-wbad"), mustArgs(t, map[string]any{
		"path": "bad\nname", "content": "x",
	})); err != nil {
		t.Fatalf("write invalid-name file: %v", err)
	}
	session, err := h.bridge.NewSession(h.bashCtx("call-bbad"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := session.Run(h.bashCtx("call-bbad"), bashbridge.RunSpec{Command: `true`}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := session.Close(); err == nil {
		t.Fatal("close succeeded, want the capture failure")
	}
	if got := len(h.captures(t, kernel.KindFSCapture)); got != 0 {
		t.Fatalf("fs.capture events = %d, want 0", got)
	}
	failed := h.captures(t, kernel.KindFSCaptureFailed)
	if len(failed) != 1 {
		t.Fatalf("fs.capture_failed events = %d, want exactly 1", len(failed))
	}
	var payload map[string]any
	if err := json.Unmarshal(failed[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["toolCallId"] != "call-bbad" {
		t.Fatalf("capture_failed toolCallId = %v", payload["toolCallId"])
	}
	if payload["error"] == nil || !strings.Contains(payload["error"].(string), "gbashfs") {
		t.Fatalf("capture_failed error = %v", payload["error"])
	}
}

// TestBackgroundJobCaptureSettlesWithoutRunLease is the job bridge: a timeout
// turn (or a nohup child) outlives the run attempt, so the run lease is already
// released when the job's session closes. Its workspace changes MUST still enter
// layerfs as one fs.capture with jobId attribution, not as a capture_failed.
func TestBackgroundJobCaptureSettlesWithoutRunLease(t *testing.T) {
	h := newSeamHarness(t)
	h.releaseRunLease(t)

	session, err := h.bridge.NewSession(h.bashJobCtx("call-bg-1", "job-bg-1"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	result, err := session.Run(h.bashJobCtx("call-bg-1", "job-bg-1"), bashbridge.RunSpec{
		Command: `mkdir -p /workspace/job && printf 'job-wrote-this\n' > /workspace/job/out.txt`,
	})
	if err != nil {
		session.Close()
		t.Fatalf("job run: %v", err)
	}
	if result.ExitCode != 0 {
		session.Close()
		t.Fatalf("job exit = %d", result.ExitCode)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close (system capture): %v", err)
	}

	captures := h.captures(t, kernel.KindFSCapture)
	if len(captures) != 1 {
		t.Fatalf("fs.capture events = %d, want exactly 1", len(captures))
	}
	if failed := h.captures(t, kernel.KindFSCaptureFailed); len(failed) != 0 {
		t.Fatalf("fs.capture_failed events = %d, want 0", len(failed))
	}
	var payload map[string]any
	if err := json.Unmarshal(captures[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["jobId"] != "job-bg-1" {
		t.Fatalf("capture jobId = %v", payload["jobId"])
	}
	if payload["taskId"] != "task-seam" {
		t.Fatalf("capture taskId = %v", payload["taskId"])
	}
	if payload["count"].(float64) != 1 {
		t.Fatalf("capture payload = %v, want the one job-written file", payload)
	}

	read, err := h.service.Read(h.fileCtx("call-r-bg"), mustArgs(t, map[string]any{"path": "job/out.txt"}))
	if err != nil {
		t.Fatalf("read the job's captured file: %v", err)
	}
	if read.Content != "job-wrote-this\n" {
		t.Fatalf("captured content = %q", read.Content)
	}
}

// TestRunCaptureStillRequiresTheLease is the negative control: an identity
// without a job id stays on the owner lane, so a released run lease still yields
// exactly one fs.capture_failed. The system lane is not a generic bypass.
func TestRunCaptureStillRequiresTheLease(t *testing.T) {
	h := newSeamHarness(t)
	h.releaseRunLease(t)

	session, err := h.bridge.NewSession(h.bashCtx("call-nolease"))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := session.Run(h.bashCtx("call-nolease"), bashbridge.RunSpec{Command: `true`}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := session.Close(); err == nil {
		t.Fatal("close succeeded without a live lease, want the owner-lane capture failure")
	}
	if got := len(h.captures(t, kernel.KindFSCapture)); got != 0 {
		t.Fatalf("fs.capture events = %d, want 0 (the owner lane has no fence)", got)
	}
	failed := h.captures(t, kernel.KindFSCaptureFailed)
	if len(failed) != 1 {
		t.Fatalf("fs.capture_failed events = %d, want exactly 1", len(failed))
	}
	var payload map[string]any
	if err := json.Unmarshal(failed[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["toolCallId"] != "call-nolease" {
		t.Fatalf("capture_failed toolCallId = %v", payload["toolCallId"])
	}
	if _, hasJob := payload["jobId"]; hasJob {
		t.Fatalf("owner-lane failure carries jobId: %v", payload)
	}
	if payload["error"] == nil || !strings.Contains(payload["error"].(string), "lease") {
		t.Fatalf("capture_failed error = %v", payload["error"])
	}
}
