package agent

// Skill tool policy (`allowed-tools`, change feat-keel-pi-parity-closure-2).
//
// An explicitly loaded skill (`/skill:<name>`) applies to the next turn. Its
// `allowed-tools` declaration is a hard restriction for that turn, not a hint:
// PI's Skill metadata has the same meaning. Two enforcement points share one
// policy:
//
//   - the attempt's tool surface is intersected with the union of the active
//     skills' declared tools, so the model never sees a tool the skill denies;
//   - the beforeToolCall gate refuses a call that still reaches the executor
//     (a stale assembly, an injected tool, a teammate's shared surface) with a
//     typed `skill_tool_not_allowed` refusal.
//
// Semantics:
//   - no active skill, or every active skill declares no allowed-tools: no
//     restriction (an empty declaration is "all tools", as in PI);
//   - otherwise the allowed set is the union of every skill that DID declare a
//     list; a sibling skill without a declaration adds nothing (it does not
//     widen the restricted set);
//   - entries are case-insensitive tool names; a parenthetical scope such as
//     `Bash(git:*)` names the `bash` tool; `mcp__<server>__<tool>` names the
//     aggregate `mcp` tool (Keel does not expose per-server names); `*` and
//     glob patterns match by name.

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// SkillToolDeniedCode is the structured tool-result code of a refusal.
const SkillToolDeniedCode = "skill_tool_not_allowed"

// mcpToolScope is one per-server/per-tool MCP grant from `allowed-tools`. Keel
// exposes the single aggregate `mcp` tool, so the scope is enforced against the
// call's (server, tool) arguments; it must never widen to every MCP tool.
type mcpToolScope struct {
	server string
	tool   string // "*" grants any tool of that server
}

// skillToolPolicy is the effective allowed-tool set of one attempt.
type skillToolPolicy struct {
	restricted bool
	allowed    []string
	// mcpAll is a bare `mcp` / `mcp*` grant: every MCP tool is allowed.
	mcpAll bool
	// mcpScopes are the per-server/tool grants. A non-empty list makes the
	// aggregate `mcp` tool visible, but only the named calls pass the gate.
	mcpScopes []mcpToolScope
	// skills names the active skills that contributed the restriction. It is
	// diagnostic only (the refusal names them).
	skills []string
}

// newSkillToolPolicy builds the policy of an attempt's pending skills.
func newSkillToolPolicy(loaded []loadedSkill) skillToolPolicy {
	var policy skillToolPolicy
	seen := map[string]struct{}{}
	seenMCP := map[mcpToolScope]struct{}{}
	for _, skill := range loaded {
		name := strings.TrimSpace(skill.Name)
		if name == "" || len(skill.AllowedTools) == 0 {
			// A skill without a declaration imposes no restriction of its own.
			// When it is the only active skill the turn stays unrestricted; next
			// to a restricted skill it does not widen that skill's set.
			continue
		}
		policy.restricted = true
		policy.skills = append(policy.skills, name)
		for _, entry := range skill.AllowedTools {
			if scope, all, isMCP := parseMCPAllowedTool(entry); isMCP {
				if all {
					policy.mcpAll = true
					continue
				}
				if scope.server == "" {
					continue
				}
				if _, duplicate := seenMCP[scope]; duplicate {
					continue
				}
				seenMCP[scope] = struct{}{}
				policy.mcpScopes = append(policy.mcpScopes, scope)
				continue
			}
			normalized := normalizeAllowedTool(entry)
			if normalized == "" {
				continue
			}
			if _, duplicate := seen[normalized]; duplicate {
				continue
			}
			seen[normalized] = struct{}{}
			policy.allowed = append(policy.allowed, normalized)
		}
	}
	sort.Strings(policy.skills)
	sort.Strings(policy.allowed)
	sort.Slice(policy.mcpScopes, func(i, j int) bool {
		if policy.mcpScopes[i].server != policy.mcpScopes[j].server {
			return policy.mcpScopes[i].server < policy.mcpScopes[j].server
		}
		return policy.mcpScopes[i].tool < policy.mcpScopes[j].tool
	})
	if policy.restricted && len(policy.allowed) == 0 && !policy.mcpAll && len(policy.mcpScopes) == 0 {
		// A declaration that names no usable tool is a restriction to nothing:
		// the skill gets no tools rather than silently all of them.
		policy.allowed = nil
	}
	return policy
}

// allows reports whether the tool may appear on the model-visible surface. For
// `mcp` this is true when any MCP grant exists; the execution gate below still
// narrows the actual (server, tool) call.
func (p skillToolPolicy) allows(tool string) bool {
	if !p.restricted {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(tool))
	if name == "" {
		return false
	}
	if name == "mcp" {
		return p.mcpAll || len(p.mcpScopes) > 0 || p.baseAllows(name)
	}
	return p.baseAllows(name)
}

