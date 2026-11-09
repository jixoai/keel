package modelcfg

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotExecutable is the sentinel returned when a selection cannot produce an
// executable model. The concrete error is *NotExecutableError and carries the
// issues that prevented execution.
var ErrNotExecutable = errors.New("modelcfg: model is not executable")

// NotExecutableError explains why a selection could not be executed. Issues
// use the same field paths as the settings view.
type NotExecutableError struct {
	ProviderID string
	ModelID    string
	Issues     []Issue
}

func (e *NotExecutableError) Error() string {
	identity := e.ModelID
	if e.ProviderID != "" {
		identity = e.ProviderID + "/" + e.ModelID
	}
	if identity == "" {
		identity = "model"
	}
	if len(e.Issues) == 0 {
		return fmt.Sprintf("modelcfg: %s is not executable", identity)
	}
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue.Field == "" {
			parts = append(parts, issue.Message)
			continue
		}
		parts = append(parts, issue.Field+": "+issue.Message)
	}
	return fmt.Sprintf("modelcfg: %s is not executable: %s", identity, strings.Join(parts, "; "))
}

// Unwrap makes errors.Is(err, ErrNotExecutable) work for every failure mode.
func (e *NotExecutableError) Unwrap() error {
	return ErrNotExecutable
}

// Factory turns a Selection into an immutable ActiveModel. It snapshots the
// resolution at construction, so rebuilding the resolver later cannot change
// models handed out by an existing factory.
type Factory struct {
	providers []ProviderView
}

// defaultMaxOutputTokens is the fallback used only when a model's spec max is
// missing. Executable models always carry a positive max.
const defaultMaxOutputTokens = 8192

// NewFactory builds a factory from one resolution.
func NewFactory(resolution *Resolution) (*Factory, error) {
	if resolution == nil {
		return nil, errors.New("modelcfg: nil resolution")
	}
	return &Factory{providers: cloneProviderViews(resolution.Providers)}, nil
}

// Resolve locates the selected model and builds its frozen ActiveModel. Any
// missing configuration, disabled flag, unknown reasoning level or unsupported
// protocol is a fail-closed ErrNotExecutable error carrying the issues.
func (f *Factory) Resolve(selection Selection) (ActiveModel, error) {
	if f == nil {
		return ActiveModel{}, notExecutableError(selection.ProviderID, selection.ModelID, []Issue{{
			Field:   "providerId",
			Message: "model factory is not initialized",
		}})
	}
	providerID := strings.TrimSpace(selection.ProviderID)
	modelID := strings.TrimSpace(selection.ModelID)
	if modelID == "" {
		return ActiveModel{}, notExecutableError(providerID, "", []Issue{{
			Field:   "modelId",
			Message: "model id is required",
		}})
	}

	provider, model, issues := f.locate(providerID, modelID)
	if len(issues) > 0 {
		return ActiveModel{}, notExecutableError(provider.ProviderID, modelID, issues)
	}
	if !provider.Executable || !model.Executable || !model.Enabled {
		combined := cloneIssues(provider.Issues)
		combined = append(combined, cloneIssues(model.Issues)...)
		return ActiveModel{}, notExecutableError(provider.ProviderID, model.ModelID, combined)
	}

	levels := model.Options.ReasoningLevel.Values
	level := strings.TrimSpace(selection.ReasoningLevel)
	if level == "" {
		if len(levels) == 0 {
			return ActiveModel{}, notExecutableError(provider.ProviderID, model.ModelID, []Issue{{
				Field:   "options.reasoningLevel.values",
				Message: "reasoning level values must be non-empty",
			}})
		}
		level = levels[0]
	}
	reasoningPatch, ok := model.Options.ReasoningLevel.Map[level]
	if !levelAllowed(levels, level) || !ok || reasoningPatch == nil {
		return ActiveModel{}, notExecutableError(provider.ProviderID, model.ModelID, []Issue{{
			Field:   "reasoningLevel",
			Message: fmt.Sprintf("unknown reasoning level %q (want %s)", level, strings.Join(levels, "|")),
		}})
	}

	maxOutputTokens := model.Options.MaxOutputTokens.Max
	if maxOutputTokens <= 0 {
		maxOutputTokens = defaultMaxOutputTokens
	}

	requestPatch := map[string]any{}
	requestPatch = mergeJSONObjects(requestPatch, reasoningPatch)
	if patch := model.Options.MaxOutputTokens.Map[""]; patch != nil {
		requestPatch = mergeJSONObjects(requestPatch, patch)
	}
	if patch := model.Options.MaxOutputTokens.Map["*"]; patch != nil {
		requestPatch = mergeJSONObjects(requestPatch, patch)
	}

	switch model.API.Type {
	case APITypeOpenAIChatCompletions:
		requestPatch["max_tokens"] = maxOutputTokens
	case APITypeOpenAIResponses:
		requestPatch["max_output_tokens"] = maxOutputTokens
	case APITypeAnthropicMessages:
		requestPatch["max_tokens"] = maxOutputTokens
		clampThinkingBudget(requestPatch, maxOutputTokens)
	default:
		return ActiveModel{}, notExecutableError(provider.ProviderID, model.ModelID, []Issue{{
			Field:   "api.type",
			Message: fmt.Sprintf("unsupported API type %q", model.API.Type),
		}})
	}

	return ActiveModel{
		ProviderID:      provider.ProviderID,
		ModelID:         model.ModelID,
		API:             cloneAPI(model.API),
		Access:          provider.Access,
		Properties:      model.Properties,
		Options:         cloneOptions(model.Options),
		ReasoningLevel:  level,
		MaxOutputTokens: maxOutputTokens,
		RequestPatch:    requestPatch,
	}, nil
}

