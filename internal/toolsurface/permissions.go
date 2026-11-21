// Tool-surface permissions (change keel-bot-identity-access, A4.2b1).
//
// Every model tool call declares the permission key it needs. The mapping is
// the single in-code registry for the runtime tool surface: the eleven builtin
// aggregate tools, the media family and the decision tool. A tool published by
// an approved plugin is a plugin action and is gated by plugin.invoke; it must
// not be able to appear without any gate.
//
// The model-visible manifest (name/description/schema) is deliberately not
// touched: the permission map is an internal assembly artifact and adding a key
// there would change the manifest hash every run pins for provenance. The
// runtime reads this map in the per-call authorization seam; ValidateCatalog
// makes an unknown tool, an undeclared action or an unregistered key fail at
// assembly, before a run can see the surface.
package toolsurface

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/access"
)

// Media and decision tool names. They are built by internal/mediawiring and
// appended to the host registry as extra tools; the permission map names them
// here so they are never an unregistered surface.
const (
	ImageGenerate = "image_generate"
	VideoGenerate = "video_generate"
	SpeechTTS     = "speech_tts"
	SpeechSTT     = "speech_stt"
	DecisionAsk   = "decision_ask"
)

// ToolPermission is one (tool, action) -> required permission key declaration.
// Action is empty for a single-action tool (read/write/edit and the media
// family); it is one of the tool's closed action set for an aggregate tool.
type ToolPermission struct {
	Tool   string
	Action string
	Key    string
}

// toolPermissionCatalog is the runtime tool surface's permission map. It is
// ordered by tool for review; RequiredKey/Endpoints sort defensively.
//
// Mapping rationale:
//   - bash executes in the session's sandbox: shell.exec (session resource).
//   - read/write/edit operate on the session's layerfs view: file.read,
//     file.write. Files have no per-file relation tuples yet (A4.2b registers
//     GET /v1/files/{id}); the ambient session is the resource, so the tool
//     authorizer still evaluates the tenant boundary and the session relation
//     when the key is resource-scoped.
//   - mcp is the aggregate MCP ecosystem tool: mcp.use for list/describe/call
//     (the task's explicit mapping; mcp.read stays the settings/discovery key).
//   - skills splits the read projection (ls/view: skill.read) from the library
//     writes (manage/sources: skill.manage). Mapping manage to skill.read would
//     let a read-only grant mutate the tenant's skill library.
//   - tasks splits reads from writes: task.read / task.write (session resource).
//   - agent is delegation: bot.invoke on the target Bot (or the run's own Bot
//     when the call names no target). The authorizer resolves agentId/sessionId
//     to the target bot resource.
//   - approve asks a human: approval.read for an informational question
//     (prompt/choice), approval.write for a yes/no approval (confirm). Both are
//     session-scoped.
//   - notify push/ack write; list reads.
//   - creation installs/validates/scaffolds plugin packages: plugin.invoke.
//   - media tools spend the deployment's provider credentials: media.invoke.
//   - decision_ask spends the decision-model credential: decision.ask.
var toolPermissionCatalog = []ToolPermission{
	{Tool: Bash, Action: ActionRun, Key: access.KeyShellExec},
	{Tool: Bash, Action: ActionJobs, Key: access.KeyShellExec},

	{Tool: Read, Key: access.KeyFileRead},
	{Tool: Write, Key: access.KeyFileWrite},
	{Tool: Edit, Key: access.KeyFileWrite},

	{Tool: MCP, Action: ActionList, Key: access.KeyMCPUse},
	{Tool: MCP, Action: ActionDesc, Key: access.KeyMCPUse},
	{Tool: MCP, Action: ActionCall, Key: access.KeyMCPUse},

	{Tool: Skills, Action: ActionLs, Key: access.KeySkillRead},
	{Tool: Skills, Action: ActionView, Key: access.KeySkillRead},
	{Tool: Skills, Action: ActionManage, Key: access.KeySkillManage},
	{Tool: Skills, Action: ActionSources, Key: access.KeySkillManage},

	{Tool: Tasks, Action: ActionCreate, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionUpdate, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionAttach, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionDetach, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionClose, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionNote, Key: access.KeyTaskWrite},
	{Tool: Tasks, Action: ActionList, Key: access.KeyTaskRead},
	{Tool: Tasks, Action: ActionShow, Key: access.KeyTaskRead},

	{Tool: Agent, Action: ActionSpawn, Key: access.KeyBotInvoke},
	{Tool: Agent, Action: ActionFork, Key: access.KeyBotInvoke},
	{Tool: Agent, Action: ActionResume, Key: access.KeyBotInvoke},
	{Tool: Agent, Action: ActionCancel, Key: access.KeyBotInvoke},
	{Tool: Agent, Action: ActionList, Key: access.KeyBotInvoke},
	{Tool: Agent, Action: ActionStatus, Key: access.KeyBotInvoke},

	{Tool: Approve, Action: ActionConfirm, Key: access.KeyApprovalWrite},
	{Tool: Approve, Action: ActionPrompt, Key: access.KeyApprovalRead},
	{Tool: Approve, Action: ActionChoice, Key: access.KeyApprovalRead},

	{Tool: Notify, Action: ActionPush, Key: access.KeyNotificationWrite},
	{Tool: Notify, Action: ActionAck, Key: access.KeyNotificationWrite},
	{Tool: Notify, Action: ActionList, Key: access.KeyNotificationRead},

	{Tool: Creation, Action: ActionCatalog, Key: access.KeyPluginInvoke},
	{Tool: Creation, Action: ActionScaffold, Key: access.KeyPluginInvoke},
	{Tool: Creation, Action: ActionValidate, Key: access.KeyPluginInvoke},
	{Tool: Creation, Action: ActionInstall, Key: access.KeyPluginInvoke},
	{Tool: Creation, Action: ActionList, Key: access.KeyPluginInvoke},
	{Tool: Creation, Action: ActionRemove, Key: access.KeyPluginInvoke},

	{Tool: ImageGenerate, Key: access.KeyMediaInvoke},
	{Tool: VideoGenerate, Key: access.KeyMediaInvoke},
	{Tool: SpeechTTS, Key: access.KeyMediaInvoke},
	{Tool: SpeechSTT, Key: access.KeyMediaInvoke},
	{Tool: DecisionAsk, Key: access.KeyDecisionAsk},
}

