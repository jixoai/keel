package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	st, err := store.Open(context.Background(), store.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func newSession(t *testing.T, st *store.Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenant := uuid.New()
	session := uuid.New()
	ctx := context.Background()
	if err := st.CreateTenant(ctx, tenant, "agent-test"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, tenant, session); err != nil {
		t.Fatal(err)
	}
	return tenant, session
}

func eventsOf(t *testing.T, st *store.Store, tenant, session uuid.UUID) []store.Event {
	t.Helper()
	events, err := st.LoadEvents(context.Background(), tenant, session, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func countKind(evs []store.Event, kind string) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func newTestRunner(st *store.Store) *Runner {
	return NewRunner(st, FakeStream(), "fake", "fake", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRunnerTextReply(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(st)
	if _, err := r.Run(ctx, store.Run{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, st, tenant, session)
	if len(events) != 2 {
		t.Fatalf("want user + assistant, got %d events", len(events))
	}
	if events[1].Kind != store.EventAssistantMessage {
		t.Fatalf("second event = %s", events[1].Kind)
	}
	var msg agentcore.AssistantMessage
	if err := json.Unmarshal(events[1].Payload, &msg); err != nil {
		t.Fatal(err)
	}
	if got := ContentText(msg.Content); got != "fake provider received: hello" {
		t.Fatalf("reply = %q", got)
	}

	// A second run with no new input must be a no-op.
	before := len(events)
	if _, err := r.Run(ctx, store.Run{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	if after := len(eventsOf(t, st, tenant, session)); after != before {
		t.Fatalf("idle run appended events: %d -> %d", before, after)
	}
}

func TestRunnerToolRoundTrip(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "please echo this"}); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(st)
	if _, err := r.Run(ctx, store.Run{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, st, tenant, session)
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	want := []string{store.EventUserMessage, store.EventAssistantMessage, store.EventToolResult, store.EventAssistantMessage}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("events = %v, want %v", kinds, want)
		}
	}
	var tr agentcore.ToolResultMessage
	if err := json.Unmarshal(events[2].Payload, &tr); err != nil {
		t.Fatal(err)
	}
	if ContentText(tr.Content) != "please echo this" {
		t.Fatalf("tool result = %q", ContentText(tr.Content))
	}
	var final agentcore.AssistantMessage
	if err := json.Unmarshal(events[3].Payload, &final); err != nil {
		t.Fatal(err)
	}
	if got := ContentText(final.Content); got != "tool echo returned: please echo this" {
		t.Fatalf("final = %q", got)
	}
}

func TestRunnerResumesUnresolvedToolCall(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "echo after crash"}); err != nil {
		t.Fatal(err)
	}
	// Simulate a process that died after committing the assistant tool call but
	// before committing the tool result.
	call := agentcore.AssistantMessage{
		RoleField: agentcore.RoleAssistant,
		Content: agentcore.ContentList{
			agentcore.NewToolCallContent("call-crash", "echo", json.RawMessage(`{"x":"echo after crash"}`)),
		},
		StopReason: agentcore.StopReasonToolUse,
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventAssistantMessage, call); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(st)
	if _, err := r.Run(ctx, store.Run{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	events := eventsOf(t, st, tenant, session)
	var toolResults int
	var final string
	for _, ev := range events {
		switch ev.Kind {
		case store.EventToolResult:
			toolResults++
		case store.EventAssistantMessage:
			var m agentcore.AssistantMessage
			_ = json.Unmarshal(ev.Payload, &m)
			if m.StopReason == agentcore.StopReasonEndTurn {
				final = ContentText(m.Content)
			}
		}
	}
	if toolResults != 1 {
		t.Fatalf("tool results = %d, want 1", toolResults)
	}
	if final != "tool echo returned: echo after crash" {
		t.Fatalf("final = %q", final)
	}

	// A second execution (lease expiry / duplicate enqueue) must not add a
	// second tool result or a second final answer.
	if _, err := r.Run(ctx, store.Run{TenantID: tenant, SessionID: session}); err != nil {
		t.Fatal(err)
	}
	again := eventsOf(t, st, tenant, session)
	if got := countKind(again, store.EventToolResult); got != 1 {
		t.Fatalf("tool results after re-run = %d, want 1", got)
	}
	if got := countKind(again, store.EventAssistantMessage); got != 2 {
		t.Fatalf("assistant messages after re-run = %d, want 2 (tool call + final)", got)
	}
}

func TestToolResultDedupeKey(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	msg := agentcore.ToolResultMessage{
		RoleField:  agentcore.RoleToolResult,
		ToolCallID: "call-1",
		ToolName:   "echo",
		Content:    agentcore.ContentList{agentcore.NewTextContent("x")},
	}
	seq1, inserted1, err := st.AppendEventDedup(ctx, tenant, session, store.EventToolResult, msg, "tool:call-1")
	if err != nil {
		t.Fatal(err)
	}
	seq2, inserted2, err := st.AppendEventDedup(ctx, tenant, session, store.EventToolResult, msg, "tool:call-1")
	if err != nil {
		t.Fatal(err)
	}
	if !inserted1 || inserted2 {
		t.Fatalf("inserted flags = %v/%v", inserted1, inserted2)
	}
	if seq1 != seq2 {
		t.Fatalf("seq = %d/%d, want the same", seq1, seq2)
	}
	if events := eventsOf(t, st, tenant, session); len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

func TestRunnerSkipsDuplicateFinal(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "one"}); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(st)
	run := store.Run{TenantID: tenant, SessionID: session}
	if _, err := r.Run(ctx, run); err != nil {
		t.Fatal(err)
	}
	// Concurrent workers can claim the same session after a lease expiry; a
	// second execution must not answer twice.
	if _, err := r.Run(ctx, run); err != nil {
		t.Fatal(err)
	}
	var assistants int
	for _, ev := range eventsOf(t, st, tenant, session) {
		if ev.Kind == store.EventAssistantMessage {
			assistants++
		}
	}
	if assistants != 1 {
		t.Fatalf("assistant messages = %d, want 1", assistants)
	}
}
