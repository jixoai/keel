package harnessext

import (
	"bytes"
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
	"github.com/jixoai/keel/internal/sessionfork"
)

// Sub-agent primitive. A sub-agent is an independent session (same tenant,
// its own run) started through the same edge commit seam as any other turn.
// The parent receives a structured result; the sub-agent's failure can never
// take the parent's turn down (it is a value, not an exception).
var (
	ErrParentSessionNotFound  = errors.New("harnessext: parent session not found")
	ErrSubagentBudgetExceeded = errors.New("harnessext: sub-agent budget exhausted")
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
	// BotID is the Bot the child session runs as. It travels into the child's
	// input as selection.botId, so the child attempt resolves that Bot's prompt,
	// tool grants and default model exactly like any other bound run. Agent
	// stays the free-form label the record and hooks carry.
	BotID string `json:"botId,omitempty"`
	// Selection, when set, is a complete selection fact for the child input
	// (model identity and/or execution binding). It takes precedence over
	// BotID, which the runner folds into it.
	Selection json.RawMessage `json:"selection,omitempty"`
	// IdempotencyKey makes the derived child session deterministic: the same
	// key in the same parent always targets the same child session, so a retry
	// reuses the committed input instead of spawning a second agent.
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	Budget         Budget `json:"budget,omitempty"`
}

// childSelection derives the child input's selection fact from the request. It
// is a fact like any other: the child attempt resolves its Agent binding from
// it, and an empty result stays absent (the child then runs the deployment
// default).
func (r SpawnRequest) childSelection() json.RawMessage {
	if raw := bytes.TrimSpace(r.Selection); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		return append(json.RawMessage(nil), raw...)
	}
	botID := strings.TrimSpace(r.BotID)
	if botID == "" {
		return nil
	}
	encoded, err := json.Marshal(map[string]string{"botId": botID})
	if err != nil {
		return nil
	}
	return encoded
}

// spawnedBotID is the member Bot a spawn delegates to. SpawnFromTool carries
// the top-level botId; a caller that wrote only selection.botId still gets the
// member ownership, while a team selection (teamBotId) keeps the parent's
// captain ownership because the team decides the captain.
func (r *SubagentRunner) spawnedBotID(req SpawnRequest) string {
	if botID := strings.TrimSpace(req.BotID); botID != "" {
		return botID
	}
	raw := bytes.TrimSpace(req.Selection)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var fact struct {
		BotID     string `json:"botId"`
		TeamBotID string `json:"teamBotId"`
	}
	if json.Unmarshal(raw, &fact) != nil {
		return ""
	}
	if strings.TrimSpace(fact.TeamBotID) != "" {
		return ""
	}
	return strings.TrimSpace(fact.BotID)
}

