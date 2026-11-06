package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " skill\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHostAssemblesPromptAndTools(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "pdf", "Read the PDF body.")
	h, warnings, err := New(Config{
		Preamble:        "You are a test agent.",
		SkillRoots:      []string{root},
		IncludeBuiltins: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	prompt := h.SystemPrompt()
	if !strings.Contains(prompt, "<skills>") || !strings.Contains(prompt, "<name>pdf</name>") {
		t.Fatalf("prompt = %q", prompt)
	}
	if _, ok := h.Tools().Get("clock"); !ok {
		t.Fatal("clock tool missing")
	}
	if _, ok := h.Tools().Get("skill"); !ok {
		t.Fatal("skill tool missing")
	}
	if _, ok := h.Tools().Get("pdf"); ok {
		t.Fatal("skill body should not be a tool")
	}
	// The skill tool loads the body on demand.
	tool, _ := h.Tools().Get("skill")
	result, err := tool.Execute(context.Background(), "call-1", json.RawMessage(`{"name":"pdf"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content[0].(agentcore.TextContent).Text, "Read the PDF body.") {
		t.Fatalf("skill body = %#v", result.Content)
	}
}

func TestHostHidesDisabledSkillFromPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "hidden")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: hidden\ndescription: hidden skill\ndisable-model-invocation: true\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h, _, err := New(Config{Preamble: "base", SkillRoots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.SystemPrompt(), "hidden") {
		t.Fatalf("disabled skill advertised: %q", h.SystemPrompt())
	}
	if _, ok := h.Tools().Get("skill"); ok {
		t.Fatal("skill tool should not exist without visible skills")
	}
}
