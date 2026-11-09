package servicehost

// The model service is the product layer's provider configuration: it resolves
// the run's selection fact into the execution identity of one attempt
// (agent.ModelResolver) and exposes the same resolution as the read-only views
// the API surface serves.
//
// It is frozen per loadout generation. The llm service descriptor carries a
// version derived from the models document (see loadout.serviceVersions), so a
// configuration change produces a new generation and a new instance: a run
// already pinned to the previous generation keeps resolving with the
// configuration it started under.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jixoai/keel/internal/agent"
	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/modelcfg"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
)

// ModelService is the product-layer provider configuration: the runtime's
// per-attempt resolver plus the read-only projections the API surface serves.
type ModelService interface {
	Resolve(ctx context.Context, scope kernel.Scope, selection json.RawMessage) (agent.ActiveModel, error)
	ValidateSelection(raw json.RawMessage) error
	ProviderViews() []modelcfg.ProviderView
	ModelViews() []modelcfg.ModelView
}

// modelsService implements ModelService over one immutable resolution.
type modelsService struct {
	factory    *modelcfg.Factory
	resolution modelcfg.Resolution
	// selection is the tenant document's default selection; an input whose fact
	// names no model inherits it.
	selection modelcfg.Selection
	// legacyKey is the hosting process's flat key (--api-key / KEEL_API_KEY),
	// the fallback for a provider whose access.env is unset or empty.
	legacyKey  string
	lookup     func(string) (string, bool)
	wrapStream func(provider.StreamFn) provider.StreamFn
}

var _ agent.ModelResolver = (*modelsService)(nil)

func (m *modelsService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (m *modelsService) Stop(context.Context) error                             { return nil }

// newModelsService resolves one tenant document (plus the hosting process's
// credential inputs) into the model service of a generation. A document the
// resolver cannot use is a construction error: the llm service then fails to
// activate and boot/reassembly reports it instead of running with a
// configuration nobody can honour.
func newModelsService(tenant host.TenantConfig, llm LLMOptions, wrap func(provider.StreamFn) provider.StreamFn) (*modelsService, error) {
	agentCfg := tenant.Agent
	overlay, err := modelcfg.OverlayFromDocument(tenant.Models, modelcfg.LegacyAgent{
		Provider:  agentCfg.Provider,
		Model:     agentCfg.Model,
		BaseURL:   agentCfg.BaseURL,
		Protocol:  agentCfg.Protocol,
		APIKeyRef: agentCfg.APIKeyRef,
	})
	if err != nil {
		return nil, fmt.Errorf("model configuration: %w", err)
	}
	resolver, err := modelcfg.NewResolver(modelcfg.EmbeddedBuiltin(), overlay)
	if err != nil {
		return nil, fmt.Errorf("model configuration: %w", err)
	}
	resolution := resolver.Resolve()
	factory, err := modelcfg.NewFactory(&resolution)
	if err != nil {
		return nil, fmt.Errorf("model configuration: %w", err)
	}
	return &modelsService{
		factory:    factory,
		resolution: resolution,
		selection:  overlay.Selection(),
		legacyKey:  llm.APIKey,
		lookup:     llm.LookupEnv,
		wrapStream: wrap,
	}, nil
}

// modelsConfigured reports whether the tenant declares a provider
// configuration: an explicit models section (an empty object carries none), or
// the legacy flat agent model the normalization turns into one anonymous
// provider. Without either there is nothing to resolve and the runner keeps
// its static stream identity (the fake/legacy path stays untouched).
func modelsConfigured(tenant host.TenantConfig) bool {
	trimmed := bytes.TrimSpace(tenant.Models)
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("{}")) {
		return true
	}
	return strings.TrimSpace(tenant.Agent.Model) != ""
}

// Resolve turns the run's selection fact into the execution identity of one
// attempt. The fact is parsed, the tenant default applies when it names no
// model, and the factory fails closed on anything it cannot execute: sending
// the wrong model is worse than failing the attempt.
func (m *modelsService) Resolve(_ context.Context, _ kernel.Scope, raw json.RawMessage) (agent.ActiveModel, error) {
	active, err := m.resolveActive(raw)
	if err != nil {
		return agent.ActiveModel{}, err
	}
	key, err := m.credential(active)
	if err != nil {
		return agent.ActiveModel{}, err
	}
	stream, err := m.stream(active)
	if err != nil {
		return agent.ActiveModel{}, err
	}
	level, err := thinkingLevel(active)
	if err != nil {
		return agent.ActiveModel{}, err
	}
	return agent.ActiveModel{
		ProviderID:    active.ProviderID,
		Model:         active.ModelID,
		APIKey:        key,
		Stream:        stream,
		ThinkingLevel: level,
		ContextWindow: active.Properties.ContextWindow,
		RequestPatch:  active.RequestPatch,
	}, nil
}

// ValidateSelection is the submit-time check: the same fail-closed resolution
// as an attempt, without touching a provider or a credential.
func (m *modelsService) ValidateSelection(raw json.RawMessage) error {
	_, err := m.resolveActive(raw)
	return err
}

