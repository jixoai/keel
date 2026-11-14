package pluginpkg

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Danger is the risk class of one CLI capability: what a caller must expect
// when it runs the command. The closed set is the spec's read|write|destructive
// (openspec/changes/keel-agent-team-v1, plugin-capabilities).
type Danger string

const (
	// DangerRead has no side effect: safe to retry and safe to hand to an agent
	// without a confirmation turn.
	DangerRead Danger = "read"
	// DangerWrite mutates state but is recoverable (create/update/start/stop).
	DangerWrite Danger = "write"
	// DangerDestructive removes or overwrites data (delete/remove/purge).
	DangerDestructive Danger = "destructive"
)

// Dangers is the closed set of danger levels.
func Dangers() []Danger { return []Danger{DangerRead, DangerWrite, DangerDestructive} }

// ParseDanger validates a danger string.
func ParseDanger(raw string) (Danger, bool) {
	for _, danger := range Dangers() {
		if string(danger) == raw {
			return danger, true
		}
	}
	return "", false
}

// RequiresConfirmation reports whether running the capability MUST ask first:
// the spec's "写/破坏性能力 MUST 有显式确认语义". The list/help surface uses it to
// render the confirmation hint, and the dispatcher uses it to refuse an
// unattended call.
func (d Danger) RequiresConfirmation() bool {
	return d == DangerWrite || d == DangerDestructive
}

// Caller is one surface allowed to invoke a capability (`callable_by`). The
// closed set is ui|agent|team_captain: the human surfaces, an agent's own tool
// face and a team captain's team CLI.
type Caller string

const (
	// CallerUI is a human surface: the CLI a person runs and the settings page.
	CallerUI Caller = "ui"
	// CallerAgent is an agent's tool/CLI face.
	CallerAgent Caller = "agent"
	// CallerTeamCaptain is an agent acting as a team captain, whose team CLI can
	// be switched off per team (`teamCli.enabled`).
	CallerTeamCaptain Caller = "team_captain"
)

// Callers is the closed set of callable_by values.
func Callers() []Caller { return []Caller{CallerUI, CallerAgent, CallerTeamCaptain} }

// ParseCaller validates a callable_by string.
func ParseCaller(raw string) (Caller, bool) {
	for _, caller := range Callers() {
		if string(caller) == raw {
			return caller, true
		}
	}
	return "", false
}

// maxCLISummaryRunes bounds the one-line summary (listings and --help).
const maxCLISummaryRunes = 200

// maxCLIDescriptionRunes bounds the longer purpose/boundary text.
const maxCLIDescriptionRunes = 2048

// maxCLIExampleRunes bounds one human-readable example.
const maxCLIExampleRunes = 512

// CLICapability is one node of a plugin's `provides.cli` tree. It is the single
// source of truth shared by `--help`, the settings page, creation mode and the
// agent tool face: none of them may invent a command or a permission the
// manifest does not declare.
//
// Spec: openspec/changes/keel-agent-team-v1/specs/plugin-capabilities/spec.md
// (Requirement: 每个插件声明能力树).
type CLICapability struct {
	// Path is the stable command path below `keel`: `[model provider list]` is
	// `keel model provider list`. Segments are lower-kebab words.
	Path []string `yaml:"path" json:"path"`
	// Summary is the one human-readable line shown in listings and --help.
	Summary string `yaml:"summary" json:"summary"`
	// Description is the longer purpose and boundary text ("what it does not
	// do" belongs here too).
	Description string `yaml:"description" json:"description"`
	// ArgsSchema is the JSON Schema of the arguments (free-form document).
	ArgsSchema map[string]any `yaml:"args_schema,omitempty" json:"args_schema,omitempty"`
	// OutputSchema is the JSON Schema of the output (free-form document).
	OutputSchema map[string]any `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
	// Permissions lists the permissions required to invoke the capability.
	Permissions []string `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	// Danger is the risk class; required, one of read|write|destructive.
	Danger Danger `yaml:"danger" json:"danger"`
	// Idempotent reports whether repeating the same call is safe. A capability
	// that omits the field is treated as non-idempotent (the safe reading): the
	// absence of a declaration can never turn a retry into a second effect.
	Idempotent bool `yaml:"idempotent" json:"idempotent"`
	// CallableBy lists the surfaces allowed to invoke the capability; required,
	// at least one of ui|agent|team_captain. An empty list would be a command no
	// one can run, which the spec forbids ("不得留下死命令").
	CallableBy []Caller `yaml:"callable_by" json:"callable_by"`
	// Examples are human-readable invocations (the spec recommends at least
	// one).
	Examples []string `yaml:"examples,omitempty" json:"examples,omitempty"`
}

