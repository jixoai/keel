// Package agentloop defines Keel's agent-loop service contract: the
// replaceable strategy that drives one run of a session.
//
// A loop turns a run's input state — the reconstructed transcript, the system
// prompt, the tool set, the model/provider/credential selection and the run's
// limits — into an ordered stream of PI agent events and the messages the run
// produced. The default implementation is "pi-loop" (PiLoop, see pi.go), which
// adapts internal/loop; any other loop may take the slot, but only after it
// passes the PI conformance suite (see README.md, "Replacement gate").
//
// # A loop does not own the ledger
//
// The interface is deliberately narrower than a runner:
//
//   - a Loop receives no store, no kernel, no lease and no session handle; it
//     cannot append an event, allocate a seq, claim a run or write a
//     checkpoint. Everything it produces is a proposal: the event stream it
//     emits, and the messages carried by the stream result. Committing them
//     (kernel.Commit, idempotency keys, pairing validation, the run cursor)
//     belongs to the run coordinator on the kernel side.
//   - Hooks observe and steer the run; they never move the ledger either. In
//     particular OnCompaction reports the summary the loop produced —
//     persisting it as a checkpoint is the coordinator's decision.
//
// The boundary is enforced structurally, not by convention: Loop, Input,
// RunStream, Hooks and the loop implementations reference no ledger package,
// and package-level tests assert it (see boundary_test.go).
//
// # Events
//
// The stream carries agentcore.AgentEvent values in PI's order — agent_start,
// then per turn turn_start / message_* / tool_execution_* / turn_end, then
// agent_end — plus Keel's additive observability events (telemetry,
// compaction, subagent_progress) which consumers dispatch by type. The stream
// result is the list of messages this run produced (the prompt it echoed, the
// assistant messages and the tool results); it never contains the pre-existing
// transcript.
package agentloop

import (
	"context"
	"errors"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/loop"
)

// DefaultMaxTurns is the turn cap the default loop (PiLoop) applies when
// Limits.MaxTurns is 0. It mirrors agent.NewRunner's production cap so the
// default loadout keeps today's bounded-run behavior.
const DefaultMaxTurns = 8

// Service errors. They are returned by Run before any event is emitted.
var (
	// ErrNoStream is returned when the loop has no provider stream (a missing
	// or mis-shaped ctx.llm binding).
	ErrNoStream = errors.New("agentloop: the loop has no provider stream")
	// ErrNothingToAnswer is returned when the transcript has no message the
	// model can answer: empty, system-only, or ending in an assistant message
	// (the caller must append the next prompt first).
	ErrNothingToAnswer = errors.New("agentloop: the transcript has nothing for the model to answer")
	// ErrInvalidInput marks a malformed Input (for example a PromptCount that
	// does not fit the transcript).
	ErrInvalidInput = errors.New("agentloop: invalid run input")
)

// Loop is the agent-loop service. Run starts one run over a complete input
// state and returns its event stream; the loop drives that run to its terminal
// event without holding any durable state of its own.
//
// Run is safe to call concurrently for different sessions: a loop instance is
// stateless between runs (any per-run state lives in the returned RunStream).
// Run returns before the run started; errors are configuration problems
// (ErrNoStream, ErrNothingToAnswer, ErrInvalidInput), never model failures —
// those arrive as a terminal assistant message with stopReason error/aborted.
type Loop interface {
	Run(ctx context.Context, in Input) (*RunStream, error)
}

// LoopFunc adapts a function to the Loop interface. It is the convenience
// fake for tests and wiring code that already has a function.
type LoopFunc func(ctx context.Context, in Input) (*RunStream, error)

// Run implements Loop.
func (f LoopFunc) Run(ctx context.Context, in Input) (*RunStream, error) { return f(ctx, in) }

// Input is the complete state of one run. The run coordinator builds it from
// the committed event log (messages reconstructed from events by the
// coordinator, not by the loop); the loop neither reads nor writes that log.
type Input struct {
	// SessionID identifies the session backing this run. It is carried in the
	// first event (agent_start) so a consumer can associate the stream with a
	// session and resume it later.
	SessionID string
	// Context is the prompt state handed to the model.
	Context Context
	// Model selects the model, provider and credential for this run.
	Model ModelConfig
	// Limits bounds the run.
	Limits Limits
	// Compaction configures automatic context compaction for this run.
	Compaction CompactionConfig
	// Batch is the optional tool-execution wiring (executor hooks, budgets).
	// Its Registry, when nil, is derived by registering Context.Tools; the
	// loop always executes against a registry, so an unknown tool call becomes
	// an error tool result instead of a crash.
	Batch agenttool.BatchConfig
	// Hooks are the optional per-run seams the coordinator wires. Every field
	// is optional (nil = the loop's default behavior).
	Hooks Hooks
}

