package harnessext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/sessionfork"
	"github.com/jixoai/keel/internal/tasklog"
)

// Fork/resume/cancel of internal sub-agents (change keel-harness-v2, W8).
//
// The three actions share one rule: a child is an ordinary session. Fork creates
// a new session whose ledger prefix and file view are references to the parent
// at an eventSeq (never copies); resume re-enqueues the same session's run
// (never a new session, input or card); cancel stops the same run. The durable
// facts are the fork columns on the child session, the child's first
// user.message marks and the task cards; the in-process registry is only an
// index that a restart can rebuild.

var (
	// ErrForkUnavailable reports a deployment without the fork seams (durable
	// fork fact or card writer). A fork without them would create a child that
	// cannot answer where it came from.
	ErrForkUnavailable = errors.New("agentext: fork is not wired in this deployment")
	// ErrForkOriginInvalid reports an eventSeq outside the parent's committed
	// log (negative, or beyond the newest seq).
	ErrForkOriginInvalid = errors.New("agentext: fork origin is outside the parent ledger")
	// ErrForkSnapshotMissing reports a fork origin whose file view cannot be
	// resolved exactly: the parent has file mutations after the newest snapshot
	// at or before the origin. The caller must fork at a captured seq.
	ErrForkSnapshotMissing = errors.New("agentext: fork origin has no exact file snapshot")
	// ErrSubagentNotFound reports a child session that is not a sub-agent of the
	// named parent (or does not exist in the tenant).
	ErrSubagentNotFound = errors.New("agentext: sub-agent not found under this parent")
	// ErrResumeUnavailable reports a deployment without the run requeue seam.
	ErrResumeUnavailable = errors.New("agentext: resume is not wired in this deployment")
)

// ForkSnapshotReader resolves a session's file-view tree at or before an event
// sequence. The file plugin's manifest implements the query; the runner only
// needs the hash.
type ForkSnapshotReader interface {
	SnapshotAtOrBefore(ctx context.Context, tenantID, sessionID uuid.UUID, seq int64) (treeHash string, snapshotSeq int64, ok bool, err error)
}

// ForkCardStore resolves the card at a ledger position and creates the forked
// card. *tasklog.Service backs it in production; the adapter is in the serve
// layer so harnessext never depends on the task-log vocabulary.
type ForkCardStore interface {
	OriginCard(ctx context.Context, scope kernel.Scope, seq int64) (uuid.UUID, error)
	ForkCard(ctx context.Context, origin kernel.Scope, originTaskID uuid.UUID, originSeq int64, target kernel.Scope, title, callID string) (uuid.UUID, error)
	// CloseForkCard closes the child card a rolled-back fork created, so a
	// failed attempt leaves a converged card instead of an open orphan.
	CloseForkCard(ctx context.Context, scope kernel.Scope, childSessionID uuid.UUID, reason, callID string) error
}

