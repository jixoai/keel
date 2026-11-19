package agentext

// Approval plugin (`keel.approval`, change keel-harness-v2, W2.5).
//
// The harness owns the model-facing `approve` aggregate tool; this plugin is
// its action backend, exactly like keel.creation backs `creation`. The three
// modes map onto the v1 decision primitive:
//
//	confirm  yes/no options, free text not allowed
//	prompt   free text is the declared answer space
//	choice   caller-declared options; the "other" escape is free text and is
//	         recorded with validated=false
//
// The tool is the human-in-the-loop barrier: it opens a durable decision, waits
// for an answer (bounded by the question's own deadline), and reports the
// persisted outcome. It cannot answer its own question — the answer arrives
// through the decisions API (or a URL/HTTP answer path), never through this
// tool. Timeout is a persisted state of the decision, not a timer in this
// process; a run cancellation cancels the open question durably.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/approval"
)

// Approval plugin identity and action handler names.
const (
	ApprovalPluginID      = "keel.approval"
	ApprovalPluginVersion = "1.0.0"

	ApprovalToolConfirm = "keel.approval.confirm"
	ApprovalToolPrompt  = "keel.approval.prompt"
	ApprovalToolChoice  = "keel.approval.choice"

	approvalDefaultTimeout = 5 * time.Minute
	approvalMinTimeout     = time.Second
	approvalMaxTimeout     = 24 * time.Hour
)

// ApprovalOptions wires the plugin. Decisions is the v1 decision primitive;
// Provenance is the audit projection (a nil value gets an in-memory one so the
// audit exists within the process lifetime).
type ApprovalOptions struct {
	Decisions      *DecisionBroker
	Provenance     *approval.Service
	DefaultTimeout time.Duration
	Logger         *slog.Logger
}

