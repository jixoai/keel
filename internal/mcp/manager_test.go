// Tests for the mcp.json loader and the MCP manager, ported from pi
// coding-agent's MCP config behaviour (MIT License).

package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigFiles(t *testing.T) {
	t.Run("merges global and project entries, project wins", func(t *testing.T) {
		dir := t.TempDir()
		global := filepath.Join(dir, "mcp.json")
		project := filepath.Join(dir, "project", "mcp.json")
		writeConfigFile(t, global, `{"mcpServers":{
			"docs":{"url":"https://example.com/mcp","description":"Docs"},
			"files":{"command":"npx","args":["-y","server"]}
		}}`)
		writeConfigFile(t, project, `{"mcpServers":{
			"files":{"command":"other","args":[]},
			"docs":{"enabled":false}
		}}`)
		loaded := LoadConfigFiles(global, project)
		if len(loaded.Errors) != 0 {
			t.Fatalf("errors = %#v", loaded.Errors)
		}
		byName := map[string]ServerEntry{}
		for _, entry := range loaded.Servers {
			byName[entry.Name] = entry
		}
		if got := ServerField(byName["files"].Config, "command"); got != "other" {
			t.Errorf("files.command = %q, want the project replacement", got)
		}
		docs := byName["docs"]
		if ServerBool(docs.Config, "enabled", true) {
			t.Error("docs.enabled = true, want the project override to disable it")
		}
		if ServerField(docs.Config, "url") != "https://example.com/mcp" || ServerField(docs.Config, "description") != "Docs" {
			t.Errorf("docs = %#v, want the global fields kept", docs.Config)
		}
		if docs.Override != project {
			t.Errorf("docs.Override = %q, want %q", docs.Override, project)
		}
	})

	t.Run("reports invalid entries and keeps valid ones", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		writeConfigFile(t, path, `{"mcpServers":{
			"ok":{"command":"x"},
			"bad name":{"command":"x"},
			"no-transport":{"enabled":true}
		}}`)
		loaded := LoadConfigFiles(path, "")
		if len(loaded.Servers) != 1 || loaded.Servers[0].Name != "ok" {
			t.Fatalf("servers = %#v", loaded.Servers)
		}
		if len(loaded.Errors) != 2 {
			t.Fatalf("errors = %#v, want two", loaded.Errors)
		}
	})

	t.Run("rejects unknown override keys and a missing global base", func(t *testing.T) {
		dir := t.TempDir()
		global := filepath.Join(dir, "mcp.json")
		project := filepath.Join(dir, "project", "mcp.json")
		writeConfigFile(t, global, `{"mcpServers":{"docs":{"url":"https://example.com/mcp"}}}`)
		writeConfigFile(t, project, `{"mcpServers":{
			"docs":{"enabled":false,"description":"x"},
			"ghost":{"enabled":false}
		}}`)
		loaded := LoadConfigFiles(global, project)
		if len(loaded.Errors) != 2 {
			t.Fatalf("errors = %#v, want two", loaded.Errors)
		}
		for _, message := range loaded.Errors {
			if !strings.Contains(message, "override can only set") && !strings.Contains(message, "global server to override") {
				t.Errorf("unexpected error %q", message)
			}
		}
	})

	t.Run("rejects server names that share a namespace", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "mcp.json")
		writeConfigFile(t, path, `{"mcpServers":{
			"a-b":{"command":"x"},
			"a_b":{"command":"y"}
		}}`)
		loaded := LoadConfigFiles(path, "")
		if len(loaded.Servers) != 1 || len(loaded.Errors) != 1 || !strings.Contains(loaded.Errors[0], "conflicts with") {
			t.Fatalf("servers = %#v errors = %#v", loaded.Servers, loaded.Errors)
		}
	})

	t.Run("missing files are not errors", func(t *testing.T) {
		loaded := LoadConfigFiles(filepath.Join(t.TempDir(), "none.json"), "")
		if len(loaded.Servers) != 0 || len(loaded.Errors) != 0 {
			t.Fatalf("loaded = %#v", loaded)
		}
	})
}

func TestManagerConnect(t *testing.T) {
	env := map[string]any{"KEEL_MCP_STDIO_HELPER": "1"}
	entries := []ServerEntry{
		{Name: "helper", Config: map[string]any{
			"command": os.Args[0],
			"args":    []any{"-test.run=^TestHelperProcess$"},
			"env":     env,
		}},
		{Name: "broken", Config: map[string]any{"command": filepath.Join(t.TempDir(), "missing-binary")}},
		{Name: "off", Config: map[string]any{"command": "x", "enabled": false}},
	}
	manager := Connect(context.Background(), entries, ConnectOptions{ClientName: "keel-test", ClientVersion: "1.0.0"})
	defer manager.Close()
	servers := manager.Servers()
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want the enabled ones only", len(servers))
	}
	var helper, broken *Server
	for _, server := range servers {
		switch server.Name() {
		case "helper":
			helper = server
		case "broken":
			broken = server
		}
	}
	if helper == nil || helper.Err != nil {
		t.Fatalf("helper = %#v", helper)
	}
	if len(helper.Tools) != 1 {
		t.Fatalf("helper tools = %#v", helper.Tools)
	}
	tool := helper.Tools[0]
	if tool.Name() != "mcp__helper__echo" {
		t.Errorf("tool name = %q", tool.Name())
	}
	result, err := tool.Execute(context.Background(), "call-1", []byte(`{"text":"hi"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if text := joinedText(result.Content); text != "hi" {
		t.Errorf("tool result = %q", text)
	}
	if broken == nil || broken.Err == nil {
		t.Fatalf("broken = %#v, want a reported error", broken)
	}
	if errors := manager.Errors(); len(errors) != 1 || !strings.Contains(errors[0], "broken") {
		t.Errorf("manager errors = %#v", errors)
	}
	if section := manager.PromptSection(); !strings.Contains(section, "- helper:") {
		t.Errorf("prompt section = %q", section)
	}
}