// Context is the prompt state of one run. The surface deliberately mirrors
// PI's AgentContext: messages and tools travel to the provider together, the
// system prompt is assembled by the host, and the two conversion seams run at
// the provider boundary.
type Context struct {
	// SystemPrompt is the host-assembled system prompt for this run.
	SystemPrompt string
	// Messages is the transcript reconstructed from committed events. The
	// trailing PromptCount entries are this run's new prompt; everything
	// before them is history the model answers from.
	Messages agentcore.MessageList
	// PromptCount is how many trailing Messages entries are this run's new
	// prompt. They receive the message lifecycle (message_start/message_end),
	// exactly like PI's agentLoop prompt; 0 marks a continuation run with no
	// new prompt (for example resuming after committed tool results).
	PromptCount int
	// Tools is the model-facing tool set for this run: what the transcript
	// declares and what the model may call.
	Tools []agentcore.AgentTool
	// TransformContext optionally rewrites the message list before conversion
	// (context trimming/injection). Contract: must not error; on failure it
	// returns a safe fallback. Runs first.
	TransformContext func(ctx context.Context, msgs agentcore.MessageList) agentcore.MessageList
	// ConvertToLlm optionally filters UI-only messages before the provider
	// request (for example Keel's transcript tool-declaration system
	// messages). Contract: must not error. Defaults to identity.
	ConvertToLlm func(msgs agentcore.MessageList) agentcore.MessageList
}

// ModelConfig selects the model and credential of one run.
type ModelConfig struct {
	// ID is the model id handed to the provider stream.
	ID string
	// Provider is the provider name used for credential lookups and error
	// reporting.
	Provider string
	// APIKey is the static credential; a non-empty GetAPIKey result wins.
	APIKey string
	// GetAPIKey optionally resolves a fresh key per request (short-lived
	// tokens). Falls back to APIKey when nil or empty.
	GetAPIKey func(ctx context.Context, provider string) string
	// ThinkingLevel is the requested reasoning effort.
	ThinkingLevel agentcore.ThinkingLevel
	// Extra is forwarded to the provider request (provider-specific options).
	Extra map[string]any
}

// Limits bounds one run. The zero value is the production default: the loop's
// DefaultMaxTurns cap and synchronous event back-pressure.
type Limits struct {
	// MaxTurns caps the number of completed turns (one provider response plus
	// its tool batch). 0 selects the loop's DefaultMaxTurns; a negative value
	// disables the cap and leaves only Hooks.ShouldStopAfterTurn.
	MaxTurns int
	// EventBuffer is the event channel buffer size. 0 gives fully synchronous
	// back-pressure — each event is delivered before the loop continues,
	// matching PI's awaited emit; the default loadout uses 32.
	EventBuffer int
}

// CompactionConfig configures automatic context compaction. Compaction is
// disabled while ContextWindow <= 0 or Settings.Enabled is false.
type CompactionConfig struct {
	// ContextWindow is the model's total token budget. The loop compacts when
	// the estimated usage crosses Settings' threshold.
	ContextWindow int
	// Settings holds the thresholds/retention knobs.
	Settings compaction.CompactionSettings
}

// Hooks are the optional per-run seams a run coordinator wires into a loop.
// They are wiring, not state: a hook may inspect or extend the run, but it
// must not append events, allocate a seq or hold a lease — that is the
// coordinator's path, never the loop's.
//
// The seam types are the PI-contract types owned by internal/loop
// (TurnUpdate, AgentTurnContext, ...); this interface reuses them instead of
// duplicating the PI surface (see README.md, "Interface decisions").
type Hooks struct {
	// GetSteeringMessages is polled by the loop at PI's three drain points.
	// Returned messages are injected before the next provider request.
	GetSteeringMessages func(ctx context.Context) []agentcore.AgentMessage
	// GetFollowUpMessages is consulted after the inner loop settles; returned
	// messages continue the outer loop as the next input.
	GetFollowUpMessages func(ctx context.Context, agentCtx *agentcore.AgentContext) []agentcore.AgentMessage
	// PrepareNextTurn runs after a completed turn and may swap context, model
	// or thinking level for the next turn.
	PrepareNextTurn func(ctx context.Context, agentCtx *agentcore.AgentContext) *loop.TurnUpdate
	// PrepareRequest runs immediately before every provider request and may
	// replace the request state. It must not poll the steering queue.
	PrepareRequest func(ctx context.Context, req loop.PrepareRequestContext) *loop.RequestUpdate
	// ShouldStopAfterTurn runs after each turn_end; true ends the run before
	// the post-turn queue drain.
	ShouldStopAfterTurn func(ctx context.Context, agentCtx *agentcore.AgentContext) bool
	// FinishTurn runs after the turn's messages were appended and before
	// turn_end; it may end or force one more request.
	FinishTurn func(ctx context.Context, turn loop.AgentTurnContext) *loop.AgentTurnDecision
	// OnStop is consulted right before the run would end naturally; a
	// blocking decision keeps it running with Guidance as the next input.
	OnStop func(ctx context.Context, agentCtx *agentcore.AgentContext) *loop.StopDecision
	// OnBeforeSettle, when set (and EmitSettleEvents is true), is the
	// actionable settle boundary; true requests exactly one more turn.
	OnBeforeSettle func(ctx context.Context, agentCtx *agentcore.AgentContext) bool
	// EmitSettleEvents enables the host-level settle lifecycle pair
	// (agent_before_settle / agent_settled) after agent_end.
	EmitSettleEvents bool
	// SkipInitialSteeringPoll suppresses the pre-first-request steering poll
	// for a run whose prompt already drained a steering message.
	SkipInitialSteeringPoll bool
	// OnCompaction reports a successful auto-compaction: the summary that
	// replaced the collapsed prefix, the first kept index and the summarized
	// count. Persisting it (as a checkpoint) is the coordinator's call; a
	// failure there is the coordinator's to log and does not abort the run.
	OnCompaction func(ctx context.Context, summary string, firstKeptIndex, summarizedCount int)
}

