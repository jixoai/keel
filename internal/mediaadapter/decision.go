package mediaadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// System One protocol identifiers. The decision family is not a chat model:
// the request is state + typed questions and the response is typed answers.
const (
	SystemOneProtocol        = "systemone"
	SystemOneProtocolVersion = "0.2.0"
)

// Decision adapter kinds.
const (
	DecisionAdapterTypeSafe   = "typesafe"
	DecisionAdapterCloudflare = "cloudflare"
	DecisionAdapterOpenRouter = "openrouter"
	DecisionAdapterOpenJev    = "openjev"
)

// Decision question types.
const (
	QuestionNoul   = "noul"
	QuestionChoice = "choice"
	QuestionScore  = "score"
	QuestionBox    = "box"
)

// Decision binding modes.
const (
	DecisionModeShadow  = "shadow"
	DecisionModeEnforce = "enforce"
)

// Decision failure policies.
const (
	DecisionFailClosed = "fail_closed"
	DecisionFailOpen   = "fail_open"
)

// DecisionQuestion is one typed question. Instructions and Criteria are raw
// JSON because System One accepts a string, object or array for each; the
// adapter validates the allowed kind before sending.
type DecisionQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// DecisionPolicy is the binding-level policy. It contains decision semantics
// only: no temperature, tools, stream or max_tokens (spec decision-provider).
type DecisionPolicy struct {
	Mode           string   `json:"mode"`
	FailPolicy     string   `json:"failPolicy"`
	Threshold      *float64 `json:"threshold,omitempty"`
	ConfidenceBand string   `json:"confidenceBand,omitempty"`
}

// DefaultDecisionPolicy is fail-closed enforcement. A gate never proceeds on a
// decision it could not obtain.
func DefaultDecisionPolicy() DecisionPolicy {
	return DecisionPolicy{Mode: DecisionModeEnforce, FailPolicy: DecisionFailClosed}
}

// DecisionRequest is one System One call.
type DecisionRequest struct {
	State     json.RawMessage             `json:"state"`
	Questions map[string]DecisionQuestion `json:"questions"`
	Model     string                      `json:"model,omitempty"`
	SessionID string                      `json:"session_id,omitempty"`
	User      string                      `json:"user,omitempty"`
	Policy    DecisionPolicy              `json:"-"`
}

// DecisionOutcome is the adapter result. Answers preserves the provider's
// typed answer objects verbatim: the adapter never adds a default answer, a
// confidence value or a rationale.
type DecisionOutcome struct {
	Answers   map[string]json.RawMessage `json:"answers,omitempty"`
	Model     string                     `json:"model,omitempty"`
	Usage     Usage                      `json:"usage"`
	Mode      string                     `json:"mode"`
	Shadow    bool                       `json:"shadow,omitempty"`
	Failure   string                     `json:"failure,omitempty"`
	Retryable bool                       `json:"retryable,omitempty"`
	RequestID string                     `json:"requestId,omitempty"`
}

// DecisionDescriptor is the capability declaration of one adapter. It is the
// fact sheet preflight and the settings UI read instead of assuming every
// decision backend supports every question type.
type DecisionDescriptor struct {
	Protocol        string   `json:"protocol"`
	ProtocolVersion string   `json:"protocolVersion"`
	Adapter         string   `json:"adapter"`
	ModelFamily     string   `json:"modelFamily"`
	Model           string   `json:"model,omitempty"`
	QuestionTypes   []string `json:"questionTypes"`
	InputModalities []string `json:"inputModalities"`
	// Confidence reports choice/score confidence support. NoulConfidence is
	// separate because a noul answer has no confidence field at all.
	Confidence              bool   `json:"confidence"`
	NoulConfidence          bool   `json:"noulConfidence"`
	Probabilities           bool   `json:"probabilities"`
	MaxQuestions            int    `json:"maxQuestions"`
	MaxChoices              int    `json:"maxChoices"`
	MaxLevels               int    `json:"maxLevels"`
	ContextWindow           int    `json:"contextWindow"`
	MaxBodyBytes            int64  `json:"maxBodyBytes"`
	MaxMediaBytes           int64  `json:"maxMediaBytes"`
	Streaming               bool   `json:"streaming"`
	BatchedQuestions        bool   `json:"batchedQuestions"`
	CrossQuestionVisibility bool   `json:"crossQuestionVisibility"`
	Rationale               bool   `json:"rationale"`
	AdvisoryOnly            bool   `json:"advisoryOnly"`
	NativeDecision          bool   `json:"nativeDecision"`
	Calibration             string `json:"calibration"`
}

