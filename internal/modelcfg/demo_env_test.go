package modelcfg

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jixoai/keel/internal/demo"
)

// These tests pin the environment switch from the outside: they drive the same
// entry point the hosting process uses (DemoCatalogFromEnv) and read the picker
// projection the /v1/models endpoint serves (Resolution.Models), so they verify
// the acceptance criteria of the change rather than an internal helper.
//
// The two halves are deliberately paired:
//
//   - unset (production): the catalog is the embedded catalog object itself, its
//     document bytes are identical, and the picker slice has no demo model;
//   - KEEL_DEMO_PROVIDER=1 (review): demo-llm appears in that same slice with
//     providerType "llm" and selectable true.
//
// A test that only checked the enabled side would not prove the production
// deployment is untouched.

// pickerModel looks up one model in the flat selectable list (the projection
// /v1/models serves). ok is false when the model is absent from the projection,
// which is different from "present but not selectable".
func pickerModel(resolution Resolution, providerID, modelID string) (ModelView, bool) {
	for _, model := range resolution.Models() {
		if model.ProviderID == providerID && model.ModelID == modelID {
			return model, true
		}
	}
	return ModelView{}, false
}

// TestDemoSwitchUnsetLeavesTheCatalogUntouched is the "zero impact" gate at the
// environment boundary: with KEEL_DEMO_PROVIDER genuinely unset, the catalog the
// process builds is the embedded one, its document is byte-identical, the
// /v1/models projection is the same slice, and it contains no demo model.
func TestDemoSwitchUnsetLeavesTheCatalogUntouched(t *testing.T) {
	// t.Setenv registers the restore; Unsetenv then makes the variable genuinely
	// absent, which is the production state (not merely "set to empty"). The empty
	// value is covered separately below because the switch reads the value.
	t.Setenv(demo.Env, "")
	if err := os.Unsetenv(demo.Env); err != nil {
		t.Fatalf("unset %s: %v", demo.Env, err)
	}
	if _, present := os.LookupEnv(demo.Env); present {
		t.Fatalf("%s must be genuinely unset for this assertion", demo.Env)
	}

	withEnv := DemoCatalogFromEnv()
	if withEnv != EmbeddedBuiltin() {
		t.Fatal("an unset switch must return the embedded catalog itself, not a copy")
	}

	// Byte-level equivalence of the catalog document: the same resolution, the same
	// bytes. This is what "the production deployment cannot differ" means.
	gotDoc, err := json.Marshal(withEnv.doc)
	if err != nil {
		t.Fatalf("marshal env catalog: %v", err)
	}
	wantDoc, err := json.Marshal(EmbeddedBuiltin().doc)
	if err != nil {
		t.Fatalf("marshal embedded catalog: %v", err)
	}
	if string(gotDoc) != string(wantDoc) {
		t.Fatalf("the unset-switch catalog document differs from the embedded one:\ngot  %s\nwant %s",
			gotDoc, wantDoc)
	}

	// The /v1/models projection itself: the same model slice as the embedded
	// catalog, with no demo entry anywhere in it.
	resolution := resolveFrom(t, withEnv, Overlay{})
	embedded := resolveFrom(t, EmbeddedBuiltin(), Overlay{})
	gotModels, err := json.Marshal(resolution.Models())
	if err != nil {
		t.Fatalf("marshal picker models: %v", err)
	}
	wantModels, err := json.Marshal(embedded.Models())
	if err != nil {
		t.Fatalf("marshal embedded picker models: %v", err)
	}
	if string(gotModels) != string(wantModels) {
		t.Fatalf("the unset-switch picker slice differs from the embedded one:\ngot  %s\nwant %s",
			gotModels, wantModels)
	}

	for _, model := range resolution.Models() {
		if demo.FamilyInModelIDs(model.ModelID) || IsDemoProvider(model.ProviderID) {
			t.Fatalf("an unset switch must contribute no demo model, found %s/%s",
				model.ProviderID, model.ModelID)
		}
	}
	if _, ok := pickerModel(resolution, demo.ProviderID, demo.ModelLLM); ok {
		t.Fatal("/v1/models must not offer demo-llm in a production deployment")
	}
	for _, provider := range resolution.Settings() {
		if IsDemoProvider(provider.ProviderID) {
			t.Fatalf("an unset switch must resolve no demo provider: %+v", provider)
		}
	}
}

