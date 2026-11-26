package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
	"github.com/jixoai/keel/internal/superagent"
	"github.com/jixoai/keel/internal/toolsurface"
)

// superagentWorkerModelTools returns the report/ask tools only when the
// deployment's source switch enables the mode. The default deployment keeps
// the eleven-tool builtin surface and the default manifest hash.
func superagentWorkerModelTools(backend *superagentToolBackend) []agentcore.AgentTool {
	if backend == nil {
		return nil
	}
	cfg, err := superagentConfigFromEnv(os.Getenv)
	if err != nil || !cfg.Enabled {
		return nil
	}
	return toolsurface.NewSuperagentTools(backend)
}

// superagentToolBackend is the product-layer adapter behind the model-facing
// `report` and `ask` tools. It is created before servicehost.Boot because the
// host assembly captures the tool instances; the orchestrator is installed
// afterwards by newProductLayer. A call before installation (or in a
// deployment whose super-agent mode is disabled) is a typed fail-closed answer.
type superagentToolBackend struct {
	mu           sync.RWMutex
	orchestrator *superagent.Orchestrator
}

var _ toolsurface.SuperagentBackend = (*superagentToolBackend)(nil)

func newSuperagentToolBackend() *superagentToolBackend { return &superagentToolBackend{} }

func (b *superagentToolBackend) set(orchestrator *superagent.Orchestrator) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.orchestrator = orchestrator
	b.mu.Unlock()
}

func (b *superagentToolBackend) get() *superagent.Orchestrator {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.orchestrator
}

// ReportFromWorker applies one report to the ticket bound to scope.SessionID.
// The model cannot name a ticket or assignment.
func (b *superagentToolBackend) ReportFromWorker(ctx context.Context, scope harnessext.RunScope, input toolsurface.SuperagentReport) (toolsurface.SuperagentReportResult, error) {
	orchestrator := b.get()
	if orchestrator == nil {
		return toolsurface.SuperagentReportResult{}, unavailableWorkerTool()
	}
	attentionID, err := optionalUUID(input.AttentionID, "attentionId")
	if err != nil {
		return toolsurface.SuperagentReportResult{}, err
	}
	outcome, err := orchestrator.ReportFromWorker(ctx, scope.TenantID, scope.SessionID, superagent.WorkerReportInput{
		State:       superagent.ReportState(strings.TrimSpace(input.State)),
		Summary:     input.Summary,
		AttentionID: attentionID,
		Result:      input.Result,
		RequestID:   input.RequestID,
	})
	if err != nil {
		return toolsurface.SuperagentReportResult{}, err
	}
	state := string(outcome.Report.State)
	if state == "" {
		state = strings.TrimSpace(input.State)
	}
	return toolsurface.SuperagentReportResult{
		TicketID:         outcome.Ticket.ID.String(),
		TicketStatus:     string(outcome.Ticket.Status),
		AssignmentID:     outcome.Assignment.ID.String(),
		AssignmentStatus: string(outcome.Assignment.Status),
		State:            state,
		Created:          outcome.Created,
	}, nil
}

// AskFromWorker opens one Attention for the ticket bound to scope.SessionID.
func (b *superagentToolBackend) AskFromWorker(ctx context.Context, scope harnessext.RunScope, input toolsurface.SuperagentAsk) (toolsurface.SuperagentAskResult, error) {
	orchestrator := b.get()
	if orchestrator == nil {
		return toolsurface.SuperagentAskResult{}, unavailableWorkerTool()
	}
	options := make([]collab.AttentionOption, 0, len(input.Options))
	for _, option := range input.Options {
		options = append(options, collab.AttentionOption{
			ID:          option.ID,
			Label:       option.Label,
			Description: option.Description,
		})
	}
	attention, ticket, err := orchestrator.AskFromWorker(ctx, scope.TenantID, scope.SessionID, collab.AttentionRequest{
		RequestID:     input.RequestID,
		Kind:          input.Kind,
		Prompt:        input.Prompt,
		Options:       options,
		AllowFreeText: input.AllowFreeText,
		Timeout:       time.Duration(input.TimeoutMS) * time.Millisecond,
	})
	if err != nil {
		return toolsurface.SuperagentAskResult{}, err
	}
	resultOptions := make([]toolsurface.SuperagentOption, 0, len(attention.Options))
	for _, option := range attention.Options {
		resultOptions = append(resultOptions, toolsurface.SuperagentOption{
			ID: option.ID, Label: option.Label, Description: option.Description,
		})
	}
	return toolsurface.SuperagentAskResult{
		AttentionID:     attention.ID.String(),
		AttentionStatus: string(attention.Status),
		TicketID:        ticket.ID.String(),
		TicketStatus:    string(ticket.Status),
		Prompt:          attention.Prompt,
		Kind:            attention.Kind,
		Options:         resultOptions,
		AllowFreeText:   attention.AllowFreeText,
		Route:           string(attention.Route),
		Channel:         attention.Channel,
		Deadline:        attention.Deadline,
	}, nil
}

func unavailableWorkerTool() error {
	return &superagent.WorkerToolError{
		Code:    superagent.WorkerToolUnavailable,
		Message: "the super-agent queue is not assembled in this deployment",
	}
}

func optionalUUID(raw, field string) (uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.Nil, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &superagent.WorkerToolError{
			Code:    superagent.WorkerToolInvalidRequest,
			Message: fmt.Sprintf("%s is not a UUID", field),
		}
	}
	return parsed, nil
}

// Compile-time guard: domain refusals must satisfy the tool surface's coded
// error seam so the model sees a typed code, not a generic backend failure.
var _ interface {
	error
	ToolCode() string
	ToolTicketID() string
	ToolTicketStatus() string
} = (*superagent.WorkerToolError)(nil)