// ToolPermissionManifest returns a copy of the runtime tool permission map.
func ToolPermissionManifest() []ToolPermission {
	return append([]ToolPermission(nil), toolPermissionCatalog...)
}

// PermissionEndpoints projects the map onto the coverage gate's endpoint
// vocabulary. Every non-public tool entrance declares its required key.
func PermissionEndpoints() []access.Endpoint {
	out := make([]access.Endpoint, 0, len(toolPermissionCatalog))
	for _, entry := range toolPermissionCatalog {
		name := entry.Tool
		if entry.Action != "" {
			name += "." + entry.Action
		}
		out = append(out, access.Endpoint{Kind: access.EndpointTool, Name: name, RequiredKey: entry.Key})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RequiredKey returns the key one tool call needs. An unknown action on a known
// aggregate tool falls back to the tool's base key, so the model still receives
// the tool's own unknown_action error instead of a misleading permission
// denial. A tool that is not in this map is a plugin-published tool: its key is
// plugin.invoke, the approval boundary that installed it.
func RequiredKey(tool, action string) (string, bool) {
	tool = strings.TrimSpace(tool)
	action = strings.TrimSpace(action)
	for _, entry := range toolPermissionCatalog {
		if entry.Tool == tool && entry.Action == action {
			return entry.Key, true
		}
	}
	if action != "" {
		for _, entry := range toolPermissionCatalog {
			if entry.Tool == tool && entry.Action == "" {
				return entry.Key, true
			}
		}
	}
	// A known tool always has a fail-closed default: an omitted or unknown
	// action still needs the tool's base authority before the tool can answer
	// with its own unknown_action error.
	if key, ok := defaultToolKeys()[tool]; ok {
		return key, true
	}
	return "", false
}

// ActionOf parses the aggregate tool's action field. A single-action tool (or
// a malformed argument object) has no action; its permission entry is the
// single-action one.
func ActionOf(tool string, args json.RawMessage) string {
	if !toolHasActions(tool) {
		return ""
	}
	var input struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return ""
	}
	return strings.TrimSpace(input.Action)
}

// toolHasActions reports whether tool is an aggregate tool with a non-empty
// closed action set (as opposed to a single-action tool).
func toolHasActions(tool string) bool {
	actions, ok := knownToolActions()[strings.TrimSpace(tool)]
	if !ok || len(actions) == 0 {
		return false
	}
	return !(len(actions) == 1 && actions[0] == "")
}

// defaultToolKeys maps each tool to its first declared key. The catalog order
// is deliberate: the first entry is the fail-closed default for an unknown
// action on an aggregate tool.
func defaultToolKeys() map[string]string {
	out := make(map[string]string, len(toolPermissionCatalog))
	for _, entry := range toolPermissionCatalog {
		if _, exists := out[entry.Tool]; !exists {
			out[entry.Tool] = entry.Key
		}
	}
	return out
}

// knownToolActions is the closed tool -> action vocabulary the permission map
// must cover. The action slices are the same values the aggregate tools
// dispatch on, so a new action without a permission declaration fails the
// assembly check instead of reaching the runtime unguarded.
func knownToolActions() map[string][]string {
	return map[string][]string{
		Bash:          append([]string(nil), bashTool.actions...),
		Read:          {""},
		Write:         {""},
		Edit:          {""},
		MCP:           append([]string(nil), mcpToolActions...),
		Skills:        append([]string(nil), skillsActions...),
		Tasks:         append([]string(nil), tasksActions...),
		Agent:         append([]string(nil), agentActions...),
		Approve:       append([]string(nil), approveActions...),
		Notify:        append([]string(nil), notifyActions...),
		Creation:      append([]string(nil), creationActions...),
		ImageGenerate: {""},
		VideoGenerate: {""},
		SpeechTTS:     {""},
		SpeechSTT:     {""},
		DecisionAsk:   {""},
	}
}

// ValidatePermissionCatalog checks the registry's invariants: every declared
// tool is a known tool, every action belongs to that tool's closed set, every
// key is in the access registry, and every known tool/action has exactly one
// declaration. It runs at host assembly; an unknown tool, an undeclared action
// or an unregistered key is a boot error, not a first-call surprise.
func ValidatePermissionCatalog() error {
	return validatePermissionCatalog(toolPermissionCatalog, knownToolActions())
}

// validatePermissionCatalog is the testable half: it accepts synthetic maps so
// the failure modes are exercised without mutating the real registry.
func validatePermissionCatalog(specs []ToolPermission, known map[string][]string) error {
	if len(specs) == 0 {
		return fmt.Errorf("tool permission catalog is empty")
	}
	declared := make(map[string]map[string]bool, len(known))
	seen := make(map[string]bool, len(specs))
	for index, spec := range specs {
		where := fmt.Sprintf("toolPermissionCatalog[%d]", index)
		tool := strings.TrimSpace(spec.Tool)
		action := strings.TrimSpace(spec.Action)
		switch {
		case tool == "":
			return fmt.Errorf("%s: tool is empty", where)
		case spec.Key == "":
			return fmt.Errorf("%s (%s): required key is empty", where, tool)
		case len(known[tool]) == 0:
			return fmt.Errorf("%s: tool %q is not a known runtime tool", where, tool)
		}
		if err := access.ValidateKey(spec.Key); err != nil {
			return fmt.Errorf("%s (%s): %w", where, tool, err)
		}
		allowed := false
		for _, candidate := range known[tool] {
			if candidate == action {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%s: tool %q has no action %q (known: %s)", where, tool, action, joinActions(known[tool]))
		}
		id := tool + "\x00" + action
		if seen[id] {
			return fmt.Errorf("%s: duplicate declaration for %s", where, endpointName(tool, action))
		}
		seen[id] = true
		if declared[tool] == nil {
			declared[tool] = make(map[string]bool, len(known[tool]))
		}
		declared[tool][action] = true
	}
	for tool, actions := range known {
		for _, action := range actions {
			if !declared[tool][action] {
				return fmt.Errorf("tool %q action %q has no permission declaration", tool, endpointName(tool, action))
			}
		}
	}
	return nil
}

func endpointName(tool, action string) string {
	if action == "" {
		return tool
	}
	return tool + "." + action
}

func joinActions(actions []string) string {
	quoted := make([]string, 0, len(actions))
	for _, action := range actions {
		if action == "" {
			quoted = append(quoted, `""`)
			continue
		}
		quoted = append(quoted, action)
	}
	return strings.Join(quoted, ", ")
}