// DecisionCapabilities returns the declaration for one adapter/model pair.
// An unknown adapter is an error, not a defaulted llm-shaped fallback.
func DecisionCapabilities(adapter, model string) (DecisionDescriptor, error) {
	adapter = normalizeAdapter(adapter)
	if adapter == "" {
		return DecisionDescriptor{}, configError(FamilyDecision, "capabilities",
			"adapter must be one of typesafe|cloudflare|openrouter|openjev")
	}
	model = strings.TrimSpace(model)
	family := "jev"
	if strings.Contains(strings.ToLower(model), "clef") || adapter == DecisionAdapterCloudflare {
		family = "clef"
	}
	descriptor := DecisionDescriptor{
		Protocol:                SystemOneProtocol,
		ProtocolVersion:         SystemOneProtocolVersion,
		Adapter:                 adapter,
		ModelFamily:             family,
		Model:                   model,
		Confidence:              true,
		Probabilities:           true,
		Streaming:               false,
		BatchedQuestions:        true,
		CrossQuestionVisibility: false,
		Rationale:               false,
		AdvisoryOnly:            true,
		NativeDecision:          true,
		Calibration:             "provider",
	}
	switch adapter {
	case DecisionAdapterTypeSafe:
		descriptor.QuestionTypes = []string{QuestionNoul, QuestionChoice, QuestionScore, QuestionBox}
		descriptor.InputModalities = []string{"text", "json", "image"}
		descriptor.MaxQuestions = 256
		descriptor.MaxChoices = 255
		descriptor.MaxLevels = 10
		descriptor.ContextWindow = 65536
		descriptor.MaxBodyBytes = 32 << 20
		descriptor.MaxMediaBytes = 16 << 20
	case DecisionAdapterCloudflare:
		// Clef is the same System One protocol behind Cloudflare's envelope. It
		// is a different model family, not a different request shape, and box is
		// not part of its surface.
		descriptor.QuestionTypes = []string{QuestionNoul, QuestionChoice, QuestionScore}
		descriptor.InputModalities = []string{"text", "json", "image", "video"}
		descriptor.MaxQuestions = 64
		descriptor.MaxChoices = 255
		descriptor.MaxLevels = 10
		descriptor.ContextWindow = 65536
		descriptor.MaxBodyBytes = 32 << 20
		descriptor.MaxMediaBytes = 16 << 20
	case DecisionAdapterOpenRouter:
		descriptor.QuestionTypes = []string{QuestionNoul, QuestionChoice, QuestionScore}
		descriptor.InputModalities = []string{"text", "json", "image"}
		descriptor.MaxQuestions = 64
		descriptor.MaxChoices = 255
		descriptor.MaxLevels = 10
		descriptor.ContextWindow = 65536
		descriptor.MaxBodyBytes = 32 << 20
		descriptor.MaxMediaBytes = 4 << 20
	case DecisionAdapterOpenJev:
		descriptor.QuestionTypes = []string{QuestionNoul, QuestionChoice, QuestionScore}
		descriptor.InputModalities = []string{"text", "json", "image"}
		descriptor.MaxQuestions = 64
		descriptor.MaxChoices = 255
		descriptor.MaxLevels = 10
		descriptor.ContextWindow = 65536
		descriptor.MaxBodyBytes = 32 << 20
		descriptor.MaxMediaBytes = 8 << 20
	default:
		return DecisionDescriptor{}, configError(FamilyDecision, "capabilities", "unknown decision adapter")
	}
	return descriptor, nil
}

func normalizeAdapter(adapter string) string {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case DecisionAdapterTypeSafe, "typesafe.ai", "type-safe":
		return DecisionAdapterTypeSafe
	case DecisionAdapterCloudflare, "clef", "cloudflare-clef", "@cf/cloudflare/clef":
		return DecisionAdapterCloudflare
	case DecisionAdapterOpenRouter, "open-router":
		return DecisionAdapterOpenRouter
	case DecisionAdapterOpenJev, "open-jev":
		return DecisionAdapterOpenJev
	default:
		return ""
	}
}

