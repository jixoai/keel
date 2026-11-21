package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/compaction"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loop"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/telemetry"
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
	Reads kernel.ReadMarker
	// Checkpoints is the compaction-projection seam. When set, the runner saves
	// the checkpoint after committing a compaction event and replays a new run
	// from the newest anchored checkpoint (summary + retained tail) instead of
	// the whole log. It is optional: a test double or a deployment without the
	// seam falls back to the full committed log.
	Checkpoints kernel.CheckpointStore
	// ForkPrefixes, when set, resolves the parent ledger prefix a forked session
	// inherits. The prefix is read at run time from the parent log (a reference,
	// never a copy) and prepended to the model transcript; it never participates
	// in the child's owed-turn/unread-input decisions.
	ForkPrefixes ForkPrefixReader
	Stream       provider.StreamFn
	// ProviderName and Model describe the static model the runner calls; they
	// are captured into telemetry and logs and stay the identity of a runner
	// without a Models resolver (the fakestream path and the legacy flat
	// configuration).
	ProviderName string
	Model        string
	APIKey       string
	// ThinkingLevel is the static reasoning level of a runner without a
	// resolver; empty means ThinkingLow (the historical default).
	ThinkingLevel agentcore.ThinkingLevel
	// ConfigHash is the static configuration identity of this runner's
	// generation, recorded in the run snapshot (configHashBefore/After). The
	// worker overwrites it from the loadout the attempt pinned, so a production
	// run records the configuration revision it actually executed under; this
	// field is the fallback for hosts (and tests) that drive a Runner without a
	// binding. Empty means the deployment supplies no configuration identity.
	ConfigHash string
	// Models, when set, resolves the execution identity per attempt from the
	// run's selection fact (the product-layer provider configuration). It is
	// consulted per attempt, so a configuration change applies to runs that
	// start after it without disturbing the ones already in flight.
	Models ModelResolver
	// Bindings, when set, resolves the attempt's execution binding: the Agent or
	// AgentTeam the input selected, its system prompt and its tool grants. It is
	// consulted per attempt (next to Models), so a team/agent edit applies to
	// the next attempt without touching an attempt already in flight.
	Bindings BindingResolver
	Tools    *agenttool.ToolRegistry
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
	// Images resolves the durable image references in the transcript into the
	// wire form the attempt's provider accepts, immediately before each request
	// (see internal/agent/images.go). Nil means the deployment has no image
	// pipeline: a referenced image degrades to a text notice naming why, and the
	// run continues.
	Images ImageResolver
	// ImageWorkspace is the blob scope images resolve against (the tenant id).
	// Empty with a non-nil Images is refused per image, not per run: the
	// transcript still reaches the model.
	ImageWorkspace string
	// SessionContexts, when set, supplies extra context blocks for this attempt
	// (the MCP app context written by ui/update-model-context). They are merged
	// into the attempt's last user message, in memory only: nothing is committed
	// to the ledger (an app context is not a session fact; spec「账本零新增
	// kind」). Nil keeps the pre-P5 prompt byte-identical.
	SessionContexts func(ctx context.Context, scope kernel.Scope) []agentcore.Content
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

// systemPromptFor returns the prompt of this attempt: the binding's system
// prompt when it contributes one, otherwise the host-assembled deployment
// prompt. The host keeps rendering its dynamic surface (MCP section, plugin
// sections, skills index) around the preamble, so selecting an Agent never
// silently drops those.
func (r *Runner) systemPromptFor(surface RunSurface) string {
	prompt := strings.TrimSpace(surface.Prompt)
	if prompt == "" {
		// A team captain with no declared prompt still needs the roster: it is
		// the only way delegation can name a member.
		if roster := renderTeamMembers(surface.Members); roster != "" {
			prompt = roster
			if renderer, ok := r.Assembly.(PreambleRenderer); ok {
				return renderer.RenderSystemPrompt(prompt)
			}
			return r.systemPrompt() + "\n\n" + roster
		}
		return r.systemPrompt()
	}
	if roster := renderTeamMembers(surface.Members); roster != "" {
		prompt += "\n\n" + roster
	}
	if renderer, ok := r.Assembly.(PreambleRenderer); ok {
		return renderer.RenderSystemPrompt(prompt)
	}
	// A host that cannot re-render the preamble around its dynamic sections is a
	// wiring error (the servicehost host does implement the renderer). The
	// binding's prompt is still the run's prompt — dropping the MCP/plugin/skills
	// sections is bad, but refusing the attempt is worse — so the degradation is
	// recorded instead of being silent.
	r.Logger.Warn("host cannot render dynamic prompt sections; the bound prompt is sent alone",
		"agent", surface.AgentID)
	return prompt
}

