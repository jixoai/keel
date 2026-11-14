package modelcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/demo"
)

// The demo provider, as a catalog layer.
//
// # What it is
//
// A review deployment needs every surface to work end to end without a vendor
// credential. Doing that on the client was rejected (a client that knows about
// demo models cannot ship), so the demo lives here: one ordinary provider — id
// "demo" — whose models resolve, list and execute through exactly the same
// paths as a vendor's. The client sees a provider group and six models; nothing
// tells it those models are answered in-process.
//
// # Why a catalog layer instead of a providerConfig literal
//
// Everything downstream (the picker projection, capability gating, the request
// patch, executability) reads resolved *views*. Registering the demo provider by
// appending catalog JSON means those views are produced by the same resolver as
// every other provider's: there is no second path that could drift, and a
// settings document that references demo models resolves them identically.
//
// # Switch and precedence
//
// The provider is registered only when KEEL_DEMO_PROVIDER is affirmative (see
// demo.Enabled). Appended entries are merged before the tenant overlay, so:
//   - a production deployment (no variable) has no demo provider at all: the
//     catalog is byte-identical to EmbeddedBuiltin()'s;
//   - a tenant that declares its own "demo" provider patches the built-in one,
//     exactly as it would patch a built-in vendor — the tenant stays the last
//     word, and hiding it (visibility: hidden) removes it from every projection.
const (
	// demoCatalogSchemaVersion mirrors the embedded catalog's version; the demo
	// layer is a catalog document and obeys the same gate.
	demoCatalogSchemaVersion = 1

	// demoContextWindow is the demo models' declared context window. It is a
	// real number so the compaction path has something to work with; the demo
	// reply is a fixed sentence, so nothing actually fills it.
	demoContextWindow = 128000

	// demoMaxOutputTokens is the demo models' output cap. It must be positive for
	// the model to be executable.
	demoMaxOutputTokens = 8192
)

// demoCatalogJSON is the demo provider document: one provider with six models,
// one per family, plus the per-model rules that describe each family's
// capabilities. It is a compiled-in constant (not a file) so the demo cannot be
// enabled by shipping an extra asset.
//
// Every model declares the facts its surface reads:
//   - inputFormat/outputFormat, so the client's capability gate and surface
//     resolution work unchanged (an image model declares image output, the
//     realtime model declares audio in and text+audio out, and so on);
//   - reasoningLevel values with patches, maxOutputTokens with a default patch,
//     because an executable model must have both (the resolver requires them).
//
// providerType is declared per model rather than per provider: the demo
// provider serves six families, and a provider-level family would flatten them
// onto one surface.
func demoCatalogDocument() map[string]any {
	return map[string]any{
		"schemaVersion": demoCatalogSchemaVersion,
		"providers": []any{
			map[string]any{
				"id":   demo.ProviderID,
				"name": demo.ProviderName,
				// The provider-level family is generic: no single panel covers six
				// families, and generic is the honest "the models say" answer. Every
				// model below overrides it.
				"type": ProviderTypeGeneric,
				"api": map[string]any{
					// The demo protocol: in-process, no endpoint, no credential. The
					// empty baseUrl is deliberate and legal only for this protocol
					// (see providerIssues).
					"type":    APITypeDemo,
					"baseUrl": "",
				},
				"access": map[string]any{
					"type": AccessTypeNone,
				},
				"modelIds":   stringList(demo.ModelIDs()),
				"visibility": VisibilityVisible,
			},
		},
		// All demo facts travel in providerModelRules (providerId + modelId), never
		// in a wildcard modelRule. A `match: "*"` rule appended to the catalog
		// would patch every *vendor* model in the same document, and the demo
		// layer must never be able to change what a real provider resolves to.
		"modelRules": []any{},
		"modelApiRules": []any{
			// The demo protocol has no reasoning_effort field (nothing is dialed),
			// so the level patches are empty documents: the value selects cleanly
			// and carries no wire effect.
			map[string]any{
				"match":   "*",
				"apiType": APITypeDemo,
				"config": map[string]any{
					"options": map[string]any{
						"reasoningLevel": map[string]any{
							"map": map[string]any{
								"off":    map[string]any{},
								"low":    map[string]any{},
								"medium": map[string]any{},
								"high":   map[string]any{},
							},
						},
					},
				},
			},
		},
		"siteRules":          []any{},
		"providerModelRules": demoProviderModelRules(),
	}
}

