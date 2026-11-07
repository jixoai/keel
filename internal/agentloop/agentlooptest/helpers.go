package agentlooptest

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agenttool"
)

const (
	// caseTimeout bounds every case. Cases are deterministic; the bound only
	// catches a loop that never settles.
	caseTimeout = 20 * time.Second

	// sessionID is the run identity every case uses. It is carried in the first
	// event, so it also proves the loop forwards Input.SessionID.
	sessionID = "conformance-session"

	// systemPrompt is the host-assembled prompt of the default scenario.
	systemPrompt = "you are the PI conformance agent"
)

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

// baseInput is the canonical input shape. Every case adjusts the transcript,
// tools, hooks or batch wiring from here.
func baseInput() agentloop.Input {
	return agentloop.Input{
		SessionID: sessionID,
		Context: agentloop.Context{
			SystemPrompt: systemPrompt,
		},
		Model: agentloop.ModelConfig{
			ID:            "conformance-model",
			Provider:      "conformance",
			APIKey:        "test-key",
			ThinkingLevel: agentcore.ThinkingLow,
		},
		Limits: agentloop.Limits{MaxTurns: 8},
	}
}

// promptInput is a prompt run: the trailing message is this run's new prompt,
// so the loop echoes it and emits its message lifecycle.
func promptInput(text string) agentloop.Input {
	in := baseInput()
	in.Context.Messages = agentcore.MessageList{userMsg(text)}
	in.Context.PromptCount = 1
	return in
}

// continuationInput is a continuation run: no new prompt, no prompt events.
func continuationInput(msgs ...agentcore.AgentMessage) agentloop.Input {
	in := baseInput()
	in.Context.Messages = agentcore.MessageList(msgs)
	in.Context.PromptCount = 0
	return in
}

// dropSystemMessages is the coordinator's provider-boundary filter: transcript
// tool declarations never reach the provider wire.
func dropSystemMessages(msgs agentcore.MessageList) agentcore.MessageList {
	out := make(agentcore.MessageList, 0, len(msgs))
	for _, m := range msgs {
		if m.Role() != agentcore.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Messages and tools
// ---------------------------------------------------------------------------

func userMsg(text string) agentcore.UserMessage {
	return agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent(text)},
	}
}

func systemMsg(text string) agentcore.SystemMessage {
	return agentcore.SystemMessage{RoleField: agentcore.RoleSystem, Content: text}
}

func assistantText(text, stop string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		StopReason: stop,
	}
}

func assistantToolUse(calls ...agentcore.ToolCallContent) agentcore.AssistantMessage {
	content := make(agentcore.ContentList, 0, len(calls))
	for _, c := range calls {
		content = append(content, c)
	}
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    content,
		StopReason: agentcore.StopReasonToolUse,
	}
}

func toolCall(id, name, args string) agentcore.ToolCallContent {
	return agentcore.NewToolCallContent(id, name, json.RawMessage(args))
}

// echoSchema is PI's `obj({ value: str() })` tool schema.
const echoSchema = `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`

// echoTool is the deterministic tool every echo scenario executes.
type echoTool struct {
	executed atomic.Int64
	mu       sync.Mutex
	seen     []string
}

func (t *echoTool) Name() string            { return "echo" }
func (t *echoTool) Description() string     { return "Echo tool" }
func (t *echoTool) Schema() json.RawMessage { return json.RawMessage(echoSchema) }
func (t *echoTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t *echoTool) Execute(_ context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	var in struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agentcore.AgentToolResult{}, err
	}
	t.executed.Add(1)
	t.mu.Lock()
	t.seen = append(t.seen, in.Value)
	t.mu.Unlock()
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent("echoed: " + in.Value)},
	}, nil
}

func (t *echoTool) executions() int64 { return t.executed.Load() }

func (t *echoTool) seenValues() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.seen...)
}

