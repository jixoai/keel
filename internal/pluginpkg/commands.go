package pluginpkg

// Chat command declarations (`provides.commands`, change keel-plugin-commands).
//
// A command is the USER-intent face of an action: `/name` in the Composer or
// TUI. It is not a prompt template, not a CLI verb (`provides.cli`) and not a
// second execution primitive: every command MUST name a tool the same manifest
// already declares in `provides.tools`, and the host runs that tool through the
// approved plugin tool dispatch.
//
// This file owns the declaration shape and its field-level validation. The
// runtime registry (which plugin commands exist right now, and whether they are
// available) lives in internal/harnessext; the product routes live in
// internal/harnessapi.
//
// Spec: openspec/changes/keel-plugin-commands/specs/plugin-commands/spec.md
// (Requirement: 插件声明命令且不新增执行原语).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Command grammar and bounds. They are part of the install contract: a manifest
// outside them is refused with a field error instead of failing at execution.
const (
	// MaxCommandArgsSchemaBytes bounds one `args_schema` document. The schema is
	// rendered by clients and copied into every invocation; 16 KiB is many times
	// the largest form a command needs.
	MaxCommandArgsSchemaBytes = 16 << 10
	// MaxCommandSummaryRunes bounds the one-line listing summary.
	MaxCommandSummaryRunes = 200
	// MaxCommandDescriptionRunes bounds the longer description.
	MaxCommandDescriptionRunes = 2048
	// MaxCommandNameLength bounds the local command name.
	MaxCommandNameLength = 32
)

// commandNamePattern is the local command name grammar. The published name is
// `/<plugin-id>:<name>`; the id grammar cannot contain ':' and the local name
// cannot either, so the published spelling is unambiguous.
var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// defaultCommandArgsSchema is the schema a declaration without one carries: a
// command with no declared arguments takes an empty object.
var defaultCommandArgsSchema = map[string]any{"type": "object"}

// DefaultCommandTargets is the target set a command without explicit `targets`
// is available on. The value is returned as a copy by EffectiveTargets.
func DefaultCommandTargets() []SlotTarget {
	return []SlotTarget{SlotTargetWeb, SlotTargetNative, SlotTargetTUI}
}

// CommandDecl is one `provides.commands[]` entry.
type CommandDecl struct {
	// Name is the local command name (^[a-z][a-z0-9-]{0,31}$). The host
	// publishes it as `<plugin-id>:<name>`.
	Name string `yaml:"name" json:"name"`
	// Summary is the one-line listing text (1..200 characters).
	Summary string `yaml:"summary" json:"summary"`
	// Description is the longer purpose/boundary text (<=2048 characters).
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// ArgsSchema is the JSON object schema of the command's arguments. Empty
	// means `{"type":"object"}`.
	ArgsSchema map[string]any `yaml:"args_schema,omitempty" json:"args_schema,omitempty"`
	// Tool is the backend tool. It MUST be one of the same manifest's
	// provides.tools; a command cannot reach another plugin's or a host
	// aggregate tool.
	Tool string `yaml:"tool" json:"tool"`
	// Danger is read|write|destructive.
	Danger Danger `yaml:"danger" json:"danger"`
	// Idempotent declares that replaying the tool with the same arguments has no
	// additional effect. It defaults to false (the safe reading).
	Idempotent bool `yaml:"idempotent,omitempty" json:"idempotent"`
	// Targets are the client targets the command is offered on. Empty means the
	// full set (web, native, tui).
	Targets []SlotTarget `yaml:"targets,omitempty" json:"targets,omitempty"`
}

// EffectiveArgsSchema returns the schema the command is validated against. The
// default is returned as a fresh map so a caller cannot mutate the shared value.
func (c CommandDecl) EffectiveArgsSchema() map[string]any {
	if len(c.ArgsSchema) == 0 {
		return map[string]any{"type": "object"}
	}
	return c.ArgsSchema
}

// EffectiveTargets returns the declared targets or the default full set. The
// result is a copy.
func (c CommandDecl) EffectiveTargets() []SlotTarget {
	if len(c.Targets) == 0 {
		return DefaultCommandTargets()
	}
	return append([]SlotTarget(nil), c.Targets...)
}

// ArgsSchemaJSON renders the effective schema for the wire and hashing.
func (c CommandDecl) ArgsSchemaJSON() []byte {
	raw, err := json.Marshal(c.EffectiveArgsSchema())
	if err != nil {
		return []byte(`{"type":"object"}`)
	}
	return raw
}

// CommandPublishedName is the session-facing spelling of a plugin command:
// `<plugin-id>:<name>` (the client renders it with a leading '/'). The plugin id
// grammar excludes ':' and the local name cannot carry it, so the name is
// unambiguous.
func CommandPublishedName(pluginID, name string) string {
	return pluginID + ":" + name
}

// ParseCommandPublishedName splits a published plugin command name. It answers
// false for a name that does not carry exactly one non-empty plugin id and one
// non-empty local name.
func ParseCommandPublishedName(published string) (pluginID, name string, ok bool) {
	index := strings.IndexByte(published, ':')
	if index <= 0 || index == len(published)-1 {
		return "", "", false
	}
	pluginID, name = published[:index], published[index+1:]
	if !ValidatePluginID(pluginID) || !commandNamePattern.MatchString(name) {
		return "", "", false
	}
	return pluginID, name, true
}

// ReservedCommandNames are the builtin bare names a plugin MUST NOT declare as
// its local name. `skill:<name>` is a builtin family; the prefix is reserved
// too, even though the local-name grammar already excludes ':'.
//
// `learn` is reserved because it is a declared CLIENT prompt template, never a
// host command: registering it (under any spelling) would reintroduce the
// prompt-template-as-command confusion this contract forbids.
func ReservedCommandNames() []string {
	return []string{"compact", "learn", "skill"}
}

