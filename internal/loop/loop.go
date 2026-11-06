// Copied from github.com/smallnest/pigo@891d1f37 (MIT License).
// Upstream: internal/runtime/loop.go
// Modified for keel: package renamed to loop; reminder injection and
// file-based checkpoint writing removed (checkpoints are persisted by the host through a callback).

// This file implements pi's two-layer agent loop (US-006, FR-1). It strings
// together streaming assistant responses, batch tool execution, and the loop's
// hooks with control flow kept faithful to pi's runLoop
// (packages/agent/src/agent-loop.ts, pin 55a5c69):
//
//   - Inner loop: one turn = stream an assistant response → execute its tool
//     calls → feed the results back, repeating until an assistant message has no
//     tool calls (a natural turn end) or every tool result asks to terminate.
//   - Outer loop: after the inner loop settles, pull getFollowUpMessages; if any
//     are returned they become the next pending input and the inner loop runs
//     again, otherwise the run settles.
//
// Queue draining (pi's three drain points): getSteeringMessages is polled once
// before the first request, after every completed turn, and again after
// PrepareNextTurn when nothing was pending. Queued messages are appended right
// before the next provider request. prepareNextTurn runs after turn_end when the
// loop will continue and may swap context / model / thinkingLevel;
// prepareRequest runs immediately before every request (including the first)
// and may replace the request state, but never polls queues itself.
// shouldStopAfterTurn (Keel's finishTurn-action-end equivalent) is consulted
// after turn_end and before the post-turn steering poll.
//
// Two stop reasons are handled specially: length (the response was truncated by
// the token cap) fails every tool call with an error result and emits the tool
// execution events so the model resends (failToolCallsFromTruncatedMessage);
// error / aborted end the run immediately.
//
// Lifecycle: agent_start … agent_end exactly as pi's core AgentEvent
// (packages/agent/src/types.ts). The settle lifecycle (agent_before_settle /
// agent_settled) belongs to pi's coding-agent session/extension layer, not the
// kernel, so it is opt-in via RunConfig.EmitSettleEvents (default off).
//
// Message lifecycle matches pi's core: message_start/message_end are emitted for
// the prompt (RunConfig.PromptCount), injected steering/follow-up messages,
// assistant stream messages, and every tool result. Hosts that persist on
// message_end must filter by role (internal/agent persists only assistant
// messages; tool results are persisted from tool_execution_end).
//
// agentLoop starts a fresh run from a prompt already appended to the context;
// StartContinue resumes from an existing context without a new prompt.
package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/provider"
)

// nowMillis returns the current Unix time in milliseconds, the timestamp unit
// used for CompactionMessage checkpoints.
func nowMillis() int64 { return time.Now().UnixMilli() }

// TurnUpdate is the optional result of PrepareNextTurn: any non-nil field
// replaces the corresponding piece of loop state before the next turn. It lets
// a caller swap the trimmed context, system prompt, tool set, model, or
// thinking level between turns (FR-6).
type TurnUpdate struct {
	Messages      *agentcore.MessageList
	SystemPrompt  *string
	Tools         *[]agentcore.AgentTool
	Model         *string
	ThinkingLevel *agentcore.ThinkingLevel
	// AppendMessages are appended to the context right before the next provider
	// request (after the update is applied, alongside any queued steering
	// messages). Mirrors pi's AgentLoopTurnUpdate.messages: "messages to append
	// before the next provider request".
	AppendMessages []agentcore.AgentMessage
}

// StopDecision is the result of the OnStop seam. Block=true prevents the run
// from ending; Guidance, when non-empty, is appended as a user-role message to
// steer the forced continuation (the Stop / SubagentStop hook's reason). The
// zero value (Block=false) lets the run end.
type StopDecision struct {
	Block    bool
	Guidance string
}

// TurnAction is the decision a FinishTurn hook returns.
type TurnAction string

const (
	// TurnActionContinue ensures one next provider request. A tool-result,
	// steering, or follow-up request that is already scheduled satisfies it and
	// adds no extra request; otherwise the loop runs exactly one context-only
	// turn (pi's finishTurn {action:"continue"}).
	TurnActionContinue TurnAction = "continue"
	// TurnActionEnd ends the run without polling queues or preparing another
	// request (pi's finishTurn {action:"end"}).
	TurnActionEnd TurnAction = "end"
)

// AgentTurnDecision is the optional result of FinishTurn.
type AgentTurnDecision struct {
	Action TurnAction
}

// AgentTurnContext is the completed-turn snapshot handed to FinishTurn. It
// mirrors pi's AgentTurnContext: the assistant message that completed the turn,
// its tool results, the context after both were appended, and the messages this
// run will return if it ends here.
type AgentTurnContext struct {
	Message     agentcore.AssistantMessage
	ToolResults []agentcore.ToolResultMessage
	Context     *agentcore.AgentContext
	NewMessages []agentcore.AgentMessage
}