// NewApprovalPlugin builds the builtin approval plugin.
func NewApprovalPlugin(opts ApprovalOptions) *Plugin {
	if opts.Provenance == nil {
		opts.Provenance = approval.NewService(approval.Options{})
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = approvalDefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	handler := &approvalPlugin{opts: opts}
	return &Plugin{
		ID:      ApprovalPluginID,
		Version: ApprovalPluginVersion,
		Tools: []ToolSpec{
			{
				Name:        ApprovalToolConfirm,
				Description: "Confirm or refuse a proposed action.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Sequential:  true,
				Execute:     handler.execute(approval.ModeConfirm),
			},
			{
				Name:        ApprovalToolPrompt,
				Description: "Ask the human a free-text question.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Sequential:  true,
				Execute:     handler.execute(approval.ModePrompt),
			},
			{
				Name:        ApprovalToolChoice,
				Description: "Ask the human to choose one of the offered options.",
				Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`),
				Internal:    true,
				Sequential:  true,
				Execute:     handler.execute(approval.ModeChoice),
			},
		},
	}
}

type approvalPlugin struct {
	opts ApprovalOptions
}

type approvalOptionArg struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

func (o *approvalOptionArg) UnmarshalJSON(raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, `"`) {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return err
		}
		o.ID = id
		return nil
	}
	type alias approvalOptionArg
	var value alias
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*o = approvalOptionArg(value)
	return nil
}

type approvalArgs struct {
	Prompt     string              `json:"prompt"`
	Options    []approvalOptionArg `json:"options"`
	YesLabel   string              `json:"yesLabel"`
	NoLabel    string              `json:"noLabel"`
	AllowOther *bool               `json:"allowOther"`
	TimeoutMs  int64               `json:"timeoutMs"`
	RequestID  string              `json:"requestId"`
}

func (p *approvalPlugin) execute(mode string) func(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return func(ctx context.Context, _ string, raw json.RawMessage, _ agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
		return p.run(ctx, mode, raw)
	}
}

func (p *approvalPlugin) run(ctx context.Context, mode string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	scope, ok := RunScopeFromContext(ctx)
	if !ok || scope.TenantID == uuid.Nil || scope.SessionID == uuid.Nil {
		return approvalFailure("run_scope_missing",
			"approve actions need the ambient run scope; a model tool call inside a run always has it", nil), nil
	}
	if p.opts.Decisions == nil {
		return approvalFailure("not_wired", "no decision broker is wired in this deployment", nil), nil
	}
	var args approvalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return approvalFailure("invalid_arguments", "arguments are not valid JSON: "+err.Error(), nil), nil
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return approvalFailure("invalid_arguments", "prompt is required", map[string]any{"field": "prompt"}), nil
	}
	timeout, err := approvalTimeout(args.TimeoutMs, p.opts.DefaultTimeout)
	if err != nil {
		return approvalFailure("invalid_arguments", err.Error(), map[string]any{"field": "timeoutMs"}), nil
	}
	options, allowFreeText, err := modeOptions(mode, args)
	if err != nil {
		return approvalFailure("invalid_arguments", err.Error(), nil), nil
	}
	requestID := approvalRequestID(scope, mode, prompt, options, args.RequestID)
	kind := approval.KindPrefix + mode
	decision, _, err := p.opts.Decisions.Open(ctx, OpenDecisionRequest{
		TenantID:      scope.TenantID,
		SessionID:     scope.SessionID,
		RequestID:     requestID,
		Kind:          kind,
		Prompt:        prompt,
		Options:       options,
		AllowFreeText: allowFreeText,
		Timeout:       timeout,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidDecision) {
			return approvalFailure("invalid_arguments", err.Error(), nil), nil
		}
		return agentcore.AgentToolResult{}, err
	}
	contextInfo := map[string]string{
		"runId":        scope.RunID,
		"requestId":    requestID,
		"decisionKind": kind,
	}
	if _, _, err := p.opts.Provenance.Open(ctx, approval.Record{
		TenantID:   scope.TenantID.String(),
		SessionID:  scope.SessionID.String(),
		DecisionID: decision.DecisionID,
		Kind:       kind,
		Mode:       mode,
		Actor:      "agent",
		Channel:    approval.ChannelTool,
		Context:    contextInfo,
	}); err != nil {
		p.opts.Logger.Warn("approval provenance (open) failed", "decision_id", decision.DecisionID, "err", err)
	}
	settled, err := p.opts.Decisions.Wait(ctx, scope.TenantID, scope.SessionID, uuid.MustParse(decision.DecisionID))
	if err != nil {
		if ctx.Err() != nil {
			cancelled := p.cancelQuestion(ctx, scope, decision.DecisionID, kind, mode)
			decision.Status = DecisionCancelled
			return approvalOutcome(mode, decision, cancelled, cancelled, true), nil
		}
		return agentcore.AgentToolResult{}, err
	}
	return p.settledResult(ctx, scope, mode, kind, settled), nil
}

// cancelQuestion turns a run cancellation into a durable cancelled decision.
// The wait context is already over, so the cancel runs on a detached context.
func (p *approvalPlugin) cancelQuestion(ctx context.Context, scope RunScope, decisionID, kind, mode string) approval.Record {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	p.opts.Decisions.CancelSession(cancelCtx, scope.TenantID, scope.SessionID, "run cancelled while waiting for approval")
	record := approval.Record{
		TenantID:   scope.TenantID.String(),
		SessionID:  scope.SessionID.String(),
		DecisionID: decisionID,
		Kind:       kind,
		Mode:       mode,
		Status:     approval.StatusCancelled,
		Context:    map[string]string{"reason": "run cancelled while waiting for approval"},
	}
	stored, _, err := p.opts.Provenance.RecordOutcome(cancelCtx, record)
	if err != nil {
		if !errors.Is(err, approval.ErrClosed) {
			p.opts.Logger.Warn("approval provenance (cancel) failed", "decision_id", decisionID, "err", err)
		}
		return record
	}
	return stored
}

func (p *approvalPlugin) settledResult(ctx context.Context, scope RunScope, mode, kind string, decision Decision) agentcore.AgentToolResult {
	record, found, err := p.opts.Provenance.Get(ctx, scope.TenantID, scope.SessionID, uuid.MustParse(decision.DecisionID))
	if err != nil {
		p.opts.Logger.Warn("approval provenance read failed", "decision_id", decision.DecisionID, "err", err)
	}
	// Reconcile the provenance with the durable decision. The row may still say
	// "open" when the answer arrived through a path that did not record
	// provenance (an older client, a direct broker answer); the tool is the
	// last observer and must not leave the audit behind the decision.
	switch decision.Status {
	case DecisionAnswered:
		if !found || record.Status != approval.StatusAnswered {
			record = p.backfill(ctx, scope, mode, kind, decision)
		}
		return approvalOutcome(mode, decision, record, record, false)
	case DecisionTimedOut:
		if !found || record.Status != approval.StatusTimedOut {
			record = p.recordOutcome(ctx, scope, mode, kind, decision, approval.StatusTimedOut)
		}
		return approvalOutcome(mode, decision, record, record, true)
	case DecisionCancelled:
		if !found || record.Status != approval.StatusCancelled {
			record = p.recordOutcome(ctx, scope, mode, kind, decision, approval.StatusCancelled)
		}
		return approvalOutcome(mode, decision, record, record, true)
	default:
		return approvalFailure("backend_error", "the decision did not reach a terminal state", map[string]any{
			"decisionId": decision.DecisionID, "status": string(decision.Status),
		})
	}
}

func (p *approvalPlugin) backfill(ctx context.Context, scope RunScope, mode, kind string, decision Decision) approval.Record {
	optionIDs := make([]string, 0, len(decision.Options))
	for _, option := range decision.Options {
		optionIDs = append(optionIDs, option.ID)
	}
	optionID, text := "", ""
	if decision.Answer != nil {
		optionID, text = decision.Answer.OptionID, decision.Answer.Text
	}
	answeredAt := time.Now().UTC()
	if decision.AnsweredAt != nil {
		answeredAt = *decision.AnsweredAt
	}
	record := approval.Backfill(kind, optionIDs, optionID, text, "", "", &decision.CreatedAt, answeredAt)
	record.TenantID = scope.TenantID.String()
	record.SessionID = scope.SessionID.String()
	record.DecisionID = decision.DecisionID
	record.Mode = mode
	stored, _, err := p.opts.Provenance.RecordAnswer(ctx, record)
	if err != nil {
		p.opts.Logger.Warn("approval provenance (backfill) failed", "decision_id", decision.DecisionID, "err", err)
		return record
	}
	return stored
}

func (p *approvalPlugin) recordOutcome(ctx context.Context, scope RunScope, mode, kind string, decision Decision, status string) approval.Record {
	record := approval.Record{
		TenantID:   scope.TenantID.String(),
		SessionID:  scope.SessionID.String(),
		DecisionID: decision.DecisionID,
		Kind:       kind,
		Mode:       mode,
		Status:     status,
	}
	stored, _, err := p.opts.Provenance.RecordOutcome(ctx, record)
	if err != nil {
		p.opts.Logger.Warn("approval provenance (outcome) failed", "decision_id", decision.DecisionID, "err", err)
		return record
	}
	return stored
}

func modeOptions(mode string, args approvalArgs) ([]DecisionOption, bool, error) {
	switch mode {
	case approval.ModeConfirm:
		yes, no := strings.TrimSpace(args.YesLabel), strings.TrimSpace(args.NoLabel)
		if yes == "" {
			yes = "Yes"
		}
		if no == "" {
			no = "No"
		}
		return []DecisionOption{{ID: "yes", Label: yes}, {ID: "no", Label: no}}, false, nil
	case approval.ModePrompt:
		return nil, true, nil
	case approval.ModeChoice:
		if len(args.Options) == 0 {
			return nil, false, fmt.Errorf("choice requires at least one option")
		}
		seen := make(map[string]struct{}, len(args.Options))
		options := make([]DecisionOption, 0, len(args.Options))
		for _, option := range args.Options {
			id := strings.TrimSpace(option.ID)
			if id == "" {
				return nil, false, fmt.Errorf("every choice option needs an id")
			}
			if _, dup := seen[id]; dup {
				return nil, false, fmt.Errorf("duplicate choice option id %q", id)
			}
			seen[id] = struct{}{}
			options = append(options, DecisionOption{ID: id, Label: option.Label, Description: option.Description})
		}
		allowOther := true
		if args.AllowOther != nil {
			allowOther = *args.AllowOther
		}
		return options, allowOther, nil
	default:
		return nil, false, fmt.Errorf("unknown approval mode %q", mode)
	}
}

func approvalTimeout(requested int64, fallback time.Duration) (time.Duration, error) {
	if requested == 0 {
		return fallback, nil
	}
	timeout := time.Duration(requested) * time.Millisecond
	if timeout < approvalMinTimeout || timeout > approvalMaxTimeout {
		return 0, fmt.Errorf("timeoutMs must be between %d and %d", approvalMinTimeout.Milliseconds(), approvalMaxTimeout.Milliseconds())
	}
	return timeout, nil
}

func approvalRequestID(scope RunScope, mode, prompt string, options []DecisionOption, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return "approve:" + mode + ":" + explicit
	}
	ids := make([]string, 0, len(options))
	for _, option := range options {
		ids = append(ids, option.ID)
	}
	sort.Strings(ids)
	runKey := scope.RunID
	if runKey == "" {
		runKey = scope.SessionID.String()
	}
	sum := sha256.Sum256([]byte(mode + "\x00" + prompt + "\x00" + strings.Join(ids, "\x00")))
	return "approve:" + runKey + ":" + hex.EncodeToString(sum[:12])
}

