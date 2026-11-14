package modelcfg

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/demo"
)

// demoListedModels returns the demo provider's models as the /v1/models
// projection lists them, in catalog order. The list is wider than the picker:
// a decision model is executable but not session-selectable, so it is here with
// Selectable=false.
func demoListedModels(t *testing.T, resolution Resolution) []ModelView {
	t.Helper()
	var out []ModelView
	for _, model := range resolution.Models() {
		if model.ProviderID == demo.ProviderID {
			out = append(out, model)
		}
	}
	return out
}

// TestDemoProviderAbsentByDefault is the "not enabled has zero impact" gate: the
// catalog a production deployment resolves is the embedded one, and no demo
// identity exists anywhere in it.
func TestDemoProviderAbsentByDefault(t *testing.T) {
	// The disabled catalog must be the embedded catalog itself, not a copy: a
	// deployment without the switch cannot differ from the previous release.
	if got, want := DemoCatalog(false), EmbeddedBuiltin(); got != want {
		t.Fatalf("DemoCatalog(false) must return the embedded catalog")
	}

	resolution := resolveFrom(t, DemoCatalog(false), Overlay{})
	for _, model := range resolution.Models() {
		if demo.FamilyInModelIDs(model.ModelID) {
			t.Fatalf("a disabled demo provider must contribute no model, found %s/%s",
				model.ProviderID, model.ModelID)
		}
	}
	for _, provider := range resolution.Settings() {
		if IsDemoProvider(provider.ProviderID) {
			t.Fatalf("a disabled demo provider must not be resolved: %+v", provider)
		}
	}
	if strings.Contains(string(demoMarshal(t, resolution)), `"providerId":"demo"`) {
		t.Fatal("the disabled resolution must not mention the demo provider")
	}
}

