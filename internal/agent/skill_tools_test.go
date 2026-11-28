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

// TestSkillToolPolicyMCPCallScope pins that a per-server/tool grant narrows the
// aggregate `mcp` tool's execution: it never widens to every MCP tool.
func TestSkillToolPolicyMCPCallScope(t *testing.T) {
	call := func(server, tool string) json.RawMessage {
		return json.RawMessage(`{"action":"call","server":"` + server + `","tool":"` + tool + `"}`)
	}
	exact := newSkillToolPolicy([]loadedSkill{{Name: "fs", AllowedTools: []string{"mcp__filesystem__read_file"}}})
	if !exact.allows("mcp") {
		t.Fatal("a scoped MCP grant must keep the aggregate tool visible")
	}
	if !exact.allowsCall("mcp", call("filesystem", "read_file")) {
		t.Fatal("the granted server/tool call was refused")
	}
	if exact.allowsCall("mcp", call("filesystem", "write_file")) {
		t.Fatal("another tool on the same server was allowed")
	}
	if exact.allowsCall("mcp", call("other", "read_file")) {
		t.Fatal("another server was allowed")
	}
	if exact.allowsCall("mcp", json.RawMessage(`{"action":"list"}`)) {
		t.Fatal("a metadata action was allowed by a per-tool grant")
	}
	if exact.allowsCall("mcp", json.RawMessage(`not-json`)) {
		t.Fatal("unparseable arguments were allowed")
	}

	serverWildcard := newSkillToolPolicy([]loadedSkill{{Name: "fs", AllowedTools: []string{"mcp__filesystem__*"}}})
	if !serverWildcard.allowsCall("mcp", call("filesystem", "delete_file")) {
		t.Fatal("mcp__server__* should allow any tool on that server")
	}
	if serverWildcard.allowsCall("mcp", call("other", "delete_file")) {
		t.Fatal("mcp__server__* leaked to another server")
	}

	serverAll := newSkillToolPolicy([]loadedSkill{{Name: "fs", AllowedTools: []string{"mcp__filesystem"}}})
	if !serverAll.allowsCall("mcp", call("filesystem", "anything")) {
		t.Fatal("mcp__server should allow that server's tools")
	}
	if serverAll.allowsCall("mcp", call("other", "anything")) {
		t.Fatal("mcp__server leaked to another server")
	}

	bare := newSkillToolPolicy([]loadedSkill{{Name: "all-mcp", AllowedTools: []string{"mcp"}}})
	if !bare.allowsCall("mcp", call("any", "anything")) {
		t.Fatal("a bare mcp grant should allow every MCP call")
	}

	// P1-B: a wildcard SERVER with a concrete tool must stay tool-scoped. It
	// must not become mcpAll and must never allow delete_file.
	anyServerRead := newSkillToolPolicy([]loadedSkill{{Name: "read-any", AllowedTools: []string{"mcp__*__read_file"}}})
	if !anyServerRead.allows("mcp") {
		t.Fatal("mcp__*__read_file must keep the aggregate tool visible")
	}
	for _, server := range []string{"filesystem", "github", "other"} {
		if !anyServerRead.allowsCall("mcp", call(server, "read_file")) {
			t.Fatalf("read_file on %s was refused", server)
		}
	}
	for _, server := range []string{"filesystem", "other"} {
		if anyServerRead.allowsCall("mcp", call(server, "delete_file")) {
			t.Fatalf("delete_file on %s was allowed by a read_file-only grant", server)
		}
	}
	// Malformed spellings grant nothing; they are never widened to all MCP.
	for _, spelling := range []string{"mcp__", "mcp__filesystem__", "mcp__*__"} {
		malformed := newSkillToolPolicy([]loadedSkill{{Name: "bad", AllowedTools: []string{spelling}}})
		if malformed.allows("mcp") {
			t.Fatalf("%q widened a malformed MCP spelling to the aggregate tool", spelling)
		}
		if malformed.allowsCall("mcp", call("filesystem", "read_file")) {
			t.Fatalf("%q allowed an MCP call", spelling)
		}
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
