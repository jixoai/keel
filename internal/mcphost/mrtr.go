// MRTR execution (change feat-mcp-apps-events, P5.2/P5.3).
//
// The executor turns one round of input requests into answers:
//
//   - elicitation/create  → a decision (the existing approval/question
//     primitive), with the requested schema projected onto options / free text.
//     A URL-mode request shows the URL in the prompt and never opens it.
//   - sampling/createMessage → the session's model service, behind the
//     KEEL_MCP_SAMPLING switch, a maxTokens cap and a per-call request budget.
//     Only objective usage is counted; no price is written anywhere.
//   - roots/list          → refused (-32601): keel has no roots handler and
//     fabricating one would be a lie.
//   - unknown method      → refused (-32601) and audited, never silently
//     omitted (the server would read omission as collaboration).
//
// The same executor serves legacy server-initiated requests: when a request
// arrives while exactly one tool call is executing, internal/mcp hands it here
// with that call's context (and therefore its tenant/session scope). A request
// outside any call has no trustworthy session and is refused by the dispatcher.
//
// Nothing here writes the event ledger: an elicitation is a decision (its own
// durable primitive), sampling is a model call with an audit row, roots and
// refusals are protocol facts. The spec's "账本零新增 kind" holds by
// construction.

package mcphost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/mcp"
)

// SamplingMode is the KEEL_MCP_SAMPLING switch.
type SamplingMode string

const (
	// SamplingOff never executes sampling/createMessage.
	SamplingOff SamplingMode = "off"
	// SamplingSession (default) executes sampling only while a session tool call
	// is running: the call's (tenant, session) scope must be on the context.
	SamplingSession SamplingMode = "session"
	// SamplingOn executes sampling from any executor context (still audited and
	// budgeted). It exists for hosts that deliberately allow it; the keel
	// deployment default is session.
	SamplingOn SamplingMode = "on"
)

// SamplingModeFromEnvironment reads KEEL_MCP_SAMPLING. A missing value is
// session; an unknown value fails closed to off (an unrecognized switch must
// never silently authorize extra model calls).
func SamplingModeFromEnvironment(getenv func(string) string) SamplingMode {
	if getenv == nil {
		return SamplingSession
	}
	switch SamplingMode(strings.TrimSpace(strings.ToLower(getenv("KEEL_MCP_SAMPLING")))) {
	case "":
		return SamplingSession
	case SamplingOff:
		return SamplingOff
	case SamplingSession:
		return SamplingSession
	case SamplingOn:
		return SamplingOn
	default:
		return SamplingOff
	}
}

// Default sampling bounds (design §4.6).
const (
	DefaultSamplingMaxTokens = 4096
	DefaultSamplingPerCall   = 3
	// maxElicitationPromptBytes bounds the server-supplied prompt text shown to
	// the user.
	maxElicitationPromptBytes = 4096
	// maxElicitationProperties bounds one requestedSchema. A server that wants
	// more than this is not asking a form question; rendering it as a decision
	// would be a denial-of-service surface.
	maxElicitationProperties = 32
	// maxElicitationEnumOptions bounds the projected options of one schema (all
	// properties together), so the decision stays renderable.
	maxElicitationEnumOptions = 64
	// maxElicitationEnumLiteralBytes bounds one enum literal (raw JSON bytes).
	maxElicitationEnumLiteralBytes = 4 << 10
)

// SamplingMessage is one text message of a sampling request.
type SamplingMessage struct {
	Role string
	Text string
}

// SamplingRequest is the executor's normalized sampling input.
type SamplingRequest struct {
	Messages         []SamplingMessage
	SystemPrompt     string
	MaxTokens        int
	Temperature      *float64
	ModelPreferences json.RawMessage
}

// SamplingResult is the model's answer in the CreateMessageResult shape.
type SamplingResult struct {
	Role       string
	Text       string
	Model      string
	StopReason string
}

// Sampler executes one sampling/createMessage against the session's model
// service. A nil sampler refuses sampling (fail closed).
type Sampler interface {
	Sample(ctx context.Context, request SamplingRequest) (SamplingResult, error)
}

// SamplerFunc adapts a function to Sampler.
type SamplerFunc func(ctx context.Context, request SamplingRequest) (SamplingResult, error)

// Sample implements Sampler.
func (f SamplerFunc) Sample(ctx context.Context, request SamplingRequest) (SamplingResult, error) {
	return f(ctx, request)
}

