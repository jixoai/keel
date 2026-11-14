package host

import (
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/skills"
)

func TestBuildSystemPrompt(t *testing.T) {
	cfg := PromptConfig{
		Preamble: "You are a helpful agent.",
		Sections: []Section{
			{Name: "tenant", Content: "Acme Corp"},
			{Name: "empty", Content: "  "},
		},
		Skills: []skills.Skill{
			{Name: "pdf", Description: "Read PDFs.", Path: "/skills/pdf/SKILL.md"},
			{Name: "hidden", Description: "Nope.", Path: "/skills/hidden/SKILL.md", DisableModelInvocation: true},
		},
	}
	got := BuildSystemPrompt(cfg)
	if !strings.HasPrefix(got, "You are a helpful agent.") {
		t.Fatalf("preamble missing: %q", got)
	}
	if !strings.Contains(got, "<tenant>\nAcme Corp\n</tenant>") {
		t.Fatalf("tenant section missing: %q", got)
	}
	if strings.Contains(got, "<empty>") {
		t.Fatalf("empty section rendered: %q", got)
	}
	if !strings.Contains(got, "<skills>") || !strings.Contains(got, "<name>pdf</name>") {
		t.Fatalf("skills section missing: %q", got)
	}
	if strings.Contains(got, "hidden") {
		t.Fatalf("hidden skill advertised: %q", got)
	}
}

func TestBuildSystemPromptNoSkills(t *testing.T) {
	got := BuildSystemPrompt(PromptConfig{Preamble: "base"})
	if got != "base" {
		t.Fatalf("got %q", got)
	}
}

// TestRenderSystemPromptSwapsThePreamble pins the execution-binding seam
// (change keel-agent-team-v1): an Agent's system prompt replaces the deployment
// preamble while the configured sections, the dynamic sections (mcp_servers,
// plugin contributions) and the skills index keep rendering around it. An empty
// preamble keeps the configured one, so a run without a binding renders the
// same bytes SystemPrompt always did.
func TestRenderSystemPromptSwapsThePreamble(t *testing.T) {
	asm, _, err := New(Config{
		Preamble: "deployment preamble",
		Sections: []Section{{Name: "tenant_rules", Content: "Tenant rule text."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A plugin contribution is the dynamic surface a binding must never drop.
	if err := asm.RefreshSections(nil, "mcp servers live here",
		[]Section{{Name: "plugin_standard", Content: "Plugin standard text."}}); err != nil {
		t.Fatal(err)
	}
	base := asm.SystemPrompt()
	if !strings.Contains(base, "deployment preamble") {
		t.Fatalf("baseline prompt = %q", base)
	}

	withBinding := asm.RenderSystemPrompt("KITE-RUNTIME-MARK-7 agent instructions")
	if !strings.Contains(withBinding, "KITE-RUNTIME-MARK-7") {
		t.Fatalf("the binding preamble did not render: %q", withBinding)
	}
	if strings.Contains(withBinding, "deployment preamble") {
		t.Fatalf("the deployment preamble survived the swap: %q", withBinding)
	}
	for _, want := range []string{"<tenant_rules>", "Tenant rule text.", "<mcp_servers>", "Plugin standard text."} {
		if !strings.Contains(withBinding, want) {
			t.Fatalf("the binding prompt dropped %q: %q", want, withBinding)
		}
	}

	// An empty preamble renders exactly what SystemPrompt does.
	if got := asm.RenderSystemPrompt(""); got != base {
		t.Fatalf("RenderSystemPrompt(\"\") = %q, want the configured prompt %q", got, base)
	}
	if got := asm.RenderSystemPrompt("   "); got != base {
		t.Fatalf("RenderSystemPrompt(blank) = %q, want the configured prompt %q", got, base)
	}
}
