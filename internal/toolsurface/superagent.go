package toolsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/harnessext"
)

// Model-facing super-agent worker tools. They are product extra tools, not part
// of the constant eleven-tool builtin surface: a deployment that enables the
// super-agent mode appends them, so the default tool manifest is unchanged. Both
// tools resolve their ticket/assignment from the ambient RunScope and the
// durable session index; there is no ticketId or assignmentId argument to
// misuse. A non-worker session receives a typed refusal, never a silent no-op.
const (
	Report = "report"
	Ask    = "ask"
)

// Typed refusal codes of the worker tools. The backend's own error type spells
// the same values; the mapping is asserted by tests.
const (
	CodeSuperagentUnavailable   = "superagent_unavailable"
	CodeSuperagentScopeRequired = "superagent_scope_required"
	CodeSuperagentNotWorker     = "superagent_not_worker"
	CodeSuperagentTicketBlocked = "superagent_ticket_blocked"
	CodeSuperagentTicketClosed  = "superagent_ticket_closed"
	CodeSuperagentInvalid       = "superagent_invalid_request"
)

// maxWorkerReportResultBytes bounds one terminal report payload and
// maxWorkerToolRequestID bounds the request id, so a tool call cannot grow a
// durable row without limit.
const (
	maxWorkerReportResultBytes = 64 << 10
	maxWorkerToolRequestID     = 200
)

// CodedToolError is the seam a backend uses to return a typed refusal without
// the tool surface importing the domain package. The tool surface renders the
// code and the caller's own ticket state; a refusal must never name another
// ticket.
type CodedToolError interface {
	error
	ToolCode() string
	ToolTicketID() string
	ToolTicketStatus() string
}

// SuperagentReport is one model report payload. It has no ticket/assignment
// field: those come from the calling worker session.
type SuperagentReport struct {
	State       string
	Summary     string
	AttentionID string
	Result      json.RawMessage
	RequestID   string
}

// SuperagentReportResult is the applied ticket/assignment state.
type SuperagentReportResult struct {
	TicketID         string
	TicketStatus     string
	AssignmentID     string
	AssignmentStatus string
	State            string
}

// SuperagentOption is one selectable attention answer.
type SuperagentOption struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// SuperagentAsk is one model question. The ticket/thread route is the worker's
// own; the model cannot select a channel.
type SuperagentAsk struct {
	Prompt        string
	Kind          string
	Options       []SuperagentOption
	AllowFreeText bool
	TimeoutMS     int64
	RequestID     string
}

// SuperagentAskResult is the persisted Attention plus the ticket it belongs to.
type SuperagentAskResult struct {
	AttentionID     string
	AttentionStatus string
	TicketID        string
	TicketStatus    string
	Prompt          string
	Kind            string
	Options         []SuperagentOption
	AllowFreeText   bool
	Route           string
	Channel         string
	Deadline        time.Time
}

// SuperagentBackend is the super-agent queue behind the two worker tools. The
// implementation resolves the ticket from scope.TenantID/scope.SessionID and
// enforces worker scope, ticket state, permissions and audit.
type SuperagentBackend interface {
	ReportFromWorker(ctx context.Context, scope harnessext.RunScope, input SuperagentReport) (SuperagentReportResult, error)
	AskFromWorker(ctx context.Context, scope harnessext.RunScope, input SuperagentAsk) (SuperagentAskResult, error)
}

// SuperagentReportStates is the closed report vocabulary the schema advertises.
var SuperagentReportStates = []string{"progress", "waiting", "ready", "blocked", "failed", "dropped"}

// NewSuperagentTools builds the two worker tools. They are appended to the
// product tool set only when the super-agent mode is enabled.
func NewSuperagentTools(backend SuperagentBackend) []agentcore.AgentTool {
	return []agentcore.AgentTool{newReportTool(backend), newAskTool(backend)}
}

const reportToolDescription = "Report this worker's progress on the ticket bound to the calling session. " +
	"The ticket and assignment are resolved from the session; the tool accepts no ticket or assignment id. " +
	"States: progress (a checkpoint), waiting (blocked on a human; attentionId is required), ready (the assignment is done), " +
	"blocked (blocked without asking a new question), failed, and dropped (give the assignment back to the queue). " +
	"A blocked or closed ticket refuses the report. Passing the same requestId again is idempotent and returns the current state."

const askToolDescription = "Ask a human a structured question about the ticket bound to the calling session. " +
	"The ticket and assignment are resolved from the session; the question is routed back to the ticket's own source thread " +
	"and the answer arrives as a new turn. Provide options and/or allowFreeText; timeoutMs bounds the deadline. " +
	"Passing the same requestId again reuses the existing Attention. A blocked or closed ticket refuses a new question."

func reportToolSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"state":{"type":"string","enum":["progress","waiting","ready","blocked","failed","dropped"],"description":"The report state."},` +
		`"summary":{"type":"string","description":"A short status line for the ticket (never a credential)."},` +
		`"attentionId":{"type":"string","description":"The Attention the worker is waiting on; required for waiting."},` +
		`"result":{"type":"object","description":"The terminal structured result for ready/failed/dropped."},` +
		`"requestId":{"type":"string","description":"Idempotency key; a retry with the same key is a no-op."}` +
		`},"required":["state"],"additionalProperties":true}`)
}

func askToolSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"prompt":{"type":"string","description":"The question for the human."},` +
		`"kind":{"type":"string","description":"decision, clarify or approval."},` +
		`"options":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["id"]}},` +
		`"allowFreeText":{"type":"boolean"},` +
		`"timeoutMs":{"type":"integer","minimum":0},` +
		`"requestId":{"type":"string","description":"Idempotency key; a retry reuses the same Attention."}` +
		`},"required":["prompt"],"additionalProperties":true}`)
}

// workerTool is a single-action tool (no action envelope): report/ask are the
// whole call. It is sequential because both mutate the ticket state.
type workerTool struct {
	name        string
	description string
	schema      json.RawMessage
	backend     SuperagentBackend
	run         func(ctx context.Context, callID string, scope harnessext.RunScope, args json.RawMessage) (agentcore.AgentToolResult, error)
}

func (t *workerTool) Name() string            { return t.name }
func (t *workerTool) Description() string     { return t.description }
func (t *workerTool) Schema() json.RawMessage { return t.schema }
func (t *workerTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionSequential
}

func (t *workerTool) Execute(ctx context.Context, callID string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	if t.backend == nil {
		return ErrorResult(CodeSuperagentUnavailable, t.name,
			"the super-agent queue is not assembled in this deployment", nil)
	}
	scope, ok := harnessext.RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return ErrorResult(CodeSuperagentScopeRequired, t.name,
			"the worker tools need the ambient run scope of a super-agent worker session", nil)
	}
	return t.run(ctx, callID, scope, args)
}

func newReportTool(backend SuperagentBackend) *workerTool {
	return &workerTool{
		name:        Report,
		description: reportToolDescription,
		schema:      reportToolSchema(),
		backend:     backend,
		run: func(ctx context.Context, callID string, scope harnessext.RunScope, args json.RawMessage) (agentcore.AgentToolResult, error) {
			var input struct {
				State       string          `json:"state"`
				Summary     string          `json:"summary"`
				AttentionID string          `json:"attentionId"`
				Result      json.RawMessage `json:"result"`
				RequestID   string          `json:"requestId"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return invalidArguments(Report, "report arguments are not valid JSON: "+err.Error())
			}
			input.State = strings.TrimSpace(input.State)
			if !validReportState(input.State) {
				return invalidArguments(Report, "state must be one of "+strings.Join(SuperagentReportStates, ", "))
			}
			attentionID := strings.TrimSpace(input.AttentionID)
			if attentionID != "" {
				if _, err := uuid.Parse(attentionID); err != nil {
					return invalidArguments(Report, "attentionId is not a UUID")
				}
			} else if input.State == "waiting" {
				return invalidArguments(Report, "a waiting report must name attentionId")
			}
			if len(input.Result) > maxWorkerReportResultBytes {
				return invalidArguments(Report, fmt.Sprintf("result exceeds %d bytes", maxWorkerReportResultBytes))
			}
			requestID, problem := workerRequestID(input.RequestID, callID, scope, Report)
			if problem != "" {
				return invalidArguments(Report, problem)
			}
			outcome, err := backend.ReportFromWorker(ctx, scope, SuperagentReport{
				State:       input.State,
				Summary:     strings.TrimSpace(input.Summary),
				AttentionID: attentionID,
				Result:      input.Result,
				RequestID:   requestID,
			})
			if err != nil {
				return superagentToolError(Report, err)
			}
			return jsonResult(Report, map[string]any{
				"reported": true,
				"state":    input.State,
				"ticket": map[string]any{
					"id":     outcome.TicketID,
					"status": outcome.TicketStatus,
				},
				"assignment": map[string]any{
					"id":     outcome.AssignmentID,
					"status": outcome.AssignmentStatus,
				},
			})
		},
	}
}

func newAskTool(backend SuperagentBackend) *workerTool {
	return &workerTool{
		name:        Ask,
		description: askToolDescription,
		schema:      askToolSchema(),
		backend:     backend,
		run: func(ctx context.Context, callID string, scope harnessext.RunScope, args json.RawMessage) (agentcore.AgentToolResult, error) {
			var input struct {
				Prompt        string             `json:"prompt"`
				Kind          string             `json:"kind"`
				Options       []SuperagentOption `json:"options"`
				AllowFreeText bool               `json:"allowFreeText"`
				TimeoutMS     int64              `json:"timeoutMs"`
				RequestID     string             `json:"requestId"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return invalidArguments(Ask, "ask arguments are not valid JSON: "+err.Error())
			}
			input.Prompt = strings.TrimSpace(input.Prompt)
			if input.Prompt == "" {
				return invalidArguments(Ask, "prompt is required")
			}
			if input.TimeoutMS < 0 {
				return invalidArguments(Ask, "timeoutMs must be non-negative")
			}
			options, problem := normalizeAttentionOptions(input.Options)
			if problem != "" {
				return invalidArguments(Ask, problem)
			}
			if len(options) == 0 && !input.AllowFreeText {
				return invalidArguments(Ask, "provide options or allowFreeText")
			}
			requestID, problem := workerRequestID(input.RequestID, callID, scope, Ask)
			if problem != "" {
				return invalidArguments(Ask, problem)
			}
			outcome, err := backend.AskFromWorker(ctx, scope, SuperagentAsk{
				Prompt:        input.Prompt,
				Kind:          strings.TrimSpace(input.Kind),
				Options:       options,
				AllowFreeText: input.AllowFreeText,
				TimeoutMS:     input.TimeoutMS,
				RequestID:     requestID,
			})
			if err != nil {
				return superagentToolError(Ask, err)
			}
			optionsOut := make([]map[string]any, 0, len(outcome.Options))
			for _, option := range outcome.Options {
				optionsOut = append(optionsOut, map[string]any{
					"id": option.ID, "label": option.Label, "description": option.Description,
				})
			}
			attention := map[string]any{
				"id":            outcome.AttentionID,
				"status":        outcome.AttentionStatus,
				"prompt":        outcome.Prompt,
				"kind":          outcome.Kind,
				"options":       optionsOut,
				"allowFreeText": outcome.AllowFreeText,
				"route":         outcome.Route,
				"channel":       outcome.Channel,
			}
			if !outcome.Deadline.IsZero() {
				attention["deadline"] = outcome.Deadline.UTC().Format(time.RFC3339)
			}
			return jsonResult(Ask, map[string]any{
				"attention": attention,
				"ticket": map[string]any{
					"id":     outcome.TicketID,
					"status": outcome.TicketStatus,
				},
			})
		},
	}
}