// DecisionBridge is the decision primitive the executor opens elicitations on.
// *agentext.DecisionBroker implements it.
type DecisionBridge interface {
	Open(ctx context.Context, request agentext.OpenDecisionRequest) (agentext.Decision, bool, error)
	Wait(ctx context.Context, tenantID, sessionID, decisionID uuid.UUID) (agentext.Decision, error)
}

// MRTROptions configures the executor. The zero value is a safe deployment:
// sampling is session-scoped, elicitation is enabled (the serve wiring installs
// the decision bridge), bounds are the defaults, and no auditor is wired.
type MRTROptions struct {
	Logger  *slog.Logger
	Auditor Auditor
	Metrics *Metrics
	// SamplingMode defaults to SamplingSession.
	SamplingMode SamplingMode
	// SamplingMaxTokens caps every sampling request (default 4096).
	SamplingMaxTokens int
	// SamplingPerCall bounds sampling requests per tool call (default 3).
	SamplingPerCall int
	// DisableElicitation makes the executor refuse elicitation and keeps the
	// capability out of ClientCapabilities. A deployment without the decision
	// primitive sets it.
	DisableElicitation bool
	// Now is the clock (tests). Nil uses time.Now.
	Now func() time.Time
}

// MRTR is the process-wide MRTR/legacy-server-request executor.
type MRTR struct {
	options MRTROptions
	logger  *slog.Logger
	now     func() time.Time

	mu        sync.RWMutex
	decisions DecisionBridge
	sampler   Sampler
}

// NewMRTR builds the executor. The decision bridge and sampler are installed by
// the hosting process (they depend on services built after the MCP connections
// are configured); until then the corresponding requests fail closed with a
// clear error, never with a fabricated answer.
func NewMRTR(options MRTROptions) *MRTR {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.SamplingMode == "" {
		options.SamplingMode = SamplingSession
	}
	if options.SamplingMaxTokens <= 0 {
		options.SamplingMaxTokens = DefaultSamplingMaxTokens
	}
	if options.SamplingPerCall <= 0 {
		options.SamplingPerCall = DefaultSamplingPerCall
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &MRTR{options: options, logger: options.Logger, now: options.Now}
}

// SetDecisions installs the decision primitive.
func (m *MRTR) SetDecisions(bridge DecisionBridge) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.decisions = bridge
	m.mu.Unlock()
}

// SetSampler installs the sampling executor (the session model service).
func (m *MRTR) SetSampler(sampler Sampler) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.sampler = sampler
	m.mu.Unlock()
}

// SamplingEnabled reports whether sampling is configured at all.
func (m *MRTR) SamplingEnabled() bool {
	if m == nil {
		return false
	}
	return m.options.SamplingMode != SamplingOff
}

// ElicitationEnabled reports whether elicitation is configured at all.
func (m *MRTR) ElicitationEnabled() bool {
	if m == nil {
		return false
	}
	return !m.options.DisableElicitation
}

// ClientCapabilities is what initialize may honestly declare: exactly the
// request types this executor answers. Roots is never declared (it is refused).
func (m *MRTR) ClientCapabilities() mcp.ClientCapabilities {
	capabilities := mcp.ClientCapabilities{}
	if m.SamplingEnabled() {
		capabilities.Sampling = map[string]json.RawMessage{}
	}
	if m.ElicitationEnabled() {
		capabilities.Elicitation = map[string]json.RawMessage{}
	}
	return capabilities
}

