package superagentboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	keelstore "github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/superagent"

	"github.com/jixoai/keel-board/protocol"
)

type fakeIdentity struct {
	tenant    uuid.UUID
	actor     protocol.Actor
	actorErr  error
	tenantErr error
}

func (f fakeIdentity) Actor(*http.Request) (protocol.Actor, error) {
	return f.actor, f.actorErr
}

func (f fakeIdentity) Tenant(*http.Request) (string, error) {
	if f.tenantErr != nil {
		return "", f.tenantErr
	}
	return f.tenant.String(), nil
}

type fakeAuthorizer struct{ err error }

func (f fakeAuthorizer) AuthorizeAnywhere(context.Context, string, protocol.Actor, protocol.Permission) error {
	return f.err
}

type fakeTaskWriteGate struct{ err error }

func (f fakeTaskWriteGate) AuthorizeTaskWrite(context.Context, uuid.UUID) error { return f.err }

// fakeSupervisor records the action and lets the test mutate the supervisor
// store the way the real orchestrator would.
type fakeSupervisor struct {
	mu             sync.Mutex
	calls          []string
	err            error
	behavior       func(action string) error
	lastAsk        collab.AttentionRequest
	lastReport     superagent.ReportRequest
	lastAnswerOpt  string
	lastAnswerText string
}

func (f *fakeSupervisor) record(action string) error {
	f.mu.Lock()
	f.calls = append(f.calls, action)
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.behavior != nil {
		return f.behavior(action)
	}
	return nil
}

func (f *fakeSupervisor) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSupervisor) Report(_ context.Context, request superagent.ReportRequest) error {
	f.mu.Lock()
	f.lastReport = request
	f.mu.Unlock()
	return f.record("report")
}

func (f *fakeSupervisor) Ask(_ context.Context, _ uuid.UUID, _ uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	// The real collab.Host refuses a question with no selectable answer. The
	// fake keeps that contract so a bridge regression (dropping AllowFreeText)
	// fails here instead of in production.
	if len(request.Options) == 0 && !request.AllowFreeText {
		return collab.Attention{}, errors.New("collab: an attention needs options or free text")
	}
	f.mu.Lock()
	f.lastAsk = request
	f.mu.Unlock()
	if err := f.record("ask"); err != nil {
		return collab.Attention{}, err
	}
	return collab.Attention{}, nil
}

func (f *fakeSupervisor) AnswerAttention(_ context.Context, _ uuid.UUID, _ uuid.UUID, optionID, text, _ string) (collab.Attention, error) {
	f.mu.Lock()
	f.lastAnswerOpt = optionID
	f.lastAnswerText = text
	f.mu.Unlock()
	if err := f.record("answer"); err != nil {
		return collab.Attention{}, err
	}
	return collab.Attention{}, nil
}

func (f *fakeSupervisor) ResumeTicket(context.Context, uuid.UUID, uuid.UUID, string, string) (superagent.Ticket, error) {
	if err := f.record("resume"); err != nil {
		return superagent.Ticket{}, err
	}
	return superagent.Ticket{}, nil
}

func (f *fakeSupervisor) StopTicket(context.Context, uuid.UUID, uuid.UUID, string, string) error {
	return f.record("stop")
}

type bridgeWorld struct {
	tenant       uuid.UUID
	clock        *testClock
	store        *superagent.MemoryStore
	board        *LogBoard
	links        *memoryLinks
	mirror       *Mirror
	surf         *Bridge
	super        *fakeSupervisor
	ticket       superagent.Ticket
	taskID       uint64
	attentionID  uuid.UUID
	assignmentID uuid.UUID
	nextHits     int
}