// approvalOutcome renders the settled question. `validated` is the answer's
// provenance, not a success flag: an answered "no" is a validated refusal.
func approvalOutcome(mode string, decision Decision, record, provenance approval.Record, isError bool) agentcore.AgentToolResult {
	document := map[string]any{
		"status":     string(decision.Status),
		"mode":       mode,
		"decisionId": decision.DecisionID,
		"prompt":     decision.Prompt,
		"options":    decision.Options,
		"deadline":   decision.Deadline,
		"validated":  record.Validated,
		"escape":     record.Escape,
		"provenance": provenance,
	}
	if decision.Answer != nil {
		document["answer"] = map[string]any{
			"optionId":  decision.Answer.OptionID,
			"text":      decision.Answer.Text,
			"validated": record.Validated,
			"escape":    record.Escape,
		}
	}
	if string(decision.Status) == approval.StatusTimedOut && mode == approval.ModeConfirm {
		// The safe side of "did the human approve?" is "no". The outcome is
		// still timed_out; this is the refusal downstream should apply.
		document["safeSide"] = map[string]any{"optionId": "no"}
	}
	raw, err := json.Marshal(document)
	if err != nil {
		raw = []byte(`{"status":"failed","error":"the result document could not be encoded"}`)
	}
	result := agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: document,
	}
	if isError {
		failure := true
		result.IsError = &failure
	}
	return result
}

