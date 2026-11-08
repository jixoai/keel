package agentext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/kernel"
)

// Sub-agent primitive. A sub-agent is an independent session (same tenant,
// its own run) started through the same edge commit seam as any other turn.
// The parent receives a structured result; the sub-agent's failure can never
// take the parent's turn down (it is a value, not an exception).
var (
	ErrParentSessionNotFound  = errors.New("agentext: parent session not found")
	ErrSubagentBudgetExceeded = errors.New("agentext: sub-agent budget exhausted")
)

// Budget bounds one sub-agent execution. Zero means "inherit the runner
// default": no turn/token cap and the default timeout.
type Budget struct {
	MaxTurns  int   `json:"maxTurns,omitempty"`
	MaxTokens int   `json:"maxTokens,omitempty"`
	TimeoutMs int64 `json:"timeoutMs,omitempty"`
}

func (b Budget) timeout(def time.Duration) time.Duration {
	if b.TimeoutMs <= 0 {
		return def
	}
	return time.Duration(b.TimeoutMs) * time.Millisecond
}

// SpawnRequest asks for one sub-agent run.
type SpawnRequest struct {
	TenantID        uuid.UUID `json:"-"`
	ParentSessionID uuid.UUID `json:"-"`
	Task            string    `json:"task"`
	Agent           string    `json:"agent,omitempty"`
	// IdempotencyKey makes the derived child session deterministic: the same
	// key in the same parent always targets the same child session, so a retry
	// reuses the committed input instead of spawning a second agent.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Budget         Budget `json:"budget,omitempty"`
}

// Artifact is one provenance entry of a sub-agent run (for now: the tool calls
// it executed). It never carries tool output text: the summary is the value,
// artifacts are the trail.
type Artifact struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
}

// Usage is the objective accounting of one sub-agent run.
type Usage struct {
	Turns        int   `json:"turns"`
	InputTokens  int   `json:"inputTokens"`
	OutputTokens int   `json:"outputTokens"`
	DurationMs   int64 `json:"durationMs"`
}

