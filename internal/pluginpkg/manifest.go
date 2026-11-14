// Package pluginpkg is the plugin package contract: the `plugin.yaml` manifest,
// its validation rules (field-level errors), the CLI capability tree
// (`provides.cli`, see cli.go), package discovery under a plugins root, the
// version gate (`engines.keel`) and the content-addressed package store (tree
// hash + managed copy).
//
// The package never loads a plugin. Loading is the agentext plugin manager's
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
	// Description is one line shown in listings.
	Description string `yaml:"description" json:"description"`
	// Engines carries the host compatibility gate; a missing keel entry means
	// "compatible with any keel".
	Engines Engines `yaml:"engines,omitempty" json:"engines,omitempty"`
	// Provides declares what the plugin contributes.
	Provides Provides `yaml:"provides,omitempty" json:"provides,omitempty"`
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
	// UI is the webview resource declaration; required for kind webview.
	UI *UI `yaml:"ui,omitempty" json:"ui,omitempty"`
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
}

// idPattern is the spec's plugin id grammar.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)

// namePattern is the grammar shared by tool, trigger and requirement names:
// the id grammar plus ':' so capability ids (cap:foo) are expressible.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// hostPattern bounds a net permission entry to hostname-shaped text.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.*-]{0,126})(:[0-9]{1,5})?$`)

// secretPattern mirrors the secret-name grammar of the settings surface
// (internal/agentapi/settings.go): a manifest cannot reference a secret name
// that no store would accept.
var secretPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// envPattern is the grammar of an entry.env variable name.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

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
// already wire safe is returned verbatim: that compatibility rule is why
// publication normalizes instead of rejecting (`example_status` and
// `spawn_subagent` must never change).
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
	if m.Provides.UI == nil {
		if validKind && m.Kind == KindWebview {
			errs.add("provides.ui", "kind webview requires provides.ui.entry")
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
