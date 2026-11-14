package agent

// The execution binding tests (change keel-agent-team-v1, RUNTIME-PLAN §c):
// what a run does when the selection fact names an Agent or an AgentTeam.
//
// The invariants these tests pin:
//
//   - no binding (or no resolver): the deployment prompt and the whole
//     registry — the compatibility line;
//   - a binding's system prompt reaches the provider request, and the host's
//     dynamic sections survive the preamble swap;
//   - a binding's grants filter the tool surface, and an unavailable tool id is
//     dropped, not fatal;
//   - a team run always keeps the delegation primitive unless the captain
//     explicitly disabled it (R3);
//   - a binding's default model applies only when the input named none;
//   - a resolver failure fails the attempt before any commit.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// stubBindings answers every run with one fixed surface.
type stubBindings struct {
	surface RunSurface
	err     error
	seen    []json.RawMessage
}

func (s *stubBindings) ResolveBinding(_ context.Context, _ kernel.Scope, selection json.RawMessage) (RunSurface, error) {
	s.seen = append(s.seen, append(json.RawMessage(nil), selection...))
	if s.err != nil {
		return RunSurface{}, s.err
	}
	return s.surface, nil
}

// captureStream records what the provider was called with (the shaped context,
// so the system prompt and tools are observable on the wire).
type captureStream struct {
	mu      sync.Mutex
	prompts []string
	tools   [][]string
}

func (s *captureStream) fn() provider.StreamFn {
	return func(ctx context.Context, model string, llm provider.LlmContext, cfg provider.StreamConfig) (*provider.AssistantMessageEventStream, error) {
		names := make([]string, 0, len(llm.Tools))
		for _, tool := range llm.Tools {
			names = append(names, tool.Name())
		}
		s.mu.Lock()
		s.prompts = append(s.prompts, llm.SystemPrompt)
		s.tools = append(s.tools, names)
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

func (s *captureStream) snapshot() ([]string, [][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prompts := append([]string(nil), s.prompts...)
	tools := make([][]string, 0, len(s.tools))
	for _, names := range s.tools {
		tools = append(tools, append([]string(nil), names...))
	}
	return prompts, tools
}

// renderingAssembly is a staticAssembly that can re-render its preamble while
// keeping a dynamic section, mirroring what host.Host does for a binding.
type renderingAssembly struct {
	prompt string
	tools  *agenttool.ToolRegistry
}

func (s renderingAssembly) SystemPrompt() string           { return s.prompt }
func (s renderingAssembly) Tools() *agenttool.ToolRegistry { return s.tools }
func (s renderingAssembly) RenderSystemPrompt(preamble string) string {
	return preamble + "\n\n<mcp_servers>\nkeep-me\n</mcp_servers>"
}

func registryWith(names ...string) *agenttool.ToolRegistry {
	reg := agenttool.NewToolRegistry()
	for _, name := range names {
		if err := reg.Register(stubTool{name: name}); err != nil {
			panic(err)
		}
	}
	return reg
}

// stubTool is a schema-less tool with a fixed name.
type stubTool struct{ name string }

func (t stubTool) Name() string            { return t.name }
func (t stubTool) Description() string     { return "stub" }
func (t stubTool) Schema() json.RawMessage { return nil }
func (t stubTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t stubTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{Content: agentcore.ContentList{agentcore.NewTextContent("stub")}}, nil
}

// bindingRunner builds a runner wired to the test kernel, with the given
// assembly, resolver and stream.
func bindingRunner(k *kernel.Kernel, assembly Assembly, bindings BindingResolver, models ModelResolver, stream provider.StreamFn) *Runner {
	r := NewRunner(k, k, stream, "fake", "fake", "", discardLog())
	r.Assembly = assembly
	r.Bindings = bindings
	r.Models = models
	return r
}

// commitBindingInput commits one input carrying a selection fact.
func commitBindingInput(t *testing.T, k *kernel.Kernel, tenant, session uuid.UUID, requestID, selection string) {
	t.Helper()
	commitInput(t, k, kernel.Scope{TenantID: tenant, SessionID: session}, requestID, "hello", "",
		map[string]any{"selection": json.RawMessage(selection)})
}

// runAttemptOf claims, executes and settles the session's run, returning the
// attempt result (whose Summary is what the worker commits as run.finished).
func runAttemptOf(t *testing.T, k *kernel.Kernel, r *Runner, tenant, session uuid.UUID) RunResult {
	t.Helper()
	ctx := context.Background()
	lease := claimScope(t, k, tenant, session)
	res, err := r.RunAttempt(ctx, lease)
	if err != nil {
		t.Fatalf("run attempt: %v", err)
	}
	if err := k.Finish(ctx, kernel.FinishRequest{Lease: lease, Cursor: res.Cursor, MaxAttempts: 3}); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	return res
}

func TestBindingWithoutResolverKeepsDeploymentPromptAndTools(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-none", `{"modelId":"m","agentId":"agent_ignored"}`)
	rec := &captureStream{}
	// The runner is built WITHOUT a binding resolver: the fact's binding keys
	// are inert and the deployment surface applies untouched.
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "echo")}, nil, nil, rec.fn())
	res := runAttemptOf(t, k, r, tenant, session)
	prompts, tools := rec.snapshot()
	if len(prompts) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "deployment prompt") {
		t.Fatalf("system prompt = %q, want the deployment prompt", prompts[0])
	}
	if got := strings.Join(tools[0], ","); got != "clock,echo" {
		t.Fatalf("tool surface = %v, want the whole registry", tools[0])
	}
	summary := res.Summary
	if summary.AgentID != "" || summary.TeamID != "" {
		t.Fatalf("summary carries a binding without a resolver: %+v", summary)
	}
	// The prompt hash is an audit fact about the request that was sent, not a
	// property of the binding: an unbound run still names the prompt that
	// answered it.
	if summary.PromptHash == "" {
		t.Fatalf("summary has no prompt hash for the wire prompt: %+v", summary)
	}
}

