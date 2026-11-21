package agentext_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/sessionfork"
	"github.com/jixoai/keel/internal/store"
)

type forkSnapshots struct {
	tree  string
	seq   int64
	ok    bool
	calls int
}

func (f *forkSnapshots) SnapshotAtOrBefore(context.Context, uuid.UUID, uuid.UUID, int64) (string, int64, bool, error) {
	f.calls++
	return f.tree, f.seq, f.ok, nil
}

type forkCards struct {
	origin    uuid.UUID
	forked    uuid.UUID
	originSeq int64
	target    kernel.Scope
	called    bool
}

func (f *forkCards) OriginCard(context.Context, kernel.Scope, int64) (uuid.UUID, error) {
	return f.origin, nil
}

func (f *forkCards) ForkCard(_ context.Context, _ kernel.Scope, originTaskID uuid.UUID, originSeq int64, target kernel.Scope, _, _ string) (uuid.UUID, error) {
	f.called = true
	f.forked = originTaskID
	f.originSeq = originSeq
	f.target = target
	return uuid.New(), nil
}

type forkNotes struct {
	calls   int
	parent  uuid.UUID
	child   uuid.UUID
	status  string
	summary string
}

func (f *forkNotes) NoteSubagentFinished(_ context.Context, parent kernel.Scope, childSessionID uuid.UUID, status, summary string) error {
	f.calls++
	f.parent = parent.SessionID
	f.child = childSessionID
	f.status = status
	f.summary = summary
	return nil
}

type acceptingSelection struct {
	seen []json.RawMessage
}

func (a *acceptingSelection) validate(raw json.RawMessage) error {
	a.seen = append(a.seen, append(json.RawMessage(nil), raw...))
	return nil
}