// DecisionConfig configures one decision provider adapter.
type DecisionConfig struct {
	Adapter string
	BaseURL string
	APIKey  string
	Model   string
	// Endpoint overrides the derived path/URL. For Cloudflare it defaults to
	// /ai/run/<model> under BaseURL.
	Endpoint         string
	ProtocolVersion  string
	Client           *http.Client
	MaxResponseBytes int64
	// AllowAnonymous permits an empty key (a local OpenJev server). It is off by
	// default so a missing credential is a typed error, not an anonymous call.
	AllowAnonymous bool
}

// DecisionClient is one configured System One adapter.
type DecisionClient struct {
	descriptor DecisionDescriptor
	wire       *wireClient
	endpoint   string
}

// NewDecisionClient validates and builds one decision adapter.
func NewDecisionClient(cfg DecisionConfig) (*DecisionClient, error) {
	descriptor, err := DecisionCapabilities(cfg.Adapter, cfg.Model)
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(cfg.ProtocolVersion)
	if version == "" {
		version = SystemOneProtocolVersion
	}
	if version != SystemOneProtocolVersion {
		return nil, configError(FamilyDecision, "config",
			fmt.Sprintf("protocol version %q is not supported; this adapter implements %s", version, SystemOneProtocolVersion))
	}
	if descriptor.Model == "" {
		return nil, requestError(FamilyDecision, "config", "model is required for a decision adapter", []FieldError{{
			Field: "model", Message: "a decision model id is required",
		}})
	}
	if strings.TrimSpace(cfg.APIKey) == "" && !cfg.AllowAnonymous {
		return nil, &Error{
			Family:  FamilyDecision,
			Op:      "config",
			Kind:    KindMissingCredentials,
			Message: "decision provider has no credential; refusing an anonymous request",
		}
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultDecisionBase(descriptor.Adapter)
	}
	if baseURL == "" {
		return nil, configError(FamilyDecision, "config",
			"cloudflare adapter requires an explicit base URL (https://api.cloudflare.com/client/v4/accounts/<id>)")
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultDecisionPath(descriptor.Adapter, descriptor.Model)
	}
	wire, err := newWireClient(baseURL, cfg.APIKey, cfg.Client, cfg.MaxResponseBytes)
	if err != nil {
		return nil, err
	}
	return &DecisionClient{descriptor: descriptor, wire: wire, endpoint: endpoint}, nil
}

func defaultDecisionBase(adapter string) string {
	switch adapter {
	case DecisionAdapterTypeSafe:
		return "https://api.typesafe.ai"
	case DecisionAdapterOpenRouter:
		return "https://openrouter.ai"
	case DecisionAdapterOpenJev:
		return "https://api.openjev.sh"
	default:
		return ""
	}
}

func defaultDecisionPath(adapter, model string) string {
	switch adapter {
	case DecisionAdapterTypeSafe, DecisionAdapterOpenJev:
		return "/v1/systemone"
	case DecisionAdapterOpenRouter:
		return "/api/alpha/decisions"
	case DecisionAdapterCloudflare:
		return "/ai/run/" + strings.TrimPrefix(model, "/")
	default:
		return "/v1/systemone"
	}
}

// Descriptor returns a copy of the capability declaration.
func (c *DecisionClient) Descriptor() DecisionDescriptor {
	if c == nil {
		return DecisionDescriptor{}
	}
	out := c.descriptor
	out.QuestionTypes = append([]string(nil), c.descriptor.QuestionTypes...)
	out.InputModalities = append([]string(nil), c.descriptor.InputModalities...)
	return out
}