// locate finds a provider/model pair. When providerID is empty the model id
// must resolve to exactly one identity across the whole configuration; an
// ambiguous id is an error rather than a guess.
func (f *Factory) locate(providerID, modelID string) (ProviderView, ModelView, []Issue) {
	if providerID == "" {
		exact := f.matches(func(model ModelView) bool { return model.ModelID == modelID })
		matches := exact
		if len(matches) == 0 {
			matches = f.matches(func(model ModelView) bool {
				return strings.EqualFold(model.ModelID, modelID)
			})
		}
		switch len(matches) {
		case 0:
			return ProviderView{}, ModelView{}, []Issue{{
				Field:   "modelId",
				Message: fmt.Sprintf("unknown model %q", modelID),
			}}
		case 1:
			return matches[0].provider, matches[0].model, nil
		default:
			providers := make([]string, 0, len(matches))
			for _, match := range matches {
				providers = append(providers, match.provider.ProviderID)
			}
			return ProviderView{}, ModelView{}, []Issue{{
				Field: "providerId",
				Message: fmt.Sprintf("model %q is ambiguous across providers (%s); providerId is required",
					modelID, strings.Join(providers, ", ")),
			}}
		}
	}

	for i := range f.providers {
		provider := f.providers[i]
		if provider.ProviderID != providerID {
			continue
		}
		for j := range provider.Models {
			if provider.Models[j].ModelID == modelID {
				return provider, provider.Models[j], nil
			}
		}
		return provider, ModelView{}, []Issue{{
			Field:   "modelId",
			Message: fmt.Sprintf("unknown model %q for provider %q", modelID, providerID),
		}}
	}
	return ProviderView{}, ModelView{}, []Issue{{
		Field:   "providerId",
		Message: fmt.Sprintf("unknown provider %q", providerID),
	}}
}

type providerModelMatch struct {
	provider ProviderView
	model    ModelView
}

func (f *Factory) matches(predicate func(ModelView) bool) []providerModelMatch {
	out := make([]providerModelMatch, 0, 2)
	for i := range f.providers {
		for j := range f.providers[i].Models {
			if predicate(f.providers[i].Models[j]) {
				out = append(out, providerModelMatch{
					provider: f.providers[i],
					model:    f.providers[i].Models[j],
				})
			}
		}
	}
	return out
}

func clampThinkingBudget(patch map[string]any, maxOutputTokens int) {
	thinking, ok := patch["thinking"].(map[string]any)
	if !ok {
		return
	}
	budget, ok := intValue(thinking["budget_tokens"])
	if !ok {
		return
	}
	if budget > maxOutputTokens {
		thinking["budget_tokens"] = maxOutputTokens
	}
}

// levelAllowed enforces that a selection uses one of the declared ordered
// values, not merely an extra key that happens to exist in the patch map.
func levelAllowed(levels []string, level string) bool {
	for _, candidate := range levels {
		if candidate == level {
			return true
		}
	}
	return false
}

func notExecutableError(providerID, modelID string, issues []Issue) error {
	return &NotExecutableError{
		ProviderID: providerID,
		ModelID:    modelID,
		Issues:     cloneIssues(issues),
	}
}