// PrepareRequestContext is the runtime state handed to PrepareRequest
// immediately before a conversational provider request: the context the request
// will be built from (pending messages already appended), the model id, and the
// thinking level. Mirrors pi's PrepareRequestContext.
type PrepareRequestContext struct {
	Context       *agentcore.AgentContext
	Model         string
	ThinkingLevel agentcore.ThinkingLevel
}

// RequestUpdate is the optional replacement request state returned by
// PrepareRequest. Any non-nil field replaces the runtime value for this and
// later requests (pi's AgentRequestUpdate). Message lists are not replaceable
// here — use Context for a full replacement (pi's prepareRequest returns a
// context, not messages).
type RequestUpdate struct {
	// Context, when non-nil, replaces the system prompt, messages, and tools the
	// next request is built from.
	Context *agentcore.AgentContext
	// Model, when non-nil, replaces the model id for this and later requests.
	Model *string
	// ThinkingLevel, when non-nil, replaces the reasoning effort for this and
	// later requests.
	ThinkingLevel *agentcore.ThinkingLevel
}

// RunConfig is the full configuration for a loop run: the per-turn streaming
// config (embedded LoopConfig), the batch tool-execution config, and the four
// loop-level hooks. Every hook is optional (nil = default behavior).
type RunConfig struct {
	LoopConfig
	// Batch holds the tool registry and the prepare/before/after hooks used to
	// execute each assistant message's tool calls.
	Batch agenttool.BatchConfig

	// GetFollowUpMessages is consulted after the inner loop settles (an assistant
	// message with no tool calls). Returning messages continues the outer loop
	// with them as the next input; returning none ends the run (FR-9).
	GetFollowUpMessages func(ctx context.Context, agentCtx *agentcore.AgentContext) []agentcore.AgentMessage
	// GetSteeringMessages is polled by the loop at pi's three drain points: once
	// before the first request, after every completed turn, and again after
	// PrepareNextTurn when nothing was pending. Returned messages are appended to
	// the context right before the next provider request (pi per-turn semantics,
	// FR-8).
	GetSteeringMessages func(ctx context.Context) []agentcore.AgentMessage
	// PrepareNextTurn runs after a completed turn when the loop is about to start
	// another one (after turn_end, before steering is polled and before the next
	// turn_start) and may swap context / model / thinkingLevel for the next turn
	// (FR-6).
	PrepareNextTurn func(ctx context.Context, agentCtx *agentcore.AgentContext) *TurnUpdate
	// PrepareRequest runs immediately before every conversational provider
	// request, including the first. Pending (steering/follow-up) messages have
	// already been appended to the context when it runs. The returned update
	// replaces the context/model/thinkingLevel for this and later requests. It
	// must not poll the steering queue — the loop owns queue draining (pi's
	// PrepareRequest contract).
	PrepareRequest func(ctx context.Context, req PrepareRequestContext) *RequestUpdate
	// ShouldStopAfterTurn runs after each turn_end; true ends the run with an
	// agent_end event (FR-7). It is consulted before the post-turn steering poll,
	// so a stop skips queue draining (pi's finishTurn action "end").
	ShouldStopAfterTurn func(ctx context.Context, agentCtx *agentcore.AgentContext) bool

	// FinishTurn runs after the turn's assistant message and tool-result
	// messages were appended and before turn_end (pi's finishTurn). Returning
	// TurnActionEnd ends the run without polling queues or preparing another
	// request. Returning TurnActionContinue guarantees one next provider
	// request: an already-scheduled tool-result, steering, or follow-up request
	// satisfies it; otherwise the loop runs exactly one context-only request
	// over the current context. Error and aborted responses remain hard exits
	// (the decision is ignored). nil preserves normal scheduling.
	FinishTurn func(ctx context.Context, turn AgentTurnContext) *AgentTurnDecision

	// OnBeforeSettle, when set, is consulted at the settle boundary right after
	// agent_before_settle is emitted and before agent_settled. Returning true
	// requests exactly one more turn over the current context (the boundary may
	// have appended messages/entries); the loop then starts a fresh low-level run
	// (agent_start) instead of settling. Mirrors pi's actionable
	// agent_before_settle boundary.
	//
	// This hook only runs when EmitSettleEvents is true. It is a host-level
	// extension: pi's core AgentEvent has no settle events (they live in
	// packages/coding-agent's session/extension layer).
	OnBeforeSettle func(ctx context.Context, agentCtx *agentcore.AgentContext) bool

	// EmitSettleEvents enables the host-level settle lifecycle pair
	// (agent_before_settle then agent_settled) after agent_end. It defaults to
	// false because pi's core AgentEvent (packages/agent/src/types.ts) ends at
	// agent_end; the settle events belong to pi's coding-agent session/extension
	// layer (extensions.md "agent_start / agent_end / agent_before_settle /
	// agent_settled"). A host that mirrors that layer opts in here. An aborted
	// run skips agent_before_settle and goes straight to agent_settled.
	EmitSettleEvents bool

	// OnStop, when set, is consulted right before the run would end naturally (no
	// tool calls and no follow-up messages). Returning a decision with Block=true
	// keeps the loop running: any Guidance is appended as a user-role message to
	// steer the continued run (Stop / SubagentStop hooks, US-008/009, FR-10). The
	// seam carries no loop-protection itself — the caller's decorator owns the
	// consecutive-block counter and the FR-12 force-stop limit, so an ill-behaved
	// hook cannot loop forever. nil or a non-blocking decision lets the run end.
	OnStop func(ctx context.Context, agentCtx *agentcore.AgentContext) *StopDecision

	// PromptCount is the number of trailing context messages that are this run's
	// new prompt. They receive message_start/message_end events like pi's
	// agentLoop prompt; StartContinue leaves it 0.
	PromptCount int

	// SkipInitialSteeringPoll suppresses the pre-first-request steering poll for
	// this run. A host that already drained a steering message into the prompt
	// (pi's Agent.continue assistant-tail fallback) uses it so a one-at-a-time
	// queue does not deliver two messages in the same turn.
	SkipInitialSteeringPoll bool

	// EventBuffer is the buffer size of the emitted EventStream. 0 gives fully
	// synchronous back-pressure (matching pi's awaited emit).
	EventBuffer int

	// SessionID, when set, is carried in the run's agent_start event so a
	// stream-json consumer sees the backing session id in the first event and can
	// resume the run later (mirrors pi/Claude Code).
	SessionID string

	// OnCompaction, when set, is called after a successful auto-compaction with
	// the summary that replaced the collapsed prefix. The host persists it as a
	// checkpoint (PG, not a file) so a later run can reload the summary instead
	// of replaying the whole transcript. A persistence failure is the host's to
	// log; the loop continues on the compacted context either way.
	OnCompaction func(ctx context.Context, summary string, firstKeptIndex, summarizedCount int)
}

