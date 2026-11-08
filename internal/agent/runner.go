package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loop"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
)

// CompactionOptions enables automatic context compaction for a runner. It is
// derived from the tenant document and supplied by the compaction service; a
// nil pointer keeps the loop's historical behavior (compaction off).
type CompactionOptions struct {
	ContextWindow int
	Settings      compaction.CompactionSettings
}

// Assembly is the host seam: the system prompt and tool registry for a run.
// host.Host implements it; tests can supply a static assembly.
type Assembly interface {
	SystemPrompt() string
	Tools() *agenttool.ToolRegistry
}

// Runner executes one claimed run: rebuild → (fill missing tool results) →
// loop → commit.
//
// The contract kernel owns the write path: every event the runner produces is
// committed through Committer with the run's fencing token, so a drained or
// fenced-out runtime cannot append after another owner took the session over.
// Reads go through the kernel's Reader, which carries the tenant boundary and
// rebuilds the transcript from committed events only.
type Runner struct {
	// Committer is the kernel write seam (Commit + Lookup). Required in
	// production; the runtime injects *kernel.Kernel. The runner uses Commit
	// only: duplicate-answer safety comes from the derived generationId (the
	// kernel dedupes a repeated generation) and from the transcript short
	// circuit, not from Lookup (see internal/kernel/README.md).
	Committer kernel.Committer
	// Events is the kernel read seam (LoadEvents + UnresolvedToolCalls).
	Events kernel.Reader
	// Reads is the kernel's durable read-point seam. The runner records the
	// load high-water mark immediately after loading the log and before the
	// first model call: a process killed before Finish must still leave the
	// fact behind, or the successor cannot tell which input this attempt never
	// saw (adversarial review N1/N2). NewRunner wires it from the committer when
	// that value is the kernel; a test double sets it explicitly.
	Reads  kernel.ReadMarker
	Stream provider.StreamFn
	// ProviderName and Model describe the model the runner calls; they are
	// captured into telemetry and logs.
	ProviderName string
	Model        string
	APIKey       string
	Tools        *agenttool.ToolRegistry
	// Assembly, when set, overrides Tools and supplies the system prompt. It is
	// the host layer (skills, MCP tools, tenant instructions) plugged into the
	// runtime; the kernel loop never knows about it.
	Assembly Assembly
	// Telemetry, when set, records one span and two metrics per run.
	Telemetry *RunTelemetry
	// Compaction, when set, enables automatic context compaction for every run
	// of this runner (the compaction service adapter). Nil keeps it off.
	Compaction *CompactionOptions
	MaxTurns   int
	Logger     *slog.Logger
}

// registry returns the tool registry for this run, preferring the host.
func (r *Runner) registry() *agenttool.ToolRegistry {
	if r.Assembly != nil {
		if reg := r.Assembly.Tools(); reg != nil {
			return reg
		}
	}
	return r.Tools
}

// systemPrompt returns the host-assembled prompt ("" when no host is wired).
func (r *Runner) systemPrompt() string {
	if r.Assembly != nil {
		return r.Assembly.SystemPrompt()
	}
	return ""
}

// NewRunner wires the production defaults: clock + echo tools and a turn cap.
// committer and events are the contract kernel seams; the runner holds no
// event-table handle of its own.
func NewRunner(committer kernel.Committer, events kernel.Reader, stream provider.StreamFn, providerName, model, apiKey string, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	reg := agenttool.NewToolRegistry()
	if err := reg.Register(ClockTool{}); err != nil {
		logger.Error("register clock tool", "err", err)
	}
	if err := reg.Register(EchoTool{}); err != nil {
		logger.Error("register echo tool", "err", err)
	}
	runner := &Runner{
		Committer:    committer,
		Events:       events,
		Stream:       stream,
		ProviderName: providerName,
		Model:        model,
		APIKey:       apiKey,
		Tools:        reg,
		MaxTurns:     8,
		Logger:       logger,
	}
	// The kernel satisfies all three seams; deriving the read marker from the
	// committer keeps the constructor honest without a fourth argument.
	if marker, ok := committer.(kernel.ReadMarker); ok {
		runner.Reads = marker
	}
	return runner
}

