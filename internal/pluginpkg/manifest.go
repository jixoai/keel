// Package pluginpkg is the plugin package contract: the `plugin.yaml` manifest,
// its validation rules (field-level errors), the CLI capability tree
// (`provides.cli`, see cli.go), package discovery under a plugins root, the
// version gate (`engines.keel`) and the content-addressed package store (tree
// hash + managed copy).
//
// The package never loads a plugin. Loading is the harnessext plugin manager's
// job, which consumes the types here; keeping the split means a manifest can be
// parsed, validated and audited in tests (and by tooling) without a runtime.
//
// Spec: openspec/changes/keel-plugin-core/specs/plugin-system/spec.md
// (Requirement: 插件包与清单 / 发现、安装与包存储);
// openspec/changes/keel-agent-team-v1/specs/plugin-capabilities/spec.md
// (Requirement: 每个插件声明能力树).
package pluginpkg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jixoai/keel/internal/modelprovider"
)

// PluginFileName is the manifest file inside a plugin package directory.
const PluginFileName = "plugin.yaml"

// Kind is the plugin's loading strategy.
type Kind string

const (
	// KindInProcess plugins are compiled into the binary; the manifest only
	// controls their enablement and configuration.
	KindInProcess Kind = "in-process"
	// KindSidecar plugins run as an external process spoken to over stdio
	// line-delimited JSON-RPC.
	KindSidecar Kind = "sidecar"
	// KindWebview plugins are UI resource packages hosted by the product at
	// /plugins/<id>/ and rendered in a webview; an optional sidecar backend may
	// accompany them.
	KindWebview Kind = "webview"
)

// Kinds is the closed set of plugin kinds.
func Kinds() []Kind { return []Kind{KindInProcess, KindSidecar, KindWebview} }

// ParseKind validates a kind string.
func ParseKind(raw string) (Kind, bool) {
	for _, kind := range Kinds() {
		if string(kind) == raw {
			return kind, true
		}
	}
	return "", false
}

// Layer is the plugin's architectural role. It is orthogonal to Kind: kind says
// *how* a plugin is carried (in-process/sidecar/webview), layer says *where* its
// contract lives (kernel/harness/provider/ui/service).
//
// Spec: openspec/changes/keel-harness-v2/specs/plugin-system/spec.md
// (Requirement: 三种 kind) and specs/plugin-architecture (Requirement: 插件
// layer 与装载边界). The freeze document is docs/PLUGIN-EXTENSIBILITY.md.
type Layer string

const (
	// LayerKernel plugins implement an agent-loop seam. They run in the kernel
	// process and MUST NOT reach the product layer or do direct IO.
	LayerKernel Layer = "kernel"
	// LayerHarness plugins provide tools, services, placement or memory inside
	// the Harness process.
	LayerHarness Layer = "harness"
	// LayerProvider plugins adapt an external capability in a separate process
	// (sidecar or a webview backend).
	LayerProvider Layer = "provider"
	// LayerUI plugins contribute client slots. They are carried by the NSH
	// client as webview packages.
	LayerUI Layer = "ui"
	// LayerService plugins are independent services spoken to over gRPC.
	LayerService Layer = "service"
)

// Layers is the closed set of plugin layers.
func Layers() []Layer {
	return []Layer{LayerKernel, LayerHarness, LayerProvider, LayerUI, LayerService}
}

// ParseLayer validates a layer string.
func ParseLayer(raw string) (Layer, bool) {
	for _, layer := range Layers() {
		if string(layer) == raw {
			return layer, true
		}
	}
	return "", false
}

// DeriveLayer maps a carry kind onto its default layer. The mapping is the
// spec's default (webview→ui, sidecar→provider, in-process→harness); an
// explicit `layer:` overrides it.
func DeriveLayer(kind Kind) Layer {
	switch kind {
	case KindWebview:
		return LayerUI
	case KindSidecar:
		return LayerProvider
	case KindInProcess:
		return LayerHarness
	}
	return ""
}

// ResolvedLayer is the manifest's architectural layer: the declared value when
// present, otherwise the kind's default. It is empty when the kind is invalid.
func (m *Manifest) ResolvedLayer() Layer {
	if m.Layer != "" {
		return m.Layer
	}
	return DeriveLayer(m.Kind)
}

// SourceType is the closed set of provenance types a manifest may declare.
type SourceType string

const (
	SourceBuiltin SourceType = "builtin"
	SourceLocal   SourceType = "local"
	SourceGit     SourceType = "git"
	SourceNPM     SourceType = "npm"
)

// SourceTypes is the closed set of provenance types.
func SourceTypes() []SourceType {
	return []SourceType{SourceBuiltin, SourceLocal, SourceGit, SourceNPM}
}

// Manifest is one parsed `plugin.yaml`. Field names and shapes follow the spec;
// unknown fields are rejected so a typo is an error instead of a silent no-op.
type Manifest struct {
	// ID is the globally unique plugin id (^[a-z][a-z0-9._-]{1,63}$).
	ID string `yaml:"id" json:"id"`
	// Version is the plugin's own semantic version.
	Version string `yaml:"version" json:"version"`
	// Kind selects the loading strategy.
	Kind Kind `yaml:"kind" json:"kind"`
	// Layer is the architectural role. Optional: an empty value is derived
	// from Kind (see ResolvedLayer).
	Layer Layer `yaml:"layer,omitempty" json:"layer,omitempty"`
	// Description is one line shown in listings.
	Description string `yaml:"description" json:"description"`
	// Engines carries the host compatibility gate; a missing keel entry means
	// "compatible with any keel".
	Engines Engines `yaml:"engines,omitempty" json:"engines,omitempty"`
	// Provides declares what the plugin contributes.
	Provides Provides `yaml:"provides,omitempty" json:"provides,omitempty"`
	// Contributes are the slot contributions this plugin ships.
	Contributes []Contribution `yaml:"contributes,omitempty" json:"contributes,omitempty"`
	// Extensibility points at the plugin's extension-surface documents. Official
	// plugins MUST declare it; third-party plugins MAY omit it.
	Extensibility *Extensibility `yaml:"extensibility,omitempty" json:"extensibility,omitempty"`
	// Requires lists plugin or service ids the plugin needs to load.
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
	// Permissions declares the capabilities the plugin may use.
	Permissions Permissions `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	// Entry describes how a sidecar (or a webview's backend) is executed.
	Entry Entry `yaml:"entry,omitempty" json:"entry,omitempty"`
	// Source is provenance: where the package came from.
	Source Source `yaml:"source,omitempty" json:"source,omitempty"`

	// Dir is set by the loader (not part of plugin.yaml): the package directory.
	Dir string `yaml:"-" json:"dir,omitempty"`
	// ParsedVersion is the parsed form of Version (set by Load).
	ParsedVersion SemVer `yaml:"-" json:"-"`
}

// Engines is the host compatibility gate.
type Engines struct {
	// Keel is a version range (e.g. ">=0.2.0 <0.3.0"). Empty means compatible.
	Keel string `yaml:"keel,omitempty" json:"keel,omitempty"`
}

// Provides is what a plugin contributes.
type Provides struct {
	// Tools are model-facing tool names (globally unique, conflicts fail closed).
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Hooks are hook events from the closed set.
	Hooks []string `yaml:"hooks,omitempty" json:"hooks,omitempty"`
	// Triggers are trigger ids the plugin owns.
	Triggers []string `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// ProviderTypes are the provider capability families the plugin declares
	// (`provides.provider_types`). Each declaration carries the display name,
	// capabilities and the config/auth/model/parameter schemas the settings UI
	// renders and the harness validates instances against; the type vocabulary is
	// open, so a plugin may declare "embedding" without a code change.
	ProviderTypes []modelprovider.ProviderType `yaml:"provider_types,omitempty" json:"provider_types,omitempty"`
	// Provider marks the plugin as a provider plugin (`provides.provider`): the
	// spec requires `layer: provider`, and the plugin may list the feature ports
	// it serves.
	Provider *ProviderPluginDecl `yaml:"provider,omitempty" json:"provider,omitempty"`
	// FeaturePorts are the feature ports the plugin itself serves
	// (`provides.feature_ports`), distinct from the ports a single provider type
	// declares.
	FeaturePorts []string `yaml:"feature_ports,omitempty" json:"feature_ports,omitempty"`
	// Agents are the agent ids the plugin contributes (`provides.agents`).
	Agents []string `yaml:"agents,omitempty" json:"agents,omitempty"`
	// AgentTeams are the agent-team ids the plugin contributes
	// (`provides.agent_teams`).
	AgentTeams []string `yaml:"agent_teams,omitempty" json:"agent_teams,omitempty"`
	// CLI is the plugin's capability tree: every command and sub-command it
	// contributes below `keel`, with the summary, schemas, permissions, danger,
	// idempotency and callers that `--help`, the settings page, creation mode
	// and the agent tool face share as one source of truth.
	CLI []CLICapability `yaml:"cli,omitempty" json:"cli,omitempty"`
	// Commands are the plugin's chat commands (`provides.commands`): named
	// user-intent entrances published as `/<plugin-id>:<name>`. Every command
	// binds one of this manifest's provides.tools; it adds no execution
	// primitive. Spec: openspec/changes/keel-plugin-commands.
	Commands []CommandDecl `yaml:"commands,omitempty" json:"commands,omitempty"`
	// UI is the webview resource declaration. It is required for a webview
	// package that contributes iframe surfaces; a pure chunk package may omit
	// it (see validateProvides).
	UI *UI `yaml:"ui,omitempty" json:"ui,omitempty"`
	// Slots are the slots this plugin declares (the extension points others
	// contribute to). Slot ids carry the declaring plugin's namespace.
	Slots []SlotDecl `yaml:"slots,omitempty" json:"slots,omitempty"`
	// Backends map existing aggregate-tool actions onto this plugin's
	// implementation. The action set itself is owned by the tool surface; a
	// backend cannot add an action name.
	Backends []BackendDecl `yaml:"backends,omitempty" json:"backends,omitempty"`
	// Telemetry declares the telemetry exporter capability (change
	// keel-plugin-telemetry). It is a closed vocabulary: one exporter protocol
	// and the three PI signals. A plugin without it cannot register an exporter.
	Telemetry *TelemetryDecl `yaml:"telemetry,omitempty" json:"telemetry,omitempty"`
}