// Preflight validates a binding before any provider call: question types,
// criteria limits, media shape, body size and question count. The returned
// field errors are what a settings form highlights; an empty slice means the
// call is admissible.
func (c *DecisionClient) Preflight(state json.RawMessage, questions map[string]DecisionQuestion) []FieldError {
	if c == nil {
		return []FieldError{{Field: "provider", Message: "decision client is not configured"}}
	}
	var fields []FieldError
	add := func(field, message string) {
		fields = append(fields, FieldError{Field: field, Message: message})
	}
	if len(questions) == 0 {
		add("questions", "at least one question is required")
	}
	if len(questions) > c.descriptor.MaxQuestions {
		add("questions", fmt.Sprintf("at most %d questions are accepted by %s", c.descriptor.MaxQuestions, c.descriptor.Adapter))
	}
	allowed := make(map[string]bool, len(c.descriptor.QuestionTypes))
	for _, questionType := range c.descriptor.QuestionTypes {
		allowed[questionType] = true
	}
	for id, question := range questions {
		path := "questions." + id
		if strings.TrimSpace(id) == "" {
			add("questions", "question ids must be non-empty")
		}
		questionType := strings.ToLower(strings.TrimSpace(question.Type))
		if !allowed[questionType] {
			add(path+".type", fmt.Sprintf("question type %q is not supported by %s (supported: %s)",
				question.Type, c.descriptor.Adapter, strings.Join(c.descriptor.QuestionTypes, ", ")))
			continue
		}
		switch questionType {
		case QuestionNoul:
			validateInstructions(add, path, question.Instructions)
			validateNoulCriteria(add, path, question.Criteria)
		case QuestionChoice:
			validateInstructions(add, path, question.Instructions)
			validateChoiceCriteria(add, path, question.Criteria, c.descriptor.MaxChoices)
		case QuestionScore:
			validateInstructions(add, path, question.Instructions)
			validateScoreCriteria(add, path, question.Criteria, c.descriptor.MaxLevels)
		case QuestionBox:
			// Box is experimental and TypeSafe-only. It needs an image in state;
			// the check below records that requirement after the scan.
			if len(strings.TrimSpace(string(question.Criteria))) > 0 && string(question.Criteria) != "null" {
				add(path+".criteria", "box questions must not declare criteria")
			}
		}
	}

	hasImage, remoteURL, mediaBytes, err := scanState(state)
	if err != nil {
		add("state", err.Error())
	} else {
		if remoteURL != "" {
			add("state", "remote URLs are not accepted anywhere in decision state; send text or a data URL/base64 asset")
		}
		if hasImage && !containsString(c.descriptor.InputModalities, "image") {
			add("state", fmt.Sprintf("adapter %s does not accept image input", c.descriptor.Adapter))
		}
		if hasImage && mediaBytes > c.descriptor.MaxMediaBytes*4/3 {
			add("state", fmt.Sprintf("image media exceeds the %d-byte limit", c.descriptor.MaxMediaBytes))
		}
		for id, question := range questions {
			if strings.EqualFold(strings.TrimSpace(question.Type), QuestionBox) && !hasImage {
				add("questions."+id, "box questions require an image in state")
			}
		}
	}

	if int64(len(state)) > c.descriptor.MaxBodyBytes {
		add("state", fmt.Sprintf("state exceeds the %d-byte body limit", c.descriptor.MaxBodyBytes))
	} else if body, err := json.Marshal(map[string]any{
		"state":     state,
		"model":     c.descriptor.Model,
		"questions": questions,
	}); err == nil && int64(len(body)) > c.descriptor.MaxBodyBytes {
		add("state", fmt.Sprintf("request exceeds the %d-byte body limit", c.descriptor.MaxBodyBytes))
	}

	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Field == fields[j].Field {
			return fields[i].Message < fields[j].Message
		}
		return fields[i].Field < fields[j].Field
	})
	return fields
}

func validateInstructions(add func(string, string), path string, raw json.RawMessage) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		add(path+".instructions", "instructions are required")
		return
	}
	if !json.Valid(raw) {
		add(path+".instructions", "instructions must be valid JSON")
		return
	}
	switch trimmed[0] {
	case '"', '{', '[':
	default:
		add(path+".instructions", "instructions must be a string, object or array")
	}
}

func validateNoulCriteria(add func(string, string), path string, raw json.RawMessage) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return
	}
	var criteria map[string]json.RawMessage
	if err := json.Unmarshal(raw, &criteria); err != nil {
		add(path+".criteria", "noul criteria must be an object with true/false descriptions")
		return
	}
	for key, value := range criteria {
		if key != "true" && key != "false" {
			add(path+".criteria", fmt.Sprintf("noul criteria contains unsupported key %q", key))
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			add(path+".criteria."+key, "description must be a string")
		}
	}
}