// HandleInputRequests executes one MRTR round. Any refused request fails the
// whole round (the wire has no per-key error variant); a budget error ends the
// call through the client's round budget.
func (m *MRTR) HandleInputRequests(ctx context.Context, server string, request mcp.InputRequiredResult, state *mcp.InputRequestState) (map[string]json.RawMessage, error) {
	if state == nil {
		state = &mcp.InputRequestState{}
	}
	keys := make([]string, 0, len(request.InputRequests))
	for key := range request.InputRequests {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	responses := make(map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		input := request.InputRequests[key]
		var result json.RawMessage
		var err error
		switch input.Method {
		case "elicitation/create":
			result, err = m.elicit(ctx, server, key, input, request, state)
		case "sampling/createMessage":
			result, err = m.sample(ctx, server, input, state)
		case "roots/list":
			m.audit(ctx, server, "mrtr.roots", "refused", "roots/list is not supported")
			err = &mcp.McpError{Code: mcp.CodeMethodNotFound, Message: "roots/list is not supported"}
		default:
			m.audit(ctx, server, "mrtr.method", "refused", "unknown input method: "+bounded(input.Method, 128))
			err = &mcp.McpError{Code: mcp.CodeMethodNotFound, Message: "Unsupported input request: " + bounded(input.Method, 128)}
		}
		if err != nil {
			return nil, err
		}
		responses[key] = result
	}
	return responses, nil
}

// HandleLegacyServerRequest answers a legacy server-initiated request through
// the same executor. The client only routes requests to it while a tool call is
// executing, so the context carries that call's tenant/session scope.
func (m *MRTR) HandleLegacyServerRequest(ctx context.Context, server string, request *mcp.Request) (mcp.Message, error) {
	round := mcp.InputRequiredResult{InputRequests: map[string]mcp.InputRequest{
		"server-request": {Method: request.Method, Params: request.Params},
	}}
	responses, err := m.HandleInputRequests(ctx, server, round, &mcp.InputRequestState{})
	if err != nil {
		return nil, err
	}
	result := responses["server-request"]
	if len(result) == 0 {
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "no response for " + request.Method}
	}
	return &mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: result}, nil
}

// --- elicitation ---------------------------------------------------------

// elicitProperty is one property of a requestedSchema.
type elicitProperty struct {
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Enum        []json.RawMessage `json:"enum"`
}

// elicitParams is the subset of ElicitRequest the host acts on.
type elicitParams struct {
	Mode            string `json:"mode"`
	Message         string `json:"message"`
	URL             string `json:"url"`
	RequestedSchema struct {
		Properties map[string]elicitProperty `json:"properties"`
		Required   []string                  `json:"required"`
	} `json:"requestedSchema"`
}

// validateElicitationSchema bounds the requestedSchema before the host projects
// it onto a decision. Everything rejected here is invalid params (-32602).
func validateElicitationSchema(params elicitParams) error {
	if len(params.RequestedSchema.Properties) > maxElicitationProperties {
		return &mcp.McpError{
			Code:    mcp.CodeInvalidParams,
			Message: fmt.Sprintf("elicitation schema has more than %d properties", maxElicitationProperties),
		}
	}
	options := 0
	for _, property := range params.RequestedSchema.Properties {
		for _, literal := range property.Enum {
			options++
			if len(literal) > maxElicitationEnumLiteralBytes {
				return &mcp.McpError{
					Code:    mcp.CodeInvalidParams,
					Message: fmt.Sprintf("elicitation enum literal exceeds %d bytes", maxElicitationEnumLiteralBytes),
				}
			}
		}
	}
	if options > maxElicitationEnumOptions {
		return &mcp.McpError{
			Code:    mcp.CodeInvalidParams,
			Message: fmt.Sprintf("elicitation schema has more than %d enum options", maxElicitationEnumOptions),
		}
	}
	return nil
}

