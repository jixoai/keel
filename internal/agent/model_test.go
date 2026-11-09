package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// recordingStream captures what the provider was actually called with, so the
// model identity of an attempt is observable on the wire rather than inferred
// from the configuration.
type recordingStream struct {
	mu     sync.Mutex
	calls  []provider.StreamConfig
	models []string
}

func (s *recordingStream) fn() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		s.mu.Lock()
		s.calls = append(s.calls, cfg)
		s.models = append(s.models, model)
		s.mu.Unlock()
		msg := agentcore.AssistantMessage{
			RoleField:  agentcore.RoleAssistant,
			Content:    agentcore.ContentList{agentcore.NewTextContent("ok")},
			StopReason: agentcore.StopReasonEndTurn,
		}
		stream := provider.NewAssistantMessageEventStream(4)
		go func() {
			defer stream.Close()
			_ = stream.Emit(ctx, provider.StreamStartEvent{Partial: msg})
			_ = stream.Emit(ctx, provider.StreamDoneEvent{Message: msg})
		}()
		return stream, nil
	}
}

func (s *recordingStream) snapshot() ([]string, []provider.StreamConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	models := append([]string(nil), s.models...)
	calls := append([]provider.StreamConfig(nil), s.calls...)
	return models, calls
}

// stubResolver records the selection it was handed and answers with a fixed
// identity.
type stubResolver struct {
	mu        sync.Mutex
	seen      []json.RawMessage
	resolved  ActiveModel
	err       error
	callCount int
}

func (s *stubResolver) Resolve(_ context.Context, _ kernel.Scope, selection json.RawMessage) (ActiveModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	s.seen = append(s.seen, append(json.RawMessage(nil), selection...))
	if s.err != nil {
		return ActiveModel{}, s.err
	}
	return s.resolved, nil
}

func (s *stubResolver) selections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.seen))
	for _, raw := range s.seen {
		out = append(out, string(raw))
	}
	return out
}

// TestResolvedModelDrivesTheAttempt pins the run-level identity contract: the
// attempt calls the provider with the resolved model id and thinking level, and
// the resolved request overlay reaches the provider untouched.
func TestResolvedModelDrivesTheAttempt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", map[string]any{
		"selection": map[string]any{"providerId": "s2api", "modelId": "deepseek-v4.1-flash", "reasoningLevel": "high"},
	})
	rec := &recordingStream{}
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID:    "s2api",
		Model:         "deepseek-v4.1-flash",
		APIKey:        "key",
		Stream:        rec.fn(),
		ThinkingLevel: agentcore.ThinkingHigh,
		ContextWindow: 128000,
		RequestPatch:  map[string]any{"max_tokens": 4096},
	}}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = resolver

	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.Summary.Status != kernel.RunSummaryDone {
		t.Fatalf("status = %q", res.Summary.Status)
	}
	if res.Summary.Provider != "s2api" || res.Summary.Model != "deepseek-v4.1-flash" {
		t.Fatalf("summary identity = %s/%s", res.Summary.Provider, res.Summary.Model)
	}
	models, calls := rec.snapshot()
	if len(models) != 1 || models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("model on the wire = %v", models)
	}
	if calls[0].ThinkingLevel != agentcore.ThinkingHigh {
		t.Fatalf("thinking level on the wire = %q", calls[0].ThinkingLevel)
	}
	if calls[0].APIKey != "key" {
		t.Fatalf("api key on the wire = %q", calls[0].APIKey)
	}
	if got := calls[0].Extra["max_tokens"]; got != 4096 {
		t.Fatalf("request overlay = %v", calls[0].Extra)
	}
	// The fake stream must not have been used: the resolved identity is the
	// attempt's only transport.
	if selections := resolver.selections(); len(selections) != 1 ||
		!strings.Contains(selections[0], "deepseek-v4.1-flash") {
		t.Fatalf("resolver selections = %v", selections)
	}
}

