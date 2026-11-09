package modelcfg

// This file defines the data contract of the provider/model configuration
// layer. Provider Config describes how to reach an endpoint; Model Config
// describes what a model can do. Identity is always the pair (providerId,
// modelId): a bare model id is never routed by name guessing.
//
// All types here are pure data. Parsing, validation and resolution live in the
// other files of this package; nothing here performs IO.

// API protocol identifiers. These are the only wire protocols v1 understands.
const (
	APITypeOpenAIChatCompletions = "openai-chat-completions"
	APITypeOpenAIResponses       = "openai-responses"
	APITypeAnthropicMessages     = "anthropic-messages"
)

// Access type identifiers. v1 supports a single access mechanism: an API key
// referenced by environment variable name or by a tenant secret reference.
const (
	AccessTypeAPIKey = "api-key"
)

// Visibility values. An empty visibility defaults to visible. A hidden
// provider is not part of the resolved configuration at all: it is not
// executable, not selectable, and its identity never appears in a diagnostic
// (see buildResolution).
const (
	VisibilityVisible = "visible"
	VisibilityHidden  = "hidden"
)

// reasoningLevels is the runtime's unified thinking-level vocabulary, in
// ascending order. It is the single set a model's reasoningLevel.values may
// draw from: the execution layer maps these strings onto driver dialects, so a
// value outside the set cannot be interpreted and the model is not executable.
// TestReasoningLevelsMatchAgentcore pins the set against agentcore's level
// constants, so the two can never drift apart.
var reasoningLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// validReasoningLevel reports whether value is in the unified vocabulary.
func validReasoningLevel(value string) bool {
	for _, level := range reasoningLevels {
		if value == level {
			return true
		}
	}
	return false
}

