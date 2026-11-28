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
//   - otherwise the allowed set is the union of every restricted skill's list;
//   - entries are case-insensitive tool names; a parenthetical scope such as
//     `Bash(git:*)` names the `bash` tool; `mcp__<server>__<tool>` names the
//     aggregate `mcp` tool (Keel does not expose per-server names); `*` and
//     glob patterns match by name.

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
)

// SkillToolDeniedCode is the structured tool-result code of a refusal.
const SkillToolDeniedCode = "skill_tool_not_allowed"

// skillToolPolicy is the effective allowed-tool set of one attempt.
type skillToolPolicy struct {
	restricted bool
	allowed    []string
	// skills names the active skills that contributed the restriction. It is
	// diagnostic only (the refusal names them).
	skills []string
}

// newSkillToolPolicy builds the policy of an attempt's pending skills.
func newSkillToolPolicy(loaded []loadedSkill) skillToolPolicy {
	var policy skillToolPolicy
	seen := map[string]struct{}{}
	for _, skill := range loaded {
		name := strings.TrimSpace(skill.Name)
		if name == "" || len(skill.AllowedTools) == 0 {
			// A skill without a declaration is unrestricted; PI treats absence
			// as "no restriction", and a single unrestricted active skill makes
			// the union unrestricted.
			continue
		}
		policy.restricted = true
		policy.skills = append(policy.skills, name)
		for _, entry := range skill.AllowedTools {
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
	if policy.restricted && len(policy.allowed) == 0 {
		// A declaration that names no usable tool is a restriction to nothing:
		// the skill gets no tools rather than silently all of them.
		policy.allowed = nil
	}
	return policy
}

// allows reports whether tool satisfies the policy.
func (p skillToolPolicy) allows(tool string) bool {
	if !p.restricted {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(tool))
	if name == "" {
		return false
	}
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
	// PI exposes one tool per MCP tool; Keel deliberately exposes the single
	// aggregate `mcp` tool. Any per-server declaration therefore names `mcp`.
	if strings.HasPrefix(value, "mcp__") || strings.HasPrefix(value, "mcp:") {
		return "mcp"
	}
	// A wildcard that only covers MCP names has no Keel equivalent beyond the
	// aggregate tool.
	if strings.HasPrefix(value, "mcp*") {
		return "mcp"
	}
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
		if policy.allows(call.Name) {
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