// funcTool is PI's `{ name, parameters, execute }` literal adapted to Go.
type funcTool struct {
	name   string
	schema string
	mode   agentcore.ToolExecutionMode
	run    func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error)
}

func (f funcTool) Name() string                               { return f.name }
func (f funcTool) Description() string                        { return f.name + " tool" }
func (f funcTool) Schema() json.RawMessage                    { return json.RawMessage(f.schema) }
func (f funcTool) ExecutionMode() agentcore.ToolExecutionMode { return f.mode }
func (f funcTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return f.run(ctx, id, args, onUpdate)
}

// registryOf builds an executable registry from the scenario's tools (the
// Batch.Registry seam).
func registryOf(t T, tools ...agentcore.AgentTool) *agenttool.ToolRegistry {
	t.Helper()
	reg := agenttool.NewToolRegistry()
	for _, tool := range tools {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// runLoop drives one run to completion and returns the observed events and the
// produced messages.
func runLoop(t T, l agentloop.Loop, in agentloop.Input) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), caseTimeout)
	defer cancel()
	stream, err := l.Run(ctx, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var events []agentcore.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
	}
	result, err := stream.Result(ctx)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	return events, result
}

// liveRun collects a run's events while the case drives it (completion order,
// cancellation).
type liveRun struct {
	mu     sync.Mutex
	events []agentcore.AgentEvent
	done   chan struct{}
}

func collectRun(events <-chan agentcore.AgentEvent) *liveRun {
	r := &liveRun{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		for ev := range events {
			r.mu.Lock()
			r.events = append(r.events, ev)
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *liveRun) snapshot() []agentcore.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentcore.AgentEvent{}, r.events...)
}

func (r *liveRun) wait(t T, what string) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(caseTimeout):
		t.Fatalf("%s did not settle", what)
	}
}

// ---------------------------------------------------------------------------
// Shapes: comparable projections with volatile fields removed (timestamps,
// durations, provider metadata) — the whitelist the change allows.
// ---------------------------------------------------------------------------

type msgShape struct {
	Role          string
	Text          string
	StopReason    string
	ToolCallID    string
	ToolName      string
	IsError       bool
	Calls         []string
	DeclaredTools []string
	RemovedTools  []string
}

func shapeOf(m agentcore.AgentMessage) msgShape {
	sh := msgShape{Role: m.Role()}
	switch msg := m.(type) {
	case agentcore.AssistantMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
		sh.StopReason = msg.StopReason
		for _, c := range msg.ToolCalls() {
			sh.Calls = append(sh.Calls, c.ID+"|"+c.Name+"|"+string(c.Arguments))
		}
	case agentcore.ToolResultMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
		sh.ToolCallID = msg.ToolCallID
		sh.ToolName = msg.ToolName
		sh.IsError = msg.IsError
	case agentcore.UserMessage:
		sh.Text = agentcore.ContentToText(msg.Content)
	case agentcore.SystemMessage:
		sh.Text = msg.Content
		for _, d := range msg.ToolsAdded {
			sh.DeclaredTools = append(sh.DeclaredTools, d.Name)
		}
		for _, r := range msg.ToolsRemoved {
			sh.RemovedTools = append(sh.RemovedTools, r.Name)
		}
	case agentcore.CompactionMessage:
		sh.Text = msg.Summary
	}
	return sh
}

func shapesOf(msgs []agentcore.AgentMessage) []msgShape {
	out := make([]msgShape, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, shapeOf(m))
	}
	return out
}

// nonSystemShapes drops transcript system messages (tool-loadout declarations
// are an implementation detail of the transcript; the suite asserts what the
// model and the ledger see: prompt, assistant and tool-result messages).
func nonSystemShapes(msgs []agentcore.AgentMessage) []msgShape {
	out := make([]msgShape, 0, len(msgs))
	for _, m := range msgs {
		if m.Role() == agentcore.RoleSystem {
			continue
		}
		out = append(out, shapeOf(m))
	}
	return out
}

