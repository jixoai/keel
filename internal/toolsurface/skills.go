package toolsurface

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/skills"
)

var skillsActions = []string{ActionLs, ActionView, ActionManage, ActionSources}

// skillsManageOps is the closed set of `manage` operations from the
// tool-surface spec. The list is published in the error for an unknown op so a
// model can correct itself without guessing.
var skillsManageOps = []string{"create", "patch", "put", "delete", "write_file", "remove_file"}

// skillsSourcesOps is the closed set of `sources` operations.
var skillsSourcesOps = []string{"list", "subscribe", "unsubscribe", "pin"}

const skillsToolDescription = "Skill knowledge base and library. Actions: " +
	"ls (visible skills: name/description/path); view {name} (full SKILL.md body). " +
	"manage {op}: create|patch|put|delete|write_file|remove_file — create {name, description, body} " +
	"turns an experience into a versioned skill, patch/put/write_file/remove_file produce a new immutable " +
	"version, delete removes the skill. sources {op}: list|subscribe|unsubscribe|pin — subscriptions carry " +
	"a trust level, pin holds a version, unsubscribe refuses while a skill still uses the source. " +
	"A skill's scripts are NOT executed here: run them with the bash tool so sandbox, job and output " +
	"bounds apply."

// SkillsBackend is the skill library behind the aggregate tool. It is the
// product layer's tenant-scoped registry (internal/skilllib): the tool keeps
// its constant name and action set, the library supplies the state. A nil
// backend keeps ls/view working for boot-discovered skills and makes
// manage/sources answer not_wired.
type SkillsBackend interface {
	// SkillsList returns the skills installed in the calling run's tenant
	// library. The tool merges them with the discovered list, so an install is
	// visible without a prompt rebuild.
	SkillsList(ctx context.Context) ([]skills.Skill, error)
	// SkillsManage runs one manage operation. args is the action payload (the
	// fields the action table documents); the backend validates the fields it
	// needs.
	SkillsManage(ctx context.Context, op string, args json.RawMessage) (any, error)
	// SkillsSources runs one sources operation.
	SkillsSources(ctx context.Context, op string, args json.RawMessage) (any, error)
}

func skillsSchema() json.RawMessage {
	return schema(`, "op":{"type":"string"}, "name":{"type":"string"}, "path":{"type":"string"}, ` +
		`"source":{"type":"string"}, "description":{"type":"string"}, "body":{"type":"string"}, ` +
		`"content":{"type":"string"}, "files":{"type":"array"}, "id":{"type":"string"}, ` +
		`"kind":{"type":"string"}, "url":{"type":"string"}, "hash":{"type":"string"}, ` +
		`"pin":{"type":"boolean"}`)
}

func newSkillsTool(discovered []skills.Skill, backend SkillsBackend) *actionTool {
	visible := make([]skills.Skill, 0, len(discovered))
	for _, skill := range discovered {
		if !skill.DisableModelInvocation {
			visible = append(visible, skill)
		}
	}
	return &actionTool{
		name:        Skills,
		description: skillsToolDescription,
		schema:      skillsSchema(),
		actions:     skillsActions,
		run: func(ctx context.Context, action string, raw json.RawMessage) (agentcore.AgentToolResult, error) {
			switch action {
			case ActionLs:
				return skillsList(ctx, visible, backend)
			case ActionView:
				return skillsView(ctx, visible, backend, raw)
			case ActionManage:
				return skillsManage(ctx, backend, raw)
			case ActionSources:
				return skillsSources(ctx, backend, raw)
			default:
				return unknownAction(Skills, action, skillsActions)
			}
		},
	}
}

// mergeSkills appends library skills that discovery did not already carry. The
// discovered list wins on a name collision: it is the deployment's configured
// root, and the library's active/ directory is normally part of it, so the
// merge only matters while a run's prompt predates an install.
func mergeSkills(discovered, installed []skills.Skill) []skills.Skill {
	if len(installed) == 0 {
		return discovered
	}
	names := make(map[string]struct{}, len(discovered))
	out := append([]skills.Skill(nil), discovered...)
	for _, skill := range discovered {
		names[skill.Name] = struct{}{}
	}
	sort.Slice(installed, func(i, j int) bool { return installed[i].Name < installed[j].Name })
	for _, skill := range installed {
		if _, ok := names[skill.Name]; ok {
			continue
		}
		names[skill.Name] = struct{}{}
		out = append(out, skill)
	}
	return out
}

