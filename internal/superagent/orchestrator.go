package superagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// InputRequest is one message delivered into an existing Worker session. The
// super-agent layer does not commit turns itself; the product adapter maps this
// onto the TurnStarter (the same edge triggers and sub-agents use).
type InputRequest struct {
	TenantID     uuid.UUID `json:"tenantId"`
	WorkItemID   uuid.UUID `json:"workItemId"`
	AssignmentID uuid.UUID `json:"assignmentId"`
	SessionID    uuid.UUID `json:"sessionId"`
	Text         string    `json:"text"`
	RequestID    string    `json:"requestId"`
	Kind         string    `json:"kind,omitempty"`
}

// InputSink delivers follow-ups and human answers into a live Worker session.
type InputSink interface {
	Deliver(ctx context.Context, request InputRequest) error
}

// WorkerWatcher observes a spawned worker session and projects its terminal
// state into the core SessionIndex. The orchestrator's slot reconciler then
// releases the durable Assignment; the watcher is not a second slot counter.
type WorkerWatcher interface {
	Watch(ctx context.Context, tenantID, sessionID uuid.UUID)
}

// WorkerState is the actual product-runner state of a worker session. It is
// consulted after the durable collab index, so a stale "active" row cannot keep
// a dead worker's slot forever.
type WorkerState string

const (
	WorkerActive    WorkerState = "active"
	WorkerDone      WorkerState = "done"
	WorkerFailed    WorkerState = "failed"
	WorkerCancelled WorkerState = "cancelled"
	WorkerMissing   WorkerState = "missing"
)

// WorkerInspector resolves the harness/run truth of a worker session. Nil
// falls back to the collab SessionIndex only.
type WorkerInspector interface {
	WorkerState(ctx context.Context, tenantID, sessionID uuid.UUID) (WorkerState, error)
}

// WorkerTreeCanceller stops a worker's bare sub-agent descendants. The core
// Host cancels the worker session tree it tracks; this seam covers the
// sub-agent runner's own tree so Stop cannot orphan grandchildren.
type WorkerTreeCanceller interface {
	CancelTree(ctx context.Context, tenantID, sessionID uuid.UUID, reason string) error
}

// Host is the core collaboration gate plus the terminal projections a
// work-queue mode needs. It is satisfied by *collab.Coordinator; no mode may
// mutate assignments or sessions around it.
type Host interface {
	collab.Host
	AnswerAttention(ctx context.Context, tenantID, workItemID, attentionID uuid.UUID, req collab.AttentionAnswerRequest) (collab.Attention, error)
	CompleteSession(ctx context.Context, tenantID, sessionID uuid.UUID, outcome collab.SessionOutcome, result json.RawMessage, failure string) error
}

// ErrOperatorUnavailable means an operator action (resume/stop from a console)
// has no authorization seam in this deployment. It is fail-closed: the action
// must not be treated as an implicit human takeover.
var ErrOperatorUnavailable = errors.New("superagent: operator authorization is not wired")

// OperatorAuthorizer is the independent permission seam for operator actions.
// It is deliberately not the HumanTakeover boolean: a free bool on an
// Assignment request is not an authorization, and no mode may mint one for
// itself.
type OperatorAuthorizer interface {
	AuthorizeOperator(ctx context.Context, tenantID, ticketID uuid.UUID, actor, action string) error
}

// NoopInputSink accepts input without delivering it. It is the honest adapter
// of a deployment that has not wired the TurnStarter yet; the decision is still
// recorded in the Ticket/Attention rows.
type NoopInputSink struct{}

// Deliver implements InputSink.
func (NoopInputSink) Deliver(context.Context, InputRequest) error { return nil }

// Config configures one tenant's orchestrator.
type Config struct {
	TenantID uuid.UUID
	ModeID   string
	Store    Store
	Host     Host
	Policy   Policy
	// Source/Channel are optional. A nil Source leaves events to the
	// HandleSourceEvent entry point; a nil Channel keeps outbox rows readable
	// but unsent.
	Source  collab.Source
	Channel collab.Channel
	Inputs  InputSink
	// Sessions reads the Host-created session index so recovery can tell a live
	// worker from a lost one. Nil skips liveness checks.
	Sessions collab.SessionIndex
	// WorkerWatcher projects worker-session completion/failure into Sessions.
	// Nil leaves completion to an explicit Report (tests without a product
	// runner).
	WorkerWatcher WorkerWatcher
	// WorkerInspector resolves the actual run state during Recover. Nil uses
	// the collab SessionIndex only.
	WorkerInspector WorkerInspector
	// WorkerTreeCanceller stops bare sub-agent descendants of a worker on
	// Ticket stop. Nil relies on the core Host's tree cancellation only.
	WorkerTreeCanceller WorkerTreeCanceller
	Audit               collab.AuditSink
	Outbox              *Outbox
	Slots               *Slots
	// Operator authorizes console actions (resume/stop). Nil keeps those
	// actions unavailable; source events are authorized by Policy instead.
	Operator OperatorAuthorizer
	// OrchestratorSession is the active Bot session that acts as the assignment
	// caller (the orchestrator role). It is a root Session created by the
	// product, not by a mode.
	OrchestratorSession uuid.UUID
	// OrchestratorBot/WorkerBot are default role identities. A Verdict may
	// override the worker per ticket.
	OrchestratorBot  string
	WorkerBot        string
	MaxPoll          int
	AttentionTimeout time.Duration
	TicketLimits     collab.Limits
	Now              func() time.Time
	Logger           *slog.Logger
	// Changed publishes one board/inbox invalidation after a successful
	// Ticket/Attention write. Nil is a no-op (tests and headless callers);
	// the product wires it to the identity SSE hub. Refused/failed writes do
	// not call it.
	Changed func(tenantID uuid.UUID)
}

// Orchestrator is the super-agent queue. It evaluates Source events, creates
// Tickets, dispatches Workers through the guarded collab.Host, routes structured
// Attention and keeps the Ticket/Assignment records independent of any Worker
// Session's lifetime.
type Orchestrator struct {
	cfg Config
	mu  sync.Mutex
}

// NewOrchestrator validates the assembly. Missing identity/seams fail at
// construction, never at the first event.
func NewOrchestrator(cfg Config) (*Orchestrator, error) {
	if cfg.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("superagent: work store is required")
	}
	if cfg.Host == nil {
		return nil, fmt.Errorf("superagent: collab host is required")
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("superagent: policy is required")
	}
	if cfg.Audit == nil {
		return nil, fmt.Errorf("superagent: audit sink is required (accepted paths are fail-closed)")
	}
	if cfg.OrchestratorSession == uuid.Nil {
		return nil, fmt.Errorf("superagent: orchestrator session is required")
	}
	if strings.TrimSpace(cfg.WorkerBot) == "" {
		return nil, fmt.Errorf("superagent: worker bot is required")
	}
	if cfg.ModeID == "" {
		cfg.ModeID = DefaultModeID
	}
	if cfg.Inputs == nil {
		cfg.Inputs = NoopInputSink{}
	}
	if cfg.MaxPoll <= 0 {
		cfg.MaxPoll = 50
	}
	if cfg.AttentionTimeout <= 0 {
		cfg.AttentionTimeout = collab.DefaultAttentionTimeout
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Orchestrator{cfg: cfg}, nil
}

