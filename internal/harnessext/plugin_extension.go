package harnessext

// Plugin extension declarations (change keel-harness-v2, W12): the tolerant
// reader and validator for `layer` / `provides.slots` / `contributes` /
// `extensibility.json`.
//
// The canonical manifest fields are being frozen by W0.5 in `pluginpkg`. This
// file deliberately does not edit that package: it reads the same shape from
// three places (the parsed manifest when the fields exist, the raw
// `plugin.yaml` so a package written before the Go structs catch up still
// declares its surface, and `extensibility.json`), then validates the result
// with the closed sets the UI contract fixes. When W0.5 lands the fields in
// `pluginpkg`, the first source starts carrying them and nothing here moves.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jixoai/keel/internal/pluginpkg"
)

// ExtensionForm is one of the four contribution forms, closed.
type ExtensionForm string

const (
	FormChunk   ExtensionForm = "chunk"
	FormPackage ExtensionForm = "package"
	FormIFrame  ExtensionForm = "iframe"
	FormData    ExtensionForm = "data"
)

// ExtensionArity is how many contributions a slot accepts.
type ExtensionArity string

const (
	AritySingle  ExtensionArity = "single"
	ArityMulti   ExtensionArity = "multi"
	ArityOrdered ExtensionArity = "ordered"
)

// ExtensionTarget is a target a slot/contribution names.
type ExtensionTarget string

const (
	TargetWeb    ExtensionTarget = "web"
	TargetNative ExtensionTarget = "native"
	TargetTUI    ExtensionTarget = "tui"
)

// ExtensionCapability is one bridge verb a contribution may be granted.
type ExtensionCapability string

const (
	CapState     ExtensionCapability = "state"
	CapCall      ExtensionCapability = "call"
	CapApprove   ExtensionCapability = "approve"
	CapSubscribe ExtensionCapability = "subscribe"
)

// ExtensionSlot is one slot a plugin declares (`provides.slots[]`).
type ExtensionSlot struct {
	ID        string          `json:"id" yaml:"id"`
	Forms     []string        `json:"forms" yaml:"forms"`
	Arity     string          `json:"arity,omitempty" yaml:"arity,omitempty"`
	Targets   []string        `json:"targets" yaml:"targets"`
	Schema    json.RawMessage `json:"schema,omitempty" yaml:"-"`
	Lifecycle []string        `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
	// Plugin is the declaring plugin id, filled by the reader.
	Plugin string `json:"plugin,omitempty" yaml:"-"`
}

// ExtensionContribution is one contribution a plugin makes.
type ExtensionContribution struct {
	ID           string          `json:"id,omitempty" yaml:"id,omitempty"`
	Slot         string          `json:"slot" yaml:"slot"`
	Form         string          `json:"form" yaml:"form"`
	Entry        string          `json:"entry,omitempty" yaml:"entry,omitempty"`
	Title        string          `json:"title,omitempty" yaml:"title,omitempty"`
	Description  string          `json:"description,omitempty" yaml:"description,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty" yaml:"-"`
	Capabilities []string        `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Targets      []string        `json:"targets,omitempty" yaml:"targets,omitempty"`
	Tool         string          `json:"tool,omitempty" yaml:"tool,omitempty"`
	Part         string          `json:"part,omitempty" yaml:"part,omitempty"`
	// Plugin is the contributing plugin id, filled by the reader.
	Plugin string `json:"plugin,omitempty" yaml:"-"`
}

