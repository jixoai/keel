package modelcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Overlay is one tenant layer. Its internals are deliberately opaque: callers
// construct it with OverlayFromDocument (or OverlayFromLegacy) and hand it to
// NewResolver. The zero value is a valid empty overlay.
type Overlay struct {
	patch     map[string]any
	selection Selection
}

// Selection returns the default selection declared by the overlay, if any.
// It is a convenience for wiring code that wants to seed a request selection;
// resolution itself never guesses a model.
func (o Overlay) Selection() Selection {
	return o.selection
}

// OverlayFromDocument builds the tenant layer from a tenant document. The
// document may only carry the keys this package owns:
//
//	{"providers": [...], "modelRules": [...], "modelApiRules": [...],
//	 "siteRules": [...], "providerModelRules": [...], "selection": {...}}
//
// Unknown top-level keys are ignored because the same document may carry the
// rest of the tenant configuration.
//
// When the document has no providers and no rules, the flat legacy
// host.AgentConfig fields are normalized into one provider plus a selection,
// so deployments that predate provider configuration keep working unchanged.
func OverlayFromDocument(raw []byte, legacy LegacyAgent) (Overlay, error) {
	trimmed := bytes.TrimSpace(raw)
	var doc map[string]any
	var err error
	if len(trimmed) == 0 {
		doc = map[string]any{}
	} else {
		doc, err = decodeJSONObject(trimmed)
		if err != nil {
			return Overlay{}, fmt.Errorf("tenant document: %w", err)
		}
	}
	if version, ok := doc["schemaVersion"]; ok {
		parsed, ok := asInt(version)
		if !ok || parsed != 1 {
			return Overlay{}, errors.New("tenant document: schemaVersion must be 1")
		}
	}
	if err := validateTenantDocument(doc); err != nil {
		return Overlay{}, fmt.Errorf("tenant document: %w", err)
	}
	var selection Selection
	if raw, ok := doc["selection"]; ok && raw != nil {
		parsed, err := decodeSelection(raw)
		if err != nil {
			return Overlay{}, fmt.Errorf("tenant document: %w", err)
		}
		selection = parsed
	}

	overlay := Overlay{patch: cloneJSONMap(doc), selection: selection}
	if documentDeclaresProviders(doc) {
		return overlay, nil
	}

	provider, ok := legacyProvider(legacy)
	if !ok {
		return overlay, nil
	}
	providerMap, err := providerToMap(provider)
	if err != nil {
		return Overlay{}, fmt.Errorf("tenant document: normalize legacy agent: %w", err)
	}
	overlay.patch["providers"] = []any{providerMap}

	selection = overlay.selection
	if selection.ModelID == "" {
		selection = Selection{
			ProviderID: provider.ID,
			ModelID:    strings.TrimSpace(legacy.Model),
		}
	} else if selection.ProviderID == "" {
		selection.ProviderID = provider.ID
	}
	overlay.selection = selection
	if selection.ModelID != "" {
		overlay.patch["selection"] = selectionToMap(selection)
	}
	return overlay, nil
}

// OverlayFromLegacy is OverlayFromDocument for an empty tenant document. It
// exists so hosts that still only have the flat configuration have a direct
// entry point.
func OverlayFromLegacy(legacy LegacyAgent) (Overlay, error) {
	return OverlayFromDocument(nil, legacy)
}

// documentDeclaresProviders reports whether a tenant document declares its own
// provider list. Only that suppresses the legacy normalization: when the
// document does not, the flat agent fields are normalized into one provider and
// a default selection (the documented incremental-adoption path), including for
// a rules-only document whose rules then customize the result. Rule sets alone
// never suppress the flat default selection.
func documentDeclaresProviders(doc map[string]any) bool {
	if doc == nil {
		return false
	}
	list, ok := doc["providers"].([]any)
	return ok && len(list) > 0
}

// ModelsDocumentConfigured reports whether a tenant's "models" section
// declares provider configuration. The decision is semantic, never byte-based:
// a document that carries none of the vocabulary keys (or only explicit nulls)
// declares nothing, whatever its bytes look like — `{}`, `{ }`, a
// newline-only body and `{"schemaVersion":1}` all leave a flat deployment on
// its static path.
// A document that is not a JSON object at all counts as configured, so the
// resolver reports it instead of it being silently ignored.
func ModelsDocumentConfigured(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return true
	}
	for _, key := range documentModelKeys {
		value, ok := doc[key]
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		return true
	}
	return false
}

