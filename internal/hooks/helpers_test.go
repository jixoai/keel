package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// recorder collects audit records and hook errors for tests. It is safe for
// concurrent use because tool batches may dispatch in parallel.
type recorder struct {
	mu     sync.Mutex
	audits []AuditRecord
	errs   []HookError
}

func (r *recorder) Record(record AuditRecord) {
	r.mu.Lock()
	r.audits = append(r.audits, record)
	r.mu.Unlock()
}

func (r *recorder) onError(err HookError) {
	r.mu.Lock()
	r.errs = append(r.errs, err)
	r.mu.Unlock()
}

func (r *recorder) auditRecords() []AuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AuditRecord(nil), r.audits...)
}

func (r *recorder) errors() []HookError {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]HookError(nil), r.errs...)
}

func newTestRegistry(t *testing.T) (*Registry, *recorder) {
	t.Helper()
	rec := &recorder{}
	reg := NewRegistry(WithAuditSink(rec), WithStderrWriter(io.Discard))
	reg.OnError(rec.onError)
	return reg, rec
}

// eventLog records event names or handler names in dispatch order.
type eventLog struct {
	mu     sync.Mutex
	values []string
}

func (l *eventLog) add(value string) {
	l.mu.Lock()
	l.values = append(l.values, value)
	l.mu.Unlock()
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.values...)
}

func assertStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func textMessage(role string, text string) agentcore.Message {
	content := agentcore.ContentList{agentcore.NewTextContent(text)}
	switch role {
	case agentcore.RoleUser:
		return agentcore.UserMessage{RoleField: role, Content: content}
	case agentcore.RoleAssistant:
		return agentcore.AssistantMessage{RoleField: role, Content: content}
	default:
		panic("unsupported role " + role)
	}
}

func messageText(message agentcore.Message) string {
	switch m := message.(type) {
	case agentcore.UserMessage:
		return agentcore.ContentToText(m.Content)
	case agentcore.AssistantMessage:
		return agentcore.ContentToText(m.Content)
	default:
		return ""
	}
}

// testTool is a minimal agentcore.AgentTool used by wrapper tests.
type testTool struct {
	name        string
	description string
	schema      json.RawMessage
	mode        agentcore.ToolExecutionMode

	mu       sync.Mutex
	calls    int
	lastArgs json.RawMessage
	fn       func(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error)
}

func (t *testTool) Name() string        { return t.name }
func (t *testTool) Description() string { return t.description }
func (t *testTool) Schema() json.RawMessage {
	if t.schema == nil {
		return json.RawMessage(`{"type":"object"}`)
	}
	return t.schema
}
func (t *testTool) ExecutionMode() agentcore.ToolExecutionMode {
	if t.mode == "" {
		return agentcore.ToolExecutionParallel
	}
	return t.mode
}

func (t *testTool) Execute(ctx context.Context, id string, args json.RawMessage, onUpdate agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.lastArgs = append(json.RawMessage(nil), args...)
	t.mu.Unlock()
	if t.fn != nil {
		return t.fn(ctx, id, args, onUpdate)
	}
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("ok")}}, nil
}

func (t *testTool) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func (t *testTool) args() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append(json.RawMessage(nil), t.lastArgs...)
}

// TestCommandHelperProcess is the child process used by command hook tests. It
// only runs when GO_WANT_HOOK_HELPER=1; every branch exits explicitly so the
// testing framework never writes to the hook protocol stdout.
func TestCommandHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_HOOK_HELPER") != "1" {
		return
	}
	mode := os.Getenv("HOOK_HELPER_MODE")
	payload, _ := io.ReadAll(os.Stdin)
	switch mode {
	case "allow":
		fmt.Print(`{"decision":"allow"}`)
	case "deny":
		fmt.Print(`{"decision":"deny","reason":"blocked by helper"}`)
	case "modify":
		fmt.Print(`{"decision":"modify","input":{"x":"modified"}}`)
	case "cc-updated-input":
		fmt.Print(`{"hookSpecificOutput":{"permissionDecision":"allow","updatedInput":{"x":"cc"}}}`)
	case "stderr-deny":
		fmt.Fprintln(os.Stderr, "dangerous command")
		os.Exit(2)
	case "stderr-allow":
		fmt.Fprintln(os.Stderr, "note: inspected")
		fmt.Print(`{"decision":"allow"}`)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "invalid":
		fmt.Print("not json")
	case "exit1":
		os.Exit(1)
	case "exit2-json":
		fmt.Print(`{"decision":"deny","reason":"json deny"}`)
		os.Exit(2)
	case "echo-event":
		var input struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(payload, &input)
		fmt.Printf(`{"decision":"deny","reason":"event=%s"}`, input.Event)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", mode)
		os.Exit(3)
	}
	os.Exit(0)
}

func helperCommandConfig(t *testing.T, name, mode string) CommandHookConfig {
	t.Helper()
	return CommandHookConfig{
		Name:    name,
		Event:   EventToolCall,
		Command: []string{os.Args[0], "-test.run=TestCommandHelperProcess"},
		Timeout: "5s",
		Env: map[string]string{
			"GO_WANT_HOOK_HELPER": "1",
			"HOOK_HELPER_MODE":    mode,
		},
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}