// LoopEventStream is the stream returned by the loop entry points: it carries
// AgentEvents and yields the messages newly produced during the run.
type LoopEventStream = agentcore.EventStream[agentcore.AgentEvent, []agentcore.AgentMessage]

// agentLoop starts a fresh run. The caller has already appended the initiating
// user message(s) to agentCtx.Messages. It returns immediately with an
// EventStream; a producer goroutine drives the loop and closes the stream when
// the run ends.
func agentLoop(ctx context.Context, agentCtx *agentcore.AgentContext, cfg RunConfig) *LoopEventStream {
	stream := agentcore.NewEventStream[agentcore.AgentEvent, []agentcore.AgentMessage](cfg.EventBuffer)
	go runLoop(ctx, agentCtx, cfg, stream)
	return stream
}

// StartRun is the exported entry point for a fresh run, used by out-of-package
// drivers (the interactive REPL, US-022). It is a thin wrapper over agentLoop so
// the loop internals stay unexported while callers outside the package can
// still launch a run and consume its event stream.
func StartRun(ctx context.Context, agentCtx *agentcore.AgentContext, cfg RunConfig) *LoopEventStream {
	return agentLoop(ctx, agentCtx, cfg)
}

// Continuation guards, worded exactly like pi's agentLoopContinue so a caller
// (or an operator reading a log) sees the same contract.
var (
	// ErrNoMessagesToContinue is returned by StartContinue for an empty context.
	ErrNoMessagesToContinue = errors.New("Cannot continue: no messages in context")
	// ErrContinueFromAssistant is returned by StartContinue when the last context
	// message is an assistant message — the model would have nothing to answer.
	ErrContinueFromAssistant = errors.New("Cannot continue from message role: assistant")
)

// StartContinue is the continuation entry point (pi's agentLoopContinue): it
// resumes from the existing context without a new prompt, so no prompt/user
// message events are emitted and the run result carries only the messages
// produced by this run. The caller is responsible for the context's last message
// converting to a user or toolResult message through ConvertToLlm; the role
// guard below is the same one pi applies.
func StartContinue(ctx context.Context, agentCtx *agentcore.AgentContext, cfg RunConfig) (*LoopEventStream, error) {
	if agentCtx == nil || len(agentCtx.Messages) == 0 {
		return nil, ErrNoMessagesToContinue
	}
	if agentCtx.Messages[len(agentCtx.Messages)-1].Role() == agentcore.RoleAssistant {
		return nil, ErrContinueFromAssistant
	}
	return agentLoop(ctx, agentCtx, cfg), nil
}