// RunSummary is the attempt's own account of what it did. The worker commits it
// as run.finished right before the attempt settles, so a client can report
// turns/tokens/stop reason without replaying the log. It is bookkeeping and
// never enters the model context.
type RunSummary struct {
	// Status is the attempt outcome: done, failed or cancelled.
	Status string `json:"status"`
	// Turns is how many assistant turns the loop completed.
	Turns int `json:"turns"`
	// Tokens are the provider's reported token counts for the attempt.
	Tokens TokenTotals `json:"tokens"`
	// ToolTimings records every tool execution of the attempt.
	ToolTimings []ToolTiming `json:"toolTimings,omitempty"`
	// Context describes the transcript the attempt ended with.
	Context ContextStats `json:"context"`
	// StopReason is why the attempt ended: a pi stop reason (end_turn,
	// tool_use, length), max_turns for a turn cap, error or aborted.
	StopReason string `json:"stopReason,omitempty"`
	// ErrorMessage is the terminal model/provider failure, when the attempt
	// ended in one. The worker records it as the run's last error.
	ErrorMessage string `json:"error,omitempty"`
	// DurationMs is the attempt's wall clock, load to settle.
	DurationMs int64 `json:"durationMs"`
}

// TokenTotals is the objective token accounting of an attempt (no prices: the
// contract layer owns money).
type TokenTotals struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Total  int `json:"total"`
}

// ToolTiming is one tool execution: the loop's own measurement (hooks
// excluded), matching pi's AgentToolCallOutcome.durationMs.
type ToolTiming struct {
	Tool       string `json:"tool,omitempty"`
	DurationMs int64  `json:"durationMs"`
	IsError    bool   `json:"isError,omitempty"`
}

// ContextStats describes the transcript an attempt ended with.
type ContextStats struct {
	// Messages is the number of messages in the final model context.
	Messages int `json:"messages"`
	// Tokens is the last provider request's input size (the closest objective
	// proxy for context size; zero when the provider reported no usage).
	Tokens int `json:"tokens"`
}

// Stop reason values the runner can produce beyond pi's set.
const (
	// StopReasonMaxTurns marks a run that ended because it hit the turn cap.
	StopReasonMaxTurns = "max_turns"
)

// RunResult is what one attempt produced: where it read up to, and how it went.
type RunResult struct {
	// Cursor is the last event sequence the attempt consumed or produced.
	Cursor int64
	// ReadThrough is the load watermark: the highest committed sequence the
	// transcript was built from. Input above it was never part of this attempt
	// (the worker passes it to Finish so the session re-queues).
	ReadThrough int64
	// Summary is the attempt-level account committed as run.finished.
	Summary RunSummary
}

// Run executes the session once under the claim (scope + fencing token). The
// returned cursor is the last event sequence the run consumed or produced; the
// caller uses it to decide whether more input arrived while the run was
// executing.
func (r *Runner) Run(ctx context.Context, lease kernel.Lease) (int64, error) {
	res, err := r.RunAttempt(ctx, lease)
	return res.Cursor, err
}

// RunWithWatermark executes the run once and additionally reports the load
// high-water mark: the highest committed sequence the transcript was built
// from. The cursor alone is not a safe "did input arrive during the run?"
// probe — the run's own commits advance it past input that arrived mid-run and
// was never part of the transcript (which would strand that input: the run
// settles done and no claim ever picks the message up again). The watermark is
// the truthful boundary: a user.message with a higher sequence must force a
// re-queue, which the worker does by passing it to FinishRequest.ReadThrough.
func (r *Runner) RunWithWatermark(ctx context.Context, lease kernel.Lease) (int64, int64, error) {
	res, err := r.RunAttempt(ctx, lease)
	return res.Cursor, res.ReadThrough, err
}

