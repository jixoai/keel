package superagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// WorkerReport is one durable worker report. The report row is the ticket's
// timeline fact and the idempotency ledger of the model-facing `report` tool:
// a repeated request id returns the original row instead of appending a second
// report. Source bodies and prompts are never copied into it.
type WorkerReport struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenantId"`
	WorkItemID   uuid.UUID       `json:"workItemId"`
	AssignmentID uuid.UUID       `json:"assignmentId"`
	SessionID    uuid.UUID       `json:"sessionId,omitempty"`
	RequestID    string          `json:"requestId"`
	State        ReportState     `json:"state"`
	Summary      string          `json:"summary,omitempty"`
	AttentionID  uuid.UUID       `json:"attentionId,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	// AuditedAt is the two-phase claim marker. A claim is inserted before its
	// audit row; a retry that finds it unaudited completes the audit before
	// applying the stored fact, so a crash cannot duplicate or lose the audit.
	AuditedAt *time.Time `json:"auditedAt,omitempty"`
	// AppliedAt records that the claim's state transition landed. A retry that
	// finds an audited-but-unapplied claim resumes the transition; a completed
	// claim returns its stored fact without re-auditing or re-applying.
	AppliedAt *time.Time `json:"appliedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ErrReportNotFound is the store's not-found answer for one report request id.
var ErrReportNotFound = errors.New("superagent: worker report not found")

// WorkerToolCode is the closed refusal vocabulary of the model-facing worker
// tools. The strings are the typed codes the tool surface returns to the model;
// internal/superagent does not import the tool surface, so the shared spelling
// is asserted by a test rather than a type.
type WorkerToolCode string

const (
	// WorkerToolUnavailable means the deployment has no assembled super-agent
	// queue. It is fail-closed, never a silent no-op.
	WorkerToolUnavailable WorkerToolCode = "superagent_unavailable"
	// WorkerToolScopeRequired means the tool call has no ambient run/session
	// scope, so no ticket can be resolved.
	WorkerToolScopeRequired WorkerToolCode = "superagent_scope_required"
	// WorkerToolNotWorker means the calling session is not an assignment worker
	// of this super-agent mode. A bare sub-agent or an ordinary session is this
	// answer, and it never names another ticket.
	WorkerToolNotWorker WorkerToolCode = "superagent_not_worker"
	// WorkerToolTicketBlocked means the ticket is waiting on a human decision.
	WorkerToolTicketBlocked WorkerToolCode = "superagent_ticket_blocked"
	// WorkerToolTicketClosed means the ticket is done/failed/cancelled or its
	// mandate was revoked.
	WorkerToolTicketClosed WorkerToolCode = "superagent_ticket_closed"
	// WorkerToolInvalidRequest means the tool arguments are malformed.
	WorkerToolInvalidRequest WorkerToolCode = "superagent_invalid_request"
	// WorkerToolAttentionInvalid means the Attention a report names is closed
	// or belongs to a different assignment/session.
	WorkerToolAttentionInvalid WorkerToolCode = "superagent_attention_invalid"
)

// WorkerToolError is a worker-tool refusal with a stable code. The tool surface
// reads ToolCode/ToolTicketID/ToolTicketStatus through its own interface, so it
// can render a typed result without importing this package. TicketID is always
// the caller's own ticket; a refusal never names another one.
type WorkerToolError struct {
	Code         WorkerToolCode
	Message      string
	TicketID     uuid.UUID
	TicketStatus collab.WorkItemStatus
}

