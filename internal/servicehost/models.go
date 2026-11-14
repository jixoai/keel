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
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// ResolutionIssues reports resolution-level diagnostics (an empty
	// resolution, every provider hidden) that no single view can carry.
	ResolutionIssues() []modelcfg.Issue
	// DefaultSelection is the tenant document's default selection (the zero
	// value when the document names none). The llm service advertises this
	// identity in its logs and loadout summary.
	DefaultSelection() modelcfg.Selection
}

// modelsService implements ModelService over one immutable resolution.
type modelsService struct {
	factory    *modelcfg.Factory
	resolution modelcfg.Resolution
	// selection is the tenant document's default selection; an input whose fact
	// names no model inherits it.
	selection modelcfg.Selection
	// legacyKey is the hosting process's flat key (--api-key / KEEL_API_KEY),
	// the fallback for a provider whose access.env is not set in this process.
	// It is never a fallback for a credential the provider declares explicitly
	// (an access.env that is set but empty, or an access.ref).
	legacyKey string
	lookup    func(string) (string, bool)
	// lookupSecret resolves `secret:NAME` references from the tenant secret
	// store (the hosting process wires it to the store + SecretBox pair). Nil
	// means this deployment has no secret storage: a secret: reference then
	// fails closed naming the secret, and it is never re-read from the
	// environment instead.
	lookupSecret func(string) (string, bool)
	wrapStream   func(provider.StreamFn) provider.StreamFn
}

var _ agent.ModelResolver = (*modelsService)(nil)

func (m *modelsService) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (m *modelsService) Stop(context.Context) error                             { return nil }

// ModelDocumentError is a models document this runtime would refuse, reported
// as the field-path issues the settings view renders. Boot/reassembly logs its
// Error() text; the settings write path turns it into the 400 body's issues, so
// both surfaces explain a rejection with the same vocabulary.
type ModelDocumentError struct {
	Issues []modelcfg.Issue
}

func (e *ModelDocumentError) Error() string {
	if len(e.Issues) == 0 {
		return "model configuration: the models document cannot be executed"
	}
	parts := make([]string, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue.Field == "" {
			parts = append(parts, issue.Message)
			continue
		}
		parts = append(parts, issue.Field+": "+issue.Message)
	}
	return "model configuration: " + strings.Join(parts, "; ")
}

// buildModelsService resolves one tenant document (plus the hosting process's
// credential inputs) into the model service of a generation, without applying
// the default-selection gate. Callers decide how a default that cannot execute
// is reported: newModelsService fails construction with the issues, the
// settings write path renders the same issues as a 400 body.
func buildModelsService(tenant host.TenantConfig, llm LLMOptions, wrap func(provider.StreamFn) provider.StreamFn) (*modelsService, error) {
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
	// The catalog is the built-in one plus the demo layer when this process
	// enables it. The switch is read from the process environment (the same
	// answer every other surface gives), so a deployment that does not set it
	// resolves the embedded catalog itself and cannot differ from the previous
	// release.
	resolver, err := modelcfg.NewResolver(modelcfg.DemoCatalogFromEnv(), overlay)
	if err != nil {
		return nil, fmt.Errorf("model configuration: %w", err)
	}
	resolution := resolver.Resolve()
	factory, err := modelcfg.NewFactory(&resolution)
	if err != nil {
		return nil, fmt.Errorf("model configuration: %w", err)
	}
	return &modelsService{
		factory:      factory,
		resolution:   resolution,
		selection:    overlay.Selection(),
		legacyKey:    llm.APIKey,
		lookup:       llm.LookupEnv,
		lookupSecret: llm.LookupSecret,
		wrapStream:   wrap,
	}, nil
}

// newModelsService builds the generation's model service and applies the
// boot/reassembly gate: the document's default selection must resolve end to
// end (credential, wire driver, runtime thinking level). A document the
// resolver cannot use is a construction error: the llm service then fails to
// activate and boot/reassembly reports it instead of running with a
// configuration nobody can honour.
func newModelsService(tenant host.TenantConfig, llm LLMOptions, wrap func(provider.StreamFn) provider.StreamFn) (*modelsService, error) {
	service, err := buildModelsService(tenant, llm, wrap)
	if err != nil {
		return nil, err
	}
	if issues := service.defaultSelectionIssues(); len(issues) > 0 {
		return nil, fmt.Errorf("model configuration: default selection: %w", &ModelDocumentError{Issues: issues})
	}
	return service, nil
}

