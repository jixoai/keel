package mediaadapter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
)

// DecisionExecutor is the tool-facing half of the System One adapter. It is an
// interface so a configured provider with no credential can still expose the
// tool and fail closed with a typed missing_credentials error instead of
// disappearing from the registry.
type DecisionExecutor interface {
	Decide(ctx context.Context, req DecisionRequest) (*DecisionOutcome, error)
}

// DecisionToolArgs is the model-facing decision_ask input. State and questions
// may be supplied per call; when omitted, the provider's saved binding is the
// fallback. The policy (mode/failPolicy/threshold) is never model-supplied: it
// comes from the binding, so a model cannot switch an enforced decision to
// shadow or fail-open.
type DecisionToolArgs struct {
	State     json.RawMessage             `json:"state,omitempty"`
	Questions map[string]DecisionQuestion `json:"questions,omitempty"`
}

// DecisionToolOutput is the decision result. Answers are the provider's typed
// objects verbatim. Answered=false is the only permitted shadow-failure shape:
// Success reports that the policy-compliant call completed, not that an answer
// exists. No noul/choice/score default is ever synthesized.
type DecisionToolOutput struct {
	Success   bool                       `json:"success"`
	Answered  bool                       `json:"answered"`
	Mode      string                     `json:"mode"`
	Shadow    bool                       `json:"shadow,omitempty"`
	Answers   map[string]json.RawMessage `json:"answers,omitempty"`
	Failure   string                     `json:"failure,omitempty"`
	Retryable bool                       `json:"retryable,omitempty"`
	Model     string                     `json:"model,omitempty"`
	RequestID string                     `json:"requestId,omitempty"`
	Usage     Usage                      `json:"usage"`
}

// DecisionTool exposes one configured System One provider as `decision_ask`.
type DecisionTool struct {
	Executor DecisionExecutor
	// Policy is the binding's enforce/shadow + fail policy. The default (zero)
	// normalizes to enforce/fail_closed inside the adapter.
	Policy DecisionPolicy
	// FallbackState/FallbackQuestions are the saved binding. They are used only
	// when the call omits the corresponding argument.
	FallbackState     json.RawMessage
	FallbackQuestions map[string]DecisionQuestion
}

// NewDecisionTool returns the decision_ask tool.
func NewDecisionTool(executor DecisionExecutor, policy DecisionPolicy, state json.RawMessage, questions map[string]DecisionQuestion) *DecisionTool {
	return &DecisionTool{
		Executor:          executor,
		Policy:            policy,
		FallbackState:     state,
		FallbackQuestions: questions,
	}
}

// Name implements agentcore.AgentTool.
func (t *DecisionTool) Name() string { return "decision_ask" }

// Description implements agentcore.AgentTool.
func (t *DecisionTool) Description() string {
	return "Ask the configured System One decision provider typed questions about a state. " +
		"Returns the provider's typed answers only; policy is the deployment's enforce/shadow setting and cannot be chosen by the model."
}

// Schema implements agentcore.AgentTool.
func (t *DecisionTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "state": {"description": "The decision state: a string, object or array. Remote http(s) URLs are rejected."},
	    "questions": {
	      "type": "object",
	      "description": "Questions keyed by id. Omit to use the provider's saved binding questions.",
	      "additionalProperties": {"type": "object"}
	    }
	  },
	  "additionalProperties": false
	}`)
}

// ExecutionMode implements agentcore.AgentTool.
func (t *DecisionTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}

// Execute implements agentcore.AgentTool.
func (t *DecisionTool) Execute(ctx context.Context, _ string, args json.RawMessage, _ agentcore.ToolUpdateFunc) (result agentcore.AgentToolResult, err error) {
	defer recoverToolPanic(FamilyDecision, &result, &err)
	var input DecisionToolArgs
	if len(strings.TrimSpace(string(args))) > 0 {
		if err := json.Unmarshal(args, &input); err != nil {
			return toolFailure("arguments are not valid JSON: "+err.Error(), ToolCodeInvalidArguments, false, nil, nil), nil
		}
	}
	session, ok := artifactSession(ctx)
	if !ok {
		return toolFailure("the decision adapter has no session for its call record",
			ToolCodeSessionMissing, false, nil, nil), nil
	}
	if t.Executor == nil {
		return toolFailure("decision provider is not configured", string(KindConfig), false, nil, decisionEcho(input, t)), nil
	}

	state := input.State
	usedSavedState := false
	if !hasJSONValue(state) {
		state = t.FallbackState
		usedSavedState = hasJSONValue(state)
	}
	questions := input.Questions
	usedSavedQuestions := false
	if len(questions) == 0 {
		questions = t.FallbackQuestions
		usedSavedQuestions = len(questions) > 0
	}
	echo := decisionEcho(input, t)
	echo.UsedSavedState = usedSavedState
	echo.UsedSavedQuestions = usedSavedQuestions
	echo.Questions = len(questions)

	request := DecisionRequest{
		State:     state,
		Questions: questions,
		SessionID: session,
		Policy:    t.Policy,
	}
	if scope, ok := agentcore.ToolRunScopeFromContext(ctx); ok {
		request.User = scope.TenantID
	}
	outcome, decideErr := t.Executor.Decide(ctx, request)
	if decideErr != nil {
		return toolFailureFromError(FamilyDecision, decideErr, echo), nil
	}
	if outcome == nil {
		return toolFailure("decision provider returned no outcome",
			string(KindProviderUnavailable), true, nil, echo), nil
	}
	output := DecisionToolOutput{
		Success:   true,
		Answered:  len(outcome.Answers) > 0,
		Mode:      outcome.Mode,
		Shadow:    outcome.Shadow,
		Answers:   outcome.Answers,
		Failure:   outcome.Failure,
		Retryable: outcome.Retryable,
		Model:     outcome.Model,
		RequestID: outcome.RequestID,
		Usage:     outcome.Usage,
	}
	raw, _ := json.Marshal(output)
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: output,
		Details: map[string]any{
			"decision": map[string]any{
				"mode":      output.Mode,
				"shadow":    output.Shadow,
				"answered":  output.Answered,
				"requestId": output.RequestID,
				"usage":     output.Usage,
			},
		},
	}, nil
}

type decisionRequestEcho struct {
	HasState           bool `json:"has_state"`
	Questions          int  `json:"questions,omitempty"`
	UsedSavedState     bool `json:"used_saved_state,omitempty"`
	UsedSavedQuestions bool `json:"used_saved_questions,omitempty"`
}

func decisionEcho(input DecisionToolArgs, tool *DecisionTool) decisionRequestEcho {
	echo := decisionRequestEcho{HasState: hasJSONValue(input.State)}
	if echo.HasState {
		echo.Questions = len(input.Questions)
		return echo
	}
	if tool != nil && hasJSONValue(tool.FallbackState) {
		echo.HasState = true
	}
	echo.Questions = len(input.Questions)
	return echo
}

// hasJSONValue reports whether a raw JSON field carries a value other than
// absent/null. An explicit null is not a state or a question set.
func hasJSONValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

var _ agentcore.AgentTool = (*DecisionTool)(nil)