// ExtensionDoc is the machine-readable extensibility document's presence.
type ExtensionDoc struct {
	Usable bool   `json:"usable"`
	Doc    string `json:"doc,omitempty"`
	Schema string `json:"schema,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ExtensionDeclaration is one plugin's whole extension surface.
type ExtensionDeclaration struct {
	Layer       string                  `json:"layer,omitempty"`
	Slots       []ExtensionSlot         `json:"slots,omitempty"`
	Contributes []ExtensionContribution `json:"contributes,omitempty"`
	Doc         *ExtensionDoc           `json:"extensibility,omitempty"`
	Problems    []pluginpkg.FieldError  `json:"problems,omitempty"`
	Warnings    []string                `json:"warnings,omitempty"`
}

// Empty reports whether the plugin declares no extension surface at all.
func (d ExtensionDeclaration) Empty() bool {
	return len(d.Slots) == 0 && len(d.Contributes) == 0
}

// extensionManifest is the tolerant shape read from `plugin.yaml`. It is not
// pluginpkg.Manifest: a field W0.5 has not added to the strict struct still
// parses here, so an installed package's declaration is never invisible.
type extensionManifest struct {
	ID            string                  `yaml:"id"`
	Version       string                  `yaml:"version"`
	Kind          string                  `yaml:"kind"`
	Layer         string                  `yaml:"layer"`
	Provides      extensionProvides       `yaml:"provides"`
	Contributes   []extensionContribution `yaml:"contributes"`
	Extensibility any                     `yaml:"extensibility"`
}

type extensionProvides struct {
	Slots []extensionSlot `yaml:"slots"`
}

type extensionSlot struct {
	ID        string   `yaml:"id"`
	Forms     []string `yaml:"forms"`
	Arity     string   `yaml:"arity"`
	Targets   []string `yaml:"targets"`
	Schema    any      `yaml:"schema"`
	Lifecycle []string `yaml:"lifecycle"`
}

type extensionContribution struct {
	ID           string   `yaml:"id"`
	Slot         string   `yaml:"slot"`
	Form         string   `yaml:"form"`
	Entry        string   `yaml:"entry"`
	Title        string   `yaml:"title"`
	Description  string   `yaml:"description"`
	Payload      any      `yaml:"payload"`
	Capabilities []string `yaml:"capabilities"`
	Targets      []string `yaml:"targets"`
	Tool         string   `yaml:"tool"`
	Part         string   `yaml:"part"`
}

type extensionFile struct {
	Layer         string                  `json:"layer"`
	Slots         []extensionSlot         `json:"slots"`
	Provides      extensionProvides       `json:"provides"`
	Contributes   []extensionContribution `json:"contributes"`
	Extensibility *extensibilityPointer   `json:"extensibility"`
}

type extensibilityPointer struct {
	Doc    string `json:"doc" yaml:"doc"`
	Schema string `json:"schema" yaml:"schema"`
}

// ExtensionFileName is the machine-readable document every official plugin
// ships (`EXTENSIBILITY.md` is its human counterpart).
const ExtensionFileName = "extensibility.json"

const extensionFileMaxBytes = 256 * 1024

var (
	extensionSlotPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}\.[a-z][a-z0-9._-]{0,127}$`)
	extensionFormSet     = map[string]bool{"chunk": true, "package": true, "iframe": true, "data": true}
	extensionAritySet    = map[string]bool{"single": true, "multi": true, "ordered": true}
	extensionTargetSet   = map[string]bool{"web": true, "native": true, "tui": true}
	extensionCapSet      = map[string]bool{"state": true, "call": true, "approve": true, "subscribe": true}
	extensionLayerSet    = map[string]bool{"kernel": true, "harness": true, "provider": true, "ui": true, "service": true}
)

// DecodeExtension reads one package's extension declaration. It never fails:
// a broken document is reported in `Problems` (and the plugin's UI surface is
// not loaded by the client), never as a manager error.
func DecodeExtension(dir string, manifest *pluginpkg.Manifest) ExtensionDeclaration {
	decl := ExtensionDeclaration{}
	pluginID := ""
	if manifest != nil {
		pluginID = manifest.ID
	}

	// 1. The parsed manifest, when pluginpkg has the fields (a JSON round-trip
	// reads them without this package linking to a struct that may not exist
	// yet).
	if manifest != nil {
		if raw, err := json.Marshal(manifest); err == nil {
			var doc extensionFile
			if err := json.Unmarshal(raw, &doc); err == nil {
				mergeExtensionFile(&decl, doc)
			}
		}
	}

	// 2. The raw manifest, so a package written against the frozen YAML shape
	// declares its surface even before the Go structs carry it.
	if dir != "" {
		if raw, err := os.ReadFile(filepath.Join(dir, pluginpkg.PluginFileName)); err == nil {
			var doc extensionManifest
			if err := yaml.Unmarshal(raw, &doc); err == nil {
				mergeExtensionManifest(&decl, doc)
				if pluginID == "" {
					pluginID = doc.ID
				}
			}
		}
	}

	// 3. `extensibility.json`: the machine-readable document the creation mode
	// aggregates. It may repeat the declaration; the file wins for what it
	// carries.
	decl.Doc = &ExtensionDoc{Usable: false, Reason: "no extensibility.json in the package"}
	if dir != "" {
		path := filepath.Join(dir, ExtensionFileName)
		raw, err := readAtMost(path, extensionFileMaxBytes)
		if err == nil {
			var doc extensionFile
			if jsonErr := json.Unmarshal(raw, &doc); jsonErr != nil {
				decl.Doc = &ExtensionDoc{Usable: false, Schema: ExtensionFileName, Reason: "extensibility.json is not valid JSON: " + jsonErr.Error()}
				decl.Problems = append(decl.Problems, pluginpkg.FieldError{Field: "extensibility.json", Message: "the machine-readable extension document is not valid JSON"})
			} else {
				mergeExtensionFile(&decl, doc)
				decl.Doc.Usable = true
				decl.Doc.Schema = ExtensionFileName
				decl.Doc.Reason = ""
				if doc.Extensibility != nil && doc.Extensibility.Doc != "" {
					decl.Doc.Doc = doc.Extensibility.Doc
				}
			}
		}
	}

	// Attribution and the structural checks the client must be able to trust.
	for index := range decl.Slots {
		decl.Slots[index].Plugin = pluginID
	}
	for index := range decl.Contributes {
		decl.Contributes[index].Plugin = pluginID
	}
	decl.Problems = append(decl.Problems, ValidateExtension(pluginID, manifestKind(manifest), decl)...)
	return decl
}