func TestBindingPromptReachesProviderAndKeepsSections(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-prompt", `{"modelId":"m","agentId":"agent_kite7"}`)
	rec := &captureStream{}
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock")},
		&stubBindings{surface: RunSurface{
			Prompt:     "KITE-RUNTIME-MARK-7 agent instructions",
			PromptHash: "sha256:test",
			AgentID:    "agent_kite7",
		}}, nil, rec.fn())
	res := runAttemptOf(t, k, r, tenant, session)
	prompts, _ := rec.snapshot()
	if len(prompts) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "KITE-RUNTIME-MARK-7") {
		t.Fatalf("the binding prompt did not reach the provider: %q", prompts[0])
	}
	if !strings.Contains(prompts[0], "keep-me") {
		t.Fatalf("the dynamic section was dropped by the preamble swap: %q", prompts[0])
	}
	summary := res.Summary
	if summary.AgentID != "agent_kite7" {
		t.Fatalf("summary = %+v, want the frozen binding identity", summary)
	}
	// The hash identifies the prompt that actually went on the wire: the binding
	// text wrapped in the host's dynamic sections. A host-section change must
	// move it even when the agent's own text is untouched.
	if summary.PromptHash == "" || summary.PromptHash == "sha256:test" {
		t.Fatalf("promptHash did not digest the wire prompt: %q", summary.PromptHash)
	}
	if want := digestPrompt(prompts[0]); summary.PromptHash != want {
		t.Fatalf("promptHash = %q, want the digest of the sent prompt %q", summary.PromptHash, want)
	}
	if summary.TeamID != "" {
		t.Fatalf("standard mode froze a team id: %+v", summary)
	}
}

func TestBindingToolGrantsFilterTheSurface(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-grants", `{"modelId":"m","agentId":"agent_grant"}`)
	rec := &captureStream{}
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "echo", "spawn_subagent")},
		&stubBindings{surface: RunSurface{
			ToolIDs: map[string]bool{"clock": true, "echo": false, "gone": true},
			AgentID: "agent_grant",
		}}, nil, rec.fn())
	runAttemptOf(t, k, r, tenant, session)
	_, tools := rec.snapshot()
	if got := strings.Join(tools[0], ","); got != "clock" {
		t.Fatalf("tool surface = %v, want [clock] (echo disabled, gone unavailable)", tools[0])
	}
}