// demoOptionSpecs is the option vocabulary every demo model declares.
func demoOptionSpecs() map[string]any {
	return map[string]any{
		"reasoningLevel": map[string]any{
			"values": stringList([]string{"off", "low", "medium", "high"}),
			"map": map[string]any{
				"off":    map[string]any{},
				"low":    map[string]any{},
				"medium": map[string]any{},
				"high":   map[string]any{},
			},
		},
		"maxOutputTokens": map[string]any{
			"max": demoMaxOutputTokens,
			"map": map[string]any{"": map[string]any{}, "*": map[string]any{}},
		},
	}
}

// demoProviderModelRules declares each demo model's complete fact sheet: family,
// media formats, capability flags and options. Everything a model needs lives in
// its own (providerId, modelId) rule, so no demo rule can reach a vendor model —
// an appended `match: "*"` rule would patch every provider in the document.
// The family values come from demo.Family so registration, the media gateway and
// the realtime provider cannot disagree about which model is which.
func demoProviderModelRules() []any {
	type modelFacts struct {
		modelID string
		family  string
		props   map[string]any
		tools   bool
	}
	textIn := map[string]any{"text": true, "image": false, "video": false, "audio": false, "pdf": false}
	audioIn := map[string]any{"text": true, "image": false, "video": false, "audio": true, "pdf": false}
	// The output formats spell out every key on purpose. The vendor catalog's
	// baseline rule is `match: "*"` with text output enabled, and merge-patch
	// semantics keep an absent key: declaring only {"image": true} would resolve
	// as text+image, which is a fact about no model here.
	textOut := map[string]any{"text": true, "image": false, "video": false, "audio": false}
	imageOut := map[string]any{"text": false, "image": true, "video": false, "audio": false}
	voiceOut := map[string]any{"text": true, "image": false, "video": false, "audio": true}
	audioOut := map[string]any{"text": false, "image": false, "video": false, "audio": true}
	facts := []modelFacts{
		{
			modelID: demo.ModelLLM,
			family:  demo.FamilyLLM,
			props: map[string]any{
				"inputFormat":  textIn,
				"outputFormat": textOut,
			},
			// The LLM model is the one that runs a real agent turn, so it declares the
			// tool capability; the media families never receive tools.
			tools: true,
		},
		{
			modelID: demo.ModelImage,
			family:  demo.FamilyImage,
			props: map[string]any{
				"inputFormat":  textIn,
				"outputFormat": imageOut,
			},
		},
		{
			modelID: demo.ModelDecision,
			family:  demo.FamilyDecision,
			props: map[string]any{
				"inputFormat":  textIn,
				"outputFormat": textOut,
			},
		},
		{
			modelID: demo.ModelRealtime,
			family:  demo.FamilyRealtime,
			props: map[string]any{
				// A live voice channel carries audio in both directions and streams a
				// transcript alongside it.
				"inputFormat":  audioIn,
				"outputFormat": voiceOut,
			},
		},
		{
			modelID: demo.ModelTTS,
			family:  demo.FamilyTTS,
			props: map[string]any{
				"inputFormat":  textIn,
				"outputFormat": audioOut,
			},
		},
		{
			modelID: demo.ModelSTT,
			family:  demo.FamilySTT,
			props: map[string]any{
				"inputFormat":  audioIn,
				"outputFormat": textOut,
			},
		},
	}
	rules := make([]any, 0, len(facts))
	for _, fact := range facts {
		properties := map[string]any{
			"contextWindow":            demoContextWindow,
			"supportsJsonSchemaOutput": fact.tools,
			"supportsNativeWebSearch":  false,
		}
		for key, value := range fact.props {
			properties[key] = value
		}
		// Spell the flag out for every model: the value is inherited from the
		// document's baseline otherwise, and "supportsToolCall: true" on an image
		// model would be a capability nothing routes.
		properties["supportsToolCall"] = fact.tools
		rules = append(rules, map[string]any{
			"providerId": demo.ProviderID,
			"modelId":    fact.modelID,
			"config": map[string]any{
				"enabled":      true,
				"providerType": fact.family,
				"properties":   properties,
				"options":      demoOptionSpecs(),
			},
		})
	}
	return rules
}