// TestDemoProviderRegistersSixModels pins the enabled catalog: one visible,
// selectable provider named by demo.ProviderID with exactly the six documented
// models, each carrying the family and media facts its surface reads.
func TestDemoProviderRegistersSixModels(t *testing.T) {
	resolution := resolveFrom(t, DemoCatalog(true), Overlay{})

	provider := mustFindProvider(t, resolution, demo.ProviderID)
	if !provider.Selectable || !provider.Executable {
		t.Fatalf("the demo provider must be complete and selectable, issues: %v", issueMessages(provider.Issues))
	}
	if len(provider.Models) != len(demo.ModelIDs()) {
		t.Fatalf("demo provider models = %d, want %d", len(provider.Models), len(demo.ModelIDs()))
	}

	wantFamilies := map[string]string{
		demo.ModelLLM:      "llm",
		demo.ModelImage:    "image",
		demo.ModelDecision: "decision",
		demo.ModelRealtime: "realtime",
		demo.ModelTTS:      "tts",
		demo.ModelSTT:      "stt",
	}
	wantSurfaces := map[string]string{
		demo.ModelLLM:      SurfaceKindLLM,
		demo.ModelImage:    SurfaceKindImage,
		demo.ModelDecision: SurfaceKindGeneric,
		demo.ModelRealtime: SurfaceKindRealtime,
		demo.ModelTTS:      SurfaceKindSpeech,
		demo.ModelSTT:      SurfaceKindSpeech,
	}
	// Only a family with a session surface is a session choice. The decision
	// model stays listed (executable, visible in Settings with its issue) but is
	// not selectable: choosing it would open the generic placeholder, a dead end.
	wantSelectable := map[string]bool{
		demo.ModelLLM:      true,
		demo.ModelImage:    true,
		demo.ModelDecision: false,
		demo.ModelRealtime: true,
		demo.ModelTTS:      true,
		demo.ModelSTT:      true,
	}

	listed := demoListedModels(t, resolution)
	if len(listed) != len(demo.ModelIDs()) {
		t.Fatalf("demo listed models = %d, want %d: %+v", len(listed), len(demo.ModelIDs()), listed)
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].ModelID < listed[j].ModelID })

	for _, model := range listed {
		family, ok := wantFamilies[model.ModelID]
		if !ok {
			t.Fatalf("unexpected demo model %q", model.ModelID)
		}
		if model.ProviderType != family {
			t.Fatalf("%s providerType = %q, want %q", model.ModelID, model.ProviderType, family)
		}
		if model.SurfaceKind != wantSurfaces[model.ModelID] {
			t.Fatalf("%s surfaceKind = %q, want %q", model.ModelID, model.SurfaceKind, wantSurfaces[model.ModelID])
		}
		if !model.Executable {
			t.Fatalf("%s must be executable, issues: %v", model.ModelID, issueMessages(model.Issues))
		}
		if model.Selectable != wantSelectable[model.ModelID] {
			t.Fatalf("%s selectable = %v, want %v", model.ModelID, model.Selectable, wantSelectable[model.ModelID])
		}
		if !wantSelectable[model.ModelID] {
			requireIssue(t, model.Issues, "providerType", "decision models are capability endpoints called through /v1/decisions/ask (or /v1/media/decision), not session models: the picker does not offer them as a conversation")
		}
		if model.Baseline {
			t.Fatalf("%s must carry its own rule, not baseline-derived facts", model.ModelID)
		}
		if model.API.Type != APITypeDemo {
			t.Fatalf("%s api.type = %q, want %q", model.ModelID, model.API.Type, APITypeDemo)
		}
		if model.Properties.ContextWindow <= 0 {
			t.Fatalf("%s contextWindow = %d, want a positive window", model.ModelID, model.Properties.ContextWindow)
		}
		if len(model.Options.ReasoningLevel.Values) == 0 || model.Options.MaxOutputTokens.Max <= 0 {
			t.Fatalf("%s options are incomplete: %+v", model.ModelID, model.Options)
		}
	}

	// The picker's own subset: one model per session surface, never a decision.
	selectable := make([]string, 0, len(listed))
	for _, model := range listed {
		if !model.Selectable {
			continue
		}
		if family, ok := demo.Family(model.ModelID); ok && family == demo.FamilyDecision {
			t.Fatalf("decision model %q must never be session-selectable", model.ModelID)
		}
		selectable = append(selectable, model.ModelID)
	}
	if len(selectable) != len(demo.ModelIDs())-1 {
		t.Fatalf("demo selectable models = %d, want %d: %v", len(selectable), len(demo.ModelIDs())-1, selectable)
	}

	// The per-family formats are what the client's capability gate and surface
	// resolution read, so they are asserted explicitly.
	formats := map[string]struct {
		textIn, audioIn   bool
		textOut, audioOut bool
		imageOut          bool
	}{
		demo.ModelLLM:      {textIn: true, textOut: true},
		demo.ModelImage:    {textIn: true, imageOut: true},
		demo.ModelDecision: {textIn: true, textOut: true},
		demo.ModelRealtime: {textIn: true, audioIn: true, textOut: true, audioOut: true},
		demo.ModelTTS:      {textIn: true, audioOut: true},
		demo.ModelSTT:      {textIn: true, audioIn: true, textOut: true},
	}
	for _, model := range listed {
		want := formats[model.ModelID]
		got := model.Properties
		if got.InputFormat.Text != want.textIn || got.InputFormat.Audio != want.audioIn {
			t.Fatalf("%s inputFormat = %+v, want text=%v audio=%v",
				model.ModelID, got.InputFormat, want.textIn, want.audioIn)
		}
		if got.OutputFormat.Text != want.textOut || got.OutputFormat.Audio != want.audioOut ||
			got.OutputFormat.Image != want.imageOut {
			t.Fatalf("%s outputFormat = %+v, want text=%v audio=%v image=%v",
				model.ModelID, got.OutputFormat, want.textOut, want.audioOut, want.imageOut)
		}
	}
}

