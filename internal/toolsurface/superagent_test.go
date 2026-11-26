package toolsurface

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/access"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
)

// errorDoc reads the error document of an internal-package tool result. The
// external test package has its own copy (errorDocument); this one is for the
// unexported constructors exercised here.
func errorDoc(t *testing.T, result agentcore.AgentToolResult) map[string]any {
	t.Helper()
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want map", result.StructuredContent)
	}
	problem, ok := document["error"].(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want an error document", document)
	}
	return problem
}

type codedRefusal struct {
	code     string
	ticketID string
	status   string
}

func (e *codedRefusal) Error() string            { return "the ticket refuses this call" }
func (e *codedRefusal) ToolCode() string         { return e.code }
func (e *codedRefusal) ToolTicketID() string     { return e.ticketID }
func (e *codedRefusal) ToolTicketStatus() string { return e.status }

type fakeSuperagentBackend struct {
	reportInput  SuperagentReport
	askInput     SuperagentAsk
	reportResult SuperagentReportResult
	askResult    SuperagentAskResult
	err          error
	reportCalls  int
	askCalls     int
}

func (f *fakeSuperagentBackend) ReportFromWorker(_ context.Context, scope harnessext.RunScope, input SuperagentReport) (SuperagentReportResult, error) {
	f.reportCalls++
	f.reportInput = input
	if f.err != nil {
		return SuperagentReportResult{}, f.err
	}
	if f.reportResult.TicketID == "" {
		return SuperagentReportResult{
			TicketID: "ticket-1", TicketStatus: "active",
			AssignmentID: "assignment-1", AssignmentStatus: "active", State: input.State, Created: true,
		}, nil
	}
	return f.reportResult, nil
}

func (f *fakeSuperagentBackend) AskFromWorker(_ context.Context, scope harnessext.RunScope, input SuperagentAsk) (SuperagentAskResult, error) {
	f.askCalls++
	f.askInput = input
	if f.err != nil {
		return SuperagentAskResult{}, f.err
	}
	if f.askResult.AttentionID == "" {
		return SuperagentAskResult{
			AttentionID: "attention-1", AttentionStatus: "open",
			TicketID: "ticket-1", TicketStatus: "active",
			Prompt: input.Prompt, Kind: "decision", Options: input.Options,
			AllowFreeText: input.AllowFreeText, Route: "channel", Channel: "issue#1",
		}, nil
	}
	return f.askResult, nil
}

func successDocument(t *testing.T, result agentcore.AgentToolResult) map[string]any {
	t.Helper()
	document, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v, want map", result.StructuredContent)
	}
	if _, hasError := document["error"]; hasError {
		t.Fatalf("structured content = %#v, want success", document)
	}
	return document
}

func workerToolContext(sessionID uuid.UUID) context.Context {
	return harnessext.WithRunScope(context.Background(), harnessext.RunScope{
		TenantID: uuid.New(), SessionID: sessionID, RunID: "run-1",
	})
}

