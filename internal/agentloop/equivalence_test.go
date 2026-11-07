package agentloop_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentloop"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/loop"
	"github.com/jixoai/keel/internal/provider"
)

// directSetup mirrors the RunConfig agent.Runner builds today from the same
// Input PiLoop consumes. It is the reference side of the equivalence tests:
// the adapter is expected to reproduce internal/loop's behavior exactly, so
// any mapping drift (a dropped seam, a changed limit) shows up as a diff.
func directSetup(stream provider.StreamFn, in agentloop.Input) (*agentcore.AgentContext, loop.RunConfig) {
	registry := agenttool.NewToolRegistry()
	for _, tool := range in.Context.Tools {
		_ = registry.Register(tool)
	}
	tools := in.Context.Tools
	if len(tools) == 0 {
		tools = registry.List()
	}
	agentCtx := &agentcore.AgentContext{
		SystemPrompt: in.Context.SystemPrompt,
		Messages:     append(agentcore.MessageList{}, in.Context.Messages...),
		Tools:        tools,
	}
	maxTurns := in.Limits.MaxTurns
	if maxTurns == 0 {
		maxTurns = agentloop.DefaultMaxTurns
	}
	turns := 0
	cfg := loop.RunConfig{
		LoopConfig: loop.LoopConfig{
			Model:            in.Model.ID,
			Provider:         in.Model.Provider,
			APIKey:           in.Model.APIKey,
			GetAPIKey:        in.Model.GetAPIKey,
			ThinkingLevel:    in.Model.ThinkingLevel,
			Stream:           stream,
			TransformContext: in.Context.TransformContext,
			ConvertToLlm:     in.Context.ConvertToLlm,
			ContextWindow:    in.Compaction.ContextWindow,
			Compaction:       in.Compaction.Settings,
			Extra:            in.Model.Extra,
		},
		Batch:       agenttool.BatchConfig{ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: registry}},
		EventBuffer: in.Limits.EventBuffer,
		SessionID:   in.SessionID,
		PromptCount: in.Context.PromptCount,
		ShouldStopAfterTurn: func(context.Context, *agentcore.AgentContext) bool {
			turns++
			return maxTurns > 0 && turns >= maxTurns
		},
	}
	return agentCtx, cfg
}

// directRun drives internal/loop directly to completion.
func directRun(t *testing.T, stream provider.StreamFn, in agentloop.Input) ([]agentcore.AgentEvent, []agentcore.AgentMessage) {
	t.Helper()
	agentCtx, cfg := directSetup(stream, in)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run := loop.StartRun(ctx, agentCtx, cfg)
	var events []agentcore.AgentEvent
	for ev := range run.Events() {
		events = append(events, ev)
	}
	result, err := run.Result(ctx)
	if err != nil {
		t.Fatalf("direct loop result: %v", err)
	}
	return events, result
}

// TestPiLoopTranscriptEquivalentToDirectLoop: the adapter is a pure wrapper —
// same events, same transcript, same requests as internal/loop driven with
// agent.Runner's configuration.
func TestPiLoopTranscriptEquivalentToDirectLoop(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		in := baseInput()
		directEvents, directResult := directRun(t, newFakeProvider(assistantText("Hi there!", agentcore.StopReasonEndTurn)).stream, in)
		loopEvents, loopResult := runLoopOnce(t, agentloop.NewPiLoop(newFakeProvider(assistantText("Hi there!", agentcore.StopReasonEndTurn)).stream), in)

		if got, want := eventTypes(loopEvents), eventTypes(directEvents); !reflect.DeepEqual(got, want) {
			t.Fatalf("adapter event sequence = %v, direct = %v", got, want)
		}
		assertShapesEqual(t, "adapter transcript vs direct transcript", shapesOf(loopResult), shapesOf(directResult))
	})

	t.Run("tools", func(t *testing.T) {
		tool := &echoTool{}
		script := func() *fakeProvider {
			return newFakeProvider(
				assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)),
				assistantText("done", agentcore.StopReasonEndTurn),
			)
		}
		in := baseInput()
		in.Context.Tools = []agentcore.AgentTool{tool}
		in.Context.ConvertToLlm = func(msgs agentcore.MessageList) agentcore.MessageList {
			out := make(agentcore.MessageList, 0, len(msgs))
			for _, m := range msgs {
				if m.Role() != agentcore.RoleSystem {
					out = append(out, m)
				}
			}
			return out
		}
		directProv := script()
		directEvents, directResult := directRun(t, directProv.stream, in)
		adapterProv := script()
		loopEvents, loopResult := runLoopOnce(t, agentloop.NewPiLoop(adapterProv.stream), in)

		if got, want := eventTypes(loopEvents), eventTypes(directEvents); !reflect.DeepEqual(got, want) {
			t.Fatalf("adapter event sequence = %v, direct = %v", got, want)
		}
		assertShapesEqual(t, "adapter transcript vs direct transcript", shapesOf(loopResult), shapesOf(directResult))

		adapterReqs := adapterProv.seenRequests()
		directReqs := directProv.seenRequests()
		if len(adapterReqs) != len(directReqs) {
			t.Fatalf("adapter provider calls = %d, direct = %d", len(adapterReqs), len(directReqs))
		}
		for i := range adapterReqs {
			assertShapesEqual(t, "provider request transcript", shapesOf(adapterReqs[i].Messages), shapesOf(directReqs[i].Messages))
			if adapterReqs[i].SystemPrompt != directReqs[i].SystemPrompt {
				t.Fatalf("request %d system prompt: adapter %q, direct %q", i, adapterReqs[i].SystemPrompt, directReqs[i].SystemPrompt)
			}
			if got, want := toolNames(adapterReqs[i].Tools), toolNames(directReqs[i].Tools); !reflect.DeepEqual(got, want) {
				t.Fatalf("request %d tools: adapter %v, direct %v", i, got, want)
			}
		}
		// Both sides declared the executable tool to the model.
		if got := toolNames(adapterReqs[1].Tools); !reflect.DeepEqual(got, []string{"echo"}) {
			t.Fatalf("second request tools = %v, want [echo]", got)
		}
	})
}