// runLoop is the producer: it drives the two-layer loop, emitting events onto
// stream and setting the stream result to the messages produced during the run.
func runLoop(ctx context.Context, agentCtx *agentcore.AgentContext, cfg RunConfig, stream *LoopEventStream) {
	// produced is this invocation's newMessages (pi's newMessages): the messages
	// the loop appended to the transcript. It is tracked explicitly rather than
	// sliced off the context so a hook that replaces the context wholesale
	// (prepareRequest / prepareNextTurn pruning) cannot corrupt the run result.
	var produced []agentcore.AgentMessage
	snapshot := func() []agentcore.AgentMessage {
		if len(produced) == 0 {
			return nil
		}
		out := make([]agentcore.AgentMessage, len(produced))
		copy(out, produced)
		return out
	}
	// tel accumulates structured telemetry (turn count, per-tool durations,
	// truncation count, compaction count, latest context-utilization ratio) from
	// the events emitted below, surfaced as a TelemetryEvent at run end.
	tel := newTelemetry()
	emit := func(ev agentcore.AgentEvent) error {
		tel.observe(ev)
		return stream.Emit(ctx, ev)
	}
	// emitFrom wraps the raw stream.Emit callback handed to streamAssistantResponse
	// and ExecuteToolCalls so telemetry observes those events (message_* and
	// tool_execution_*) too, without changing their signatures.
	emitFrom := func(c context.Context, ev agentcore.AgentEvent) error {
		tel.observe(ev)
		return stream.Emit(c, ev)
	}

	// outcome records how the low-level run ended; it rides agent_before_settle
	// and decides whether that boundary is emitted at all (an aborted run skips
	// it, going straight from agent_end to agent_settled — pi's rule).
	outcome := agentcore.SettleOutcomeCompleted

	// finalizeRun ends the current low-level run: the telemetry summary first (so
	// a consumer sees the run's metrics immediately before the terminal event),
	// then agent_end carrying everything produced so far.
	finalizeRun := func() {
		_ = emit(tel.summary())
		_ = emit(agentcore.AgentEndEvent{Messages: snapshot()})
	}
	// settle records the result and closes the stream exactly once, emitting the
	// host-level settle lifecycle pair first when enabled (not part of pi's core
	// AgentEvent).
	settle := func() {
		if cfg.EmitSettleEvents {
			if outcome != agentcore.SettleOutcomeAborted {
				_ = emit(agentcore.AgentBeforeSettleEvent{Outcome: outcome})
			}
			_ = emit(agentcore.AgentSettledEvent{Aborted: outcome == agentcore.SettleOutcomeAborted})
		}
		msgs := snapshot()
		stream.SetResult(msgs)
		stream.Close()
	}
	finish := func() {
		finalizeRun()
		settle()
	}

	if err := emit(agentcore.AgentStartEvent{SessionID: cfg.SessionID}); err != nil {
		finish()
		return
	}

	// pi emits the message lifecycle for the prompt before the first turn.
	emitMessage := func(m agentcore.AgentMessage) error {
		if err := emit(agentcore.MessageStartEvent{Message: m}); err != nil {
			return err
		}
		return emit(agentcore.MessageEndEvent{Message: m})
	}
	if cfg.PromptCount > 0 {
		start := len(agentCtx.Messages) - cfg.PromptCount
		if start < 0 {
			start = 0
		}
		// The prompt tail is also where a tool-loadout declaration belongs: it
		// must precede the first prompt message (pi's initialMessages).
		tail := append([]agentcore.AgentMessage{}, agentCtx.Messages[start:]...)
		declared, _ := declareToolChanges(agentCtx, tail)
		agentCtx.Messages = append(agentCtx.Messages[:start], declared...)
		for _, m := range declared {
			if err := emitMessage(m); err != nil {
				finish()
				return
			}
		}
		// pi's runAgentLoop returns its initial messages (declaration + prompt) in
		// newMessages; a prompt run therefore includes the prompt in its result.
		produced = append(produced, declared...)
	}

	// pi polls the steering queue once before the first request: the user may
	// have typed while the run was being prepared.
	var pending []agentcore.AgentMessage
	if !cfg.SkipInitialSteeringPoll {
		pending = pollSteering(ctx, &cfg)
	}
	// prepared holds messages a PrepareNextTurn update asked to append; they are
	// injected right before the next request, ahead of queued steering messages
	// (pi's [...preparedMessages, ...pendingMessages]).
	var prepared []agentcore.AgentMessage
	// lastTurnDone mirrors pi's lastCompletedTurn: prepareNextTurn and the extra
	// steering poll only run once a turn has completed. It is reset when a settle
	// continuation starts a fresh low-level run.
	lastTurnDone := false
	// explicitContinuation records a FinishTurn {action:"continue"} decision that
	// no natural (tool-result, steering, follow-up) request satisfied yet; the
	// outer loop then runs exactly one context-only turn (pi's explicitContinuation).
	explicitContinuation := false

	// settleOrContinue emits agent_end and, when EmitSettleEvents is enabled, the
	// host-level settle boundary. An aborted run skips agent_before_settle and
	// goes straight to agent_settled. A boundary hook may request exactly one more
	// turn, in which case a fresh low-level run is started (agent_start again,
	// like pi's host re-invoking runAgentLoop) and done is false.
	settleOrContinue := func() (done bool) {
		finalizeRun()
		if cfg.EmitSettleEvents {
			if outcome == agentcore.SettleOutcomeAborted {
				_ = emit(agentcore.AgentSettledEvent{Aborted: true})
				msgs := snapshot()
				stream.SetResult(msgs)
				stream.Close()
				return true
			}
			_ = emit(agentcore.AgentBeforeSettleEvent{Outcome: outcome})
			if cfg.OnBeforeSettle != nil {
				before := len(agentCtx.Messages)
				if cfg.OnBeforeSettle(ctx, agentCtx) {
					// Anything the boundary appended is part of this run's result.
					if before < len(agentCtx.Messages) {
						produced = append(produced, agentCtx.Messages[before:]...)
					}
					if err := emit(agentcore.AgentStartEvent{SessionID: cfg.SessionID}); err != nil {
						_ = emit(agentcore.AgentSettledEvent{Aborted: false})
						msgs := snapshot()
						stream.SetResult(msgs)
						stream.Close()
						return true
					}
					outcome = agentcore.SettleOutcomeCompleted
					lastTurnDone = false
					pending = pollSteering(ctx, &cfg)
					return false
				}
			}
			_ = emit(agentcore.AgentSettledEvent{Aborted: false})
		}
		msgs := snapshot()
		stream.SetResult(msgs)
		stream.Close()
		return true
	}

	for { // outer loop: pending / follow-up messages and settle continuations
		hasMoreToolCalls := true
		for hasMoreToolCalls || len(pending) > 0 { // inner loop: turns until no tool calls
			if lastTurnDone {
				// pi applies prepareNextTurn first (it can be long-running, e.g.
				// compaction) and then picks up steering queued while it ran. Only
				// poll when nothing was already pending: otherwise "one-at-a-time"
				// mode would deliver two messages in the same turn.
				if cfg.PrepareNextTurn != nil {
					if upd := cfg.PrepareNextTurn(ctx, agentCtx); upd != nil {
						applyTurnUpdate(agentCtx, &cfg, upd)
						prepared = upd.AppendMessages
					}
				}
				if len(pending) == 0 {
					pending = pollSteering(ctx, &cfg)
				}
			}
			if err := emit(agentcore.TurnStartEvent{}); err != nil {
				finish()
				return
			}
			// Inject messages the previous turn's update asked to append, then the
			// queued steering messages, before the next provider request. A tool
			// loadout change is declared ahead of them (pi's declareToolChanges).
			injected := make([]agentcore.AgentMessage, 0, len(prepared)+len(pending))
			injected = append(injected, prepared...)
			injected = append(injected, pending...)
			injected, _ = declareToolChanges(agentCtx, injected)
			for _, m := range injected {
				if err := emitMessage(m); err != nil {
					finish()
					return
				}
				agentCtx.Messages = append(agentCtx.Messages, m)
				produced = append(produced, m)
			}
			prepared = nil
			pending = nil

			// pi's prepareRequest: every conversational request, including the
			// first, after pending messages were appended. It may replace the
			// context/model/thinkingLevel and must not poll queues.
			applyPrepareRequest(ctx, agentCtx, &cfg)

			assistant, err := streamAssistantResponse(ctx, agentCtx, cfg.LoopConfig, emitFrom)
			if err != nil {
				// emit was cancelled mid-stream; end the run.
				if ctx.Err() != nil {
					outcome = agentcore.SettleOutcomeAborted
				}
				finish()
				return
			}
			produced = append(produced, assistant)

			var toolResults []agentcore.ToolResultMessage
			switch assistant.StopReason {
			case agentcore.StopReasonLength:
				// Truncated by the token cap: fail every tool call so the model
				// resends, then keep looping (pi sets hasMoreToolCalls = true for a
				// non-terminating batch).
				toolResults = failToolCallsFromTruncatedMessage(ctx, agentCtx, assistant, emitFrom)
				hasMoreToolCalls = len(toolResults) > 0
				for _, tr := range toolResults {
					if err := emitMessage(tr); err != nil {
						finish()
						return
					}
				}
				produced = appendToolResults(produced, toolResults)
			case agentcore.StopReasonError, agentcore.StopReasonAborted:
				// Terminal failure: emit the turn end and stop. A failure before
				// the stream existed never back-filled the context, so record
				// the terminal message before finishing.
				if assistant.StopReason == agentcore.StopReasonAborted || ctx.Err() != nil {
					outcome = agentcore.SettleOutcomeAborted
				} else {
					outcome = agentcore.SettleOutcomeError
				}
				if len(agentCtx.Messages) == 0 || agentCtx.Messages[len(agentCtx.Messages)-1].Role() != agentcore.RoleAssistant {
					agentCtx.Messages = append(agentCtx.Messages, assistant)
				}
				// pi still calls finishTurn for a terminal assistant message, but the
				// decision cannot change the hard exit.
				if cfg.FinishTurn != nil {
					cfg.FinishTurn(ctx, AgentTurnContext{
						Message:     assistant,
						ToolResults: nil,
						Context:     agentCtx,
						NewMessages: snapshot(),
					})
				}
				_ = emit(agentcore.TurnEndEvent{Message: assistant})
				finish()
				return
			default:
				calls := toAgentToolCalls(assistant.ToolCalls())
				hasMoreToolCalls = false
				if len(calls) > 0 {
					// Inject the run-level emitter into the context so tools
					// (notably the generic task tool) can retrieve it via
					// ProgressEmitterFromContext and surface a dispatched
					// sub-agent's progress up this parent event stream. emitFrom
					// feeds the parent stream and is run-scoped, so a child's
					// SubAgentProgressEvent lands on the right run's stream.
					toolCtx := agentcore.WithProgressEmitter(ctx, emitFrom)
					var allTerminate bool
					toolResults, allTerminate = agenttool.ExecuteToolCalls(toolCtx, cfg.Batch, calls, emitFrom)
					// The batch stops the loop only when every finalized result
					// asks to terminate (pi's shouldTerminateToolBatch).
					hasMoreToolCalls = !allTerminate
					for _, tr := range toolResults {
						if err := emitMessage(tr); err != nil {
							finish()
							return
						}
						agentCtx.Messages = append(agentCtx.Messages, tr)
					}
					produced = appendToolResults(produced, toolResults)
				}
			}

			// pi's finishTurn: after every message of the turn (assistant + tool
			// results) was appended and before turn_end.
			var decision *AgentTurnDecision
			if cfg.FinishTurn != nil {
				decision = cfg.FinishTurn(ctx, AgentTurnContext{
					Message:     assistant,
					ToolResults: toolResults,
					Context:     agentCtx,
					NewMessages: snapshot(),
				})
			}
			if err := emit(agentcore.TurnEndEvent{Message: assistant, ToolResults: toolResults}); err != nil {
				finish()
				return
			}
			lastTurnDone = true
			// {action:"end"} ends the run without polling queues or preparing the
			// next turn.
			if decision != nil && decision.Action == TurnActionEnd {
				finish()
				return
			}
			explicitContinuation = decision != nil && decision.Action == TurnActionContinue

			// Keel extension: auto-compaction at the turn boundary (pi's host does
			// it between runs). Record utilization after any compaction so the
			// telemetry summary reports the current used/window figure.
			maybeAutoCompact(ctx, agentCtx, &cfg, emit, tel)
			if tel != nil && cfg.ContextWindow > 0 {
				tokens := compaction.EstimateContextTokens(agentCtx.Messages).Tokens
				tel.recordContext(tokens, cfg.ContextWindow)
			}
			// Stop hook, consulted before queue draining: a stop skips the post-turn
			// steering poll (pi's finishTurn action "end").
			if cfg.ShouldStopAfterTurn != nil && cfg.ShouldStopAfterTurn(ctx, agentCtx) {
				finish()
				return
			}
			// Queue the next input: steering delivered after the turn, then the
			// inner loop continues when tools still need a follow-up request. A
			// natural request satisfies an explicit continuation; otherwise it is
			// fulfilled once by the outer loop.
			pending = pollSteering(ctx, &cfg)
			if hasMoreToolCalls || len(pending) > 0 {
				explicitContinuation = false
			}
		}

		// Inner loop settled: consult follow-up messages. They become the next
		// pending input (not appended directly) so prepareNextTurn sees the same
		// context pi would show it.
		if cfg.GetFollowUpMessages != nil {
			if follow := cfg.GetFollowUpMessages(ctx, agentCtx); len(follow) > 0 {
				explicitContinuation = false
				pending = follow
				continue // outer loop with the follow-ups as new input
			}
		}
		// No natural request was selected, so fulfill the continuation decision
		// with exactly one context-only turn.
		if explicitContinuation {
			explicitContinuation = false
			continue
		}
		// Stop hook: the run is about to end naturally. A hook may block the end
		// and force a continuation, feeding its guidance back as the next input
		// (US-008/009, FR-10). The consecutive-block counter and force-stop limit
		// (FR-12) live in the decorator behind OnStop, so this seam stays simple.
		if cfg.OnStop != nil {
			if dec := cfg.OnStop(ctx, agentCtx); dec != nil && dec.Block {
				if dec.Guidance != "" {
					guidance := agentcore.UserMessage{
						RoleField: agentcore.RoleUser,
						Content:   agentcore.ContentList{agentcore.NewTextContent(dec.Guidance)},
					}
					agentCtx.Messages = append(agentCtx.Messages, guidance)
					produced = append(produced, guidance)
				}
				continue // outer loop: keep the run alive
			}
		}
		// Settle: agent_end, then agent_before_settle / agent_settled. The boundary
		// may request one more turn over the (possibly extended) context.
		if settleOrContinue() {
			return
		}
	}
}