func newBridgeWorld(t *testing.T, config SuperagentConfig, setup func(world *bridgeWorld)) *bridgeWorld {
	t.Helper()
	world := &bridgeWorld{clock: newTestClock()}
	world.board = newTestBoard(world.clock)
	world.links = newMemoryLinks()
	world.store = superagent.NewMemoryStore(world.clock.Now)
	world.tenant = uuid.New()
	world.ticket = seedTicket(t, world.store, world.tenant, "Fix the build", "Body", "manual", "t-bridge", "evt-bridge")
	world.mirror = newMirrorForTest(t, world.tenant, world.store, world.board, world.links,
		SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, world.clock)
	world.super = &fakeSupervisor{}
	if setup != nil {
		setup(world)
	}
	if _, err := world.mirror.SyncOnce(context.Background()); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	link := mustLink(t, world.links, world.tenant, world.ticket.ID.String(), keelstore.BoardLinkTicket)
	world.taskID = link.BoardTaskID

	commands, err := NewSupervisorCommands(world.tenant, world.store, world.super)
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		world.nextHits++
		w.WriteHeader(http.StatusTeapot)
	})
	world.surf = &Bridge{
		Next: next,
		Identity: fakeIdentity{tenant: world.tenant, actor: protocol.Actor{
			ID: "account:alice", Principal: "account:alice", Kind: protocol.ActorPerson,
			Handle: "alice", Name: "Alice",
		}},
		Authorizer: fakeAuthorizer{},
		Operator:   fakeTaskWriteGate{},
		Logger:     discardLogger(),
		Timeout:    200 * time.Millisecond,
	}
	world.surf.Attach(world.links, StaticConfigReader{Config: config}, world.mirror, commands, world.board)
	return world
}

func (w *bridgeWorld) post(action string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/v1/board/tasks/"+formatID(w.taskID)+"/"+action, bytes.NewReader(raw))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	w.surf.ServeHTTP(recorder, request)
	return recorder
}

func formatID(id uint64) string { return strconv.FormatUint(id, 10) }

func decodeCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error %q: %v", recorder.Body.String(), err)
	}
	return body.Error.Code
}

func TestBridgeEachActionCallsSupervisorThenMirrors(t *testing.T) {
	type testCase struct {
		name     string
		action   string
		body     map[string]any
		setup    func(w *bridgeWorld)
		mutate   func(w *bridgeWorld) error
		wantCall string
		assert   func(t *testing.T, w *bridgeWorld)
	}
	cases := []testCase{
		{
			name: "claim", action: "claim", body: map[string]any{},
			mutate: func(w *bridgeWorld) error {
				addAssignment(t, w.store, w.ticket, collab.AssignmentActive, "researcher", uuid.New())
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemActive)
				return nil
			},
			wantCall: "resume",
			assert: func(t *testing.T, w *bridgeWorld) {
				if mustTask(t, w.board, w.tenant, w.taskID).State != StateClaimed {
					t.Fatal("board task is not claimed")
				}
			},
		},
		{
			name: "assign", action: "assign", body: map[string]any{"for": "researcher"},
			mutate: func(w *bridgeWorld) error {
				addAssignment(t, w.store, w.ticket, collab.AssignmentActive, "researcher", uuid.New())
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemActive)
				return nil
			},
			wantCall: "resume",
			assert: func(t *testing.T, w *bridgeWorld) {
				task := mustTask(t, w.board, w.tenant, w.taskID)
				if task.For == "" || !strings.HasSuffix(task.For, "+researcher") {
					t.Fatalf("for = %q", task.For)
				}
			},
		},
		{
			name: "complete", action: "complete", body: map[string]any{"result": "done"},
			setup: func(w *bridgeWorld) { activeWorld(t, w) },
			mutate: func(w *bridgeWorld) error {
				return finishWorldAssignment(t, w, collab.AssignmentDone, collab.WorkItemDone, superagent.ReportReady, "done")
			},
			wantCall: "report",
			assert: func(t *testing.T, w *bridgeWorld) {
				if mustTask(t, w.board, w.tenant, w.taskID).State != StateCompleted {
					t.Fatal("board task is not completed")
				}
			},
		},
		{
			name: "fail", action: "fail", body: map[string]any{"reason": "broken"},
			setup: func(w *bridgeWorld) { activeWorld(t, w) },
			mutate: func(w *bridgeWorld) error {
				return finishWorldAssignment(t, w, collab.AssignmentFailed, collab.WorkItemFailed, superagent.ReportFailed, "broken")
			},
			wantCall: "report",
			assert: func(t *testing.T, w *bridgeWorld) {
				if mustTask(t, w.board, w.tenant, w.taskID).State != StateFailed {
					t.Fatal("board task is not failed")
				}
			},
		},
		{
			name: "block", action: "block", body: map[string]any{"reason": "Approve?", "kind": "awaiting_approval"},
			setup: func(w *bridgeWorld) { activeWorld(t, w) },
			mutate: func(w *bridgeWorld) error {
				assignment := newestAssignment(t, w)
				addAttention(t, w.store, w.ticket, assignment, collab.AttentionApproval, collab.AttentionOpen)
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemBlocked)
				return nil
			},
			wantCall: "ask",
			assert: func(t *testing.T, w *bridgeWorld) {
				task := mustTask(t, w.board, w.tenant, w.taskID)
				if task.State != StateBlocked {
					t.Fatalf("state = %q", task.State)
				}
				view, _ := w.board.TaskView(context.Background(), w.tenant, w.taskID)
				if view.Task.Blocked == nil || view.Task.Blocked.Kind == nil || *view.Task.Blocked.Kind != HoldAwaitingApproval {
					t.Fatalf("blocked = %#v", view.Task.Blocked)
				}
			},
		},
		{
			name: "unblock", action: "unblock", body: map[string]any{"reason": "approved"},
			setup: func(w *bridgeWorld) {
				activeWorld(t, w)
				assignment := newestAssignment(t, w)
				attention := addAttention(t, w.store, w.ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
				w.attentionID = attention.ID
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemBlocked)
			},
			mutate: func(w *bridgeWorld) error {
				attention, err := w.store.GetAttention(context.Background(), w.tenant, w.attentionID)
				if err != nil {
					return err
				}
				answerAttention(t, w.store, attention, "yes")
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemActive)
				return nil
			},
			wantCall: "answer",
			assert: func(t *testing.T, w *bridgeWorld) {
				task := mustTask(t, w.board, w.tenant, w.taskID)
				if task.State != StateClaimed {
					t.Fatalf("state = %q, want claimed after unblock", task.State)
				}
				if task.Replies != 1 {
					t.Fatalf("replies = %d", task.Replies)
				}
			},
		},
		{
			name: "cancel", action: "cancel", body: map[string]any{"reason": "stop"},
			mutate: func(w *bridgeWorld) error {
				updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemCancelled)
				return nil
			},
			wantCall: "stop",
			assert: func(t *testing.T, w *bridgeWorld) {
				if mustTask(t, w.board, w.tenant, w.taskID).State != StateCanceled {
					t.Fatal("board task is not canceled")
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, testCase.setup)
			world.super.behavior = func(string) error { return testCase.mutate(world) }
			recorder := world.post(testCase.action, testCase.body)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if world.super.callCount() != 1 {
				t.Fatalf("supervisor calls = %d, want 1", world.super.callCount())
			}
			if got := world.super.calls[0]; got != testCase.wantCall {
				t.Fatalf("supervisor action = %q, want %q", got, testCase.wantCall)
			}
			testCase.assert(t, world)
		})
	}
}