func validateChoiceCriteria(add func(string, string), path string, raw json.RawMessage, maxChoices int) {
	var criteria map[string]json.RawMessage
	if err := json.Unmarshal(raw, &criteria); err != nil || len(criteria) == 0 {
		add(path+".criteria", "choice criteria must be a non-empty object of option -> description")
		return
	}
	if len(criteria) > maxChoices {
		add(path+".criteria", fmt.Sprintf("at most %d choices are accepted", maxChoices))
	}
	for name, value := range criteria {
		if strings.TrimSpace(name) == "" {
			add(path+".criteria", "choice names must be non-empty")
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil && string(value) != "null" {
			add(path+".criteria."+name, "description must be a string or null")
		}
	}
}

func validateScoreCriteria(add func(string, string), path string, raw json.RawMessage, maxLevels int) {
	var levels []json.RawMessage
	if err := json.Unmarshal(raw, &levels); err != nil {
		add(path+".criteria", "score criteria must be an ordered array of level descriptions")
		return
	}
	if len(levels) < 2 {
		add(path+".criteria", "score criteria needs at least two levels")
	}
	if len(levels) > maxLevels {
		add(path+".criteria", fmt.Sprintf("at most %d levels are accepted", maxLevels))
	}
	for index, value := range levels {
		var text string
		if json.Unmarshal(value, &text) != nil {
			add(fmt.Sprintf("%s.criteria[%d]", path, index), "level description must be a string")
		}
	}
}

// scanState validates the top-level state kind and finds image media without
// loading or decoding it. It returns the base64 length, not the byte length.
// It also reports the first http(s) URL anywhere in the tree: the decision
// protocol accepts text and inline data only, never a remote fetch target.
func scanState(raw json.RawMessage) (bool, string, int64, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false, "", 0, fmt.Errorf("state is required")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, "", 0, fmt.Errorf("state must be valid JSON")
	}
	switch value.(type) {
	case string, map[string]any, []any:
	default:
		return false, "", 0, fmt.Errorf("state must be a string, object or array")
	}
	var hasImage bool
	var remoteURL string
	var mediaBytes int64
	if text, ok := value.(string); ok && strings.HasPrefix(text, "data:image/") {
		hasImage = true
		mediaBytes = int64(len(text))
	}
	var walk func(any, bool)
	walk = func(node any, imageContext bool) {
		switch typed := node.(type) {
		case string:
			// The decision protocol never dereferences anything: state is text
			// or inline media. A remote URL anywhere in the tree would make the
			// provider (or a later proxy) fetch it, so the whole tree is scanned,
			// not only image-keyed values.
			if strings.Contains(typed, "http://") || strings.Contains(typed, "https://") {
				if remoteURL == "" {
					remoteURL = typed
				}
			}
			if !imageContext {
				return
			}
			if strings.HasPrefix(typed, "data:image/") {
				hasImage = true
				mediaBytes += int64(len(typed))
			}
		case []any:
			for _, item := range typed {
				walk(item, imageContext)
			}
		case map[string]any:
			for key, item := range typed {
				childContext := imageContext || isImageKey(key)
				if key == "assets" {
					if assets, ok := item.(map[string]any); ok && len(assets) > 0 {
						hasImage = true
						for _, asset := range assets {
							if text, ok := asset.(string); ok {
								mediaBytes += int64(len(text))
							}
						}
					}
				}
				walk(item, childContext)
			}
		}
	}
	walk(value, false)
	return hasImage, remoteURL, mediaBytes, nil
}

func isImageKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "image", "images", "image_url", "imageurl", "image_data", "imagedata":
		return true
	default:
		return false
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// CheckPolicy validates a binding policy. Fail-open is legal only in shadow
// mode; anything else is a request error.
func (c *DecisionClient) CheckPolicy(policy DecisionPolicy) []FieldError {
	_, fields := normalizePolicy(policy)
	return fields
}