func manifestKind(manifest *pluginpkg.Manifest) string {
	if manifest == nil {
		return ""
	}
	return string(manifest.Kind)
}

// derivedLayer is the manifest's documented default when `layer` is omitted:
// webview→ui, sidecar→provider, in-process→harness.
func derivedLayer(kind string) string {
	switch kind {
	case "webview":
		return "ui"
	case "sidecar":
		return "provider"
	case "in-process":
		return "harness"
	}
	return ""
}

func readAtMost(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, os.ErrInvalid
	}
	return os.ReadFile(path)
}

func mergeExtensionManifest(dst *ExtensionDeclaration, doc extensionManifest) {
	if dst.Layer == "" {
		dst.Layer = strings.TrimSpace(doc.Layer)
	}
	for _, slot := range doc.Provides.Slots {
		mergeSlot(dst, slot)
	}
	for _, contribution := range doc.Contributes {
		mergeContribution(dst, contribution)
	}
}

func mergeExtensionFile(dst *ExtensionDeclaration, doc extensionFile) {
	if dst.Layer == "" {
		dst.Layer = strings.TrimSpace(doc.Layer)
	}
	for _, slot := range doc.Slots {
		mergeSlot(dst, slot)
	}
	for _, slot := range doc.Provides.Slots {
		mergeSlot(dst, slot)
	}
	for _, contribution := range doc.Contributes {
		mergeContribution(dst, contribution)
	}
}

func mergeSlot(dst *ExtensionDeclaration, raw extensionSlot) {
	id := strings.TrimSpace(raw.ID)
	if id == "" {
		return
	}
	for _, existing := range dst.Slots {
		if existing.ID == id {
			return
		}
	}
	slot := ExtensionSlot{
		ID:        id,
		Forms:     append([]string(nil), raw.Forms...),
		Arity:     strings.TrimSpace(raw.Arity),
		Targets:   append([]string(nil), raw.Targets...),
		Lifecycle: append([]string(nil), raw.Lifecycle...),
	}
	if raw.Schema != nil {
		if encoded, err := json.Marshal(raw.Schema); err == nil {
			slot.Schema = encoded
		}
	}
	dst.Slots = append(dst.Slots, slot)
}

func mergeContribution(dst *ExtensionDeclaration, raw extensionContribution) {
	slot := strings.TrimSpace(raw.Slot)
	if slot == "" {
		return
	}
	id := strings.TrimSpace(raw.ID)
	form := strings.TrimSpace(raw.Form)
	for _, existing := range dst.Contributes {
		if id != "" && existing.ID == id {
			return
		}
		if id == "" && existing.ID == "" && existing.Slot == slot && existing.Form == form && existing.Entry == strings.TrimSpace(raw.Entry) {
			return
		}
	}
	contribution := ExtensionContribution{
		ID:           id,
		Slot:         slot,
		Form:         form,
		Entry:        strings.TrimSpace(raw.Entry),
		Title:        strings.TrimSpace(raw.Title),
		Description:  strings.TrimSpace(raw.Description),
		Capabilities: append([]string(nil), raw.Capabilities...),
		Targets:      append([]string(nil), raw.Targets...),
		Tool:         strings.TrimSpace(raw.Tool),
		Part:         strings.TrimSpace(raw.Part),
	}
	if raw.Payload != nil {
		if encoded, err := json.Marshal(raw.Payload); err == nil {
			contribution.Payload = encoded
		}
	}
	dst.Contributes = append(dst.Contributes, contribution)
}