// toolsForRun returns the attempt's tool registry: the host registry filtered by
// the binding's grants, or the whole registry when the binding declares none.
// sessionID is diagnostic context only (the team auto-keep warning names it).
func (r *Runner) toolsForRun(surface RunSurface, sessionID string) (*agenttool.ToolRegistry, error) {
	registry := r.registry()
	if registry == nil || surface.ToolIDs == nil {
		return registry, nil
	}
	names := make([]string, 0, len(surface.ToolIDs))
	for name, enabled := range surface.ToolIDs {
		if !enabled {
			continue
		}
		names = append(names, name)
	}
	// A tool id the registry does not serve (an uninstalled plugin) is dropped
	// with a diagnostic: the spec keeps the user's configuration and shows it as
	// unavailable, it never blocks the run.
	for _, name := range names {
		if _, ok := registry.Get(name); !ok {
			r.Logger.Warn("agent binding grants an unavailable tool", "tool", name, "agent", surface.AgentID)
		}
	}
	if surface.TeamMode {
		// R3: a team whose captain could never delegate is a silently broken
		// product. The delegation primitive stays available unless the captain
		// explicitly disabled it in its own grants (an explicit decision wins).
		if enabled, declared := surface.ToolIDs[SpawnToolName]; !declared || enabled {
			if _, ok := registry.Get(SpawnToolName); ok {
				names = append(names, SpawnToolName)
			} else {
				// The registry does not serve the primitive. Nothing can be added
				// to the surface, but a captain silently unable to delegate is
				// exactly the broken product R3 forbids: leave the diagnostic.
				r.Logger.Warn("team mode cannot keep the delegation primitive: the registry does not serve it",
					"tool", SpawnToolName, "agent", surface.AgentID, "team", surface.TeamID,
					"session_id", sessionID)
			}
		}
	}
	subset, err := registry.Subset(names)
	if err != nil {
		return nil, fmt.Errorf("agent: agent binding tool surface: %w", err)
	}
	return subset, nil
}

// SpawnToolName is the delegation primitive a team run always keeps. It is the
// `agent` aggregate tool (action spawn), not a per-capability tool name.
const SpawnToolName = "agent"

// PreambleRenderer is the host capability the binding needs: render the loaded
// assembly with a different preamble (the agent's system prompt) while keeping
// the dynamic sections (MCP, plugins, skills) in place.
type PreambleRenderer interface {
	RenderSystemPrompt(preamble string) string
}