func normalizePolicy(policy DecisionPolicy) (DecisionPolicy, []FieldError) {
	var fields []FieldError
	mode := strings.ToLower(strings.TrimSpace(policy.Mode))
	if mode == "" {
		mode = DecisionModeEnforce
	}
	if mode != DecisionModeShadow && mode != DecisionModeEnforce {
		fields = append(fields, FieldError{Field: "mode", Message: "mode must be shadow or enforce"})
	}
	failPolicy := strings.ToLower(strings.TrimSpace(policy.FailPolicy))
	if failPolicy == "" {
		failPolicy = DecisionFailClosed
	}
	if failPolicy != DecisionFailClosed && failPolicy != DecisionFailOpen {
		fields = append(fields, FieldError{Field: "failPolicy", Message: "failPolicy must be fail_closed or fail_open"})
	}
	if failPolicy == DecisionFailOpen && mode != DecisionModeShadow {
		fields = append(fields, FieldError{Field: "failPolicy", Message: "fail_open is only allowed in shadow mode"})
	}
	if policy.Threshold != nil && (*policy.Threshold < 0 || *policy.Threshold > 1) {
		fields = append(fields, FieldError{Field: "threshold", Message: "threshold must be between 0 and 1"})
	}
	policy.Mode = mode
	policy.FailPolicy = failPolicy
	return policy, fields
}

// Decide runs one decision call.
//
// Preflight and policy errors are always returned: a malformed call is a caller
// bug, not an observed provider failure. Provider failures follow the policy:
// enforce + fail-closed returns ErrDecisionUnavailable and no answers; shadow
// returns an outcome carrying the failure and no answers. In no case does the
// adapter synthesize noul:0, choice:"unknown" or confidence:0.
func (c *DecisionClient) Decide(ctx context.Context, req DecisionRequest) (*DecisionOutcome, error) {
	if c == nil {
		return nil, configError(FamilyDecision, "decide", "decision client is not configured")
	}
	policy, policyFields := normalizePolicy(req.Policy)
	if len(policyFields) > 0 {
		return nil, requestError(FamilyDecision, "decide", "invalid decision policy", policyFields)
	}
	if fields := c.Preflight(req.State, req.Questions); len(fields) > 0 {
		return nil, requestError(FamilyDecision, "preflight", "decision binding failed preflight", fields)
	}
	model := firstNonEmpty(req.Model, c.descriptor.Model)
	body := systemOneRequest{
		State:     req.State,
		Model:     model,
		Questions: req.Questions,
		SessionID: req.SessionID,
		User:      req.User,
	}
	raw, err := guard(FamilyDecision, "decide", func() ([]byte, error) {
		return c.send(ctx, body, req.Questions)
	})
	if err != nil {
		return c.failure(policy, len(req.Questions), err)
	}
	parsed, err := parseSystemOneResponse(raw)
	if err != nil {
		return c.failure(policy, len(req.Questions), err)
	}
	if fields := validateAnswers(req.Questions, parsed.Answers); len(fields) > 0 {
		return c.failure(policy, len(req.Questions), requestError(FamilyDecision, "answer",
			"provider returned answers that violate the System One contract", fields))
	}
	outcome := &DecisionOutcome{
		Answers:   parsed.Answers,
		Model:     firstNonEmpty(parsed.Model, model),
		Usage:     decisionUsage(parsed, len(req.Questions)),
		Mode:      policy.Mode,
		Shadow:    policy.Mode == DecisionModeShadow,
		RequestID: parsed.ID,
	}
	return outcome, nil
}

func (c *DecisionClient) failure(policy DecisionPolicy, questions int, err error) (*DecisionOutcome, error) {
	typed := providerError(FamilyDecision, "decide", err)
	if policy.Mode == DecisionModeShadow {
		return &DecisionOutcome{
			Mode:      DecisionModeShadow,
			Shadow:    true,
			Failure:   typed.Error(),
			Retryable: typed.Retryable,
			Usage:     Usage{Questions: questions},
		}, nil
	}
	message := "decision unavailable (fail-closed): " + typed.Error()
	if summary := fieldSummary(typed.Fields); summary != "" {
		message += " [" + summary + "]"
	}
	return nil, &Error{
		Family:    FamilyDecision,
		Op:        "decide",
		Kind:      KindProviderUnavailable,
		Status:    typed.Status,
		Message:   message,
		Retryable: typed.Retryable,
		Fields:    typed.Fields,
		Err:       err,
	}
}