// liveRun drains a run's events into a slice while the test drives it (needed
// for the cancellation scenario, where the run is cancelled mid-flight).
type liveRun struct {
	mu     sync.Mutex
	events []agentcore.AgentEvent
	done   chan struct{}
}

func collectRun(events <-chan agentcore.AgentEvent) *liveRun {
	run := &liveRun{done: make(chan struct{})}
	go func() {
		defer close(run.done)
		for ev := range events {
			run.mu.Lock()
			run.events = append(run.events, ev)
			run.mu.Unlock()
		}
	}()
	return run
}

func (r *liveRun) snapshot() []agentcore.AgentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentcore.AgentEvent{}, r.events...)
}

func (r *liveRun) waitSettled(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not settle", what)
	}
}

// TestPiLoopCancelEquivalentToDirectLoop: cancelling the adapter mid-run
// produces the same deterministic prefix and the same result prefix as
// cancelling internal/loop directly. Everything before the blocked provider
// call must be identical; the torn tail is best-effort on both sides (emits
// race the cancelled context).
func TestPiLoopCancelEquivalentToDirectLoop(t *testing.T) {
	script := func() *fakeProvider {
		return newFakeProvider(assistantToolUse(toolCall("call-1", "echo", `{"value":"hello"}`)))
	}
	in := baseInput()
	in.Context.Tools = []agentcore.AgentTool{&echoTool{}}
	in.Limits.EventBuffer = 32

	// Adapter run: cancel while the second provider call is in flight.
	adapterProv := script()
	enteredAdapter := adapterProv.blockFromCall(2)
	ctxAdapter, cancelAdapter := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelAdapter()
	adapterStream, err := agentloop.NewPiLoop(adapterProv.stream).Run(ctxAdapter, in)
	if err != nil {
		t.Fatalf("adapter run: %v", err)
	}
	adapterRun := collectRun(adapterStream.Events())
	waitEntered(t, enteredAdapter, "adapter second provider call")
	adapterStream.Cancel()
	adapterRun.waitSettled(t, "the adapter run after Cancel")
	adapterResult, err := adapterStream.Result(context.Background())
	if err != nil {
		t.Fatalf("adapter result after cancel: %v", err)
	}

	// Direct run: the same scenario through internal/loop.
	directProv := script()
	enteredDirect := directProv.blockFromCall(2)
	agentCtx, cfg := directSetup(directProv.stream, in)
	ctxDirect, cancelDirect := context.WithCancel(context.Background())
	defer cancelDirect()
	directStream := loop.StartRun(ctxDirect, agentCtx, cfg)
	directRun := collectRun(directStream.Events())
	waitEntered(t, enteredDirect, "direct second provider call")
	cancelDirect()
	directRun.waitSettled(t, "the direct run after cancel")
	directResult, err := directStream.Result(context.Background())
	if err != nil {
		t.Fatalf("direct result after cancel: %v", err)
	}

	adapterPrefix := piEventTypes(prefixThroughTurnStart(adapterRun.snapshot(), 2))
	directPrefix := piEventTypes(prefixThroughTurnStart(directRun.snapshot(), 2))
	if !reflect.DeepEqual(adapterPrefix, directPrefix) {
		t.Fatalf("cancelled prefix differs:\n adapter %v\n direct  %v", adapterPrefix, directPrefix)
	}
	if !reflect.DeepEqual(adapterPrefix, toolTurnPrefix) {
		t.Fatalf("cancelled prefix = %v, want %v", adapterPrefix, toolTurnPrefix)
	}
	if len(adapterResult) < len(toolTurnFirstTurn) || len(directResult) < len(toolTurnFirstTurn) {
		t.Fatalf("produced prefixes: adapter %v, direct %v", shapesOf(adapterResult), shapesOf(directResult))
	}
	assertShapesEqual(t, "adapter result prefix", shapesOf(adapterResult[:len(toolTurnFirstTurn)]), toolTurnFirstTurn)
	assertShapesEqual(t, "direct result prefix", shapesOf(directResult[:len(toolTurnFirstTurn)]), toolTurnFirstTurn)

	// Both sides may keep the aborted terminal message of the blocked turn;
	// neither may keep an unpaired tool call.
	for name, result := range map[string][]agentcore.AgentMessage{"adapter": adapterResult, "direct": directResult} {
		assertToolResultsPaired(t, name, result)
		if len(result) <= len(toolTurnFirstTurn) {
			continue
		}
		last := shapesOf(result)[len(result)-1]
		if last.Role != agentcore.RoleAssistant || last.StopReason != agentcore.StopReasonAborted || len(last.Calls) != 0 {
			t.Fatalf("%s cancelled tail = %+v, want an aborted assistant message without tool calls", name, last)
		}
	}
}

func waitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never started", what)
	}
}