// NewRunner wires the production defaults (an empty fallback registry and a turn
// cap). The fallback registry is empty on purpose: the model-visible surface is
// the host assembly (the 11 builtin aggregate tools plus approved plugin tools),
// and the old clock/echo development tools live in the keel.testkit plugin.
// Tests that drive the deterministic echo round-trip register it on
// runner.Tools themselves. committer and events are the contract kernel seams;
// the runner holds no event-table handle of its own.
func NewRunner(committer kernel.Committer, events kernel.Reader, stream provider.StreamFn, providerName, model, apiKey string, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	runner := &Runner{
		Committer:    committer,
		Events:       events,
		Stream:       stream,
		ProviderName: providerName,
		Model:        model,
		APIKey:       apiKey,
		Tools:        agenttool.NewToolRegistry(),
		MaxTurns:     8,
		Logger:       logger,
	}
	// The kernel satisfies all three seams; deriving the read marker from the
	// committer keeps the constructor honest without a fourth argument. The
	// checkpoint seam is optional (it is a projection, not a write authority),
	// so only a committer that implements it enables checkpoint replay.
	if marker, ok := committer.(kernel.ReadMarker); ok {
		runner.Reads = marker
	}
	if checkpoints, ok := committer.(kernel.CheckpointStore); ok {
		runner.Checkpoints = checkpoints
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
	// Provider and Model record the resolved execution identity of the attempt
	// (providerId + modelId). They are in the ledger because "which model
	// answered this turn" is an audit fact, and because a retry after a
	// configuration change must be distinguishable from the attempt it
	// replaces.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ConfigHashBefore/After pin the harness configuration identity around the
	// attempt. They are the run snapshot's configHash: the attempt runs under
	// one pinned generation, so a configuration rotation can never change the
	// value mid-attempt and both fields stay equal. The worker fills them from
	// the loadout the attempt pinned; Runner.ConfigHash is the fallback for
	// hosts that drive the runner without a binding. Empty is allowed for logs
	// predating the field.
	ConfigHashBefore string `json:"configHashBefore,omitempty"`
	ConfigHashAfter  string `json:"configHashAfter,omitempty"`
	// AnchorEventSeq pins where the attempt's configuration snapshot starts. A
	// committed config.rotated event is the anchor (the configuration became
	// active there); without one the load high-water mark is used — the newest
	// committed sequence the transcript was built from, i.e. the log position
	// at which this run begins. Always emitted: zero means the log was empty.
	AnchorEventSeq int64 `json:"anchorEventSeq"`
	// ToolManifestHash pins the model-visible tool manifest injected into this
	// attempt, hashed at attempt start from the exact tool slice handed to the
	// loop (the frozen surface). A later MCP/plugin refresh cannot move it. An
	// attempt that injected no tools records the empty-manifest digest.
	ToolManifestHash string `json:"toolManifestHash,omitempty"`
	// AgentID, TeamID, TeamRevision, TeamHash and PromptHash are the execution
	// binding the attempt froze (RUNTIME-PLAN §b): which Agent answered, which
	// AgentTeam it came from, that team's stored revision and content hash, and
	// the digest of the system prompt the attempt used. Empty means "the
	// deployment default answered" (the pre-binding behaviour).
	AgentID      string `json:"agentId,omitempty"`
	TeamID       string `json:"teamId,omitempty"`
	TeamRevision uint64 `json:"teamRevision,omitempty"`
	TeamHash     string `json:"teamHash,omitempty"`
	PromptHash   string `json:"promptHash,omitempty"`
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
	cursor, err := r.observeRun(lease, func(span telemetry.Span) (int64, error) {
		out, runErr := r.run(ctx, lease, span)
		res = out
		return out.Cursor, runErr
	})
	res.Cursor = cursor
	res.Summary.DurationMs = time.Since(started).Milliseconds()
	// The runner's static identity is the fallback for hosts without a loadout
	// binding. In production the worker replaces both fields with the pinned
	// loadout's ConfigHash before committing run.finished, so this can never
	// upgrade a stale value into a fresh one.
	if res.Summary.ConfigHashBefore == "" {
		res.Summary.ConfigHashBefore = r.ConfigHash
	}
	if res.Summary.ConfigHashAfter == "" {
		res.Summary.ConfigHashAfter = r.ConfigHash
	}
	// Every run.finished carries a tool manifest hash. An attempt that returned
	// before resolving a surface (empty log, nothing owed) injected no tools,
	// and the empty-manifest digest is the honest value for that.
	if res.Summary.ToolManifestHash == "" {
		if emptyManifest, err := agenttool.ManifestHash(nil); err == nil {
			res.Summary.ToolManifestHash = emptyManifest
		}
	}
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
	// The metric records the attempt's settled outcome, so it is recorded after
	// the status is derived: a provider-turn failure rides the loop as a terminal
	// message and would otherwise be posted as a success.
	r.recordRunMetrics(res, started, err)
	return res, err
}

func (r *Runner) run(ctx context.Context, lease kernel.Lease, span telemetry.Span) (RunResult, error) {
	var res RunResult
	events, cursor, err := r.loadEvents(ctx, lease.Scope)
	// The load high-water mark: everything the transcript was built from.
	// Input committed after it was not seen and must re-queue the run.
	res.ReadThrough = cursor
	res.Cursor = cursor
	// The run snapshot's configuration anchor is taken from the loaded log
	// before anything is committed. A config.rotated event is the authoritative
	// anchor; without one the run-start read-through is used (see
	// runSnapshotAnchor).
	anchor, rotatedConfigHash := runSnapshotAnchor(events, cursor)
	res.Summary.AnchorEventSeq = anchor
	if hash := firstNonEmpty(r.ConfigHash, rotatedConfigHash); hash != "" {
		// The attempt pins one generation, so the configuration observed at the
		// end is the one observed at the start. The worker replaces these with
		// the pinned loadout's hash when one is available.
		res.Summary.ConfigHashBefore = hash
		res.Summary.ConfigHashAfter = hash
	}
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
	//
	// The model transcript is built from the newest anchored checkpoint when
	// one exists: its summary stands in for the compacted prefix and only the
	// retained tail is decoded. The full log still drives every fact decision
	// (owedTurn, generation identity, unread inputs) and remains readable by
	// seq; the checkpoint is a projection, never a replacement for the log.
	view, checkpoint, checkpointSeq := r.modelView(ctx, lease.Scope, events)
	msgs, seqs := DecodeMessages(view, func(seq int64, kind string, err error) {
		r.Logger.Warn("skipping unreadable event", "session_id", lease.Scope.SessionID,
			"seq", seq, "kind", kind, "err", err)
	})
	if checkpoint != nil {
		msgs = append(agentcore.MessageList{*checkpoint}, msgs...)
		seqs = append([]int64{checkpointSeq}, seqs...)
	}
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
	// A forked session inherits its parent's transcript prefix as a read-time
	// union: the parent log stays the only storage of those facts, and the
	// prefix carries negative synthetic seqs so it can never be mistaken for a
	// child input. It is prepended after the child's own checkpoint summary
	// because a child compaction summarizes only the child's log.
	if prefixMsgs, prefixSeqs, err := r.forkPrefixMessages(ctx, lease.Scope); err != nil {
		return res, err
	} else if len(prefixMsgs) > 0 {
		msgs = append(prefixMsgs, msgs...)
		seqs = append(prefixSeqs, seqs...)
	}

	// The execution identity of this attempt is resolved from the run's
	// selection fact before anything is committed: a selection that cannot be
	// executed fails the attempt here, where no tool call has been dispatched
	// and no event written, so the retry (or the operator's fix) starts clean.
	//
	// The binding resolves first because it may supply the default model an
	// input without a model falls back to (selection > agent default > tenant
	// default); the identity resolution then runs on the merged fact.
	selection := runSelection(events)
	surface, err := r.resolveSurface(ctx, lease, selection)
	if err != nil {
		return res, fmt.Errorf("agent: resolve agent binding: %w", err)
	}
	// The fact that resolved the attempt is also the fact tools see: an
	// out-of-band model call during this attempt (MCP sampling) must run on the
	// same model as the attempt itself.
	selectionForModel := MergeSelectionDefaultModel(selection, surface.DefaultModel)
	model, err := r.activeModelFor(ctx, lease, events, selectionForModel)
	if err != nil {
		return res, fmt.Errorf("agent: resolve model: %w", err)
	}
	// The attempt's real identity, wherever it is observed: the span replaces
	// the boot defaults the mixer opened it with, and the summary carries it
	// into the run.finished event.
	if span != nil {
		span.SetAttributes(telemetry.SpanAttributes{"provider": model.ProviderID, "model": model.Model})
	}
	res.Summary.Provider = model.ProviderID
	res.Summary.Model = model.Model
	res.Summary.AgentID = surface.AgentID
	res.Summary.TeamID = surface.TeamID
	res.Summary.TeamRevision = surface.TeamRevision
	res.Summary.TeamHash = surface.TeamHash
	tools, err := r.toolsForRun(surface, lease.Scope.SessionID.String())
	if err != nil {
		return res, err
	}
	// The attempt's injected tool surface, materialized once. This exact slice
	// goes to the loop and is hashed for the run snapshot: a host refresh that
	// adds an MCP server or a plugin tool after this point changes the next
	// attempt's manifest, never this one's. Nil tools means an empty surface
	// (the manifest hash of no tools), not "unknown".
	var injectedTools []agentcore.AgentTool
	if tools != nil {
		injectedTools = tools.List()
	}
	manifestHash, err := agenttool.ManifestHash(injectedTools)
	if err != nil {
		return res, fmt.Errorf("agent: tool manifest hash: %w", err)
	}
	res.Summary.ToolManifestHash = manifestHash
	// A model that cannot run the request must be refused before anything is
	// committed (the same rule the identity resolution follows): the tool list is
	// part of the request, so a model whose configuration denies tool calls may not
	// be handed any.
	if props := model.Properties; props != nil && !props.SupportsToolCall {
		if len(injectedTools) > 0 {
			return res, fmt.Errorf("agent: model %s/%s declares no tool-call support but %d tools are available",
				model.ProviderID, model.Model, len(injectedTools))
		}
	}

	// The tool-execution context carries the attempt's (tenant, session) scope:
	// a tool that receives out-of-band frames (MCP notifications/progress) keys
	// them on this scope plus the call id. The attempt id freezes the MCP
	// catalogue for the whole attempt (the next attempt gets a new id and sees
	// the republished catalogue). Both are transient, never persisted.
	runCtx := agentcore.WithToolRunScope(ctx, agentcore.ToolRunScope{
		TenantID:  lease.Scope.TenantID.String(),
		SessionID: lease.Scope.SessionID.String(),
	})
	runCtx = agentcore.WithMCPAttempt(runCtx, uuid.NewString())
	// Sampling executed by a tool of this attempt must resolve the same session
	// selection the attempt resolved. An app-initiated tool call (no runner
	// attempt) has no such key and keeps the deployment default.
	runCtx = agentcore.WithToolModelSelection(runCtx, selectionForModel)
	// A previous process may have died after committing an assistant tool call
	// but before committing its results. Execute the missing calls now (at least
	// once) so the model sees a complete turn — through THIS attempt's filtered
	// registry, never the host-wide one: a crash must not become a way to run a
	// tool the binding does not grant (a removed grant, or a captain that
	// explicitly disabled delegation, would otherwise only hold until the next
	// restart).
	msgs, seqs, cursor, err = r.fillMissingToolResults(runCtx, lease, msgs, seqs, cursor, tools)
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
	msgs, seqs, movedStart := presentUnreadInputLast(msgs, seqs, func(seq int64) bool {
		if consumptionKnown {
			return unread[seq]
		}
		// A read watermark of zero means no attempt has ever proven a read
		// point: the log order is the only information available and is left
		// alone.
		return lease.ReadWatermark > 0 && seq > lease.ReadWatermark
	})
	// The transcript length the loop starts from, before it appends produced
	// messages (and before it inserts an in-memory system declaration). A
	// checkpoint cut at or above movedStart and below this length lands in the
	// moved block, where the in-memory index can no longer be resolved against
	// log-order seqs.
	initialLen := len(msgs)

	// Session context (MCP app context) is merged into the attempt's last user
	// message, in memory only. It is context for this attempt, not a committed
	// fact: no event kind is added and a later attempt re-reads the store.
	if r.SessionContexts != nil {
		msgs = prependSessionContext(msgs, r.SessionContexts(ctx, lease.Scope))
	}

	// The inputs this attempt actually presents to the model. The assistant
	// message records them (payload.inputSeqs) so "unanswered input" is a fact
	// in the log rather than a watermark heuristic: a successor can tell a
	// queue input from a guide input, and a crash between the assistant commit
	// and Finish cannot duplicate an answer. The two slices move in lockstep
	// (presentUnreadInputLast returns them unchanged on a length mismatch).
	inputSeqs := make([]int64, 0, len(msgs))
	if len(seqs) == len(msgs) {
		for i, m := range msgs {
			// seq <= 0 marks history that is not a committed child input: the
			// fork prefix (negative) and an in-process rebuilt tool result (0).
			if m.Role() == agentcore.RoleUser && seqs[i] > 0 {
				inputSeqs = append(inputSeqs, seqs[i])
			}
		}
	}

	// Outbound image resolution: the transcript stores references (blobId), the
	// provider wire wants bytes, a URL or a file handle. This is the boundary
	// crossing, and it happens per attempt because the right form depends on the
	// provider chosen for this attempt and on handle state (a Gemini handle past
	// its 48h window is re-uploaded here). Nothing resolved enters the log: only
	// this attempt's in-memory view changes.
	msgs, imageReport := r.resolveImages(ctx, msgs, imageResolution{
		resolver:  r.Images,
		workspace: r.imageWorkspace(lease),
		target: provider.ImageTarget{
			Provider: model.ProviderID,
			BaseURL:  model.Endpoint,
			APIKey:   model.APIKey,
		},
		caps: imageCapsFor(model),
	})
	if !imageReport.empty() {
		logImageResolution(r.Logger, lease, model, imageReport)
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
	// maxTurnsHit is written by the loop goroutine (ShouldStopAfterTurn) and read
	// here after the stream closed: an atomic is the cheapest correct handoff, and
	// the run-level stop reason is labelled on this side so the two goroutines
	// never share a plain field.
	var maxTurnsHit atomic.Bool
	// answerCommitted records that this attempt committed a terminal answer, which
	// the cancellation guard reads to tell "the caller stopped an attempt that had
	// already answered" from "the attempt produced nothing".
	answerCommitted := false
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
			Model:         model.Model,
			Provider:      model.ProviderID,
			APIKey:        model.APIKey,
			Stream:        model.Stream,
			ThinkingLevel: model.ThinkingLevel,
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
			// supplies settings; the loop reports a successful compaction on
			// CompactionEvent, which the consumer persists below.
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
				maxTurnsHit.Store(true)
				return true
			}
			return false
		},
	}
	if r.Compaction != nil && r.Compaction.Settings.Enabled {
		// The model's own budget is the primary source (a 1M-context model must
		// not be compacted at the tenant's conservative default); the tenant
		// setting remains the fallback for models that declare nothing.
		window := model.ContextWindow
		if window <= 0 {
			window = r.Compaction.ContextWindow
		}
		if window > 0 {
			cfg.ContextWindow = window
			cfg.Compaction = r.Compaction.Settings
		}
	}
	// The request overlay of the resolved model travels to the provider as
	// opaque options; nothing in the kernel interprets it.
	if len(model.RequestPatch) > 0 {
		cfg.Extra = model.RequestPatch
	}
	// The prompt that actually goes on the wire: the host renders its dynamic
	// sections (MCP, plugin, skills) around the binding's preamble, so the digest
	// must be taken here, not over the binding text. A reader of run.finished then
	// sees which prompt answered the attempt — including a host-side section
	// change that leaves the agent's own text untouched.
	systemPrompt := r.systemPromptFor(surface)
	if surface.PromptHash == "" && strings.TrimSpace(surface.Prompt) != "" {
		// No host renderer contributed sections: the wire prompt IS the binding
		// text, so its digest is the binding's.
		res.Summary.PromptHash = digestPrompt(surface.Prompt)
	} else if strings.TrimSpace(surface.Prompt) != "" || strings.TrimSpace(systemPrompt) != "" {
		res.Summary.PromptHash = digestPrompt(systemPrompt)
	}
	agentCtx := &agentcore.AgentContext{
		SystemPrompt: systemPrompt,
		Messages:     msgs,
		Tools:        injectedTools,
	}
	stream := loop.StartRun(runCtx, agentCtx, cfg)
	for ev := range stream.Events() {
		if ctx.Err() != nil {
			res.Cursor = cursor
			summary.StopReason = agentcore.StopReasonAborted
			return res, ctx.Err()
		}
		switch e := ev.(type) {
		case agentcore.TurnEndEvent:
			summary.Turns++
		case agentcore.CompactionEvent:
			// The loop compacted its in-memory context. The compaction event is
			// the durable fact (the summary and the kept boundary), and it is
			// committed here, in the ordered consumer, so it can never precede
			// the messages it summarizes: every MessageEnd/ToolExecutionEnd
			// emitted before it was committed above.
			if e.ErrorMessage != "" || strings.TrimSpace(e.Summary) == "" {
				// A failed compaction is observable on the event stream only; it
				// has no fact to persist (the context was left unchanged).
				continue
			}
			raw, err := marshalPayload(map[string]any{
				"summary":             e.Summary,
				"tokensBefore":        e.TokensBefore,
				"firstKept":           e.FirstKeptIndex,
				"firstKeptTranscript": e.FirstKeptTranscriptIndex,
				"summarized":          e.SummarizedCount,
				"kept":                e.KeptCount,
				"timestamp":           time.Now().UnixMilli(),
			})
			if err != nil {
				r.Logger.Warn("persist compaction", "err", err)
				continue
			}
			seq, err := r.commitEvent(ctx, lease, kernel.CommitRequest{
				Kind:    kernel.KindCompaction,
				Payload: raw,
			})
			if err != nil {
				// The loop already compacted its in-memory context; failing the
				// attempt would replay the turn from the uncompacted log. Keep
				// the run moving and let the next turn try again.
				r.Logger.Warn("persist compaction", "err", err)
				continue
			}
			cursor = seq
			if r.Checkpoints == nil {
				continue
			}
			// The checkpoint is a projection of the event just committed. Its
			// save is best-effort by contract: a failure is logged and never
			// rolls the event back, and replay falls back to the full log.
			//
			// The cut index is in in-memory order, but checkpointAnchor resolves
			// it against log-order seqs. Two situations make that mapping
			// unusable: presentUnreadInputLast moved unread inputs to the tail
			// (a cut in the moved block would resolve to a later seq and drop the
			// input), and the loop injected guide messages at a turn boundary
			// (their log seq order can differ from injection order). In both
			// cases the sample is "replay the full log" (kept_from_seq NULL)
			// instead of guessing a boundary: duplication is recoverable, a lost
			// input is not.
			anchor := int64(0)
			switch {
			case feed.handedAny():
				r.Logger.Debug("checkpoint left unanchored: guide input injected", "session_id", lease.Scope.SessionID, "seq", seq)
			case movedStart < initialLen && e.FirstKeptTranscriptIndex >= movedStart && e.FirstKeptTranscriptIndex < initialLen:
				r.Logger.Debug("checkpoint left unanchored: cut in moved unread inputs", "session_id", lease.Scope.SessionID,
					"seq", seq, "first_kept", e.FirstKeptTranscriptIndex, "moved_start", movedStart, "initial_len", initialLen)
			default:
				anchor = r.checkpointAnchor(ctx, lease.Scope, e.FirstKeptTranscriptIndex, seq)
			}
			if err := r.Checkpoints.SaveCheckpoint(ctx, lease.Scope, seq, anchor, e.Summary); err != nil {
				r.Logger.Warn("save checkpoint", "session_id", lease.Scope.SessionID, "seq", seq, "err", err)
			}
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
				if assistant.StopReason == agentcore.StopReasonEndTurn {
					// A terminal answer is in the log: the attempt's work is done,
					// whatever the caller decides afterwards.
					answerCommitted = true
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
	// A cancel that lands while the loop is settling is invisible to the per-event
	// check above: Emit honours the context, so the loop's remaining events are
	// dropped and this consumer sees a plain channel close. The attempt must not
	// settle as a clean finish in that case — the worker's terminal-outcome rule
	// reads the returned error, and "done" would claim an answer nobody produced.
	// The one honest exception is an attempt whose final answer is already
	// committed: that work is done, and reporting it cancelled would hide it.
	if ctxErr := ctx.Err(); ctxErr != nil && !answerCommitted {
		res.Cursor = cursor
		summary.StopReason = agentcore.StopReasonAborted
		return res, ctxErr
	}
	if maxTurnsHit.Load() {
		summary.StopReason = StopReasonMaxTurns
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
//
// Bookkeeping kinds are skipped when reading the log tail: a failed attempt
// commits its summary and settles without ever producing an answer, so
// "run.finished is the newest event" says nothing about whether the input was
// answered. Reading the tail literally would strand the input of every
// deployment whose very first attempt fails before committing anything (an
// unrunnable selection, a missing credential): the watermark that attempt
// recorded covers the input, so the retry would settle done with no answer at
// all.
func legacyOwedTurn(events []store.Event, readWatermark int64) bool {
	if readWatermark > 0 {
		for _, ev := range events {
			if ev.Kind == store.EventUserMessage && ev.Seq > readWatermark {
				return true
			}
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Kind {
		case store.EventUserMessage:
			return true
		case store.EventAssistantMessage, store.EventToolResult:
			return newestAssistantHasToolCalls(events)
		default:
			// Every non-transcript kind is bookkeeping (run.*, task.*, fs.*,
			// config.*, placement.*, supervision.*, guardrail.*, resume.attempt,
			// notification.*). It must not hide an owed input: a resume.attempt
			// committed after the user.message is exactly the W8 case where the
			// old "break on anything unknown" rule settled a resumed run without
			// ever calling the model.
			continue
		}
	}
	return newestAssistantHasToolCalls(events)
}

// fillMissingToolResults executes tool calls that a crashed process left
// unresolved. The call is at-least-once; tools are expected to be idempotent or
// to carry their own idempotency keys.
//
// The registry is the attempt's filtered surface (toolsForRun), not the host
// one: recovery replays work the same agent asked for under the same grants, so
// a call outside the surface must fail as an unknown tool rather than execute.
func (r *Runner) fillMissingToolResults(ctx context.Context, lease kernel.Lease, msgs agentcore.MessageList, seqs []int64, cursor int64, tools *agenttool.ToolRegistry) (agentcore.MessageList, []int64, int64, error) {
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
		ToolExecutorConfig: agenttool.ToolExecutorConfig{Registry: tools},
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
// The seq slice moves in lockstep with the messages: the assistant message's
// inputSeqs consumption fact and a checkpoint anchor both pair the two by index,
// and a reorder that left seqs behind would attribute the wrong committed input
// to the reordered message.
//
// The returned movedStart is the index where the moved block begins (== len of
// the transcript when nothing moved). A checkpoint index that lands in that
// block cannot be resolved against the log-order seqs (the moved message's seq
// is older than the message occupying its in-memory position), so the caller
// must treat it as unanchorable rather than guess.
//
// unread is the caller's rule: the inputSeqs consumption fact for new logs, the
// read watermark for legacy ones.
func presentUnreadInputLast(msgs agentcore.MessageList, seqs []int64, unread func(seq int64) bool) (agentcore.MessageList, []int64, int) {
	if len(msgs) == 0 || len(seqs) != len(msgs) {
		return msgs, seqs, len(msgs)
	}
	kept := make(agentcore.MessageList, 0, len(msgs))
	keptSeqs := make([]int64, 0, len(seqs))
	moved := make(agentcore.MessageList, 0, len(msgs))
	movedSeqs := make([]int64, 0, len(seqs))
	for i, m := range msgs {
		// seq 0 marks a message rebuilt in-process (a re-executed tool result),
		// never a committed user input.
		if m.Role() == agentcore.RoleUser && seqs[i] > 0 && unread(seqs[i]) {
			moved = append(moved, m)
			movedSeqs = append(movedSeqs, seqs[i])
			continue
		}
		kept = append(kept, m)
		keptSeqs = append(keptSeqs, seqs[i])
	}
	if len(moved) == 0 {
		return msgs, seqs, len(msgs)
	}
	return append(kept, moved...), append(keptSeqs, movedSeqs...), len(kept)
}

// prependSessionContext merges this attempt's extra context blocks into the last
// user message's content, in memory. The message is copied, never mutated, so
// the transcript value read from the log stays untouched. A transcript with no
// user message gets one trailing synthetic message: losing the context would be
// worse than an extra turn of context.
func prependSessionContext(msgs agentcore.MessageList, contents []agentcore.Content) agentcore.MessageList {
	if len(contents) == 0 {
		return msgs
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role() != agentcore.RoleUser {
			continue
		}
		user, ok := msgs[i].(agentcore.UserMessage)
		if !ok {
			return msgs
		}
		merged := make(agentcore.ContentList, 0, len(contents)+len(user.Content))
		merged = append(merged, contents...)
		merged = append(merged, user.Content...)
		user.Content = merged
		out := append(agentcore.MessageList(nil), msgs...)
		out[i] = user
		return out
	}
	contextOnly := agentcore.UserMessage{
		RoleField: agentcore.RoleUser,
		Content:   append(agentcore.ContentList(nil), contents...),
		Timestamp: time.Now().UnixMilli(),
	}
	return append(msgs, contextOnly)
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

// modelView returns the events that build the model transcript. With an
// anchored checkpoint it is the checkpoint summary (reconstructed below)
// followed by the events from kept_from_seq; without one it is the whole log.
// The full log remains the only source of truth for fact decisions and stays
// readable by seq.
//
// An anchored checkpoint is required: kept_from_seq == 0 means the host has not
// tied the retained tail to committed events, and truncating on a guessed
// boundary could drop context. The fallback is the full log, which is always
// correct.
func (r *Runner) modelView(ctx context.Context, scope kernel.Scope, events []store.Event) ([]store.Event, *agentcore.CompactionMessage, int64) {
	if r.Checkpoints == nil {
		return events, nil, 0
	}
	cp, found, err := r.Checkpoints.LoadCheckpoint(ctx, scope)
	if err != nil {
		// The checkpoint is a projection: a read failure must not stop the run.
		r.Logger.Warn("load checkpoint", "session_id", scope.SessionID, "err", err)
		return events, nil, 0
	}
	if !found || cp.KeptFromSeq <= 0 {
		return events, nil, 0
	}
	trimmed := make([]store.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq < cp.KeptFromSeq {
			continue
		}
		// The checkpoint stands in for its own compaction event; replaying the
		// event too would duplicate the summary. Older compaction events inside
		// the retained range are covered by the same summary (the newest
		// summary folds them in), so they are skipped as well.
		if ev.Kind == store.EventCompaction && ev.Seq <= cp.UptoSeq {
			continue
		}
		trimmed = append(trimmed, ev)
	}
	msg := checkpointMessage(cp, events)
	return trimmed, &msg, cp.UptoSeq
}

// checkpointMessage rebuilds the CompactionMessage a checkpoint stands for,
// reading the fields the checkpoint row does not store (tokens-before and
// timestamp) from the committed compaction event. The summary is the
// checkpoint's own text: it is the projection the row was saved from.
func checkpointMessage(cp kernel.Checkpoint, events []store.Event) agentcore.CompactionMessage {
	msg := agentcore.CompactionMessage{
		RoleField: agentcore.RoleCompaction,
		Summary:   cp.Summary,
	}
	for _, ev := range events {
		if ev.Seq != cp.UptoSeq || ev.Kind != store.EventCompaction {
			continue
		}
		var p struct {
			TokensBefore int   `json:"tokensBefore"`
			Timestamp    int64 `json:"timestamp"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil {
			msg.TokensBefore = p.TokensBefore
			msg.Timestamp = p.Timestamp
		}
		break
	}
	return msg
}

// checkpointAnchor resolves the seq of the first event retained by a
// compaction. It indexes the pre-compaction model view (the newest previous
// checkpoint plus committed events before uptoSeq), which both the checkpoint
// row and the compaction event's firstKept index refer to. The event-derived
// view can only be shorter than the loop's in-memory list (uncommitted
// messages), and extra unpolled inputs sit later in the log; indexing it
// therefore resolves to an earlier-or-equal boundary, never a later one that
// would drop retained messages. Zero means "not anchored": the checkpoint is
// still written, and replay falls back to the full log.
func (r *Runner) checkpointAnchor(ctx context.Context, scope kernel.Scope, firstKeptIndex int, uptoSeq int64) int64 {
	if firstKeptIndex <= 0 || uptoSeq <= 0 {
		return 0
	}
	events, _, err := r.loadEvents(ctx, scope)
	if err != nil {
		r.Logger.Warn("resolve checkpoint anchor", "session_id", scope.SessionID, "err", err)
		return 0
	}
	before := make([]store.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq >= uptoSeq {
			continue
		}
		before = append(before, ev)
	}
	view, checkpoint, checkpointSeq := r.modelView(ctx, scope, before)
	_, seqs := DecodeMessages(view, nil)
	if checkpoint != nil {
		// The loop's pre-compaction view starts with the previous checkpoint
		// message; keep the index mapping aligned with it.
		seqs = append([]int64{checkpointSeq}, seqs...)
	}
	if firstKeptIndex >= len(seqs) {
		return 0
	}
	anchor := seqs[firstKeptIndex]
	if anchor <= 0 || anchor >= uptoSeq {
		return 0
	}
	return anchor
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