func commitMessage(t *testing.T, kern *kernel.Kernel, scope kernel.Scope, requestID, text string, extras map[string]any) int64 {
	t.Helper()
	payload := map[string]any{"text": text, "requestId": requestID}
	for key, value := range extras {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := kern.Commit(context.Background(), kernel.CommitRequest{
		Scope: scope, Kind: kernel.KindUserMessage, RequestID: requestID, Payload: raw,
	})
	if err != nil {
		t.Fatalf("commit message: %v", err)
	}
	return receipt.Seq
}

// TestForkCreatesAPrefixSessionCardAndInheritedBinding covers the structural
// half of agent.fork: the origin eventSeq, the durable fork fact, the new card's
// parentTaskId, and the inherited execution binding (the child starts on the
// parent's configuration at the origin).
func TestForkCreatesAPrefixSessionCardAndInheritedBinding(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "fork-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	parentScope := kernel.Scope{TenantID: tenant, SessionID: parent}
	originSeq := commitMessage(t, kern, parentScope, "parent-r1", "parent question", map[string]any{
		"selection": map[string]any{"agentId": "agent_parent"},
	})
	// A parent run snapshot at the origin: the fork must freeze it.
	lease, err := kern.ClaimScope(ctx, parentScope, "fork-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]any{
		"status": "done", "configHashAfter": "sha256:parent-config",
		"toolManifestHash": "sha256:parent-tools", "anchorEventSeq": originSeq,
	})
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: parentScope, Kind: kernel.KindRunFinished, Token: lease.Token,
		RunID: "parent-run", Payload: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	if err := kern.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	parentEvents, err := st.LoadEvents(ctx, tenant, parent, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	tip := parentEvents[len(parentEvents)-1].Seq

	cards := &forkCards{origin: uuid.New()}
	snapshots := &forkSnapshots{tree: "sha256:tree", seq: originSeq, ok: true}
	notes := &forkNotes{}
	validator := &acceptingSelection{}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate:              storeSessions{st},
		Edge:              kern,
		Events:            kern,
		Runs:              storeSessions{st},
		Logger:            testLogger(),
		Forks:             st,
		Snapshots:         snapshots,
		Cards:             cards,
		Notes:             notes,
		ValidateSelection: validator.validate,
	})
	record, err := runner.ForkAsync(ctx, agentext.ForkRequest{
		TenantID: tenant, ParentSessionID: parent, EventSeq: 0,
		Task: "branch task", Title: "branch card",
	})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	child, err := uuid.Parse(record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Origin != "fork" || record.OriginSeq != tip || record.ParentTaskID != cards.origin.String() {
		t.Fatalf("fork record = %+v", record)
	}
	if record.ConfigHash != "sha256:parent-config" || record.ToolManifestHash != "sha256:parent-tools" {
		t.Fatalf("fork inherited snapshot = %+v", record)
	}
	if !cards.called || cards.forked != cards.origin || cards.originSeq != tip || cards.target.SessionID != child {
		t.Fatalf("fork card = %+v", cards)
	}
	fact, found, err := st.SessionFork(ctx, tenant, child)
	if err != nil || !found {
		t.Fatalf("durable fork fact: found=%v err=%v", found, err)
	}
	want := sessionfork.Fact{
		ParentSessionID: parent, OriginSeq: tip, TreeHash: "sha256:tree",
		ConfigHash: "sha256:parent-config", ToolManifestHash: "sha256:parent-tools", AnchorSeq: originSeq,
	}
	if fact != want {
		t.Fatalf("fork fact = %+v, want %+v", fact, want)
	}
	events, err := st.LoadEvents(ctx, tenant, child, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "user.message" {
		t.Fatalf("child events = %+v", events)
	}
	var payload struct {
		Text      string          `json:"text"`
		Selection json.RawMessage `json:"selection"`
		Subagent  struct {
			Origin    string `json:"origin"`
			OriginSeq int64  `json:"originSeq"`
		} `json:"subagent"`
		Fork struct {
			ParentSessionID string `json:"parentSessionId"`
			OriginSeq       int64  `json:"originSeq"`
			TreeHash        string `json:"treeHash"`
			OriginTaskID    string `json:"originTaskId"`
		} `json:"fork"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Text != "branch task" || payload.Subagent.Origin != "fork" || payload.Subagent.OriginSeq != tip {
		t.Fatalf("child payload = %+v", payload)
	}
	if payload.Fork.ParentSessionID != parent.String() || payload.Fork.OriginSeq != tip ||
		payload.Fork.TreeHash != "sha256:tree" || payload.Fork.OriginTaskID != cards.origin.String() {
		t.Fatalf("child fork extras = %+v", payload.Fork)
	}
	var selection struct {
		AgentID string `json:"agentId"`
	}
	if err := json.Unmarshal(payload.Selection, &selection); err != nil {
		t.Fatalf("child selection %s: %v", payload.Selection, err)
	}
	if selection.AgentID != "agent_parent" {
		t.Fatalf("child inherited selection = %+v", selection)
	}
	if len(validator.seen) != 1 {
		t.Fatalf("validator calls = %d, want the inherited selection", len(validator.seen))
	}

	// The forked child runs on its own ledger and records the W2 run snapshot.
	// The parent prefix is read, not copied: the child log holds only its input
	// and its answer.
	lease, err = kern.ClaimScope(ctx, kernel.Scope{TenantID: tenant, SessionID: child}, "fork-test-child", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", testLogger())
	attempt.ConfigHash = "sha256:current"
	runResult, err := attempt.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("forked child attempt: %v", err)
	}
	if runResult.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("forked child status = %q", runResult.Summary.Status)
	}
	if runResult.Summary.ConfigHashAfter != "sha256:current" || runResult.Summary.ToolManifestHash == "" || runResult.Summary.AnchorEventSeq <= 0 {
		t.Fatalf("forked child run snapshot = %+v", runResult.Summary)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{
		Lease: lease, Cursor: runResult.Cursor, ReadThrough: runResult.ReadThrough, MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	childEvents, err := st.LoadEvents(ctx, tenant, child, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, event := range childEvents {
		if event.Kind == "assistant.message" {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("forked child answers = %d, want one", answers)
	}
	if len(childEvents) != 2 {
		t.Fatalf("child ledger = %d events, want input + answer (the prefix is not copied)", len(childEvents))
	}
	// The structured return is self-describing: origin, origin seq and the
	// parent card travel with the summary.
	forkResult, err := runner.Wait(ctx, tenant, child, agentext.Budget{TimeoutMs: 2000}, "")
	if err != nil {
		t.Fatalf("wait forked child: %v", err)
	}
	if forkResult.Status != "done" || forkResult.Origin != "fork" || forkResult.OriginSeq != tip ||
		forkResult.ParentTaskID != cards.origin.String() {
		t.Fatalf("fork result = %+v", forkResult)
	}
	// The parent log did not gain the child's events.
	parentAfter, err := st.LoadEvents(ctx, tenant, parent, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentAfter) != len(parentEvents) {
		t.Fatalf("parent log grew from %d to %d events", len(parentEvents), len(parentAfter))
	}
}

// TestForkRefusesAnInexactOrigin pins the snapshot rule: a parent with file
// facts but no exact view at the origin must not fork an invented file view.
func TestForkRefusesAnInexactOrigin(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "fork-refuse"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	parentScope := kernel.Scope{TenantID: tenant, SessionID: parent}
	commitMessage(t, kern, parentScope, "parent-r1", "parent question", nil)
	lease, err := kern.ClaimScope(ctx, parentScope, "fork-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: parentScope, Kind: kernel.KindFSWrite, Token: lease.Token,
		FSWriteKey: "write-1",
		Payload:    json.RawMessage(`{"sessionId":"` + parent.String() + `","path":"a.txt","hash":"sha256:abc"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := kern.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: storeSessions{st}, Edge: kern, Events: kern, Runs: storeSessions{st},
		Logger: testLogger(), Forks: st,
		Snapshots: &forkSnapshots{ok: false},
		Cards:     &forkCards{origin: uuid.New()},
	})
	_, err = runner.ForkAsync(ctx, agentext.ForkRequest{
		TenantID: tenant, ParentSessionID: parent, EventSeq: 0, Task: "branch",
	})
	if !errors.Is(err, agentext.ErrForkSnapshotMissing) {
		t.Fatalf("fork without a snapshot = %v, want ErrForkSnapshotMissing", err)
	}
}

type storeRequeuer struct{ st *store.Store }

func (r storeRequeuer) RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (agentext.RequeueOutcome, error) {
	outcome, err := r.st.RequeueRun(ctx, tenantID, sessionID)
	return agentext.RequeueOutcome{Status: outcome.Status, Attempts: outcome.Attempts}, err
}

func newLinkedChild(t *testing.T, st *store.Store, kern *kernel.Kernel, tenant, parent uuid.UUID) uuid.UUID {
	t.Helper()
	child := uuid.New()
	if err := st.CreateSession(context.Background(), tenant, child); err != nil {
		t.Fatal(err)
	}
	commitMessage(t, kern, kernel.Scope{TenantID: tenant, SessionID: child}, "child-r1", "child task", map[string]any{
		"subagent": map[string]any{"parentSessionId": parent.String(), "origin": "spawn"},
	})
	return child
}

// TestResumeContinuesTheSameSession is the W8 resume proof: an interrupted
// child is re-queued in place (same session, one input, no new card, one
// resume.attempt), a repeated requestId is idempotent, and the recovered
// attempt completes the original task.
func TestResumeContinuesTheSameSession(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "resume-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	child := newLinkedChild(t, st, kern, tenant, parent)
	childScope := kernel.Scope{TenantID: tenant, SessionID: child}

	validator := &acceptingSelection{}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: storeSessions{st}, Edge: kern, Events: kern, Runs: storeSessions{st},
		Logger: testLogger(), Requeue: storeRequeuer{st: st},
		ConfigVersion:     func() (string, int64) { return "sha256:current", 7 },
		ValidateSelection: validator.validate,
	})
	// A queued child is not duplicated by resume.
	if result, err := runner.Resume(ctx, agentext.ResumeRequest{
		TenantID: tenant, ParentSessionID: parent, SessionID: child,
	}); err != nil || result.Status != "pending" {
		t.Fatalf("resume pending = %+v err=%v", result, err)
	}
	// Simulate an interrupted attempt: failed with a spent budget.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'failed', attempts = 3, pending_input = false,
		                last_error = 'interrupted'
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, child); err != nil {
		t.Fatal(err)
	}
	override := json.RawMessage(`{"agentId":"agent_override"}`)
	first, err := runner.Resume(ctx, agentext.ResumeRequest{
		TenantID: tenant, ParentSessionID: parent, SessionID: child, RequestID: "resume-1", Reason: "process crash",
		Selection: override,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if first.Status != "resumed" || first.EventSeq == 0 || first.ConfigHash != "sha256:current" || first.ConfigRevision != 7 {
		t.Fatalf("resume result = %+v", first)
	}
	// Same requestId + same interrupted state converges on the same fact.
	if _, err := st.Pool().Exec(ctx, `
		UPDATE runs SET status = 'failed', attempts = 3, pending_input = false,
		                last_error = 'interrupted'
		 WHERE tenant_id = $1 AND session_id = $2`, tenant, child); err != nil {
		t.Fatal(err)
	}
	second, err := runner.Resume(ctx, agentext.ResumeRequest{
		TenantID: tenant, ParentSessionID: parent, SessionID: child, RequestID: "resume-1", Reason: "process crash",
		Selection: override,
	})
	if err != nil || second.EventSeq != first.EventSeq {
		t.Fatalf("idempotent resume = %+v err=%v, want seq %d", second, err, first.EventSeq)
	}
	events, err := st.LoadEvents(ctx, tenant, child, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	resumes := 0
	inputs := 0
	var resumePayload struct {
		Selection json.RawMessage `json:"selection"`
	}
	for _, event := range events {
		switch event.Kind {
		case "resume.attempt":
			resumes++
			if err := json.Unmarshal(event.Payload, &resumePayload); err != nil {
				t.Fatal(err)
			}
		case "user.message":
			inputs++
		}
	}
	if resumes != 1 || inputs != 1 {
		t.Fatalf("child log = %d resume.attempt, %d user.message; want one each", resumes, inputs)
	}
	if !strings.Contains(string(resumePayload.Selection), "agent_override") || len(validator.seen) != 2 {
		t.Fatalf("resume override = %s (validator calls %d)", resumePayload.Selection, len(validator.seen))
	}
	var cards int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM task_cards WHERE tenant_id = $1 AND session_id = $2`, tenant, child).Scan(&cards); err != nil {
		t.Fatal(err)
	}
	if cards != 0 {
		t.Fatalf("resume created %d task cards; it must not reopen the task", cards)
	}

	// Recovery: the requeued run is claimed and completes the original task.
	lease, err := kern.ClaimScope(ctx, childScope, "resume-recovery", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt := agent.NewRunner(kern, kern, agent.FakeStream(), "fake", "fake", "", testLogger())
	attempt.ConfigHash = "sha256:current"
	result, err := attempt.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("recovered attempt: %v", err)
	}
	// The worker writes the attempt summary before the settle; mirror it here so
	// the test observes the same run.finished fact a production run leaves.
	summaryRaw, err := json.Marshal(result.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindRunFinished, Token: lease.Token,
		RunID: "recovered-run", Payload: summaryRaw,
	}); err != nil {
		t.Fatalf("commit run.finished: %v", err)
	}
	if err := kern.Finish(ctx, kernel.FinishRequest{
		Lease: lease, Cursor: result.Cursor, ReadThrough: result.ReadThrough, MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := (storeSessions{st}).RunState(ctx, tenant, child)
	if err != nil || state.Status != "done" {
		t.Fatalf("recovered run state = %+v err=%v", state, err)
	}
	events, err = st.LoadEvents(ctx, tenant, child, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	assistants := 0
	var finished *store.Event
	for i := range events {
		switch events[i].Kind {
		case "assistant.message":
			assistants++
		case "run.finished":
			finished = &events[i]
		}
	}
	if assistants != 1 {
		t.Fatalf("recovered task produced %d answers, want one", assistants)
	}
	// W2 snapshot semantics survive resume: the recovered attempt records the
	// configuration it ran under, a non-empty tool manifest and an anchor inside
	// the child's own log.
	if finished == nil {
		t.Fatal("the recovered attempt wrote no run.finished")
	}
	var summary struct {
		ConfigHashAfter  string `json:"configHashAfter"`
		ToolManifestHash string `json:"toolManifestHash"`
		AnchorEventSeq   int64  `json:"anchorEventSeq"`
	}
	if err := json.Unmarshal(finished.Payload, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.ConfigHashAfter != "sha256:current" || summary.ToolManifestHash == "" || summary.AnchorEventSeq <= 0 {
		t.Fatalf("run snapshot = %+v", summary)
	}
}

// TestCancelStopsTheChildInPlace pins cancel: it targets the same session and
// refuses a child that does not belong to the named parent.
func TestCancelStopsTheChildInPlace(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "cancel-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	child := newLinkedChild(t, st, kern, tenant, parent)
	control := agentext.NewRunControl(storeSessions{st}, testLogger())
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: storeSessions{st}, Edge: kern, Events: kern, Runs: storeSessions{st},
		Logger: testLogger(), Control: control,
	})
	runCtx, finish := control.BeginRun(ctx, kernel.Scope{TenantID: tenant, SessionID: child}, "run-1")
	defer finish("cancelled")
	_ = runCtx
	result, err := runner.Cancel(ctx, agentext.CancelRequest{
		TenantID: tenant, ParentSessionID: parent, SessionID: child, Reason: "operator stop",
	})
	if err != nil || !result.Cancelled {
		t.Fatalf("cancel = %+v err=%v", result, err)
	}
	if _, err := runner.Cancel(ctx, agentext.CancelRequest{
		TenantID: tenant, ParentSessionID: uuid.New(), SessionID: child,
	}); !errors.Is(err, agentext.ErrSubagentNotFound) {
		t.Fatalf("foreign-parent cancel = %v, want ErrSubagentNotFound", err)
	}
}

type fixedRuns struct{ status string }

func (f fixedRuns) RunState(context.Context, uuid.UUID, uuid.UUID) (agentext.RunState, error) {
	return agentext.RunState{Status: f.status}, nil
}

// TestSubagentReturnCarriesArtifactRefsAndParentNote pins the W8 return
// contract: the structured result lists content-addressed artifact references
// (file hash, capture tree, tool output blob) and the parent card receives one
// idempotent task.note.
func TestSubagentReturnCarriesArtifactRefsAndParentNote(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	tenant, parent := uuid.New(), uuid.New()
	if err := st.CreateTenant(ctx, tenant, "artifact-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, parent); err != nil {
		t.Fatal(err)
	}
	klog, err := kernel.NewPG(ctx, st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	kern := kernel.New(klog, kernel.Options{})
	child := newLinkedChild(t, st, kern, tenant, parent)
	childScope := kernel.Scope{TenantID: tenant, SessionID: child}
	lease, err := kern.ClaimScope(ctx, childScope, "artifact-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	toolCall := agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{agentcore.ToolCallContent{
			Type: "toolCall", ID: "call-bash", Name: "bash", Arguments: json.RawMessage(`{"action":"run","command":"ls"}`),
		}},
		StopReason: agentcore.StopReasonToolUse,
	}
	toolCallRaw, _ := json.Marshal(toolCall)
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindAssistantMessage, Token: lease.Token,
		GenerationID: "gen-1", Payload: toolCallRaw,
	}); err != nil {
		t.Fatal(err)
	}
	toolResult := agentcore.ToolResultMessage{
		RoleField: agentcore.RoleToolResult, ToolCallID: "call-bash", ToolName: "bash",
		Content: agentcore.ContentList{agentcore.NewTextContent("ok")},
		Details: map[string]any{"stdout": map[string]any{"blobId": "11111111-1111-1111-1111-111111111111"}},
	}
	toolResultRaw, _ := json.Marshal(toolResult)
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindToolResult, Token: lease.Token,
		ToolCallID: "call-bash", Payload: toolResultRaw,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindFSWrite, Token: lease.Token, FSWriteKey: "write-1",
		Payload: json.RawMessage(`{"sessionId":"` + child.String() + `","path":"out.txt","hash":"sha256:file","mode":420,"size":5}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindFSCapture, Token: lease.Token, FSCaptureKey: "capture-1",
		Payload: json.RawMessage(`{"sessionId":"` + child.String() + `","baseSeq":0,"count":1,"treeHash":"sha256:tree"}`),
	}); err != nil {
		t.Fatal(err)
	}
	answer := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent("all done")},
		StopReason: agentcore.StopReasonEndTurn,
	}
	answerRaw, _ := json.Marshal(answer)
	if _, err := kern.Commit(ctx, kernel.CommitRequest{
		Scope: childScope, Kind: kernel.KindAssistantMessage, Token: lease.Token,
		GenerationID: "gen-2", Payload: answerRaw,
	}); err != nil {
		t.Fatal(err)
	}
	if err := kern.Release(ctx, lease); err != nil {
		t.Fatal(err)
	}

	notes := &forkNotes{}
	runner := agentext.NewSubagentRunner(agentext.SubagentOptions{
		Gate: storeSessions{st}, Edge: kern, Events: kern, Runs: fixedRuns{status: "done"},
		Logger: testLogger(), Notes: notes,
	})
	result, err := runner.Wait(ctx, tenant, child, agentext.Budget{TimeoutMs: 5000}, "")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if result.Status != "done" || result.Summary != "all done" || result.ParentSessionID != parent.String() {
		t.Fatalf("result = %+v", result)
	}
	seen := map[string]agentext.Artifact{}
	for _, artifact := range result.Artifacts {
		seen[artifact.Type+":"+artifact.Ref] = artifact
	}
	if _, ok := seen["file:sha256:file"]; !ok {
		t.Fatalf("missing file artifact: %+v", result.Artifacts)
	}
	if _, ok := seen["tree:sha256:tree"]; !ok {
		t.Fatalf("missing tree artifact: %+v", result.Artifacts)
	}
	if _, ok := seen["blob:11111111-1111-1111-1111-111111111111"]; !ok {
		t.Fatalf("missing blob artifact: %+v", result.Artifacts)
	}
	if _, ok := seen["tool:call-bash"]; !ok {
		t.Fatalf("missing tool trail: %+v", result.Artifacts)
	}
	if notes.calls != 1 || notes.parent != parent || notes.child != child || notes.status != "done" || notes.summary != "all done" {
		t.Fatalf("parent note = %+v", notes)
	}
}