// legacyProvider normalizes the flat agent configuration into one provider.
// Fields the legacy document does not carry fall back to the matching embedded
// catalog entry so a deployment that only set model (or provider) still gets
// the same endpoint and environment variable it had before.
func legacyProvider(legacy LegacyAgent) (ProviderConfig, bool) {
	modelID := strings.TrimSpace(legacy.Model)
	providerName := strings.ToLower(strings.TrimSpace(legacy.Provider))
	baseURL := strings.TrimSpace(legacy.BaseURL)
	if modelID == "" && providerName == "" && baseURL == "" {
		return ProviderConfig{}, false
	}

	def := builtinProviderByName(providerName)
	if def == nil && modelID != "" {
		def = builtinProviderForModel(modelID)
	}

	provider := ProviderConfig{
		ID:         providerName,
		Name:       providerName,
		Visibility: VisibilityVisible,
	}
	if provider.ID == "" {
		provider.ID = "legacy"
		provider.Name = "legacy"
	}

	apiType := protocolToAPIType(legacy.Protocol)
	if apiType == "" && def != nil {
		apiType = def.API.Type
	}
	if apiType == "" {
		apiType = inferLegacyAPIType(provider.ID, modelID)
	}
	provider.API.Type = apiType
	provider.API.BaseURL = baseURL
	if provider.API.BaseURL == "" && def != nil {
		provider.API.BaseURL = def.API.BaseURL
		provider.API.Headers = cloneStringMap(def.API.Headers)
	}

	provider.Access.Type = AccessTypeAPIKey
	if ref := strings.TrimSpace(legacy.APIKeyRef); ref != "" {
		provider.Access.Ref = ref
	} else if def != nil {
		provider.Access.Env = def.Access.Env
		provider.Access.Ref = def.Access.Ref
	} else {
		provider.Access.Env = deriveEnvName(provider.ID, modelID)
	}
	if modelID != "" {
		provider.ModelIDs = []string{modelID}
	}
	return provider, true
}

func protocolToAPIType(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "":
		return ""
	case "openai", "openai/chat", "chat", APITypeOpenAIChatCompletions:
		return APITypeOpenAIChatCompletions
	case "openai/resp_api", "responses", APITypeOpenAIResponses:
		return APITypeOpenAIResponses
	case "anthropic", APITypeAnthropicMessages:
		return APITypeAnthropicMessages
	default:
		// Keep the raw value so resolution reports an unsupported api.type
		// instead of silently guessing a protocol.
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func inferLegacyAPIType(providerID, modelID string) string {
	name := strings.ToLower(strings.TrimSpace(providerID))
	model := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case name == "anthropic" || strings.HasPrefix(model, "claude"):
		return APITypeAnthropicMessages
	case name == "openai" || strings.HasPrefix(model, "gpt"):
		return APITypeOpenAIChatCompletions
	default:
		return APITypeOpenAIChatCompletions
	}
}

func deriveEnvName(providerID, modelID string) string {
	env := func(name string) string {
		name = strings.TrimSpace(name)
		name = strings.ReplaceAll(name, "-", "_")
		name = strings.ReplaceAll(name, ".", "_")
		name = strings.ReplaceAll(name, "/", "_")
		return strings.ToUpper(name) + "_API_KEY"
	}
	if providerID != "" && providerID != "legacy" {
		return env(providerID)
	}
	model := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(model, "claude"):
		return "ANTHROPIC_API_KEY"
	case strings.HasPrefix(model, "deepseek"):
		return "DEEPSEEK_API_KEY"
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "o1"), strings.HasPrefix(model, "o3"):
		return "OPENAI_API_KEY"
	default:
		return ""
	}
}

func builtinProviders() []ProviderConfig {
	doc, err := decodeDocument(EmbeddedBuiltin().doc)
	if err != nil {
		// The embedded catalog was already validated; this is unreachable.
		panic("modelcfg: decode embedded catalog: " + err.Error())
	}
	return doc.providers
}

func builtinProviderByName(name string) *ProviderConfig {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	for _, provider := range builtinProviders() {
		if strings.ToLower(provider.ID) == name {
			copy := provider
			return &copy
		}
	}
	return nil
}

func builtinProviderForModel(modelID string) *ProviderConfig {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	var matches []ProviderConfig
	for _, provider := range builtinProviders() {
		for _, candidate := range provider.ModelIDs {
			if strings.EqualFold(candidate, modelID) {
				matches = append(matches, provider)
				break
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) == 1 {
		copy := matches[0]
		return &copy
	}
	// Prefer a provider that names the model's family (deepseek-* →
	// deepseek) over aggregators that happen to list the same id.
	lower := strings.ToLower(modelID)
	for _, provider := range matches {
		id := strings.ToLower(provider.ID)
		if strings.HasPrefix(lower, id+"-") || strings.HasPrefix(lower, id+"/") {
			copy := provider
			return &copy
		}
	}
	return nil
}

func providerToMap(provider ProviderConfig) (map[string]any, error) {
	raw, err := json.Marshal(provider)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func selectionToMap(selection Selection) map[string]any {
	out := map[string]any{"modelId": selection.ModelID}
	if selection.ProviderID != "" {
		out["providerId"] = selection.ProviderID
	}
	if selection.ReasoningLevel != "" {
		out["reasoningLevel"] = selection.ReasoningLevel
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