// TestDemoModelsResolveThroughTheFactory pins that the demo identity executes
// through the ordinary resolution path: the factory returns a frozen ActiveModel
// with the demo protocol, no credential requirement and a request patch that
// carries the resolved max output tokens.
func TestDemoModelsResolveThroughTheFactory(t *testing.T) {
	resolution := resolveFrom(t, DemoCatalog(true), Overlay{})
	factory := mustFactory(t, resolution)

	for _, modelID := range demo.ModelIDs() {
		active, err := factory.Resolve(Selection{ProviderID: demo.ProviderID, ModelID: modelID})
		if err != nil {
			t.Fatalf("Resolve(%s): %v", modelID, err)
		}
		if active.ProviderID != demo.ProviderID || active.ModelID != modelID {
			t.Fatalf("identity = %s/%s, want %s/%s", active.ProviderID, active.ModelID, demo.ProviderID, modelID)
		}
		if active.API.Type != APITypeDemo {
			t.Fatalf("%s api.type = %q, want %q", modelID, active.API.Type, APITypeDemo)
		}
		if !active.Access.CredentialFree() {
			t.Fatalf("%s access = %+v, want the credential-free declaration", modelID, active.Access)
		}
		if active.MaxOutputTokens <= 0 {
			t.Fatalf("%s maxOutputTokens = %d", modelID, active.MaxOutputTokens)
		}
		if got, ok := active.RequestPatch["max_tokens"]; !ok || got != demoMaxOutputTokens {
			t.Fatalf("%s request patch max_tokens = %#v, want %d", modelID, got, demoMaxOutputTokens)
		}
		if active.ReasoningLevel != "off" {
			t.Fatalf("%s default reasoning level = %q, want off", modelID, active.ReasoningLevel)
		}
	}
}

// TestDemoProviderLeavesVendorModelsUntouched pins the blast radius: enabling the
// demo provider must not change a single fact of a vendor model. The demo layer
// uses only provider-scoped rules for exactly this reason.
func TestDemoProviderLeavesVendorModelsUntouched(t *testing.T) {
	plain := resolveFrom(t, DemoCatalog(false), Overlay{})
	withDemo := resolveFrom(t, DemoCatalog(true), Overlay{})

	for _, before := range plain.Models() {
		after := mustFindModel(t, withDemo, before.ProviderID, before.ModelID)
		if !sameJSON(t, before, after) {
			t.Fatalf("enabling the demo provider changed %s/%s:\nbefore %s\nafter  %s",
				before.ProviderID, before.ModelID, demoMarshal(t, before), demoMarshal(t, after))
		}
	}
	// Vendor providers are unchanged too (the demo provider is an extra entry).
	for _, before := range plain.Settings() {
		after := mustFindProvider(t, withDemo, before.ProviderID)
		if !sameJSON(t, before, after) {
			t.Fatalf("enabling the demo provider changed provider %q", before.ProviderID)
		}
	}
}

// TestDemoProviderHonoursTenantOverlay pins that the tenant stays the last word:
// the tenant may hide the demo provider (it disappears from every projection) or
// patch one of its models, exactly as it may for a built-in vendor.
func TestDemoProviderHonoursTenantOverlay(t *testing.T) {
	hidden := mustOverlay(t, map[string]any{
		"providers": []any{map[string]any{"id": demo.ProviderID, "visibility": VisibilityHidden}},
	}, LegacyAgent{})
	resolution := resolveFrom(t, DemoCatalog(true), hidden)
	for _, provider := range resolution.Settings() {
		if IsDemoProvider(provider.ProviderID) {
			t.Fatal("a tenant-hidden demo provider must not be resolved")
		}
	}
	if models := demoListedModels(t, resolution); len(models) != 0 {
		t.Fatalf("a tenant-hidden demo provider must contribute no models: %+v", models)
	}

	disabled := mustOverlay(t, map[string]any{
		"providerModelRules": []any{map[string]any{
			"providerId": demo.ProviderID,
			"modelId":    demo.ModelLLM,
			"config":     map[string]any{"enabled": false},
		}},
	}, LegacyAgent{})
	resolution = resolveFrom(t, DemoCatalog(true), disabled)
	model := mustFindModel(t, resolution, demo.ProviderID, demo.ModelLLM)
	if model.Selectable {
		t.Fatal("a tenant-disabled demo model must not be selectable")
	}
	// The other five models stay available: the patch is per model.
	if len(demoListedModels(t, resolution)) != len(demo.ModelIDs())-1 {
		t.Fatalf("demo picker models = %d, want %d", len(demoListedModels(t, resolution)), len(demo.ModelIDs())-1)
	}
}