func TestTeamModeKeepsSpawnToolUnlessExplicitlyDisabled(t *testing.T) {
	cases := []struct {
		name   string
		grants map[string]bool
		want   string
	}{
		{name: "grants without the primitive", grants: map[string]bool{"clock": true}, want: "clock,spawn_subagent"},
		{name: "grants with it enabled", grants: map[string]bool{"clock": true, "spawn_subagent": true}, want: "clock,spawn_subagent"},
		{name: "grants with it disabled", grants: map[string]bool{"clock": true, "spawn_subagent": false}, want: "clock"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			st := testStore(t)
			tenant, session := newSession(t, st)
			k := testKernel(t, st)
			commitBindingInput(t, k, tenant, session, "binding-team", `{"modelId":"m","teamId":"team_kite"}`)
			rec := &captureStream{}
			r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "echo", "spawn_subagent")},
				&stubBindings{surface: RunSurface{
					ToolIDs:  test.grants,
					TeamMode: true,
					AgentID:  "agent_captain",
					TeamID:   "team_kite",
				}}, nil, rec.fn())
			runAttemptOf(t, k, r, tenant, session)
			_, tools := rec.snapshot()
			if got := strings.Join(tools[0], ","); got != test.want {
				t.Fatalf("tool surface = %v, want %v", tools[0], test.want)
			}
		})
	}
}

func TestBindingDefaultModelAppliesOnlyWithoutAChoice(t *testing.T) {
	cases := []struct {
		name      string
		selection string
		wantModel string
	}{
		{name: "fact names a model", selection: `{"providerId":"p-user","modelId":"m-user","agentId":"a"}`, wantModel: "m-user"},
		{name: "fact names only the binding", selection: `{"agentId":"a"}`, wantModel: "m-agent"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			st := testStore(t)
			tenant, session := newSession(t, st)
			k := testKernel(t, st)
			commitBindingInput(t, k, tenant, session, "binding-model", test.selection)
			wire := &captureStream{}
			resolver := &stubResolver{resolved: ActiveModel{ProviderID: "p", Model: "resolved", Stream: wire.fn()}}
			r := bindingRunner(k, nil,
				&stubBindings{surface: RunSurface{
					AgentID:      "agent_default",
					DefaultModel: &SurfaceModelBinding{ProviderID: "p-agent", ModelID: "m-agent"},
				}}, resolver, FakeStream())
			runAttemptOf(t, k, r, tenant, session)
			if len(resolver.seen) != 1 {
				t.Fatalf("resolver calls = %d, want 1", len(resolver.seen))
			}
			var merged struct {
				ModelID string `json:"modelId"`
			}
			if err := json.Unmarshal(resolver.seen[0], &merged); err != nil {
				t.Fatalf("resolver selection %s: %v", resolver.seen[0], err)
			}
			if merged.ModelID != test.wantModel {
				t.Fatalf("resolved model fact = %s, want modelId %q", resolver.seen[0], test.wantModel)
			}
		})
	}
}

func TestBindingResolverFailureFailsBeforeAnyCommit(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-fail", `{"agentId":"agent_gone"}`)
	before := len(eventsOf(t, st, tenant, session))
	r := bindingRunner(k, nil, &stubBindings{err: errors.New(`agentId "agent_gone" does not exist`)}, nil, FakeStream())
	lease := claimScope(t, k, tenant, session)
	_, err := r.Run(context.Background(), lease)
	if err == nil || !strings.Contains(err.Error(), "resolve agent binding") {
		t.Fatalf("run error = %v, want a binding resolution failure", err)
	}
	if after := len(eventsOf(t, st, tenant, session)); after != before {
		t.Fatalf("the failed attempt committed %d events", after-before)
	}
}