func activeWorld(t *testing.T, w *bridgeWorld) {
	assignment := addAssignment(t, w.store, w.ticket, collab.AssignmentActive, "researcher", uuid.New())
	w.assignmentID = assignment.ID
	updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemActive)
}

func finishWorldAssignment(t *testing.T, w *bridgeWorld, status collab.AssignmentStatus, ticketStatus collab.WorkItemStatus, state superagent.ReportState, summary string) error {
	assignment, err := w.store.GetAssignment(context.Background(), w.tenant, w.assignmentID)
	if err != nil {
		return err
	}
	assignment.Status = status
	assignment.Result = json.RawMessage(`"done"`)
	if _, err := w.store.UpdateAssignment(context.Background(), assignment); err != nil {
		return err
	}
	addAppliedReport(t, w.store, w.ticket, assignment, state, summary)
	updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, ticketStatus)
	return nil
}

func newestAssignment(t *testing.T, w *bridgeWorld) collab.Assignment {
	t.Helper()
	assignments, err := w.store.ListAssignments(context.Background(), w.tenant, w.ticket.ID)
	if err != nil || len(assignments) == 0 {
		t.Fatalf("assignments: %v", err)
	}
	return assignments[len(assignments)-1]
}

func TestBridgeSupervisorRejectionLeavesBoardUnchanged(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, func(w *bridgeWorld) { activeWorld(t, w) })
	before := mustTask(t, world.board, world.tenant, world.taskID)
	head := boardHead(t, world.board, world.tenant)
	world.super.err = errFake
	recorder := world.post("complete", map[string]any{"result": "done"})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "supervisor_rejected" {
		t.Fatalf("code = %q", code)
	}
	after := mustTask(t, world.board, world.tenant, world.taskID)
	if after.State != before.State || after.Epoch != before.Epoch {
		t.Fatalf("board changed after rejection: before=%+v after=%+v", before, after)
	}
	if head != boardHead(t, world.board, world.tenant) {
		t.Fatal("a rejected supervisor action appended board events")
	}
}