// RunAttempt executes the run once and returns the full result: cursor, read
// watermark and the attempt summary. The worker uses it so the settle carries
// the summary it commits as run.finished.
func (r *Runner) RunAttempt(ctx context.Context, lease kernel.Lease) (RunResult, error) {
	if r.Committer == nil || r.Events == nil || r.Reads == nil {
		return RunResult{}, errors.New("agent: runner needs the contract kernel seams (Committer, Reader and ReadMarker)")
	}
	started := time.Now()
	var res RunResult
	cursor, err := r.observeRun(lease, func() (int64, error) {
		out, runErr := r.run(ctx, lease)
		res = out
		return out.Cursor, runErr
	})
	res.Cursor = cursor
	res.Summary.DurationMs = time.Since(started).Milliseconds()
	switch {
	case err != nil:
		res.Summary.Status = kernel.RunSummaryFailed
		if res.Summary.StopReason == "" {
			res.Summary.StopReason = agentcore.StopReasonError
		}
	case res.Summary.StopReason == agentcore.StopReasonError:
		// pi reports a provider failure as a terminal assistant message
		// (stopReason=error), not as a Go error. The attempt failed all the same;
		// the worker turns this into the settle's error so the run is retried and
		// the row records why.
		res.Summary.Status = kernel.RunSummaryFailed
	case res.Summary.Status == "":
		res.Summary.Status = kernel.RunSummaryDone
	}
	if res.Summary.StopReason == "" {
		res.Summary.StopReason = agentcore.StopReasonEndTurn
	}
	return res, err
}