// TestMergeSelectionDefaultModel pins the merge rules of the fallback chain.
func TestMergeSelectionDefaultModel(t *testing.T) {
	binding := &SurfaceModelBinding{ProviderID: "p", ModelID: "m"}
	cases := []struct {
		name      string
		selection string
		want      string
	}{
		{name: "empty fact gains the default", selection: "{}", want: `{"modelId":"m","providerId":"p"}`},
		{name: "binding-only fact gains the default", selection: `{"agentId":"a"}`, want: `{"agentId":"a","modelId":"m","providerId":"p"}`},
		{name: "chosen model wins", selection: `{"modelId":"chosen"}`, want: `{"modelId":"chosen"}`},
		{name: "half binding is left to the resolver", selection: `{"providerId":"p2"}`, want: `{"providerId":"p2"}`},
		{name: "reasoning level survives the merge", selection: `{"reasoningLevel":"high"}`, want: `{"modelId":"m","providerId":"p","reasoningLevel":"high"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := MergeSelectionDefaultModel(json.RawMessage(test.selection), binding)
			if !jsonEqual(string(got), test.want) {
				t.Fatalf("merged = %s, want %s", got, test.want)
			}
		})
	}
	// A nil fact with a binding gains the default: the binding itself came from a
	// choice, and an agent's default model must apply to an input that named no
	// model of its own.
	if got := MergeSelectionDefaultModel(nil, binding); !jsonEqual(string(got), `{"modelId":"m","providerId":"p"}`) {
		t.Fatalf("nil fact = %s, want the default model fact", got)
	}
	// Without a binding the nil fact stays nil ("no choice made").
	if got := MergeSelectionDefaultModel(nil, nil); got != nil {
		t.Fatalf("nil fact without a binding = %s, want nil", got)
	}
	// A nil or incomplete binding changes nothing.
	if got := MergeSelectionDefaultModel(json.RawMessage(`{"agentId":"a"}`), nil); string(got) != `{"agentId":"a"}` {
		t.Fatalf("nil binding changed the fact: %s", got)
	}
	if got := MergeSelectionDefaultModel(json.RawMessage(`{"agentId":"a"}`), &SurfaceModelBinding{ProviderID: "p"}); string(got) != `{"agentId":"a"}` {
		t.Fatalf("half binding changed the fact: %s", got)
	}
}

// jsonEqual compares two JSON documents by value.
func jsonEqual(a, b string) bool {
	var left, right any
	if json.Unmarshal([]byte(a), &left) != nil || json.Unmarshal([]byte(b), &right) != nil {
		return false
	}
	la, _ := json.Marshal(left)
	lb, _ := json.Marshal(right)
	return string(la) == string(lb)
}

// TestBindingEmptyToolSurfaceUnblocksNoToolModels pins the interaction between
// the tool filter and the no-tool guard: the guard refuses an attempt that would
// hand tools to a model declaring SupportsToolCall=false. A binding whose grants
// leave an empty surface therefore makes such a model runnable — the guard's
// condition (a non-empty list) is what it always was, the list is just smaller.
func TestBindingEmptyToolSurfaceUnblocksNoToolModels(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-notools", `{"modelId":"m","agentId":"agent_notools"}`)
	wire := &captureStream{}
	resolver := &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "no-tools", Stream: wire.fn(),
		Properties: &ModelProperties{SupportsToolCall: false},
	}}
	// The agent declares grants and none is enabled: the filtered surface is
	// empty, so the model's declared lack of tool support is satisfied.
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "echo")},
		&stubBindings{surface: RunSurface{ToolIDs: map[string]bool{"clock": false}, AgentID: "agent_notools"}},
		resolver, FakeStream())
	runAttemptOf(t, k, r, tenant, session)
	_, tools := wire.snapshot()
	if len(tools) != 1 || len(tools[0]) != 0 {
		t.Fatalf("tool surface = %v, want an empty list", tools)
	}

	// Without the binding the same model still refuses the whole registry (the
	// pre-binding rule, unchanged).
	st2 := testStore(t)
	tenant2, session2 := newSession(t, st2)
	k2 := testKernel(t, st2)
	commitBindingInput(t, k2, tenant2, session2, "binding-notools-2", `{"modelId":"m"}`)
	resolver2 := &stubResolver{resolved: ActiveModel{
		ProviderID: "p", Model: "no-tools", Stream: wire.fn(),
		Properties: &ModelProperties{SupportsToolCall: false},
	}}
	r2 := bindingRunner(k2, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock")}, nil, resolver2, FakeStream())
	lease := claimScope(t, k2, tenant2, session2)
	if _, err := r2.Run(context.Background(), lease); err == nil || !strings.Contains(err.Error(), "declares no tool-call support") {
		t.Fatalf("run error = %v, want the no-tool refusal", err)
	}
}

// TestRecoveryDoesNotExecuteToolsOutsideTheGrant pins the crash-recovery
// boundary: an assistant tool call left unresolved by a crashed process is
// replayed through the attempt's filtered surface, not the host registry. A
// binding that grants only clock must not gain echo just because the process
// died mid-turn — otherwise removing a grant, or a captain explicitly disabling
// delegation, would only hold until the next restart.
func TestRecoveryDoesNotExecuteToolsOutsideTheGrant(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	scope := kernel.Scope{TenantID: tenant, SessionID: session}

	commitBindingInput(t, k, tenant, session, "binding-recovery", `{"modelId":"m","agentId":"agent_grant"}`)
	// A previous attempt committed an assistant tool call for "echo" and died
	// before committing its result.
	lease := claimScope(t, k, tenant, session)
	orphan := mustJSON(map[string]any{
		"role":       "assistant",
		"content":    []map[string]any{{"type": "toolCall", "id": "call_orphan", "name": "echo", "arguments": map[string]any{"text": "hi"}}},
		"stopReason": "tool_use",
	})
	if _, err := k.Commit(ctx, kernel.CommitRequest{Scope: scope, Kind: kernel.KindAssistantMessage,
		Token: lease.Token, GenerationID: "orphan-a0", Payload: orphan}); err != nil {
		t.Fatalf("commit orphan tool call: %v", err)
	}
	if err := k.Release(ctx, lease); err != nil {
		t.Fatalf("release: %v", err)
	}

	rec := &captureStream{}
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "echo")},
		&stubBindings{surface: RunSurface{ToolIDs: map[string]bool{"clock": true}, AgentID: "agent_grant"}},
		nil, rec.fn())
	runAttemptOf(t, k, r, tenant, session)

	// The recovery replay must refuse the ungranted tool, and the refusal must be
	// recorded as an error result rather than the tool's own output.
	events, err := st.LoadEvents(ctx, tenant, session, 0, 1000)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var found bool
	for _, ev := range events {
		if ev.Kind != store.EventToolResult {
			continue
		}
		var payload struct {
			ToolCallID string          `json:"toolCallId"`
			IsError    bool            `json:"isError"`
			Content    json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if payload.ToolCallID != "call_orphan" {
			continue
		}
		found = true
		if !payload.IsError {
			t.Fatalf("recovery executed the ungranted tool echo: %s", ev.Payload)
		}
	}
	if !found {
		t.Fatal("recovery committed no result for the orphan tool call")
	}
}

// TestCaptainSeesTheTeamRoster pins the delegation affordance: the captain's
// prompt lists the team's members, so spawn_subagent can name one instead of
// guessing an agentId (spawn_subagent's schema takes a free-text id).
func TestCaptainSeesTheTeamRoster(t *testing.T) {
	st := testStore(t)
	tenant, session := newSession(t, st)
	k := testKernel(t, st)
	commitBindingInput(t, k, tenant, session, "binding-roster", `{"modelId":"m","teamId":"team_kite"}`)
	rec := &captureStream{}
	r := bindingRunner(k, renderingAssembly{prompt: "deployment prompt", tools: registryWith("clock", "spawn_subagent")},
		&stubBindings{surface: RunSurface{
			Prompt:   "KITE-RUNTIME-MARK-7 captain instructions",
			TeamMode: true,
			AgentID:  "agent_captain",
			TeamID:   "team_kite",
			Members: []RosterMember{
				{AgentID: "agent_reviewer", Label: "Reviewer", Description: "reviews the draft"},
				{AgentID: "agent_writer", Label: "Writer"},
			},
		}}, nil, rec.fn())
	runAttemptOf(t, k, r, tenant, session)
	prompts, _ := rec.snapshot()
	if len(prompts) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(prompts))
	}
	text := prompts[0]
	for _, want := range []string{"<team_members>", "agent_reviewer", "Reviewer", "reviews the draft", "agent_writer", "</team_members>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("captain prompt is missing %q:\n%s", want, text)
		}
	}
	// The roster is part of the prompt that goes on the wire, so the hash covers
	// it: two teams differing only in members must not collide.
	if !strings.Contains(text, "KITE-RUNTIME-MARK-7") {
		t.Fatalf("the roster replaced the captain prompt instead of extending it:\n%s", text)
	}
}