func (e *WorkerToolError) Error() string {
	if e == nil {
		return "superagent: worker tool refused"
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return string(e.Code)
}

// ToolCode implements the tool surface's coded-error seam.
func (e *WorkerToolError) ToolCode() string {
	if e == nil {
		return ""
	}
	return string(e.Code)
}

// ToolTicketID implements the tool surface's coded-error seam.
func (e *WorkerToolError) ToolTicketID() string {
	if e == nil || e.TicketID == uuid.Nil {
		return ""
	}
	return e.TicketID.String()
}

// ToolTicketStatus implements the tool surface's coded-error seam.
func (e *WorkerToolError) ToolTicketStatus() string {
	if e == nil {
		return ""
	}
	return string(e.TicketStatus)
}

// WorkerReportInput is the model-facing report payload. It deliberately has no
// ticket or assignment field: both are resolved from the calling session.
type WorkerReportInput struct {
	State       ReportState     `json:"state"`
	Summary     string          `json:"summary,omitempty"`
	AttentionID uuid.UUID       `json:"attentionId,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	RequestID   string          `json:"requestId,omitempty"`
}

// WorkerReportOutcome is the ticket/assignment state after a report, plus the
// stored report fact. Created is false when an existing claim (same
// assignment/session/request id) produced the result.
type WorkerReportOutcome struct {
	Ticket     Ticket
	Assignment collab.Assignment
	Report     WorkerReport
	Created    bool
}

// ReportFromWorker applies a report for the session in scope. The ticket and
// assignment come from the durable session index plus the assignment rows; a
// model-supplied ticket id has no way in. A blocked/closed ticket is refused,
// and a repeated RequestID is a no-op that returns the current state.
func (o *Orchestrator) ReportFromWorker(ctx context.Context, tenantID, sessionID uuid.UUID, input WorkerReportInput) (WorkerReportOutcome, error) {
	if err := o.checkTenant(tenantID); err != nil {
		return WorkerReportOutcome{}, &WorkerToolError{
			Code:    WorkerToolScopeRequired,
			Message: "the worker tool call is outside the orchestrator's tenant",
		}
	}
	ticket, assignment, err := o.resolveWorker(ctx, tenantID, sessionID)
	if err != nil {
		return WorkerReportOutcome{}, err
	}
	return o.report(ctx, ReportRequest{
		TenantID:        tenantID,
		WorkItemID:      ticket.ID,
		AssignmentID:    assignment.ID,
		CallerSessionID: sessionID,
		State:           input.State,
		Summary:         input.Summary,
		AttentionID:     input.AttentionID,
		Result:          input.Result,
		RequestID:       strings.TrimSpace(input.RequestID),
	}, true)
}

// AskFromWorker opens one structured Attention for the session in scope. The
// ticket/assignment are resolved from the session; a blocked/closed ticket is
// refused. RequestID keeps a retried question idempotent (the core Host
// returns the original Attention).
func (o *Orchestrator) AskFromWorker(ctx context.Context, tenantID, sessionID uuid.UUID, request collab.AttentionRequest) (collab.Attention, Ticket, error) {
	if err := o.checkTenant(tenantID); err != nil {
		return collab.Attention{}, Ticket{}, &WorkerToolError{
			Code:    WorkerToolScopeRequired,
			Message: "the worker tool call is outside the orchestrator's tenant",
		}
	}
	ticket, assignment, err := o.resolveWorker(ctx, tenantID, sessionID)
	if err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	// The ambient session is the actor; the model cannot name another one.
	request.CallerSessionID = sessionID
	request.AssignmentID = assignment.ID
	attention, askTicket, err := o.ask(ctx, ticket.ID, assignment.ID, request, true)
	if err != nil {
		return collab.Attention{}, Ticket{}, err
	}
	return attention, askTicket, nil
}

// resolveWorker is the only ticket/assignment resolution the model-facing
// tools use. It reads the durable session index the guarded Host writes (mode
// + work item), then requires an assignment row bound to exactly this session.
// Every failure is a typed not-worker refusal, so a non-worker session cannot
// probe for ticket existence.
func (o *Orchestrator) resolveWorker(ctx context.Context, tenantID, sessionID uuid.UUID) (Ticket, collab.Assignment, error) {
	if sessionID == uuid.Nil {
		return Ticket{}, collab.Assignment{}, &WorkerToolError{
			Code:    WorkerToolScopeRequired,
			Message: "the worker tool call has no session id",
		}
	}
	if o.cfg.Sessions == nil {
		return Ticket{}, collab.Assignment{}, &WorkerToolError{
			Code:    WorkerToolUnavailable,
			Message: "the durable collaboration session index is not wired in this deployment",
		}
	}
	ref, err := o.cfg.Sessions.Session(ctx, tenantID, sessionID)
	if err != nil {
		return Ticket{}, collab.Assignment{}, notWorker()
	}
	if ref.ModeID != o.cfg.ModeID || ref.WorkItemID == uuid.Nil {
		return Ticket{}, collab.Assignment{}, notWorker()
	}
	ticket, err := o.loadTicket(ctx, tenantID, ref.WorkItemID)
	if err != nil {
		if errors.Is(err, collab.ErrWorkItemNotFound) || errors.Is(err, collab.ErrInvalidRequest) {
			return Ticket{}, collab.Assignment{}, notWorker()
		}
		return Ticket{}, collab.Assignment{}, err
	}
	assignments, err := o.cfg.Store.ListAssignments(ctx, tenantID, ticket.ID)
	if err != nil {
		return Ticket{}, collab.Assignment{}, err
	}
	for _, assignment := range assignments {
		if assignment.SessionID != sessionID {
			continue
		}
		if assignment.ModeID != "" && assignment.ModeID != o.cfg.ModeID {
			continue
		}
		if assignment.BotID != "" && ref.BotID != "" && assignment.BotID != ref.BotID {
			continue
		}
		return ticket, assignment, nil
	}
	return Ticket{}, collab.Assignment{}, notWorker()
}

func notWorker() *WorkerToolError {
	return &WorkerToolError{
		Code:    WorkerToolNotWorker,
		Message: "this session is not an active super-agent worker assignment; the tool only acts on the ticket bound to the calling worker session",
	}
}