func TestSuperagentToolsRequireRunScope(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	tools := NewSuperagentTools(backend)
	if len(tools) != 2 || tools[0].Name() != Report || tools[1].Name() != Ask {
		t.Fatalf("tools = %v", tools)
	}
	for _, tool := range tools {
		result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"state":"progress","prompt":"hi"}`), nil)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeSuperagentScopeRequired {
			t.Fatalf("%s code = %v, want %s", tool.Name(), code, CodeSuperagentScopeRequired)
		}
	}
	if backend.reportCalls != 0 || backend.askCalls != 0 {
		t.Fatalf("a scope-less call reached the backend: %d/%d", backend.reportCalls, backend.askCalls)
	}
}

func TestReportToolUsesSessionScopeAndIgnoresTicketArgs(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	scope := uuid.New()
	tool := newReportTool(backend)
	result, err := tool.Execute(workerToolContext(scope), "call-1", json.RawMessage(
		`{"state":"progress","summary":"halfway","requestId":"r1","ticketId":"other-ticket","assignmentId":"other-assignment"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	successDocument(t, result)
	if backend.reportInput.State != "progress" || backend.reportInput.Summary != "halfway" || backend.reportInput.RequestID != "r1" {
		t.Fatalf("backend input = %+v", backend.reportInput)
	}
	// SuperagentReport has no ticket/assignment field: the model's extra keys
	// cannot cross the boundary.
	raw, _ := json.Marshal(backend.reportInput)
	if strings.Contains(string(raw), "other-ticket") || strings.Contains(string(raw), "other-assignment") {
		t.Fatalf("model-supplied ticket ids reached the backend: %s", raw)
	}
	document := result.StructuredContent.(map[string]any)
	ticket, _ := document["ticket"].(map[string]any)
	if ticket["id"] != "ticket-1" || ticket["status"] != "active" {
		t.Fatalf("result ticket = %v", ticket)
	}
}

func TestReportToolDerivesIdempotencyKeyFromCall(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	scope := uuid.New()
	tool := newReportTool(backend)
	if _, err := tool.Execute(workerToolContext(scope), "call-42", json.RawMessage(`{"state":"progress"}`), nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := "tool:report:" + scope.String() + ":call-42"
	if backend.reportInput.RequestID != want {
		t.Fatalf("request id = %q, want %q", backend.reportInput.RequestID, want)
	}
}

func TestReportToolValidatesStateAndWaitingAttention(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	tool := newReportTool(backend)
	ctx := workerToolContext(uuid.New())

	for _, args := range []string{
		`{"state":"bogus"}`,
		`{"state":"waiting"}`,
		`{"state":"progress","attentionId":"not-a-uuid"}`,
		`not-json`,
	} {
		result, err := tool.Execute(ctx, "call-1", json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeInvalidArguments {
			t.Fatalf("%s code = %v, want %s", args, code, CodeInvalidArguments)
		}
	}
	if backend.reportCalls != 0 {
		t.Fatalf("invalid arguments reached the backend %d times", backend.reportCalls)
	}
}

func TestAskToolParsesOptionsAndDerivesRequestID(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	scope := uuid.New()
	tool := newAskTool(backend)
	result, err := tool.Execute(workerToolContext(scope), "call-7", json.RawMessage(
		`{"prompt":"  Which branch?  ","options":[{"id":" main ","label":"main"},{"id":"next"}],"allowFreeText":true,"timeoutMs":1000}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	successDocument(t, result)
	if backend.askInput.Prompt != "Which branch?" || len(backend.askInput.Options) != 2 {
		t.Fatalf("backend input = %+v", backend.askInput)
	}
	if backend.askInput.Options[0].ID != "main" || !backend.askInput.AllowFreeText || backend.askInput.TimeoutMS != 1000 {
		t.Fatalf("backend input = %+v", backend.askInput)
	}
	if want := "tool:ask:" + scope.String() + ":call-7"; backend.askInput.RequestID != want {
		t.Fatalf("request id = %q, want %q", backend.askInput.RequestID, want)
	}
	document := result.StructuredContent.(map[string]any)
	attention, _ := document["attention"].(map[string]any)
	if attention["id"] != "attention-1" || attention["status"] != "open" {
		t.Fatalf("result attention = %v", attention)
	}
}

func TestAskToolValidatesBeforeTheBackend(t *testing.T) {
	backend := &fakeSuperagentBackend{}
	tool := newAskTool(backend)
	ctx := workerToolContext(uuid.New())
	for _, args := range []string{
		`{"prompt":""}`,
		`{"prompt":"question?"}`,
		`{"prompt":"question?","options":[{"id":"a"},{"id":"a"}]}`,
		`{"prompt":"question?","options":[{"label":"no id"}],"allowFreeText":true}`,
		`{"prompt":"question?","allowFreeText":true,"timeoutMs":-1}`,
	} {
		result, err := tool.Execute(ctx, "call-1", json.RawMessage(args), nil)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeInvalidArguments {
			t.Fatalf("%s code = %v, want %s", args, code, CodeInvalidArguments)
		}
	}
	if backend.askCalls != 0 {
		t.Fatalf("invalid arguments reached the backend %d times", backend.askCalls)
	}
}

func TestSuperagentToolRefusalKeepsCodeAndTicketState(t *testing.T) {
	backend := &fakeSuperagentBackend{err: &codedRefusal{
		code: CodeSuperagentNotWorker, ticketID: "ticket-9", status: "blocked",
	}}
	tool := newReportTool(backend)
	result, err := tool.Execute(workerToolContext(uuid.New()), "call-1", json.RawMessage(`{"state":"progress"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	problem := errorDoc(t, result)
	if problem["code"] != CodeSuperagentNotWorker {
		t.Fatalf("code = %v", problem["code"])
	}
	if problem["ticketId"] != "ticket-9" || problem["ticketStatus"] != "blocked" {
		t.Fatalf("refusal ticket = %v", problem)
	}
}

func TestSuperagentToolsUnavailableWithoutBackend(t *testing.T) {
	for _, tool := range NewSuperagentTools(nil) {
		result, err := tool.Execute(workerToolContext(uuid.New()), "call-1",
			json.RawMessage(`{"state":"progress","prompt":"question?"}`), nil)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeSuperagentUnavailable {
			t.Fatalf("%s code = %v, want %s", tool.Name(), code, CodeSuperagentUnavailable)
		}
	}
}

func TestReportToolReturnsStoredStateAndCreated(t *testing.T) {
	backend := &fakeSuperagentBackend{reportResult: SuperagentReportResult{
		TicketID: "ticket-1", TicketStatus: "active",
		AssignmentID: "assignment-1", AssignmentStatus: "active",
		State: "ready", Created: false,
	}}
	tool := newReportTool(backend)
	result, err := tool.Execute(workerToolContext(uuid.New()), "call-1", json.RawMessage(`{"state":"progress"}`), nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	document := successDocument(t, result)
	if document["state"] != "ready" {
		t.Fatalf("state = %v, want the stored state (the input was progress)", document["state"])
	}
	if document["created"] != false {
		t.Fatalf("created = %v, want false", document["created"])
	}
}

func TestWorkerToolInputBounds(t *testing.T) {
	ctx := workerToolContext(uuid.New())

	reportAccepted := func(t *testing.T, args map[string]any) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		backend := &fakeSuperagentBackend{}
		result, err := newReportTool(backend).Execute(ctx, "call-1", raw, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		successDocument(t, result)
		if backend.reportCalls != 1 {
			t.Fatalf("backend calls = %d", backend.reportCalls)
		}
	}
	reportRejected := func(t *testing.T, args map[string]any) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		backend := &fakeSuperagentBackend{}
		result, err := newReportTool(backend).Execute(ctx, "call-1", raw, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeInvalidArguments {
			t.Fatalf("code = %v, want %s", code, CodeInvalidArguments)
		}
		if backend.reportCalls != 0 {
			t.Fatalf("an out-of-bounds report reached the backend")
		}
	}

	reportAccepted(t, map[string]any{"state": "progress", "summary": strings.Repeat("s", maxWorkerReportSummaryBytes)})
	reportRejected(t, map[string]any{"state": "progress", "summary": strings.Repeat("s", maxWorkerReportSummaryBytes+1)})

	askAccepted := func(t *testing.T, args map[string]any) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		backend := &fakeSuperagentBackend{}
		result, err := newAskTool(backend).Execute(ctx, "call-1", raw, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		successDocument(t, result)
		if backend.askCalls != 1 {
			t.Fatalf("backend calls = %d", backend.askCalls)
		}
	}
	askRejected := func(t *testing.T, args map[string]any) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		backend := &fakeSuperagentBackend{}
		result, err := newAskTool(backend).Execute(ctx, "call-1", raw, nil)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if code := errorDoc(t, result)["code"]; code != CodeInvalidArguments {
			t.Fatalf("code = %v, want %s", code, CodeInvalidArguments)
		}
		if backend.askCalls != 0 {
			t.Fatalf("an out-of-bounds ask reached the backend")
		}
	}

	askAccepted(t, map[string]any{
		"prompt": strings.Repeat("p", maxWorkerAskPromptBytes), "allowFreeText": true,
	})
	askRejected(t, map[string]any{
		"prompt": strings.Repeat("p", maxWorkerAskPromptBytes+1), "allowFreeText": true,
	})
	options := func(count int) []map[string]any {
		out := make([]map[string]any, 0, count)
		for index := 0; index < count; index++ {
			out = append(out, map[string]any{"id": "opt-" + strings.Repeat("x", index%5) + string(rune('a'+index%26))})
		}
		return out
	}
	askAccepted(t, map[string]any{"prompt": "q", "options": options(maxWorkerAskOptions)})
	askRejected(t, map[string]any{"prompt": "q", "options": options(maxWorkerAskOptions + 1)})
	askRejected(t, map[string]any{"prompt": "q", "options": []map[string]any{{
		"id": strings.Repeat("i", maxWorkerAskOptionIDBytes+1),
	}}})
	askRejected(t, map[string]any{"prompt": "q", "options": []map[string]any{{
		"id": "a", "label": strings.Repeat("l", maxWorkerAskOptionLabelBytes+1),
	}}})
	askRejected(t, map[string]any{"prompt": "q", "options": []map[string]any{{
		"id": "a", "description": strings.Repeat("d", maxWorkerAskOptionDescBytes+1),
	}}})
	overTotal := make([]map[string]any, 0, 9)
	for index := 0; index < 9; index++ {
		overTotal = append(overTotal, map[string]any{
			"id": "o" + string(rune('a'+index)), "description": strings.Repeat("d", maxWorkerAskOptionDescBytes),
		})
	}
	askRejected(t, map[string]any{"prompt": "q", "options": overTotal})
}

func TestSuperagentToolsAreProductExtrasNotBuiltins(t *testing.T) {
	for _, name := range []string{Report, Ask} {
		found := false
		for _, builtin := range BuiltinNames() {
			if builtin == name {
				found = true
			}
		}
		if found {
			t.Fatalf("%q must not be in the constant builtin surface", name)
		}
		if key, ok := RequiredKey(name, ""); !ok || key != access.KeyTaskWrite {
			t.Fatalf("RequiredKey(%q) = %q, %v; want task.write", name, key, ok)
		}
	}
}