// elicit executes one elicitation/create as a decision.
func (m *MRTR) elicit(ctx context.Context, server, key string, input mcp.InputRequest, round mcp.InputRequiredResult, state *mcp.InputRequestState) (json.RawMessage, error) {
	m.mu.RLock()
	decisions := m.decisions
	m.mu.RUnlock()
	if m.options.DisableElicitation || decisions == nil {
		// A capability this deployment does not serve is "method not found",
		// not an internal failure: the server asked for a method we never
		// declared (the spec allows a client to answer -32601 for an
		// undeclared/unsupported request). The no-scope case below stays an
		// internal error: the method is supported, the caller context is not.
		m.audit(ctx, server, "mrtr.elicitation", "refused", "the decision primitive is not wired")
		return nil, &mcp.McpError{Code: mcp.CodeMethodNotFound, Message: "elicitation is not supported in this deployment"}
	}
	scope, ok := agentcore.ToolRunScopeFromContext(ctx)
	if !ok {
		m.audit(ctx, server, "mrtr.elicitation", "refused", "no session scope")
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "elicitation requires a session scope"}
	}
	tenantID, err := uuid.Parse(scope.TenantID)
	if err != nil {
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "elicitation has an invalid tenant scope"}
	}
	sessionID, err := uuid.Parse(scope.SessionID)
	if err != nil {
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "elicitation has an invalid session scope"}
	}
	var params elicitParams
	if len(input.Params) > 0 {
		if err := json.Unmarshal(input.Params, &params); err != nil {
			return nil, &mcp.McpError{Code: mcp.CodeInvalidParams, Message: "elicitation params are invalid"}
		}
	}
	// The schema is attacker-controlled: bound it before any decision is opened
	// or any option is projected.
	if err := validateElicitationSchema(params); err != nil {
		return nil, err
	}
	mode := strings.ToLower(strings.TrimSpace(params.Mode))
	if mode == "" {
		mode = "form"
	}
	if mode != "form" && mode != "url" {
		return nil, &mcp.McpError{Code: mcp.CodeInvalidParams, Message: "unsupported elicitation mode: " + bounded(mode, 32)}
	}

	plan := elicitationPlanFor(params, mode)
	requestID := elicitationRequestID(server, key, round, state)
	decision, _, err := decisions.Open(ctx, agentext.OpenDecisionRequest{
		TenantID:      tenantID,
		SessionID:     sessionID,
		RequestID:     requestID,
		Kind:          "mcp.elicitation",
		Prompt:        plan.prompt,
		Options:       plan.options,
		AllowFreeText: plan.freeTextField != "",
	})
	if err != nil {
		m.audit(ctx, server, "mrtr.elicitation", "error", "open decision failed")
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "elicitation decision could not be opened"}
	}
	decisionID, err := uuid.Parse(decision.DecisionID)
	if err != nil {
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "elicitation decision has an invalid id"}
	}
	answered, waitErr := decisions.Wait(ctx, tenantID, sessionID, decisionID)
	if waitErr != nil {
		// A call timeout or cancellation is the user not answering: the spec's
		// answer is action=cancel (never a fabricated accept).
		m.audit(ctx, server, "mrtr.elicitation", "cancelled", "the decision wait ended")
		return elicitResult("cancel", nil)
	}
	action, content := plan.answer(answered)
	result, err := elicitResult(action, content)
	if err != nil {
		return nil, err
	}
	resultName := "ok"
	if action != "accept" {
		resultName = strings.ToLower(action)
	}
	m.audit(ctx, server, "mrtr.elicitation", resultName,
		fmt.Sprintf("mode=%s fields=%d", mode, len(params.RequestedSchema.Properties)))
	return result, nil
}

// elicitationValue is the accepted value of one projected enum option.
type elicitationValue struct {
	field string
	value any
}

// elicitationPlan is the decision projection of a requestedSchema.
type elicitationPlan struct {
	prompt        string
	options       []agentext.DecisionOption
	freeTextField string
	values        map[string]elicitationValue
}

// elicitationPlanFor builds the prompt and options from the requested schema.
// Enum values become options; the first non-enum property enables free text; URL
// mode shows the URL in an option description (the host never opens it
// automatically).
func elicitationPlanFor(params elicitParams, mode string) elicitationPlan {
	message := strings.TrimSpace(params.Message)
	if message == "" {
		message = "The MCP server requests input."
	}
	message = bounded(message, maxElicitationPromptBytes)
	properties := make([]string, 0, len(params.RequestedSchema.Properties))
	for name := range params.RequestedSchema.Properties {
		properties = append(properties, name)
	}
	sort.Strings(properties)
	plan := elicitationPlan{prompt: message, values: map[string]elicitationValue{}}
	for fieldIndex, name := range properties {
		property := params.RequestedSchema.Properties[name]
		title := strings.TrimSpace(property.Title)
		if title == "" {
			title = name
		}
		if len(property.Enum) > 0 && mode == "form" {
			for valueIndex, raw := range property.Enum {
				literal := enumLiteral(raw)
				// Field and value indices keep option ids unique across properties;
				// a duplicate id would make the decision primitive reject the open.
				id := fmt.Sprintf("v%d_%d", fieldIndex, valueIndex)
				plan.options = append(plan.options, agentext.DecisionOption{
					ID:          id,
					Label:       bounded(enumString(literal), 128),
					Description: bounded(title, 128),
				})
				plan.values[id] = elicitationValue{field: name, value: literal}
			}
			continue
		}
		if mode == "form" && plan.freeTextField == "" && property.Type != "boolean" {
			plan.freeTextField = name
			if description := strings.TrimSpace(property.Description); description != "" {
				plan.prompt += "\n" + bounded(title, 128) + ": " + bounded(description, 256)
			}
		}
	}
	if mode == "url" {
		plan.options = append(plan.options, agentext.DecisionOption{
			ID:          "accept",
			Label:       "同意将该链接交给 View 打开",
			Description: bounded(strings.TrimSpace(params.URL), 256),
		})
	}
	if len(plan.options) == 0 {
		plan.options = append(plan.options, agentext.DecisionOption{ID: "accept", Label: "接受"})
	}
	plan.options = append(plan.options,
		agentext.DecisionOption{ID: "decline", Label: "拒绝"},
		agentext.DecisionOption{ID: "cancel", Label: "取消"},
	)
	return plan
}