func eventTypes(events []agentcore.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.EventType())
	}
	return out
}

// piEventTypes filters Keel's additive observability events so a sequence
// assertion compares exactly PI's core AgentEvent set.
func piEventTypes(events []agentcore.AgentEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		switch ev.(type) {
		case agentcore.TelemetryEvent, agentcore.CompactionEvent,
			agentcore.CompactionStartEvent, agentcore.SubAgentProgressEvent:
			continue
		}
		out = append(out, ev.EventType())
	}
	return out
}

func countEvents(events []agentcore.AgentEvent, want string) int {
	n := 0
	for _, ev := range events {
		if ev.EventType() == want {
			n++
		}
	}
	return n
}

func assertShapesEqual(t T, label string, got, want []msgShape) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got  %v\n want %v", label, got, want)
	}
}

// requestUserTexts returns the user texts of a provider request transcript.
func requestUserTexts(msgs agentcore.MessageList) []string {
	var out []string
	for _, m := range msgs {
		if u, ok := m.(agentcore.UserMessage); ok {
			out = append(out, agentcore.ContentToText(u.Content))
		}
	}
	return out
}

func requestHasUserText(msgs agentcore.MessageList, want string) bool {
	for _, text := range requestUserTexts(msgs) {
		if text == want {
			return true
		}
	}
	return false
}

func messageRoles(msgs []agentcore.AgentMessage) []string {
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		roles = append(roles, m.Role())
	}
	return roles
}

// ---------------------------------------------------------------------------
// Contract checks
// ---------------------------------------------------------------------------