// TestSelectionIsTheNewestInputs pins that the selection fact comes from the
// newest input carrying one: a session switch mid-conversation takes effect for
// the attempt that answers that input.
func TestSelectionIsTheNewestInputs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "first", "", map[string]any{
		"selection": map[string]any{"modelId": "old-model"},
	})
	commitInput(t, k, scope, "r2", "second", "", map[string]any{
		"selection": map[string]any{"modelId": "new-model"},
	})
	rec := &recordingStream{}
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "new-model", APIKey: "k",
		Stream: rec.fn(), ThinkingLevel: agentcore.ThinkingMedium,
	}}
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = resolver
	lease := claimScope(t, k, tenant, session)
	first, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: first.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	selections := resolver.selections()
	if len(selections) != 1 || !strings.Contains(selections[0], "new-model") {
		t.Fatalf("resolver got %v, want the newest input's selection", selections)
	}

	// The third input carries no selection: the newest input that has one still
	// wins (choosing a model is session state expressed as an input, not a
	// one-shot per message).
	commitInput(t, k, scope, "r3", "third", "", nil)
	requeue(t, st, tenant, session)
	lease = claimScope(t, k, tenant, session)
	second, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: second.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish 2: %v", err)
	}
	selections = resolver.selections()
	if len(selections) != 2 || !strings.Contains(selections[1], "new-model") {
		t.Fatalf("resolver got %v on the second attempt", selections)
	}
}

// TestUnresolvedSelectionFailsBeforeAnyCommit pins the fail-closed ordering: a
// selection that cannot be executed must fail the attempt before a tool call is
// dispatched or an event written, so nothing half-executed is left behind.
func TestUnresolvedSelectionFailsBeforeAnyCommit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", map[string]any{
		"selection": map[string]any{"modelId": "missing-model"},
	})
	before := len(eventsOf(t, st, tenant, session))
	runner := NewRunner(k, k, FakeStream(), "fake", "fake", "", discardLog())
	runner.Models = &stubResolver{err: errors.New(`modelcfg: unknown model "missing-model"`)}

	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	if err == nil {
		t.Fatal("want the attempt to fail")
	}
	if finishErr := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, Err: err, MaxAttempts: 3}); finishErr != nil {
		t.Fatalf("finish: %v", finishErr)
	}
	if !strings.Contains(err.Error(), "resolve model") || !strings.Contains(err.Error(), "missing-model") {
		t.Fatalf("err = %v", err)
	}
	if res.Summary.Status != kernel.RunSummaryFailed {
		t.Fatalf("status = %q", res.Summary.Status)
	}
	if after := len(eventsOf(t, st, tenant, session)); after != before {
		t.Fatalf("the failed attempt committed %d events", after-before)
	}
}

// TestStaticIdentityWithoutResolver pins compatibility: a runtime without a
// ModelResolver keeps the flat configuration and the historical thinking level.
func TestStaticIdentityWithoutResolver(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitInput(t, k, scope, "r1", "hello", "", nil)
	rec := &recordingStream{}
	runner := NewRunner(k, k, rec.fn(), "openai", "gpt-test", "flat-key", discardLog())
	lease := claimScope(t, k, tenant, session)
	res, err := runner.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if res.Summary.Provider != "openai" || res.Summary.Model != "gpt-test" {
		t.Fatalf("summary identity = %s/%s", res.Summary.Provider, res.Summary.Model)
	}
	models, calls := rec.snapshot()
	if models[0] != "gpt-test" || calls[0].APIKey != "flat-key" {
		t.Fatalf("wire identity = %v / %q", models, calls[0].APIKey)
	}
	if calls[0].ThinkingLevel != agentcore.ThinkingLow {
		t.Fatalf("thinking level = %q, want the historical low default", calls[0].ThinkingLevel)
	}
}

// TestRunSelectionReadsOnlyTheSelectionField pins the extraction: a payload
// whose selection is null or absent contributes nothing, and a malformed
// payload does not crash the scan.
func TestRunSelectionReadsOnlyTheSelectionField(t *testing.T) {
	events := []store.Event{
		{Seq: 1, Kind: store.EventUserMessage, Payload: json.RawMessage(`{"text":"a"}`)},
		{Seq: 2, Kind: store.EventAssistantMessage, Payload: json.RawMessage(`{"selection":{"modelId":"nope"}}`)},
		{Seq: 3, Kind: store.EventUserMessage, Payload: json.RawMessage(`{"text":"b","selection":null}`)},
		{Seq: 4, Kind: store.EventUserMessage, Payload: json.RawMessage(`not json`)},
		{Seq: 5, Kind: store.EventUserMessage, Payload: json.RawMessage(`{"text":"c","selection":{"modelId":"picked"}}`)},
	}
	if got := string(runSelection(events)); !strings.Contains(got, "picked") {
		t.Fatalf("selection = %q", got)
	}
	if got := runSelection(events[:4]); got != nil {
		t.Fatalf("selection = %q, want nil", got)
	}
}