// ForkAdmission is the placement compensation seam. A fork that fails after the
// admission gate may have created a placement (and, if the failure was later
// than admission, a run reservation); the rollback releases both.
type ForkAdmission interface {
	Release(ctx context.Context, tenantID, sessionID uuid.UUID) error
	Leave(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// ForkSessionCleanup is the session compensation seam of a fork that never
// started. The store implements it: an empty child is deleted, a child with
// ledger facts (the closed card) is archived, and a deterministic retry revives
// the archived child before reusing it.
type ForkSessionCleanup interface {
	EnsureSessionActive(ctx context.Context, tenantID, sessionID uuid.UUID) error
	ArchiveSession(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)
	DeleteSessionIfEmpty(ctx context.Context, tenantID, sessionID uuid.UUID) (bool, error)
}

// RunRequeuer re-enqueues an existing session's run. The store implements it:
// the update is guarded by the run row lock, so a live attempt is never
// duplicated.
type RunRequeuer interface {
	RequeueRun(ctx context.Context, tenantID, sessionID uuid.UUID) (RequeueOutcome, error)
}

// RequeueOutcome is the durable state after a requeue attempt.
type RequeueOutcome struct {
	// Status is "requeued", "pending", "running", "done" or "no_run".
	Status   string
	Attempts int
}

// SubagentNoteWriter appends the parent card's task.note when a child returns.
type SubagentNoteWriter interface {
	NoteSubagentFinished(ctx context.Context, parent kernel.Scope, childSessionID uuid.UUID, status, summary string) error
}

// ForkRequest asks for one forked child session.
type ForkRequest struct {
	TenantID        uuid.UUID `json:"-"`
	ParentSessionID uuid.UUID `json:"-"`
	// EventSeq is the parent ledger origin. Zero means "the current tip" (a
	// convenience for the common "fork now" call).
	EventSeq int64 `json:"eventSeq"`
	// Task is the child's instruction. Empty falls back to a deterministic
	// "continue from the fork origin" input so the child still has a turn.
	Task string `json:"task,omitempty"`
	// Title names the child card (empty inherits the origin card's title).
	Title string `json:"title,omitempty"`
	// TaskID optionally names the origin card explicitly. Empty resolves the
	// card of the newest task event at or before EventSeq.
	TaskID string `json:"taskId,omitempty"`
	// Agent is the free-form label the record/hooks carry; BotID/Selection is
	// the execution binding. Empty inherits the parent's selection at the origin
	// (the child starts on the same configuration), then the child may carry its
	// own selection on later inputs.
	Agent          string          `json:"agent,omitempty"`
	BotID          string          `json:"botId,omitempty"`
	Selection      json.RawMessage `json:"selection,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Budget         Budget          `json:"budget,omitempty"`
}

// childSelection derives the child input's selection fact from the fork
// request, exactly like SpawnRequest.childSelection.
func (r ForkRequest) childSelection() json.RawMessage {
	if raw := strings.TrimSpace(string(r.Selection)); raw != "" && raw != "null" {
		return append(json.RawMessage(nil), r.Selection...)
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

// ResumeRequest asks to continue an interrupted sub-agent run in place.
type ResumeRequest struct {
	TenantID        uuid.UUID `json:"-"`
	ParentSessionID uuid.UUID `json:"-"`
	SessionID       uuid.UUID `json:"sessionId"`
	// RequestID is the idempotency key of the resume. Empty derives one from the
	// session and its current attempt count, so two immediate calls converge.
	RequestID string `json:"requestId,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Selection / BotID optionally change the child's execution binding for
	// the resumed attempt (the "parent changes the child's config, then resumes"
	// scenario). The override travels in the resume.attempt fact, never as a new
	// user message, so the child's input history is not duplicated.
	Selection json.RawMessage `json:"selection,omitempty"`
	BotID     string          `json:"botId,omitempty"`
}

// resumeSelection derives the optional binding override.
func (r ResumeRequest) resumeSelection() json.RawMessage {
	if raw := strings.TrimSpace(string(r.Selection)); raw != "" && raw != "null" {
		return append(json.RawMessage(nil), r.Selection...)
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

// ResumeResult is the explicit answer of a resume call. Status is never a
// claim of success: "terminal" and "running" are outcomes too.
type ResumeResult struct {
	Status         string `json:"status"`
	SessionID      string `json:"sessionId"`
	EventSeq       int64  `json:"eventSeq,omitempty"`
	ConfigHash     string `json:"configHash,omitempty"`
	ConfigRevision int64  `json:"configRevision,omitempty"`
	Attempts       int    `json:"attempts,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// CancelRequest asks to stop a child run.
type CancelRequest struct {
	TenantID        uuid.UUID `json:"-"`
	ParentSessionID uuid.UUID `json:"-"`
	SessionID       uuid.UUID `json:"sessionId"`
	Reason          string    `json:"reason,omitempty"`
}

// ForkAsync creates the child session, its fork fact, its card and its first
// input, then returns immediately. Wait collects the structured result.
func (r *SubagentRunner) ForkAsync(ctx context.Context, req ForkRequest) (SubagentRecord, error) {
	if r.starter == nil || r.runs == nil {
		return SubagentRecord{}, errors.New("agentext: fork needs the session gate, the kernel edge and the run reader")
	}
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil {
		return SubagentRecord{}, errors.New("agentext: tenant and parent session are required")
	}
	if req.EventSeq < 0 {
		return SubagentRecord{}, fmt.Errorf("%w: eventSeq must be non-negative", ErrForkOriginInvalid)
	}
	if r.forks == nil || r.cards == nil {
		return SubagentRecord{}, ErrForkUnavailable
	}
	if _, exists, err := r.gate.SessionOwner(ctx, req.TenantID, req.ParentSessionID); err != nil {
		return SubagentRecord{}, err
	} else if !exists {
		return SubagentRecord{}, ErrParentSessionNotFound
	}

	parentScope := kernel.Scope{TenantID: req.TenantID, SessionID: req.ParentSessionID}
	events, maxSeq, err := r.loadLedger(ctx, parentScope)
	if err != nil {
		return SubagentRecord{}, err
	}
	origin := req.EventSeq
	if origin == 0 {
		origin = maxSeq
	}
	if origin > maxSeq {
		return SubagentRecord{}, fmt.Errorf("%w: eventSeq %d is beyond the newest committed seq %d", ErrForkOriginInvalid, origin, maxSeq)
	}

	// The origin card: explicit taskId wins, otherwise the newest task event at
	// or before the origin (the card that owned that ledger position).
	var originTaskID uuid.UUID
	if strings.TrimSpace(req.TaskID) != "" {
		parsed, err := uuid.Parse(req.TaskID)
		if err != nil {
			return SubagentRecord{}, fmt.Errorf("%w: taskId is not a UUID", ErrInvalidSpawn)
		}
		originTaskID = parsed
	} else {
		originTaskID, err = r.cards.OriginCard(ctx, parentScope, origin)
		if err != nil {
			return SubagentRecord{}, err
		}
	}

	treeHash, err := r.forkTreeHash(ctx, parentScope, events, origin)
	if err != nil {
		return SubagentRecord{}, err
	}
	configHash, manifestHash, anchorSeq := latestRunSnapshot(events, origin)

	// The child starts on the parent's configuration at the origin unless the
	// caller names its own binding. The selection travels as the child input's
	// selection fact, exactly like a spawned child, so the child attempt
	// resolves the same Agent/prompt/tools/model.
	selection := req.childSelection()
	if len(selection) == 0 {
		selection = latestSelection(events, origin)
	}
	if len(selection) > 0 {
		if r.validateSelection == nil {
			return SubagentRecord{}, fmt.Errorf("%w: selection is not supported by this sub-agent runner", ErrInvalidSpawn)
		}
		if err := r.validateSelection(selection); err != nil {
			return SubagentRecord{}, fmt.Errorf("%w: selection: %v", ErrInvalidSpawn, err)
		}
	}

	key := strings.TrimSpace(req.IdempotencyKey)
	childID := uuid.New()
	requestID := uuid.NewString()
	if key != "" {
		childID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("keel.subagent.fork.v1|"+
			req.TenantID.String()+"|"+req.ParentSessionID.String()+"|"+
			fmt.Sprintf("%d", origin)+"|"+key))
		requestID = "subagent-fork:" + key
	}
	// The fork child is a session resource: copy the parent's owner relation
	// before the row is written, so its routes are authorized by the same
	// relation as the parent. gate.CreateSession below is ON CONFLICT DO NOTHING
	// and therefore does not touch the owned row.
	if r.inheritOwnership != nil {
		if err := r.inheritOwnership(ctx, req.TenantID, req.ParentSessionID, childID); err != nil {
			return SubagentRecord{}, err
		}
	}
	if err := r.gate.CreateSession(ctx, req.TenantID, childID); err != nil {
		return SubagentRecord{}, err
	}
	// A deterministic child id makes a retry reuse the same session. A previous
	// attempt that failed after the card was created archived that child; revive
	// it here so the retry converges instead of writing into a hidden session.
	if r.cleanup != nil {
		if err := r.cleanup.EnsureSessionActive(ctx, req.TenantID, childID); err != nil {
			return SubagentRecord{}, err
		}
	}
	fact := sessionfork.Fact{
		ParentSessionID:  req.ParentSessionID,
		OriginSeq:        origin,
		TreeHash:         treeHash,
		ConfigHash:       configHash,
		ToolManifestHash: manifestHash,
		AnchorSeq:        anchorSeq,
	}
	if err := r.forks.RecordSessionFork(ctx, req.TenantID, childID, fact); err != nil {
		r.rollbackFork(ctx, req.TenantID, childID, false)
		return SubagentRecord{}, err
	}
	childScope := kernel.Scope{TenantID: req.TenantID, SessionID: childID}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = forkTitle(req.Task)
	}
	callID := "fork:" + childID.String()
	if _, err := r.cards.ForkCard(ctx, parentScope, originTaskID, origin, childScope, title, callID); err != nil {
		r.rollbackFork(ctx, req.TenantID, childID, true)
		return SubagentRecord{}, fmt.Errorf("agentext: create forked card: %w", err)
	}

	task := strings.TrimSpace(req.Task)
	if task == "" {
		task = fmt.Sprintf("Continue from fork origin event %d.", origin)
	}
	extras := map[string]any{
		"subagent": map[string]any{
			"parentSessionId": req.ParentSessionID.String(),
			"agent":           req.Agent,
			"idempotencyKey":  key,
			"budget":          req.Budget,
			"origin":          "fork",
			"originSeq":       origin,
		},
		"fork": map[string]any{
			"parentSessionId":  req.ParentSessionID.String(),
			"originSeq":        origin,
			"treeHash":         treeHash,
			"configHash":       configHash,
			"toolManifestHash": manifestHash,
			"anchorSeq":        anchorSeq,
			"originTaskId":     originTaskID.String(),
		},
	}
	if len(selection) > 0 {
		extras["selection"] = selection
	}
	start, err := r.starter.StartTurn(ctx, StartTurnRequest{
		TenantID:  req.TenantID,
		SessionID: childID,
		Text:      task,
		RequestID: requestID,
		Extras:    extras,
	})
	if err != nil {
		// A failure after the input commit (for example the binding projection)
		// must NOT be rolled back: the child run is durable and would otherwise
		// execute without its fork fact or card. Only a fork whose input never
		// landed is compensated.
		if r.Edge() != nil {
			if _, found, lookupErr := r.Edge().Lookup(ctx, childScope,
				kernel.KeyRef{Kind: kernel.KeyRequest, Value: requestID}); lookupErr != nil {
				r.log.Warn("fork failed and the commit state is unknown; not rolling back",
					"session_id", childID, "err", err, "lookup_err", lookupErr)
				return SubagentRecord{}, err
			} else if found {
				r.log.Warn("fork input committed but the turn returned an error; keeping the fork",
					"session_id", childID, "err", err)
				return SubagentRecord{}, err
			}
		}
		// The admission gate refused the first input (group full / tenant quota)
		// or the commit failed before appending. Compensate every side effect
		// this fork created: the slot reservation, the fork fact, the card and
		// the child session.
		r.rollbackFork(ctx, req.TenantID, childID, true)
		return SubagentRecord{}, err
	}
	record := SubagentRecord{
		ID:               childID.String(),
		TenantID:         req.TenantID.String(),
		ParentSessionID:  req.ParentSessionID.String(),
		SessionID:        childID.String(),
		Task:             task,
		Agent:            req.Agent,
		Status:           "running",
		StartedAt:        r.now(),
		Origin:           "fork",
		OriginSeq:        origin,
		ParentTaskID:     originTaskID.String(),
		ConfigHash:       configHash,
		ToolManifestHash: manifestHash,
		StartSeq:         start.Seq,
	}
	r.store(record)
	r.emit(HookSubagentStarted, record, map[string]string{
		"seq":      fmt.Sprintf("%d", start.Seq),
		"origin":   "fork",
		"eventSeq": fmt.Sprintf("%d", origin),
	})
	return record, nil
}

// Fork creates a forked child and waits (bounded by the budget) for its
// structured result.
func (r *SubagentRunner) Fork(ctx context.Context, req ForkRequest) (SubagentResult, SubagentRecord, error) {
	record, err := r.ForkAsync(ctx, req)
	if err != nil {
		return SubagentResult{}, SubagentRecord{}, err
	}
	tenantID, _ := uuid.Parse(record.TenantID)
	sessionID, _ := uuid.Parse(record.SessionID)
	result, err := r.Wait(ctx, tenantID, sessionID, req.Budget, req.Agent)
	if latest, found := r.Get(sessionID); found {
		record = latest
	}
	return result, record, err
}

// Resume re-enqueues an existing child run. It never creates a session, an
// input or a card: the child continues from its own ledger, and the resume fact
// is a bookkeeping event (resume.attempt) keyed by requestId.
func (r *SubagentRunner) Resume(ctx context.Context, req ResumeRequest) (ResumeResult, error) {
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil || req.SessionID == uuid.Nil {
		return ResumeResult{}, errors.New("agentext: tenant, parent session and child session are required")
	}
	if _, err := r.requireChild(ctx, req.TenantID, req.ParentSessionID, req.SessionID); err != nil {
		return ResumeResult{}, err
	}
	if r.runs == nil {
		return ResumeResult{}, ErrResumeUnavailable
	}
	state, err := r.runs.RunState(ctx, req.TenantID, req.SessionID)
	if errors.Is(err, ErrNoRun) {
		return ResumeResult{Status: "no_run", SessionID: req.SessionID.String(),
			Detail: "the child session has no run row; there is nothing to resume"}, nil
	}
	if err != nil {
		return ResumeResult{}, err
	}
	result := ResumeResult{Status: state.Status, SessionID: req.SessionID.String(), Attempts: state.Attempts}
	switch state.Status {
	case "running":
		result.Detail = "the child is already executing; resume did not start a second attempt"
		return result, nil
	case "pending":
		result.Detail = "the child is already queued; resume did not add another attempt"
		return result, nil
	case "done":
		result.Status = "terminal"
		result.Detail = "the child run is already done; resume does not reopen a completed task"
		return result, nil
	}
	if hash, revision := r.currentConfig(); hash != "" || revision > 0 {
		result.ConfigHash, result.ConfigRevision = hash, revision
	}
	// Only a run that is actually being re-queued needs the write seams; a
	// terminal/pending/running answer stays an explicit read-only outcome.
	if r.Edge() == nil || r.requeue == nil {
		return ResumeResult{}, ErrResumeUnavailable
	}
	selection := req.resumeSelection()
	if len(selection) > 0 {
		if r.validateSelection == nil {
			return ResumeResult{}, fmt.Errorf("%w: selection is not supported by this sub-agent runner", ErrInvalidSpawn)
		}
		if err := r.validateSelection(selection); err != nil {
			return ResumeResult{}, fmt.Errorf("%w: selection: %v", ErrInvalidSpawn, err)
		}
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		requestID = fmt.Sprintf("resume:%s:%d", req.SessionID, state.Attempts)
	}
	fields := map[string]any{
		"sessionId":      req.SessionID.String(),
		"statusBefore":   state.Status,
		"attemptsBefore": state.Attempts,
		"configHash":     result.ConfigHash,
		"configRevision": result.ConfigRevision,
		"reason":         strings.TrimSpace(req.Reason),
	}
	if len(selection) > 0 {
		fields["selection"] = selection
	}
	payload, _ := json.Marshal(fields)
	receipt, err := r.Edge().Commit(ctx, kernel.CommitRequest{
		Scope:     kernel.Scope{TenantID: req.TenantID, SessionID: req.SessionID},
		Kind:      kernel.KindResumeAttempt,
		System:    true,
		ResumeKey: requestID,
		Payload:   payload,
	})
	if err != nil {
		return ResumeResult{}, err
	}
	result.EventSeq = receipt.Seq
	outcome, err := r.requeue.RequeueRun(ctx, req.TenantID, req.SessionID)
	if err != nil {
		return ResumeResult{}, err
	}
	result.Attempts = outcome.Attempts
	switch outcome.Status {
	case "running":
		result.Status = "running"
		result.Detail = "a live attempt won the requeue race; resume did not duplicate it"
	case "done":
		result.Status = "terminal"
		result.Detail = "the child run settled while resuming; resume did not reopen it"
	case "no_run":
		result.Status = "no_run"
		result.Detail = "the child session has no run row"
	default:
		result.Status = "resumed"
		result.Detail = "the same child session was re-queued; its ledger, card and bindings are unchanged"
	}
	r.update(req.SessionID, func(record *SubagentRecord) {
		record.Status = "running"
		record.Error = ""
		if result.ConfigHash != "" {
			record.ConfigHash = result.ConfigHash
		}
	})
	return result, nil
}

// Cancel stops a child run in place. It never creates a card or a new run.
func (r *SubagentRunner) Cancel(ctx context.Context, req CancelRequest) (CancelResult, error) {
	if req.TenantID == uuid.Nil || req.ParentSessionID == uuid.Nil || req.SessionID == uuid.Nil {
		return CancelResult{}, errors.New("agentext: tenant, parent session and child session are required")
	}
	if _, err := r.requireChild(ctx, req.TenantID, req.ParentSessionID, req.SessionID); err != nil {
		return CancelResult{}, err
	}
	if r.control == nil {
		return CancelResult{}, ErrCancelUnavailable
	}
	result, err := r.control.Cancel(ctx, kernel.Scope{TenantID: req.TenantID, SessionID: req.SessionID})
	if err != nil {
		return result, err
	}
	if result.Cancelled {
		r.update(req.SessionID, func(record *SubagentRecord) {
			record.Status = "cancelled"
			record.Error = strings.TrimSpace(req.Reason)
		})
	}
	return result, nil
}

// Edge exposes the kernel commit seam the resume path needs (the runner keeps
// it private for the starter).
func (r *SubagentRunner) Edge() kernel.Committer {
	if r.starter == nil {
		return nil
	}
	return r.starter.Edge
}

// forkTreeHash resolves the parent's file view at the origin. An exact view is
// required when the parent has file facts: a snapshot older than the origin is
// accepted only when no file mutation happened in between (the view is then
// identical), and a parent with file facts but no snapshot is refused.
func (r *SubagentRunner) forkTreeHash(ctx context.Context, parent kernel.Scope, events []kernel.Event, origin int64) (string, error) {
	if r.snapshots == nil {
		if fileFacts(events, 0, origin) {
			return "", fmt.Errorf("%w: the file plugin is not wired to resolve a snapshot", ErrForkSnapshotMissing)
		}
		return "", nil
	}
	treeHash, snapshotSeq, found, err := r.snapshots.SnapshotAtOrBefore(ctx, parent.TenantID, parent.SessionID, origin)
	if err != nil {
		return "", err
	}
	if found {
		if fileFacts(events, snapshotSeq, origin) {
			return "", fmt.Errorf("%w: the newest snapshot is at seq %d and the parent wrote files before %d", ErrForkSnapshotMissing, snapshotSeq, origin)
		}
		return treeHash, nil
	}
	if fileFacts(events, 0, origin) {
		return "", fmt.Errorf("%w: the parent has file facts at or before seq %d but no captured snapshot", ErrForkSnapshotMissing, origin)
	}
	return "", nil
}

// loadLedger reads the whole parent log and answers the newest seq. Fork is not
// a hot path; reading the log once is cheaper than a second query seam.
func (r *SubagentRunner) loadLedger(ctx context.Context, scope kernel.Scope) ([]kernel.Event, int64, error) {
	var all []kernel.Event
	var after int64
	for {
		page, err := r.events.LoadEvents(ctx, scope, after, 500)
		if err != nil {
			return nil, 0, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return all, after, nil
}

// latestSelection returns the newest selection fact at or before the origin.
func latestSelection(events []kernel.Event, origin int64) json.RawMessage {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Seq > origin {
			continue
		}
		// A resume.attempt may carry a binding override; the newest of the two
		// fact kinds is the configuration the parent actually ran under.
		if event.Kind != kernel.KindUserMessage && event.Kind != kernel.KindResumeAttempt {
			continue
		}
		var payload struct {
			Selection json.RawMessage `json:"selection"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if raw := strings.TrimSpace(string(payload.Selection)); raw != "" && raw != "null" {
			return append(json.RawMessage(nil), payload.Selection...)
		}
	}
	return nil
}

// latestRunSnapshot returns the newest run.finished values at or before the
// origin: the configuration identity the parent attempt froze. Empty values
// mean the parent never ran.
func latestRunSnapshot(events []kernel.Event, origin int64) (configHash, manifestHash string, anchorSeq int64) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Seq > origin || event.Kind != kernel.KindRunFinished {
			continue
		}
		var payload struct {
			ConfigHash       string `json:"configHashAfter"`
			ToolManifestHash string `json:"toolManifestHash"`
			AnchorEventSeq   int64  `json:"anchorEventSeq"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		return payload.ConfigHash, payload.ToolManifestHash, payload.AnchorEventSeq
	}
	return "", "", 0
}

// fileFacts reports whether the parent wrote files in (after, upto]. The fs
// event kinds are the only ways a file view changes.
func fileFacts(events []kernel.Event, after, upto int64) bool {
	for _, event := range events {
		if event.Seq <= after || event.Seq > upto {
			continue
		}
		switch event.Kind {
		case kernel.KindFSWrite, kernel.KindFSCapture:
			return true
		}
	}
	return false
}

// forkTitle derives a card title from the child's task.
func forkTitle(task string) string {
	task = strings.Join(strings.Fields(task), " ")
	if task == "" {
		return "Fork"
	}
	runes := []rune(task)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return task
}

// requireChild verifies durably that child is a sub-agent of parent. The
// registry is only consulted after the ledger: after a restart the registry is
// cold, and the child's first input is the durable parent link.
func (r *SubagentRunner) requireChild(ctx context.Context, tenantID, parentID, childID uuid.UUID) (SubagentRecord, error) {
	if record, ok := r.Get(childID); ok {
		if record.TenantID != tenantID.String() || record.ParentSessionID != parentID.String() {
			return SubagentRecord{}, ErrSubagentNotFound
		}
		return record, nil
	}
	link, ok, err := r.durableLink(ctx, tenantID, childID)
	if err != nil {
		return SubagentRecord{}, err
	}
	if !ok || link.ParentSessionID != parentID {
		return SubagentRecord{}, ErrSubagentNotFound
	}
	return SubagentRecord{
		ID: childID.String(), TenantID: tenantID.String(), ParentSessionID: parentID.String(),
		SessionID: childID.String(), Agent: link.Agent, Origin: link.Origin, OriginSeq: link.OriginSeq,
	}, nil
}

// durableLink reads the parent mark from the child's first user.message. It is
// the post-restart truth the registry cannot provide.
type durableLink struct {
	ParentSessionID uuid.UUID
	Origin          string
	OriginSeq       int64
	Agent           string
}

func (r *SubagentRunner) durableLink(ctx context.Context, tenantID, childID uuid.UUID) (durableLink, bool, error) {
	events, err := r.readEvents(ctx, tenantID, childID)
	if err != nil {
		return durableLink{}, false, err
	}
	for _, event := range events {
		if event.Kind != kernel.KindUserMessage {
			continue
		}
		var payload struct {
			Subagent struct {
				ParentSessionID string `json:"parentSessionId"`
				Agent           string `json:"agent"`
				Origin          string `json:"origin"`
				OriginSeq       int64  `json:"originSeq"`
			} `json:"subagent"`
			Fork struct {
				ParentSessionID string `json:"parentSessionId"`
				OriginSeq       int64  `json:"originSeq"`
			} `json:"fork"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		parentRaw := payload.Subagent.ParentSessionID
		if parentRaw == "" {
			parentRaw = payload.Fork.ParentSessionID
		}
		parentID, err := uuid.Parse(parentRaw)
		if err != nil {
			return durableLink{}, false, nil
		}
		origin := payload.Subagent.Origin
		originSeq := payload.Subagent.OriginSeq
		if origin == "" && payload.Fork.OriginSeq > 0 {
			origin, originSeq = "fork", payload.Fork.OriginSeq
		}
		if origin == "" {
			origin = "spawn"
		}
		return durableLink{ParentSessionID: parentID, Origin: origin, OriginSeq: originSeq, Agent: payload.Subagent.Agent}, true, nil
	}
	return durableLink{}, false, nil
}

// durableParent answers the parent session id ("" when the child has no
// durable link).
func (r *SubagentRunner) durableParent(ctx context.Context, tenantID, childID uuid.UUID) string {
	link, ok, err := r.durableLink(ctx, tenantID, childID)
	if err != nil || !ok {
		return ""
	}
	return link.ParentSessionID.String()
}

// currentConfig reports the configuration identity the next attempt resolves.
func (r *SubagentRunner) currentConfig() (string, int64) {
	if r.configVersion == nil {
		return "", 0
	}
	return r.configVersion()
}

// noteParent writes the parent-card task.note for a finished child. It is
// best-effort: the structured result is the return value, the note is the
// parent's audit trail.
func (r *SubagentRunner) noteParent(ctx context.Context, tenantID, parentID, childID uuid.UUID, status, summary string) {
	if r.notes == nil {
		return
	}
	if err := r.notes.NoteSubagentFinished(context.WithoutCancel(ctx),
		kernel.Scope{TenantID: tenantID, SessionID: parentID}, childID, status, summary); err != nil {
		r.log.Warn("append sub-agent note to the parent card", "session_id", childID, "err", err)
	}
}

// rollbackFork compensates a fork that never started. Every step is
// best-effort and runs on a fresh bounded context: the first input's failure
// may be a cancelled request, and a cancelled context must not strand the
// reservation. The order is deliberate: release the run slot and the placement
// before deleting the session, close the card before archiving the child (so
// the task tree sees a converged card, not an open orphan), and clear the fork
// fact before the archive.
func (r *SubagentRunner) rollbackFork(ctx context.Context, tenantID, childID uuid.UUID, cardAttempted bool) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if r.admission != nil {
		if err := r.admission.Release(cleanupCtx, tenantID, childID); err != nil {
			r.log.Warn("fork rollback: release run slot", "session_id", childID, "err", err)
		}
		if err := r.admission.Leave(cleanupCtx, tenantID, childID); err != nil {
			r.log.Warn("fork rollback: leave placement", "session_id", childID, "err", err)
		}
	}
	if cardAttempted && r.cards != nil {
		scope := kernel.Scope{TenantID: tenantID, SessionID: childID}
		// The close is a state transition, not the fork's create fact: a fresh
		// suffix lets a retry cycle close the reopened card (the projection's
		// updated_seq guard would otherwise ignore an older deduped event). The
		// card count still converges to one.
		if err := r.cards.CloseForkCard(cleanupCtx, scope, childID, tasklog.ForkRollbackReason,
			"fork-rollback:"+childID.String()+":"+uuid.NewString()); err != nil {
			r.log.Warn("fork rollback: close card", "session_id", childID, "err", err)
		}
	}
	if r.forks != nil {
		if err := r.forks.ClearSessionFork(cleanupCtx, tenantID, childID); err != nil {
			r.log.Warn("fork rollback: clear fork fact", "session_id", childID, "err", err)
		}
	}
	if r.cleanup != nil {
		deleted, err := r.cleanup.DeleteSessionIfEmpty(cleanupCtx, tenantID, childID)
		if err != nil {
			r.log.Warn("fork rollback: delete empty session", "session_id", childID, "err", err)
			return
		}
		if deleted {
			return
		}
		if _, err := r.cleanup.ArchiveSession(cleanupCtx, tenantID, childID); err != nil {
			r.log.Warn("fork rollback: archive session", "session_id", childID, "err", err)
		}
	}
}

// ForkFromTool is the one parser/handler behind the `agent` aggregate tool's
// fork action.
func ForkFromTool(ctx context.Context, runner *SubagentRunner, args json.RawMessage) (SubagentResult, SubagentRecord, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok {
		return SubagentResult{}, SubagentRecord{}, ErrRunScopeMissing
	}
	if runner == nil {
		return SubagentResult{}, SubagentRecord{}, errors.New("agentext: no sub-agent runner is wired")
	}
	var input struct {
		EventSeq       *int64          `json:"eventSeq"`
		Task           string          `json:"task"`
		Title          string          `json:"title"`
		TaskID         string          `json:"taskId"`
		Agent          string          `json:"agent"`
		BotID          string          `json:"botId"`
		Selection      json.RawMessage `json:"selection"`
		IdempotencyKey string          `json:"idempotencyKey"`
		MaxTurns       int             `json:"maxTurns"`
		MaxTokens      int             `json:"maxTokens"`
		TimeoutMs      int64           `json:"timeoutMs"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return SubagentResult{}, SubagentRecord{}, fmt.Errorf("%w: %v", ErrInvalidSpawn, err)
	}
	if input.EventSeq == nil {
		return SubagentResult{}, SubagentRecord{}, fmt.Errorf("%w: eventSeq is required (0 means the current tip)", ErrInvalidSpawn)
	}
	return runner.Fork(ctx, ForkRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		EventSeq:        *input.EventSeq,
		Task:            input.Task,
		Title:           input.Title,
		TaskID:          input.TaskID,
		Agent:           input.Agent,
		BotID:           input.BotID,
		Selection:       input.Selection,
		IdempotencyKey:  input.IdempotencyKey,
		Budget:          Budget{MaxTurns: input.MaxTurns, MaxTokens: input.MaxTokens, TimeoutMs: input.TimeoutMs},
	})
}

// ResumeFromTool is the one parser/handler behind the `agent` aggregate tool's
// resume action.
func ResumeFromTool(ctx context.Context, runner *SubagentRunner, args json.RawMessage) (ResumeResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok {
		return ResumeResult{}, ErrRunScopeMissing
	}
	if runner == nil {
		return ResumeResult{}, errors.New("agentext: no sub-agent runner is wired")
	}
	var input struct {
		SessionID string          `json:"sessionId"`
		RequestID string          `json:"requestId"`
		Reason    string          `json:"reason"`
		Selection json.RawMessage `json:"selection"`
		BotID     string          `json:"botId"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return ResumeResult{}, fmt.Errorf("%w: %v", ErrInvalidSpawn, err)
	}
	sessionID, err := uuid.Parse(input.SessionID)
	if err != nil {
		return ResumeResult{}, fmt.Errorf("%w: resume requires a child sessionId", ErrInvalidSpawn)
	}
	return runner.Resume(ctx, ResumeRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		SessionID:       sessionID,
		RequestID:       input.RequestID,
		Reason:          input.Reason,
		Selection:       input.Selection,
		BotID:           input.BotID,
	})
}

// CancelFromTool is the one parser/handler behind the `agent` aggregate tool's
// cancel action.
func CancelFromTool(ctx context.Context, runner *SubagentRunner, args json.RawMessage) (CancelResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok {
		return CancelResult{}, ErrRunScopeMissing
	}
	if runner == nil {
		return CancelResult{}, errors.New("agentext: no sub-agent runner is wired")
	}
	var input struct {
		SessionID string `json:"sessionId"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return CancelResult{}, fmt.Errorf("%w: %v", ErrInvalidSpawn, err)
	}
	sessionID, err := uuid.Parse(input.SessionID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("%w: cancel requires a child sessionId", ErrInvalidSpawn)
	}
	return runner.Cancel(ctx, CancelRequest{
		TenantID:        scope.TenantID,
		ParentSessionID: scope.SessionID,
		SessionID:       sessionID,
		Reason:          input.Reason,
	})
}