// RunStream is the output of one Loop.Run: the ordered event stream, the
// messages the run produced (the stream result) and explicit cancellation.
//
// It is the producer/consumer pair of agentcore.EventStream, shaped for the
// service boundary:
//
//   - a loop implementation drives it (Emit, Finish);
//   - the coordinator consumes it (Events, Result) and may Cancel it.
//
// Events, Result and Cancel are safe to use concurrently. Cancel signals the
// run to stop; the producer still settles — it sets the result and closes the
// stream. Emits after cancellation are best-effort (they may be dropped, as
// the underlying stream selects on the run context), so a consumer must treat
// events after a cancel as optional and read the result for the outcome. A
// consumer that stops reading events before the run settles MUST Cancel (or
// cancel the context passed to Loop.Run): the producer blocks on its next emit
// until one of the two happens.
type RunStream struct {
	stream *agentcore.EventStream[agentcore.AgentEvent, []agentcore.AgentMessage]
	cancel context.CancelFunc
}

// NewRunStream creates an empty run stream for a loop implementation to drive.
// buffer is the event channel buffer (0 = synchronous back-pressure); cancel,
// when non-nil, aborts the run when the consumer calls RunStream.Cancel.
func NewRunStream(buffer int, cancel context.CancelFunc) *RunStream {
	return WrapRunStream(agentcore.NewEventStream[agentcore.AgentEvent, []agentcore.AgentMessage](buffer), cancel)
}

// WrapRunStream adapts an existing agentcore event stream to a RunStream. It
// is the adapter seam for a loop that already owns such a stream (internal/loop
// returns one). cancel may be nil when the implementation is driven purely by
// the caller's context.
func WrapRunStream(stream *agentcore.EventStream[agentcore.AgentEvent, []agentcore.AgentMessage], cancel context.CancelFunc) *RunStream {
	return &RunStream{stream: stream, cancel: cancel}
}

// Events returns the receive-only event channel; ranging terminates when the
// producer closes the stream.
func (s *RunStream) Events() <-chan agentcore.AgentEvent { return s.stream.Events() }

// Emit publishes one event, honoring cancellation: it returns ctx.Err() when
// the context ended before the event was received (the event is dropped).
func (s *RunStream) Emit(ctx context.Context, ev agentcore.AgentEvent) error {
	return s.stream.Emit(ctx, ev)
}

// SetResult records the messages this run produced. Only the first call wins.
func (s *RunStream) SetResult(msgs []agentcore.AgentMessage) { s.stream.SetResult(msgs) }

// Close ends consumer iteration. It is called exactly once by the producer
// after the last Emit (Finish does both).
func (s *RunStream) Close() { s.stream.Close() }

// Finish settles the stream with the run's produced messages: SetResult then
// Close. A loop implementation calls it exactly once, after its last event.
func (s *RunStream) Finish(msgs []agentcore.AgentMessage) {
	s.stream.SetResult(msgs)
	s.stream.Close()
}

// Result blocks until the run settles and returns the messages it produced
// (never the pre-existing transcript), or the context error. The returned
// slice belongs to the caller; it is not shared with the loop.
func (s *RunStream) Result(ctx context.Context) ([]agentcore.AgentMessage, error) {
	return s.stream.Result(ctx)
}

// Cancel asks the run to stop. It is idempotent and safe to call at any time
// (including after the run settled, where it is a no-op in effect): the
// producer settles and closes the stream either way.
func (s *RunStream) Cancel() {
	if s.cancel != nil {
		s.cancel()
	}
}