// answer maps a terminal decision onto an ElicitResult action and content.
// An explicit decline/cancel option always wins over free text: the decision
// UI may leave typed text in the box when the user picks a terminal option,
// and forwarding that text as an accept would turn a refusal into consent.
func (p elicitationPlan) answer(decision agentext.Decision) (string, map[string]any) {
	if decision.Status != agentext.DecisionAnswered || decision.Answer == nil {
		return "cancel", nil
	}
	answer := decision.Answer
	switch answer.OptionID {
	case "decline":
		return "decline", nil
	case "cancel":
		return "cancel", nil
	case "":
		if answer.Text == "" {
			return "cancel", nil
		}
	}
	// An explicit projected option wins over leftover free text for the same
	// reason: a chosen option is the answer, not an input method.
	if projected, ok := p.values[answer.OptionID]; ok {
		return "accept", map[string]any{projected.field: projected.value}
	}
	if answer.Text != "" {
		if p.freeTextField == "" {
			return "accept", nil
		}
		return "accept", map[string]any{p.freeTextField: answer.Text}
	}
	// "accept" (URL mode or a schema-free request) records consent only; a link
	// is opened through ui/open-link, never from here.
	return "accept", nil
}

// elicitResult renders an ElicitResult.
func elicitResult(action string, content map[string]any) (json.RawMessage, error) {
	payload := map[string]any{"action": action}
	if len(content) > 0 {
		payload["content"] = content
	}
	return json.Marshal(payload)
}

// enumLiteral decodes one enum value into the Go value the content map carries
// (string, bool, or number). An undecodable literal stays a string.
func enumLiteral(raw json.RawMessage) any {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number
	}
	return strings.TrimSpace(string(raw))
}

// enumString renders an enum literal for an option label.
func enumString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return fmt.Sprintf("%t", typed)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// elicitationRequestID is deterministic per call/key/round/state so a retried
// round finds the same decision instead of opening a duplicate question, while
// two parallel calls (different CallID) never share one.
func elicitationRequestID(server, key string, round mcp.InputRequiredResult, state *mcp.InputRequestState) string {
	callID := ""
	roundNumber := 0
	if state != nil {
		callID = state.CallID
		roundNumber = state.Round
	}
	seed := server + "\x00" + callID + "\x00" + key + "\x00" + string(round.RequestState) + "\x00" + fmt.Sprintf("%d", roundNumber)
	sum := sha256.Sum256([]byte(seed))
	return "mcp-mrtr:" + hex.EncodeToString(sum[:12])
}

// --- sampling ------------------------------------------------------------

// sampleParams is the subset of CreateMessageRequest the host acts on.
type sampleParams struct {
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	SystemPrompt     string          `json:"systemPrompt"`
	MaxTokens        int             `json:"maxTokens"`
	Temperature      *float64        `json:"temperature"`
	ModelPreferences json.RawMessage `json:"modelPreferences"`
}