func TestBridgeTimeoutLeavesBoardUnchanged(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, func(w *bridgeWorld) { activeWorld(t, w) })
	before := mustTask(t, world.board, world.tenant, world.taskID)
	head := boardHead(t, world.board, world.tenant)
	world.super.behavior = func(string) error {
		time.Sleep(400 * time.Millisecond)
		return context.DeadlineExceeded
	}
	recorder := world.post("complete", map[string]any{"result": "done"})
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "supervisor_timeout" {
		t.Fatalf("code = %q", code)
	}
	after := mustTask(t, world.board, world.tenant, world.taskID)
	if after.State != before.State || after.Epoch != before.Epoch {
		t.Fatalf("board changed after timeout: before=%+v after=%+v", before, after)
	}
	if head != boardHead(t, world.board, world.tenant) {
		t.Fatal("a timed-out supervisor action appended board events")
	}
}

func TestBridgeOffIsMirrorReadOnly(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: false, Channel: "superagent"}, nil)
	before := mustTask(t, world.board, world.tenant, world.taskID)
	recorder := world.post("claim", map[string]any{})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("code = %q", code)
	}
	if world.super.callCount() != 0 {
		t.Fatal("bridge called the supervisor while disabled")
	}
	if got := mustTask(t, world.board, world.tenant, world.taskID); got.State != before.State {
		t.Fatalf("state = %q, want %q", got.State, before.State)
	}
}

func TestBridgeClaimReadyRejected(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/board/tasks/claim",
		strings.NewReader(`{"channel":"superagent"}`))
	recorder := httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("code = %q", code)
	}
	if world.nextHits != 0 {
		t.Fatal("claim-ready reached the board handler")
	}
}

func TestBridgeNonMirroredTaskPassesThrough(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/board/tasks/99999/claim", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want pass-through", recorder.Code)
	}
	if world.nextHits != 1 {
		t.Fatalf("next hits = %d", world.nextHits)
	}
}

func TestBridgeUnsupportedActionIsReadOnly(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	recorder := world.post("release", map[string]any{})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("code = %q", code)
	}
	if world.super.callCount() != 0 {
		t.Fatal("unsupported action reached the supervisor")
	}
}

func TestBridgeMessageEditRejected(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	request := httptest.NewRequest(http.MethodPost,
		"/v1/board/messages/"+formatID(world.taskID)+"/edit", strings.NewReader(`{"text":"tamper"}`))
	recorder := httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("code = %q", code)
	}
}

func TestBridgeAuthorizerDenialStopsBeforeSupervisor(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	world.surf.Authorizer = fakeAuthorizer{err: protocol.Forbidden("denied")}
	recorder := world.post("claim", map[string]any{})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if world.super.callCount() != 0 {
		t.Fatal("a denied caller reached the supervisor")
	}
}

// TestStaleLinkIsReadOnlyWhenMirrorIsOff covers the configuration matrix's
// dangerous corner: a link left from a period when mirroring was on must not
// let the bridge act while mirror is off (the board could not be repaired).
func TestStaleLinkIsReadOnlyWhenMirrorIsOff(t *testing.T) {
	for _, bridgeFlag := range []bool{true, false} {
		world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
		commands, err := NewSupervisorCommands(world.tenant, world.store, world.super)
		if err != nil {
			t.Fatal(err)
		}
		world.surf.Attach(world.links,
			StaticConfigReader{Config: SuperagentConfig{Mirror: false, Bridge: bridgeFlag, Channel: "superagent"}},
			world.mirror, commands, world.board)
		recorder := world.post("claim", map[string]any{})
		if recorder.Code != http.StatusConflict {
			t.Fatalf("bridge=%v status = %d body=%s", bridgeFlag, recorder.Code, recorder.Body.String())
		}
		if code := decodeCode(t, recorder); code != "mirror_read_only" {
			t.Fatalf("bridge=%v code = %q", bridgeFlag, code)
		}
		if world.super.callCount() != 0 {
			t.Fatal("a stale link reached the supervisor while the mirror was off")
		}
	}
}

