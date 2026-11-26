// Extensibility documents: the machine-readable half of a plugin's extension
// surface (extensibility.json, contract keel.plugin-extensibility/1) and the
// human-readable half (EXTENSIBILITY.md). Creation mode aggregates these files
// from every installed plugin, so the format is frozen here and validated with
// the same field-level errors as plugin.yaml.
//
// Spec: openspec/changes/keel-harness-v2/specs/plugin-ui-extension/spec.md
// (Requirement: 创造模式的聚合协议) and specs/plugin-architecture/spec.md
// (Requirement: 插件的扩展面声明). The prose contract is
// docs/PLUGIN-EXTENSIBILITY.md.
package pluginpkg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/jixoai/keel/internal/modelprovider"
)

const (
	// ExtensibilityFileName is the machine-readable document inside a package.
	ExtensibilityFileName = "extensibility.json"
	// ExtensibilityDocFileName is the human-readable document inside a package.
	ExtensibilityDocFileName = "EXTENSIBILITY.md"
	// ExtensibilitySchema is the only legal value of the document's `schema`.
	ExtensibilitySchema = "keel.plugin-extensibility/1"
)

// ExtConfig is one configuration key a plugin accepts. Keys are environment
// style (`KEEL_WEB_SEARCH_TIMEOUT_MS`) so the same name is usable in plugin.yaml,
// a sidecar env whitelist and the settings UI.
type ExtConfig struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Description string `json:"description"`
}

// ExtHook is one hook event a plugin subscribes to.
type ExtHook struct {
	Event       string `json:"event"`
	Description string `json:"description"`
}

// ExtProvider is one replaceable provider port this plugin serves (for example
// `web.search` or `image.generate`).
type ExtProvider struct {
	Port        string `json:"port"`
	Description string `json:"description"`
}

// ExtExample is one usage example creation mode can show an agent.
type ExtExample struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Snippet     string `json:"snippet,omitempty"`
}

// ExtLimit is one bound the plugin enforces (timeout, result count, memory).
type ExtLimit struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description"`
}

// ExtensibilityDoc is the parsed extensibility.json. Every array is required
// (an empty list is `[]`, not an omitted key) so the document is complete by
// construction and the aggregator never has to guess.
type ExtensibilityDoc struct {
	Schema        string         `json:"schema"`
	Plugin        string         `json:"plugin"`
	Version       string         `json:"version"`
	Summary       string         `json:"summary"`
	Slots         []SlotDecl     `json:"slots"`
	Contributes   []Contribution `json:"contributes"`
	Configuration []ExtConfig    `json:"configuration"`
	Hooks         []ExtHook      `json:"hooks"`
	Providers     []ExtProvider  `json:"providers"`
	Examples      []ExtExample   `json:"examples"`
	Limits        []ExtLimit     `json:"limits"`
}

// extConfigTypes is the closed set of configuration value types.
var extConfigTypes = map[string]bool{
	"string": true, "integer": true, "number": true, "boolean": true,
	"string_list": true, "object": true,
}

// extConfigKeyPattern is the environment-style configuration key grammar.
var extConfigKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// extLimitNamePattern is the grammar of a limit name (`timeout`, `max_results`).
var extLimitNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)

// ParseExtensibility decodes extensibility.json bytes with strict unknown-field
// rejection and no trailing document.
func ParseExtensibility(raw []byte) (*ExtensibilityDoc, error) {
	var doc ExtensibilityDoc
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&doc); err != nil {
		return nil, &ValidationError{Errors: []FieldError{{Field: "", Message: ExtensibilityFileName + ": " + err.Error()}}}
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, &ValidationError{Errors: []FieldError{{Field: "", Message: ExtensibilityFileName + " carries more than one JSON document"}}}
	} else if !errors.Is(err, io.EOF) {
		// A non-EOF error after the first value is a malformed trailing token.
		return nil, &ValidationError{Errors: []FieldError{{Field: "", Message: ExtensibilityFileName + ": " + err.Error()}}}
	}
	return &doc, nil
}