// Run polls the Source until the context is cancelled. It recovers interrupted
// work first, then polls at the interval; a poll error is logged and retried
// (the cursor is not advanced on an infrastructure failure).
func (o *Orchestrator) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if _, err := o.Recover(ctx); err != nil {
		o.cfg.Logger.Warn("superagent: recovery failed", "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := o.PollOnce(ctx); err != nil {
			o.cfg.Logger.Warn("superagent: poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PollOnce reads one bounded page and then runs housekeeping. A Source failure
// does not stop attention expiry, slot reconciliation or outbox delivery: the
// poll error is returned alongside the housekeeping result.
func (o *Orchestrator) PollOnce(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(o.cfg.TenantID); err != nil {
		return 0, err
	}
	handled, pollErr := o.pollSourceLocked(ctx)
	houseErr := o.housekeepingLocked(ctx)
	return handled, errors.Join(pollErr, houseErr)
}

// Housekeeping runs the non-Source maintenance loop (attention expiry, session
// reconciliation, queue dispatch and outbox flush).
func (o *Orchestrator) Housekeeping(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(o.cfg.TenantID); err != nil {
		return err
	}
	return o.housekeepingLocked(ctx)
}

func (o *Orchestrator) pollSourceLocked(ctx context.Context) (int, error) {
	if o.cfg.Source == nil {
		return 0, nil
	}
	cursor, err := o.cfg.Store.GetSourceCursor(ctx, o.cfg.TenantID, sourceID(o.cfg.Source))
	if err != nil {
		return 0, err
	}
	page, err := o.cfg.Source.Poll(ctx, o.cfg.TenantID, cursor, o.cfg.MaxPoll)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, raw := range page.Events {
		if _, err := o.handleSourceEventLocked(ctx, raw); err != nil {
			return handled, err
		}
		handled++
	}
	if page.NextCursor != "" && page.NextCursor != cursor {
		if err := o.cfg.Store.SetSourceCursor(ctx, o.cfg.TenantID, sourceID(o.cfg.Source), page.NextCursor); err != nil {
			return handled, err
		}
	}
	return handled, nil
}

func (o *Orchestrator) housekeepingLocked(ctx context.Context) error {
	if _, err := o.expireAttentionsLocked(ctx); err != nil {
		return err
	}
	if _, err := o.reconcileLocked(ctx); err != nil {
		return err
	}
	if dispatched, err := o.dispatchQueuedLocked(ctx); err != nil {
		return err
	} else if dispatched > 0 {
		o.changed(o.cfg.TenantID)
	}
	if o.cfg.Outbox != nil {
		if _, err := o.cfg.Outbox.Flush(ctx, o.cfg.TenantID, 50); err != nil {
			return err
		}
	}
	return nil
}

// HandleSourceEvent is the single-event entry point (a webhook or a test).
// It is idempotent by source event id.
func (o *Orchestrator) HandleSourceEvent(ctx context.Context, raw collab.SourceEvent) (EventResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.handleSourceEventLocked(ctx, raw)
}

// EventResult reports what one event did.
type EventResult struct {
	EventID   string
	Decision  string
	TicketID  uuid.UUID
	Duplicate bool
	Reason    string
}

func (o *Orchestrator) handleSourceEventLocked(ctx context.Context, raw collab.SourceEvent) (EventResult, error) {
	event, parseErr := ParseSourceEvent(raw)
	result := EventResult{EventID: event.EventID}
	if parseErr != nil {
		// An undecodable event is recorded as seen-only when it has an identity;
		// it never blocks the cursor forever.
		if event.EventID != "" && event.SourceID != "" {
			_ = o.recordSeen(ctx, event, "invalid", uuid.Nil)
		}
		result.Decision = "invalid"
		result.Reason = parseErr.Error()
		return result, nil
	}
	result.EventID = event.EventID
	if err := event.Validate(); err != nil {
		result.Decision = "invalid"
		result.Reason = err.Error()
		return result, nil
	}
	seen, err := o.cfg.Store.SourceEventSeen(ctx, o.cfg.TenantID, event.SourceID, event.EventID)
	if err != nil {
		return result, err
	}
	if seen {
		result.Decision = "duplicate"
		result.Duplicate = true
		result.Reason = "the source event was already consumed"
		return result, nil
	}
	verdict, err := o.cfg.Policy.Evaluate(ctx, o.cfg.TenantID, event)
	if err != nil {
		return result, err
	}
	if !verdict.Action.Valid() {
		verdict = Verdict{Action: ActionIgnore, Reason: "policy returned an unknown action"}
	}
	if verdict.Action == ActionStop {
		stopped, err := o.stopThreadLocked(ctx, event, verdict)
		if err != nil {
			return result, err
		}
		_ = o.recordSeen(ctx, event, string(ActionStop), firstUUID(stopped))
		result.Decision = string(ActionStop)
		result.TicketID = firstUUID(stopped)
		result.Reason = verdict.Reason
		return result, nil
	}

	// A matching subscription fans out regardless of the sender identity: the
	// thread is context, not authority. An untrusted comment is delivered to the
	// already-accepted Ticket, but it can never create one or reach a stopped
	// Ticket (attachLocked skips those).
	subs, err := o.cfg.Store.ListThreadSubscriptions(ctx, o.cfg.TenantID, event.Thread)
	if err != nil {
		return result, err
	}
	if len(subs) > 0 {
		tickets, err := o.attachLocked(ctx, event, verdict, subs)
		if err != nil {
			return result, err
		}
		if len(tickets) > 0 {
			o.changed(o.cfg.TenantID)
		}
		reason := verdict.Reason
		if verdict.Action == ActionIgnore {
			reason = "delivered as untrusted thread context: " + verdict.Reason
		}
		_ = o.recordSeen(ctx, event, string(ActionAttach), firstTicketID(tickets))
		result.Decision = string(ActionAttach)
		result.TicketID = firstTicketID(tickets)
		result.Reason = reason
		return result, nil
	}
	if verdict.Action == ActionIgnore {
		_ = o.recordSeen(ctx, event, string(ActionIgnore), uuid.Nil)
		o.audit(ctx, AuditEventIgnored, "accepted", event, uuid.Nil, verdict.Reason)
		result.Decision = string(ActionIgnore)
		result.Reason = verdict.Reason
		return result, nil
	}
	if verdict.Action == ActionAttach {
		_ = o.recordSeen(ctx, event, string(ActionIgnore), uuid.Nil)
		o.audit(ctx, AuditEventIgnored, "accepted", event, uuid.Nil, "attach without a subscription")
		result.Decision = string(ActionIgnore)
		result.Reason = "no ticket subscribes to this thread"
		return result, nil
	}

	ticket, err := o.acceptLocked(ctx, event, verdict)
	if err != nil {
		return result, err
	}
	_ = o.recordSeen(ctx, event, string(ActionAccept), ticket.ID)
	result.Decision = string(ActionAccept)
	result.TicketID = ticket.ID
	result.Reason = verdict.Reason
	_, _ = o.dispatchTicketLocked(ctx, ticket, false, "accepted event "+event.EventID, "")
	o.changed(o.cfg.TenantID)
	return result, nil
}

func (o *Orchestrator) acceptLocked(ctx context.Context, event Event, verdict Verdict) (Ticket, error) {
	now := o.cfg.Now()
	workerBot := strings.TrimSpace(verdict.BotID)
	if workerBot == "" {
		workerBot = o.cfg.WorkerBot
	}
	ticket := NewTicket(o.cfg.TenantID, o.cfg.ModeID, event, verdict, workerBot, now)
	ticket.OwnerSessionID = o.cfg.OrchestratorSession
	ticket.Mandate.AcceptedBy = o.cfg.OrchestratorBot
	ticket.Limits = o.cfg.TicketLimits
	ticket.IdempotencyKey = event.IdempotencyKey()
	ticket.WorkerBot = workerBot
	if err := o.auditRequired(ctx, AuditTicketAccepted, "accepted", ticket.ID, verdict.Reason); err != nil {
		return Ticket{}, err
	}
	stored, err := o.cfg.Store.CreateWorkItem(ctx, ticket.Record())
	if err != nil {
		return Ticket{}, err
	}
	ticket = TicketFromWorkItem(stored)
	if ticket.WorkerBot == "" {
		ticket.WorkerBot = workerBot
	}
	if _, err := o.ensureSubscription(ctx, ticket); err != nil {
		return Ticket{}, err
	}
	if ticket.Mandate.Strength == MandateObserve {
		// An observe-only mandate is real authorization to track the thread, not
		// to start a Worker. Blocking is the visible queued-for-human state.
		if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemBlocked, "observe-only mandate"); err != nil {
			return Ticket{}, err
		}
	}
	if o.cfg.Outbox != nil {
		_, _ = o.cfg.Outbox.Enqueue(ctx, OutboxMessage{
			TenantID:       o.cfg.TenantID,
			WorkItemID:     ticket.ID,
			ChannelID:      ticket.Thread.ID,
			Thread:         ticket.Thread,
			Text:           fmt.Sprintf("Ticket %s accepted: %s", shortID(ticket.ID), ticket.Title),
			Data:           marshalData(map[string]any{"ticketId": ticket.ID.String(), "event": "accepted"}),
			IdempotencyKey: "ticket:" + ticket.ID.String() + ":accepted",
		})
	}
	return ticket, nil
}

func (o *Orchestrator) attachLocked(ctx context.Context, event Event, verdict Verdict, subs []Subscription) ([]Ticket, error) {
	seenTickets := make(map[uuid.UUID]bool, len(subs))
	var delivered []Ticket
	for _, subscription := range subs {
		if seenTickets[subscription.WorkItemID] {
			continue
		}
		seenTickets[subscription.WorkItemID] = true
		ticket, err := o.loadTicket(ctx, o.cfg.TenantID, subscription.WorkItemID)
		if err != nil {
			continue
		}
		if ticket.Mandate.Revoked() || ticket.Status == collab.WorkItemCancelled || ticket.Status == collab.WorkItemCancelling {
			// A context subscription never grants authority and never reopens a
			// stopped ticket.
			_ = o.deactivateSubscription(ctx, subscription)
			continue
		}
		if ticket.Status == collab.WorkItemDone || ticket.Status == collab.WorkItemFailed {
			// Human follow-up may resume a finished ticket, but a subscribed
			// comment must not do it automatically (spec: subscription is context,
			// not authorization).
			continue
		}
		trusted := verdict.Action == ActionAttach || verdict.Action == ActionAccept
		// A policy-allowed comment on a thread with exactly one open question is
		// the structured answer path. An untrusted comment is never interpreted
		// as an answer.
		if trusted {
			answered, answerErr := o.answerFromCommentLocked(ctx, o.cfg.TenantID, ticket, event)
			if answerErr != nil {
				return nil, answerErr
			}
			if answered {
				delivered = append(delivered, ticket)
				continue
			}
		}
		if err := o.auditRequired(ctx, AuditTicketAttached, "accepted", ticket.ID, verdict.Reason); err != nil {
			return nil, err
		}
		assignment, _ := o.activeAssignment(ctx, ticket.ID)
		if assignment.ID != uuid.Nil && assignment.SessionID != uuid.Nil {
			text := eventInputText(event)
			if err := o.cfg.Inputs.Deliver(ctx, InputRequest{
				TenantID: o.cfg.TenantID, WorkItemID: ticket.ID, AssignmentID: assignment.ID,
				SessionID: assignment.SessionID, Text: text,
				RequestID: "followup:" + event.SourceID + ":" + event.EventID, Kind: "followup",
			}); err != nil {
				return nil, err
			}
		} else {
			// No live worker: only a policy-allowed human follow-up may resume a
			// blocked ticket. An untrusted Source comment is context, not authority,
			// and is never allowed to start a worker.
			if !trusted {
				continue
			}
			if ticket.Status == collab.WorkItemBlocked {
				if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemActive, "follow-up received"); err != nil {
					continue
				}
			}
			if _, err := o.dispatchTicketLocked(ctx, ticket, true, "human follow-up on "+event.Thread.ID, eventInputText(event)); err != nil {
				o.cfg.Logger.Warn("superagent: follow-up dispatch failed", "ticket", ticket.ID, "error", err)
			}
		}
		delivered = append(delivered, ticket)
	}
	return delivered, nil
}

func (o *Orchestrator) answerFromCommentLocked(ctx context.Context, tenantID uuid.UUID, ticket Ticket, event Event) (bool, error) {
	attentions, err := o.cfg.Store.ListAttentions(ctx, tenantID, ticket.ID)
	if err != nil {
		return false, err
	}
	var open []collab.Attention
	for _, attention := range attentions {
		if attention.Status == collab.AttentionOpen {
			open = append(open, attention)
		}
	}
	if len(open) != 1 {
		// Zero means nothing to answer; more than one is ambiguous and must not
		// be guessed from a comment.
		return false, nil
	}
	attention := open[0]
	optionID, text := answerFromAttention(attention, event.Body)
	if optionID == "" && text == "" {
		return false, nil
	}
	actor := event.Actor.Login
	if actor == "" {
		actor = event.Actor.ID
	}
	if _, err := o.answerAttentionLocked(ctx, tenantID, attention, optionID, text, actor); err != nil {
		return false, err
	}
	return true, nil
}

func answerFromAttention(attention collab.Attention, body string) (string, string) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", ""
	}
	if len(attention.Options) > 0 {
		for _, option := range attention.Options {
			if strings.EqualFold(trimmed, option.ID) || (option.Label != "" && strings.EqualFold(trimmed, option.Label)) {
				return option.ID, ""
			}
		}
		return "", ""
	}
	if attention.AllowFreeText {
		return "", trimmed
	}
	return "", ""
}