// DemoCatalog returns the built-in catalog: the compiled-in vendor catalog with
// the demo provider layer appended when it is enabled.
//
// This is the only place the switch is read. Callers (the service assembly, the
// tests) use it instead of EmbeddedBuiltin() so there is a single answer to
// "is the demo provider part of this process". A disabled process gets the
// embedded catalog itself (same pointer), so nothing about it can change.
func DemoCatalogFromEnv() *Catalog {
	return DemoCatalog(demo.EnabledFromGetenv(os.Getenv))
}

// DemoCatalog builds the catalog for an explicit switch. Tests use it to avoid
// touching the process environment.
func DemoCatalog(enabled bool) *Catalog {
	base := EmbeddedBuiltin()
	if !enabled {
		return base
	}
	demoOnce.Do(func() {
		merged, err := mergeCatalogDocument(base.doc, demoCatalogDocument())
		if err != nil {
			// The demo document is a compiled-in constant; an error here is a
			// programming defect, and silently running without the demo would hide
			// it.
			panic("modelcfg: build demo catalog: " + err.Error())
		}
		demoCat, demoErr = LoadCatalog(merged)
	})
	if demoErr != nil {
		panic("modelcfg: load demo catalog: " + demoErr.Error())
	}
	return demoCat
}

// mergeCatalogDocument appends the demo layer to the base catalog document.
// Providers are unioned by id (the demo id never collides with a vendor's, but a
// future catalog that declares it must not be silently duplicated), and the
// rule sets are appended in order so the demo rules apply after the vendor
// baseline exactly as a later tenant rule would.
func mergeCatalogDocument(base, layer map[string]any) ([]byte, error) {
	merged := cloneJSONMap(base)
	if merged == nil {
		merged = map[string]any{}
	}
	if baseProviders, ok := merged["providers"].([]any); ok {
		merged["providers"] = append(append([]any{}, baseProviders...), jsonListOf(layer["providers"])...)
	} else {
		merged["providers"] = jsonListOf(layer["providers"])
	}
	for _, key := range []string{"modelRules", "modelApiRules", "siteRules", "providerModelRules"} {
		existing, _ := merged[key].([]any)
		merged[key] = append(append([]any{}, existing...), jsonListOf(layer[key])...)
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged catalog: %w", err)
	}
	return raw, nil
}

// stringList converts a []string into the []any shape a decoded JSON document
// uses (the demo document is built as decoded-JSON values so it passes through
// the same validation path as any other document).
func stringList(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// jsonListOf returns the layer's list, treating nil as empty so an omitted key
// contributes nothing.
func jsonListOf(value any) []any {
	list, _ := value.([]any)
	return list
}

// DemoProviderID is the demo provider's identity, re-exported here so callers
// that already depend on modelcfg do not have to import internal/demo for the
// one string.
const DemoProviderID = demo.ProviderID

// IsDemoProvider reports whether a provider id names the demo provider.
func IsDemoProvider(providerID string) bool {
	return strings.EqualFold(strings.TrimSpace(providerID), demo.ProviderID)
}

// IsDemoProtocol reports whether an api.type is the in-process demo protocol.
// The runtime's driver factory branches on this to build the in-process driver
// instead of an HTTP client.
func IsDemoProtocol(apiType string) bool {
	return strings.EqualFold(strings.TrimSpace(apiType), APITypeDemo)
}

// demoDocumentBytes exposes the demo layer as JSON for diagnostics and tests
// (the settings view can print what a review deployment adds).
func demoDocumentBytes() []byte {
	raw, err := json.Marshal(demoCatalogDocument())
	if err != nil {
		panic("modelcfg: marshal demo document: " + err.Error())
	}
	return raw
}

// demoDocumentEquals reports whether two demo layers are byte-identical. Tests
// use it to pin the layer's stability (a change is a deliberate change).
func demoDocumentEquals(other []byte) bool {
	return bytes.Equal(bytes.TrimSpace(demoDocumentBytes()), bytes.TrimSpace(other))
}

var (
	demoOnce sync.Once
	demoCat  *Catalog
	demoErr  error
)