// checkComplete asserts the invariants every run over a Loop must honor:
//
//   - exactly one agent_start, and it is first;
//   - exactly one agent_end, and it is last; its Messages equal the result;
//   - turn_start/turn_end and message_start/message_end are balanced;
//   - every tool_execution_start has its tool_execution_end;
//   - every tool result message matches an earlier tool call (no orphans, no
//     duplicate results);
//   - when resolveAllCalls is true, every tool call has a result (a cancelled
//     run may leave unresolved calls to the coordinator's resume path).
func checkComplete(t T, label string, events []agentcore.AgentEvent, result []agentcore.AgentMessage, resolveAllCalls bool) {
	t.Helper()
	if len(events) == 0 {
		t.Fatalf("%s: no events emitted", label)
	}
	if _, ok := events[0].(agentcore.AgentStartEvent); !ok {
		t.Fatalf("%s: first event = %T, want AgentStartEvent", label, events[0])
	}
	starts, ends := 0, 0
	turnStarts, turnEnds := 0, 0
	endIndex := -1
	openTools := map[string]bool{}
	endedTools := map[string]bool{}
	for i, ev := range events {
		switch e := ev.(type) {
		case agentcore.AgentStartEvent:
			starts++
			if i != 0 {
				t.Fatalf("%s: agent_start at event %d, want first", label, i)
			}
		case agentcore.AgentEndEvent:
			ends++
			if endIndex >= 0 {
				t.Fatalf("%s: multiple agent_end events", label)
			}
			endIndex = i
			assertShapesEqual(t, label+": agent_end messages vs stream result", shapesOf(e.Messages), shapesOf(result))
		case agentcore.TurnStartEvent:
			turnStarts++
			if turnEnds >= turnStarts {
				t.Fatalf("%s: turn_start %d after %d turn_end events", label, turnStarts, turnEnds)
			}
		case agentcore.TurnEndEvent:
			turnEnds++
			if turnStarts == 0 || turnEnds > turnStarts {
				t.Fatalf("%s: turn_end %d without a matching turn_start", label, turnEnds)
			}
		case agentcore.ToolExecutionStartEvent:
			if openTools[e.ToolCallID] {
				t.Fatalf("%s: tool_execution_start for %q emitted twice without an end", label, e.ToolCallID)
			}
			openTools[e.ToolCallID] = true
		case agentcore.ToolExecutionEndEvent:
			if !openTools[e.ToolCallID] {
				t.Fatalf("%s: tool_execution_end for %q without a start", label, e.ToolCallID)
			}
			delete(openTools, e.ToolCallID)
			endedTools[e.ToolCallID] = true
		case agentcore.MessageStartEvent:
			if tr, ok := e.Message.(agentcore.ToolResultMessage); ok && !endedTools[tr.ToolCallID] {
				t.Fatalf("%s: tool result %q announced before its tool_execution_end", label, tr.ToolCallID)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("%s: agent_start events = %d, want 1", label, starts)
	}
	if endIndex < 0 {
		t.Fatalf("%s: no agent_end; events = %v", label, piEventTypes(events))
	}
	if endIndex != len(events)-1 {
		t.Fatalf("%s: events after agent_end: %v", label, piEventTypes(events[endIndex+1:]))
	}
	if turnStarts != turnEnds {
		t.Fatalf("%s: turn_start/turn_end = %d/%d, want balanced", label, turnStarts, turnEnds)
	}
	if len(openTools) != 0 {
		t.Fatalf("%s: tool executions never ended: %v", label, openTools)
	}
	shapes := shapesOf(result)
	messageStarts, messageEnds := 0, 0
	for _, ev := range events {
		switch ev.(type) {
		case agentcore.MessageStartEvent:
			messageStarts++
		case agentcore.MessageEndEvent:
			messageEnds++
		}
	}
	if messageStarts != messageEnds {
		t.Fatalf("%s: message_start/message_end = %d/%d, want balanced (events %v)", label, messageStarts, messageEnds, piEventTypes(events))
	}
	checkToolPairing(t, label, shapes, resolveAllCalls)
}

// checkToolPairing asserts every tool result has a matching earlier call in
// the same produced transcript, that results are unique, and (optionally) that
// every call was resolved.
func checkToolPairing(t T, label string, shapes []msgShape, resolveAllCalls bool) {
	t.Helper()
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, sh := range shapes {
		switch sh.Role {
		case agentcore.RoleAssistant:
			for _, call := range sh.Calls {
				id := call
				for i := 0; i < len(call); i++ {
					if call[i] == '|' {
						id = call[:i]
						break
					}
				}
				if calls[id] {
					t.Fatalf("%s: tool call %q declared twice in the produced transcript: %v", label, id, shapes)
				}
				calls[id] = true
			}
		case agentcore.RoleToolResult:
			if !calls[sh.ToolCallID] {
				t.Fatalf("%s: tool result %q has no matching call: %v", label, sh.ToolCallID, shapes)
			}
			if results[sh.ToolCallID] {
				t.Fatalf("%s: duplicate tool result for %q: %v", label, sh.ToolCallID, shapes)
			}
			results[sh.ToolCallID] = true
		}
	}
	if !resolveAllCalls {
		return
	}
	for id := range calls {
		if !results[id] {
			t.Fatalf("%s: tool call %q has no result: %v", label, id, shapes)
		}
	}
}

// promptLifecycle asserts the trailing prompt received its message lifecycle
// before the first turn started (PI's agentLoop prompt contract).
func promptLifecycle(t T, label string, events []agentcore.AgentEvent, prompt agentcore.AgentMessage) {
	t.Helper()
	sawStart, sawEnd := false, false
	for _, ev := range events {
		_, isTurnStart := ev.(agentcore.TurnStartEvent)
		switch e := ev.(type) {
		case agentcore.MessageStartEvent:
			if reflect.DeepEqual(shapeOf(e.Message), shapeOf(prompt)) {
				sawStart = true
			}
		case agentcore.MessageEndEvent:
			if reflect.DeepEqual(shapeOf(e.Message), shapeOf(prompt)) {
				sawEnd = true
			}
		}
		if isTurnStart {
			break
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("%s: prompt message lifecycle missing before the first turn (start=%v end=%v)", label, sawStart, sawEnd)
	}
}