func (r *Runner) run(ctx context.Context, lease kernel.Lease) (RunResult, error) {
	var res RunResult
	events, cursor, err := r.loadEvents(ctx, lease.Scope)
	// The load high-water mark: everything the transcript was built from.
	// Input committed after it was not seen and must re-queue the run.
	res.ReadThrough = cursor
	res.Cursor = cursor
	if err != nil {
		return res, err
	}
	// Persist the load point BEFORE anything else (in particular before
	// fillMissingToolResults can commit): whatever happens next — a model
	// failure, a kill, a drain — the successor can see exactly which input this
	// attempt did not read. A failure here aborts the attempt while nothing has
	// been committed yet, so the retry is clean.
	if err := r.Reads.RecordRead(ctx, lease, cursor); err != nil {
		return res, fmt.Errorf("agent: record read point: %w", err)
	}
	// A payload the kernel would not accept today (legacy or foreign data) is
	// skipped with a diagnostic instead of failing the run: the session must
	// keep moving, and the skipped sequence is visible in the log line.
	msgs, seqs := DecodeMessages(events, func(seq int64, kind string, err error) {
		r.Logger.Warn("skipping unreadable event", "session_id", lease.Scope.SessionID,
			"seq", seq, "kind", kind, "err", err)
	})
	if len(msgs) == 0 {
		return res, nil
	}
	// Nothing is owed to the model when every committed input is covered. The
	// decision is durable, not a transcript-tail guess: the claim delivers the
	// previous attempt's read watermark, so input committed after it is known
	// to be unread even though an answer the previous attempt produced sits
	// after that input in the log.
	if !owedTurn(events, lease.ReadWatermark) {
		return res, nil
	}

	// A previous process may have died after committing an assistant tool call
	// but before committing its results. Execute the missing calls now (at least
	// once) so the model sees a complete turn.
	msgs, seqs, cursor, err = r.fillMissingToolResults(ctx, lease, msgs, seqs, cursor)
	res.Cursor = cursor
	if err != nil {
		return res, err
	}
	// The model must see the input this transcript owes an answer to LAST:
	// input that arrived while the previous attempt was running sits before
	// that attempt's answer in the log, so leaving the log order intact makes
	// the model treat the input as already answered (no reply, no tool call —
	// adversarial review B5). "Unread" is the consumption fact when the log
	// carries it: a guide input answered in an earlier attempt must NOT be
	// presented as a new prompt (that would answer it twice).
	unread, consumptionKnown := unreadInputs(events, lease.ReadWatermark)
	msgs = presentUnreadInputLast(msgs, seqs, func(seq int64) bool {
		if consumptionKnown {
			return unread[seq]
		}
		// A read watermark of zero means no attempt has ever proven a read
		// point: the log order is the only information available and is left
		// alone.
		return lease.ReadWatermark > 0 && seq > lease.ReadWatermark
	})

	// The inputs this attempt actually presents to the model. The assistant
	// message records them (payload.inputSeqs) so "unanswered input" is a fact
	// in the log rather than a watermark heuristic: a successor can tell a
	// queue input from a guide input, and a crash between the assistant commit
	// and Finish cannot duplicate an answer. The two slices move in lockstep
	// (presentUnreadInputLast returns them unchanged on a length mismatch).
	inputSeqs := make([]int64, 0, len(msgs))
	if len(seqs) == len(msgs) {
		for i, m := range msgs {
			if m.Role() == agentcore.RoleUser {
				inputSeqs = append(inputSeqs, seqs[i])
			}
		}
	}

	// Generation identity: the kernel dedupes assistant messages by
	// generationId, and the id must be the same for a retried attempt, so it is
	// derived from the committed log (see generationBase).
	generation := generationBase(events)
	// The steering feed is the loop's GetSteeringMessages source: guide inputs
	// committed after this attempt loaded its transcript, handed out one at a
	// time at PI's drain points (see guideFeed).
	feed := newGuideFeed(r, lease, res.ReadThrough)
	summary := &res.Summary
	turns := 0
	tools := r.registry()
	// Only a trailing user input is this run's prompt; a run that continues from
	// committed tool results must not replay prompt lifecycle events for them.
	promptCount := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role() != agentcore.RoleUser {
			break
		}
		promptCount++
	}
	cfg := loop.RunConfig{
		LoopConfig: loop.LoopConfig{
			Model:         r.Model,
			Provider:      r.ProviderName,
			APIKey:        r.APIKey,
			Stream:        r.Stream,
			ThinkingLevel: agentcore.ThinkingLow,
			// Transcript tool-loadout declarations (system messages) are not part
			// of the provider wire in Keel: tools travel in LlmContext.Tools, and
			// the OpenAI/Anthropic encoders would drop the system entries anyway.
			// Filter them here so the provider sees the conversational transcript
			// (pi's collapseSystemMessages at the provider boundary).
			ConvertToLlm: func(msgs agentcore.MessageList) agentcore.MessageList {
				out := make(agentcore.MessageList, 0, len(msgs))
				for _, m := range msgs {
					if m.Role() != agentcore.RoleSystem {
						out = append(out, m)
					}
				}
				return out
			},
			// Compaction is off unless the loadout's compaction service
			// supplies settings; the loop supports it via OnCompaction.
		},
		Batch: agenttool.BatchConfig{
			ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: tools},
		},
		EventBuffer: 32,
		SessionID:   lease.Scope.SessionID.String(),
		// The run is triggered by its trailing user message(s); they are the
		// prompt whose message lifecycle pi emits.
		PromptCount: promptCount,
		// Guide input is injected inside the running turn: the loop polls this
		// before the first request, after every completed turn (all sibling tool
		// results committed) and again after PrepareNextTurn.
		GetSteeringMessages: feed.poll,
		// The request boundary is where the consumption fact is pinned: the loop
		// has appended the pending steering messages and is about to call the
		// provider, so the snapshot is exactly what this request presents. The
		// runner reads it by request order (structural alignment), never by
		// "current feed state" — the loop may run ahead through the event buffer.
		PrepareRequest: func(ctx context.Context, req loop.PrepareRequestContext) *loop.RequestUpdate {
			feed.beginRequest()
			return nil
		},
		ShouldStopAfterTurn: func(ctx context.Context, agentCtx *agentcore.AgentContext) bool {
			turns++
			if r.MaxTurns > 0 && turns >= r.MaxTurns {
				summary.StopReason = StopReasonMaxTurns
				return true
			}
			return false
		},
		OnCompaction: func(ctx context.Context, summary string, firstKeptIndex, summarizedCount int) {
			raw, err := marshalPayload(map[string]any{
				"summary":    summary,
				"firstKept":  firstKeptIndex,
				"summarized": summarizedCount,
				"timestamp":  time.Now().UnixMilli(),
			})
			if err != nil {
				r.Logger.Warn("persist compaction", "err", err)
				return
			}
			seq, err := r.commitEvent(ctx, lease, kernel.CommitRequest{
				Kind:    kernel.KindCompaction,
				Payload: raw,
			})
			if err != nil {
				r.Logger.Warn("persist compaction", "err", err)
				return
			}
			cursor = seq
		},
	}
	if r.Compaction != nil && r.Compaction.Settings.Enabled && r.Compaction.ContextWindow > 0 {
		cfg.ContextWindow = r.Compaction.ContextWindow
		cfg.Compaction = r.Compaction.Settings
	}
	agentCtx := &agentcore.AgentContext{
		SystemPrompt: r.systemPrompt(),
		Messages:     msgs,
		Tools:        tools.List(),
	}
	stream := loop.StartRun(ctx, agentCtx, cfg)
	for ev := range stream.Events() {
		if ctx.Err() != nil {
			res.Cursor = cursor
			summary.StopReason = agentcore.StopReasonAborted
			return res, ctx.Err()
		}
		switch e := ev.(type) {
		case agentcore.TurnEndEvent:
			summary.Turns++
		case agentcore.MessageEndEvent:
			// User prompts and tool results are persisted by their own paths
			// (input submission / tool_execution_end); only assistant answers
			// belong to assistant.message.
			if e.Message.Role() != agentcore.RoleAssistant {
				continue
			}
			if assistant, ok := e.Message.(agentcore.AssistantMessage); ok {
				if assistant.Usage != nil {
					summary.Tokens.Input += assistant.Usage.InputTokens
					summary.Tokens.Output += assistant.Usage.OutputTokens
					summary.Context.Tokens = assistant.Usage.InputTokens
				}
				if assistant.StopReason != "" {
					summary.StopReason = assistant.StopReason
				}
				if assistant.StopReason == agentcore.StopReasonError {
					summary.ErrorMessage = assistant.ErrorMessage
				}
			}
			raw, err := marshalPayload(e.Message)
			if err != nil {
				res.Cursor = cursor
				return res, err
			}
			// Record what this answer consumed: the committed user inputs this
			// request presented to the model (the transcript it was built from
			// plus every guide input the loop has injected since). owedTurn uses
			// the union of these sets (not a watermark) to decide what is still
			// owed.
			raw, err = attachInputSeqs(raw, mergeInputSeqs(inputSeqs, feed.takeRequestSeqs()))
			if err != nil {
				res.Cursor = cursor
				return res, err
			}
			seq, err := r.commitEvent(ctx, lease, kernel.CommitRequest{
				Kind:         kernel.KindAssistantMessage,
				GenerationID: generation.next(),
				Payload:      raw,
			})
			if err != nil {
				res.Cursor = cursor
				return res, err
			}
			cursor = seq
		case agentcore.ToolExecutionEndEvent:
			timing := ToolTiming{Tool: e.ToolName, IsError: e.IsError}
			if e.DurationMs != nil {
				timing.DurationMs = *e.DurationMs
			}
			summary.ToolTimings = append(summary.ToolTimings, timing)
			msg := agentcore.ToolResultMessage{
				RoleField:  agentcore.RoleToolResult,
				ToolCallID: e.ToolCallID,
				ToolName:   e.ToolName,
				Content:    e.Result.Content,
				Details:    e.Result.Details,
				IsError:    e.IsError,
			}
			seq, err := r.appendToolResult(ctx, lease, msg)
			if err != nil {
				res.Cursor = cursor
				return res, err
			}
			cursor = seq
		}
	}
	if _, err := stream.Result(ctx); err != nil && !errors.Is(err, context.Canceled) {
		res.Cursor = cursor
		return res, err
	}
	// The final context size is the transcript the loop ended with; steering may
	// have extended it beyond the messages this attempt loaded.
	summary.Context.Messages = len(agentCtx.Messages)
	summary.Tokens.Total = summary.Tokens.Input + summary.Tokens.Output
	res.Cursor = cursor
	return res, nil
}