// modelsConfigured reports whether the tenant carries a provider
// configuration document (the "models" slice). The flat agent.* fields are the
// pre-document compatibility path, deliberately NOT a provider configuration
// here: a deployment that has never seen a models document keeps the static llm
// identity, and therefore its historical thinking level and request shape —
// upgrading must not change what such a deployment sends. A document that names
// a provider-less configuration (for example only a selection) still goes
// through the resolver, where the flat fields are the documented incremental
// adoption path.
//
// Emptiness is decided semantically by modelcfg, not by the bytes: `{}`, `{ }`
// and `{"schemaVersion":1}` declare nothing and leave the deployment on the
// static path.
func modelsConfigured(tenant host.TenantConfig) bool {
	return modelcfg.ModelsDocumentConfigured(tenant.Models)
}

// DefaultSelection returns the tenant document's default selection.
func (m *modelsService) DefaultSelection() modelcfg.Selection { return m.selection }

// defaultSelectionIssues explains why the tenant's default selection cannot be
// executed today, as the field-path issues the settings view renders. An empty
// result means it is executable, or that the document names no default at all
// (legal for the runtime — runs then need a selection fact of their own; the
// settings write path additionally requires one).
//
// The verdict comes from the same calls an attempt makes (resolveActive,
// attemptModel); this function only renders their failure.
func (m *modelsService) defaultSelectionIssues() []modelcfg.Issue {
	selection := m.selection
	if strings.TrimSpace(selection.ModelID) == "" && strings.TrimSpace(selection.ProviderID) == "" {
		return nil
	}
	active, err := m.resolveActive(nil)
	if err != nil {
		return selectionIssues(err)
	}
	_, issues, err := m.attemptModel(active)
	if err != nil {
		return issues
	}
	return nil
}

// selectionIssues renders a resolution failure as field-path issues. The
// resolver's own issues already carry model-relative paths (modelId,
// providerId, api.type, ...); they are prefixed with the scope the settings
// document edited.
func selectionIssues(err error) []modelcfg.Issue {
	var notExecutable *modelcfg.NotExecutableError
	if errors.As(err, &notExecutable) {
		out := make([]modelcfg.Issue, 0, len(notExecutable.Issues))
		for _, issue := range notExecutable.Issues {
			out = append(out, modelcfg.Issue{
				Field:   fieldPath("selection", issue.Field),
				Message: issue.Message,
			})
		}
		if len(out) == 0 {
			out = append(out, modelcfg.Issue{Field: "selection", Message: err.Error()})
		}
		return out
	}
	return []modelcfg.Issue{{Field: "selection", Message: err.Error()}}
}

// providerField names a provider-scoped field path (providers[s2api].access.env)
// so a rejection points at the entry the operator edited.
func providerField(providerID, field string) string {
	if strings.TrimSpace(providerID) == "" {
		if strings.TrimSpace(field) == "" {
			return "providers"
		}
		return "providers[]." + field
	}
	if strings.TrimSpace(field) == "" {
		return "providers[" + providerID + "]"
	}
	return "providers[" + providerID + "]." + field
}

func fieldPath(prefix, field string) string {
	field = strings.TrimSpace(field)
	if field == "" {
		return prefix
	}
	return prefix + "." + field
}

// Resolve turns the run's selection fact into the execution identity of one
// attempt. The fact is parsed, the tenant default applies where the fact is
// silent, and the factory fails closed on anything it cannot execute: sending
// the wrong model is worse than failing the attempt.
func (m *modelsService) Resolve(_ context.Context, _ kernel.Scope, raw json.RawMessage) (agent.ActiveModel, error) {
	active, err := m.resolveActive(raw)
	if err != nil {
		return agent.ActiveModel{}, err
	}
	resolved, _, err := m.attemptModel(active)
	return resolved, err
}

// ValidateSelection is the submit-time check. It runs the exact composition an
// attempt runs — resolution, credential, wire driver, runtime thinking level —
// so the input edge can never accept a selection the next attempt would deny.
func (m *modelsService) ValidateSelection(raw json.RawMessage) error {
	if err := modelcfg.ValidateSelectionFact(raw); err != nil {
		return err
	}
	active, err := m.resolveActive(raw)
	if err != nil {
		return err
	}
	_, _, err = m.attemptModel(active)
	return err
}