// ProviderViews returns the resolved settings for visible providers only: a
// hidden provider is not part of the API surface.
func (m *modelsService) ProviderViews() []modelcfg.ProviderView {
	all := m.resolution.Settings()
	out := make([]modelcfg.ProviderView, 0, len(all))
	for _, view := range all {
		if view.Visibility == modelcfg.VisibilityHidden {
			continue
		}
		out = append(out, view)
	}
	return out
}

// ModelViews returns the flat, selectable model list of the visible providers.
// The visibility filter is repeated here (the resolution already refuses to
// select models of a hidden provider) so a hidden provider can never leak into
// a picker projection.
func (m *modelsService) ModelViews() []modelcfg.ModelView {
	visible := make(map[string]struct{}, len(m.resolution.Providers))
	for _, view := range m.resolution.Providers {
		if view.Visibility != modelcfg.VisibilityHidden {
			visible[view.ProviderID] = struct{}{}
		}
	}
	all := m.resolution.Models()
	out := make([]modelcfg.ModelView, 0, len(all))
	for _, model := range all {
		if _, ok := visible[model.ProviderID]; !ok {
			continue
		}
		out = append(out, model)
	}
	return out
}

// resolveActive is the shared fail-closed core of Resolve and
// ValidateSelection. It performs no IO.
func (m *modelsService) resolveActive(raw json.RawMessage) (modelcfg.ActiveModel, error) {
	selection, err := modelcfg.ParseSelection(raw)
	if err != nil {
		return modelcfg.ActiveModel{}, err
	}
	if selection.ModelID == "" {
		selection = m.selection
	}
	active, err := m.factory.Resolve(selection)
	if err != nil {
		return modelcfg.ActiveModel{}, err
	}
	return active, nil
}

// credential resolves the provider's api-key: the access environment variable
// first, then the hosting process's flat key. A provider without any key is an
// error, never a request — a paid endpoint must not be called with an empty
// credential. Neither the key nor the environment value is ever logged.
func (m *modelsService) credential(active modelcfg.ActiveModel) (string, error) {
	env := strings.TrimSpace(active.Access.Env)
	if env != "" {
		lookup := m.lookup
		if lookup == nil {
			lookup = os.LookupEnv
		}
		if value, ok := lookup(env); ok && value != "" {
			return value, nil
		}
	}
	if m.legacyKey != "" {
		return m.legacyKey, nil
	}
	detail := "access.env is unset"
	if env != "" {
		detail = fmt.Sprintf("access.env (%s) is unset or empty", env)
	}
	return "", fmt.Errorf("model %s: no credential: %s and no API key is configured", modelIdentity(active), detail)
}

// stream builds the wire driver for the resolved protocol. The mapping is
// explicit: an unsupported protocol or a header the driver cannot carry is an
// error, never a heuristic guess or a silently dropped field.
func (m *modelsService) stream(active modelcfg.ActiveModel) (provider.StreamFn, error) {
	if len(active.API.Headers) > 0 {
		return nil, fmt.Errorf("model %s: api.headers is not supported by the %s driver",
			modelIdentity(active), active.API.Type)
	}
	var driver provider.Provider
	switch active.API.Type {
	case modelcfg.APITypeOpenAIChatCompletions:
		driver = provider.NewOpenAICompatibleProvider(active.API.BaseURL, nil)
	case modelcfg.APITypeOpenAIResponses:
		driver = provider.NewOpenAIResponsesProvider(active.ProviderID, active.API.BaseURL, nil)
	case modelcfg.APITypeAnthropicMessages:
		driver = provider.NewAnthropicProvider(active.API.BaseURL, nil)
	default:
		return nil, fmt.Errorf("model %s: unsupported API type %q", modelIdentity(active), active.API.Type)
	}
	// A nil model catalog keeps the driver's own capability checks permissive:
	// the resolved properties are the product layer's capability source, and a
	// two-field stub here would read as "no image support" for every model.
	stream := provider.StreamFnFromProvider(driver)
	if m.wrapStream != nil {
		stream = m.wrapStream(stream)
	}
	return stream, nil
}

// thinkingLevel validates the resolved reasoning level against the runtime's
// vocabulary. The level is what drivers map onto their wire dialects, so a
// level the runtime cannot interpret must fail here instead of silently
// degrading to the default.
func thinkingLevel(active modelcfg.ActiveModel) (agentcore.ThinkingLevel, error) {
	level := agentcore.ThinkingLevel(active.ReasoningLevel)
	switch level {
	case agentcore.ThinkingOff, agentcore.ThinkingMinimal, agentcore.ThinkingLow,
		agentcore.ThinkingMedium, agentcore.ThinkingHigh, agentcore.ThinkingXHigh, agentcore.ThinkingMax:
		return level, nil
	default:
		return "", fmt.Errorf("model %s: reasoning level %q is not a runtime thinking level",
			modelIdentity(active), active.ReasoningLevel)
	}
}

func modelIdentity(active modelcfg.ActiveModel) string {
	if active.ProviderID == "" {
		return active.ModelID
	}
	return active.ProviderID + "/" + active.ModelID
}