// Key is the canonical identity of a capability: its path segments joined by
// single spaces. The same string is what `keel` runs, so duplicate detection,
// lookup and --help cannot disagree about which command a node is.
func (c CLICapability) Key() string { return strings.Join(c.Path, " ") }

// Command is the full command head (`keel model provider list`). An empty path
// renders the bare `keel`, which validation rejects for a declared capability.
func (c CLICapability) Command() string {
	if len(c.Path) == 0 {
		return "keel"
	}
	return "keel " + c.Key()
}

// CallableByCaller reports whether caller may invoke the capability.
func (c CLICapability) CallableByCaller(caller Caller) bool {
	for _, allowed := range c.CallableBy {
		if allowed == caller {
			return true
		}
	}
	return false
}

// clone returns a copy that shares no mutable state with c, so a caller (a UI
// view, an API encoder) cannot edit the manifest through a returned copy.
func (c CLICapability) clone() CLICapability {
	copied := c
	copied.Path = append([]string(nil), c.Path...)
	copied.Permissions = append([]string(nil), c.Permissions...)
	copied.CallableBy = append([]Caller(nil), c.CallableBy...)
	copied.Examples = append([]string(nil), c.Examples...)
	copied.ArgsSchema = cloneSchema(c.ArgsSchema)
	copied.OutputSchema = cloneSchema(c.OutputSchema)
	return copied
}

// cloneSchema deep-copies a JSON Schema document.
func cloneSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	copied := make(map[string]any, len(schema))
	for key, value := range schema {
		copied[key] = cloneValue(value)
	}
	return copied
}

// cloneValue deep-copies one decoded JSON/YAML value.
func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneSchema(typed)
	case []any:
		copied := make([]any, len(typed))
		for i, item := range typed {
			copied[i] = cloneValue(item)
		}
		return copied
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// CLICapabilities returns every declared capability in stable command order
// (sorted by path), which is the order --help, --json and the settings page
// render. The result is a copy: editing it does not touch the manifest.
func (m *Manifest) CLICapabilities() []CLICapability {
	capabilities := make([]CLICapability, 0, len(m.Provides.CLI))
	for _, capability := range m.Provides.CLI {
		capabilities = append(capabilities, capability.clone())
	}
	sort.SliceStable(capabilities, func(i, j int) bool {
		return lessPath(capabilities[i].Path, capabilities[j].Path)
	})
	return capabilities
}

// FindCLICapability looks a capability up by its exact path (no aliases: the
// path is the command).
func (m *Manifest) FindCLICapability(path ...string) (CLICapability, bool) {
	for _, capability := range m.Provides.CLI {
		if samePath(capability.Path, path) {
			return capability.clone(), true
		}
	}
	return CLICapability{}, false
}

// CallableCLICapabilities returns the capabilities caller may invoke, in the
// same stable order as CLICapabilities. It is how an agent tool face or a team
// captain's CLI surface is computed: switching a caller off is a list that no
// longer contains the command, never a prompt that asks the model not to use
// it.
func (m *Manifest) CallableCLICapabilities(caller Caller) []CLICapability {
	callable := make([]CLICapability, 0, len(m.Provides.CLI))
	for _, capability := range m.CLICapabilities() {
		if capability.CallableByCaller(caller) {
			callable = append(callable, capability)
		}
	}
	return callable
}