// pollSteering drains the steering queue once; nil when the hook is unset
// (pi's `config.getSteeringMessages?.() || []`).
func pollSteering(ctx context.Context, cfg *RunConfig) []agentcore.AgentMessage {
	if cfg.GetSteeringMessages == nil {
		return nil
	}
	return cfg.GetSteeringMessages(ctx)
}

// applyPrepareRequest runs the prepareRequest hook and applies its replacement
// state. A non-nil Context replaces the system prompt, messages, and tools the
// request is built from; Model / ThinkingLevel replace the runtime values for
// this and later requests. The hook never polls queues.
func applyPrepareRequest(ctx context.Context, agentCtx *agentcore.AgentContext, cfg *RunConfig) {
	if cfg.PrepareRequest == nil {
		return
	}
	upd := cfg.PrepareRequest(ctx, PrepareRequestContext{
		Context:       agentCtx,
		Model:         cfg.Model,
		ThinkingLevel: cfg.ThinkingLevel,
	})
	if upd == nil {
		return
	}
	if upd.Context != nil {
		agentCtx.SystemPrompt = upd.Context.SystemPrompt
		agentCtx.Messages = upd.Context.Messages
		agentCtx.Tools = upd.Context.Tools
	}
	if upd.Model != nil {
		cfg.Model = *upd.Model
	}
	if upd.ThinkingLevel != nil {
		cfg.ThinkingLevel = *upd.ThinkingLevel
	}
}