func approvalFailure(code, message string, extra map[string]any) agentcore.AgentToolResult {
	document := map[string]any{
		"status":  "failed",
		"code":    code,
		"message": strings.TrimSpace(message),
	}
	for key, value := range extra {
		document[key] = value
	}
	raw, _ := json.Marshal(document)
	failure := true
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: document,
		IsError:           &failure,
	}
}

// ApproveBackend adapts the plugin's internal action handlers to the
// harness-owned `approve` aggregate tool.
type ApproveBackend struct {
	Manager func() *Manager
}

var approveBackendActions = map[string]string{
	"confirm": ApprovalToolConfirm,
	"prompt":  ApprovalToolPrompt,
	"choice":  ApprovalToolChoice,
}

// Invoke runs one approve action through the plugin manager. A disabled plugin
// returns ErrPluginState/ErrPluginNotFound, which the tool maps to
// plugin_disabled.
func (b ApproveBackend) Invoke(ctx context.Context, action string, args json.RawMessage) (agentcore.AgentToolResult, error) {
	tool, ok := approveBackendActions[action]
	if !ok {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: unknown approve action %q", action)
	}
	manager := (*Manager)(nil)
	if b.Manager != nil {
		manager = b.Manager()
	}
	if manager == nil {
		return agentcore.AgentToolResult{}, fmt.Errorf("agentext: %w: the plugin manager is not wired", ErrPluginState)
	}
	return manager.InvokeToolResult(ctx, ApprovalPluginID, tool, args)
}
