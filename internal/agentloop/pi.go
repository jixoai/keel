package agentloop

import (
	"context"
	"fmt"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/loop"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
)

// PiLoop is the default loop service ("pi-loop"): it adapts internal/loop's PI
// loop to the Loop interface.
//
// The adapter adds no behavior of its own beyond the Input mapping:
//
//   - PI event order, tool-call ↔ result pairing and the cancellation
//     semantics are whatever internal/loop produces (the adapter wraps
//     StartRun unchanged);
//   - the turn cap (Limits.MaxTurns) is applied as a ShouldStopAfterTurn
//     wrapper, at the same point RunConfig consults it today;
//   - Context.Tools become both the model-facing declarations and — when
//     Batch.Registry is nil — the executable registry.
//
// A PiLoop holds no run state and no output sink: every run gets a fresh
// transcript copy and a fresh RunStream. It never touches a store, a lease or
// an event ledger.
type PiLoop struct {
	stream provider.StreamFn
}

// PiLoop satisfies the Loop service contract and the servicegraph lifecycle
// (a stateless service: no effects to acquire).
var (
	_ Loop                  = (*PiLoop)(nil)
	_ servicegraph.Instance = (*PiLoop)(nil)
)

// NewPiLoop builds the default loop over the llm provider binding: stream
// produces the provider stream for every run of this instance. A nil stream
// makes every run fail with ErrNoStream.
func NewPiLoop(stream provider.StreamFn) *PiLoop {
	return &PiLoop{stream: stream}
}

// Start implements the servicegraph.Instance lifecycle. PiLoop acquires no
// runtime effects: every run's event stream is created by Run and released
// when the run settles. It exists so the graph can manage the instance
// uniformly (and so a future stateful loop has the same shape).
func (p *PiLoop) Start(context.Context, *servicegraph.EffectScope) error { return nil }

// Stop implements the servicegraph.Instance lifecycle. See Start.
func (p *PiLoop) Stop(context.Context) error { return nil }

// Run starts one PI run. It validates the input, derives the executable tool
// registry, copies the transcript and delegates to loop.StartRun; the returned
// stream is the live run.
func (p *PiLoop) Run(ctx context.Context, in Input) (*RunStream, error) {
	if p == nil || p.stream == nil {
		return nil, ErrNoStream
	}
	if err := validateInput(in); err != nil {
		return nil, err
	}
	registry := in.Batch.Registry
	if registry == nil {
		var err error
		registry, err = registryFromTools(in.Context.Tools)
		if err != nil {
			return nil, err
		}
	}
	tools := in.Context.Tools
	if len(tools) == 0 {
		tools = registry.List()
	} else {
		tools = append([]agentcore.AgentTool{}, tools...)
	}
	agentCtx := &agentcore.AgentContext{
		SystemPrompt: in.Context.SystemPrompt,
		// The transcript and the tool set are copied: a run must not mutate the
		// caller's input.
		Messages: append(agentcore.MessageList{}, in.Context.Messages...),
		Tools:    tools,
	}
	batch := in.Batch
	batch.Registry = registry
	cfg := loop.RunConfig{
		LoopConfig: loop.LoopConfig{
			Model:            in.Model.ID,
			Provider:         in.Model.Provider,
			APIKey:           in.Model.APIKey,
			GetAPIKey:        in.Model.GetAPIKey,
			ThinkingLevel:    in.Model.ThinkingLevel,
			Stream:           p.stream,
			TransformContext: in.Context.TransformContext,
			ConvertToLlm:     in.Context.ConvertToLlm,
			ContextWindow:    in.Compaction.ContextWindow,
			Compaction:       in.Compaction.Settings,
			Extra:            in.Model.Extra,
		},
		Batch:                   batch,
		GetFollowUpMessages:     in.Hooks.GetFollowUpMessages,
		GetSteeringMessages:     in.Hooks.GetSteeringMessages,
		PrepareNextTurn:         in.Hooks.PrepareNextTurn,
		PrepareRequest:          in.Hooks.PrepareRequest,
		ShouldStopAfterTurn:     stopAfterTurn(in.Limits.MaxTurns, in.Hooks.ShouldStopAfterTurn),
		FinishTurn:              in.Hooks.FinishTurn,
		OnBeforeSettle:          in.Hooks.OnBeforeSettle,
		EmitSettleEvents:        in.Hooks.EmitSettleEvents,
		OnStop:                  in.Hooks.OnStop,
		PromptCount:             in.Context.PromptCount,
		SkipInitialSteeringPoll: in.Hooks.SkipInitialSteeringPoll,
		EventBuffer:             in.Limits.EventBuffer,
		SessionID:               in.SessionID,
		OnCompaction:            in.Hooks.OnCompaction,
	}
	runCtx, cancel := context.WithCancel(ctx)
	stream := loop.StartRun(runCtx, agentCtx, cfg)
	return WrapRunStream(stream, cancel), nil
}

// validateInput checks the run shape a loop cannot recover from: the
// transcript must have something for the model to answer, and PromptCount must
// describe a real trailing prompt.
func validateInput(in Input) error {
	if pc := in.Context.PromptCount; pc < 0 || pc > len(in.Context.Messages) {
		return fmt.Errorf("%w: PromptCount %d does not fit a transcript of %d messages",
			ErrInvalidInput, pc, len(in.Context.Messages))
	}
	last := -1
	for i := len(in.Context.Messages) - 1; i >= 0; i-- {
		if in.Context.Messages[i].Role() != agentcore.RoleSystem {
			last = i
			break
		}
	}
	if last < 0 {
		return ErrNothingToAnswer
	}
	switch in.Context.Messages[last].Role() {
	case agentcore.RoleUser, agentcore.RoleToolResult, agentcore.RoleCompaction:
		return nil
	default:
		return fmt.Errorf("%w: the last message role is %q",
			ErrNothingToAnswer, in.Context.Messages[last].Role())
	}
}

// registryFromTools builds the executable registry for a tool set. A tool that
// cannot register (duplicate name, invalid schema) fails the run before it
// starts: execution would otherwise silently degrade.
func registryFromTools(tools []agentcore.AgentTool) (*agenttool.ToolRegistry, error) {
	registry := agenttool.NewToolRegistry()
	for _, tool := range tools {
		if tool == nil {
			return nil, fmt.Errorf("%w: nil tool in Context.Tools", ErrInvalidInput)
		}
		if err := registry.Register(tool); err != nil {
			return nil, fmt.Errorf("%w: register tool %q: %v", ErrInvalidInput, tool.Name(), err)
		}
	}
	return registry, nil
}

// stopAfterTurn builds the run's turn cap. Limits.MaxTurns == 0 selects
// DefaultMaxTurns (mirroring producer defaults); a negative value disables the
// cap and leaves only the caller's hook. The cap short-circuits the hook, so a
// reached cap can never be overridden by a later-turn hook.
func stopAfterTurn(max int, hook func(context.Context, *agentcore.AgentContext) bool) func(context.Context, *agentcore.AgentContext) bool {
	if max == 0 {
		max = DefaultMaxTurns
	}
	if max < 0 && hook == nil {
		return nil
	}
	turns := 0
	return func(ctx context.Context, agentCtx *agentcore.AgentContext) bool {
		turns++
		if max > 0 && turns >= max {
			return true
		}
		if hook != nil {
			return hook(ctx, agentCtx)
		}
		return false
	}
}