// TelemetryDecl is the `provides.telemetry` block.
type TelemetryDecl struct {
	// Exporters are the exporter protocols the plugin can drive. The closed set
	// is otlp-http (the zero-dependency adapter); OTLP/gRPC and vendor SDKs are
	// out of scope.
	Exporters []string `yaml:"exporters,omitempty" json:"exporters,omitempty"`
	// Signals are the PI signal families the exporter can carry.
	Signals []string `yaml:"signals,omitempty" json:"signals,omitempty"`
}

// TelemetryExporters is the closed exporter set.
func TelemetryExporters() []string { return []string{"otlp-http"} }

// TelemetrySignals is the closed signal set (mirrors internal/telemetry).
func TelemetrySignals() []string { return []string{"traces", "metrics", "logs"} }

// SlotForm is one contribution carrier a slot may accept.
type SlotForm string

const (
	// SlotFormChunk is an NSH dynamic import rendered inside the host tree.
	SlotFormChunk SlotForm = "chunk"
	// SlotFormPackage is a signed NSH package run as an isolated surface.
	SlotFormPackage SlotForm = "package"
	// SlotFormIframe is an HTML page rendered in an iframe/WebView.
	SlotFormIframe SlotForm = "iframe"
	// SlotFormData is host-consumed JSON (nav items, actions); no code runs.
	SlotFormData SlotForm = "data"
)

// SlotForms is the closed set of contribution forms.
func SlotForms() []SlotForm {
	return []SlotForm{SlotFormChunk, SlotFormPackage, SlotFormIframe, SlotFormData}
}

// SlotArity is the multiplicity semantics of a slot.
type SlotArity string

const (
	SlotAritySingle  SlotArity = "single"
	SlotArityMulti   SlotArity = "multi"
	SlotArityOrdered SlotArity = "ordered"
)

// SlotArities is the closed set of slot arities.
func SlotArities() []SlotArity { return []SlotArity{SlotAritySingle, SlotArityMulti, SlotArityOrdered} }

// SlotTarget is a client target a slot is available on.
type SlotTarget string

const (
	SlotTargetWeb    SlotTarget = "web"
	SlotTargetNative SlotTarget = "native"
	SlotTargetTUI    SlotTarget = "tui"
)

// SlotTargets is the closed set of slot targets.
func SlotTargets() []SlotTarget { return []SlotTarget{SlotTargetWeb, SlotTargetNative, SlotTargetTUI} }

// SlotDecl is one slot declaration (`provides.slots[]`). A slot declares the
// forms it accepts, its multiplicity, the targets it is available on and (for
// data contributions) the JSON schema those payloads obey.
type SlotDecl struct {
	// ID is the namespaced slot id (`<plugin>.<name>`).
	ID string `yaml:"id" json:"id"`
	// Forms are the contribution forms the slot accepts; non-empty.
	Forms []SlotForm `yaml:"forms" json:"forms"`
	// Arity is single|multi|ordered.
	Arity SlotArity `yaml:"arity" json:"arity"`
	// Targets are the client targets the slot is available on; non-empty.
	Targets []SlotTarget `yaml:"targets" json:"targets"`
	// Schema is the JSON schema data contributions must satisfy. Required when
	// Forms contains data.
	Schema map[string]any `yaml:"schema,omitempty" json:"schema,omitempty"`
}

// BackendDecl is one `provides.backends[]` entry: an aggregate-tool action this
// plugin implements. The action name is validated against the host's closed
// action set at assembly when the host supplies Environment.KnownBackend.
type BackendDecl struct {
	// Tool is the aggregate tool name (bash, mcp, skills, tasks, ...).
	Tool string `yaml:"tool" json:"tool"`
	// Action is the existing action inside that tool.
	Action string `yaml:"action" json:"action"`
	// Impl names the implementation inside the plugin (informational for the
	// host; never a model-facing name).
	Impl string `yaml:"impl" json:"impl"`
}

// Contribution is one `contributes[]` entry: what this plugin puts into a slot.
type Contribution struct {
	// Slot is the target slot id (the declaring plugin's namespace).
	Slot string `yaml:"slot" json:"slot"`
	// Form is chunk|package|iframe|data.
	Form SlotForm `yaml:"form" json:"form"`
	// Entry is the package-relative file for chunk/package/iframe forms.
	Entry string `yaml:"entry,omitempty" json:"entry,omitempty"`
	// Payload is the host-consumed JSON for the data form.
	Payload map[string]any `yaml:"payload,omitempty" json:"payload,omitempty"`
	// Part narrows a `keel.ui.message-part` contribution to one message-part
	// kind. Claims are declared in extensibility.json (change
	// keel-plugin-attachment); plugin.yaml contributions carry none.
	Part string `yaml:"part,omitempty" json:"part,omitempty"`
	// Mime narrows an attachment contribution to MIME patterns. Each pattern is
	// `type/subtype` or `type/*` (lowercase); a contribution without patterns
	// claims every attachment MIME.
	Mime []string `yaml:"mime,omitempty" json:"mime,omitempty"`
}

// Extensibility is the manifest's pointer at a plugin's extension-surface
// documents: EXTENSIBILITY.md (human) and extensibility.json (machine). An
// official plugin MUST declare it; a third-party plugin MAY omit it and is then
// marked "not usable by creation mode" (spec: 创造模式的聚合协议).
type Extensibility struct {
	// Doc is the human-readable document, relative to the package root.
	Doc string `yaml:"doc,omitempty" json:"doc,omitempty"`
	// Schema is the machine-readable document, relative to the package root.
	Schema string `yaml:"schema,omitempty" json:"schema,omitempty"`
}

