package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/agenttool"
	"github.com/jixoai/keel/internal/store"
	"github.com/jixoai/keel/internal/testkit"
)

func testToolRegistry(t *testing.T) *agenttool.ToolRegistry {
	t.Helper()
	registry := agenttool.NewToolRegistry()
	if err := registry.Register(testkit.ClockTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(testkit.EchoTool{}); err != nil {
		t.Fatal(err)
	}
	return registry
}

// TestSkillToolPolicy pins the allowed-tools vocabulary: absence means no
// restriction, a declaration is a hard restriction, scoped entries name the
// aggregate tool, and the union of several active skills is what applies.
func TestSkillToolPolicy(t *testing.T) {
	cases := []struct {
		name    string
		skills  []loadedSkill
		allowed []string
		denied  []string
	}{
		{
			name:    "no active skill",
			allowed: []string{"read", "bash", "echo"},
		},
		{
			name:    "skill without declaration",
			skills:  []loadedSkill{{Name: "pdf"}},
			allowed: []string{"read", "bash", "echo"},
		},
		{
			name:    "declared restriction",
			skills:  []loadedSkill{{Name: "pdf", AllowedTools: []string{"Read", "Bash(git:*)"}}},
			allowed: []string{"read", "bash"},
			denied:  []string{"echo", "write", "mcp"},
		},
		{
			name: "union of two restricted skills",
			skills: []loadedSkill{
				{Name: "pdf", AllowedTools: []string{"Read"}},
				{Name: "data", AllowedTools: []string{"Write"}},
			},
			allowed: []string{"read", "write"},
			denied:  []string{"echo"},
		},
		{
			name:    "wildcard",
			skills:  []loadedSkill{{Name: "all", AllowedTools: []string{"*"}}},
			allowed: []string{"read", "echo", "mcp"},
		},
		{
			name:    "per-server MCP entry names the aggregate tool",
			skills:  []loadedSkill{{Name: "mcp-skill", AllowedTools: []string{"mcp__filesystem__read_file"}}},
			allowed: []string{"mcp"},
			denied:  []string{"read", "echo"},
		},
		{
			name:   "empty declaration denies every tool",
			skills: []loadedSkill{{Name: "empty", AllowedTools: []string{"  ", "(scope)"}}},
			denied: []string{"read", "echo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := newSkillToolPolicy(tc.skills)
			for _, tool := range tc.allowed {
				if !policy.allows(tool) {
					t.Errorf("tool %q denied by %+v", tool, policy)
				}
			}
			for _, tool := range tc.denied {
				if policy.allows(tool) {
					t.Errorf("tool %q allowed by %+v", tool, policy)
				}
			}
		})
	}
}

// TestSkillToolPolicyReadsLoadedPin proves the policy is derived from the
// durable skill.loaded payload (the command host writes allowedTools there).
func TestSkillToolPolicyReadsLoadedPin(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"name": "pdf", "body": "body", "allowedTools": []string{"read"},
	})
	events := []store.Event{{Seq: 2, Kind: store.EventSkillLoaded, Payload: payload}}
	policy := newSkillToolPolicy(pendingLoadedSkills(events))
	if !policy.restricted || !policy.allows("read") || policy.allows("echo") {
		t.Fatalf("policy = %+v, want read-only", policy)
	}
}

// TestSkillToolPolicyIntersectsSurface pins the model-visible half: the
// registry handed to the loop excludes tools the active skill does not allow.
func TestSkillToolPolicyIntersectsSurface(t *testing.T) {
	policy := newSkillToolPolicy([]loadedSkill{{Name: "pdf", AllowedTools: []string{"Clock"}}})
	subset, err := policy.intersectTools(testToolRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	names := subset.Names()
	if len(names) != 1 || names[0] != "clock" {
		t.Fatalf("surface = %v, want only clock", names)
	}
}

// TestSkillToolGuardRefusesTyped pins the execution gate: a call that reaches
// the executor despite the surface intersection is refused with a structured
// code and a model-readable message naming the skill.
func TestSkillToolGuardRefusesTyped(t *testing.T) {
	runner := &Runner{}
	policy := newSkillToolPolicy([]loadedSkill{{Name: "pdf", AllowedTools: []string{"read"}}})
	guard := runner.skillToolGuard(policy)
	if guard == nil {
		t.Fatal("a restricted policy produced no guard")
	}
	if decision := guard(context.Background(), agentcore.AgentToolCall{Name: "read"}); decision != nil {
		t.Fatalf("allowed tool was blocked: %+v", decision)
	}
	decision := guard(context.Background(), agentcore.AgentToolCall{Name: "echo"})
	if decision == nil || !decision.Block {
		t.Fatalf("disallowed tool decision = %+v, want block", decision)
	}
	if decision.Content == nil {
		t.Fatal("refusal has no model-visible content")
	}
	text := ContentText(*decision.Content)
	if !strings.Contains(text, "echo") || !strings.Contains(text, "pdf") || !strings.Contains(text, "read") {
		t.Fatalf("refusal text = %q", text)
	}
	if decision.Details == nil {
		t.Fatal("refusal has no structured details")
	}
	raw, _ := json.Marshal(*decision.Details)
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if details["code"] != SkillToolDeniedCode || details["tool"] != "echo" {
		t.Fatalf("details = %+v", details)
	}
}

// TestSkillToolGuardComposesAfterAuthorization pins ordering: authorization
// runs first, so a skill refusal can never hide a missing grant, and a tool the
// principal may not use is refused by authorization even when the skill allows
// the name.
func TestSkillToolGuardComposesAfterAuthorization(t *testing.T) {
	policy := newSkillToolPolicy([]loadedSkill{{Name: "pdf", AllowedTools: []string{"read"}}})
	authCalled := false
	base := func(context.Context, agentcore.AgentToolCall) *agentcore.BeforeToolCallDecision {
		authCalled = true
		content := agentcore.ContentList{agentcore.NewTextContent("denied by access")}
		return &agentcore.BeforeToolCallDecision{Block: true, Content: &content}
	}
	guard := composeSkillToolGuard(base, (&Runner{}).skillToolGuard(policy))
	decision := guard(context.Background(), agentcore.AgentToolCall{Name: "read"})
	if !authCalled || decision == nil || !decision.Block {
		t.Fatalf("composition = (auth=%v, decision=%+v), want the access refusal first", authCalled, decision)
	}
	if text := ContentText(*decision.Content); text != "denied by access" {
		t.Fatalf("refusal = %q, want the access decision", text)
	}
}