// validateAnswers checks that every question has an answer of the matching
// type with its primary field present. Optional fields (probabilities,
// confidence, legend) are preserved, never required and never invented.
func validateAnswers(questions map[string]DecisionQuestion, answers map[string]json.RawMessage) []FieldError {
	var fields []FieldError
	add := func(field, message string) {
		fields = append(fields, FieldError{Field: field, Message: message})
	}
	for id, question := range questions {
		raw, ok := answers[id]
		if !ok {
			add("answers."+id, "provider returned no answer for this question")
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			add("answers."+id, "answer is not a JSON object")
			continue
		}
		expected := strings.ToLower(strings.TrimSpace(question.Type))
		if strings.ToLower(strings.TrimSpace(envelope.Type)) != expected {
			add("answers."+id+".type", fmt.Sprintf("answer type %q does not match question type %q", envelope.Type, expected))
			continue
		}
		switch expected {
		case QuestionNoul:
			var answer struct {
				Noul       *float64        `json:"noul"`
				Confidence *float64        `json:"confidence"`
				Rationale  json.RawMessage `json:"rationale"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil || answer.Noul == nil {
				add("answers."+id, "noul answer must carry a noul probability")
			} else if *answer.Noul < 0 || *answer.Noul > 1 {
				add("answers."+id+".noul", "probability must be between 0 and 1")
			}
			if answer.Confidence != nil {
				add("answers."+id+".confidence", "noul answers must not carry confidence")
			}
			if len(answer.Rationale) > 0 && string(answer.Rationale) != "null" {
				add("answers."+id+".rationale", "native decision answers must not carry rationale")
			}
		case QuestionChoice:
			var answer struct {
				Choice string `json:"choice"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil || strings.TrimSpace(answer.Choice) == "" {
				add("answers."+id, "choice answer must carry the chosen option")
			} else if criteria, err := choiceOptionNames(question.Criteria); err == nil && len(criteria) > 0 {
				if _, declared := criteria[answer.Choice]; !declared {
					add("answers."+id+".choice", fmt.Sprintf("choice %q is not one of the declared options", answer.Choice))
				}
			}
		case QuestionScore:
			var answer struct {
				Score *float64 `json:"score"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil || answer.Score == nil {
				add("answers."+id, "score answer must carry a score")
			} else if levels, err := scoreLevels(question.Criteria); err == nil && len(levels) > 0 {
				if *answer.Score < 0 || *answer.Score > float64(len(levels)-1) {
					add("answers."+id+".score", fmt.Sprintf("score %g is outside the declared 0..%d range", *answer.Score, len(levels)-1))
				}
			}
		case QuestionBox:
			var answer struct {
				Box *struct {
					Top    float64 `json:"top"`
					Bottom float64 `json:"bottom"`
					Left   float64 `json:"left"`
					Right  float64 `json:"right"`
				} `json:"box"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil || answer.Box == nil {
				add("answers."+id, "box answer must carry a box")
			} else {
				box := answer.Box
				for name, edge := range map[string]float64{
					"top": box.Top, "bottom": box.Bottom, "left": box.Left, "right": box.Right,
				} {
					if edge < 0 || edge > 1 {
						add("answers."+id+".box."+name, "box edge must be a fraction between 0 and 1")
					}
				}
				if box.Top > box.Bottom || box.Left > box.Right {
					add("answers."+id+".box", "box edges are inverted")
				}
			}
		}
	}
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Field < fields[j].Field })
	return fields
}

// choiceOptionNames returns the declared choice keys. A null/absent criteria
// yields an empty map, which means "nothing to cross-check".
func choiceOptionNames(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var criteria map[string]json.RawMessage
	if err := json.Unmarshal(raw, &criteria); err != nil {
		return nil, err
	}
	return criteria, nil
}

// scoreLevels returns the declared ordered levels.
func scoreLevels(raw json.RawMessage) ([]json.RawMessage, error) {
	var levels []json.RawMessage
	if err := json.Unmarshal(raw, &levels); err != nil {
		return nil, err
	}
	return levels, nil
}
