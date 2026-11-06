package durable

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
)

// fauxStep is one scripted provider response. It may block, which the tests use
// to hold a run "busy" until a gate opens or the run's abort signal fires.
type fauxStep func(ctx context.Context, req Request) (agentcore.AssistantMessage, error)

type fauxProvider struct {
	mu      sync.Mutex
	steps   []fauxStep
	next    int
	calls   int
	checked []Request
}

func (f *fauxProvider) setResponses(steps ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = nil
	for _, step := range steps {
		switch v := step.(type) {
		case fauxStep:
			f.steps = append(f.steps, v)
		case func(context.Context, Request) (agentcore.AssistantMessage, error):
			f.steps = append(f.steps, fauxStep(v))
		case agentcore.AssistantMessage:
			msg := v
			f.steps = append(f.steps, func(context.Context, Request) (agentcore.AssistantMessage, error) {
				return msg, nil
			})
		default:
			panic(fmt.Sprintf("faux: unsupported step %T", step))
		}
	}
}

func (f *fauxProvider) Complete(ctx context.Context, req Request) (agentcore.AssistantMessage, error) {
	f.mu.Lock()
	i := f.next
	f.next++
	f.calls++
	f.checked = append(f.checked, req)
	var step fauxStep
	if i < len(f.steps) {
		step = f.steps[i]
	}
	f.mu.Unlock()
	if step == nil {
		return agentcore.AssistantMessage{}, fmt.Errorf("faux: unexpected provider call %d", i+1)
	}
	return step(ctx, req)
}

func (f *fauxProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fauxProvider) lastRequest() Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.checked) == 0 {
		return Request{}
	}
	return f.checked[len(f.checked)-1]
}

func textMessage(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(text)},
		StopReason: agentcore.StopReasonEndTurn,
	}
}

func errorMessage(text string) agentcore.AssistantMessage {
	return agentcore.AssistantMessage{
		RoleField:    agentcore.RoleAssistant,
		StopReason:   agentcore.StopReasonError,
		ErrorMessage: text,
	}
}

type callSpec struct {
	name string
	args map[string]any
	id   string
}

func calls(specs ...callSpec) agentcore.AssistantMessage {
	var content agentcore.ContentList
	for _, spec := range specs {
		raw, _ := json.Marshal(spec.args)
		content = append(content, agentcore.NewToolCallContent(spec.id, spec.name, raw))
	}
	return agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    content,
		StopReason: agentcore.StopReasonToolUse,
	}
}

func textOfMessage(m agentcore.AssistantMessage) string {
	return OutputText(m.Content)
}

// deferred is a manually resolved gate that also fires when a context ends.
type deferred struct {
	once sync.Once
	ch   chan struct{}
}

func newDeferred() *deferred { return &deferred{ch: make(chan struct{})} }

func (d *deferred) resolve() { d.once.Do(func() { close(d.ch) }) }

func (d *deferred) wait(ctx context.Context) error {
	select {
	case <-d.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// gated returns a step that signals reached, then waits for release or the
// request's abort signal before answering msg.
func gated(msg agentcore.AssistantMessage) (fauxStep, <-chan struct{}, *deferred) {
	reached := make(chan struct{})
	release := newDeferred()
	var once sync.Once
	step := func(ctx context.Context, req Request) (agentcore.AssistantMessage, error) {
		once.Do(func() { close(reached) })
		select {
		case <-release.ch:
			return msg, nil
		case <-ctx.Done():
			return agentcore.AssistantMessage{}, ctx.Err()
		}
	}
	return step, reached, release
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within 5s")
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func openChat(t *testing.T, store *MemStore, opts Options) (*Harness, *Conversation) {
	t.Helper()
	if store == nil {
		store = NewMemStore()
	}
	opts.Store = store
	if opts.Model == (ModelRef{}) {
		opts.Model = ModelRef{Provider: "faux", ModelID: "faux-1"}
	}
	if len(opts.AvailableModels) == 0 {
		opts.AvailableModels = []ModelRef{opts.Model}
	}
	h := NewHarness(opts)
	t.Cleanup(h.Close)
	return h, h.Root()
}

func entryKind(t *testing.T, h *Harness, conv *Conversation, kind string) []EntryRecord {
	t.Helper()
	var out []EntryRecord
	for _, e := range h.Entries(conv) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type seenResult struct {
	CallID string
	Text   string
	Error  bool
}

func toolResults(h *Harness, conv *Conversation) map[string]agentcore.ToolResultMessage {
	out := map[string]agentcore.ToolResultMessage{}
	for _, e := range h.Entries(conv) {
		if e.Kind != "pi.tool-result" {
			continue
		}
		for _, m := range e.Model {
			if tr, ok := m.(agentcore.ToolResultMessage); ok {
				out[tr.ToolCallID] = tr
			}
		}
	}
	return out
}

func toolCodes(h *Harness, conv *Conversation, callID string) []string {
	for _, e := range h.Entries(conv) {
		if e.Kind != "pi.tool-result" {
			continue
		}
		for _, m := range e.Model {
			tr, ok := m.(agentcore.ToolResultMessage)
			if !ok || tr.ToolCallID != callID {
				continue
			}
			var data struct {
				Diagnostics []Diagnostic `json:"diagnostics"`
			}
			_ = json.Unmarshal(e.Data, &data)
			var codes []string
			for _, d := range data.Diagnostics {
				codes = append(codes, d.Code)
			}
			return codes
		}
	}
	return nil
}

func entryByID(h *Harness, conv *Conversation, id EntryID) (EntryRecord, bool) {
	for _, e := range h.Entries(conv) {
		if e.ID == id {
			return e, true
		}
	}
	return EntryRecord{}, false
}

func rolesOf(msgs []ContextMessage) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Role)
	}
	return out
}

func textMessages(msgs []ContextMessage) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Role+":"+m.Text())
	}
	return out
}

func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func strPtr(s string) *string { return &s }

func resultOK(output string) ToolResult {
	return ToolResult{HasOutput: true, Output: agentcore.ContentList{agentcore.NewTextContent(output)}}
}

func resultErr(output string) ToolResult {
	return ToolResult{HasOutput: true, Output: agentcore.ContentList{agentcore.NewTextContent(output)}, IsError: true}
}