// maybeAutoCompact checks whether the context has outgrown its usable window and,
// if so, compacts it in place and emits a CompactionEvent. Compaction is a no-op
// when disabled, when the context window is unknown (<= 0), or when usage is
// under threshold. A compaction failure is non-fatal: the original context is
// preserved and a CompactionEvent carrying ErrorMessage is emitted so the failure
// is observable without aborting the run (US-004).
func maybeAutoCompact(ctx context.Context, agentCtx *agentcore.AgentContext, cfg *RunConfig, emit func(agentcore.AgentEvent) error, tel *telemetry) {
	if !cfg.Compaction.Enabled || cfg.ContextWindow <= 0 {
		return
	}
	before := compaction.EstimateContextTokens(agentCtx.Messages).Tokens
	// Record pre-compaction utilization so the ratio reflects the peak that
	// triggered (or nearly triggered) compaction even when the summary is read
	// mid-run. The turn boundary overwrites it with the post-settle figure.
	if tel != nil {
		tel.recordContext(before, cfg.ContextWindow)
	}
	if !compaction.ShouldCompact(before, cfg.ContextWindow, cfg.Compaction) {
		return
	}
	// Signal the start so a front-end can show an in-progress indicator while the
	// summarization request (an LLM call that blocks the loop) is in flight.
	_ = emit(agentcore.CompactionStartEvent{Reason: "threshold", TokensBefore: before})
	res, err := runCompaction(ctx, agentCtx.Messages, cfg)
	kept := len(agentCtx.Messages)
	if err != nil {
		_ = emit(agentcore.CompactionEvent{
			Reason:       "threshold",
			TokensBefore: before,
			TokensAfter:  before,
			KeptCount:    kept,
			ErrorMessage: err.Error(),
		})
		return
	}
	if res == nil {
		// Nothing to summarize (cut point left no prefix); leave context as-is.
		return
	}
	now := nowMillis()
	rebuilt := res.RebuildContext(agentCtx.Messages, now)
	summarized := len(agentCtx.Messages) - (len(rebuilt) - 1)
	// Hand the summary to the host so it can persist a checkpoint (PG row, not a
	// file). It reuses the summary compaction just produced — no extra LLM call —
	// and is best-effort: a persistence failure is the host's to log and the run
	// continues on the compacted context.
	if cfg.OnCompaction != nil {
		cfg.OnCompaction(ctx, res.Summary, res.FirstKeptIndex, summarized)
	}
	agentCtx.Messages = rebuilt
	after := compaction.EstimateContextTokens(rebuilt).Tokens
	_ = emit(agentcore.CompactionEvent{
		Reason:          "threshold",
		TokensBefore:    before,
		TokensAfter:     after,
		SummarizedCount: summarized,
		KeptCount:       len(rebuilt) - 1,
	})
}