// ReservedSlotIDs are the first-party slot ids frozen by this contract. They
// are declared by plugins (`keel.ui`, `keel.tasks`), never built into the
// Harness; the list is exported so tooling and tests share one spelling.
func ReservedSlotIDs() []string {
	return []string{
		"keel.ui.route",
		"keel.ui.nav",
		"keel.ui.action",
		"keel.ui.tool-renderer",
		"keel.ui.message-part",
		"keel.ui.sidebar",
		"keel.ui.settings-section",
		"keel.ui.surface",
		"keel.tasks.widget",
	}
}

// The attachment preview contract (change keel-plugin-attachment): an
// extensibility.json contribution to the message-part slot claims attachment
// rendering with `part: attachment` and an optional `mime` narrowing.
const (
	// MessagePartSlotID is the generic message-part slot attachment previews
	// reuse. It stays a general slot; `part: attachment` is the claim.
	MessagePartSlotID = "keel.ui.message-part"
	// AttachmentPartKind is the only message-part kind defined today.
	AttachmentPartKind = "attachment"
	// MimePatternsMax bounds one contribution's MIME patterns.
	MimePatternsMax = 32
	// MimePatternMaxBytes bounds one MIME pattern (the longest legal MIME type
	// names are far below this; the bound keeps a plugin from storing junk).
	MimePatternMaxBytes = 128
	// mimeTokenMax bounds one type/subtype token inside a pattern.
	mimeTokenMax = 127
)

// mimeTokenPattern is the grammar of one non-wildcard MIME token: lowercase
// letters, digits and the RFC 6838 token punctuation. A leading alphanumeric
// keeps the pattern from starting with punctuation.
var mimeTokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// MimePatternProblem reports why a MIME narrowing pattern is not `type/subtype`
// or `type/*`. An empty result means the pattern is well-formed.
func MimePatternProblem(pattern string) string {
	if strings.TrimSpace(pattern) == "" {
		return "a MIME pattern must not be empty"
	}
	if len(pattern) > MimePatternMaxBytes {
		return fmt.Sprintf("a MIME pattern must be at most %d bytes", MimePatternMaxBytes)
	}
	if pattern != strings.ToLower(pattern) {
		return "a MIME pattern must be lowercase"
	}
	slash := strings.IndexByte(pattern, '/')
	if slash <= 0 || slash == len(pattern)-1 || strings.Count(pattern, "/") != 1 {
		return "a MIME pattern must be type/subtype or type/*"
	}
	typ, subtype := pattern[:slash], pattern[slash+1:]
	if typ == "*" {
		return "the wildcard is only allowed as the subtype (type/*)"
	}
	if len(typ) > mimeTokenMax || !mimeTokenPattern.MatchString(typ) {
		return "the MIME type token has an unsupported character"
	}
	if subtype == "*" {
		return ""
	}
	if len(subtype) > mimeTokenMax || !mimeTokenPattern.MatchString(subtype) {
		return "the MIME subtype token has an unsupported character"
	}
	return ""
}

// AttachmentClaimProblems validates the `part`/`mime` claim of one contribution
// relative to its slot. Field paths are relative ("part", "mime[0]"); callers
// prefix them with the contribution path they render. The duplicate rule is
// per plugin and lives in the callers that see every contribution.
func AttachmentClaimProblems(slot, part string, mime []string) []FieldError {
	var problems []FieldError
	add := func(field, format string, args ...any) {
		problems = append(problems, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}
	slot = strings.TrimSpace(slot)
	part = strings.TrimSpace(part)
	if slot != MessagePartSlotID {
		if part != "" {
			add("part", "part is only supported on %s", MessagePartSlotID)
		}
		if len(mime) > 0 {
			add("mime", "mime narrowing is only supported on %s", MessagePartSlotID)
		}
		return problems
	}
	if part != "" && part != AttachmentPartKind {
		add("part", "part must be %s", AttachmentPartKind)
	}
	if len(mime) > 0 && part != AttachmentPartKind {
		add("part", "mime narrowing requires part: %s", AttachmentPartKind)
		add("mime", "mime narrowing requires part: %s", AttachmentPartKind)
	}
	if len(mime) > MimePatternsMax {
		add("mime", "at most %d MIME patterns are allowed per contribution", MimePatternsMax)
	}
	for index, pattern := range mime {
		if problem := MimePatternProblem(pattern); problem != "" {
			add(fmt.Sprintf("mime[%d]", index), "%s", problem)
		}
	}
	return problems
}

// formSupportsTarget reports whether a contribution form can render on a
// target. `package` and `iframe` need a graphics surface; `chunk` and `data`
// render everywhere, including the TUI.
func formSupportsTarget(form SlotForm, target SlotTarget) bool {
	switch form {
	case SlotFormChunk, SlotFormData:
		return true
	case SlotFormPackage, SlotFormIframe:
		return target == SlotTargetWeb || target == SlotTargetNative
	}
	return false
}

// ProviderPluginLayer is the only legal value of `provides.provider.layer`.
const ProviderPluginLayer = "provider"

// ProviderPluginDecl is the plugin-level provider declaration
// (`provides.provider`): a provider plugin MUST declare `layer: provider`
// (specs/provider-registry, Requirement: Provider 插件隔离与崩溃边界).
type ProviderPluginDecl struct {
	// Layer is the plugin layer; must be ProviderPluginLayer.
	Layer string `yaml:"layer" json:"layer"`
	// FeaturePorts are the feature ports this provider plugin serves.
	FeaturePorts []string `yaml:"feature_ports,omitempty" json:"feature_ports,omitempty"`
}

// UI is a webview plugin's resource declaration.
type UI struct {
	// Entry is the HTML entry point, relative to the package root
	// (e.g. "ui/index.html"). It must stay inside the package.
	Entry string `yaml:"entry" json:"entry"`
	// Title is the display name of the plugin's surface.
	Title string `yaml:"title,omitempty" json:"title,omitempty"`
}

// Permissions is the capability declaration. Shape validation happens here;
// enforcement (tool whitelist at dispatch, net hosts, secret references,
// sidecar env whitelist) is the runtime's half of the spec.
type Permissions struct {
	// FS lists file-effect permissions (e.g. workspace-write).
	FS []string `yaml:"fs,omitempty" json:"fs,omitempty"`
	// Net lists allowed hosts.
	Net []string `yaml:"net,omitempty" json:"net,omitempty"`
	// Secrets lists secret names the plugin may reference.
	Secrets []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// Tools lists tool names the plugin may call.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty"`
}

// Empty reports whether no permission is declared.
func (p Permissions) Empty() bool {
	return len(p.FS) == 0 && len(p.Net) == 0 && len(p.Secrets) == 0 && len(p.Tools) == 0
}

// Entry describes the executable side of a sidecar (or webview backend).
type Entry struct {
	// Command is the argv head (sidecar: required).
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	// Args are appended to Command.
	Args []string `yaml:"args,omitempty" json:"args,omitempty"`
	// Env lists environment variable *names* the sidecar may receive. Values are
	// supplied by the host's whitelist, never by the manifest (a manifest that
	// could carry a value could exfiltrate one).
	Env []string `yaml:"env,omitempty" json:"env,omitempty"`
}

// Empty reports whether the entry declares nothing.
func (e Entry) Empty() bool { return len(e.Command) == 0 && len(e.Args) == 0 && len(e.Env) == 0 }

// Source is provenance. Type is the closed set builtin|local|git|npm.
type Source struct {
	Type SourceType `yaml:"type,omitempty" json:"type,omitempty"`
	// Path is the origin directory for local installs (informational).
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// URL and Ref are the git origin and revision.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty"`
	// Package is the npm package spec (name@version).
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
}

// Environment is the host facts a manifest is validated against.
type Environment struct {
	// KeelVersion is the running keel version, compared against engines.keel.
	KeelVersion string
	// KnownHookEvent reports membership in the closed hook-event set. Nil skips
	// the closed-set check (used by pure manifest-syntax tests).
	KnownHookEvent func(string) bool
	// KnownSlot resolves a slot id declared by an installed plugin. When set,
	// a contribution to an unknown slot — or in a form the slot does not accept
	// — is refused. Nil keeps validation to syntax + self-declared slots.
	KnownSlot func(id string) (SlotDecl, bool)
	// KnownBackend reports whether (tool, action) is an existing action of an
	// aggregate tool. When set, a plugin cannot invent an action name. Nil keeps
	// validation to syntax.
	KnownBackend func(tool, action string) bool
	// RequireExtensibility is the official-package release gate: a manifest
	// without `extensibility.doc` + `extensibility.schema` is refused. The
	// runtime install path leaves it false (third parties may omit the docs).
	RequireExtensibility bool
}

// idPattern is the spec's plugin id grammar.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)

// namePattern is the grammar shared by tool, trigger and requirement names:
// the id grammar plus ':' so capability ids (cap:foo) are expressible.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// hostPattern bounds a net permission entry to hostname-shaped text.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.*-]{0,126})(:[0-9]{1,5})?$`)

// secretPattern mirrors the secret-name grammar of the settings surface
// (internal/harnessapi/settings.go): a manifest cannot reference a secret name
// that no store would accept.
var secretPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// envPattern is the grammar of an entry.env variable name.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// slotIDPattern is the grammar of a namespaced slot id. A slot id always
// contains a dot (the declaring plugin's namespace), so a bare name like
// "route" is refused instead of silently becoming a global slot.
var slotIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,127}$`)

// backendNamePattern is the grammar of a contribution/backend name.
var backendNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]{0,127}$`)

