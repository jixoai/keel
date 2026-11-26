package harnessext

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/superagent"
)

// SuperagentInput adapts the super-agent follow-up/answer sink onto the one
// turn-starter edge triggers and sub-agents use. It never creates a session:
// the worker session already exists and is owned by its Bot.
type SuperagentInput struct {
	Starter *TurnStarter
}

var _ superagent.InputSink = SuperagentInput{}

// DeliversInput implements superagent.DeliveryCapable: without a turn starter
// Deliver returns an error, so only an assembled starter counts as delivery.
func (s SuperagentInput) DeliversInput() bool { return s.Starter != nil }

// Deliver commits one user message into the worker session with a stable
// request id, so a replayed follow-up or a retried attention answer dedupes in
// the kernel ledger rather than starting a second turn.
func (s SuperagentInput) Deliver(ctx context.Context, request superagent.InputRequest) error {
	if s.Starter == nil {
		return fmt.Errorf("%w: super-agent input sink needs a turn starter", ErrNotWired)
	}
	if request.TenantID == uuid.Nil || request.SessionID == uuid.Nil {
		return fmt.Errorf("harnessext: super-agent input needs tenant and session")
	}
	_, err := s.Starter.StartTurn(ctx, StartTurnRequest{
		TenantID:  request.TenantID,
		SessionID: request.SessionID,
		Text:      request.Text,
		RequestID: request.RequestID,
		Extras: map[string]any{
			"superagent": map[string]string{
				"workItemId":   request.WorkItemID.String(),
				"assignmentId": request.AssignmentID.String(),
				"kind":         request.Kind,
			},
		},
	})
	return err
}