// sample executes one sampling/createMessage against the session model service.
func (m *MRTR) sample(ctx context.Context, server string, input mcp.InputRequest, state *mcp.InputRequestState) (json.RawMessage, error) {
	mode := m.options.SamplingMode
	if mode == SamplingOff {
		m.audit(ctx, server, "mrtr.sampling", "refused", "sampling is disabled")
		return nil, &mcp.McpError{Code: mcp.CodeMethodNotFound, Message: "sampling is disabled on this host"}
	}
	if mode == SamplingSession {
		if _, ok := agentcore.ToolRunScopeFromContext(ctx); !ok {
			m.audit(ctx, server, "mrtr.sampling", "refused", "no session scope")
			return nil, &mcp.McpError{Code: mcp.CodeMethodNotFound, Message: "sampling is only available inside a session"}
		}
	}
	m.mu.RLock()
	sampler := m.sampler
	m.mu.RUnlock()
	if sampler == nil {
		m.audit(ctx, server, "mrtr.sampling", "refused", "no model service")
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "sampling has no model service"}
	}
	if state.SamplingCalls >= m.options.SamplingPerCall {
		m.audit(ctx, server, "mrtr.sampling", "refused", "per-call budget exhausted")
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "sampling budget exhausted for this tool call"}
	}
	var params sampleParams
	if len(input.Params) > 0 {
		if err := json.Unmarshal(input.Params, &params); err != nil {
			return nil, &mcp.McpError{Code: mcp.CodeInvalidParams, Message: "sampling params are invalid"}
		}
	}
	if len(params.Messages) == 0 {
		return nil, &mcp.McpError{Code: mcp.CodeInvalidParams, Message: "sampling messages are empty"}
	}
	messages := make([]SamplingMessage, 0, len(params.Messages))
	for _, message := range params.Messages {
		text, err := samplingText(message.Content)
		if err != nil {
			return nil, &mcp.McpError{Code: mcp.CodeInvalidParams, Message: err.Error()}
		}
		messages = append(messages, SamplingMessage{Role: message.Role, Text: text})
	}
	maxTokens := params.MaxTokens
	if maxTokens <= 0 || maxTokens > m.options.SamplingMaxTokens {
		maxTokens = m.options.SamplingMaxTokens
	}
	state.SamplingCalls++
	answer, err := sampler.Sample(ctx, SamplingRequest{
		Messages:         messages,
		SystemPrompt:     params.SystemPrompt,
		MaxTokens:        maxTokens,
		Temperature:      params.Temperature,
		ModelPreferences: params.ModelPreferences,
	})
	if err != nil {
		m.audit(ctx, server, "mrtr.sampling", "error", "model call failed")
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "sampling failed: " + bounded(err.Error(), 256)}
	}
	stopReason := answer.StopReason
	if stopReason == "" {
		stopReason = "endTurn"
	}
	role := answer.Role
	if role == "" {
		role = "assistant"
	}
	result, err := json.Marshal(map[string]any{
		"role":       role,
		"content":    map[string]any{"type": "text", "text": answer.Text},
		"model":      answer.Model,
		"stopReason": stopReason,
	})
	if err != nil {
		return nil, &mcp.McpError{Code: mcp.CodeInternalError, Message: "sampling result could not be encoded"}
	}
	m.audit(ctx, server, "mrtr.sampling", "ok",
		fmt.Sprintf("messages=%d maxTokens=%d", len(messages), maxTokens))
	return result, nil
}

// samplingText extracts text from a sampling content value: one block object or
// an array of blocks. Non-text blocks are refused (the executor only speaks
// text; pretending to interpret an image would be worse).
func samplingText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var blocks []json.RawMessage
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", fmt.Errorf("sampling content is invalid")
		}
	} else {
		blocks = []json.RawMessage{raw}
	}
	var builder strings.Builder
	for _, block := range blocks {
		var fields map[string]json.RawMessage
		if json.Unmarshal(block, &fields) != nil {
			return "", fmt.Errorf("sampling content blocks must be objects")
		}
		var kind, text string
		_ = json.Unmarshal(fields["type"], &kind)
		_ = json.Unmarshal(fields["text"], &text)
		if kind != "text" {
			return "", fmt.Errorf("sampling content type %q is not supported", bounded(kind, 32))
		}
		builder.WriteString(text)
	}
	return builder.String(), nil
}

// RecordClientDiagnostic reports an MRTR protocol outcome the client observed
// (budget exceeded, executor missing/failed). It is the audit seam
// ConnectOptions.MRTRDiagnostics points at.
func (m *MRTR) RecordClientDiagnostic(server, kind, detail string) {
	if m == nil {
		return
	}
	m.audit(context.Background(), server, "mrtr."+bounded(kind, 32), "refused", detail)
}

// RecordRounds records one finished exchange in the mcp_mrtr_rounds histogram.
func (m *MRTR) RecordRounds(rounds int) {
	if m == nil || m.options.Metrics == nil || rounds <= 0 {
		return
	}
	m.options.Metrics.MRTRRound(rounds)
}

// --- audit ---------------------------------------------------------------

func (m *MRTR) audit(ctx context.Context, server, action, result, detail string) {
	if m == nil {
		return
	}
	level := slog.LevelInfo
	if result != "ok" {
		level = slog.LevelWarn
	}
	m.logger.Log(ctx, level, "mcp mrtr", "server", server, "action", action,
		"result", result, "detail", bounded(detail, 300))
	if m.options.Auditor != nil {
		m.options.Auditor.RecordMCPApp(ctx, AuditEvent{
			Server: server,
			Action: action,
			Result: result,
			Detail: bounded(detail, 300),
		})
	}
}