// TestDemoEnablementTokens pins the switch: only an affirmative token enables the
// demo provider, so a typo or an explicit "0" leaves a production deployment
// untouched.
func TestDemoEnablementTokens(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !demo.Enabled(value) {
			t.Fatalf("%q must enable the demo provider", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "off", "enabled", "demo"} {
		if demo.Enabled(value) {
			t.Fatalf("%q must not enable the demo provider", value)
		}
	}
}

// TestDemoProtocolCompleteness pins the provider-level rules the demo provider
// relies on: the demo protocol needs no base URL and the credential-free access
// needs no environment variable, while every other protocol keeps requiring both.
func TestDemoProtocolCompleteness(t *testing.T) {
	demoProvider := ProviderConfig{
		ID:   "demo",
		Type: ProviderTypeGeneric,
		API:  API{Type: APITypeDemo},
		// No base URL, no credential reference: complete for this protocol.
		Access:   Access{Type: AccessTypeNone},
		ModelIDs: []string{"m"},
	}
	issues, complete := providerIssues(demoProvider, 1)
	if !complete {
		t.Fatalf("the demo provider must be complete, issues: %v", issueMessages(issues))
	}

	// The same shape with a vendor protocol stays incomplete: an absent base URL
	// and an absent credential are still errors there.
	vendor := demoProvider
	vendor.API.Type = APITypeOpenAIChatCompletions
	issues, complete = providerIssues(vendor, 1)
	if complete {
		t.Fatal("a vendor protocol without a base URL must stay incomplete")
	}
	requireIssue(t, issues, "api.baseUrl", "base URL is required")

	withKey := vendor
	withKey.API.BaseURL = "https://example.test/v1"
	withKey.Access = Access{Type: AccessTypeAPIKey}
	issues, complete = providerIssues(withKey, 1)
	if complete {
		t.Fatal("an api-key provider without a credential reference must stay incomplete")
	}
	requireIssue(t, issues, "access", "access environment variable name or secret reference is required")

	// The credential-free declaration is accepted by the loader too, and an
	// unknown access type is refused.
	if !validAccessType(AccessTypeNone) || !validAccessType(AccessTypeAPIKey) {
		t.Fatal("both documented access types must be accepted")
	}
	if validAccessType("oauth") {
		t.Fatal("an unknown access type must be refused")
	}
}

// TestDemoPerModelFamilyOverride pins the per-model family fact: a model's own
// providerType wins over the provider's, and a model that declares none inherits
// the provider's family exactly as before.
func TestDemoPerModelFamilyOverride(t *testing.T) {
	doc := testCatalogDoc([]any{
		testProviderWithType("multi", ProviderTypeLLM,
			APITypeOpenAIChatCompletions, "https://multi.example/v1", "MULTI_KEY", "multi-text", "multi-image"),
	})
	doc["providerModelRules"] = []any{
		map[string]any{
			"providerId": "multi",
			"modelId":    "multi-image",
			"config": map[string]any{
				"providerType": "image",
				"properties": map[string]any{
					"outputFormat": map[string]any{"image": true},
				},
			},
		},
	}
	resolution := resolveFrom(t, loadTestCatalog(t, doc), Overlay{})

	inherited := mustFindModel(t, resolution, "multi", "multi-text")
	if inherited.ProviderType != ProviderTypeLLM || inherited.SurfaceKind != SurfaceKindLLM {
		t.Fatalf("inheriting model = %s/%s, want llm/llm", inherited.ProviderType, inherited.SurfaceKind)
	}
	overridden := mustFindModel(t, resolution, "multi", "multi-image")
	if overridden.ProviderType != ProviderTypeImage || overridden.SurfaceKind != SurfaceKindImage {
		t.Fatalf("overriding model = %s/%s, want image/image", overridden.ProviderType, overridden.SurfaceKind)
	}
	if !overridden.Executable {
		t.Fatalf("an image model needs no text output to be executable, issues %v", issueMessages(overridden.Issues))
	}

	// The provider keeps its own declared family: a model-level override is not a
	// provider-level promotion.
	provider := mustFindProvider(t, resolution, "multi")
	if provider.ProviderType != ProviderTypeLLM || provider.SurfaceKind != SurfaceKindLLM {
		t.Fatalf("provider = %s/%s, want llm/llm", provider.ProviderType, provider.SurfaceKind)
	}
}

// TestTextOutputRequiredOnlyForConversationFamilies pins the executability rule
// the families encode: a text-less llm/realtime model cannot serve a turn, while
// an image/audio model legitimately produces another medium.
func TestTextOutputRequiredOnlyForConversationFamilies(t *testing.T) {
	cases := map[string]bool{
		ProviderTypeLLM:      true,
		ProviderTypeRealtime: true,
		ProviderTypeImage:    false,
		ProviderTypeVideo:    false,
		ProviderTypeTTS:      false,
		ProviderTypeSTT:      false,
		ProviderTypeDecision: false,
		ProviderTypeGeneric:  false,
	}
	for family, want := range cases {
		if got := requiresTextOutput(family); got != want {
			t.Fatalf("requiresTextOutput(%q) = %v, want %v", family, got, want)
		}
	}

	// A realtime model that declares audio only is still incomplete: a live voice
	// turn always yields a transcript.
	config := ModelConfig{
		Properties: &Properties{
			ContextWindow: 1000,
			OutputFormat:  MediaOut{Audio: true},
		},
		Options: &OptionSpecs{
			ReasoningLevel:  EnumSpec{Values: []string{"off"}, Map: map[string]map[string]any{"off": {}}},
			MaxOutputTokens: LimitSpec{Max: 100, Map: map[string]map[string]any{"": {}}},
		},
	}
	if _, complete := modelIssues(config, ProviderTypeRealtime, true, true); complete {
		t.Fatal("a realtime model without text output must stay incomplete")
	}
	if _, complete := modelIssues(config, ProviderTypeImage, true, true); !complete {
		t.Fatal("an image model without text output must be complete")
	}
}

// TestDemoCatalogIsStable pins the demo layer's shape against accidental drift:
// every model id is registered exactly once, and the document is a valid catalog
// on its own (it is loaded through the same validator as any other document).
func TestDemoCatalogIsStable(t *testing.T) {
	raw := demoDocumentBytes()
	if _, err := LoadCatalog(raw); err != nil {
		t.Fatalf("the demo layer must be a valid catalog document: %v", err)
	}
	var doc struct {
		Providers []struct {
			ID       string   `json:"id"`
			Type     string   `json:"type"`
			ModelIDs []string `json:"modelIds"`
		} `json:"providers"`
		ModelRules []any `json:"modelRules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode demo layer: %v", err)
	}
	if len(doc.Providers) != 1 || doc.Providers[0].ID != demo.ProviderID {
		t.Fatalf("the demo layer must declare exactly the demo provider: %+v", doc.Providers)
	}
	if got, want := doc.Providers[0].ModelIDs, demo.ModelIDs(); !equalStrings(got, want) {
		t.Fatalf("modelIds = %v, want %v", got, want)
	}
	// A wildcard rule would patch vendor models; the demo layer must declare none.
	if len(doc.ModelRules) != 0 {
		t.Fatalf("the demo layer must declare no modelRules, got %d", len(doc.ModelRules))
	}
}

// demoMarshal serializes a value for the diagnostics above.
func demoMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// sameJSON reports whether two values encode to the same JSON, so an assertion
// compares the wire facts rather than Go struct identity.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	return string(demoMarshal(t, a)) == string(demoMarshal(t, b))
}

// equalStrings compares two string slices element by element.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