func TestBridgeBlockSendsAnswerableAsk(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"},
		func(w *bridgeWorld) { activeWorld(t, w) })
	world.super.behavior = func(string) error {
		assignment := newestAssignment(t, world)
		addAttention(t, world.store, world.ticket, assignment, collab.AttentionApproval, collab.AttentionOpen)
		updateTicketStatus(t, world.store, world.tenant, world.ticket.ID, collab.WorkItemBlocked)
		return nil
	}
	recorder := world.post("block", map[string]any{"reason": "Approve?", "kind": "awaiting_approval"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	world.super.mu.Lock()
	request := world.super.lastAsk
	world.super.mu.Unlock()
	if !request.AllowFreeText {
		t.Fatal("the bridge's Ask has no options and no free text: the real supervisor would refuse it")
	}
	if request.Prompt != "Approve?" {
		t.Fatalf("prompt = %q", request.Prompt)
	}
	if request.CallerSessionID != uuid.Nil {
		t.Fatalf("the bridge impersonated worker session %s", request.CallerSessionID)
	}
}

func TestBridgeRequiresSupervisorTaskWrite(t *testing.T) {
	for _, action := range []string{"claim", "complete", "fail", "block", "unblock", "assign", "cancel"} {
		t.Run(action, func(t *testing.T) {
			world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"},
				func(w *bridgeWorld) { activeWorld(t, w) })
			world.surf.Operator = fakeTaskWriteGate{err: errors.New("task.write missing")}
			recorder := world.post(action, map[string]any{"reason": "x", "result": "x"})
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			if world.super.callCount() != 0 {
				t.Fatalf("%s reached the supervisor without task.write", action)
			}
		})
	}
}

func TestBridgeClaimReadyOverlapGuard(t *testing.T) {
	cases := []struct {
		name          string
		channel       string
		mirrorChannel string
		mirror        bool
		bridge        bool
		want          int
	}{
		{name: "exact", channel: "superagent", mirror: true, bridge: true, want: http.StatusConflict},
		{name: "ancestor", channel: "work/", mirrorChannel: "work/agents", mirror: true, bridge: true, want: http.StatusConflict},
		{name: "descendant", channel: "superagent/sub", mirror: true, bridge: true, want: http.StatusConflict},
		{name: "unrelated", channel: "work/agents", mirror: true, bridge: true, want: http.StatusTeapot},
		{name: "both off exact", channel: "superagent", want: http.StatusConflict},
		{name: "both off ancestor", channel: "work/", mirrorChannel: "work/agents", want: http.StatusConflict},
		{name: "empty", mirror: true, bridge: true, want: http.StatusConflict},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			channel := testCase.mirrorChannel
			if channel == "" {
				channel = "superagent"
			}
			world := newBridgeWorld(t, SuperagentConfig{
				Mirror: testCase.mirror, Bridge: testCase.bridge, Channel: channel,
			}, nil)
			body := map[string]any{}
			if testCase.channel != "" {
				body["channel"] = testCase.channel
			}
			raw, _ := json.Marshal(body)
			request := httptest.NewRequest(http.MethodPost, "/v1/board/tasks/claim", bytes.NewReader(raw))
			recorder := httptest.NewRecorder()
			world.surf.ServeHTTP(recorder, request)
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d body=%s, want %d", recorder.Code, recorder.Body.String(), testCase.want)
			}
			if testCase.want == http.StatusConflict {
				if code := decodeCode(t, recorder); code != "mirror_read_only" {
					t.Fatalf("code = %q", code)
				}
			}
		})
	}
}

func TestBridgeAnswerDeliveryErrors(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "pending", err: superagent.ErrAnswerDeliveryPending, wantStatus: http.StatusAccepted, wantCode: ""},
		{name: "failed", err: superagent.ErrAnswerDeliveryFailed, wantStatus: http.StatusConflict, wantCode: "answer_delivery_failed"},
		{name: "not delivered", err: superagent.ErrAnswerNotDelivered, wantStatus: http.StatusConflict, wantCode: "answer_not_delivered"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"},
				func(w *bridgeWorld) {
					activeWorld(t, w)
					assignment := newestAssignment(t, w)
					attention := addAttention(t, w.store, w.ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
					w.attentionID = attention.ID
					updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemBlocked)
				})
			world.super.err = testCase.err
			recorder := world.post("unblock", map[string]any{"reason": "approved"})
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", recorder.Code, recorder.Body.String(), testCase.wantStatus)
			}
			if testCase.wantCode != "" {
				if code := decodeCode(t, recorder); code != testCase.wantCode {
					t.Fatalf("code = %q, want %q", code, testCase.wantCode)
				}
			}
		})
	}
}