// CLINode is one node of the rendered command tree. A node either groups
// deeper commands (Capability nil) or declares a capability, or both: the
// `keel model provider` group may itself be a command while `... list` is its
// sub-command.
type CLINode struct {
	// Segment is the path segment at this depth ("model").
	Segment string `json:"segment"`
	// Command is the command head at this depth ("keel model").
	Command string `json:"command"`
	// Capability is this path's declaration, when it is a command rather than
	// only a group.
	Capability *CLICapability `json:"capability,omitempty"`
	// Children are the deeper segments, sorted by segment.
	Children []*CLINode `json:"children,omitempty"`
}

// CLITree renders the declared paths as one tree below `keel`, for --help and
// the settings page. Validation rejects duplicate paths, so a node carries at
// most one capability; an empty path is skipped (it is a manifest error, not a
// renderable command).
func (m *Manifest) CLITree() []*CLINode {
	var roots []*CLINode
	index := map[string]*CLINode{}
	for _, capability := range m.CLICapabilities() {
		if len(capability.Path) == 0 {
			continue
		}
		nodes := &roots
		for depth, segment := range capability.Path {
			key := strings.Join(capability.Path[:depth+1], " ")
			node, exists := index[key]
			if !exists {
				node = &CLINode{Segment: segment, Command: "keel " + key}
				index[key] = node
				*nodes = append(*nodes, node)
			}
			if depth == len(capability.Path)-1 {
				declared := capability
				node.Capability = &declared
			}
			nodes = &node.Children
		}
	}
	sortCLINodes(roots)
	return roots
}

// sortCLINodes orders a tree level by segment so rendering is deterministic.
func sortCLINodes(nodes []*CLINode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Segment < nodes[j].Segment })
	for _, node := range nodes {
		sortCLINodes(node.Children)
	}
}

// lessPath orders two command paths lexicographically segment by segment; a
// shorter prefix sorts before the longer path below it.
func lessPath(left, right []string) bool {
	for i := 0; i < len(left) && i < len(right); i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return len(left) < len(right)
}

// samePath reports whether two command paths are identical.
func samePath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// cliSegmentPattern is the grammar of one command segment: a lower-kebab word.
// Command paths are stable interface, so casing and separators are fixed here
// rather than normalized later (a command that renders differently in --help
// and the settings page would be a second truth).
var cliSegmentPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func dangerList() string {
	names := make([]string, 0, len(Dangers()))
	for _, danger := range Dangers() {
		names = append(names, string(danger))
	}
	return strings.Join(names, "|")
}

func callerList() string {
	names := make([]string, 0, len(Callers()))
	for _, caller := range Callers() {
		names = append(names, string(caller))
	}
	return strings.Join(names, "|")
}

// validateCLI checks the `provides.cli` tree. Every failure names the exact
// node and element (`provides.cli[1].path[2]`, `provides.cli[0].callable_by[1]`)
// so an API can return a precise 400 and the settings page can highlight the
// field instead of failing the whole manifest generically.
func (m *Manifest) validateCLI(errs *fieldSet) {
	seenPaths := map[string]int{}
	for i := range m.Provides.CLI {
		capability := m.Provides.CLI[i]
		base := fmt.Sprintf("provides.cli[%d]", i)
		pathValid := validateCLIPath(errs, base, capability)
		if pathValid {
			key := capability.Key()
			if previous, exists := seenPaths[key]; exists {
				errs.add(base+".path", "path %q is already declared by provides.cli[%d]", capability.Command(), previous)
			} else {
				seenPaths[key] = i
			}
		}
		validateCLIText(errs, base, capability)
		validateCLISchemas(errs, base, capability)
		validateCLIPermissions(errs, base, capability)
		validateCLICallers(errs, base, capability)
		validateCLIExamples(errs, base, capability)
	}
}

// validateCLIPath checks the command path and reports whether it is usable for
// duplicate detection.
func validateCLIPath(errs *fieldSet, base string, capability CLICapability) bool {
	if len(capability.Path) == 0 {
		errs.add(base+".path", "path is required (the command path below keel, e.g. [model provider list])")
		return false
	}
	valid := true
	for i, segment := range capability.Path {
		field := fmt.Sprintf("%s.path[%d]", base, i)
		switch {
		case strings.TrimSpace(segment) == "":
			errs.add(field, "path segment must not be empty")
			valid = false
		case !cliSegmentPattern.MatchString(segment):
			errs.add(field, "path segment %q must match %s", segment, cliSegmentPattern.String())
			valid = false
		}
	}
	return valid
}