func (o *Orchestrator) stopThreadLocked(ctx context.Context, event Event, verdict Verdict) ([]uuid.UUID, error) {
	subs, err := o.cfg.Store.ListThreadSubscriptions(ctx, o.cfg.TenantID, event.Thread)
	if err != nil {
		return nil, err
	}
	var stopped []uuid.UUID
	for _, subscription := range subs {
		if err := o.stopTicketLocked(ctx, subscription.WorkItemID, event.Actor, "human stop from "+event.SourceID, event); err != nil {
			return stopped, err
		}
		stopped = append(stopped, subscription.WorkItemID)
	}
	if len(stopped) == 0 {
		o.audit(ctx, AuditEventIgnored, "accepted", event, uuid.Nil, "stop without a subscribed ticket")
	}
	return stopped, nil
}

// dispatchQueuedLocked dispatches open tickets that have no live assignment.
func (o *Orchestrator) dispatchQueuedLocked(ctx context.Context) (int, error) {
	items, err := o.cfg.Store.ListWorkItems(ctx, o.cfg.TenantID, collab.WorkFilter{ModeID: o.cfg.ModeID, Status: collab.WorkItemOpen})
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for _, item := range items {
		ticket := TicketFromWorkItem(item)
		if !ticket.CanDispatch() {
			continue
		}
		ok, err := o.dispatchTicketLocked(ctx, ticket, false, "queued ticket", "")
		if err != nil {
			if errors.Is(err, collab.ErrConcurrencyExceeded) {
				break
			}
			return dispatched, err
		}
		if ok {
			dispatched++
		}
	}
	return dispatched, nil
}

// DispatchQueued is the public bounded queue pump (tests/admin calls).
func (o *Orchestrator) DispatchQueued(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	count, err := o.dispatchQueuedLocked(ctx)
	if count > 0 {
		o.changed(o.cfg.TenantID)
	}
	return count, err
}

// dispatchTicketLocked creates one Assignment and Worker Session. The old
// Assignment is never overwritten: a replacement is a new row so the history
// stays auditable. The guarded Host is the only authorization gate: this
// method does not inspect WorkItemID membership or Scope.BotIDs itself; core
// owns those checks. It never sets HumanTakeover — a human resume goes through
// OperatorAuthorizer, not a free boolean.
func (o *Orchestrator) dispatchTicketLocked(ctx context.Context, ticket Ticket, humanFollowUp bool, reason, contextText string) (bool, error) {
	if !ticket.CanDispatch() {
		return false, nil
	}
	if existing, err := o.activeAssignment(ctx, ticket.ID); err != nil {
		return false, err
	} else if existing.ID != uuid.Nil {
		return false, nil
	}
	if o.cfg.Slots != nil {
		if err := o.cfg.Slots.Acquire(ctx, o.cfg.TenantID, humanFollowUp); err != nil {
			return false, err
		}
	}
	workerBot := strings.TrimSpace(ticket.WorkerBot)
	if workerBot == "" {
		workerBot = o.cfg.WorkerBot
	}
	assignments, err := o.cfg.Store.ListAssignments(ctx, o.cfg.TenantID, ticket.ID)
	if err != nil {
		return false, err
	}
	attempt := len(assignments) + 1
	assignmentKey := fmt.Sprintf("ticket:%s:assignment:%d", ticket.ID, attempt)
	input := marshalData(assignmentDoc{HumanFollowUp: humanFollowUp, Reason: reason, Context: contextText})
	if err := o.auditRequired(ctx, AuditDispatch, "accepted", ticket.ID, reason); err != nil {
		return false, err
	}
	assignment, err := o.cfg.Host.Assign(ctx, o.cfg.TenantID, ticket.ID, workerBot, collab.AssignmentRequest{
		CallerSessionID: o.cfg.OrchestratorSession,
		Title:           ticket.Title,
		Input:           input,
		IdempotencyKey:  assignmentKey,
	})
	if err != nil {
		o.audit(ctx, AuditDispatchFailed, "denied", Event{SourceID: ticket.Source.Kind, Thread: ticket.Thread}, ticket.ID, err.Error())
		return false, err
	}
	if _, err := o.bindWorkerSession(ctx, ticket, assignment); err != nil {
		// Failure must leave a terminal assignment, not an open row that blocks
		// the Ticket forever. Re-read before writing so a concurrent recovery
		// cannot be overwritten.
		o.failOpenAssignment(ctx, assignment.ID, err.Error())
		o.audit(ctx, AuditDispatchFailed, "failed", Event{SourceID: ticket.Source.Kind, Thread: ticket.Thread}, ticket.ID, err.Error())
		return false, err
	}
	return true, nil
}

func (o *Orchestrator) failOpenAssignment(ctx context.Context, assignmentID uuid.UUID, failure string) {
	assignment, err := o.cfg.Store.GetAssignment(ctx, o.cfg.TenantID, assignmentID)
	if err != nil || assignment.Status.Terminal() {
		return
	}
	assignment.Status = collab.AssignmentFailed
	assignment.Error = failure
	assignment.UpdatedAt = o.cfg.Now()
	_, _ = o.cfg.Store.UpdateAssignment(ctx, assignment)
}

func (o *Orchestrator) bindWorkerSession(ctx context.Context, ticket Ticket, assignment collab.Assignment) (collab.SessionRef, error) {
	// The worker is an ordinary Bot session spawned through the sub-agent
	// launcher. Its own sub-agent children are counted by the subagent mode's
	// depth tree, not by the Ticket/Assignment level: a Ticket is not a session
	// parent and never consumes sub-agent depth.
	task := workerTask(ticket)
	if note := strings.TrimSpace(assignmentDocOf(assignment).Context); note != "" {
		task += "\n\nFollow-up context:\n" + note
	}
	inputs := marshalData(map[string]any{
		"ticketId": ticket.ID.String(),
		"title":    ticket.Title,
		"source":   ticket.Source,
		"thread":   ticket.Thread,
		"mandate":  ticket.Mandate,
	})
	session, err := o.cfg.Host.CreateSession(ctx, o.cfg.TenantID, o.cfg.ModeID, collab.SessionRequest{
		WorkItemID:      ticket.ID,
		ParentSessionID: o.cfg.OrchestratorSession,
		BotID:           assignment.BotID,
		Task:            task,
		Inputs:          inputs,
		AssignmentID:    assignment.ID,
		IdempotencyKey:  "worker:" + assignment.ID.String(),
	})
	if err != nil {
		assignment.Status = collab.AssignmentFailed
		assignment.Error = err.Error()
		assignment.UpdatedAt = o.cfg.Now()
		_, _ = o.cfg.Store.UpdateAssignment(ctx, assignment)
		return collab.SessionRef{}, err
	}
	if o.cfg.WorkerWatcher != nil && session.SessionID != uuid.Nil {
		o.cfg.WorkerWatcher.Watch(ctx, o.cfg.TenantID, session.SessionID)
	}
	return session, nil
}

// ExpireAttentions closes open questions whose deadline passed. The row keeps
// its history; the inbox simply stops showing it. Expiry is a super-agent
// lifecycle over the core Attention row, not a second table.
func (o *Orchestrator) ExpireAttentions(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.expireAttentionsLocked(ctx)
}

func (o *Orchestrator) expireAttentionsLocked(ctx context.Context) (int, error) {
	attentions, err := o.cfg.Store.ListOpenAttentions(ctx, o.cfg.TenantID)
	if err != nil {
		return 0, err
	}
	now := o.cfg.Now()
	expired := 0
	for _, attention := range attentions {
		if attention.Deadline.IsZero() || attention.Deadline.After(now) {
			continue
		}
		attention.Status = collab.AttentionTimedOut
		if _, err := o.cfg.Store.UpdateAttention(ctx, attention); err != nil {
			return expired, err
		}
		expired++
		o.audit(ctx, AuditAttentionExpire, "accepted", Event{}, attention.WorkItemID, "attention "+attention.ID.String())
	}
	if expired > 0 {
		o.changed(o.cfg.TenantID)
	}
	return expired, nil
}

// Reconcile projects the core SessionIndex onto Assignments. It is a repair
// pass, not a second scheduler: a worker session the Host already marked done
// or cancelled stops occupying a durable slot. Missing session rows are left to
// Recover (a restart), because another process may legitimately own a live
// session handle.
func (o *Orchestrator) Reconcile(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reconcileLocked(ctx)
}

