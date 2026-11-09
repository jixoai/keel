package modelcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Resolver holds one complete parse of a catalog plus a tenant overlay.
// Resolve returns a snapshot; configuration changes produce a new Resolver
// instead of mutating this one.
type Resolver struct {
	resolution Resolution
}

// NewResolver merges the tenant overlay into the catalog (sparse per-provider
// patches, appended rule sets; see mergeTenant) and resolves the result into
// provider/model views. The catalog is never mutated.
func NewResolver(cat *Catalog, overlay Overlay) (*Resolver, error) {
	if cat == nil {
		return nil, errors.New("modelcfg: nil catalog")
	}
	doc := mergeTenant(cat.doc, overlay.patch)
	decoded, err := decodeDocument(doc)
	if err != nil {
		return nil, fmt.Errorf("modelcfg: resolve model configuration: %w", err)
	}
	return &Resolver{resolution: buildResolution(decoded)}, nil
}

// Resolve returns the settings view and the executable registry projection.
func (r *Resolver) Resolve() Resolution {
	if r == nil {
		return Resolution{Providers: []ProviderView{}}
	}
	return cloneResolution(r.resolution)
}

// Models returns the flat, selectable model list in stable catalog order.
func (r *Resolution) Models() []ModelView {
	out := make([]ModelView, 0)
	if r == nil {
		return out
	}
	for i := range r.Providers {
		for j := range r.Providers[i].Models {
			model := r.Providers[i].Models[j]
			if model.Selectable {
				out = append(out, cloneModelView(model))
			}
		}
	}
	return out
}

// Find returns one resolved model by exact identity, including models that are
// not selectable. Callers that need executability must check the returned
// flags (or use Factory, which fails closed with the view's issues).
func (r *Resolution) Find(providerID, modelID string) (ModelView, bool) {
	if r == nil {
		return ModelView{}, false
	}
	for i := range r.Providers {
		if r.Providers[i].ProviderID != providerID {
			continue
		}
		for j := range r.Providers[i].Models {
			if r.Providers[i].Models[j].ModelID == modelID {
				return cloneModelView(r.Providers[i].Models[j]), true
			}
		}
	}
	return ModelView{}, false
}

// Settings returns every provider, including incomplete and disabled entries.
func (r *Resolution) Settings() []ProviderView {
	if r == nil {
		return []ProviderView{}
	}
	return cloneProviderViews(r.Providers)
}

func buildResolution(doc document) Resolution {
	resolution := Resolution{Providers: make([]ProviderView, 0, len(doc.providers))}
	for _, provider := range doc.providers {
		resolution.Providers = append(resolution.Providers, resolveProvider(provider, doc))
	}
	return resolution
}

func resolveProvider(provider ProviderConfig, doc document) ProviderView {
	visibility := provider.Visibility
	if visibility == "" {
		visibility = VisibilityVisible
	}
	modelIDs := resolveModelIDs(provider, doc.providerModelRules)
	view := ProviderView{
		ProviderID: provider.ID,
		Name:       provider.Name,
		Visibility: visibility,
		API:        cloneAPI(provider.API),
		Access:     provider.Access,
		Models:     make([]ModelView, 0, len(modelIDs)),
	}
	issues, complete := providerIssues(provider, len(modelIDs))
	view.Issues = issues
	view.Executable = complete

	providerUsable := complete && visibility != VisibilityHidden
	anySelectable := false
	for _, modelID := range modelIDs {
		model := resolveModel(provider, modelID, doc)
		model.Selectable = model.Executable && model.Enabled && providerUsable
		if model.Selectable {
			anySelectable = true
		}
		view.Models = append(view.Models, model)
	}
	view.Selectable = providerUsable && anySelectable
	return view
}

// resolveModelIDs returns the provider's declared models in order, followed by
// models that an exact providerModelRule introduces. Duplicates keep their
// first position; map iteration is never involved.
func resolveModelIDs(provider ProviderConfig, rules []rule) []string {
	seen := make(map[string]struct{}, len(provider.ModelIDs))
	out := make([]string, 0, len(provider.ModelIDs))
	appendUnique := func(modelID string) {
		if modelID == "" {
			return
		}
		if _, duplicate := seen[modelID]; duplicate {
			return
		}
		seen[modelID] = struct{}{}
		out = append(out, modelID)
	}
	for _, modelID := range provider.ModelIDs {
		appendUnique(modelID)
	}
	for _, rule := range rules {
		if rule.providerID == provider.ID {
			appendUnique(rule.modelID)
		}
	}
	return out
}