// validateCLIText checks summary, description and danger.
func validateCLIText(errs *fieldSet, base string, capability CLICapability) {
	switch {
	case strings.TrimSpace(capability.Summary) == "":
		errs.add(base+".summary", "summary is required")
	case len([]rune(capability.Summary)) > maxCLISummaryRunes:
		errs.add(base+".summary", "summary must be at most %d characters", maxCLISummaryRunes)
	}
	switch {
	case strings.TrimSpace(capability.Description) == "":
		errs.add(base+".description", "description is required")
	case len([]rune(capability.Description)) > maxCLIDescriptionRunes:
		errs.add(base+".description", "description must be at most %d characters", maxCLIDescriptionRunes)
	}
	switch capability.Danger {
	case "":
		errs.add(base+".danger", "danger is required (one of %s)", dangerList())
	case DangerRead, DangerWrite, DangerDestructive:
	default:
		errs.add(base+".danger", "danger %q is not one of %s", capability.Danger, dangerList())
	}
}

// validateCLISchemas checks the two schema slots the spec makes mandatory on
// every capability node: a command without an args/output contract could be
// published to --help and the agent tool face while nobody (the settings page,
// a model, a captain) knows what it takes or returns.
func validateCLISchemas(errs *fieldSet, base string, capability CLICapability) {
	if capability.ArgsSchema == nil {
		errs.add(base+".args_schema", "args_schema is required (the JSON Schema of the command arguments; use {\"type\":\"object\"} for no arguments)")
	}
	if capability.OutputSchema == nil {
		errs.add(base+".output_schema", "output_schema is required (the JSON Schema of the command output)")
	}
}

// validateCLIPermissions checks the declared permission names. At least one
// permission is required (the spec's `permissions` field is MUST), and the name
// grammar is the manifest-wide one, so a capability cannot require a permission
// the rest of the manifest could not express.
func validateCLIPermissions(errs *fieldSet, base string, capability CLICapability) {
	if len(capability.Permissions) == 0 {
		errs.add(base+".permissions", "permissions is required (at least one permission the caller must hold)")
		return
	}
	for i, permission := range capability.Permissions {
		field := fmt.Sprintf("%s.permissions[%d]", base, i)
		switch {
		case strings.TrimSpace(permission) == "":
			errs.add(field, "permission must not be empty")
		case !namePattern.MatchString(permission):
			errs.add(field, "permission %q must match %s", permission, namePattern.String())
		}
	}
}

// validateCLICallers checks callable_by: required, closed set, no duplicates.
func validateCLICallers(errs *fieldSet, base string, capability CLICapability) {
	if len(capability.CallableBy) == 0 {
		errs.add(base+".callable_by", "callable_by is required (at least one of %s)", callerList())
		return
	}
	seen := map[Caller]bool{}
	for i, caller := range capability.CallableBy {
		field := fmt.Sprintf("%s.callable_by[%d]", base, i)
		switch {
		case strings.TrimSpace(string(caller)) == "":
			errs.add(field, "callable_by entry must not be empty")
		case !isKnownCaller(caller):
			errs.add(field, "callable_by %q is not one of %s", caller, callerList())
		case seen[caller]:
			errs.add(field, "callable_by %q is declared twice", caller)
		default:
			seen[caller] = true
		}
	}
}

// isKnownCaller reports whether caller is in the closed callable_by set.
func isKnownCaller(caller Caller) bool {
	_, ok := ParseCaller(string(caller))
	return ok
}

// validateCLIExamples checks the human-readable examples when present.
func validateCLIExamples(errs *fieldSet, base string, capability CLICapability) {
	for i, example := range capability.Examples {
		field := fmt.Sprintf("%s.examples[%d]", base, i)
		switch {
		case strings.TrimSpace(example) == "":
			errs.add(field, "example must not be empty")
		case len([]rune(example)) > maxCLIExampleRunes:
			errs.add(field, "example must be at most %d characters", maxCLIExampleRunes)
		}
	}
}