func (o *Orchestrator) reconcileLocked(ctx context.Context) (int, error) {
	if o.cfg.Sessions == nil {
		return 0, nil
	}
	assignments, err := o.cfg.Store.ListAssignmentsByStatus(ctx, o.cfg.TenantID, collab.AssignmentOpen, collab.AssignmentActive)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, assignment := range assignments {
		if assignment.SessionID == uuid.Nil {
			continue
		}
		ref, err := o.cfg.Sessions.Session(ctx, o.cfg.TenantID, assignment.SessionID)
		if err != nil {
			continue
		}
		if ref.State != collab.SessionDone && ref.State != collab.SessionCancelled {
			continue
		}
		ticket, err := o.loadTicket(ctx, o.cfg.TenantID, assignment.WorkItemID)
		if err != nil {
			continue
		}
		if ref.State == collab.SessionCancelled {
			assignment.Status = collab.AssignmentCancelled
			assignment.Error = "worker session was cancelled; slot released"
			if ticket.Status == collab.WorkItemActive {
				_ = o.setTicketStatus(ctx, &ticket, collab.WorkItemOpen, "worker session cancelled")
			}
		} else {
			assignment.Status = collab.AssignmentDone
			assignment.Error = "worker session ended without a report"
			if ticket.Status == collab.WorkItemActive || ticket.Status == collab.WorkItemOpen {
				_ = o.setTicketStatus(ctx, &ticket, collab.WorkItemBlocked, "worker session ended without a report")
			}
		}
		assignment.UpdatedAt = o.cfg.Now()
		if _, err := o.cfg.Store.UpdateAssignment(ctx, assignment); err != nil {
			return changed, err
		}
		changed++
		o.audit(ctx, AuditRecovered, "accepted", Event{}, ticket.ID, assignment.Error)
	}
	if changed > 0 {
		o.changed(o.cfg.TenantID)
	}
	return changed, nil
}

// Recover repairs work interrupted by a restart: an open Assignment is bound to
// its Worker session, a dead running Assignment is marked interrupted and the
// Ticket gets a new Assignment with a recovery note. Source cursors are durable,
// so already-consumed events are not replayed into new tickets.
func (o *Orchestrator) Recover(ctx context.Context) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	recovered := 0
	if _, err := o.expireAttentionsLocked(ctx); err != nil {
		return 0, err
	}
	// Recover intentionally does not run the runtime reconciler: a stale
	// cancelled session must fail its assignment with a recovery note and
	// redispatch, not be silently cancelled by the housekeeping projection.
	if o.cfg.Outbox != nil {
		if n, err := o.cfg.Outbox.RecoverOutbox(ctx, o.cfg.TenantID, 100); err != nil {
			return 0, err
		} else {
			recovered += n
		}
	}
	assignments, err := o.cfg.Store.ListAssignmentsByStatus(ctx, o.cfg.TenantID, collab.AssignmentOpen, collab.AssignmentActive)
	if err != nil {
		return recovered, err
	}
	for _, assignment := range assignments {
		ticket, err := o.loadTicket(ctx, o.cfg.TenantID, assignment.WorkItemID)
		if err != nil {
			assignment.Status = collab.AssignmentFailed
			assignment.Error = "ticket not found during recovery"
			assignment.UpdatedAt = o.cfg.Now()
			_, _ = o.cfg.Store.UpdateAssignment(ctx, assignment)
			continue
		}
		if ticket.Status == collab.WorkItemCancelled || ticket.Status == collab.WorkItemCancelling || ticket.Mandate.Revoked() {
			assignment.Status = collab.AssignmentCancelled
			assignment.Error = "ticket was stopped"
			assignment.UpdatedAt = o.cfg.Now()
			_, _ = o.cfg.Store.UpdateAssignment(ctx, assignment)
			continue
		}
		if assignment.SessionID == uuid.Nil {
			if err := o.auditRequired(ctx, AuditRecovered, "accepted", ticket.ID, "bind missing worker session"); err != nil {
				return recovered, err
			}
			if _, err := o.bindWorkerSession(ctx, ticket, assignment); err != nil {
				o.cfg.Logger.Warn("superagent: recovery bind failed", "assignment", assignment.ID, "error", err)
				continue
			}
			recovered++
			continue
		}
		if o.cfg.Sessions != nil {
			ref, err := o.cfg.Sessions.Session(ctx, o.cfg.TenantID, assignment.SessionID)
			if err != nil || ref.State != collab.SessionActive {
				n, err := o.failAndRedispatch(ctx, ticket, assignment, "worker session was lost; assignment restarted with a recovery note")
				if err != nil {
					return recovered, err
				}
				recovered += n
				continue
			}
			if o.cfg.WorkerInspector != nil {
				state, inspectErr := o.cfg.WorkerInspector.WorkerState(ctx, o.cfg.TenantID, assignment.SessionID)
				if inspectErr != nil {
					return recovered, inspectErr
				}
				switch state {
				case WorkerActive:
					// The run is genuinely live; leave the assignment alone.
				case WorkerDone:
					assignment.Status = collab.AssignmentDone
					assignment.Error = "worker run completed without a report"
					assignment.UpdatedAt = o.cfg.Now()
					if err := o.auditRequired(ctx, AuditRecovered, "accepted", ticket.ID, assignment.Error); err != nil {
						return recovered, err
					}
					if _, err := o.cfg.Store.UpdateAssignment(ctx, assignment); err != nil {
						return recovered, err
					}
					if ticket.Status == collab.WorkItemActive {
						_ = o.setTicketStatus(ctx, &ticket, collab.WorkItemBlocked, assignment.Error)
					}
					recovered++
				default:
					n, err := o.failAndRedispatch(ctx, ticket, assignment, "worker run is "+string(state)+"; assignment restarted with a recovery note")
					if err != nil {
						return recovered, err
					}
					recovered += n
				}
			}
		}
	}
	if _, err := o.dispatchQueuedLocked(ctx); err != nil {
		return recovered, err
	}
	if recovered > 0 {
		o.changed(o.cfg.TenantID)
	}
	return recovered, nil
}

func (o *Orchestrator) failAndRedispatch(ctx context.Context, ticket Ticket, assignment collab.Assignment, note string) (int, error) {
	assignment.Status = collab.AssignmentFailed
	assignment.Error = note
	assignment.UpdatedAt = o.cfg.Now()
	if err := o.auditRequired(ctx, AuditRecovered, "accepted", ticket.ID, note); err != nil {
		return 0, err
	}
	if _, err := o.cfg.Store.UpdateAssignment(ctx, assignment); err != nil {
		return 0, err
	}
	if ticket.Status == collab.WorkItemActive || ticket.Status == collab.WorkItemBlocked {
		if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemOpen, "worker lost after restart"); err != nil {
			return 0, err
		}
	}
	human := assignmentDocOf(assignment).HumanFollowUp
	if _, err := o.dispatchTicketLocked(ctx, ticket, human, "recovered after restart: "+note, ""); err != nil {
		o.cfg.Logger.Warn("superagent: recovery dispatch failed", "ticket", ticket.ID, "error", err)
		return 0, nil
	}
	return 1, nil
}

// ReportState is the closed worker report vocabulary. "waiting" requires a
// structured Attention id: a free-text "I am waiting" is never inferred.
type ReportState string

const (
	ReportProgress ReportState = "progress"
	ReportWaiting  ReportState = "waiting"
	ReportReady    ReportState = "ready"
	ReportBlocked  ReportState = "blocked"
	ReportFailed   ReportState = "failed"
	ReportDropped  ReportState = "dropped"
)

// Valid reports whether the state is in the closed vocabulary.
func (s ReportState) Valid() bool {
	switch s {
	case ReportProgress, ReportWaiting, ReportReady, ReportBlocked, ReportFailed, ReportDropped:
		return true
	default:
		return false
	}
}