func resolveModel(provider ProviderConfig, modelID string, doc document) ModelView {
	config, exact := evaluateModelConfig(provider, modelID, doc)
	decoded, err := decodeModelConfig(config)
	if err != nil {
		// Validated inputs make this unreachable, but reporting an issue is
		// preferable to dropping the model or panicking.
		return ModelView{
			ProviderID: provider.ID,
			ModelID:    modelID,
			API:        cloneAPI(provider.API),
			Enabled:    true,
			Issues: []Issue{{
				Field:   "options",
				Message: "invalid resolved model configuration: " + err.Error(),
			}},
		}
	}

	enabled := true
	if decoded.Enabled != nil {
		enabled = *decoded.Enabled
	}
	properties := Properties{}
	if decoded.Properties != nil {
		properties = *decoded.Properties
	}
	options := OptionSpecs{}
	if decoded.Options != nil {
		options = *decoded.Options
	}
	issues, complete := modelIssues(decoded, enabled, exact)
	return ModelView{
		ProviderID: provider.ID,
		ModelID:    modelID,
		API:        cloneAPI(provider.API),
		Properties: properties,
		Options:    cloneOptions(options),
		Enabled:    enabled,
		Executable: complete,
		Issues:     issues,
	}
}

// evaluateModelConfig applies every matching rule in the fixed layer order.
// Within one layer the array order is the priority; later matches patch over
// earlier ones. Site rules are matched against the normalized base URL.
func evaluateModelConfig(provider ProviderConfig, modelID string, doc document) (map[string]any, bool) {
	config := map[string]any{}
	exact := false

	for _, rule := range doc.modelRules {
		if !matchModelGlob(rule.match, modelID) {
			continue
		}
		config = mergeJSONObjects(config, rule.config)
		if !hasGlobWildcard(rule.match) {
			exact = true
		}
	}
	for _, rule := range doc.modelAPIRules {
		if !matchModelGlob(rule.match, modelID) || !strings.EqualFold(rule.apiType, provider.API.Type) {
			continue
		}
		config = mergeJSONObjects(config, rule.config)
		if !hasGlobWildcard(rule.match) {
			exact = true
		}
	}
	if baseURL := normalizeBaseURL(provider.API.BaseURL); baseURL != "" {
		for _, rule := range doc.siteRules {
			if !matchModelGlob(rule.match, modelID) {
				continue
			}
			if normalizeBaseURL(rule.baseURL) != baseURL {
				continue
			}
			config = mergeJSONObjects(config, rule.config)
		}
	}
	for _, rule := range doc.providerModelRules {
		if rule.providerID != provider.ID || rule.modelID != modelID {
			continue
		}
		config = mergeJSONObjects(config, rule.config)
		exact = true
	}
	return config, exact
}