// ProviderViews returns the resolved settings for every visible provider: a
// hidden provider is not part of the API surface (the resolution does not
// contain it at all). A provider whose credential this process cannot resolve
// stays visible with a diagnostic issue; that is where an operator reads why it
// is missing from the picker list.
func (m *modelsService) ProviderViews() []modelcfg.ProviderView {
	all := m.resolution.Settings()
	out := make([]modelcfg.ProviderView, 0, len(all))
	for _, view := range all {
		if view.Visibility == modelcfg.VisibilityHidden {
			continue
		}
		// An incomplete provider already carries the configuration issue that
		// explains it; the credential diagnostic is for a provider that is
		// complete on paper but cannot authenticate in this process.
		if view.Executable {
			if issue, missing := m.credentialIssue(view.Access); missing {
				view.Issues = append(view.Issues, issue)
				// The picker refuses what an attempt would fail on, so the settings
				// flags must say the same thing: a provider this process cannot
				// authenticate is not selectable here, however complete its config is.
				view.Selectable = false
				for i := range view.Models {
					view.Models[i].Selectable = false
				}
			}
		}
		out = append(out, view)
	}
	return out
}

// ModelViews returns the flat registry list of the visible providers whose
// credential this process can actually resolve: every complete, enabled model,
// including a non-selectable one (a decision model is executable but opens no
// session surface). Selectable is the picker's narrower fact — the client skips
// selectable=false rows and the settings view shows them with their issues. A
// model that is selectable on paper but cannot authenticate here is still not
// offered: the picker must not list what an attempt would fail on, so a
// provider without a resolvable credential contributes nothing. The visibility
// filter is repeated here (the resolution already refuses to resolve hidden
// providers) so a hidden provider can never leak into a picker projection.
func (m *modelsService) ModelViews() []modelcfg.ModelView {
	visible := make(map[string]modelcfg.ProviderView, len(m.resolution.Providers))
	for _, view := range m.resolution.Providers {
		if view.Visibility == modelcfg.VisibilityHidden {
			continue
		}
		if m.credentialAvailable(view.Access) {
			visible[view.ProviderID] = view
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

// ResolutionIssues reports resolution-level diagnostics: with everything
// hidden there is no provider to select, which is a different fact from "no
// provider configuration was loaded" and must read differently.
func (m *modelsService) ResolutionIssues() []modelcfg.Issue {
	if len(m.resolution.Providers) > 0 {
		return nil
	}
	return []modelcfg.Issue{{
		Field:   "providers",
		Message: "every configured provider is hidden; no model can be selected",
	}}
}

// ProvidersExecutable counts the visible providers this process can actually
// serve: complete on paper AND holding a credential it can resolve here. It is
// the count the write summary publishes, and it is derived from the same
// ProviderViews the settings/registry endpoints serve, so the number and the
// list can never disagree.
func (m *modelsService) ProvidersExecutable() int {
	count := 0
	for _, view := range m.ProviderViews() {
		if view.Executable && m.credentialAvailable(view.Access) {
			count++
		}
	}
	return count
}

// ModelsSelectable counts the models a picker may offer: the selectable subset
// of the flat registry list. ModelViews also carries non-selectable entries (a
// decision model is executable but opens no session), so this is a filter, not
// a length.
func (m *modelsService) ModelsSelectable() int {
	count := 0
	for _, model := range m.ModelViews() {
		if model.Selectable {
			count++
		}
	}
	return count
}

// resolveActive is the shared fail-closed core of Resolve and
// ValidateSelection. It performs no IO.
//
// The fact overlays the tenant default field by field: a fact that names a
// model executes that identity; a fact that only narrows the reasoning level
// keeps the default model and takes the level. A fact that names a provider
// without a model is refused rather than resolved to "that provider's default
// model" — picking a model here would be a guess, and a wrong model on the wire
// is worse than a rejected input.
func (m *modelsService) resolveActive(raw json.RawMessage) (modelcfg.ActiveModel, error) {
	fact, err := modelcfg.ParseSelection(raw)
	if err != nil {
		return modelcfg.ActiveModel{}, err
	}
	selection := m.selection
	switch {
	case fact.ModelID != "":
		selection = fact
	case fact.ProviderID != "":
		return modelcfg.ActiveModel{}, fmt.Errorf(
			"selection: providerId %q is not enough to choose a model: modelId is required", fact.ProviderID)
	case fact.ReasoningLevel != "":
		selection.ReasoningLevel = fact.ReasoningLevel
	}
	return m.factory.Resolve(selection)
}

// attemptModel composes the attempt's execution identity from a resolved
// ActiveModel: the credential, the runtime thinking level and the wire driver
// must all exist and agree with the resolved protocol. It is the one
// composition ValidateSelection and Resolve share.
//
// A failure is reported twice over: as the error the attempt path returns, and
// as the field-path issue the settings view renders (the decider is the failing
// check itself — there is no second validation pass that could disagree).
func (m *modelsService) attemptModel(active modelcfg.ActiveModel) (agent.ActiveModel, []modelcfg.Issue, error) {
	key, err := m.credential(active)
	if err != nil {
		issue, _ := m.credentialIssue(active.Access)
		message := strings.TrimSpace(issue.Message)
		if message == "" {
			message = err.Error()
		}
		return agent.ActiveModel{}, []modelcfg.Issue{{
			Field:   providerField(active.ProviderID, issue.Field),
			Message: message,
		}}, err
	}
	level, err := thinkingLevel(active)
	if err != nil {
		return agent.ActiveModel{}, []modelcfg.Issue{{
			Field:   "selection.reasoningLevel",
			Message: fmt.Sprintf("reasoning level %q is not a runtime thinking level", active.ReasoningLevel),
		}}, err
	}
	stream, err := m.stream(active)
	if err != nil {
		return agent.ActiveModel{}, []modelcfg.Issue{{
			Field:   providerField(active.ProviderID, "api"),
			Message: err.Error(),
		}}, err
	}
	return agent.ActiveModel{
		ProviderID:    active.ProviderID,
		Model:         active.ModelID,
		APIKey:        key,
		Stream:        stream,
		ThinkingLevel: level,
		ContextWindow: m.contextWindow(active),
		RequestPatch:  active.RequestPatch,
		// The capability facts travel with the identity: the runtime enforces what
		// the configuration declares (the tool gate today; the media gate is a
		// recorded deferral), and it must not have to re-derive them.
		Properties: &agent.ModelProperties{
			SupportsToolCall:         active.Properties.SupportsToolCall,
			SupportsJSONSchemaOutput: active.Properties.SupportsJSONSchemaOutput,
			SupportsNativeWebSearch:  active.Properties.SupportsNativeWebSearch,
			ImageInput:               active.Properties.InputFormat.Image,
		},
	}, nil, nil
}

// contextWindow returns the window the attempt may compact against. A window
// that only wildcard/baseline rules contributed is not a declaration about this
// model — nobody described it — so it is withheld: the runner then keeps the
// tenant's compaction window authoritative instead of compacting at an invented
// number.
func (m *modelsService) contextWindow(active modelcfg.ActiveModel) int {
	if view, ok := m.resolution.Find(active.ProviderID, active.ModelID); ok && view.Baseline {
		return 0
	}
	return active.Properties.ContextWindow
}

// credential resolves the provider's api-key. The declared access shape decides
// the source: an explicit secret reference, otherwise the declared environment
// variable, otherwise the hosting process's flat key. A declared source that
// cannot produce a value fails closed — falling back to another credential
// would silently send the wrong provider's key — and neither the key nor the
// environment value is ever logged.
func (m *modelsService) credential(active modelcfg.ActiveModel) (string, error) {
	access := active.Access
	// A provider that declares no credential resolves to the empty key. This is
	// not a fallback: the declaration is the fact, and asking the environment for
	// a value the provider never wanted would be the bug. The demo protocol and a
	// local endpoint are the two shapes that declare it.
	if access.CredentialFree() && strings.TrimSpace(access.Ref) == "" && strings.TrimSpace(access.Env) == "" {
		return "", nil
	}
	if ref := strings.TrimSpace(access.Ref); ref != "" {
		value, reason := m.resolveRef(ref)
		if reason == "" {
			return value, nil
		}
		return "", fmt.Errorf("model %s: no credential: access.ref (%s) %s", modelIdentity(active), ref, reason)
	}
	if env := strings.TrimSpace(access.Env); env != "" {
		value, found := m.lookupEnv(env)
		switch {
		case found && value != "":
			return value, nil
		case !found && m.legacyKey != "":
			// The variable is not set at all: the process-wide key is the
			// documented fallback. A variable that is set but empty never falls
			// back — that is a deliberate empty value, not a missing one.
			return m.legacyKey, nil
		case found:
			return "", fmt.Errorf("model %s: no credential: access.env (%s) is set but empty", modelIdentity(active), env)
		default:
			return "", fmt.Errorf("model %s: no credential: access.env (%s) is not set in this process and no API key is configured",
				modelIdentity(active), env)
		}
	}
	if m.legacyKey != "" {
		return m.legacyKey, nil
	}
	return "", fmt.Errorf("model %s: no credential: the provider declares no access reference and no API key is configured",
		modelIdentity(active))
}

// credentialAvailable reports whether this process can produce a credential
// for the provider, applying exactly the rules credential() applies. It is the
// picker's gate: a model whose provider cannot authenticate here is not a
// choice. The value itself is never returned or logged.
func (m *modelsService) credentialAvailable(access modelcfg.Access) bool {
	_, missing := m.credentialIssue(access)
	return !missing
}

// credentialIssue reports why a provider's credential cannot be resolved here,
// as an operator-readable issue for the settings view. Availability and the
// diagnostic come from one function, so the picker and the settings view can
// never disagree about whether a provider can authenticate.
func (m *modelsService) credentialIssue(access modelcfg.Access) (modelcfg.Issue, bool) {
	// A declared credential-free provider has nothing to resolve, so it is
	// available by construction — the same verdict credential() returns.
	if access.CredentialFree() && strings.TrimSpace(access.Ref) == "" && strings.TrimSpace(access.Env) == "" {
		return modelcfg.Issue{}, false
	}
	if ref := strings.TrimSpace(access.Ref); ref != "" {
		if _, reason := m.resolveRef(ref); reason == "" {
			return modelcfg.Issue{}, false
		} else {
			return modelcfg.Issue{
				Field:   "access.ref",
				Message: fmt.Sprintf("secret reference %q %s", ref, reason),
			}, true
		}
	}
	if env := strings.TrimSpace(access.Env); env != "" {
		value, found := m.lookupEnv(env)
		switch {
		case found && value != "":
			return modelcfg.Issue{}, false
		case found:
			return modelcfg.Issue{
				Field:   "access.env",
				Message: fmt.Sprintf("environment variable %q is set but empty", env),
			}, true
		case m.legacyKey != "":
			return modelcfg.Issue{}, false
		default:
			return modelcfg.Issue{
				Field:   "access.env",
				Message: fmt.Sprintf("environment variable %q is not set and no process API key is configured", env),
			}, true
		}
	}
	if m.legacyKey != "" {
		return modelcfg.Issue{}, false
	}
	return modelcfg.Issue{
		Field:   "access",
		Message: "no access reference is declared and no process API key is configured",
	}, true
}

// resolveRef resolves a declared secret reference. The reference kind decides
// the source and the kinds never cross over: `secret:NAME` resolves from the
// tenant secret store only (a deployment without one fails closed naming the
// secret), while `${NAME}` and `env:NAME` resolve from the injected environment
// lookup. An unresolvable reference is a failed credential, not an invitation
// to try another source — the second return is the reason it could not resolve
// (empty on success), always naming the reference and never a value.
func (m *modelsService) resolveRef(ref string) (string, string) {
	kind, name, ok := secretRefParts(ref)
	if !ok {
		return "", `is not a secret reference (want ${NAME}, env:NAME or secret:NAME)`
	}
	if kind == "secret" {
		if m.lookupSecret == nil {
			return "", fmt.Sprintf("names the tenant secret %q, but this deployment has no tenant secret storage (KEEL_SECRET_KEY is not configured)", name)
		}
		value, found := m.lookupSecret(name)
		if !found || value == "" {
			return "", fmt.Sprintf("names the tenant secret %q, which is not stored for this tenant", name)
		}
		return value, ""
	}
	value, found := m.lookupEnv(name)
	if !found || value == "" {
		return "", fmt.Sprintf("names the environment variable %q, which is not set in this process", name)
	}
	return value, ""
}

// secretRefParts splits one declared reference into its kind and its name,
// using the modelcfg vocabulary: ${NAME}, env:NAME, secret:NAME. The grammar is
// deliberately identical to modelcfg's own (a reference the resolver accepted
// must not be re-read differently here), and an unknown shape is refused.
func secretRefParts(ref string) (kind, name string, ok bool) {
	value := strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		return "env", value[2 : len(value)-1], true
	case strings.HasPrefix(value, "env:"):
		return "env", strings.TrimPrefix(value, "env:"), true
	case strings.HasPrefix(value, "secret:"):
		return "secret", strings.TrimPrefix(value, "secret:"), true
	}
	return "", "", false
}

// lookupEnv consults only the injected lookup. A nil lookup resolves nothing:
// the service must never reach into the process environment behind the host's
// back (a test or an embedded host would otherwise read the developer's real
// environment).
func (m *modelsService) lookupEnv(name string) (string, bool) {
	if m.lookup == nil {
		return "", false
	}
	return m.lookup(name)
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
	case modelcfg.APITypeDemo:
		// The in-process demo driver. It reaches no endpoint and needs no
		// credential, which is exactly why the protocol is distinct: a fake base
		// URL would be dialed the day this branch is missed.
		driver = provider.NewDemoProvider()
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