// workerRequestID derives the idempotency key. An explicit requestId wins; a
// tool-call id makes a kernel retry of the same call stable without collapsing
// two distinct model questions into one.
func workerRequestID(explicit, callID string, scope harnessext.RunScope, tool string) (string, string) {
	requestID := strings.TrimSpace(explicit)
	if requestID == "" {
		requestID = "tool:" + tool + ":" + scope.SessionID.String()
		if strings.TrimSpace(callID) != "" {
			requestID += ":" + strings.TrimSpace(callID)
		}
	}
	if len(requestID) > maxWorkerToolRequestID {
		return "", fmt.Sprintf("requestId exceeds %d bytes", maxWorkerToolRequestID)
	}
	return requestID, ""
}

func validReportState(state string) bool {
	for _, candidate := range SuperagentReportStates {
		if candidate == state {
			return true
		}
	}
	return false
}

// normalizeAttentionOptions validates the model's options before the durable
// Attention is created; the core Host repeats the validation as the authority.
func normalizeAttentionOptions(options []SuperagentOption) ([]SuperagentOption, string) {
	out := make([]SuperagentOption, 0, len(options))
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		id := strings.TrimSpace(option.ID)
		if id == "" {
			return nil, "attention option ids are required"
		}
		if seen[id] {
			return nil, "attention option ids must be unique"
		}
		seen[id] = true
		out = append(out, SuperagentOption{
			ID:          id,
			Label:       strings.TrimSpace(option.Label),
			Description: strings.TrimSpace(option.Description),
		})
	}
	return out, ""
}

// superagentToolError renders a backend refusal. A coded refusal keeps its
// code and the caller's own ticket state; an untyped error is a backend_error
// whose text is the domain message (ids of the caller's ticket only).
func superagentToolError(tool string, err error) (agentcore.AgentToolResult, error) {
	var coded CodedToolError
	if errors.As(err, &coded) {
		extra := map[string]any{}
		if ticketID := strings.TrimSpace(coded.ToolTicketID()); ticketID != "" {
			extra["ticketId"] = ticketID
		}
		if status := strings.TrimSpace(coded.ToolTicketStatus()); status != "" {
			extra["ticketStatus"] = status
		}
		return ErrorResult(coded.ToolCode(), tool, err.Error(), extra)
	}
	if errors.Is(err, harnessext.ErrRunScopeMissing) {
		return ErrorResult(CodeSuperagentScopeRequired, tool,
			"the worker tools need the ambient run scope of a super-agent worker session", nil)
	}
	return ErrorResult(CodeBackendFailed, tool, err.Error(), nil)
}

func jsonResult(tool string, document map[string]any) (agentcore.AgentToolResult, error) {
	raw, err := json.Marshal(document)
	if err != nil {
		return ErrorResult(CodeBackendFailed, tool, "encode result: "+err.Error(), nil)
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: document,
	}, nil
}