// ReportRequest is one structured worker report. It is not a free-text status
// string: waiting/blocked must name the Attention that asks the human.
type ReportRequest struct {
	TenantID     uuid.UUID `json:"tenantId"`
	WorkItemID   uuid.UUID `json:"workItemId"`
	AssignmentID uuid.UUID `json:"assignmentId"`
	// CallerSessionID, when set, must be the assignment's worker session. The
	// HTTP worker route sends it so a caller cannot report for another ticket.
	CallerSessionID uuid.UUID       `json:"callerSessionId,omitempty"`
	State           ReportState     `json:"state"`
	Summary         string          `json:"summary,omitempty"`
	AttentionID     uuid.UUID       `json:"attentionId,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	// RequestID is the durable idempotency key of the report. A retried call
	// with the same key returns the current state without a second audit,
	// timeline row or terminal side effect.
	RequestID string `json:"requestId,omitempty"`
}

// Report applies a worker report. Ready/failed are Ticket decisions; a report
// never silently changes another tenant's row.
func (o *Orchestrator) Report(ctx context.Context, request ReportRequest) error {
	_, err := o.report(ctx, request, false)
	return err
}

// report applies one report and returns the resulting ticket/assignment plus
// the stored report fact. The model-facing worker path passes
// requireOpen=true: a blocked or closed ticket refuses the report instead of
// applying a partial transition.
//
// A report with a requestId is a two-phase durable claim:
//
//  1. the claim row is inserted (assignment+session+request id scoped);
//  2. its audit row is written and marked on the claim;
//  3. the state transition is applied and marked on the claim.
//
// A retry resumes only the missing phases, so a crash between them can neither
// duplicate the audit nor strand the state. A completed claim returns its
// stored state and does not re-audit or re-apply.
func (o *Orchestrator) report(ctx context.Context, request ReportRequest, requireOpen bool) (WorkerReportOutcome, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(request.TenantID); err != nil {
		return WorkerReportOutcome{}, err
	}
	if !request.State.Valid() {
		return WorkerReportOutcome{}, fmt.Errorf("superagent: report state %q is not in the closed vocabulary", request.State)
	}
	ticket, err := o.loadTicket(ctx, request.TenantID, request.WorkItemID)
	if err != nil {
		return WorkerReportOutcome{}, err
	}
	assignment, err := o.cfg.Store.GetAssignment(ctx, request.TenantID, request.AssignmentID)
	if err != nil {
		return WorkerReportOutcome{}, err
	}
	if assignment.WorkItemID != ticket.ID {
		return WorkerReportOutcome{}, fmt.Errorf("superagent: assignment does not belong to the ticket")
	}
	if request.CallerSessionID != uuid.Nil && request.CallerSessionID != assignment.SessionID {
		return WorkerReportOutcome{}, fmt.Errorf("%w: the caller session is not the assignment worker", collab.ErrPermissionDenied)
	}
	requestID := strings.TrimSpace(request.RequestID)
	if requestID == "" {
		if err := o.ensureReportPermitted(ctx, request, ticket, assignment, requireOpen); err != nil {
			return WorkerReportOutcome{}, err
		}
		if err := o.auditRequiredWorker(ctx, AuditReport, "accepted", ticket, assignment, "state="+string(request.State)); err != nil {
			return WorkerReportOutcome{}, err
		}
		ticket, assignment, err = o.applyReportLocked(ctx, request, ticket, assignment)
		if err != nil {
			return WorkerReportOutcome{}, err
		}
		o.changed(request.TenantID)
		return WorkerReportOutcome{Ticket: ticket, Assignment: assignment, Created: true}, nil
	}
	sessionID := request.CallerSessionID
	if sessionID == uuid.Nil {
		sessionID = assignment.SessionID
	}
	if sessionID == uuid.Nil {
		return WorkerReportOutcome{}, fmt.Errorf("superagent: a report requestId needs a worker session")
	}
	stored, err := o.cfg.Store.GetWorkerReport(ctx, request.TenantID, ticket.ID, assignment.ID, sessionID, requestID)
	existed := err == nil
	if err != nil && !errors.Is(err, ErrReportNotFound) {
		return WorkerReportOutcome{}, err
	}
	effective := request
	created := false
	if existed {
		effective = reportRequestFromStored(stored)
	}
	if !existed {
		if err := o.ensureReportPermitted(ctx, effective, ticket, assignment, requireOpen); err != nil {
			return WorkerReportOutcome{}, err
		}
		claim := WorkerReport{
			ID:           uuid.New(),
			TenantID:     request.TenantID,
			WorkItemID:   ticket.ID,
			AssignmentID: assignment.ID,
			SessionID:    sessionID,
			RequestID:    requestID,
			State:        effective.State,
			Summary:      strings.TrimSpace(effective.Summary),
			AttentionID:  effective.AttentionID,
			Result:       append(json.RawMessage(nil), effective.Result...),
			CreatedAt:    o.cfg.Now(),
		}
		stored, created, err = o.cfg.Store.ClaimWorkerReport(ctx, claim)
		if err != nil {
			return WorkerReportOutcome{}, err
		}
		if !created {
			// Another process claimed the same scoped key first. Its stored
			// payload is the fact; re-check the guards against that payload.
			existed = true
			effective = reportRequestFromStored(stored)
			if err := o.ensureReportPermitted(ctx, effective, ticket, assignment, requireOpen); err != nil {
				return WorkerReportOutcome{}, err
			}
		}
	}
	if existed {
		// A previous attempt may have stopped after the claim. Complete only the
		// missing phases, in order.
		appliedNow := false
		if stored.AuditedAt == nil {
			if err := o.auditRequiredWorker(ctx, AuditReport, "accepted", ticket, assignment, "state="+string(stored.State)); err != nil {
				return WorkerReportOutcome{}, err
			}
			at := o.cfg.Now()
			marked, err := o.cfg.Store.MarkWorkerReportAudited(ctx, o.cfg.TenantID, stored.ID, at)
			if err != nil {
				return WorkerReportOutcome{}, err
			}
			if marked {
				stored.AuditedAt = &at
			}
		}
		if stored.AppliedAt == nil {
			if err := o.ensureReportPermitted(ctx, effective, ticket, assignment, requireOpen); err != nil {
				return WorkerReportOutcome{}, err
			}
			ticket, assignment, err = o.applyReportLocked(ctx, effective, ticket, assignment)
			if err != nil {
				return WorkerReportOutcome{}, err
			}
			appliedNow = true
			at := o.cfg.Now()
			marked, err := o.cfg.Store.MarkWorkerReportApplied(ctx, o.cfg.TenantID, stored.ID, at)
			if err != nil {
				return WorkerReportOutcome{}, err
			}
			if marked {
				stored.AppliedAt = &at
			}
		}
		if appliedNow {
			o.changed(request.TenantID)
		}
		return WorkerReportOutcome{Ticket: ticket, Assignment: assignment, Report: stored, Created: created}, nil
	}
	// This call created the claim. Audit it, apply the state and mark the claim.
	if err := o.auditRequiredWorker(ctx, AuditReport, "accepted", ticket, assignment, "state="+string(effective.State)); err != nil {
		return WorkerReportOutcome{}, err
	}
	at := o.cfg.Now()
	marked, err := o.cfg.Store.MarkWorkerReportAudited(ctx, o.cfg.TenantID, stored.ID, at)
	if err != nil {
		return WorkerReportOutcome{}, err
	}
	if marked {
		stored.AuditedAt = &at
	}
	ticket, assignment, err = o.applyReportLocked(ctx, effective, ticket, assignment)
	if err != nil {
		return WorkerReportOutcome{}, err
	}
	appliedAt := o.cfg.Now()
	if marked, err := o.cfg.Store.MarkWorkerReportApplied(ctx, o.cfg.TenantID, stored.ID, appliedAt); err != nil {
		return WorkerReportOutcome{}, err
	} else if marked {
		stored.AppliedAt = &appliedAt
	}
	o.changed(request.TenantID)
	return WorkerReportOutcome{Ticket: ticket, Assignment: assignment, Report: stored, Created: true}, nil
}

// reportRequestFromStored projects an idempotency claim onto the request the
// state transition consumes. A retry may carry different text; the stored fact
// wins exactly like the HTTP idempotency key semantics.
func reportRequestFromStored(stored WorkerReport) ReportRequest {
	return ReportRequest{
		TenantID:        stored.TenantID,
		WorkItemID:      stored.WorkItemID,
		AssignmentID:    stored.AssignmentID,
		State:           stored.State,
		Summary:         stored.Summary,
		AttentionID:     stored.AttentionID,
		Result:          stored.Result,
		CallerSessionID: stored.SessionID,
		RequestID:       stored.RequestID,
	}
}

// ensureReportPermitted applies the shared guards: a terminal assignment is a
// stale worker, the model path refuses blocked/closed tickets, and a waiting
// report must name an open Attention owned by this assignment/session.
func (o *Orchestrator) ensureReportPermitted(ctx context.Context, request ReportRequest, ticket Ticket, assignment collab.Assignment, requireOpen bool) error {
	if assignment.Status.Terminal() && !reportAlreadyApplied(ticket, assignment, request.State) {
		return &WorkerToolError{
			Code:         WorkerToolTicketClosed,
			Message:      "the worker assignment has already ended; no further report is accepted",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	if requireOpen {
		if refusal := openTicketRefusal(ticket); refusal != nil && !reportAlreadyApplied(ticket, assignment, request.State) {
			return refusal
		}
	}
	if request.State != ReportWaiting && request.State != ReportBlocked {
		return nil
	}
	if request.AttentionID == uuid.Nil {
		if request.State == ReportWaiting {
			return fmt.Errorf("superagent: a waiting report must name a structured attention")
		}
		return nil
	}
	attention, err := o.cfg.Store.GetAttention(ctx, request.TenantID, request.AttentionID)
	if err != nil {
		return err
	}
	if attention.WorkItemID != ticket.ID {
		return &WorkerToolError{
			Code:         WorkerToolAttentionInvalid,
			Message:      "the attention belongs to a different ticket",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	caller := request.CallerSessionID
	if caller == uuid.Nil {
		caller = assignment.SessionID
	}
	if attention.AssignmentID != assignment.ID || (attention.SessionID != uuid.Nil && attention.SessionID != caller) {
		return &WorkerToolError{
			Code:         WorkerToolAttentionInvalid,
			Message:      "the attention belongs to a different worker assignment or session",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	if attention.Status != collab.AttentionOpen {
		return &WorkerToolError{
			Code:         WorkerToolAttentionInvalid,
			Message:      "the attention is not open; it was answered, timed out or cancelled",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	return nil
}

// applyReportLocked applies the state transition and reloads the resulting
// ticket/assignment. It performs no audit and no idempotency bookkeeping.
func (o *Orchestrator) applyReportLocked(ctx context.Context, request ReportRequest, ticket Ticket, assignment collab.Assignment) (Ticket, collab.Assignment, error) {
	switch request.State {
	case ReportProgress:
		// A progress report has no state edge; the durable report claim is its
		// fact.
	case ReportWaiting, ReportBlocked:
		if ticket.Status == collab.WorkItemActive || ticket.Status == collab.WorkItemOpen {
			if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemBlocked, request.Summary); err != nil {
				return Ticket{}, collab.Assignment{}, err
			}
		}
	case ReportReady:
		if err := o.finishAssignment(ctx, assignment, collab.AssignmentDone, request.Result); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
		if ticket.Status != collab.WorkItemDone {
			if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemDone, request.Summary); err != nil {
				return Ticket{}, collab.Assignment{}, err
			}
			o.enqueueTerminal(ctx, ticket, "completed", request.Summary)
		}
		if _, err := o.dispatchQueuedLocked(ctx); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
	case ReportFailed:
		if err := o.finishAssignment(ctx, assignment, collab.AssignmentFailed, request.Result); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
		if ticket.Status != collab.WorkItemFailed && ticket.Status != collab.WorkItemCancelled {
			if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemFailed, request.Summary); err != nil {
				return Ticket{}, collab.Assignment{}, err
			}
			o.enqueueTerminal(ctx, ticket, "failed", request.Summary)
		}
		if _, err := o.dispatchQueuedLocked(ctx); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
	case ReportDropped:
		if err := o.finishAssignment(ctx, assignment, collab.AssignmentCancelled, request.Result); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
		if ticket.Status == collab.WorkItemActive {
			if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemOpen, "worker dropped the assignment"); err != nil {
				return Ticket{}, collab.Assignment{}, err
			}
		}
		if _, err := o.dispatchQueuedLocked(ctx); err != nil {
			return Ticket{}, collab.Assignment{}, err
		}
	}
	// Reload so the caller sees the applied assignment/ticket state.
	ticket, err := o.loadTicket(ctx, request.TenantID, ticket.ID)
	if err != nil {
		return Ticket{}, collab.Assignment{}, err
	}
	assignment, err = o.cfg.Store.GetAssignment(ctx, request.TenantID, assignment.ID)
	if err != nil {
		return Ticket{}, collab.Assignment{}, err
	}
	return ticket, assignment, nil
}

// reportAlreadyApplied reports whether the requested report state is already
// the ticket/assignment fact. It is the retry allowance after a state change
// landed but its report row did not.
func reportAlreadyApplied(ticket Ticket, assignment collab.Assignment, state ReportState) bool {
	switch state {
	case ReportReady:
		return assignment.Status == collab.AssignmentDone
	case ReportFailed:
		return assignment.Status == collab.AssignmentFailed
	case ReportDropped:
		return assignment.Status == collab.AssignmentCancelled
	case ReportWaiting, ReportBlocked:
		return !assignment.Status.Terminal() && ticket.Status == collab.WorkItemBlocked
	default:
		return false
	}
}

// openTicketRefusal is the model-tool boundary: work is only reportable while
// the ticket accepts it. A blocked ticket is waiting on a human, and a closed
// ticket (or a revoked mandate) must never be resurrected by a report.
func openTicketRefusal(ticket Ticket) *WorkerToolError {
	if ticket.Status == collab.WorkItemBlocked {
		return &WorkerToolError{
			Code:         WorkerToolTicketBlocked,
			Message:      "the ticket is blocked on a human decision; wait for the answer before reporting",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	if ticket.Status.Closed() || ticket.Mandate.Revoked() {
		return &WorkerToolError{
			Code:         WorkerToolTicketClosed,
			Message:      "the ticket is closed and no longer accepts worker reports",
			TicketID:     ticket.ID,
			TicketStatus: ticket.Status,
		}
	}
	return nil
}

func (o *Orchestrator) finishAssignment(ctx context.Context, assignment collab.Assignment, status collab.AssignmentStatus, result json.RawMessage) error {
	assignment.Status = status
	assignment.Result = append(json.RawMessage(nil), result...)
	assignment.UpdatedAt = o.cfg.Now()
	if _, err := o.cfg.Store.UpdateAssignment(ctx, assignment); err != nil {
		return err
	}
	if assignment.SessionID != uuid.Nil && o.cfg.Sessions != nil {
		_ = o.cfg.Sessions.SetState(ctx, o.cfg.TenantID, assignment.SessionID, collab.SessionDone)
	}
	return nil
}

// Ask opens one structured Attention through the guarded Host. The question is
// routed back to the Ticket thread, never to an unrelated Channel.
func (o *Orchestrator) Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error) {
	attention, _, err := o.ask(ctx, ticketID, assignmentID, request, false)
	return attention, err
}

// ask is the one Ask implementation. requireOpen is set by the model-facing
// worker tool: a blocked or closed ticket refuses a new question instead of
// opening a second Attention the human cannot usefully answer.
func (o *Orchestrator) ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest, requireOpen bool) (collab.Attention, Ticket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(o.cfg.TenantID); err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	ticket, err := o.loadTicket(ctx, o.cfg.TenantID, ticketID)
	if err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	if request.Route == "" {
		request.Route = collab.AttentionRouteChannel
	}
	if !ticket.Thread.Valid() {
		return collab.Attention{}, Ticket{}, fmt.Errorf("superagent: ticket has no source thread")
	}
	if request.Channel != "" && request.Channel != ticket.Thread.ID {
		return collab.Attention{}, Ticket{}, fmt.Errorf("superagent: attention channel %q is not the ticket thread %q", request.Channel, ticket.Thread.ID)
	}
	request.Channel = ticket.Thread.ID
	if request.Timeout == 0 {
		request.Timeout = o.cfg.AttentionTimeout
	}
	assignment, err := o.cfg.Store.GetAssignment(ctx, o.cfg.TenantID, assignmentID)
	if err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	if assignment.SessionID == uuid.Nil {
		return collab.Attention{}, Ticket{}, fmt.Errorf("superagent: assignment has no worker session")
	}
	if request.CallerSessionID != uuid.Nil && request.CallerSessionID != assignment.SessionID {
		return collab.Attention{}, Ticket{}, fmt.Errorf("%w: the caller session is not the assignment worker", collab.ErrPermissionDenied)
	}
	callerSession := assignment.SessionID
	if request.CallerSessionID != uuid.Nil {
		callerSession = request.CallerSessionID
	}
	// The requestId is scoped to this assignment/session. A retried question
	// returns the same Attention; a stale worker reusing an id from another
	// assignment gets a conflict instead of its predecessor's question.
	if requestID := strings.TrimSpace(request.RequestID); requestID != "" {
		attentions, err := o.cfg.Store.ListAttentions(ctx, o.cfg.TenantID, ticket.ID)
		if err != nil {
			return collab.Attention{}, Ticket{}, err
		}
		for _, existing := range attentions {
			if existing.RequestID != requestID {
				continue
			}
			if existing.AssignmentID == assignment.ID &&
				(existing.SessionID == uuid.Nil || existing.SessionID == callerSession) {
				return existing, ticket, nil
			}
			if requireOpen {
				return collab.Attention{}, Ticket{}, &WorkerToolError{
					Code:         WorkerToolInvalidRequest,
					Message:      "the requestId already belongs to another worker assignment; use a new requestId",
					TicketID:     ticket.ID,
					TicketStatus: ticket.Status,
				}
			}
			return collab.Attention{}, Ticket{}, fmt.Errorf("%w: the requestId already belongs to another worker assignment", collab.ErrAttentionConflict)
		}
	}
	if requireOpen {
		if assignment.Status.Terminal() {
			return collab.Attention{}, Ticket{}, &WorkerToolError{
				Code:         WorkerToolTicketClosed,
				Message:      "the worker assignment has already ended; no new question is accepted",
				TicketID:     ticket.ID,
				TicketStatus: ticket.Status,
			}
		}
		if refusal := openTicketRefusal(ticket); refusal != nil {
			return collab.Attention{}, Ticket{}, refusal
		}
	}
	if err := o.auditRequiredWorker(ctx, AuditAttentionOpen, "accepted", ticket, assignment, "attention requested"); err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	request.CallerSessionID = callerSession
	request.AssignmentID = assignment.ID
	attention, err := o.cfg.Host.Ask(ctx, o.cfg.TenantID, ticketID, request)
	if err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	o.changed(o.cfg.TenantID)
	return attention, ticket, nil
}

// AnswerAttention records a human answer and wakes the same Ticket. A stopped
// Ticket is never reopened: its Mandate was revoked. Operator answers require
// the independent OperatorAuthorizer; a policy-allowed Source comment uses
// answerAttentionLocked directly.
func (o *Orchestrator) AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(tenantID); err != nil {
		return collab.Attention{}, err
	}
	attention, err := o.cfg.Store.GetAttention(ctx, tenantID, attentionID)
	if err != nil {
		return collab.Attention{}, err
	}
	if err := o.requireOperator(ctx, tenantID, attention.WorkItemID, actor, "answer"); err != nil {
		return collab.Attention{}, err
	}
	answered, err := o.answerAttentionLocked(ctx, tenantID, attention, optionID, text, actor)
	if err != nil {
		return collab.Attention{}, err
	}
	o.changed(tenantID)
	return answered, nil
}

func (o *Orchestrator) answerAttentionLocked(ctx context.Context, tenantID uuid.UUID, attention collab.Attention, optionID, text, actor string) (collab.Attention, error) {
	optionID = strings.TrimSpace(optionID)
	text = strings.TrimSpace(text)
	if attention.Status == collab.AttentionAnswered && sameAnswer(attention.Answer, optionID, text) {
		// A retried answer is one fact: return the original result without
		// reopening the Ticket or dispatching another Worker.
		return attention, nil
	}
	ticket, err := o.loadTicket(ctx, tenantID, attention.WorkItemID)
	if err != nil {
		return collab.Attention{}, err
	}
	if ticket.Status == collab.WorkItemCancelled || ticket.Status == collab.WorkItemCancelling || ticket.Mandate.Revoked() {
		return collab.Attention{}, fmt.Errorf("%w: the ticket was stopped", collab.ErrWorkItemClosed)
	}
	if err := o.auditRequired(ctx, AuditAttentionAnswer, "accepted", ticket.ID, "attention "+attention.ID.String()+" by "+actor); err != nil {
		return collab.Attention{}, err
	}
	answered, err := o.cfg.Host.AnswerAttention(ctx, tenantID, ticket.ID, attention.ID, collab.AttentionAnswerRequest{
		OptionID: optionID, Text: text, Actor: actor,
	})
	if err != nil {
		return collab.Attention{}, err
	}
	if ticket.Status == collab.WorkItemBlocked {
		if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemActive, "attention answered by "+actor); err != nil {
			return collab.Attention{}, err
		}
	}
	assignment, err := o.activeAssignment(ctx, ticket.ID)
	if err != nil {
		return collab.Attention{}, err
	}
	answerText := attentionAnswerText(answered)
	if o.cfg.Outbox != nil {
		_, _ = o.cfg.Outbox.Enqueue(ctx, OutboxMessage{
			TenantID: tenantID, WorkItemID: ticket.ID, ChannelID: ticket.Thread.ID,
			Thread: ticket.Thread, Text: answerText,
			Data:           marshalData(map[string]any{"ticketId": ticket.ID.String(), "event": "attention_answered"}),
			IdempotencyKey: "attention:" + answered.ID.String() + ":answer",
		})
	}
	if assignment.ID != uuid.Nil && assignment.SessionID != uuid.Nil {
		if err := o.cfg.Inputs.Deliver(ctx, InputRequest{
			TenantID: tenantID, WorkItemID: ticket.ID, AssignmentID: assignment.ID,
			SessionID: assignment.SessionID, Text: answerText,
			RequestID: "attention:" + answered.ID.String(), Kind: "attention_answer",
		}); err != nil {
			return collab.Attention{}, err
		}
	} else {
		if _, err := o.dispatchTicketLocked(ctx, ticket, true, "attention answered by "+actor, answerText); err != nil {
			return collab.Attention{}, err
		}
	}
	return answered, nil
}

func sameAnswer(answer *collab.AttentionAnswer, optionID, text string) bool {
	return answer != nil && answer.OptionID == optionID && answer.Text == text
}

// ResumeTicket is the explicit human follow-up path: a finished or blocked
// Ticket reopens and gets a human-follow-up Assignment if no worker is live.
// It is deliberately separate from subscription fan-out, which never resumes a
// finished Ticket automatically. A stopped Ticket stays stopped because its
// Mandate was revoked.
func (o *Orchestrator) ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (Ticket, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(tenantID); err != nil {
		return Ticket{}, err
	}
	if err := o.requireOperator(ctx, tenantID, ticketID, actor, "resume"); err != nil {
		return Ticket{}, err
	}
	ticket, err := o.loadTicket(ctx, tenantID, ticketID)
	if err != nil {
		return Ticket{}, err
	}
	if ticket.Mandate.Revoked() || ticket.Status == collab.WorkItemCancelled || ticket.Status == collab.WorkItemCancelling {
		return Ticket{}, fmt.Errorf("%w: the ticket was stopped", collab.ErrWorkItemClosed)
	}
	if err := o.auditRequired(ctx, AuditTicketResumed, "accepted", ticket.ID, reason); err != nil {
		return Ticket{}, err
	}
	switch ticket.Status {
	case collab.WorkItemDone, collab.WorkItemFailed, collab.WorkItemBlocked:
		if err := o.setTicketStatus(ctx, &ticket, collab.WorkItemOpen, "human follow-up by "+actor); err != nil {
			return Ticket{}, err
		}
	case collab.WorkItemOpen, collab.WorkItemActive:
		// Already live; the caller may still want a human-follow-up assignment.
	default:
		return Ticket{}, fmt.Errorf("superagent: ticket %s cannot resume from %s", ticketID, ticket.Status)
	}
	if _, err := o.dispatchTicketLocked(ctx, ticket, true, "human follow-up: "+reason, reason); err != nil && !errors.Is(err, collab.ErrConcurrencyExceeded) {
		return Ticket{}, err
	}
	resumed, err := o.loadTicket(ctx, tenantID, ticketID)
	if err != nil {
		return Ticket{}, err
	}
	o.changed(tenantID)
	return resumed, nil
}

// StopTicket applies a human stop: the Mandate is revoked, the Ticket cannot
// accept automatic input again, and every bound session/attention is cancelled
// through the guarded Host. The records stay auditable.
func (o *Orchestrator) StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.checkTenant(tenantID); err != nil {
		return err
	}
	if err := o.requireOperator(ctx, tenantID, ticketID, actor, "stop"); err != nil {
		return err
	}
	return o.stopTicketLocked(ctx, ticketID, Actor{ID: actor, Login: actor}, reason, Event{})
}

func (o *Orchestrator) stopTicketLocked(ctx context.Context, ticketID uuid.UUID, actor Actor, reason string, event Event) error {
	if err := o.checkTenant(o.cfg.TenantID); err != nil {
		return err
	}
	ticket, err := o.loadTicket(ctx, o.cfg.TenantID, ticketID)
	if err != nil {
		return err
	}
	if ticket.Status == collab.WorkItemCancelled {
		return nil
	}
	now := o.cfg.Now()
	ticket.Mandate.RevokedAt = &now
	ticket.Mandate.RevokedBy = actor.Login
	if ticket.Mandate.RevokedBy == "" {
		ticket.Mandate.RevokedBy = actor.ID
	}
	ticket.Mandate.RevocationCause = reason
	if o.cfg.WorkerTreeCanceller != nil {
		assignments, err := o.cfg.Store.ListAssignments(ctx, o.cfg.TenantID, ticketID)
		if err != nil {
			return err
		}
		for _, assignment := range assignments {
			if assignment.Status.Terminal() || assignment.SessionID == uuid.Nil {
				continue
			}
			if err := o.cfg.WorkerTreeCanceller.CancelTree(ctx, o.cfg.TenantID, assignment.SessionID, reason); err != nil {
				return err
			}
		}
	}
	if err := o.auditRequired(ctx, AuditTicketStopped, "accepted", ticket.ID, reason); err != nil {
		return err
	}
	// Cancellation fan-out (including every bound worker session and its
	// subtree) is the core Host's contract. This method never enumerates or
	// cancels sessions itself, so a core fix for bare children applies here
	// without a second implementation.
	if err := o.cfg.Host.Cancel(ctx, o.cfg.TenantID, ticketID, reason); err != nil {
		// The Host leaves the item in "cancelling" when propagation fails; keep
		// the mandate revocation fact so a retry cannot resurrect the ticket.
		ticket.Status = collab.WorkItemCancelling
		ticket.UpdatedAt = now
		_, _ = o.cfg.Store.UpdateWorkItem(ctx, ticket.Record())
		return err
	}
	subs, err := o.cfg.Store.ListSubscriptions(ctx, o.cfg.TenantID, ticketID)
	if err != nil {
		return err
	}
	for _, subscription := range subs {
		_ = o.deactivateSubscription(ctx, subscription)
	}
	ticket.Status = collab.WorkItemCancelled
	ticket.UpdatedAt = now
	if _, err := o.cfg.Store.UpdateWorkItem(ctx, ticket.Record()); err != nil {
		return err
	}
	o.changed(o.cfg.TenantID)
	return nil
}

// Board is the read-only ticket board projection.
type BoardFilter struct {
	Status collab.WorkItemStatus `json:"status,omitempty"`
	// Limit bounds the newest tickets (updated-desc head), not the oldest tail.
	Limit int `json:"limit,omitempty"`
}

// TicketView is one board row.
type TicketView struct {
	Ticket
	AssignmentCount    int                `json:"assignmentCount"`
	ActiveAssignment   *collab.Assignment `json:"activeAssignment,omitempty"`
	AttentionCount     int                `json:"attentionCount"`
	OpenAttentionCount int                `json:"openAttentionCount"`
}

// BoardView is the board response.
type BoardView struct {
	TenantID uuid.UUID      `json:"tenantId"`
	ModeID   string         `json:"modeId"`
	Tickets  []TicketView   `json:"tickets"`
	Counts   map[string]int `json:"counts"`
}

// Board lists the tenant's tickets, most recently updated first (bounded by
// filter limit). The limit is applied to the head of that order: a small board
// shows the newest activity, never the oldest tail.
func (o *Orchestrator) Board(ctx context.Context, tenantID uuid.UUID, filter BoardFilter) (BoardView, error) {
	if err := o.checkTenant(tenantID); err != nil {
		return BoardView{}, err
	}
	items, err := o.cfg.Store.ListWorkItems(ctx, tenantID, collab.WorkFilter{
		ModeID: o.cfg.ModeID, Status: filter.Status, Order: collab.WorkOrderUpdatedDesc,
	})
	if err != nil {
		return BoardView{}, err
	}
	if filter.Limit > 0 && len(items) > filter.Limit {
		items = items[:filter.Limit]
	}
	view := BoardView{TenantID: tenantID, ModeID: o.cfg.ModeID, Tickets: make([]TicketView, 0, len(items)), Counts: map[string]int{}}
	for _, item := range items {
		ticket := TicketFromWorkItem(item)
		assignments, _ := o.cfg.Store.ListAssignments(ctx, tenantID, ticket.ID)
		attentions, _ := o.cfg.Store.ListAttentions(ctx, tenantID, ticket.ID)
		row := TicketView{Ticket: ticket, AssignmentCount: len(assignments), AttentionCount: len(attentions)}
		for i := range assignments {
			assignment := assignments[i]
			if assignment.Status == collab.AssignmentActive || assignment.Status == collab.AssignmentOpen {
				row.ActiveAssignment = &assignment
			}
		}
		for _, attention := range attentions {
			if attention.Status == collab.AttentionOpen {
				row.OpenAttentionCount++
			}
		}
		view.Tickets = append(view.Tickets, row)
		view.Counts[string(ticket.Status)]++
	}
	return view, nil
}

// AttentionView is one inbox row.
type AttentionView struct {
	collab.Attention
	TicketTitle string    `json:"ticketTitle,omitempty"`
	Thread      ThreadRef `json:"thread"`
}

// AttentionInbox is the read-only inbox response.
type AttentionInbox struct {
	TenantID uuid.UUID       `json:"tenantId"`
	Items    []AttentionView `json:"items"`
}

// Inbox lists open attention requests across the tenant's tickets.
func (o *Orchestrator) Inbox(ctx context.Context, tenantID uuid.UUID) (AttentionInbox, error) {
	if err := o.checkTenant(tenantID); err != nil {
		return AttentionInbox{}, err
	}
	attentions, err := o.cfg.Store.ListOpenAttentions(ctx, tenantID)
	if err != nil {
		return AttentionInbox{}, err
	}
	inbox := AttentionInbox{TenantID: tenantID, Items: make([]AttentionView, 0, len(attentions))}
	for _, attention := range attentions {
		ticket, err := o.loadTicket(ctx, tenantID, attention.WorkItemID)
		if err != nil {
			continue
		}
		inbox.Items = append(inbox.Items, AttentionView{Attention: attention, TicketTitle: ticket.Title, Thread: ticket.Thread})
	}
	return inbox, nil
}

func (o *Orchestrator) loadTicket(ctx context.Context, tenantID, ticketID uuid.UUID) (Ticket, error) {
	item, err := o.cfg.Store.GetWorkItem(ctx, tenantID, ticketID)
	if err != nil {
		return Ticket{}, err
	}
	if item.ModeID != o.cfg.ModeID {
		return Ticket{}, fmt.Errorf("%w: work item belongs to mode %q", collab.ErrInvalidRequest, item.ModeID)
	}
	return TicketFromWorkItem(item), nil
}

func (o *Orchestrator) setTicketStatus(ctx context.Context, ticket *Ticket, to collab.WorkItemStatus, reason string) error {
	if err := ValidateTransition(ticket.Status, to); err != nil {
		return err
	}
	ticket.Status = to
	ticket.UpdatedAt = o.cfg.Now()
	ticket.WorkItem = ticket.Record()
	stored, err := o.cfg.Store.UpdateWorkItem(ctx, ticket.Record())
	if err != nil {
		return err
	}
	*ticket = TicketFromWorkItem(stored)
	_ = reason
	return nil
}

func (o *Orchestrator) ensureSubscription(ctx context.Context, ticket Ticket) (Subscription, error) {
	return o.cfg.Store.CreateSubscription(ctx, Subscription{
		TenantID: o.cfg.TenantID, WorkItemID: ticket.ID, Thread: ticket.Thread, Active: true,
	})
}

func (o *Orchestrator) deactivateSubscription(ctx context.Context, subscription Subscription) error {
	subscription.Active = false
	_, err := o.cfg.Store.UpdateSubscription(ctx, subscription)
	return err
}

func (o *Orchestrator) activeAssignment(ctx context.Context, ticketID uuid.UUID) (collab.Assignment, error) {
	assignments, err := o.cfg.Store.ListAssignments(ctx, o.cfg.TenantID, ticketID)
	if err != nil {
		return collab.Assignment{}, err
	}
	for _, assignment := range assignments {
		if assignment.Status == collab.AssignmentActive || assignment.Status == collab.AssignmentOpen {
			return assignment, nil
		}
	}
	return collab.Assignment{}, nil
}

func (o *Orchestrator) recordSeen(ctx context.Context, event Event, decision string, ticketID uuid.UUID) error {
	return o.cfg.Store.RecordSourceEvent(ctx, SeenEvent{
		TenantID: o.cfg.TenantID, SourceID: event.SourceID, EventID: event.EventID,
		Kind: string(event.Kind), ActorID: event.Actor.ID, ThreadID: event.Thread.ID,
		Payload: event.Raw, Decision: decision, WorkItemID: ticketID,
		ReceivedAt: event.ReceivedAt, CreatedAt: o.cfg.Now(),
	})
}

func (o *Orchestrator) enqueueTerminal(ctx context.Context, ticket Ticket, event, summary string) {
	if o.cfg.Outbox == nil {
		return
	}
	text := fmt.Sprintf("Ticket %s %s", shortID(ticket.ID), event)
	if strings.TrimSpace(summary) != "" {
		text += ": " + strings.TrimSpace(summary)
	}
	_, _ = o.cfg.Outbox.Enqueue(ctx, OutboxMessage{
		TenantID: o.cfg.TenantID, WorkItemID: ticket.ID,
		ChannelID: ticket.Thread.ID, Thread: ticket.Thread, Text: text,
		Data:           marshalData(map[string]any{"ticketId": ticket.ID.String(), "event": event}),
		IdempotencyKey: "ticket:" + ticket.ID.String() + ":" + event,
	})
}

// auditRequired writes the audit row that authorizes an accepted state change.
// It is fail-closed: an accepted Ticket/Assignment/Attention/report cannot be
// created without its durable row.
func (o *Orchestrator) auditRequired(ctx context.Context, kind, result string, ticketID uuid.UUID, detail string) error {
	if o.cfg.Audit == nil {
		return fmt.Errorf("%w: no audit sink", collab.ErrAuditUnavailable)
	}
	err := o.cfg.Audit.Record(ctx, collab.AuditEvent{
		Kind: kind, Result: result, Operation: "superagent",
		TenantID: o.cfg.TenantID, ModeID: o.cfg.ModeID, WorkItemID: ticketID,
		BotID: o.cfg.OrchestratorBot, Detail: detail, At: o.cfg.Now(),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", collab.ErrAuditUnavailable, err)
	}
	return nil
}

// auditRequiredWorker is the worker-actor variant: the row names the worker
// session and Bot (actor), the ticket and the assignment, and carries only a
// structured action/detail. It never carries a report summary, an Attention
// prompt or a Source body.
func (o *Orchestrator) auditRequiredWorker(ctx context.Context, kind, result string, ticket Ticket, assignment collab.Assignment, detail string) error {
	if o.cfg.Audit == nil {
		return fmt.Errorf("%w: no audit sink", collab.ErrAuditUnavailable)
	}
	sessionID := assignment.SessionID
	if sessionID == uuid.Nil {
		sessionID = assignment.CallerSessionID
	}
	err := o.cfg.Audit.Record(ctx, collab.AuditEvent{
		Kind: kind, Result: result, Operation: "superagent",
		TenantID: o.cfg.TenantID, ModeID: o.cfg.ModeID, WorkItemID: ticket.ID,
		AssignmentID: assignment.ID, SessionID: sessionID, BotID: assignment.BotID,
		Detail: detail, At: o.cfg.Now(),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", collab.ErrAuditUnavailable, err)
	}
	return nil
}

func (o *Orchestrator) audit(ctx context.Context, kind, result string, event Event, ticketID uuid.UUID, detail string) {
	if o.cfg.Audit == nil {
		return
	}
	_ = o.cfg.Audit.Record(ctx, collab.AuditEvent{
		Kind: kind, Result: result, Reason: detail, Operation: "superagent",
		TenantID: o.cfg.TenantID, ModeID: o.cfg.ModeID, WorkItemID: ticketID,
		BotID: o.cfg.OrchestratorBot, Detail: detail, At: o.cfg.Now(),
	})
}

func (o *Orchestrator) requireOperator(ctx context.Context, tenantID, ticketID uuid.UUID, actor, action string) error {
	if o.cfg.Operator == nil {
		return ErrOperatorUnavailable
	}
	return o.cfg.Operator.AuthorizeOperator(ctx, tenantID, ticketID, actor, action)
}

func (o *Orchestrator) checkTenant(tenantID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant is required", collab.ErrTenantRequired)
	}
	if tenantID != o.cfg.TenantID {
		return fmt.Errorf("%w: the tenant does not own this orchestrator", collab.ErrTenantMismatch)
	}
	return nil
}

// changed publishes one invalidation fact after a successful Ticket/Attention
// write. A refused or failed write returns before this call, so the SSE stream
// never teaches a client that a rejected action changed something.
func (o *Orchestrator) changed(tenantID uuid.UUID) {
	if o == nil || o.cfg.Changed == nil || tenantID == uuid.Nil {
		return
	}
	o.cfg.Changed(tenantID)
}

type assignmentDoc struct {
	HumanFollowUp bool   `json:"humanFollowUp,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Context       string `json:"context,omitempty"`
}

