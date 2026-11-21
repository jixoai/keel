package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// mcpDetailsTool stands in for the aggregate `mcp` tool's call action: its
// model-facing content is plain text, while Details carries the app binding and
// the structured content the UI needs. Details must survive the ledger and must
// never reach the model.
type mcpDetailsTool struct{}

func (mcpDetailsTool) Name() string            { return "mcp" }
func (mcpDetailsTool) Description() string     { return "MCP aggregate" }
func (mcpDetailsTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (mcpDetailsTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

func (mcpDetailsTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("visible result text")},
		Details: map[string]any{
			"server": "docs",
			"tool":   "render",
			"app": map[string]any{
				"appId":       "mcpapp_details_marker",
				"resourceUri": "ui://docs/panel",
				"visibility":  []string{"model", "app"},
			},
			"structuredContent": map[string]any{"secret": "structured-details-marker"},
		},
	}, nil
}

// detailsRecordingStream captures every provider request and scripts one tool
// call followed by a final answer.
type detailsRecordingStream struct {
	mu    sync.Mutex
	calls []agentcore.MessageList
}

func (s *detailsRecordingStream) fn() provider.StreamFn {
	return func(ctx context.Context, _ string, llm provider.LlmContext, _ provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s.mu.Lock()
		s.calls = append(s.calls, append(agentcore.MessageList(nil), llm.Messages...))
		s.mu.Unlock()
		var message agentcore.AssistantMessage
		last := llm.Messages[len(llm.Messages)-1]
		if _, ok := last.(agentcore.ToolResultMessage); ok {
			message = agentcore.AssistantMessage{
				RoleField:  agentcore.RoleAssistant,
				Content:    agentcore.ContentList{agentcore.NewTextContent("final answer")},
				StopReason: agentcore.StopReasonEndTurn,
			}
		} else {
			message = agentcore.AssistantMessage{
				RoleField: agentcore.RoleAssistant,
				Content: agentcore.ContentList{
					agentcore.NewToolCallContent("details-call-1", "mcp", json.RawMessage(`{}`)),
				},
				StopReason: agentcore.StopReasonToolUse,
			}
		}
		stream := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer stream.Close()
			_ = stream.Emit(ctx, provider.StreamStartEvent{Partial: message})
			_ = stream.Emit(ctx, provider.StreamDoneEvent{Message: message})
		}()
		return stream, nil
	}
}

func (s *detailsRecordingStream) messages() []agentcore.MessageList {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agentcore.MessageList(nil), s.calls...)
}

// TestMCPToolDetailsSurviveTheLedgerAndStayOutOfTheModelContext pins P1.4: the
// app binding and the structured content travel in tool.result details, can be
// read back from the committed log, and never appear in any provider request.
func TestMCPToolDetailsSurviveTheLedgerAndStayOutOfTheModelContext(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "details-r1", "render the panel", "", nil)

	stream := &detailsRecordingStream{}
	runner := NewRunner(k, k, stream.fn(), "fake", "fake", "", discardLog())
	runner.ToolAccess = AllowAllToolAccess{}
	if err := runner.Tools.Register(mcpDetailsTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// The committed tool.result carries the details verbatim.
	var toolResult *store.Event
	events := eventsOf(t, st, tenant, session)
	for index := range events {
		if events[index].Kind == store.EventToolResult {
			toolResult = &events[index]
			break
		}
	}
	if toolResult == nil {
		t.Fatal("no tool.result was committed")
	}
	var payload struct {
		ToolCallID string         `json:"toolCallId"`
		Details    map[string]any `json:"details"`
		Content    []any          `json:"content"`
	}
	if err := json.Unmarshal(toolResult.Payload, &payload); err != nil {
		t.Fatalf("decode tool.result: %v", err)
	}
	if payload.ToolCallID != "details-call-1" {
		t.Fatalf("toolCallId = %q", payload.ToolCallID)
	}
	app, ok := payload.Details["app"].(map[string]any)
	if !ok || app["appId"] != "mcpapp_details_marker" || app["resourceUri"] != "ui://docs/panel" {
		t.Fatalf("details.app = %#v", payload.Details["app"])
	}
	structured, ok := payload.Details["structuredContent"].(map[string]any)
	if !ok || structured["secret"] != "structured-details-marker" {
		t.Fatalf("details.structuredContent = %#v", payload.Details["structuredContent"])
	}
	if !strings.Contains(string(toolResult.Payload), "visible result text") {
		t.Fatalf("tool.result content = %s", toolResult.Payload)
	}

	// The model path reads the same details back from the committed log: they
	// are part of the durable tool result, not of a second in-memory structure.
	decoded, _ := DecodeMessages(events, nil)
	var decodedResult agentcore.ToolResultMessage
	found := false
	for _, message := range decoded {
		if candidate, ok := message.(agentcore.ToolResultMessage); ok {
			decodedResult = candidate
			found = true
		}
	}
	if !found {
		t.Fatal("the decoded transcript has no tool result")
	}
	decodedDetails, ok := decodedResult.Details.(map[string]any)
	if !ok || decodedDetails["server"] != "docs" {
		t.Fatalf("decoded details = %#v", decodedResult.Details)
	}

	// The model saw the content, never the details: the provider-facing text of
	// every message is free of the details markers. (The wire encoders are
	// pinned separately in internal/provider: Details never reach a request.)
	calls := stream.messages()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	for index, messages := range calls {
		for _, message := range messages {
			var text string
			switch typed := message.(type) {
			case agentcore.UserMessage:
				text = agentcore.ContentToText(typed.Content)
			case agentcore.AssistantMessage:
				text = agentcore.ContentToText(typed.Content)
			case agentcore.ToolResultMessage:
				text = agentcore.ContentToText(typed.Content)
			}
			for _, forbidden := range []string{"mcpapp_details_marker", "structured-details-marker"} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("provider call %d model text leaked %q: %s", index, forbidden, text)
				}
			}
		}
	}
	second, err := json.Marshal(calls[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "visible result text") {
		t.Fatalf("the second provider call did not carry the tool content: %s", second)
	}
}