// ValidateExtension checks the shape the client relies on. Slot existence in
// another plugin's catalog is the client registry's check (the harness owns no
// slot list); this function owns required fields, closed sets and path safety.
func ValidateExtension(pluginID, kind string, decl ExtensionDeclaration) []pluginpkg.FieldError {
	var problems []pluginpkg.FieldError
	add := func(field, message string) {
		problems = append(problems, pluginpkg.FieldError{Field: field, Message: message})
	}
	if layer := strings.TrimSpace(decl.Layer); layer != "" && !extensionLayerSet[layer] {
		add("layer", "layer must be kernel|harness|provider|ui|service")
	}
	for index, slot := range decl.Slots {
		field := func(name string) string { return "provides.slots[" + itoa(index) + "]." + name }
		id := strings.TrimSpace(slot.ID)
		if id == "" {
			add(field("id"), "a slot needs an id")
		} else if !extensionSlotPattern.MatchString(id) {
			add(field("id"), "a slot id must be <plugin>.<name> (lowercase, dots allowed)")
		} else if pluginID != "" && !strings.HasPrefix(id, pluginID+".") {
			add(field("id"), "a slot id must be namespaced by its declaring plugin ("+pluginID+".)")
		}
		if len(slot.Forms) == 0 {
			add(field("forms"), "a slot must accept at least one form")
		}
		seenForm := map[string]bool{}
		for formIndex, form := range slot.Forms {
			form = strings.TrimSpace(form)
			if !extensionFormSet[form] {
				add(field("forms["+itoa(formIndex)+"]"), `form must be chunk|package|iframe|data`)
				continue
			}
			if seenForm[form] {
				add(field("forms["+itoa(formIndex)+"]"), "form "+form+" is declared twice")
			}
			seenForm[form] = true
		}
		if len(slot.Targets) == 0 {
			add(field("targets"), "a slot must name at least one target")
		}
		seenTarget := map[string]bool{}
		for targetIndex, target := range slot.Targets {
			target = strings.TrimSpace(target)
			if !extensionTargetSet[target] {
				add(field("targets["+itoa(targetIndex)+"]"), "target must be web|native|tui")
				continue
			}
			if seenTarget[target] {
				add(field("targets["+itoa(targetIndex)+"]"), "target "+target+" is declared twice")
			}
			seenTarget[target] = true
		}
		if arity := strings.TrimSpace(slot.Arity); arity != "" && !extensionAritySet[arity] {
			add(field("arity"), "arity must be single|multi|ordered")
		}
		if len(slot.Schema) > 0 && !json.Valid(slot.Schema) {
			add(field("schema"), "schema must be valid JSON")
		}
	}
	for index, contribution := range decl.Contributes {
		field := func(name string) string { return "contributes[" + itoa(index) + "]." + name }
		if strings.TrimSpace(contribution.Slot) == "" {
			add(field("slot"), "a contribution must name its slot")
		} else if !extensionSlotPattern.MatchString(contribution.Slot) {
			add(field("slot"), "the slot id must be <plugin>.<name>")
		}
		form := strings.TrimSpace(contribution.Form)
		if !extensionFormSet[form] {
			add(field("form"), "form must be chunk|package|iframe|data")
		} else if form != string(FormData) {
			entry := strings.TrimSpace(contribution.Entry)
			if entry == "" {
				add(field("entry"), form+" contributions need an entry")
			} else if problem := entryProblem(form, entry); problem != "" {
				add(field("entry"), problem)
			}
		}
		if form == string(FormData) && len(contribution.Payload) == 0 {
			add(field("payload"), "data contributions need a payload")
		}
		if len(contribution.Capabilities) > 0 {
			for capIndex, capability := range contribution.Capabilities {
				if !extensionCapSet[strings.TrimSpace(capability)] {
					add(field("capabilities["+itoa(capIndex)+"]"), "capability must be state|call|approve|subscribe")
				}
			}
		}
		for targetIndex, target := range contribution.Targets {
			if !extensionTargetSet[strings.TrimSpace(target)] {
				add(field("targets["+itoa(targetIndex)+"]"), "target must be web|native|tui")
			}
		}
		if contribution.ID != "" && !extensionSlotPattern.MatchString(contribution.ID) {
			// A contribution id is not a slot id; only the shared safe grammar is
			// required (letters, digits, dot, underscore, colon, dash).
			if !safeExtensionID(contribution.ID) {
				add(field("id"), "the contribution id has an unsupported character")
			}
		}
	}
	return problems
}

func entryProblem(form, entry string) string {
	if strings.Contains(entry, "\\") || strings.Contains(entry, "..") {
		return "entry must not escape the package (no backslash or ..)"
	}
	for _, r := range entry {
		if r < 0x20 {
			return "entry must not carry control characters"
		}
	}
	if form == string(FormIFrame) && strings.HasPrefix(strings.ToLower(entry), "javascript:") {
		return "an iframe entry must not be a javascript: URL"
	}
	if form == string(FormPackage) && !strings.HasSuffix(strings.ToLower(entry), ".nshpkg") {
		return "a package entry must name a .nshpkg"
	}
	return ""
}

func safeExtensionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