func decodeModelConfig(config map[string]any) (ModelConfig, error) {
	raw, err := json.Marshal(config)
	if err != nil {
		return ModelConfig{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var decoded ModelConfig
	if err := dec.Decode(&decoded); err != nil {
		return ModelConfig{}, err
	}
	normalizeModelConfig(&decoded)
	return decoded, nil
}

func normalizeModelConfig(config *ModelConfig) {
	if config == nil || config.Options == nil {
		return
	}
	for level, patch := range config.Options.ReasoningLevel.Map {
		if normalized, ok := normalizeJSONNumbers(patch).(map[string]any); ok {
			config.Options.ReasoningLevel.Map[level] = normalized
		}
	}
	for key, patch := range config.Options.MaxOutputTokens.Map {
		if normalized, ok := normalizeJSONNumbers(patch).(map[string]any); ok {
			config.Options.MaxOutputTokens.Map[key] = normalized
		}
	}
}

// modelIssues checks the completeness contract and records field paths.
// enabled is separate from completeness: a disabled model can be complete and
// still never be selectable.
func modelIssues(config ModelConfig, enabled, exact bool) ([]Issue, bool) {
	issues := make([]Issue, 0, 5)
	complete := true

	if !enabled {
		issues = append(issues, Issue{Field: "enabled", Message: "model is disabled"})
	}
	if config.Properties == nil {
		issues = append(issues, Issue{Field: "properties", Message: "model properties are required"})
		complete = false
	} else {
		properties := *config.Properties
		if properties.ContextWindow <= 0 {
			issues = append(issues, Issue{
				Field:   "properties.contextWindow",
				Message: "contextWindow must be greater than zero",
			})
			complete = false
		}
		if !properties.OutputFormat.Text {
			issues = append(issues, Issue{
				Field:   "properties.outputFormat.text",
				Message: "text output must be supported",
			})
			complete = false
		}
	}
	if config.Options == nil {
		issues = append(issues, Issue{Field: "options", Message: "model options are required"})
		complete = false
	} else {
		options := *config.Options
		if len(options.ReasoningLevel.Values) == 0 {
			issues = append(issues, Issue{
				Field:   "options.reasoningLevel.values",
				Message: "reasoning level values must be non-empty",
			})
			complete = false
		} else {
			for _, value := range options.ReasoningLevel.Values {
				patch, ok := options.ReasoningLevel.Map[value]
				if !ok || patch == nil {
					issues = append(issues, Issue{
						Field:   "options.reasoningLevel.map." + value,
						Message: "reasoning level has no request patch",
					})
					complete = false
				}
			}
		}
		if options.MaxOutputTokens.Max <= 0 {
			issues = append(issues, Issue{
				Field:   "options.maxOutputTokens.max",
				Message: "max output tokens must be greater than zero",
			})
			complete = false
		}
		if !hasDefaultLimitPatch(options.MaxOutputTokens.Map) {
			issues = append(issues, Issue{
				Field:   "options.maxOutputTokens.map",
				Message: "max output tokens needs a default patch",
			})
			complete = false
		}
	}
	if !exact {
		issues = append(issues, Issue{
			Field:   "properties",
			Message: "baseline-derived capabilities; override with a model rule",
		})
	}
	return issues, complete
}

func providerIssues(provider ProviderConfig, modelCount int) ([]Issue, bool) {
	issues := make([]Issue, 0, 5)
	complete := true

	switch {
	case provider.API.Type == "":
		issues = append(issues, Issue{
			Field:   "api.type",
			Message: "API type is required (want " + APITypeOpenAIChatCompletions + "|" + APITypeOpenAIResponses + "|" + APITypeAnthropicMessages + ")",
		})
		complete = false
	case !validAPIType(provider.API.Type):
		issues = append(issues, Issue{
			Field: "api.type",
			Message: fmt.Sprintf("unsupported API type %q (want %s|%s|%s)", provider.API.Type,
				APITypeOpenAIChatCompletions, APITypeOpenAIResponses, APITypeAnthropicMessages),
		})
		complete = false
	}
	if strings.TrimSpace(provider.API.BaseURL) == "" {
		issues = append(issues, Issue{Field: "api.baseUrl", Message: "base URL is required"})
		complete = false
	}
	switch {
	case provider.Access.Type == "":
		issues = append(issues, Issue{
			Field:   "access.type",
			Message: "access type is required (want " + AccessTypeAPIKey + ")",
		})
		complete = false
	case provider.Access.Type != AccessTypeAPIKey:
		issues = append(issues, Issue{
			Field:   "access.type",
			Message: fmt.Sprintf("unsupported access type %q (want %s)", provider.Access.Type, AccessTypeAPIKey),
		})
		complete = false
	}
	if provider.Access.Env == "" && provider.Access.Ref == "" {
		issues = append(issues, Issue{
			Field:   "access",
			Message: "access environment variable name or secret reference is required",
		})
		complete = false
	}
	if modelCount == 0 {
		issues = append(issues, Issue{Field: "modelIds", Message: "provider has no models"})
		complete = false
	}
	return issues, complete
}

func hasDefaultLimitPatch(patches map[string]map[string]any) bool {
	for _, key := range []string{"", "*"} {
		if patch, ok := patches[key]; ok && patch != nil {
			return true
		}
	}
	return false
}

func cloneResolution(resolution Resolution) Resolution {
	return Resolution{Providers: cloneProviderViews(resolution.Providers)}
}

func cloneProviderViews(providers []ProviderView) []ProviderView {
	out := make([]ProviderView, len(providers))
	for i := range providers {
		out[i] = cloneProviderView(providers[i])
	}
	return out
}

func cloneProviderView(view ProviderView) ProviderView {
	out := view
	out.API = cloneAPI(view.API)
	out.Models = make([]ModelView, len(view.Models))
	for i := range view.Models {
		out.Models[i] = cloneModelView(view.Models[i])
	}
	out.Issues = cloneIssues(view.Issues)
	return out
}

func cloneModelView(view ModelView) ModelView {
	out := view
	out.API = cloneAPI(view.API)
	out.Options = cloneOptions(view.Options)
	out.Issues = cloneIssues(view.Issues)
	return out
}

func cloneAPI(api API) API {
	out := api
	out.Headers = cloneStringMap(api.Headers)
	return out
}

func cloneOptions(options OptionSpecs) OptionSpecs {
	out := options
	out.ReasoningLevel.Values = cloneStrings(options.ReasoningLevel.Values)
	out.ReasoningLevel.Map = clonePatchMaps(options.ReasoningLevel.Map)
	out.MaxOutputTokens.Map = clonePatchMaps(options.MaxOutputTokens.Map)
	return out
}

func clonePatchMaps(patches map[string]map[string]any) map[string]map[string]any {
	if patches == nil {
		return nil
	}
	out := make(map[string]map[string]any, len(patches))
	for key, patch := range patches {
		if patch == nil {
			out[key] = nil
			continue
		}
		cloned, _ := cloneJSONValue(patch).(map[string]any)
		out[key] = cloned
	}
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneIssues(in []Issue) []Issue {
	if in == nil {
		return nil
	}
	out := make([]Issue, len(in))
	copy(out, in)
	return out
}