// generation names one model generation: the seq of the newest committed user
// message the run answers plus the ordinal of the assistant message among the
// messages committed after that user message. Both terms are read back from the
// committed log, so a retried run derives exactly the same id as the attempt it
// is resuming and the kernel reuses the committed fact instead of appending a
// second answer (contract-kernel: generationId idempotency).
type generation struct {
	promptSeq int64
	ordinal   int
}

// generationBase reads the generation identity of the next model call out of
// the committed transcript.
func generationBase(events []store.Event) generation {
	var g generation
	for _, ev := range events {
		switch ev.Kind {
		case store.EventUserMessage:
			g.promptSeq = ev.Seq
			g.ordinal = 0
		case store.EventAssistantMessage:
			g.ordinal++
		}
	}
	return g
}

// next returns the generation id for the next assistant message and advances
// the ordinal.
func (g *generation) next() string {
	id := fmt.Sprintf("u%d-a%d", g.promptSeq, g.ordinal)
	g.ordinal++
	return id
}

// owedTurn reports whether the run must produce a model turn.
//
// The authoritative rule is the consumption fact recorded in the log: every
// assistant message carries payload.inputSeqs — the committed user inputs that
// request presented to the model. A user message no assistant references is
// unanswered, whatever the watermark says. This is what makes queue and guide
// inputs exact across crashes and interleavings:
//
//   - an answer committed before a crash has already consumed its inputs, so a
//     successor does not answer them twice;
//   - an attempt killed before committing its answer consumed nothing, so its
//     inputs stay owed;
//   - a guide input consumed in a turn and a queue input left for the next turn
//     are distinguished by their seqs, not by their position relative to a
//     watermark.
//
// Legacy logs (no assistant message carries inputSeqs) fall back to the old
// watermark + tail heuristic, so historical sessions keep working. A log that
// straddles the upgrade (some answers carry the fact, older ones do not) keeps
// the legacy decision for the pre-upgrade prefix: re-deciding those inputs from
// inputSeqs alone would answer them a second time.
//
// Independently of input consumption, the newest assistant message carrying
// tool calls means the loop must fill their results and continue.
//
// Bookkeeping events (run.started, run.failed, run.finished, compaction) never
// answer input.
func owedTurn(events []store.Event, readWatermark int64) bool {
	if unread, known := unreadInputs(events, readWatermark); known {
		// A terminal failure (stopReason error/aborted) is not an answer: the
		// turn it was producing was never completed, so the loop must run it
		// again. Without this a failed second turn after a completed tool turn
		// would leave nothing owed and the retry would settle the run "done"
		// without ever calling the model.
		return len(unread) > 0 || newestAssistantHasToolCalls(events) || newestAssistantTerminallyFailed(events)
	}
	return legacyOwedTurn(events, readWatermark)
}