// Artifact is one provenance entry of a sub-agent run. It never carries tool
// output text: the summary is the value, artifacts are the trail. A blob/file
// artifact carries the content-addressed reference only (never the bytes):
// tool outputs land as blob ids, file writes as layerfs hashes and captures as
// a tree hash.
type Artifact struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
	Hash string `json:"hash,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// artifactLimit bounds the artifact trail a parent receives. A long child run
// can write thousands of files; the summary plus a bounded reference list is
// the contract, and the full trail stays readable in the child's ledger.
const artifactLimit = 256

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
	// Origin / OriginSeq / ParentTaskID make a fork result self-describing: the
	// parent can see which ledger position the child branched from and which
	// card it hangs under without a second status call.
	Origin           string `json:"origin,omitempty"`
	OriginSeq        int64  `json:"originSeq,omitempty"`
	ParentTaskID     string `json:"parentTaskId,omitempty"`
	ConfigHash       string `json:"configHash,omitempty"`
	ToolManifestHash string `json:"toolManifestHash,omitempty"`
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
	// Origin is "spawn" or "fork"; OriginSeq is the parent ledger position a
	// fork started at (0 for a spawn).
	Origin    string `json:"origin,omitempty"`
	OriginSeq int64  `json:"originSeq,omitempty"`
	// ParentTaskID is the card the child card points at (set by the fork or
	// subagent link).
	ParentTaskID string `json:"parentTaskId,omitempty"`
	// ConfigHash / ToolManifestHash freeze the parent attempt's run snapshot
	// values at the fork origin (empty for a spawn, which has no parent run
	// snapshot to inherit).
	ConfigHash       string `json:"configHash,omitempty"`
	ToolManifestHash string `json:"toolManifestHash,omitempty"`
	// StartSeq is the child's initial input sequence (the resume point a parent
	// can name).
	StartSeq int64 `json:"startSeq,omitempty"`
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
	// ValidateSelection is the same submit-time gate the session input edge
	// runs (the host binds it to the current model generation's
	// ValidateSelection). Spawn validates the child selection before the child
	// session or its user.message exists, so a selection the next attempt would
	// deny is refused at the spawn call instead of failing every attempt.
	// Nil is fail-closed: a selection is refused when no validator is wired.
	ValidateSelection func(json.RawMessage) error
	// Bindings projects the child's selection onto the session binding index, so
	// a live child session blocks its Agent's delete/archive exactly like a
	// session started through POST /input. Nil keeps the pre-projection behavior.
	Bindings SessionBinder
	// Forks records the durable fork origin (the store implements it). Fork
	// refuses without it: a fork whose file/ledger prefix is not durable is a
	// broken session, not a best-effort one.
	Forks sessionfork.Store
	// Snapshots resolves the parent's file-view tree at a fork origin.
	Snapshots ForkSnapshotReader
	// Cards resolves the origin card and creates the forked card (parentTaskId).
	Cards ForkCardStore
	// Admission releases/leaves a slot reservation left by a failed first
	// input. Nil skips the compensation (a deployment without placement).
	Admission ForkAdmission
	// Cleanup archives/deletes the child session of a fork that never started.
	// Nil skips the compensation; the card is still closed and the fact cleared.
	Cleanup ForkSessionCleanup
	// Requeue re-enqueues an existing run for resume (the store implements it).
	// Resume refuses without it.
	Requeue RunRequeuer
	// Notes appends the parent-card task.note when a sub-agent returns. Nil
	// leaves the ledger note out; the structured result is unaffected.
	Notes SubagentNoteWriter
	// ConfigVersion reports the configuration identity the next attempt will
	// run under (the current loadout). Resume returns it so the caller can tell
	// "resumed under revision N" from a guess.
	ConfigVersion func() (hash string, revision int64)
	// InheritOwnership copies the parent session's ownership/owner relation to a
	// newly created child (spawn/fork) before the child's first input. Nil
	// leaves the child unowned: the route relation gate then denies its
	// single-resource routes (fail closed), so a deployment with the identity
	// surface must wire this. A failure aborts the child before any event is
	// committed.
	InheritOwnership func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID) error
	// OwnSpawnedChild creates a spawned child's ownership. When botID is
	// non-empty the child belongs to that member Bot (its Profile's Account);
	// an empty botID inherits the parent. It takes precedence over
	// InheritOwnership for spawn; fork keeps using InheritOwnership because a
	// fork is a copy of the parent's identity, not a delegation to a member.
	OwnSpawnedChild func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID, botID string) error
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
	// validateSelection is the input edge's submit-time selection gate (nil is
	// fail-closed, see SubagentOptions.ValidateSelection).
	validateSelection func(json.RawMessage) error

	forks            sessionfork.Store
	snapshots        ForkSnapshotReader
	cards            ForkCardStore
	admission        ForkAdmission
	cleanup          ForkSessionCleanup
	requeue          RunRequeuer
	notes            SubagentNoteWriter
	configVersion    func() (string, int64)
	inheritOwnership func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID) error
	ownSpawnedChild  func(ctx context.Context, tenantID, parentSessionID, childSessionID uuid.UUID, botID string) error

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
		gate:              opts.Gate,
		events:            opts.Events,
		runs:              opts.Runs,
		control:           opts.Control,
		hooks:             opts.Hooks,
		log:               opts.Logger,
		poll:              opts.Poll,
		defaultTimeout:    opts.DefaultTimeout,
		now:               opts.Now,
		records:           make(map[string]*SubagentRecord),
		validateSelection: opts.ValidateSelection,
		forks:             opts.Forks,
		snapshots:         opts.Snapshots,
		cards:             opts.Cards,
		admission:         opts.Admission,
		cleanup:           opts.Cleanup,
		requeue:           opts.Requeue,
		notes:             opts.Notes,
		configVersion:     opts.ConfigVersion,
		inheritOwnership:  opts.InheritOwnership,
		ownSpawnedChild:   opts.OwnSpawnedChild,
	}
	if opts.Gate != nil && opts.Edge != nil {
		r.starter = NewTurnStarter(opts.Gate, opts.Edge, opts.Hooks, opts.Logger)
		r.starter.Bindings = opts.Bindings
	}
	return r
}

// SpawnAsync starts a sub-agent and returns immediately. The child run is
// owned by the worker; call Wait to collect the structured result.
func (r *SubagentRunner) SpawnAsync(ctx context.Context, req SpawnRequest) (SubagentRecord, error) {
	if r.starter == nil || r.runs == nil {
		return SubagentRecord{}, errors.New("harnessext: sub-agent runner needs the session gate, the kernel edge and the run reader")
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return SubagentRecord{}, errors.New("harnessext: tenant and parent session are required")
	}
	req.Task = strings.TrimSpace(req.Task)
	if req.Task == "" {
		return SubagentRecord{}, errors.New("harnessext: task is required")
	}
	if _, exists, err := r.gate.SessionOwner(ctx, req.TenantID, req.ParentSessionID); err != nil {
		return SubagentRecord{}, err
	} else if !exists {
		return SubagentRecord{}, ErrParentSessionNotFound
	}

	// The child's execution binding travels as the input's own selection fact:
	// the child attempt then resolves the member's prompt, tools and model,
	// exactly like a session that picked that agent. The same submit-time gate
	// the session input edge runs applies here, before the child session or the
	// user.message exists: a selection the next attempt would deny is refused at
	// the spawn call, never stored as a fact that fails every attempt.
	selection := req.childSelection()
	if len(selection) > 0 {
		if r.validateSelection == nil {
			return SubagentRecord{}, fmt.Errorf("%w: selection is not supported by this sub-agent runner", ErrInvalidSpawn)
		}
		if err := r.validateSelection(selection); err != nil {
			return SubagentRecord{}, fmt.Errorf("%w: selection: %v", ErrInvalidSpawn, err)
		}
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
			"origin":          "spawn",
		},
	}
	// The validated selection travels as the child input's own fact.
	if len(selection) > 0 {
		extras["selection"] = selection
	}
	// The child is a session resource: inherit the parent's owner before its
	// first input, so the route relation gate can authorize its single-resource
	// endpoints. A failure aborts before anything is committed.
	spawnBotID := r.spawnedBotID(req)
	if r.ownSpawnedChild != nil {
		if err := r.ownSpawnedChild(ctx, req.TenantID, req.ParentSessionID, childID, spawnBotID); err != nil {
			return SubagentRecord{}, err
		}
	} else if r.inheritOwnership != nil {
		if err := r.inheritOwnership(ctx, req.TenantID, req.ParentSessionID, childID); err != nil {
			return SubagentRecord{}, err
		}
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
		Origin:          "spawn",
		StartSeq:        start.Seq,
	}
	r.store(record)
	r.emit(HookSubagentStarted, record, map[string]string{
		"seq":    fmt.Sprintf("%d", start.Seq),
		"origin": "spawn",
	})
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
		return SubagentResult{}, errors.New("harnessext: sub-agent runner has no run reader")
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
		result.Origin = record.Origin
		result.OriginSeq = record.OriginSeq
		result.ParentTaskID = record.ParentTaskID
		result.ConfigHash = record.ConfigHash
		result.ToolManifestHash = record.ToolManifestHash
		parentSessionID = record.ParentSessionID
	}
	if parentSessionID == "" {
		// A restart loses the registry; the child's first input carries the
		// durable parent link. Without it the note has no card to land on.
		parentSessionID = r.durableParent(ctx, tenantID, sessionID)
		result.ParentSessionID = parentSessionID
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
	// The parent card's task.note is the return fact the parent reads next to
	// the structured value. It is idempotent through its task-event key, so a
	// repeated Wait cannot duplicate it.
	if parentSessionID != "" {
		if parentID, err := uuid.Parse(parentSessionID); err == nil {
			r.noteParent(ctx, tenantID, parentID, sessionID, status, summary)
		}
	}
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
	out := make([]Artifact, 0)
	add := func(artifact Artifact) {
		if len(out) >= artifactLimit {
			return
		}
		key := artifact.Type + "\x00" + artifact.Ref + "\x00" + artifact.Path
		if artifact.Ref == "" && artifact.Path == "" {
			return
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, artifact)
	}
	for _, event := range events {
		switch event.Kind {
		case kernel.KindToolResult:
			var result struct {
				ToolCallID string `json:"toolCallId"`
				ToolName   string `json:"toolName"`
				Details    any    `json:"details"`
			}
			if err := json.Unmarshal(event.Payload, &result); err != nil || result.ToolCallID == "" {
				continue
			}
			add(Artifact{Type: "tool", Name: result.ToolName, Ref: result.ToolCallID})
			// A bash result's full output is a blob reference; the trail keeps
			// the id, never the bytes.
			if details, ok := result.Details.(map[string]any); ok {
				for _, stream := range []string{"stdout", "stderr"} {
					entry, ok := details[stream].(map[string]any)
					if !ok {
						continue
					}
					if blobID, ok := entry["blobId"].(string); ok && blobID != "" {
						add(Artifact{Type: "blob", Name: stream, Ref: blobID})
					}
				}
			}
		case kernel.KindFSWrite:
			var write struct {
				Path string `json:"path"`
				Hash string `json:"hash"`
			}
			if err := json.Unmarshal(event.Payload, &write); err != nil {
				continue
			}
			add(Artifact{Type: "file", Name: write.Path, Ref: write.Hash, Path: write.Path, Hash: write.Hash})
		case kernel.KindFSCapture:
			var capture struct {
				TreeHash string `json:"treeHash"`
				Count    int    `json:"count"`
			}
			if err := json.Unmarshal(event.Payload, &capture); err != nil || capture.TreeHash == "" {
				continue
			}
			add(Artifact{Type: "tree", Name: fmt.Sprintf("capture:%d", capture.Count), Ref: capture.TreeHash, Hash: capture.TreeHash})
		}
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

// --- run scope + shared spawn handler ------------------------------------

type runScopeKey struct{}

// RunScope is the ambient run identity the `agent` tool needs to attribute a
// sub-agent to its parent. RunControl.BeginRun installs it on the attempt
// context, so a model tool call inside a run always has it.
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

// ErrRunScopeMissing reports a spawn request outside a run (no ambient parent).
var ErrRunScopeMissing = errors.New("harnessext: the spawn action needs the ambient run scope")

// ErrInvalidSpawn reports a malformed spawn request (bad JSON, missing task).
var ErrInvalidSpawn = errors.New("harnessext: invalid spawn arguments")

// SpawnFromTool is the one parser/handler behind the `agent` aggregate tool's
// spawn action. It carries the model's botId into the child input's selection
// fact (the child attempt then resolves that Agent's prompt, tool grants and
// default model), exactly like the HTTP sub-agent endpoint.
func SpawnFromTool(ctx context.Context, runner *SubagentRunner, args json.RawMessage) (SubagentResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok {
		return SubagentResult{}, ErrRunScopeMissing
	}
	if runner == nil {
		return SubagentResult{}, errors.New("harnessext: no sub-agent runner is wired")
	}
	var input struct {
		Task           string `json:"task"`
		Agent          string `json:"agent"`
		BotID          string `json:"botId"`
		IdempotencyKey string `json:"idempotencyKey"`
		MaxTurns       int    `json:"maxTurns"`
		MaxTokens      int    `json:"maxTokens"`
		TimeoutMs      int64  `json:"timeoutMs"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return SubagentResult{}, fmt.Errorf("%w: %v", ErrInvalidSpawn, err)
	}
	if strings.TrimSpace(input.Task) == "" {
		return SubagentResult{}, fmt.Errorf("%w: task is required", ErrInvalidSpawn)
	}
	return runner.Spawn(ctx, SpawnRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		Task:            input.Task,
		Agent:           input.Agent,
		BotID:           input.BotID,
		IdempotencyKey:  input.IdempotencyKey,
		Budget:          Budget{MaxTurns: input.MaxTurns, MaxTokens: input.MaxTokens, TimeoutMs: input.TimeoutMs},
	})
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
