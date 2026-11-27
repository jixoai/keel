package superagentboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/superagent"
)

// SupervisorAPI is the existing supervisor surface the command bridge calls.
// *superagent.Orchestrator satisfies it; tests use a fake. The bridge never
// invents a second mutation path: claim/assign are the operator's existing
// ResumeTicket, complete/fail are worker Report, block/unblock are Ask and
// AnswerAttention, and cancel is StopTicket.
type SupervisorAPI interface {
	Report(ctx context.Context, request superagent.ReportRequest) error
	Ask(ctx context.Context, ticketID, assignmentID uuid.UUID, request collab.AttentionRequest) (collab.Attention, error)
	AnswerAttention(ctx context.Context, tenantID, attentionID uuid.UUID, optionID, text, actor string) (collab.Attention, error)
	ResumeTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) (superagent.Ticket, error)
	StopTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor, reason string) error
}

// SupervisorCommands maps the board's action vocabulary onto the supervisor's
// existing API. Every method returns only after the supervisor wrote; the
// caller mirrors the new truth to the board afterwards.
type SupervisorCommands struct {
	TenantID   uuid.UUID
	Store      superagent.Store
	Supervisor SupervisorAPI
	Now        func() time.Time
}

// NewSupervisorCommands validates the seams.
func NewSupervisorCommands(tenantID uuid.UUID, store superagent.Store, supervisor SupervisorAPI) (*SupervisorCommands, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("superagentboard: commands tenant is required")
	}
	if store == nil {
		return nil, errors.New("superagentboard: commands need the supervisor store")
	}
	if supervisor == nil {
		return nil, errors.New("superagentboard: commands need the supervisor API")
	}
	return &SupervisorCommands{
		TenantID: tenantID, Store: store, Supervisor: supervisor,
		Now: func() time.Time { return time.Now().UTC() },
	}, nil
}

// ClaimTicket asks the supervisor to dispatch the ticket now. The board's
// claim is the supervisor's operator resume, not a second assignment.
func (c *SupervisorCommands) ClaimTicket(ctx context.Context, tenantID, ticketID uuid.UUID, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	_, err := c.Supervisor.ResumeTicket(ctx, tenantID, ticketID, actor, "board claim")
	return err
}

// AssignTicket keeps the work with a board profile. The supervisor dispatch is
// the durable assignment; the board's `for` is presentation the bridge writes
// after this succeeds.
func (c *SupervisorCommands) AssignTicket(ctx context.Context, tenantID, ticketID uuid.UUID, forValue, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	reason := "board assign"
	if strings.TrimSpace(forValue) != "" {
		reason += ": " + strings.TrimSpace(forValue)
	}
	_, err := c.Supervisor.ResumeTicket(ctx, tenantID, ticketID, actor, reason)
	return err
}

// CompleteTicket reports the active assignment done. A ticket with no active
// assignment is refused: there is no worker whose report this would be.
func (c *SupervisorCommands) CompleteTicket(ctx context.Context, tenantID, ticketID uuid.UUID, result, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	assignment, err := c.activeAssignment(ctx, ticketID)
	if err != nil {
		return err
	}
	request := superagent.ReportRequest{
		TenantID: tenantID, WorkItemID: ticketID, AssignmentID: assignment.ID,
		State:   superagent.ReportReady,
		Summary: strings.TrimSpace(result),
	}
	if encoded, marshalErr := json.Marshal(strings.TrimSpace(result)); marshalErr == nil && strings.TrimSpace(result) != "" {
		request.Result = encoded
	}
	return c.Supervisor.Report(ctx, request)
}

// FailTicket reports the active assignment failed.
func (c *SupervisorCommands) FailTicket(ctx context.Context, tenantID, ticketID uuid.UUID, reason, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	assignment, err := c.activeAssignment(ctx, ticketID)
	if err != nil {
		return err
	}
	return c.Supervisor.Report(ctx, superagent.ReportRequest{
		TenantID: tenantID, WorkItemID: ticketID, AssignmentID: assignment.ID,
		State:   superagent.ReportFailed,
		Summary: BoardNote(reason, "failed from board"),
	})
}