// runCompaction invokes compaction.Compact with the loop's summarization config,
// falling back to the primary Stream/Model when the summary-specific fields are
// unset. Compact derives the cut point from settings.KeepRecentTokens.
func runCompaction(ctx context.Context, msgs agentcore.MessageList, cfg *RunConfig) (*compaction.CompactionResult, error) {
	stream := cfg.SummaryStream
	if stream == nil {
		stream = cfg.Stream
	}
	model := cfg.SummaryModel
	if model.ID == "" {
		model = provider.Model{Provider: cfg.Provider, ID: cfg.Model, ContextWindow: cfg.ContextWindow}
	}
	// Resolve the API key the same way the primary turn does (dynamic key wins,
	// static APIKey is the fallback) so the summarization stream authenticates
	// against auth-requiring providers instead of failing with "missing API key".
	key := cfg.APIKey
	if cfg.GetAPIKey != nil {
		if dyn := cfg.GetAPIKey(ctx, cfg.Provider); dyn != "" {
			key = dyn
		}
	}
	scfg := provider.StreamConfig{APIKey: key, ThinkingLevel: cfg.ThinkingLevel}
	return compaction.Compact(ctx, stream, model, msgs, cfg.Compaction, -1, nil, "", scfg)
}

// applyTurnUpdate applies a non-nil TurnUpdate to the mutable loop state: any
// set field replaces the current context / config value for the next turn.
// AppendMessages is not applied here — the caller injects it right before the
// next provider request so it lands in the transcript at pi's injection point.
func applyTurnUpdate(agentCtx *agentcore.AgentContext, cfg *RunConfig, upd *TurnUpdate) {
	if upd.Messages != nil {
		agentCtx.Messages = *upd.Messages
	}
	if upd.SystemPrompt != nil {
		agentCtx.SystemPrompt = *upd.SystemPrompt
	}
	if upd.Tools != nil {
		agentCtx.Tools = *upd.Tools
	}
	if upd.Model != nil {
		cfg.Model = *upd.Model
	}
	if upd.ThinkingLevel != nil {
		cfg.ThinkingLevel = *upd.ThinkingLevel
	}
}