func (p skillToolPolicy) baseAllows(name string) bool {
	for _, pattern := range p.allowed {
		if pattern == name || pattern == "*" {
			return true
		}
		if strings.Contains(pattern, "?") || strings.Contains(pattern, "[") || strings.Contains(pattern, "*") {
			if matched, err := path.Match(pattern, name); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// allowsCall is the execution-time gate. A non-MCP tool is judged by name; the
// aggregate `mcp` tool is judged by its action call's server/tool, so a grant
// for `mcp__filesystem__read_file` cannot be used to delete a file or to call
// another server. Metadata actions (list/desc) are not granted by a per-tool
// entry.
func (p skillToolPolicy) allowsCall(tool string, arguments json.RawMessage) bool {
	if !p.restricted {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(tool))
	if name != "mcp" {
		return p.baseAllows(name)
	}
	if p.mcpAll || p.baseAllows(name) {
		return true
	}
	if len(p.mcpScopes) == 0 {
		return false
	}
	var call struct {
		Action string `json:"action"`
		Server string `json:"server"`
		Tool   string `json:"tool"`
	}
	if err := json.Unmarshal(arguments, &call); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(call.Action), "call") {
		return false
	}
	server := strings.ToLower(strings.TrimSpace(call.Server))
	toolName := strings.ToLower(strings.TrimSpace(call.Tool))
	if server == "" || toolName == "" {
		return false
	}
	for _, scope := range p.mcpScopes {
		if scope.server != server {
			continue
		}
		if scope.tool == "*" || scope.tool == toolName {
			return true
		}
		if strings.ContainsAny(scope.tool, "?*[") {
			if matched, err := path.Match(scope.tool, toolName); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// parseMCPAllowedTool recognises the PI MCP spellings. It reports (scope, all,
// isMCP): a bare `mcp`/`mcp*` grants every MCP tool; `mcp__server__tool` and
// `mcp__server__*` grant one server/tool; `mcp__server` grants that server.
func parseMCPAllowedTool(entry string) (mcpToolScope, bool, bool) {
	value := strings.ToLower(strings.TrimSpace(entry))
	if index := strings.IndexByte(value, '('); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	switch value {
	case "mcp", "mcp*":
		return mcpToolScope{}, true, true
	}
	if !strings.HasPrefix(value, "mcp__") {
		return mcpToolScope{}, false, false
	}
	parts := strings.Split(strings.TrimPrefix(value, "mcp__"), "__")
	server := strings.TrimSpace(parts[0])
	if server == "*" {
		return mcpToolScope{}, true, true
	}
	if server == "" {
		return mcpToolScope{}, false, true
	}
	toolName := "*"
	if len(parts) > 1 {
		toolName = strings.TrimSpace(strings.Join(parts[1:], "__"))
		if toolName == "" {
			toolName = "*"
		}
	}
	return mcpToolScope{server: server, tool: toolName}, false, true
}

// normalizeAllowedTool maps one allowed-tools entry onto a Keel tool name.
func normalizeAllowedTool(entry string) string {
	value := strings.TrimSpace(entry)
	if value == "" {
		return ""
	}
	// `Bash(git:*)` / `Read(*)` name a tool plus a command/path scope; Keel's
	// aggregate tool is already scoped by its action argument, so the scope
	// does not narrow the tool name.
	if index := strings.IndexByte(value, '('); index >= 0 {
		value = value[:index]
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	// MCP spellings are parsed by parseMCPAllowedTool before this function; a
	// leftover `mcp:`-shaped name is not the aggregate tool and stays a name
	// that matches nothing (safe deny).
	return value
}

// intersectTools returns the registry restricted to the policy. The source
// registry is never mutated: Subset builds the per-attempt copy.
func (p skillToolPolicy) intersectTools(registry *agenttool.ToolRegistry) (*agenttool.ToolRegistry, error) {
	if registry == nil || !p.restricted {
		return registry, nil
	}
	names := make([]string, 0, len(registry.Names()))
	for _, name := range registry.Names() {
		if p.allows(name) {
			names = append(names, name)
		}
	}
	subset, err := registry.Subset(names)
	if err != nil {
		return nil, fmt.Errorf("agent: skill allowed-tools surface: %w", err)
	}
	return subset, nil
}

// skillToolGuard returns the beforeToolCall gate for one attempt. It runs after
// authorization (a skill can never widen the principal's grants) and refuses a
// tool outside the active skills' allowed set with a typed, model-readable
// message.
func (r *Runner) skillToolGuard(policy skillToolPolicy) agentcore.BeforeToolCallFunc {
	if !policy.restricted {
		return nil
	}
	return func(_ context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		if policy.allowsCall(call.Name, call.Arguments) {
			return nil
		}
		skills := strings.Join(policy.skills, ", ")
		if skills == "" {
			skills = "the loaded skill"
		}
		allowed := "none"
		if len(policy.allowed) > 0 {
			allowed = strings.Join(policy.allowed, ", ")
		}
		message := fmt.Sprintf(
			"Tool %q is not allowed by the loaded skill (%s). Allowed tools: %s.",
			call.Name, skills, allowed)
		details := map[string]any{
			"code":    SkillToolDeniedCode,
			"tool":    call.Name,
			"skills":  append([]string(nil), policy.skills...),
			"allowed": append([]string(nil), policy.allowed...),
		}
		var payload any = details
		content := agentcore.ContentList{agentcore.NewTextContent(message)}
		return &agentcore.BeforeToolCallDecision{Block: true, Content: &content, Details: &payload}
	}
}

// composeSkillToolGuard places the skill gate after authorization and before
// guardrails/hooks: a refusal is the skill's rule, not a repeat or a hook.
func composeSkillToolGuard(base agentcore.BeforeToolCallFunc, guard agentcore.BeforeToolCallFunc) agentcore.BeforeToolCallFunc {
	if guard == nil {
		return base
	}
	return func(ctx context.Context, call agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		if base != nil {
			if decision := base(ctx, call); decision != nil {
				return decision
			}
		}
		return guard(ctx, call)
	}
}