// BlockTicket asks one structured question. The board's block kind selects a
// clarify question or an approval; the reason is the prompt.
//
// Idempotency and intent identity (Phase 1 hardening P1-2):
//   - the supervisor's Ask is idempotent per RequestID, but a RequestID built
//     only from ticket+reason would return the *closed* Attention of an earlier
//     block forever. board's task epoch does not change on block/unblock, while
//     the root message's Version advances on every block/unblock/assign event.
//     The RequestID therefore includes the board version and the active
//     assignment: block -> answer -> block(same reason) is a new intent, and a
//     new assignment is a new intent.
//   - a retried HTTP request must not open a second Attention. An open
//     Attention created by the same board block intent (same ticket,
//     assignment and reason digest, any version) makes the retry a no-op. This
//     is also correct for a deliberate second block while the first is open:
//     the ticket is already waiting on that exact question. A different reason
//     is a different question and is not swallowed.
func (c *SupervisorCommands) BlockTicket(ctx context.Context, tenantID, ticketID uuid.UUID, kind, reason string, boardVersion uint64, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	assignment, err := c.activeAssignment(ctx, ticketID)
	if err != nil {
		return err
	}
	attentionKind := collab.AttentionClarify
	if strings.TrimSpace(kind) == HoldAwaitingApproval {
		attentionKind = collab.AttentionApproval
	}
	intentPrefix := "board-block:v2:" + ticketID.String() + ":" + assignment.ID.String() + ":" + digestOf(strings.TrimSpace(reason))[:16]
	attentions, err := c.Store.ListAttentions(ctx, c.TenantID, ticketID)
	if err != nil {
		return err
	}
	for _, attention := range attentions {
		if attention.Status == collab.AttentionOpen && strings.HasPrefix(attention.RequestID, intentPrefix) {
			// The same intent is already open: this is a retry (or a duplicate
			// block), not a new question.
			return nil
		}
	}
	requestID := intentPrefix + ":" + strconv.FormatUint(boardVersion, 10)
	_, err = c.Supervisor.Ask(ctx, ticketID, assignment.ID, collab.AttentionRequest{
		// A board block is an operator question, not a worker tool call: the
		// supervisor contract refuses a question with no options and no free
		// text, so the operator can answer it in the thread.
		RequestID:     requestID,
		Kind:          attentionKind,
		Prompt:        BoardNote(reason, "blocked from board"),
		AllowFreeText: true,
		Route:         collab.AttentionRouteChannel,
	})
	return err
}

// UnblockTicket answers the ticket's oldest open Attention with the operator's
// text. A ticket that has no open Attention is refused, not silently resumed:
// unblocking a supervisor state the board did not create would be a second
// truth.
func (c *SupervisorCommands) UnblockTicket(ctx context.Context, tenantID, ticketID uuid.UUID, optionID, reason, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	attention, err := c.openAttention(ctx, ticketID)
	if err != nil {
		return err
	}
	_, err = c.Supervisor.AnswerAttention(ctx, tenantID, attention.ID, optionID, BoardNote(reason, "unblocked from board"), actor)
	return err
}

// CancelTicket is the supervisor's human stop.
func (c *SupervisorCommands) CancelTicket(ctx context.Context, tenantID, ticketID uuid.UUID, reason, actor string) error {
	if err := c.checkTenant(tenantID); err != nil {
		return err
	}
	return c.Supervisor.StopTicket(ctx, tenantID, ticketID, actor, BoardNote(reason, "canceled from board"))
}

func (c *SupervisorCommands) checkTenant(tenantID uuid.UUID) error {
	if tenantID == uuid.Nil || tenantID != c.TenantID {
		return fmt.Errorf("superagentboard: command tenant %s is not this adapter's tenant", tenantID)
	}
	return nil
}

func (c *SupervisorCommands) activeAssignment(ctx context.Context, ticketID uuid.UUID) (collab.Assignment, error) {
	assignments, err := c.Store.ListAssignments(ctx, c.TenantID, ticketID)
	if err != nil {
		return collab.Assignment{}, err
	}
	sorted := append([]collab.Assignment(nil), assignments...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for index := len(sorted) - 1; index >= 0; index-- {
		assignment := sorted[index]
		if assignment.Status == collab.AssignmentActive && assignment.SessionID != uuid.Nil {
			return assignment, nil
		}
	}
	return collab.Assignment{}, ErrNoActiveAssignment
}

func (c *SupervisorCommands) openAttention(ctx context.Context, ticketID uuid.UUID) (collab.Attention, error) {
	attentions, err := c.Store.ListAttentions(ctx, c.TenantID, ticketID)
	if err != nil {
		return collab.Attention{}, err
	}
	sorted := append([]collab.Attention(nil), attentions...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for _, attention := range sorted {
		if attention.Status == collab.AttentionOpen {
			return attention, nil
		}
	}
	return collab.Attention{}, ErrNoOpenAttention
}