// API describes the wire protocol, endpoint and static headers of a provider.
// Headers are static, non-secret values (for example an API-version header);
// credentials belong to Access.
type API struct {
	Type    string            `json:"type"`
	BaseURL string            `json:"baseUrl"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Access references a credential. Env is an environment variable *name*; Ref
// is a tenant secret reference (the legacy apiKeyRef). Neither field may ever
// contain a credential value.
type Access struct {
	Type string `json:"type"`          // "api-key"
	Env  string `json:"env,omitempty"` // env var name only; never a secret value
	Ref  string `json:"ref,omitempty"` // tenant secret reference (apiKeyRef)
}

// MediaIn describes which input modalities a model accepts.
type MediaIn struct {
	Text  bool `json:"text"`
	Image bool `json:"image"`
	Video bool `json:"video"`
	Audio bool `json:"audio"`
	PDF   bool `json:"pdf"`
}

// MediaOut describes which output modalities a model produces.
type MediaOut struct {
	Text bool `json:"text"`
}

// Properties is the complete capability fact sheet of a model. Execution
// branches (chat, compaction, subagents, idle work) must read these values
// instead of hard-coding capability by model name.
type Properties struct {
	ContextWindow            int      `json:"contextWindow"`
	InputFormat              MediaIn  `json:"inputFormat"`
	OutputFormat             MediaOut `json:"outputFormat"`
	SupportsToolCall         bool     `json:"supportsToolCall"`
	SupportsJSONSchemaOutput bool     `json:"supportsJsonSchemaOutput"`
	SupportsNativeWebSearch  bool     `json:"supportsNativeWebSearch"`
}

// OptionSpecs is the complete option vocabulary of a model. Values is ordered
// and non-empty for executable models; Map is a closed set of request-body
// patch templates (no expressions in v1).
type OptionSpecs struct {
	ReasoningLevel  EnumSpec  `json:"reasoningLevel"`
	MaxOutputTokens LimitSpec `json:"maxOutputTokens"`
}

// EnumSpec maps each allowed value to a JSON Merge Patch template. Every value
// in Values must have an entry in Map for a model to be executable.
type EnumSpec struct {
	Values []string                  `json:"values"`
	Map    map[string]map[string]any `json:"map"`
}

// LimitSpec describes the maximum output token budget and an optional patch
// template. The default template is stored under the empty key (""); "*" is
// accepted as an equivalent wildcard key. Both are applied in that order when
// present.
type LimitSpec struct {
	Max int                       `json:"max"`
	Map map[string]map[string]any `json:"map"`
}

// ProviderConfig is one provider entry. ModelIDs lists the provider's member
// models in display order. Visibility is "visible" (default) or "hidden".
type ProviderConfig struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	API        API      `json:"api"`
	Access     Access   `json:"access"`
	ModelIDs   []string `json:"modelIds,omitempty"`
	Visibility string   `json:"visibility,omitempty"` // "visible"|"hidden"
}

// ModelConfig is a sparse model configuration. It is the shape carried by
// every rule and by a tenant's per-model overrides: absent fields inherit,
// explicit null clears, and concrete values replace.
type ModelConfig struct {
	Enabled    *bool        `json:"enabled,omitempty"`
	Properties *Properties  `json:"properties,omitempty"`
	Options    *OptionSpecs `json:"options,omitempty"`
}

// Selection names a model identity. It deliberately carries no endpoint,
// capability or credential data: those are resolved from configuration.
type Selection struct {
	ProviderID     string `json:"providerId,omitempty"`
	ModelID        string `json:"modelId"`
	ReasoningLevel string `json:"reasoningLevel,omitempty"`
}

// Issue is one configuration problem tied to a dotted field path.
type Issue struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ModelView is the resolved view of one model. It is present in Settings even
// when incomplete or disabled; Enabled reports the configured enabled flag
// (default true), Executable reports completeness, and Selectable reports
// completeness + enabled + a usable provider. Baseline reports that no rule
// named this model: its facts were patched in by wildcard/baseline rules, so
// they are a heuristic about the model rather than a declaration about it
// (servicehost therefore does not hand a baseline-derived context window to the
// compaction path).
type ModelView struct {
	ProviderID string      `json:"providerId"`
	ModelID    string      `json:"modelId"`
	API        API         `json:"api"`
	Properties Properties  `json:"properties"`
	Options    OptionSpecs `json:"options"`
	Enabled    bool        `json:"enabled"`
	Executable bool        `json:"executable"`
	Selectable bool        `json:"selectable"`
	Baseline   bool        `json:"baseline"`
	Issues     []Issue     `json:"issues,omitempty"`
}

// ProviderView is the resolved view of one provider. Models contains every
// resolved member model (including incomplete/disabled ones). Executable
// reports that the provider itself is complete enough to be used; Selectable
// reports that it is visible and has at least one selectable model. Hidden
// providers are not resolved at all, so they appear in no view.
type ProviderView struct {
	ProviderID string      `json:"providerId"`
	Name       string      `json:"name,omitempty"`
	Visibility string      `json:"visibility"`
	API        API         `json:"api"`
	Access     Access      `json:"access"`
	Models     []ModelView `json:"models"`
	Issues     []Issue     `json:"issues,omitempty"`
	Executable bool        `json:"executable"`
	Selectable bool        `json:"selectable"`
}

// Resolution is one complete parse of catalog + tenant overlay. Settings and
// the executable registry are projections of the same value, so they cannot
// drift apart. Hidden providers are dropped while the resolution is built, so
// neither projection (nor a Factory built from the resolution) can reach them.
type Resolution struct {
	Providers []ProviderView `json:"providers"`
}

// ActiveModel is the immutable result of a factory resolution. A run pins one
// ActiveModel at claim time; later configuration updates only affect runs that
// start afterwards.
type ActiveModel struct {
	ProviderID      string         `json:"providerId"`
	ModelID         string         `json:"modelId"`
	API             API            `json:"api"`
	Access          Access         `json:"access"`
	Properties      Properties     `json:"properties"`
	Options         OptionSpecs    `json:"options"`
	ReasoningLevel  string         `json:"reasoningLevel"`
	MaxOutputTokens int            `json:"maxOutputTokens"`
	RequestPatch    map[string]any `json:"requestPatch"`
}

// LegacyAgent is the flat host.AgentConfig subset that predates provider
// configuration. It is normalized into one anonymous provider plus a
// selection when the tenant document carries no providers or rules.
type LegacyAgent struct {
	Provider  string
	Model     string
	BaseURL   string
	Protocol  string
	APIKeyRef string
}
