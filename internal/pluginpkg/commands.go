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

// ReservedPluginIDs are plugin ids whose published command namespace would
// collide with a builtin family. `skill` is reserved: its commands would be
// published as `skill:<name>`, exactly the builtin `/skill:<name>` spelling.
func ReservedPluginIDs() []string { return []string{"skill"} }

// ReservedPluginID reports whether a plugin id is reserved by the command
// contract.
func ReservedPluginID(id string) bool {
	for _, reserved := range ReservedPluginIDs() {
		if id == reserved {
			return true
		}
	}
	return false
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
		case command.Name != name:
			errs.add(prefix+".name", "name must not carry leading or trailing whitespace")
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
		case command.Tool != tool:
			errs.add(prefix+".tool", "tool must not carry leading or trailing whitespace")
		case !declaredTools[tool]:
			errs.add(prefix+".tool", "tool %q is not declared by this manifest's provides.tools", tool)
		}

		if _, ok := ParseDanger(string(command.Danger)); !ok {
			errs.add(prefix+".danger", "danger %q is not one of %s", command.Danger, dangerList())
		}

		if command.ArgsSchema != nil && len(command.ArgsSchema) == 0 {
			errs.add(prefix+".args_schema", "args_schema must declare a JSON object schema (top-level type: object)")
		} else if len(command.ArgsSchema) > 0 {
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
				} else {
					validateCommandSchemaKeywords(errs, prefix+".args_schema", command.ArgsSchema, 0)
				}
			}
		}

		if command.Targets != nil && len(command.Targets) == 0 {
			errs.add(prefix+".targets", "targets must not be empty when declared; omit the field for web|native|tui")
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

// commandSchemaKeywords is the closed set of JSON-schema keywords a command
// args schema may use. The execution validator implements every one of them;
// anything else (oneOf/anyOf/allOf/$ref/format/const/...) is refused at install
// instead of being silently ignored.
var commandSchemaKeywords = map[string]bool{
	"type": true, "description": true, "title": true, "default": true,
	"enum": true, "examples": true,
	"properties": true, "required": true, "additionalProperties": true,
	"items": true, "minItems": true, "maxItems": true, "uniqueItems": true,
	"minLength": true, "maxLength": true, "pattern": true,
	"minimum": true, "maximum": true,
}

// commandSchemaTypes is the closed set of JSON types a command schema may name.
var commandSchemaTypes = map[string]bool{
	"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true,
}

// validateCommandSchemaKeywords walks the schema recursively and rejects an
// unsupported keyword or a keyword value that the runtime cannot enforce. The
// field path is the manifest's own snake_case path
// (provides.commands[0].args_schema.properties.path.pattern).
func validateCommandSchemaKeywords(errs *fieldSet, prefix string, schema map[string]any, depth int) {
	if depth > 8 {
		errs.add(prefix, "args_schema is nested too deeply")
		return
	}
	var properties map[string]any
	for keyword, value := range schema {
		path := prefix + "." + keyword
		if !commandSchemaKeywords[keyword] {
			errs.add(path, "keyword %q is not supported by command args schemas", keyword)
			continue
		}
		switch keyword {
		case "type":
			kind, ok := value.(string)
			if !ok || !commandSchemaTypes[kind] {
				errs.add(path, "type must be one of object|array|string|number|integer|boolean")
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				errs.add(path, "properties must be an object")
				continue
			}
			properties = props
			for name, raw := range props {
				property, ok := raw.(map[string]any)
				if !ok {
					errs.add(path+"."+name, "a property schema must be an object")
					continue
				}
				validateCommandSchemaKeywords(errs, path+"."+name, property, depth+1)
			}
		case "required":
			names := schemaStringList(value)
			if names == nil {
				errs.add(path, "required must be an array of strings")
			}
		case "items":
			item, ok := value.(map[string]any)
			if !ok {
				errs.add(path, "items must be an object schema")
				continue
			}
			validateCommandSchemaKeywords(errs, path, item, depth+1)
		case "additionalProperties":
			allowed, ok := value.(bool)
			if !ok {
				errs.add(path, "additionalProperties must be a boolean")
			} else if allowed {
				errs.add(path, "additionalProperties: true is not supported: undeclared command arguments are always refused")
			}
		case "pattern":
			pattern, ok := value.(string)
			if !ok {
				errs.add(path, "pattern must be a string")
				continue
			}
			if _, err := regexp.Compile(pattern); err != nil {
				errs.add(path, "pattern is not a valid regular expression: %v", err)
			}
		case "minLength", "maxLength", "minItems", "maxItems":
			if number, ok := schemaInteger(value); !ok || number < 0 {
				errs.add(path, "%s must be a non-negative integer", keyword)
			}
		case "minimum", "maximum":
			if _, ok := schemaNumber(value); !ok {
				errs.add(path, "%s must be a number", keyword)
			}
		case "uniqueItems":
			if _, ok := value.(bool); !ok {
				errs.add(path, "uniqueItems must be a boolean")
			}
		case "enum":
			if _, ok := value.([]any); !ok {
				errs.add(path, "enum must be an array")
			}
		}
	}
	if names := schemaStringList(schema["required"]); len(names) > 0 {
		for _, name := range names {
			if _, declared := properties[name]; !declared {
				errs.add(prefix+".required", "required argument %q is not declared in properties", name)
			}
		}
	}
}

func schemaStringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			name, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, name)
		}
		return out
	}
	return nil
}

func schemaInteger(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func schemaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	}
	return 0, false
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