// LoadExtensibility reads `<dir>/extensibility.json` and validates it. A package
// without the file returns (nil, nil): the document is optional for third-party
// plugins and mandatory only at the official release gate.
func LoadExtensibility(dir string) (*ExtensibilityDoc, []FieldError, error) {
	path := filepath.Join(dir, ExtensibilityFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	doc, err := ParseExtensibility(raw)
	if err != nil {
		return nil, FieldErrors(err), err
	}
	return doc, FieldErrors(doc.Validate()), nil
}

// Validate checks the document on its own: schema identity, required arrays,
// namespaced slots, contribution shape and the closed configuration type set.
func (d *ExtensibilityDoc) Validate() error {
	errs := &fieldSet{}
	if d.Schema != ExtensibilitySchema {
		errs.add("schema", "schema must be %q, got %q", ExtensibilitySchema, d.Schema)
	}
	if !idPattern.MatchString(d.Plugin) {
		errs.add("plugin", "plugin id %q must match ^[a-z][a-z0-9._-]{1,63}$", d.Plugin)
	}
	if _, err := ParseSemVer(d.Version); err != nil {
		errs.add("version", "%v", err)
	}
	if strings.TrimSpace(d.Summary) == "" {
		errs.add("summary", "summary is required")
	} else if len([]rune(d.Summary)) > 1024 {
		errs.add("summary", "summary must be at most 1024 characters")
	}
	requireArray := func(field string, present bool) {
		if !present {
			errs.add(field, "%s is required (use [] when empty)", field)
		}
	}
	requireArray("slots", d.Slots != nil)
	requireArray("contributes", d.Contributes != nil)
	requireArray("configuration", d.Configuration != nil)
	requireArray("hooks", d.Hooks != nil)
	requireArray("providers", d.Providers != nil)
	requireArray("examples", d.Examples != nil)
	requireArray("limits", d.Limits != nil)

	validateSlotList(errs, "slots", d.Plugin, d.Slots)
	selfSlots := map[string]SlotDecl{}
	for _, slot := range d.Slots {
		selfSlots[slot.ID] = slot
	}
	validateContributionList(errs, "contributes", selfSlots, Environment{}, d.Contributes, true)

	seenConfig := map[string]bool{}
	for index, config := range d.Configuration {
		prefix := fmt.Sprintf("configuration[%d]", index)
		switch {
		case strings.TrimSpace(config.Key) == "":
			errs.add(prefix+".key", "key is required")
		case !extConfigKeyPattern.MatchString(config.Key):
			errs.add(prefix+".key", "key %q must match %s", config.Key, extConfigKeyPattern.String())
		case seenConfig[config.Key]:
			errs.add(prefix+".key", "key %q is declared twice", config.Key)
		}
		seenConfig[config.Key] = true
		if !extConfigTypes[config.Type] {
			errs.add(prefix+".type", "type %q is not one of string|integer|number|boolean|string_list|object", config.Type)
		}
		if strings.TrimSpace(config.Description) == "" {
			errs.add(prefix+".description", "description is required")
		}
	}
	seenHooks := map[string]bool{}
	for index, hook := range d.Hooks {
		prefix := fmt.Sprintf("hooks[%d]", index)
		if !namePattern.MatchString(hook.Event) {
			errs.add(prefix+".event", "hook event %q must match %s", hook.Event, namePattern.String())
		} else if seenHooks[hook.Event] {
			errs.add(prefix+".event", "hook event %q is declared twice", hook.Event)
		}
		seenHooks[hook.Event] = true
		if strings.TrimSpace(hook.Description) == "" {
			errs.add(prefix+".description", "description is required")
		}
	}
	seenPorts := map[string]bool{}
	for index, provider := range d.Providers {
		prefix := fmt.Sprintf("providers[%d]", index)
		if !modelprovider.ValidFeaturePort(provider.Port) {
			errs.add(prefix+".port", "provider port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'", provider.Port)
		} else if seenPorts[provider.Port] {
			errs.add(prefix+".port", "provider port %q is declared twice", provider.Port)
		}
		seenPorts[provider.Port] = true
		if strings.TrimSpace(provider.Description) == "" {
			errs.add(prefix+".description", "description is required")
		}
	}
	for index, example := range d.Examples {
		prefix := fmt.Sprintf("examples[%d]", index)
		if strings.TrimSpace(example.Title) == "" {
			errs.add(prefix+".title", "title is required")
		}
		if strings.TrimSpace(example.Description) == "" {
			errs.add(prefix+".description", "description is required")
		}
	}
	seenLimits := map[string]bool{}
	for index, limit := range d.Limits {
		prefix := fmt.Sprintf("limits[%d]", index)
		switch {
		case strings.TrimSpace(limit.Name) == "":
			errs.add(prefix+".name", "name is required")
		case !extLimitNamePattern.MatchString(limit.Name):
			errs.add(prefix+".name", "name %q must match %s", limit.Name, extLimitNamePattern.String())
		case seenLimits[limit.Name]:
			errs.add(prefix+".name", "limit %q is declared twice", limit.Name)
		}
		seenLimits[limit.Name] = true
		if strings.TrimSpace(limit.Value) == "" {
			errs.add(prefix+".value", "value is required")
		}
		if strings.TrimSpace(limit.Description) == "" {
			errs.add(prefix+".description", "description is required")
		}
	}
	return errs.err()
}

// ValidateAgainstManifest pins the two spellings together: the document must
// describe the same plugin version and the same slots/contributions the
// manifest declares. An official package whose JSON drifted from plugin.yaml is
// refused at the release gate.
func (d *ExtensibilityDoc) ValidateAgainstManifest(m *Manifest) error {
	errs := &fieldSet{}
	if m == nil {
		errs.add("plugin", "a manifest is required to validate an extensibility document")
		return errs.err()
	}
	if d.Plugin != m.ID {
		errs.add("plugin", "document names plugin %q but the manifest is %q", d.Plugin, m.ID)
	}
	if d.Version != m.Version {
		errs.add("version", "document names version %q but the manifest is %q", d.Version, m.Version)
	}
	if !sameJSONValue(d.Slots, m.Provides.Slots) {
		errs.add("slots", "document slots do not match the manifest's provides.slots")
	}
	// `part`/`mime` are extensibility.json claims added by change
	// keel-plugin-attachment; the manifest contribution surface is the base
	// (slot/form/entry/payload), and the document may narrow it with claims.
	if !sameJSONValue(contributionSurface(d.Contributes), contributionSurface(m.Contributes)) {
		errs.add("contributes", "document contributions do not match the manifest's contributes (attachment claims are declared in the document)")
	}
	return errs.err()
}

// contributionSurface strips the claim fields so the manifest and the document
// can be compared on the declaration they must share.
func contributionSurface(contributions []Contribution) []Contribution {
	out := make([]Contribution, len(contributions))
	for index, contribution := range contributions {
		contribution.Part = ""
		contribution.Mime = nil
		out[index] = contribution
	}
	if out == nil {
		return []Contribution{}
	}
	return out
}

// sameJSONValue compares two decoded values by their canonical JSON encoding.
// It tolerates the yaml-vs-json number spelling difference (both sides are
// re-encoded before comparison) and treats a nil slice/map as its empty
// spelling, because "no slots" is the same declaration in both files.
func sameJSONValue(a, b any) bool {
	left, err := json.Marshal(normalizeEmpty(a))
	if err != nil {
		return false
	}
	right, err := json.Marshal(normalizeEmpty(b))
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

func normalizeEmpty(value any) any {
	if value == nil {
		return nil
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice:
		if rv.IsNil() {
			return []any{}
		}
	case reflect.Map:
		if rv.IsNil() {
			return map[string]any{}
		}
	}
	return value
}