// MaxPublishedToolName bounds a model-facing tool name. OpenAI-compatible
// endpoints accept `^[a-zA-Z0-9_-]{1,64}$` as a function name; a published name
// outside that grammar is not callable by a real model, so a manifest that
// would publish one is refused at install instead of failing at call time.
const MaxPublishedToolName = 64

// publishedToolPattern is the wire grammar of a published (model-facing) tool
// name. A manifest may DECLARE a richer name (`provides.tools` follows the id
// grammar plus ':'); what the host PUBLISHES must satisfy this pattern.
var publishedToolPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// isWireSafeRune reports whether r may appear in a published tool name.
func isWireSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		return true
	}
	return false
}

// NormalizePublishedToolName rewrites every character outside [A-Za-z0-9_-] to
// '_'. The host namespaces a sidecar's tools as `<plugin>.<tool>`, and a '.' on
// the wire is a function name no OpenAI-compatible endpoint accepts, so
// `demo.sidecar.greet` is published as `demo_sidecar_greet`. A name that is
// already wire safe is returned verbatim: publication normalizes instead of
// rejecting, and never rewrites a name that is already valid.
func NormalizePublishedToolName(name string) string {
	clean := true
	for _, r := range name {
		if !isWireSafeRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return name
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isWireSafeRune(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

// ValidPublishedToolName reports whether a name satisfies the wire grammar the
// model-facing surface must obey.
func ValidPublishedToolName(name string) bool { return publishedToolPattern.MatchString(name) }

// NamespacedPublishedToolName is the published name of a sidecar (or webview
// backend) tool: the plugin id joined with the local name, then normalized.
// local is the name the plugin itself speaks (`greet`), never a declared
// spelling, so the same tool cannot be qualified twice.
func NamespacedPublishedToolName(pluginID, local string) string {
	if pluginID == "" {
		return NormalizePublishedToolName(local)
	}
	return NormalizePublishedToolName(pluginID + "." + local)
}

// PublishedTool is one `provides.tools` declaration and the model-facing name it
// is published under.
type PublishedTool struct {
	// Declared is the manifest spelling (`greet`, `demo.sidecar.greet`).
	Declared string `json:"declared"`
	// Local is the name the plugin itself speaks: the declared name without the
	// host's `<id>.` namespace, which is what a sidecar lists in tools/list.
	Local string `json:"local"`
	// Published is the wire-safe name the model calls (`demo_sidecar_greet`).
	Published string `json:"published"`
}

// NamespacesTools reports whether the host publishes this kind's tools as
// `<id>.<local>`: a sidecar (and a webview's optional sidecar backend) speaks
// local tool names, so the host namespaces them; an in-process plugin names its
// model-facing tools directly.
func (m *Manifest) NamespacesTools() bool {
	return m.Kind == KindSidecar || m.Kind == KindWebview
}

// PublishedTools maps every `provides.tools` entry onto its declared, local and
// published spellings. It is the one conversion the validator, the loaders, the
// permission policy and the inventory view share, so "what the model calls"
// cannot drift between them.
func (m *Manifest) PublishedTools() []PublishedTool {
	tools := make([]PublishedTool, 0, len(m.Provides.Tools))
	namespaced := m.NamespacesTools()
	for _, declared := range m.Provides.Tools {
		published := PublishedTool{Declared: declared, Local: declared}
		if namespaced {
			published.Local = strings.TrimPrefix(declared, m.ID+".")
			published.Published = NamespacedPublishedToolName(m.ID, published.Local)
		} else {
			published.Published = NormalizePublishedToolName(declared)
		}
		tools = append(tools, published)
	}
	return tools
}

// FieldError is one field-level validation failure.
type FieldError struct {
	// Field is the manifest path ("provides.tools[1]", "engines.keel").
	Field string `json:"field"`
	// Message is the human-readable reason.
	Message string `json:"message"`
}

func (e FieldError) String() string { return e.Field + ": " + e.Message }

// ValidationError aggregates field-level failures.
type ValidationError struct {
	// Errors are the failures, in declaration order.
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	switch len(e.Errors) {
	case 0:
		return "plugin manifest is invalid"
	case 1:
		return e.Errors[0].String()
	default:
		parts := make([]string, 0, len(e.Errors))
		for _, field := range e.Errors {
			parts = append(parts, field.String())
		}
		return fmt.Sprintf("%d invalid fields: %s", len(e.Errors), strings.Join(parts, "; "))
	}
}

// FieldErrors extracts the field-level errors of err, when it carries any.
func FieldErrors(err error) []FieldError {
	if err == nil {
		return nil
	}
	if validation, ok := err.(*ValidationError); ok {
		return validation.Errors
	}
	return nil
}

// fieldSet collects field failures while validating one manifest.
type fieldSet struct {
	list []FieldError
}

func (e *fieldSet) add(field, format string, args ...any) {
	e.list = append(e.list, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (e *fieldSet) err() error {
	if len(e.list) == 0 {
		return nil
	}
	return &ValidationError{Errors: e.list}
}

// ParseManifest decodes plugin.yaml bytes. Unknown fields, duplicate keys and
// YAML type errors are field-level errors, so the caller can return a precise
// 400. It does not validate: call Validate (or LoadDir, which does both).
func ParseManifest(raw []byte) (*Manifest, error) {
	var manifest Manifest
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, &ValidationError{Errors: []FieldError{{Field: "", Message: "plugin.yaml: " + err.Error()}}}
	}
	// A second document is refused: one package, one manifest.
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, &ValidationError{Errors: []FieldError{{Field: "", Message: "plugin.yaml carries more than one document"}}}
	}
	return &manifest, nil
}

// Validate checks the manifest against the spec and the host environment. It
// returns a *ValidationError carrying one FieldError per problem, or nil.
func (m *Manifest) Validate(env Environment) error {
	errs := &fieldSet{}
	validID, validVersion, validKind := m.validateIdentity(errs)
	if validVersion {
		m.ParsedVersion, _ = ParseSemVer(m.Version)
	}
	m.validateEngines(errs)
	m.validateLayer(errs, validKind)
	m.validateProvides(errs, env, validKind)
	m.validateRequires(errs, validID)
	m.validatePermissions(errs)
	m.validateEntry(errs, validKind)
	m.validateSource(errs)
	return errs.err()
}

func (m *Manifest) validateIdentity(errs *fieldSet) (validID, validVersion, validKind bool) {
	switch {
	case m.ID == "":
		errs.add("id", "id is required")
	case !idPattern.MatchString(m.ID):
		errs.add("id", "id %q must match ^[a-z][a-z0-9._-]{1,63}$", m.ID)
	case ReservedPluginID(m.ID):
		errs.add("id", "id %q is reserved: /skill:<name> is the builtin skill command family", m.ID)
	default:
		validID = true
	}
	if m.Version == "" {
		errs.add("version", "version is required")
	} else if _, err := ParseSemVer(m.Version); err != nil {
		errs.add("version", "%v", err)
	} else {
		validVersion = true
	}
	if m.Kind == "" {
		errs.add("kind", "kind is required (one of %s)", kindList())
	} else if _, ok := ParseKind(string(m.Kind)); !ok {
		errs.add("kind", "kind %q is not one of %s", m.Kind, kindList())
	} else {
		validKind = true
	}
	if strings.TrimSpace(m.Description) == "" {
		errs.add("description", "description is required")
	} else if len([]rune(m.Description)) > 1024 {
		errs.add("description", "description must be at most 1024 characters")
	}
	return validID, validVersion, validKind
}

func kindList() string {
	names := make([]string, 0, len(Kinds()))
	for _, kind := range Kinds() {
		names = append(names, string(kind))
	}
	return strings.Join(names, "|")
}

func layerList() string {
	names := make([]string, 0, len(Layers()))
	for _, layer := range Layers() {
		names = append(names, string(layer))
	}
	return strings.Join(names, "|")
}

// validateLayer checks the architectural layer and its carry-kind constraint.
// The layer is optional (derived from kind), but when declared it must be one of
// the five values and the carry kind must be able to host it: kernel seams are
// compiled in, services are separate processes, providers are sidecars (or a
// webview with a sidecar backend), and UI packages are webviews.
func (m *Manifest) validateLayer(errs *fieldSet, validKind bool) {
	if m.Layer != "" {
		if _, ok := ParseLayer(string(m.Layer)); !ok {
			errs.add("layer", "layer %q is not one of %s", m.Layer, layerList())
			return
		}
	}
	if !validKind || m.Layer == "" {
		return
	}
	switch m.Layer {
	case LayerKernel:
		if m.Kind != KindInProcess {
			errs.add("layer", "layer kernel runs inside the kernel process: kind %q is not in-process", m.Kind)
		}
	case LayerService:
		if m.Kind == KindWebview {
			errs.add("layer", "layer service is an independent service reached over gRPC: kind webview is not allowed")
		}
	case LayerProvider:
		if m.Kind == KindInProcess {
			errs.add("layer", "layer provider must run outside the Harness process: kind in-process is not allowed")
		}
	case LayerUI:
		if m.Kind != KindWebview {
			errs.add("layer", "layer ui is carried by the client: kind %q must be webview", m.Kind)
		}
	}
}

// validateEngines checks the range *syntax*. Whether this host satisfies the
// range is the version gate (CheckEngines), which install and enable run: an
// incompatible package is a host fact, not a broken manifest, and the two are
// reported differently (5xx-free refusal with the range vs invalid package).
func (m *Manifest) validateEngines(errs *fieldSet) {
	raw := strings.TrimSpace(m.Engines.Keel)
	if raw == "" {
		return
	}
	if _, err := ParseVersionRange(raw); err != nil {
		errs.add("engines.keel", "%v", err)
	}
}

// RangeAdmits reports whether the range admits a running keel version. A
// prerelease of the running version (a dev build "0.2.0-dev") is judged as its
// release version, so a development binary still satisfies the release ranges
// its plugins declare; explicit prerelease ranges keep their own precedence.
func RangeAdmits(rng VersionRange, keelVersion string) bool {
	version, err := ParseSemVer(keelVersion)
	if err != nil {
		// An unparseable running version cannot be gated; callers that care
		// pass a parsed version through CheckEngines.
		return false
	}
	if version.Pre != "" && rng.namesPrereleaseOf(version) == false {
		version = SemVer{Major: version.Major, Minor: version.Minor, Patch: version.Patch}
	}
	return rng.Contains(version)
}

// CheckEngines is the version gate used by install and enable: it reports the
// mismatch as a field-level error naming the incompatible range.
func (m *Manifest) CheckEngines(keelVersion string) error {
	raw := strings.TrimSpace(m.Engines.Keel)
	if raw == "" {
		return nil
	}
	rng, err := ParseVersionRange(raw)
	if err != nil {
		return &ValidationError{Errors: []FieldError{{Field: "engines.keel", Message: err.Error()}}}
	}
	version, err := ParseSemVer(keelVersion)
	if err != nil {
		return &ValidationError{Errors: []FieldError{{Field: "engines.keel", Message: fmt.Sprintf("running keel version %q is not a semantic version", keelVersion)}}}
	}
	if !RangeAdmits(rng, keelVersion) {
		return &ValidationError{Errors: []FieldError{{
			Field:   "engines.keel",
			Message: fmt.Sprintf("plugin %s %s requires keel %s; this host runs %s", m.ID, m.Version, raw, version),
		}}}
	}
	return nil
}

func (m *Manifest) validateProvides(errs *fieldSet, env Environment, validKind bool) {
	seenTools := map[string]bool{}
	for i, name := range m.Provides.Tools {
		field := fmt.Sprintf("provides.tools[%d]", i)
		switch {
		case strings.TrimSpace(name) == "":
			errs.add(field, "tool name must not be empty")
		case !namePattern.MatchString(name):
			errs.add(field, "tool name %q must match %s", name, namePattern.String())
		case seenTools[name]:
			errs.add(field, "tool name %q is declared twice", name)
		default:
			seenTools[name] = true
		}
	}
	// The publication half of the tool contract: what the host puts on the wire
	// must satisfy the model endpoint's function-name grammar. The check runs
	// here, at manifest validation (install), so a package that would publish an
	// uncallable name is refused with a field error instead of failing when a
	// model finally calls it.
	seenPublished := map[string]string{}
	for i, tool := range m.PublishedTools() {
		field := fmt.Sprintf("provides.tools[%d]", i)
		if !ValidPublishedToolName(tool.Published) {
			errs.add(field, "published tool name %q (from %q) must match ^[A-Za-z0-9_-]{1,%d}$",
				tool.Published, tool.Declared, MaxPublishedToolName)
			continue
		}
		if previous, exists := seenPublished[tool.Published]; exists && previous != tool.Declared {
			errs.add(field, "tool %q publishes the same name %q as %q", tool.Declared, tool.Published, previous)
			continue
		}
		seenPublished[tool.Published] = tool.Declared
	}
	seenHooks := map[string]bool{}
	for i, event := range m.Provides.Hooks {
		field := fmt.Sprintf("provides.hooks[%d]", i)
		switch {
		case strings.TrimSpace(event) == "":
			errs.add(field, "hook event must not be empty")
		case seenHooks[event]:
			errs.add(field, "hook event %q is declared twice", event)
		case env.KnownHookEvent != nil && !env.KnownHookEvent(event):
			errs.add(field, "hook event %q is not in the closed event set", event)
		default:
			seenHooks[event] = true
		}
	}
	seenTriggers := map[string]bool{}
	for i, id := range m.Provides.Triggers {
		field := fmt.Sprintf("provides.triggers[%d]", i)
		switch {
		case strings.TrimSpace(id) == "":
			errs.add(field, "trigger id must not be empty")
		case !namePattern.MatchString(id):
			errs.add(field, "trigger id %q must match %s", id, namePattern.String())
		case seenTriggers[id]:
			errs.add(field, "trigger id %q is declared twice", id)
		default:
			seenTriggers[id] = true
		}
	}
	m.validateProviderProvides(errs)
	m.validateAgentProvides(errs)
	m.validateCLI(errs)
	m.validateCommands(errs, seenTools)
	m.validateSlots(errs)
	m.validateBackends(errs, env)
	m.validateContributions(errs, env)
	m.validateTelemetry(errs)
	m.validateExtensibility(errs, env)
	// An iframe contribution is an HTML surface: the package must declare the
	// resource entry it is loaded from (spec: webview 的两种承载). This is
	// checked before the provides.ui block below because the requirement is
	// about the contribution, not about the manifest having a UI block.
	for index, contribution := range m.Contributes {
		if contribution.Form == SlotFormIframe && (m.Provides.UI == nil || strings.TrimSpace(m.Provides.UI.Entry) == "") {
			errs.add(fmt.Sprintf("contributes[%d].entry", index),
				"form iframe requires provides.ui.entry (the HTML surface served at /plugins/<id>/)")
		}
	}
	if m.Provides.UI == nil {
		if validKind && m.Kind == KindWebview && len(m.Contributes) == 0 {
			errs.add("provides.ui", "kind webview requires provides.ui.entry or at least one contributes entry")
		}
		return
	}
	if err := validateRelPath(m.Provides.UI.Entry); err != nil {
		errs.add("provides.ui.entry", "%v", err)
	}
	if len([]rune(m.Provides.UI.Title)) > 128 {
		errs.add("provides.ui.title", "title must be at most 128 characters")
	}
}

// validateTelemetry checks the closed `provides.telemetry` vocabulary. The set
// is deliberately small: one zero-dependency exporter protocol and the three PI
// signals. A typo must fail at install, not silently advertise a capability the
// host cannot register.
func (m *Manifest) validateTelemetry(errs *fieldSet) {
	declaration := m.Provides.Telemetry
	if declaration == nil {
		return
	}
	if len(declaration.Exporters) == 0 {
		errs.add("provides.telemetry.exporters", "a telemetry plugin must declare at least one exporter")
	}
	exporters := map[string]bool{}
	for index, exporter := range declaration.Exporters {
		field := fmt.Sprintf("provides.telemetry.exporters[%d]", index)
		switch {
		case !manifestContains(TelemetryExporters(), exporter):
			errs.add(field, "exporter %q is not one of %s", exporter, strings.Join(TelemetryExporters(), "|"))
		case exporters[exporter]:
			errs.add(field, "exporter %q is declared twice", exporter)
		default:
			exporters[exporter] = true
		}
	}
	if len(declaration.Signals) == 0 {
		errs.add("provides.telemetry.signals", "a telemetry plugin must declare at least one signal")
	}
	signals := map[string]bool{}
	for index, signal := range declaration.Signals {
		field := fmt.Sprintf("provides.telemetry.signals[%d]", index)
		switch {
		case !manifestContains(TelemetrySignals(), signal):
			errs.add(field, "signal %q is not one of %s", signal, strings.Join(TelemetrySignals(), "|"))
		case signals[signal]:
			errs.add(field, "signal %q is declared twice", signal)
		default:
			signals[signal] = true
		}
	}
}

func manifestContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// validateSlots checks `provides.slots`: namespaced ids owned by this manifest,
// closed form/arity/target sets, unique values, and a data schema when the slot
// accepts data contributions.
func (m *Manifest) validateSlots(errs *fieldSet) {
	validateSlotList(errs, "provides.slots", m.ID, m.Provides.Slots)
}

// validateSlotList is the one slot validator shared by plugin.yaml and
// extensibility.json, so the two spellings cannot drift.
func validateSlotList(errs *fieldSet, fieldPrefix, ownerID string, slots []SlotDecl) {
	seen := map[string]bool{}
	for index, slot := range slots {
		prefix := fmt.Sprintf("%s[%d]", fieldPrefix, index)
		if !slotIDPattern.MatchString(slot.ID) || !strings.Contains(slot.ID, ".") {
			errs.add(prefix+".id", "slot id %q must be namespaced (<plugin>.<name>) and match %s", slot.ID, slotIDPattern.String())
		} else if ownerID != "" && !strings.HasPrefix(slot.ID, ownerID+".") {
			errs.add(prefix+".id", "slot id %q must carry the declaring plugin's namespace %q", slot.ID, ownerID+".")
		}
		if seen[slot.ID] {
			errs.add(prefix+".id", "slot id %q is declared twice", slot.ID)
		}
		seen[slot.ID] = true

		if len(slot.Forms) == 0 {
			errs.add(prefix+".forms", "a slot must declare at least one form")
		}
		forms := map[SlotForm]bool{}
		for formIndex, form := range slot.Forms {
			if !validSlotForm(form) {
				errs.add(fmt.Sprintf("%s.forms[%d]", prefix, formIndex), "form %q is not one of chunk|package|iframe|data", form)
				continue
			}
			if forms[form] {
				errs.add(fmt.Sprintf("%s.forms[%d]", prefix, formIndex), "form %q is declared twice", form)
			}
			forms[form] = true
		}
		if !validSlotArity(slot.Arity) {
			errs.add(prefix+".arity", "arity %q is not one of single|multi|ordered", slot.Arity)
		}
		if len(slot.Targets) == 0 {
			errs.add(prefix+".targets", "a slot must declare at least one target")
		}
		targets := map[SlotTarget]bool{}
		for targetIndex, target := range slot.Targets {
			if !validSlotTarget(target) {
				errs.add(fmt.Sprintf("%s.targets[%d]", prefix, targetIndex), "target %q is not one of web|native|tui", target)
				continue
			}
			if targets[target] {
				errs.add(fmt.Sprintf("%s.targets[%d]", prefix, targetIndex), "target %q is declared twice", target)
			}
			targets[target] = true
		}
		// A target no accepted form can render is a broken slot: the TUI, for
		// example, has no graphics surface, so a slot that only accepts
		// iframe/package cannot be TUI-only.
		for target := range targets {
			renderable := false
			for form := range forms {
				if formSupportsTarget(form, target) {
					renderable = true
					break
				}
			}
			if !renderable {
				errs.add(prefix+".targets", "no declared form can render on target %q (package/iframe need web or native)", target)
			}
		}
		if forms[SlotFormData] && len(slot.Schema) == 0 {
			errs.add(prefix+".schema", "a slot accepting data contributions must declare its JSON schema")
		}
	}
}

// validateBackends checks `provides.backends` syntax and, when the host supplies
// the action catalog, that the action exists.
func (m *Manifest) validateBackends(errs *fieldSet, env Environment) {
	seen := map[string]bool{}
	for index, backend := range m.Provides.Backends {
		prefix := fmt.Sprintf("provides.backends[%d]", index)
		valid := true
		switch {
		case strings.TrimSpace(backend.Tool) == "":
			errs.add(prefix+".tool", "tool must not be empty")
			valid = false
		case !backendNamePattern.MatchString(backend.Tool):
			errs.add(prefix+".tool", "tool %q must match %s", backend.Tool, backendNamePattern.String())
			valid = false
		}
		switch {
		case strings.TrimSpace(backend.Action) == "":
			errs.add(prefix+".action", "action must not be empty")
			valid = false
		case !backendNamePattern.MatchString(backend.Action):
			errs.add(prefix+".action", "action %q must match %s", backend.Action, backendNamePattern.String())
			valid = false
		}
		if strings.TrimSpace(backend.Impl) == "" {
			errs.add(prefix+".impl", "impl must not be empty")
			valid = false
		}
		key := backend.Tool + "." + backend.Action
		if seen[key] {
			errs.add(prefix, "backend %s is declared twice", key)
		}
		seen[key] = true
		if valid && env.KnownBackend != nil && !env.KnownBackend(backend.Tool, backend.Action) {
			errs.add(prefix+".action", "%s.%s is not an existing action of the aggregate tool", backend.Tool, backend.Action)
		}
	}
}

// validateContributions checks `contributes`: namespaced target slots, closed
// forms, form-specific entry/payload shape, package-relative entries and — when
// the host supplies the slot catalog — that the target exists and accepts the
// form.
func (m *Manifest) validateContributions(errs *fieldSet, env Environment) {
	selfSlots := map[string]SlotDecl{}
	for _, slot := range m.Provides.Slots {
		selfSlots[slot.ID] = slot
	}
	validateContributionList(errs, "contributes", selfSlots, env, m.Contributes, false)
}

// validateContributionList is the one contribution validator shared by
// plugin.yaml and extensibility.json. claimsAllowed says whether the source may
// carry `part`/`mime`: the machine-readable extensibility.json owns attachment
// claims, plugin.yaml does not (change keel-plugin-attachment).
func validateContributionList(errs *fieldSet, fieldPrefix string, selfSlots map[string]SlotDecl, env Environment, contributions []Contribution, claimsAllowed bool) {
	seen := map[string]bool{}
	claimedMime := map[string]string{}
	for index, contribution := range contributions {
		prefix := fmt.Sprintf("%s[%d]", fieldPrefix, index)
		validSlot := false
		switch {
		case strings.TrimSpace(contribution.Slot) == "":
			errs.add(prefix+".slot", "slot must not be empty")
		case !slotIDPattern.MatchString(contribution.Slot) || !strings.Contains(contribution.Slot, "."):
			errs.add(prefix+".slot", "slot %q must be a namespaced slot id (<plugin>.<name>)", contribution.Slot)
		default:
			validSlot = true
		}
		if !claimsAllowed && (strings.TrimSpace(contribution.Part) != "" || len(contribution.Mime) > 0) {
			if strings.TrimSpace(contribution.Part) != "" {
				errs.add(prefix+".part", "part is an extensibility.json claim; declare it in %s, not %s", ExtensibilityFileName, PluginFileName)
			}
			if len(contribution.Mime) > 0 {
				errs.add(prefix+".mime", "mime is an extensibility.json claim; declare it in %s, not %s", ExtensibilityFileName, PluginFileName)
			}
		} else if claimsAllowed {
			for _, problem := range AttachmentClaimProblems(contribution.Slot, contribution.Part, contribution.Mime) {
				errs.add(prefix+"."+problem.Field, "%s", problem.Message)
			}
			// The same plugin may not claim one MIME pattern twice: the host
			// picks the first matching contribution, so a duplicate would make
			// declaration order the only difference between two identical claims.
			for mimeIndex, pattern := range contribution.Mime {
				if first, exists := claimedMime[pattern]; exists {
					errs.add(fmt.Sprintf("%s.mime[%d]", prefix, mimeIndex),
						"MIME pattern %q is already claimed by %s", pattern, first)
					continue
				}
				claimedMime[pattern] = fmt.Sprintf("%s.mime[%d]", prefix, mimeIndex)
			}
		}
		if !validSlotForm(contribution.Form) {
			errs.add(prefix+".form", "form %q is not one of chunk|package|iframe|data", contribution.Form)
			continue
		}
		switch contribution.Form {
		case SlotFormChunk, SlotFormPackage, SlotFormIframe:
			if err := validateRelPath(contribution.Entry); err != nil {
				errs.add(prefix+".entry", "form %s: %v", contribution.Form, err)
			}
			if len(contribution.Payload) > 0 {
				errs.add(prefix+".payload", "form %s carries an entry, not a payload", contribution.Form)
			}
		case SlotFormData:
			if strings.TrimSpace(contribution.Entry) != "" {
				errs.add(prefix+".entry", "form data carries a payload, not an entry")
			}
			if len(contribution.Payload) == 0 {
				errs.add(prefix+".payload", "form data requires a non-empty payload")
			}
		}
		key := contribution.Slot + "|" + string(contribution.Form) + "|" + contribution.Entry
		if seen[key] {
			errs.add(prefix, "the same contribution to %s is declared twice", contribution.Slot)
		}
		seen[key] = true

		if !validSlot {
			continue
		}
		if slot, declared := selfSlots[contribution.Slot]; declared {
			if !slotAcceptsForm(slot, contribution.Form) {
				errs.add(prefix+".form", "slot %q accepts only %s, not %q",
					contribution.Slot, formList(slot.Forms), contribution.Form)
			}
			continue
		}
		if env.KnownSlot != nil {
			slot, known := env.KnownSlot(contribution.Slot)
			if !known {
				errs.add(prefix+".slot", "slot %q is not declared by any installed plugin", contribution.Slot)
				continue
			}
			if !slotAcceptsForm(slot, contribution.Form) {
				errs.add(prefix+".form", "slot %q accepts only %s, not %q",
					contribution.Slot, formList(slot.Forms), contribution.Form)
			}
		}
	}
}

// validateExtensibility checks the document pointer and the official-package
// gate. File existence is checked by validateTree, which has the package root.
func (m *Manifest) validateExtensibility(errs *fieldSet, env Environment) {
	ext := m.Extensibility
	if ext == nil {
		if env.RequireExtensibility {
			errs.add("extensibility", "official plugins MUST ship extensibility.doc (EXTENSIBILITY.md) and extensibility.schema (extensibility.json)")
		}
		return
	}
	if strings.TrimSpace(ext.Doc) == "" {
		errs.add("extensibility.doc", "extensibility.doc must name the human-readable document (EXTENSIBILITY.md)")
	} else if err := validateRelPath(ext.Doc); err != nil {
		errs.add("extensibility.doc", "%v", err)
	}
	if strings.TrimSpace(ext.Schema) == "" {
		errs.add("extensibility.schema", "extensibility.schema must name the machine-readable document (extensibility.json)")
	} else if err := validateRelPath(ext.Schema); err != nil {
		errs.add("extensibility.schema", "%v", err)
	}
}

func validSlotForm(form SlotForm) bool {
	for _, allowed := range SlotForms() {
		if form == allowed {
			return true
		}
	}
	return false
}

func validSlotArity(arity SlotArity) bool {
	for _, allowed := range SlotArities() {
		if arity == allowed {
			return true
		}
	}
	return false
}

func validSlotTarget(target SlotTarget) bool {
	for _, allowed := range SlotTargets() {
		if target == allowed {
			return true
		}
	}
	return false
}

func slotAcceptsForm(slot SlotDecl, form SlotForm) bool {
	for _, allowed := range slot.Forms {
		if allowed == form {
			return true
		}
	}
	return false
}

func formList(forms []SlotForm) string {
	names := make([]string, 0, len(forms))
	for _, form := range forms {
		names = append(names, string(form))
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, "|")
}

// providerDeclarationFieldNames maps the domain field names of a provider type
// declaration onto the manifest's snake_case names, so a field error names the
// key the plugin author wrote.
var providerDeclarationFieldNames = map[string]string{
	"typeId":       "type_id",
	"displayName":  "display_name",
	"pluginId":     "plugin_id",
	"configSchema": "config_schema",
	"authSchema":   "auth_schema",
	"modelSchema":  "model_schema",
	"featurePorts": "feature_ports",
	"surface":      "surface",
	// The surface block's nested fields use camelCase in the domain and
	// snake_case in a plugin manifest; the exact paths keep a nested error
	// naming the key the author wrote.
	"surface.surfaceKind":                        "surface.surface_kind",
	"surface.realtime.maxConcurrentSessions":     "surface.realtime.max_concurrent_sessions",
	"surface.realtime.maxSessionDurationSeconds": "surface.realtime.max_session_duration_seconds",
}

// manifestFieldPath renders a provider declaration field error path in the
// manifest's own spelling ("provides.provider_types[0].config_schema").
func manifestFieldPath(prefix, domainField string) string {
	if domainField == "" {
		return prefix
	}
	for camel, snake := range providerDeclarationFieldNames {
		switch {
		case domainField == camel:
			return prefix + "." + snake
		case strings.HasPrefix(domainField, camel+"."), strings.HasPrefix(domainField, camel+"["):
			return prefix + "." + snake + domainField[len(camel):]
		}
	}
	return prefix + "." + domainField
}

// validateProviderProvides checks the provider extension surface:
// `provides.provider_types`, `provides.provider` and `provides.feature_ports`.
// A manifest that writes these fields to the spec must be accepted by
// KnownFields(true), and a declaration that could not be assembled must fail
// here with the field that is wrong.
func (m *Manifest) validateProviderProvides(errs *fieldSet) {
	for index, declaration := range m.Provides.ProviderTypes {
		prefix := fmt.Sprintf("provides.provider_types[%d]", index)
		if err := declaration.ValidateDeclaration(); err != nil {
			for _, failure := range modelprovider.FieldErrors(err) {
				errs.add(manifestFieldPath(prefix, failure.Field), "%s", failure.Message)
			}
		}
		if declaration.PluginID != "" && m.ID != "" && declaration.PluginID != m.ID {
			errs.add(
				prefix+".plugin_id",
				"provider type %q names plugin %q but is declared by this manifest (%q); a type declared here is owned by the manifest's plugin",
				declaration.TypeID, declaration.PluginID, m.ID,
			)
		}
	}

	seenPorts := map[string]bool{}
	for index, port := range m.Provides.FeaturePorts {
		field := fmt.Sprintf("provides.feature_ports[%d]", index)
		switch {
		case strings.TrimSpace(port) == "":
			errs.add(field, "feature port must not be empty")
		case !modelprovider.ValidFeaturePort(port):
			errs.add(field, "feature port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'", port)
		case seenPorts[port]:
			errs.add(field, "feature port %q is declared twice", port)
		default:
			seenPorts[port] = true
		}
	}

	declaration := m.Provides.Provider
	switch {
	case declaration == nil:
		if len(m.Provides.ProviderTypes) > 0 {
			errs.add("provides.provider", "a manifest declaring provides.provider_types must also declare provides.provider with layer: provider")
		}
		return
	case declaration.Layer != ProviderPluginLayer:
		errs.add("provides.provider.layer", "provider plugin layer must be %q, got %q", ProviderPluginLayer, declaration.Layer)
	}
	if len(m.Provides.ProviderTypes) == 0 {
		errs.add("provides.provider_types", "a provider plugin must declare at least one provider type")
	}
	seenProviderPorts := map[string]bool{}
	for index, port := range declaration.FeaturePorts {
		field := fmt.Sprintf("provides.provider.feature_ports[%d]", index)
		switch {
		case strings.TrimSpace(port) == "":
			errs.add(field, "feature port must not be empty")
		case !modelprovider.ValidFeaturePort(port):
			errs.add(field, "feature port %q must be lowercase, start with a letter and contain only letters, digits, '.', '_', ':' or '-'", port)
		case seenProviderPorts[port]:
			errs.add(field, "feature port %q is declared twice", port)
		default:
			seenProviderPorts[port] = true
		}
	}
}

// validateAgentProvides checks the agent/team extension surface
// (`provides.agents`, `provides.agent_teams`): declared ids follow the manifest
// name grammar and are unique, so an install cannot contribute one agent twice.
func (m *Manifest) validateAgentProvides(errs *fieldSet) {
	validateDeclaredNames := func(field string, names []string) {
		seen := map[string]bool{}
		for index, name := range names {
			path := fmt.Sprintf("%s[%d]", field, index)
			switch {
			case strings.TrimSpace(name) == "":
				errs.add(path, "name must not be empty")
			case !namePattern.MatchString(name):
				errs.add(path, "name %q must match %s", name, namePattern.String())
			case seen[name]:
				errs.add(path, "name %q is declared twice", name)
			default:
				seen[name] = true
			}
		}
	}
	validateDeclaredNames("provides.agents", m.Provides.Agents)
	validateDeclaredNames("provides.agent_teams", m.Provides.AgentTeams)
}

// ProviderTypes returns the declared provider types with the declaring plugin
// filled in, ready for assembly against the plugin's SchemaSet. The result is a
// copy: editing it cannot edit the manifest.
func (m *Manifest) ProviderTypes() []modelprovider.ProviderType {
	if len(m.Provides.ProviderTypes) == 0 {
		return nil
	}
	types := make([]modelprovider.ProviderType, len(m.Provides.ProviderTypes))
	for index, declaration := range m.Provides.ProviderTypes {
		types[index] = declaration.Normalized()
		if types[index].PluginID == "" {
			types[index].PluginID = m.ID
		}
	}
	return types
}

// validateRelPath refuses absolute paths and any ".." segment: a UI entry may
// only point inside its own package.
func validateRelPath(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) {
		return fmt.Errorf("path %q must be relative to the plugin package", value)
	}
	if strings.Contains(value, `\`) {
		return fmt.Errorf("path %q must use forward slashes", value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return fmt.Errorf("path %q must not escape the plugin package", value)
		}
	}
	return nil
}

func (m *Manifest) validateRequires(errs *fieldSet, validID bool) {
	seen := map[string]bool{}
	for i, id := range m.Requires {
		field := fmt.Sprintf("requires[%d]", i)
		switch {
		case strings.TrimSpace(id) == "":
			errs.add(field, "requirement must not be empty")
		case !namePattern.MatchString(id):
			errs.add(field, "requirement %q must match %s", id, namePattern.String())
		case seen[id]:
			errs.add(field, "requirement %q is declared twice", id)
		case validID && id == m.ID:
			errs.add(field, "a plugin cannot require itself")
		default:
			seen[id] = true
		}
	}
}

func (m *Manifest) validatePermissions(errs *fieldSet) {
	permissions := m.Permissions
	for i, value := range permissions.FS {
		field := fmt.Sprintf("permissions.fs[%d]", i)
		if !namePattern.MatchString(value) {
			errs.add(field, "fs permission %q must match %s", value, namePattern.String())
		}
	}
	for i, value := range permissions.Net {
		field := fmt.Sprintf("permissions.net[%d]", i)
		if !hostPattern.MatchString(value) {
			errs.add(field, "net permission %q is not a host (host or host:port)", value)
		}
	}
	for i, value := range permissions.Secrets {
		field := fmt.Sprintf("permissions.secrets[%d]", i)
		if !secretPattern.MatchString(value) {
			errs.add(field, "secret permission %q must match %s", value, secretPattern.String())
		}
	}
	for i, value := range permissions.Tools {
		field := fmt.Sprintf("permissions.tools[%d]", i)
		if !namePattern.MatchString(value) {
			errs.add(field, "tool permission %q must match %s", value, namePattern.String())
		}
	}
}

func (m *Manifest) validateEntry(errs *fieldSet, validKind bool) {
	entry := m.Entry
	for i, part := range entry.Command {
		if strings.TrimSpace(part) == "" {
			errs.add(fmt.Sprintf("entry.command[%d]", i), "command element must not be empty")
		}
	}
	for i, name := range entry.Env {
		if !envPattern.MatchString(name) {
			errs.add(fmt.Sprintf("entry.env[%d]", i), "environment variable %q must match %s", name, envPattern.String())
		}
	}
	if !validKind {
		return
	}
	switch m.Kind {
	case KindInProcess:
		if len(entry.Command) > 0 {
			errs.add("entry.command", "kind in-process runs in the host process: entry.command is not allowed")
		}
	case KindSidecar:
		if len(entry.Command) == 0 {
			errs.add("entry.command", "kind sidecar requires entry.command")
		}
	case KindWebview:
		// A webview package may declare an optional sidecar backend; nothing to
		// require beyond provides.ui.entry (checked in validateProvides).
	}
}

func (m *Manifest) validateSource(errs *fieldSet) {
	if m.Source.Type == "" {
		return
	}
	valid := false
	for _, allowed := range SourceTypes() {
		if m.Source.Type == allowed {
			valid = true
			break
		}
	}
	if !valid {
		errs.add("source.type", "source.type %q must be one of %s", m.Source.Type, sourceTypeList())
		return
	}
	switch m.Source.Type {
	case SourceGit:
		if strings.TrimSpace(m.Source.URL) == "" {
			errs.add("source.url", "source.type git requires source.url")
		}
	case SourceNPM:
		if strings.TrimSpace(m.Source.Package) == "" {
			errs.add("source.package", "source.type npm requires source.package")
		}
	}
}

func sourceTypeList() string {
	names := make([]string, 0, len(SourceTypes()))
	for _, source := range SourceTypes() {
		names = append(names, string(source))
	}
	return strings.Join(names, "|")
}

// ValidatePluginID validates the id grammar of a plugin id. The API layer uses
// it so the grammar has exactly one source.
func ValidatePluginID(value string) bool { return idPattern.MatchString(value) }

// MaxPackageBytes bounds a copied plugin package (the whole tree) so an
// install cannot fill the host's disk.
const MaxPackageBytes = 64 << 20

// MaxPackageFileBytes bounds one file inside a package.
const MaxPackageFileBytes = 16 << 20

// describeSize renders a byte count for a field error.
func describeSize(bytes int64) string { return strconv.FormatInt(bytes, 10) + " bytes" }