func skillsList(ctx context.Context, discovered []skills.Skill, backend SkillsBackend) (agentcore.AgentToolResult, error) {
	all := discovered
	var libraryError string
	if backend != nil {
		installed, err := backend.SkillsList(ctx)
		if err != nil {
			// The discovered roots still answer; the failure is named instead of
			// being silently presented as "no library skills".
			libraryError = err.Error()
		} else {
			all = mergeSkills(discovered, installed)
		}
	}
	items := make([]map[string]string, 0, len(all))
	for _, skill := range all {
		if skill.DisableModelInvocation {
			continue
		}
		items = append(items, map[string]string{
			"name":        skill.Name,
			"description": skill.Description,
			"path":        skill.Path,
		})
	}
	document := map[string]any{"count": len(items), "skills": items}
	if libraryError != "" {
		document["libraryError"] = libraryError
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
		StructuredContent: document,
	}, nil
}

func skillsView(ctx context.Context, discovered []skills.Skill, backend SkillsBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return invalidArguments(Skills, err.Error())
	}
	if input.Name == "" {
		return invalidArguments(Skills, "view requires name")
	}
	all := discovered
	if backend != nil {
		// A library read is scoped to the run's tenant; an error here is not fatal
		// because the discovered list may already hold the skill.
		if installed, err := backend.SkillsList(ctx); err == nil {
			all = mergeSkills(discovered, installed)
		}
	}
	for _, skill := range all {
		if skill.Name != input.Name {
			continue
		}
		document := map[string]any{
			"name":        skill.Name,
			"description": skill.Description,
			"path":        skill.Path,
			"body":        skill.Body,
		}
		return agentcore.AgentToolResult{
			Content: agentcore.ContentList{agentcore.NewTextContent(
				fmt.Sprintf("Skill %s (%s)\n\n%s", skill.Name, skill.Path, skill.Body),
			)},
			StructuredContent: document,
		}, nil
	}
	return notFound(Skills, fmt.Sprintf("unknown skill %q", input.Name))
}

func skillsManage(ctx context.Context, backend SkillsBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	op, failed, ok := subAction(Skills, raw, "op", skillsManageOps)
	if !ok {
		return failed, nil
	}
	if backend == nil {
		return notWired(Skills, "manage needs the skill library, which is not wired in this deployment")
	}
	result, callErr := backend.SkillsManage(ctx, op, raw)
	if callErr != nil {
		return skillsBackendFailure("manage", callErr)
	}
	return skillsResult(result)
}

func skillsSources(ctx context.Context, backend SkillsBackend, raw json.RawMessage) (agentcore.AgentToolResult, error) {
	op, failed, ok := subAction(Skills, raw, "sourceAction", skillsSourcesOps)
	if !ok {
		return failed, nil
	}
	if backend == nil {
		return notWired(Skills, "sources needs the skill library, which is not wired in this deployment")
	}
	result, callErr := backend.SkillsSources(ctx, op, raw)
	if callErr != nil {
		return skillsBackendFailure("sources", callErr)
	}
	return skillsResult(result)
}

// subAction reads the operation name of a nested action. The spec calls the
// `manage` members create/patch/..., but the wire argument can reasonably be
// `op`, `manageAction` or `sourceAction`; every spelling is accepted so a
// client (or a model) is not failed on a naming detail. An unknown or missing
// op is invalid_arguments with the closed list.
func subAction(tool string, raw json.RawMessage, primary string, allowed []string) (string, agentcore.AgentToolResult, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		result, _ := invalidArguments(tool, err.Error())
		return "", result, false
	}
	op := ""
	for _, key := range []string{primary, "op", "manageAction", "sourceAction"} {
		rawValue, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err == nil && value != "" {
			op = value
			break
		}
	}
	if op == "" {
		result, _ := invalidArguments(tool, "the nested action is required (one of: "+joinList(allowed)+")")
		return "", result, false
	}
	for _, candidate := range allowed {
		if op == candidate {
			return op, agentcore.AgentToolResult{}, true
		}
	}
	result, _ := invalidArguments(tool, fmt.Sprintf("unknown nested action %q (available: %s)", op, joinList(allowed)))
	return "", result, false
}

func skillsBackendFailure(action string, err error) (agentcore.AgentToolResult, error) {
	code := CodeBackendFailed
	if coder, ok := err.(interface{ ToolCode() string }); ok {
		if value := coder.ToolCode(); value != "" {
			code = value
		}
	}
	return ErrorResult(code, Skills, " "+action+" failed: "+err.Error(), map[string]any{"action": action})
}

func skillsResult(value any) (agentcore.AgentToolResult, error) {
	if value == nil {
		document := map[string]any{"ok": true}
		return agentcore.AgentToolResult{
			Content:           agentcore.ContentList{agentcore.NewTextContent(marshalDocument(document))},
			StructuredContent: document,
		}, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrorResult(CodeBackendFailed, Skills, " cannot encode the result: "+err.Error(), nil)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return ErrorResult(CodeBackendFailed, Skills, " cannot encode the result: "+err.Error(), nil)
	}
	return agentcore.AgentToolResult{
		Content:           agentcore.ContentList{agentcore.NewTextContent(string(raw))},
		StructuredContent: document,
	}, nil
}

func joinList(values []string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += value
	}
	return out
}