// ReservedCommandName reports whether name collides with a builtin command name
// or the `skill:` family. A plugin's local name is what is checked; the
// published name is always namespaced.
func ReservedCommandName(name string) bool {
	name = strings.TrimSpace(name)
	for _, reserved := range ReservedCommandNames() {
		if name == reserved {
			return true
		}
	}
	return strings.HasPrefix(name, "skill:")
}

// validateCommands checks `provides.commands`. declaredTools is the set of
// provides.tools entries that passed their own validation: a command may only
// bind a tool the same manifest declares.
func (m *Manifest) validateCommands(errs *fieldSet, declaredTools map[string]bool) {
	seen := map[string]bool{}
	for index, command := range m.Provides.Commands {
		prefix := fmt.Sprintf("provides.commands[%d]", index)
		name := strings.TrimSpace(command.Name)
		switch {
		case name == "":
			errs.add(prefix+".name", "name is required")
		case !commandNamePattern.MatchString(name):
			errs.add(prefix+".name", "name %q must match %s", command.Name, commandNamePattern.String())
		case ReservedCommandName(name):
			errs.add(prefix+".name", "name %q is reserved: builtin command names (compact, skill:<name>) and the client template learn cannot be re-declared by a plugin", name)
		case seen[name]:
			errs.add(prefix+".name", "command name %q is declared twice", name)
		}
		seen[name] = true

		summary := strings.TrimSpace(command.Summary)
		switch {
		case summary == "":
			errs.add(prefix+".summary", "summary is required")
		case len([]rune(command.Summary)) > MaxCommandSummaryRunes:
			errs.add(prefix+".summary", "summary must be at most %d characters", MaxCommandSummaryRunes)
		}
		if len([]rune(command.Description)) > MaxCommandDescriptionRunes {
			errs.add(prefix+".description", "description must be at most %d characters", MaxCommandDescriptionRunes)
		}

		tool := strings.TrimSpace(command.Tool)
		switch {
		case tool == "":
			errs.add(prefix+".tool", "tool is required: a command is a named entrance to a tool this plugin declares")
		case !declaredTools[tool]:
			errs.add(prefix+".tool", "tool %q is not declared by this manifest's provides.tools", tool)
		}

		if _, ok := ParseDanger(string(command.Danger)); !ok {
			errs.add(prefix+".danger", "danger %q is not one of %s", command.Danger, dangerList())
		}

		if len(command.ArgsSchema) > 0 {
			raw, err := json.Marshal(command.ArgsSchema)
			if err != nil {
				errs.add(prefix+".args_schema", "args_schema is not JSON: %v", err)
			} else {
				if len(raw) > MaxCommandArgsSchemaBytes {
					errs.add(prefix+".args_schema", "args_schema is %d bytes (limit %d)", len(raw), MaxCommandArgsSchemaBytes)
				}
				var schema struct {
					Type any `json:"type"`
				}
				if err := json.Unmarshal(raw, &schema); err != nil {
					errs.add(prefix+".args_schema", "args_schema is not a JSON object schema: %v", err)
				} else if kind, _ := schema.Type.(string); kind != "object" {
					errs.add(prefix+".args_schema", "args_schema must be a JSON object schema (top-level type: object)")
				}
			}
		}

		targets := map[SlotTarget]bool{}
		for targetIndex, target := range command.Targets {
			path := fmt.Sprintf("%s.targets[%d]", prefix, targetIndex)
			if !validSlotTarget(target) {
				errs.add(path, "target %q is not one of web|native|tui", target)
				continue
			}
			if targets[target] {
				errs.add(path, "target %q is declared twice", target)
			}
			targets[target] = true
		}
		if len(command.Targets) == 0 {
			// The default set is non-empty by construction; nothing to check.
			continue
		}
	}
}

// CommandByName returns the declaration with the given local name.
func (m *Manifest) CommandByName(name string) (CommandDecl, bool) {
	for _, command := range m.Provides.Commands {
		if command.Name == name {
			return command, true
		}
	}
	return CommandDecl{}, false
}

// PublishedCommand is one declared command and the names it is published under.
type PublishedCommand struct {
	// Decl is the manifest declaration.
	Decl CommandDecl `json:"decl"`
	// Local is the plugin's own name (the declaration's name).
	Local string `json:"local"`
	// Published is the session-facing name (`<plugin-id>:<local>`).
	Published string `json:"published"`
	// Tool is the local tool spelling the plugin itself speaks. DeclaredTool is
	// the `provides.tools` entry it binds.
	Tool         string `json:"tool"`
	DeclaredTool string `json:"declaredTool"`
	// PublishedTool is the model-facing/wire spelling of the backend tool.
	PublishedTool string `json:"publishedTool"`
}

// PublishedCommands maps every declaration onto its published command name and
// backend tool spellings. The validator, the runtime registry and the approval
// text share this one conversion.
func (m *Manifest) PublishedCommands() []PublishedCommand {
	if len(m.Provides.Commands) == 0 {
		return nil
	}
	tools := make(map[string]PublishedTool, len(m.Provides.Tools))
	for _, tool := range m.PublishedTools() {
		tools[tool.Declared] = tool
	}
	commands := make([]PublishedCommand, 0, len(m.Provides.Commands))
	for _, command := range m.Provides.Commands {
		published := PublishedCommand{
			Decl:         command,
			Local:        command.Name,
			Published:    CommandPublishedName(m.ID, command.Name),
			DeclaredTool: command.Tool,
			Tool:         command.Tool,
		}
		if tool, ok := tools[command.Tool]; ok {
			published.Tool = tool.Local
			published.PublishedTool = tool.Published
		}
		commands = append(commands, published)
	}
	return commands
}