// SubagentResult is the structured value the parent receives.
type SubagentResult struct {
	Status          string     `json:"status"`
	Summary         string     `json:"summary"`
	Artifacts       []Artifact `json:"artifacts,omitempty"`
	Usage           Usage      `json:"usage"`
	SessionID       string     `json:"sessionId"`
	ParentSessionID string     `json:"parentSessionId,omitempty"`
	Agent           string     `json:"agent,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// SubagentRecord is the observable registry entry of one spawned sub-agent.
type SubagentRecord struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenantId"`
	ParentSessionID string          `json:"parentSessionId"`
	SessionID       string          `json:"sessionId"`
	Task            string          `json:"task"`
	Agent           string          `json:"agent,omitempty"`
	Status          string          `json:"status"`
	StartedAt       time.Time       `json:"startedAt"`
	EndedAt         *time.Time      `json:"endedAt,omitempty"`
	Result          *SubagentResult `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// SubagentOptions configures the runner.
type SubagentOptions struct {
	Gate    SessionGate
	Edge    kernel.Committer
	Events  kernel.Reader
	Runs    RunStatusReader
	Control *RunControl
	Hooks   *HookBus
	Logger  *slog.Logger
	// Poll bounds how often the run row is read while waiting (default 250ms).
	Poll time.Duration
	// DefaultTimeout bounds a wait when the request carries no timeout
	// (default 10 minutes).
	DefaultTimeout time.Duration
	Now            func() time.Time
}

const (
	defaultSubagentPoll    = 250 * time.Millisecond
	defaultSubagentTimeout = 10 * time.Minute
	maxSubagentRecords     = 512
)

// SubagentRunner spawns and observes sub-agent sessions.
type SubagentRunner struct {
	gate           SessionGate
	starter        *TurnStarter
	events         kernel.Reader
	runs           RunStatusReader
	control        *RunControl
	hooks          *HookBus
	log            *slog.Logger
	poll           time.Duration
	defaultTimeout time.Duration
	now            func() time.Time

	mu      sync.Mutex
	records map[string]*SubagentRecord
	order   []string
}

// NewSubagentRunner validates the inputs.
func NewSubagentRunner(opts SubagentOptions) *SubagentRunner {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Poll <= 0 {
		opts.Poll = defaultSubagentPoll
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = defaultSubagentTimeout
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	r := &SubagentRunner{
		gate:           opts.Gate,
		events:         opts.Events,
		runs:           opts.Runs,
		control:        opts.Control,
		hooks:          opts.Hooks,
		log:            opts.Logger,
		poll:           opts.Poll,
		defaultTimeout: opts.DefaultTimeout,
		now:            opts.Now,
		records:        make(map[string]*SubagentRecord),
	}
	if opts.Gate != nil && opts.Edge != nil {
		r.starter = NewTurnStarter(opts.Gate, opts.Edge, opts.Hooks, opts.Logger)
	}
	return r
}

// SpawnAsync starts a sub-agent and returns immediately. The child run is
// owned by the worker; call Wait to collect the structured result.
func (r *SubagentRunner) SpawnAsync(ctx context.Context, req SpawnRequest) (SubagentRecord, error) {
	if r.starter == nil || r.runs == nil {
		return SubagentRecord{}, errors.New("agentext: sub-agent runner needs the session gate, the kernel edge and the run reader")
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return SubagentRecord{}, errors.New("agentext: tenant and parent session are required")
	}
	req.Task = strings.TrimSpace(req.Task)
	if req.Task == "" {
		return SubagentRecord{}, errors.New("agentext: task is required")
	}
	if _, exists, err := r.gate.SessionOwner(ctx, req.TenantID, req.ParentSessionID); err != nil {
		return SubagentRecord{}, err
	} else if !exists {
		return SubagentRecord{}, ErrParentSessionNotFound
	}

	childID := uuid.New()
	key := strings.TrimSpace(req.IdempotencyKey)
	requestID := uuid.NewString()
	if key != "" {
		childID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.subagent.v1|"+
			req.TenantID.String()+"|"+req.ParentSessionID.String()+"|"+key))
		requestID = "subagent:" + key
	}
	extras := map[string]any{
		"subagent": map[string]any{
			"parentSessionId": req.ParentSessionID.String(),
			"agent":           req.Agent,
			"idempotencyKey":  key,
			"budget":          req.Budget,
		},
	}
	start, err := r.starter.StartTurn(ctx, StartTurnRequest{
		TenantID:  req.TenantID,
		SessionID: childID,
		Text:      req.Task,
		RequestID: requestID,
		Extras:    extras,
	})
	if err != nil {
		return SubagentRecord{}, err
	}
	record := SubagentRecord{
		ID:              start.SessionID.String(),
		TenantID:        req.TenantID.String(),
		ParentSessionID: req.ParentSessionID.String(),
		SessionID:       start.SessionID.String(),
		Task:            req.Task,
		Agent:           req.Agent,
		Status:          "running",
		StartedAt:       r.now(),
	}
	r.store(record)
	r.emit(HookSubagentStarted, record, map[string]string{"seq": fmt.Sprintf("%d", start.Seq)})
	return record, nil
}

// Spawn starts a sub-agent and waits (bounded by the budget) for its structured
// result.
func (r *SubagentRunner) Spawn(ctx context.Context, req SpawnRequest) (SubagentResult, error) {
	record, err := r.SpawnAsync(ctx, req)
	if err != nil {
		return SubagentResult{}, err
	}
	tenantID, err := uuid.Parse(record.TenantID)
	if err != nil {
		return SubagentResult{}, err
	}
	sessionID, err := uuid.Parse(record.SessionID)
	if err != nil {
		return SubagentResult{}, err
	}
	return r.Wait(ctx, tenantID, sessionID, req.Budget, req.Agent)
}

// Wait polls the child's run row until it settles or the budget ends. It is
// safe to call after a restart: the child session and its record are recovered
// from the run row and the event log (the in-process registry is only the
// parent-link index).
func (r *SubagentRunner) Wait(ctx context.Context, tenantID, sessionID uuid.UUID, budget Budget, agent string) (SubagentResult, error) {
	if r.runs == nil {
		return SubagentResult{}, errors.New("agentext: sub-agent runner has no run reader")
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget.timeout(r.defaultTimeout))
	defer cancel()
	started := r.now()
	lastBudgetCheck := time.Time{}
	var lastState RunState
	for {
		state, err := r.runs.RunState(waitCtx, tenantID, sessionID)
		switch {
		case err == nil:
			lastState = state
		case errors.Is(err, ErrNoRun):
			// The edge commit queues the run in the same transaction, so this
			// is a short lag at most; keep waiting until the budget ends.
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "budget_exhausted", "wait cancelled before the sub-agent settled")
		default:
			return SubagentResult{}, err
		}

		if err == nil {
			if overlay, ok := r.controlStatus(tenantID, sessionID); ok && overlay == "cancelled" {
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "cancelled", "the sub-agent run was cancelled")
			}
			switch state.Status {
			case "done":
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "done", "")
			case "failed":
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "failed", state.LastError)
			}
		}

		if time.Since(lastBudgetCheck) >= time.Second && (budget.MaxTurns > 0 || budget.MaxTokens > 0) {
			lastBudgetCheck = time.Now()
			if reason, exceeded := r.budgetExceeded(waitCtx, tenantID, sessionID, budget); exceeded {
				r.cancelChild(tenantID, sessionID)
				return r.finish(waitCtx, tenantID, sessionID, agent, started, budget, "budget_exhausted", reason)
			}
		}

		select {
		case <-waitCtx.Done():
			if lastState.Status == "" {
				lastState.Status = "unknown"
			}
			r.cancelChild(tenantID, sessionID)
			return r.finish(ctx, tenantID, sessionID, agent, started, budget, "budget_exhausted",
				fmt.Sprintf("timed out waiting for the sub-agent (last run status %s)", lastState.Status))
		case <-time.After(r.poll):
		}
	}
}

func (r *SubagentRunner) controlStatus(tenantID, sessionID uuid.UUID) (string, bool) {
	if r.control == nil {
		return "", false
	}
	return r.control.Status(kernel.Scope{TenantID: tenantID, SessionID: sessionID})
}

func (r *SubagentRunner) budgetExceeded(ctx context.Context, tenantID, sessionID uuid.UUID, budget Budget) (string, bool) {
	events, err := r.readEvents(ctx, tenantID, sessionID)
	if err != nil {
		return "", false
	}
	turns, tokens := 0, 0
	for _, event := range events {
		if event.Kind != kernel.KindAssistantMessage {
			continue
		}
		turns++
		var msg struct {
			Usage *agentcore.Usage `json:"usage"`
		}
		if json.Unmarshal(event.Payload, &msg) == nil && msg.Usage != nil {
			tokens += msg.Usage.InputTokens + msg.Usage.OutputTokens
		}
	}
	if budget.MaxTurns > 0 && turns > budget.MaxTurns {
		return fmt.Sprintf("turn budget exceeded (%d > %d)", turns, budget.MaxTurns), true
	}
	if budget.MaxTokens > 0 && tokens > budget.MaxTokens {
		return fmt.Sprintf("token budget exceeded (%d > %d)", tokens, budget.MaxTokens), true
	}
	return "", false
}

func (r *SubagentRunner) cancelChild(tenantID, sessionID uuid.UUID) {
	if r.control == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelWait)
	defer cancel()
	if _, err := r.control.Cancel(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}); err != nil {
		r.log.Info("sub-agent cancel is not wired; the child run continues", "session_id", sessionID, "err", err)
	}
}

// finish reads the child's log, builds the structured result, updates the
// registry and emits subagent.finished.
func (r *SubagentRunner) finish(ctx context.Context, tenantID, sessionID uuid.UUID, agent string, started time.Time, _ Budget, status, failure string) (SubagentResult, error) {
	events, readErr := r.readEvents(ctx, tenantID, sessionID)
	if readErr != nil && !errors.Is(readErr, kernel.ErrNotFound) {
		r.log.Warn("read sub-agent log", "session_id", sessionID, "err", readErr)
	}
	summary, turns, tokens := summarizeEvents(events)
	result := SubagentResult{
		Status:          status,
		Summary:         summary,
		Artifacts:       artifactsFromEvents(events),
		Usage:           Usage{Turns: turns, InputTokens: tokens.Input, OutputTokens: tokens.Output, DurationMs: r.now().Sub(started).Milliseconds()},
		SessionID:       sessionID.String(),
		ParentSessionID: "",
		Agent:           agent,
	}
	if failure != "" {
		result.Error = failure
	}
	parentSessionID := ""
	if record := r.lookup(sessionID); record != nil {
		result.ParentSessionID = record.ParentSessionID
		parentSessionID = record.ParentSessionID
	}
	now := r.now()
	durationFrom := started
	r.update(sessionID, func(record *SubagentRecord) {
		record.Status = status
		record.EndedAt = &now
		record.Result = &result
		record.Error = failure
		if !record.StartedAt.IsZero() {
			durationFrom = record.StartedAt
		}
	})
	result.Usage.DurationMs = now.Sub(durationFrom).Milliseconds()
	payload := HookPayload{
		Event:             HookSubagentFinished,
		TenantID:          tenantID.String(),
		SessionID:         sessionID.String(),
		ParentSessionID:   parentSessionID,
		SubagentSessionID: sessionID.String(),
		Status:            status,
		At:                now,
	}
	payload.SetData("turns", fmt.Sprintf("%d", turns))
	payload.SetData("durationMs", fmt.Sprintf("%d", result.Usage.DurationMs))
	if r.hooks != nil {
		r.hooks.Emit(payload)
	}
	return result, nil
}

func (r *SubagentRunner) readEvents(ctx context.Context, tenantID, sessionID uuid.UUID) ([]kernel.Event, error) {
	if r.events == nil {
		return nil, nil
	}
	var all []kernel.Event
	var after int64
	for {
		page, err := r.events.LoadEvents(ctx, kernel.Scope{TenantID: tenantID, SessionID: sessionID}, after, 500)
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			return all, nil
		}
		after = page[len(page)-1].Seq
	}
}

func summarizeEvents(events []kernel.Event) (string, int, struct{ Input, Output int }) {
	var tokens struct{ Input, Output int }
	turns := 0
	summary := ""
	for _, event := range events {
		switch event.Kind {
		case kernel.KindAssistantMessage:
			turns++
			var msg agentcore.AssistantMessage
			if err := json.Unmarshal(event.Payload, &msg); err != nil {
				continue
			}
			if msg.Usage != nil {
				tokens.Input += msg.Usage.InputTokens
				tokens.Output += msg.Usage.OutputTokens
			}
			var text strings.Builder
			for _, content := range msg.Content {
				if block, ok := content.(agentcore.TextContent); ok {
					text.WriteString(block.Text)
				}
			}
			if text.Len() > 0 {
				summary = text.String()
			}
		}
	}
	return summary, turns, tokens
}

func artifactsFromEvents(events []kernel.Event) []Artifact {
	seen := make(map[string]struct{})
	var out []Artifact
	for _, event := range events {
		if event.Kind != kernel.KindToolResult {
			continue
		}
		var result struct {
			ToolCallID string `json:"toolCallId"`
			ToolName   string `json:"toolName"`
		}
		if err := json.Unmarshal(event.Payload, &result); err != nil || result.ToolCallID == "" {
			continue
		}
		if _, dup := seen[result.ToolCallID]; dup {
			continue
		}
		seen[result.ToolCallID] = struct{}{}
		out = append(out, Artifact{Type: "tool", Name: result.ToolName, Ref: result.ToolCallID})
	}
	return out
}

// List returns sub-agent records, newest first. A non-nil parent filters by
// parent session.
func (r *SubagentRunner) List(tenantID uuid.UUID, parent *uuid.UUID) []SubagentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SubagentRecord, 0, len(r.records))
	for _, record := range r.records {
		if tenantID != uuid.Nil && record.TenantID != tenantID.String() {
			continue
		}
		if parent != nil && record.ParentSessionID != parent.String() {
			continue
		}
		copied := *record
		if record.Result != nil {
			result := *record.Result
			copied.Result = &result
		}
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].SessionID > out[j].SessionID
	})
	return out
}

// ParentOf reports the parent session of a sub-agent session (the session-list
// endpoint uses it to link a child row to its parent while the registry is
// warm; the durable copy of the link lives in the child's first user.message
// payload).
func (r *SubagentRunner) ParentOf(sessionID uuid.UUID) (string, bool) {
	record := r.lookup(sessionID)
	if record == nil || record.ParentSessionID == "" {
		return "", false
	}
	return record.ParentSessionID, true
}

// Get returns one record by child session id.
func (r *SubagentRunner) Get(sessionID uuid.UUID) (SubagentRecord, bool) {
	record := r.lookup(sessionID)
	if record == nil {
		return SubagentRecord{}, false
	}
	return *record, true
}

func (r *SubagentRunner) lookup(sessionID uuid.UUID) *SubagentRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[sessionID.String()]
	if record == nil {
		return nil
	}
	copied := *record
	return &copied
}

func (r *SubagentRunner) store(record SubagentRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copied := record
	r.records[copied.SessionID] = &copied
	r.order = append(r.order, copied.SessionID)
	// Bound the registry: drop the oldest terminal records first.
	for len(r.order) > maxSubagentRecords {
		dropID := r.order[0]
		r.order = r.order[1:]
		if existing := r.records[dropID]; existing != nil && existing.Status == "running" {
			// Never drop a live entry; keep it and re-append the id so the
			// bound applies to settled records only.
			r.order = append(r.order, dropID)
			break
		}
		delete(r.records, dropID)
	}
}

func (r *SubagentRunner) update(sessionID uuid.UUID, mutate func(*SubagentRecord)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[sessionID.String()]
	if record == nil {
		return
	}
	mutate(record)
}

func (r *SubagentRunner) emit(event HookEvent, record SubagentRecord, data map[string]string) {
	if r.hooks == nil {
		return
	}
	payload := HookPayload{
		Event:             event,
		TenantID:          record.TenantID,
		SessionID:         record.SessionID,
		ParentSessionID:   record.ParentSessionID,
		SubagentSessionID: record.SessionID,
		Status:            record.Status,
		At:                r.now(),
	}
	if record.Agent != "" {
		payload.SetData("agent", record.Agent)
	}
	for key, value := range data {
		payload.SetData(key, value)
	}
	r.hooks.Emit(payload)
}

// --- spawn_subagent tool -------------------------------------------------

type runScopeKey struct{}

// RunScope is the ambient run identity a tool needs to attribute a sub-agent to
// its parent. The kernel runner does not put it in the tool context yet, so the
// tool answers with an actionable error until that one-line wiring exists.
type RunScope struct {
	TenantID  uuid.UUID
	SessionID uuid.UUID
	RunID     string
}

// WithRunScope attaches the run identity to a tool execution context.
func WithRunScope(ctx context.Context, scope RunScope) context.Context {
	return context.WithValue(ctx, runScopeKey{}, scope)
}

// RunScopeFromContext reads the ambient run identity.
func RunScopeFromContext(ctx context.Context) (RunScope, bool) {
	scope, ok := ctx.Value(runScopeKey{}).(RunScope)
	return scope, ok
}

// SubagentTool is the model-facing `spawn_subagent` primitive. It is exported
// rather than registered by default because it can only execute when the runner
// supplies the ambient run scope (WithRunScope); registering a tool that always
// fails would pollute every model prompt.
type SubagentTool struct {
	Runner *SubagentRunner
}

// Name implements agentcore.AgentTool.
func (t *SubagentTool) Name() string { return "spawn_subagent" }

// Description implements agentcore.AgentTool.
func (t *SubagentTool) Description() string {
	return "Spawn a sub-agent in an independent session and return its structured result (status, summary, usage)."
}

// Schema implements agentcore.AgentTool.
func (t *SubagentTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"task":{"type":"string","description":"What the sub-agent must do."},` +
		`"agent":{"type":"string","description":"Optional agent/profile name."},` +
		`"maxTurns":{"type":"integer","minimum":0},` +
		`"maxTokens":{"type":"integer","minimum":0},` +
		`"timeoutMs":{"type":"integer","minimum":0}` +
		`},"required":["task"],"additionalProperties":false}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *SubagentTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *SubagentTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || t.Runner == nil {
		return errorToolResult("spawn_subagent needs the ambient run scope: wire agentext.WithRunScope(ctx, agentext.RunScope{...}) in the run executor"), nil
	}
	var input struct {
		Task      string `json:"task"`
		Agent     string `json:"agent"`
		MaxTurns  int    `json:"maxTurns"`
		MaxTokens int    `json:"maxTokens"`
		TimeoutMs int64  `json:"timeoutMs"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return errorToolResult("invalid arguments: " + err.Error()), nil
	}
	result, err := t.Runner.Spawn(ctx, SpawnRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		Task:            input.Task,
		Agent:           input.Agent,
		Budget:          Budget{MaxTurns: input.MaxTurns, MaxTokens: input.MaxTokens, TimeoutMs: input.TimeoutMs},
	})
	if err != nil {
		return errorToolResult("spawn_subagent failed: " + err.Error()), nil
	}
	raw, _ := json.Marshal(result)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: result,
	}, nil
}

// ErrorToolResult is exported for plugin authors: a tool failure is a result
// the model can read and react to, not a Go error that aborts the batch.
func ErrorToolResult(message string) agentcore.AgentToolResult { return errorToolResult(message) }

func errorToolResult(message string) agentcore.AgentToolResult {
	isError := true
	return agentcore.AgentToolResult{
		Content: agentcore.ContentList{agentcore.NewTextContent(message)},
		IsError: &isError,
	}
}