// unreadInputs returns the committed user inputs that still owe an answer, plus
// whether the log carries any consumption fact at all.
//
// A terminal failure (stopReason error/aborted) does NOT consume its input: the
// model saw it but produced no answer, so the input stays owed and a retry
// re-asks. Only a completed turn (end_turn/tool_use/…) is an answer.
//
// The upgrade boundary is the seq of the first assistant message carrying
// inputSeqs: everything committed before it was decided by the old watermark
// heuristic, and an input below it that the watermark already covered is
// treated as answered (re-answering old input is the one outcome this fact
// must never cause).
func unreadInputs(events []store.Event, readWatermark int64) (map[int64]bool, bool) {
	answered := make(map[int64]bool)
	var boundary int64
	known := false
	for _, ev := range events {
		if ev.Kind != store.EventAssistantMessage {
			continue
		}
		var payload struct {
			InputSeqs  []int64 `json:"inputSeqs"`
			StopReason string  `json:"stopReason"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			continue
		}
		if len(payload.InputSeqs) == 0 {
			continue
		}
		if !known {
			boundary = ev.Seq
			known = true
		}
		if payload.StopReason == agentcore.StopReasonError || payload.StopReason == agentcore.StopReasonAborted {
			continue
		}
		for _, seq := range payload.InputSeqs {
			answered[seq] = true
		}
	}
	if !known {
		return nil, false
	}
	unread := make(map[int64]bool)
	for _, ev := range events {
		if ev.Kind != store.EventUserMessage || answered[ev.Seq] {
			continue
		}
		// A pre-upgrade input that the watermark already covered keeps the old
		// decision. This holds even when a boundary message *presented* it: the
		// boundary message may itself be a terminal failure (the first
		// post-upgrade attempt failed), and its presentation consumed nothing.
		if ev.Seq < boundary && readWatermark > 0 && ev.Seq <= readWatermark {
			continue
		}
		unread[ev.Seq] = true
	}
	return unread, true
}

// newestAssistantTerminallyFailed reports whether the newest assistant message
// is a terminal failure (the provider errored or the turn was aborted). Such a
// message is a record of an attempt, not an answer: the turn still owes work.
func newestAssistantTerminallyFailed(events []store.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != store.EventAssistantMessage {
			continue
		}
		var payload struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			return false
		}
		return payload.StopReason == agentcore.StopReasonError || payload.StopReason == agentcore.StopReasonAborted
	}
	return false
}

// newestAssistantHasToolCalls reports whether the newest assistant message
// declares tool calls (their results may still be missing after a crash).
func newestAssistantHasToolCalls(events []store.Event) bool {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != store.EventAssistantMessage {
			continue
		}
		var m agentcore.AssistantMessage
		if err := json.Unmarshal(events[i].Payload, &m); err != nil {
			// A payload that cannot be decoded cannot be answered for; the
			// transcript skips it with a diagnostic.
			return false
		}
		return len(m.ToolCalls()) > 0
	}
	return false
}

// legacyOwedTurn is the pre-inputSeqs heuristic, kept for logs that predate the
// consumption fact (and for messages persisted by helpers that do not record
// it). New logs never take this path.
func legacyOwedTurn(events []store.Event, readWatermark int64) bool {
	if readWatermark > 0 {
		for _, ev := range events {
			if ev.Kind == store.EventUserMessage && ev.Seq > readWatermark {
				return true
			}
		}
	}
	if len(events) > 0 && events[len(events)-1].Kind == store.EventUserMessage {
		return true
	}
	return newestAssistantHasToolCalls(events)
}

// fillMissingToolResults executes tool calls that a crashed process left
// unresolved. The call is at-least-once; tools are expected to be idempotent or
// to carry their own idempotency keys.
func (r *Runner) fillMissingToolResults(ctx context.Context, lease kernel.Lease, msgs agentcore.MessageList, seqs []int64, cursor int64) (agentcore.MessageList, []int64, int64, error) {
	lastAssistant := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := msgs[i].(agentcore.AssistantMessage); ok {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 {
		return msgs, seqs, cursor, nil
	}
	assistant, _ := msgs[lastAssistant].(agentcore.AssistantMessage)
	calls := assistant.ToolCalls()
	if len(calls) == 0 {
		return msgs, seqs, cursor, nil
	}
	done := map[string]bool{}
	for _, m := range msgs[lastAssistant+1:] {
		if tr, ok := m.(agentcore.ToolResultMessage); ok {
			done[tr.ToolCallID] = true
		}
	}
	var missing []agentcore.AgentToolCall
	for _, c := range calls {
		if !done[c.ID] {
			missing = append(missing, agentcore.AgentToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
	}
	if len(missing) == 0 {
		return msgs, seqs, cursor, nil
	}
	r.Logger.Warn("resuming unresolved tool calls", "session", lease.Scope.SessionID, "count", len(missing))
	results, _ := agenttool.ExecuteToolCalls(ctx, agenttool.BatchConfig{
		ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: r.registry()},
	}, missing, nil)
	for _, tr := range results {
		msgs = append(msgs, tr)
		// A re-executed result was not read from the log: it belongs after the
		// call it answers, never in the "unread input" set.
		seqs = append(seqs, 0)
		seq, err := r.appendToolResult(ctx, lease, tr)
		if err != nil {
			return msgs, seqs, cursor, err
		}
		cursor = seq
	}
	return msgs, seqs, cursor, nil
}

// presentUnreadInputLast moves the user messages no committed answer consumed
// (unread(seq)) to the end of the transcript, keeping their relative order. The
// rest of the transcript — including the previous attempt's answer, which sits
// AFTER that input in the log — stays in front. A model then sees the input it
// owes an answer to as its prompt, so the input is actually answered (and any
// tool call it triggers executes) instead of being swallowed by an answer to
// older input.
//
// unread is the caller's rule: the inputSeqs consumption fact for new logs, the
// read watermark for legacy ones.
func presentUnreadInputLast(msgs agentcore.MessageList, seqs []int64, unread func(seq int64) bool) agentcore.MessageList {
	if len(msgs) == 0 || len(seqs) != len(msgs) {
		return msgs
	}
	kept := make(agentcore.MessageList, 0, len(msgs))
	moved := make(agentcore.MessageList, 0, len(msgs))
	for i, m := range msgs {
		// seq 0 marks a message rebuilt in-process (a re-executed tool result),
		// never a committed user input.
		if m.Role() == agentcore.RoleUser && seqs[i] > 0 && unread(seqs[i]) {
			moved = append(moved, m)
			continue
		}
		kept = append(kept, m)
	}
	if len(moved) == 0 {
		return msgs
	}
	return append(kept, moved...)
}

// loadEvents reads the whole committed log, page by page. The kernel read seam
// answers ErrNotFound for a foreign or missing session.
func (r *Runner) loadEvents(ctx context.Context, scope kernel.Scope) ([]store.Event, int64, error) {
	var all []store.Event
	var after int64
	for {
		page, err := r.Events.LoadEvents(ctx, scope, after, 500)
		if err != nil {
			return nil, after, err
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			all = append(all, store.Event{Seq: ev.Seq, Kind: string(ev.Kind), Payload: ev.Payload})
		}
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return all, after, nil
}

// appendToolResult commits a tool result through the kernel with its
// toolCallId as the idempotency key. The execution layer is at-least-once; the
// log holds exactly one result per call, and the kernel rejects a result whose
// call is not committed (no orphan results).
func (r *Runner) appendToolResult(ctx context.Context, lease kernel.Lease, msg agentcore.ToolResultMessage) (int64, error) {
	raw, err := marshalPayload(msg)
	if err != nil {
		r.Logger.Error("marshal tool result", "err", err)
		return 0, err
	}
	return r.commitEvent(ctx, lease, kernel.CommitRequest{
		Kind:       kernel.KindToolResult,
		ToolCallID: msg.ToolCallID,
		Payload:    raw,
	})
}

// commitEvent fills the scope and the run's fencing token into the request and
// commits it through the kernel. It returns the sequence the fact was committed
// at (the existing one on a deduplicated replay).
func (r *Runner) commitEvent(ctx context.Context, lease kernel.Lease, req kernel.CommitRequest) (int64, error) {
	req.Scope = lease.Scope
	req.Token = lease.Token
	receipt, err := r.Committer.Commit(ctx, req)
	if err != nil {
		r.Logger.Error("commit event", "kind", req.Kind, "err", err)
		return 0, err
	}
	if receipt.Deduped {
		r.Logger.Debug("commit event deduplicated", "kind", req.Kind, "seq", receipt.Seq)
	}
	return receipt.Seq, nil
}

// marshalPayload marshals an event payload. A failure (a tool returning a
// value JSON cannot represent, for example) keeps the caller on the error path
// instead of storing a half-empty fact.
func marshalPayload(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal event payload: %w", err)
	}
	return raw, nil
}

// attachInputSeqs adds the consumed-input fact (payload.inputSeqs) to an
// assistant message payload. Values are kept as raw JSON so the message's own
// encoding is untouched; only the added key changes. An empty set is omitted:
// there is nothing to consume.
func attachInputSeqs(raw json.RawMessage, seqs []int64) (json.RawMessage, error) {
	if len(seqs) == 0 {
		return raw, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("agent: assistant payload is not an object: %w", err)
	}
	encoded, err := json.Marshal(seqs)
	if err != nil {
		return nil, fmt.Errorf("agent: encode inputSeqs: %w", err)
	}
	payload["inputSeqs"] = encoded
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("agent: marshal assistant payload: %w", err)
	}
	return out, nil
}

// MarshalAssistant is a small helper for callers that persist a message outside
// the loop (tests, migrations).
func MarshalAssistant(m agentcore.AssistantMessage) (json.RawMessage, error) {
	return json.Marshal(m)
}