// failToolCallsFromTruncatedMessage produces an error tool-result message for
// every tool call in a truncated (stopReason=length) assistant message, telling
// the model the response was cut off and to resend. Streamed tool-call arguments
// are completed by a best-effort JSON salvage parser, so a truncated message can
// yield calls whose arguments parse and validate but are silently incomplete;
// none of them is safe to execute. Each call still gets the normal observable
// lifecycle — tool_execution_start then an error tool_execution_end — so a
// consumer sees every finalized call. The results are appended to the context
// and returned. Mirrors pi's failToolCallsFromTruncatedMessage.
func failToolCallsFromTruncatedMessage(ctx context.Context, agentCtx *agentcore.AgentContext, assistant agentcore.AssistantMessage, emit agentcore.EmitFunc) []agentcore.ToolResultMessage {
	calls := assistant.ToolCalls()
	if len(calls) == 0 {
		return nil
	}
	results := make([]agentcore.ToolResultMessage, 0, len(calls))
	for _, c := range calls {
		if emit != nil {
			_ = emit(ctx, agentcore.ToolExecutionStartEvent{ToolCallID: c.ID, ToolName: c.Name, Args: c.Arguments})
		}
		content := agentcore.ContentList{agentcore.NewTextContent(fmt.Sprintf(
			"Tool call %q was not executed: the response hit the output token limit, "+
				"so its arguments may be truncated. Re-issue the tool call with complete arguments.", c.Name))}
		if emit != nil {
			_ = emit(ctx, agentcore.ToolExecutionEndEvent{
				ToolCallID: c.ID,
				ToolName:   c.Name,
				Result:     agentcore.AgentToolResult{Content: content},
				IsError:    true,
			})
		}
		results = append(results, agentcore.ToolResultMessage{
			RoleField:  agentcore.RoleToolResult,
			ToolCallID: c.ID,
			ToolName:   c.Name,
			Content:    content,
			IsError:    true,
		})
	}
	for _, r := range results {
		agentCtx.Messages = append(agentCtx.Messages, r)
	}
	return results
}

// appendToolResults appends tool-result messages to the run's produced-message
// list as AgentMessages.
func appendToolResults(dst []agentcore.AgentMessage, results []agentcore.ToolResultMessage) []agentcore.AgentMessage {
	for _, tr := range results {
		dst = append(dst, tr)
	}
	return dst
}

// toAgentToolCalls converts the assistant message's ToolCallContent blocks into
// the loop-level AgentToolCall view executeToolCalls consumes.
func toAgentToolCalls(blocks []agentcore.ToolCallContent) []agentcore.AgentToolCall {
	if len(blocks) == 0 {
		return nil
	}
	calls := make([]agentcore.AgentToolCall, len(blocks))
	for i, b := range blocks {
		calls[i] = agentcore.AgentToolCall{ID: b.ID, Name: b.Name, Arguments: b.Arguments}
	}
	return calls
}