func assignmentDocOf(assignment collab.Assignment) assignmentDoc {
	var doc assignmentDoc
	if len(assignment.Input) > 0 {
		_ = json.Unmarshal(assignment.Input, &doc)
	}
	return doc
}

func sourceID(source collab.Source) string {
	if source == nil {
		return ""
	}
	switch source.(type) {
	case *GitHubSource:
		return SourceGitHub
	case *FixtureSource:
		return SourceFixture
	default:
		return "source"
	}
}

func eventInputText(event Event) string {
	if strings.TrimSpace(event.Body) == "" {
		return fmt.Sprintf("New %s on %s", event.Kind, event.Thread.ID)
	}
	return event.Body
}

func workerTask(ticket Ticket) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Ticket %s: %s\n\n", shortID(ticket.ID), ticket.Title)
	if ticket.Mandate.Strength != "" {
		fmt.Fprintf(&builder, "Mandate strength: %s (observe never writes; comment is thread-scoped)\n\n", ticket.Mandate.Strength)
	}
	if strings.TrimSpace(ticket.Request) != "" {
		builder.WriteString(ticket.Request)
		builder.WriteString("\n\n")
	}
	if ticket.Source.URI != "" {
		fmt.Fprintf(&builder, "Source: %s\n\n", ticket.Source.URI)
	}
	builder.WriteString("Work in your own session. Report progress with the `report` tool and ask a human with the `ask` tool; both act only on the ticket bound to this session and never accept a ticket id. Use structured attention when you need a human decision; do not infer approval from free text.")
	return builder.String()
}

func attentionAnswerText(attention collab.Attention) string {
	if attention.Answer == nil {
		return "A human answered the attention."
	}
	if attention.Answer.Text != "" {
		return "Human answer: " + attention.Answer.Text
	}
	return "Human selected: " + attention.Answer.OptionID
}

func firstTicketID(tickets []Ticket) uuid.UUID {
	if len(tickets) == 0 {
		return uuid.Nil
	}
	return tickets[0].ID
}

func firstUUID(ids []uuid.UUID) uuid.UUID {
	if len(ids) == 0 {
		return uuid.Nil
	}
	return ids[0]
}

func shortID(id uuid.UUID) string {
	text := id.String()
	if len(text) > 8 {
		return text[:8]
	}
	return text
}
