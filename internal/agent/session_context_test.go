// P5.5 runner seam: the MCP app context reaches the attempt prompt and never
// the ledger. The fake provider echoes the last user text, so the assertion is
// a direct observation of what the model saw.

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/store"
)

func TestSessionContextReachesPromptWithoutLedgerGain(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	ctx := context.Background()
	r, k := newTestRunner(t, st)
	calls := 0
	r.SessionContexts = func(_ context.Context, scope kernel.Scope) []agentcore.Content {
		calls++
		if scope.TenantID != tenant || scope.SessionID != session {
			t.Errorf("scope = %+v, want the run's tenant/session", scope)
		}
		return []agentcore.Content{agentcore.NewTextContent(
			"<mcp_app_context>\n- app mcpapp_fixture: viewing invoice 42\n</mcp_app_context>")}
	}
	if _, err := st.AppendEvent(ctx, tenant, session, store.EventUserMessage, map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := execRun(t, k, r, tenant, session); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("SessionContexts calls = %d, want 1", calls)
	}
	events := eventsOf(t, st, tenant, session)
	if len(events) != 2 {
		t.Fatalf("events = %d, want user + assistant (no new kind)", len(events))
	}
	if events[0].Kind != store.EventUserMessage || events[1].Kind != store.EventAssistantMessage {
		t.Fatalf("kinds = %s,%s", events[0].Kind, events[1].Kind)
	}
	// The ledger still carries the user's original text only.
	var input map[string]string
	if err := json.Unmarshal(events[0].Payload, &input); err != nil {
		t.Fatal(err)
	}
	if input["text"] != "hello" {
		t.Fatalf("committed user text = %q, want the context never persisted", input["text"])
	}
	// The model saw the context merged into the last user message.
	var assistant agentcore.AssistantMessage
	if err := json.Unmarshal(events[1].Payload, &assistant); err != nil {
		t.Fatal(err)
	}
	reply := ContentText(assistant.Content)
	if !strings.Contains(reply, "<mcp_app_context>") || !strings.Contains(reply, "invoice 42") || !strings.Contains(reply, "hello") {
		t.Fatalf("model prompt = %q, want context + user text", reply)
	}
}

func TestPrependSessionContextCopiesTheMessage(t *testing.T) {
	original := agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   agentcore.ContentList{agentcore.NewTextContent("hello")},
	}
	msgs := agentcore.MessageList{original}
	got := prependSessionContext(msgs, []agentcore.Content{agentcore.NewTextContent("ctx")})
	if len(got) != 1 {
		t.Fatalf("messages = %d", len(got))
	}
	merged, ok := got[0].(agentcore.UserMessage)
	if !ok {
		t.Fatalf("message = %T", got[0])
	}
	if text := ContentText(merged.Content); text != "ctx\nhello" && text != "ctxhello" {
		t.Fatalf("merged text = %q", text)
	}
	if text := ContentText(original.Content); text != "hello" {
		t.Fatalf("the original was mutated: %q", text)
	}
	// No user message: the context is still presented.
	onlyContext := prependSessionContext(agentcore.MessageList{}, []agentcore.Content{agentcore.NewTextContent("ctx")})
	if len(onlyContext) != 1 || ContentText(onlyContext[0].(agentcore.UserMessage).Content) != "ctx" {
		t.Fatalf("context-only transcript = %#v", onlyContext)
	}
}