func TestBridgeIgnoresForgedSourceLink(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	// A board task that only a non-superagent link names must not route to the
	// supervisor: the reverse lookup requires source=superagent, kind=ticket.
	forged, err := world.board.EnsureTicketThread(context.Background(), world.tenant, TicketThread{
		TicketID: "forged-ticket", Channel: "superagent", Title: "Forged", Request: "Body",
		Actor: ServiceActor(world.tenant),
	})
	if err != nil {
		t.Fatalf("forged task: %v", err)
	}
	if _, err := world.links.PutBoardLink(context.Background(), keelstore.BoardLink{
		TenantID: world.tenant, Source: "forged", SourceID: "forged-ticket",
		Kind: keelstore.BoardLinkTicket, BoardTaskID: forged.ID,
	}); err != nil {
		t.Fatalf("forged link insert: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{})
	request := httptest.NewRequest(http.MethodPost,
		"/v1/board/tasks/"+formatID(forged.ID)+"/claim", bytes.NewReader(raw))
	recorder := httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTeapot {
		t.Fatalf("status = %d body=%s, want pass-through", recorder.Code, recorder.Body.String())
	}
	if world.super.callCount() != 0 {
		t.Fatal("a forged link reached the supervisor")
	}
}

func TestBridgeTaskMutationsAreReadOnly(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	for _, action := range []string{"release", "renew", "reopen", "needs"} {
		recorder := world.post(action, map[string]any{})
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s status = %d body=%s", action, recorder.Code, recorder.Body.String())
		}
		if code := decodeCode(t, recorder); code != "mirror_read_only" {
			t.Fatalf("%s code = %q", action, code)
		}
	}
}

func TestBridgeMessageRepliesAndReactionsAreReadOnly(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"}, nil)
	// A reply whose `re` names the mirrored root.
	raw := []byte(`{"re":"` + formatID(world.taskID) + `","text":"hello"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/board/messages", bytes.NewReader(raw))
	recorder := httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("reply status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("reply code = %q", code)
	}
	// A reaction on the mirrored root.
	request = httptest.NewRequest(http.MethodPost,
		"/v1/board/messages/"+formatID(world.taskID)+"/reactions", strings.NewReader(`{"emoji":"+1"}`))
	recorder = httptest.NewRecorder()
	world.surf.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("reaction status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if code := decodeCode(t, recorder); code != "mirror_read_only" {
		t.Fatalf("reaction code = %q", code)
	}
}

func TestBridgeDoesNotImpersonateTheWorkerSession(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"},
		func(w *bridgeWorld) { activeWorld(t, w) })
	world.post("complete", map[string]any{"result": "done"})
	world.super.mu.Lock()
	report := world.super.lastReport
	world.super.mu.Unlock()
	if report.CallerSessionID != uuid.Nil {
		t.Fatalf("the bridge impersonated worker session %s", report.CallerSessionID)
	}
}

func TestBridgeUnblockPassesOptionID(t *testing.T) {
	world := newBridgeWorld(t, SuperagentConfig{Mirror: true, Bridge: true, Channel: "superagent"},
		func(w *bridgeWorld) {
			activeWorld(t, w)
			assignment := newestAssignment(t, w)
			attention := addAttention(t, w.store, w.ticket, assignment, collab.AttentionClarify, collab.AttentionOpen)
			w.attentionID = attention.ID
			updateTicketStatus(t, w.store, w.tenant, w.ticket.ID, collab.WorkItemBlocked)
		})
	world.super.behavior = func(string) error {
		attention, err := world.store.GetAttention(context.Background(), world.tenant, world.attentionID)
		if err != nil {
			return err
		}
		answerAttention(t, world.store, attention, "yes")
		updateTicketStatus(t, world.store, world.tenant, world.ticket.ID, collab.WorkItemActive)
		return nil
	}
	recorder := world.post("unblock", map[string]any{"optionId": "yes", "reason": "approved"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	world.super.mu.Lock()
	option, text := world.super.lastAnswerOpt, world.super.lastAnswerText
	world.super.mu.Unlock()
	if option != "yes" {
		t.Fatalf("optionId = %q", option)
	}
	if text == "" {
		t.Fatal("free-text answer lost")
	}
}
