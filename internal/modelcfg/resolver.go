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

// Models returns the flat list of models a session could use, in stable
// catalog order: every complete, enabled model of a complete provider. It is
// wider than the picker: Selectable is the narrower fact (the model's family
// also has a session surface a client can open), and a non-selectable entry
// stays in this list so the settings view can show it with its issues. Callers
// that offer a choice must filter on Selectable.
func (r *Resolution) Models() []ModelView {
	out := make([]ModelView, 0)
	if r == nil {
		return out
	}
	for i := range r.Providers {
		provider := r.Providers[i]
		if provider.Visibility == VisibilityHidden || !provider.Executable {
			continue
		}
		for j := range provider.Models {
			model := provider.Models[j]
			if !model.Executable || !model.Enabled {
				continue
			}
			out = append(out, cloneModelView(model))
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

// Settings returns every resolved provider, including incomplete and disabled
// entries. Hidden providers are not resolved at all, so they are never here.
func (r *Resolution) Settings() []ProviderView {
	if r == nil {
		return []ProviderView{}
	}
	return cloneProviderViews(r.Providers)
}

// buildResolution resolves every declared provider except the hidden ones.
//
// A hidden provider is not part of the resolved configuration: it is not
// executable, not selectable and not routable, and its identity never appears
// in a diagnostic. Dropping it here — rather than flagging it in a later
// projection — is the only place that can keep every consumer honest at once:
// the settings view, the executable registry and the ModelFactory (whose
// ambiguity scan walks the resolved providers) never see the hidden entry, so a
// hidden id can neither execute nor leak through an ambiguity list.
func buildResolution(doc document) Resolution {
	resolution := Resolution{Providers: make([]ProviderView, 0, len(doc.providers))}
	for _, provider := range doc.providers {
		view := resolveProvider(provider, doc)
		if view.Visibility == VisibilityHidden {
			continue
		}
		resolution.Providers = append(resolution.Providers, view)
	}
	return resolution
}

func resolveProvider(provider ProviderConfig, doc document) ProviderView {
	visibility := provider.Visibility
	if visibility == "" {
		visibility = VisibilityVisible
	}
	providerType, surfaceKind := providerSurface(provider)
	modelIDs := resolveModelIDs(provider, doc.providerModelRules)
	view := ProviderView{
		ProviderID:   provider.ID,
		Name:         provider.Name,
		ProviderType: providerType,
		SurfaceKind:  surfaceKind,
		Visibility:   visibility,
		API:          cloneAPI(provider.API),
		Access:       provider.Access,
		Models:       make([]ModelView, 0, len(modelIDs)),
	}
	issues, complete := providerIssues(provider, len(modelIDs))
	view.Issues = issues
	view.Executable = complete

	anySelectable := false
	for _, modelID := range modelIDs {
		model := resolveModel(provider, modelID, doc)
		// Selectable means "can be chosen as a session model in the picker", so
		// it additionally requires a family a client has a session surface for.
		// A decision model is executable (the gateway answers it) but not
		// selectable: choosing it would open the generic "no dedicated panel"
		// placeholder, a dead end the user cannot leave except by picking
		// again. See sessionSelectableFamily's doc comment.
		model.Selectable = model.Executable && model.Enabled && complete &&
			visibility != VisibilityHidden && sessionSelectableFamily(model.ProviderType)
		if model.Selectable {
			anySelectable = true
		}
		view.Models = append(view.Models, model)
	}
	view.Selectable = complete && visibility != VisibilityHidden && anySelectable
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

// providerSurface resolves the provider's family and the session surface it
// opens. The type defaults to llm so a document that predates provider types
// reads exactly as it did before; an unrecognized family is the generic surface
// (never a silent llm promotion).
func providerSurface(provider ProviderConfig) (providerType, surfaceKind string) {
	providerType = normalizeProviderType(provider.Type)
	return providerType, SurfaceKindForType(providerType)
}

func resolveModel(provider ProviderConfig, modelID string, doc document) ModelView {
	config, exact := evaluateModelConfig(provider, modelID, doc)
	providerType, surfaceKind := providerSurface(provider)
	decoded, err := decodeModelConfig(config)
	if err != nil {
		// Validated inputs make this unreachable, but reporting an issue is
		// preferable to dropping the model or panicking.
		return ModelView{
			ProviderID:   provider.ID,
			ModelID:      modelID,
			ProviderType: providerType,
			SurfaceKind:  surfaceKind,
			API:          cloneAPI(provider.API),
			Enabled:      true,
			Issues: []Issue{{
				Field:   "options",
				Message: "invalid resolved model configuration: " + err.Error(),
			}},
		}
	}

	// A per-model family overrides the provider's family: one provider may serve
	// several families (an aggregator gateway, the multi-family demo provider),
	// and the model's own declaration is the finer fact. An absent override
	// inherits the provider family, so a document that never declared one
	// resolves exactly as before.
	if decoded.ProviderType != "" {
		providerType = normalizeProviderType(decoded.ProviderType)
		surfaceKind = SurfaceKindForType(providerType)
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
	issues, complete := modelIssues(decoded, providerType, enabled, exact)
	// A capability-endpoint family is executable but not a session model; the
	// issue explains the missing Selectable flag next to the model in Settings.
	// It is not a completeness defect, so it never clears Executable.
	if !sessionSelectableFamily(providerType) {
		issues = append(issues, Issue{
			Field:   "providerType",
			Message: "decision models are capability endpoints called through /v1/decisions/ask (or /v1/media/decision), not session models: the picker does not offer them as a conversation",
		})
	}
	return ModelView{
		ProviderID:   provider.ID,
		ModelID:      modelID,
		ProviderType: providerType,
		SurfaceKind:  surfaceKind,
		API:          cloneAPI(provider.API),
		Properties:   properties,
		Options:      cloneOptions(options),
		Enabled:      enabled,
		Executable:   complete,
		Baseline:     !exact,
		Issues:       issues,
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

// requiresTextOutput reports whether a model of this family must declare text
// output to be executable. Text and realtime families answer a conversation, so
// a text-less model of those families cannot serve a turn. The medium families
// (image/video/tts/stt) and a decision family legitimately produce something
// else; requiring text there would make the whole family unusable.
func requiresTextOutput(providerType string) bool {
	switch normalizeProviderType(providerType) {
	case ProviderTypeLLM, ProviderTypeRealtime:
		return true
	default:
		return false
	}
}

// sessionSelectableFamily reports whether a provider family can be chosen as a
// session model from the picker. A decision model is a capability endpoint:
// clients call POST /v1/decisions/ask (or POST /v1/media/decision) with a
// structured request, and the client surface registry has no decision panel, so
// selecting one in the picker would land on the generic "no dedicated panel"
// placeholder — a dead end the user can only leave by picking again. Such a
// model stays Executable (the gateway does answer it) and stays in the settings
// view with an issue (the capability exists and must be readable there); only
// Selectable is false.
//
// The check reads the model's *effective* family (a per-model providerType
// override wins over the provider's), never the provider-level family: the demo
// provider is generic at provider level and declares decision on one model.
func sessionSelectableFamily(providerType string) bool {
	return normalizeProviderType(providerType) != ProviderTypeDecision
}

// modelIssues checks the completeness contract and records field paths.
// providerType is the model's effective family (it decides whether text output
// is required). enabled is separate from completeness: a disabled model can be
// complete and still never be selectable.
//
// The text-output requirement is gated on the family: a text model that
// produces no text cannot answer a turn, but an image, video, tts or realtime
// model legitimately produces another medium (a realtime model declares text
// and audio; an image model declares image only). Requiring text everywhere
// would make every non-text model unexecutable, which is exactly what the
// capability families exist to express.
func modelIssues(config ModelConfig, providerType string, enabled, exact bool) ([]Issue, bool) {
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
		if !properties.OutputFormat.Text && requiresTextOutput(providerType) {
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
				if !validReasoningLevel(value) {
					issues = append(issues, Issue{
						Field: "options.reasoningLevel.values",
						Message: fmt.Sprintf("reasoning level %q is not a runtime level (want %s)",
							value, strings.Join(reasoningLevels, "|")),
					})
					complete = false
				}
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
	// The demo protocol runs in-process: it has no endpoint to reach, so a base
	// URL is neither required nor meaningful. Every other protocol needs one, and
	// requiring it for demo too would make the provider incomplete by
	// construction (a base URL nobody dials is a lie about the transport).
	isDemo := strings.EqualFold(strings.TrimSpace(provider.API.Type), APITypeDemo)
	if !isDemo && strings.TrimSpace(provider.API.BaseURL) == "" {
		issues = append(issues, Issue{Field: "api.baseUrl", Message: "base URL is required"})
		complete = false
	}
	freeAccess := provider.Access.CredentialFree()
	switch {
	case provider.Access.Type == "":
		issues = append(issues, Issue{
			Field:   "access.type",
			Message: "access type is required (want " + AccessTypeAPIKey + "|" + AccessTypeNone + ")",
		})
		complete = false
	case freeAccess:
		// No credential to resolve; the credential block is complete as declared.
	case provider.Access.Type != AccessTypeAPIKey:
		issues = append(issues, Issue{
			Field:   "access.type",
			Message: fmt.Sprintf("unsupported access type %q (want %s|%s)", provider.Access.Type, AccessTypeAPIKey, AccessTypeNone),
		})
		complete = false
	}
	if len(provider.API.Headers) > 0 {
		issues = append(issues, Issue{
			Field:   "api.headers",
			Message: "static headers are not carried by any runtime driver yet; a provider with headers cannot execute",
		})
		complete = false
	}
	if !freeAccess && strings.TrimSpace(provider.Access.Env) == "" && strings.TrimSpace(provider.Access.Ref) == "" {
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