// TestDemoSwitchEnabledListsDemoLLM is the acceptance gate for a review
// deployment: with KEEL_DEMO_PROVIDER=1 the picker projection carries demo-llm as
// an ordinary llm model that a client may select.
func TestDemoSwitchEnabledListsDemoLLM(t *testing.T) {
	t.Setenv(demo.Env, "1")

	catalog := DemoCatalogFromEnv()
	if catalog == EmbeddedBuiltin() {
		t.Fatal("an enabled switch must not return the embedded catalog itself")
	}

	resolution := resolveFrom(t, catalog, Overlay{})

	model, ok := pickerModel(resolution, demo.ProviderID, demo.ModelLLM)
	if !ok {
		t.Fatalf("/v1/models must offer %s/%s when the demo provider is enabled",
			demo.ProviderID, demo.ModelLLM)
	}
	if model.ProviderType != ProviderTypeLLM {
		t.Fatalf("%s providerType = %q, want %q", demo.ModelLLM, model.ProviderType, ProviderTypeLLM)
	}
	if model.SurfaceKind != SurfaceKindLLM {
		t.Fatalf("%s surfaceKind = %q, want %q", demo.ModelLLM, model.SurfaceKind, SurfaceKindLLM)
	}
	if !model.Selectable {
		t.Fatalf("%s must be selectable, issues: %v", demo.ModelLLM, issueMessages(model.Issues))
	}
	if !model.Executable {
		t.Fatalf("%s must be executable, issues: %v", demo.ModelLLM, issueMessages(model.Issues))
	}
	if model.API.Type != APITypeDemo {
		t.Fatalf("%s api.type = %q, want %q", demo.ModelLLM, model.API.Type, APITypeDemo)
	}

	// The demo provider must read as an ordinary picker group: visible, complete
	// and selectable, with no issue explaining it away.
	provider := mustFindProvider(t, resolution, demo.ProviderID)
	if provider.Visibility != VisibilityVisible {
		t.Fatalf("demo provider visibility = %q, want %q", provider.Visibility, VisibilityVisible)
	}
	if !provider.Executable || !provider.Selectable {
		t.Fatalf("demo provider must be executable and selectable, issues: %v",
			issueMessages(provider.Issues))
	}
	if provider.Name != demo.ProviderName {
		t.Fatalf("demo provider name = %q, want %q", provider.Name, demo.ProviderName)
	}
}

// TestDemoSwitchKeepsVendorModelsIdentical pins the blast radius at the environment
// boundary: enabling the demo provider adds one provider and changes no fact of
// any vendor model in the picker projection.
func TestDemoSwitchKeepsVendorModelsIdentical(t *testing.T) {
	t.Setenv(demo.Env, "")
	without := resolveFrom(t, DemoCatalogFromEnv(), Overlay{})

	t.Setenv(demo.Env, "1")
	with := resolveFrom(t, DemoCatalogFromEnv(), Overlay{})

	vendorBefore := vendorPickerModels(without)
	vendorAfter := vendorPickerModels(with)
	if len(vendorBefore) != len(vendorAfter) {
		t.Fatalf("vendor picker models = %d with the demo on, want %d",
			len(vendorAfter), len(vendorBefore))
	}
	for i := range vendorBefore {
		before, err := json.Marshal(vendorBefore[i])
		if err != nil {
			t.Fatalf("marshal vendor model: %v", err)
		}
		after, err := json.Marshal(vendorAfter[i])
		if err != nil {
			t.Fatalf("marshal vendor model: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("enabling the demo provider changed %s/%s:\nwithout %s\nwith    %s",
				vendorBefore[i].ProviderID, vendorBefore[i].ModelID, before, after)
		}
	}
}

// vendorPickerModels returns the selectable models that are not the demo
// provider's, in projection order.
func vendorPickerModels(resolution Resolution) []ModelView {
	models := resolution.Models()
	out := make([]ModelView, 0, len(models))
	for _, model := range models {
		if IsDemoProvider(model.ProviderID) {
			continue
		}
		out = append(out, model)
	}
	return out
}
