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
