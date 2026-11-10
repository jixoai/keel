package main

// The model-side proof of the plugin prompt seam (change keel-plugin-core, the
// last functional stitch): the sections a plugin contributes while it is active
// must reach the system prompt the runner assembles. The test drives the real
// serving wiring — newPluginSurface (the production plugin layer), the
// production manager over the bundled plugin set and a real MCP stdio server
// (a child of this test binary) — so what it asserts on is
// Runtime.Assembly().SystemPrompt(), the exact string the runner puts into
// agentcore.AgentContext.SystemPrompt for the model.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jixoai/keel/internal/agentext"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/kernel"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/mcp"
	"github.com/jixoai/keel/internal/servicehost"
	"github.com/jixoai/keel/internal/store"
)

func TestPluginPromptReachesSystemPrompt(t *testing.T) {
	ctx := context.Background()
	log := discardLogger()
	rt := pluginPromptTestRuntime(t)

	hooks := agentext.NewHookBus(agentext.HookBusOptions{Logger: log})
	decisions := agentext.NewDecisionBroker(agentext.DecisionBrokerOptions{Logger: log})
	partials := agentext.NewPartialHub()
	triggers := agentext.NewTriggerEngine(agentext.TriggerEngineOptions{Hooks: hooks, Logger: log})
	subagents := agentext.NewSubagentRunner(agentext.SubagentOptions{Hooks: hooks, Logger: log})
	t.Cleanup(func() {
		triggers.Close()
		decisions.Close()
		hooks.Close()
	})

	surface, err := newPluginSurface(ctx, rt, nil, serveOptions{
		TenantID:    uuid.New(),
		PluginsRoot: t.TempDir(),
	}, hooks, triggers, decisions, subagents, partials, log)
	if err != nil {
		t.Fatalf("plugin surface: %v", err)
	}
	t.Cleanup(func() { surface.runtime.Close(context.Background()) })

	// The bundled creation plugin is default-enabled, so its section is in the
	// system prompt before anything is asked, next to the MCP section — and both
	// arrive on the one path the model sees.
	assertPrompt := func(step string, wantPlugin bool) {
		t.Helper()
		prompt := rt.Assembly().SystemPrompt()
		if !strings.Contains(prompt, "<mcp_servers>") || !strings.Contains(prompt, "- promptfixture:") {
			t.Fatalf("%s: the MCP section is not in the prompt: %q", step, prompt)
		}
		if !strings.Contains(prompt, "You are the prompt-seam test agent.") || !strings.Contains(prompt, "Tenant rule text.") {
			t.Fatalf("%s: a configured section was dropped: %q", step, prompt)
		}
		hasPlugin := strings.Contains(prompt, "<plugin_standard>")
		if wantPlugin {
			if !hasPlugin || !strings.Contains(prompt, agentext.CreationPrompt()) {
				t.Fatalf("%s: the plugin_standard section (name and content) is not in the prompt: %q", step, prompt)
			}
			return
		}
		if hasPlugin || strings.Contains(prompt, "creation_scaffold") {
			t.Fatalf("%s: the plugin section survived the disable: %q", step, prompt)
		}
	}

	assertPrompt("boot", true)
	// The inventory the API serves carries the same effective contribution.
	if snapshot := surface.manager.Snapshot(); len(snapshot.Prompt) == 0 ||
		snapshot.Prompt[0].Name != agentext.CreationPromptSectionName {
		t.Fatalf("inventory prompt = %+v", snapshot.Prompt)
	}

	// Disable: the section is retracted from the prompt the model would see, and
	// the MCP section — the one the previous refresh seam could carry — survives.
	if _, err := surface.manager.Disable(ctx, agentext.CreationPluginID, agentext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	assertPrompt("disable", false)
	if _, ok := rt.Assembly().Tools().Get("creation_scaffold"); ok {
		t.Fatal("the creation tool survived the disable")
	}
	if snapshot := surface.manager.Snapshot(); len(snapshot.Prompt) != 0 {
		t.Fatalf("the inventory still reports a prompt after the disable: %+v", snapshot.Prompt)
	}
	// Compatibility line: with no plugin contributing, the live assembly's prompt
	// is byte-for-byte the prompt a host assembled without the plugin layer builds
	// for the same configuration — the publish cycle changed nothing.
	want, _, err := host.New(host.Config{
		Preamble: "You are the prompt-seam test agent.",
		Sections: []host.Section{
			{Name: "tenant_rules", Content: "Tenant rule text."},
			{Name: "mcp_servers", Content: "- promptfixture: prompt fixture"},
		},
		IncludeBuiltins: true,
	})
	if err != nil {
		t.Fatalf("reference host: %v", err)
	}
	if got := rt.Assembly().SystemPrompt(); got != want.SystemPrompt() {
		t.Fatalf("the no-plugin prompt drifted from the reference assembly:\n got: %q\nwant: %q", got, want.SystemPrompt())
	}

	// Re-enable: the section is back on the same live assembly, no restart.
	if _, err := surface.manager.Enable(ctx, agentext.CreationPluginID, agentext.AuditContext{Actor: "test"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	assertPrompt("enable", true)
	if _, ok := rt.Assembly().Tools().Get("creation_scaffold"); !ok {
		t.Fatal("the creation tool is not in the host registry after the enable")
	}
}

// pluginPromptTestRuntime boots the service runtime the plugin surface attaches
// to: the in-memory kernel, the run-row mirror and one real MCP stdio server
// (the fixture child), so the prompt assertions run against a prompt that
// genuinely carries an mcp_servers section.
func pluginPromptTestRuntime(t *testing.T) *servicehost.Runtime {
	t.Helper()
	log := discardLogger()
	mem := kernel.NewMemStore()
	k := kernel.New(mem, kernel.Options{})
	entry := mcp.ServerEntry{
		Name: "promptfixture",
		Config: map[string]any{
			"command":     os.Args[0],
			"args":        []any{"-test.run=^TestPluginPromptMCPHelper$"},
			"env":         map[string]any{pluginPromptMCPHelperEnv: "1"},
			"description": "prompt fixture",
		},
	}
	rt, err := servicehost.Boot(context.Background(), servicehost.Config{
		Seams: servicehost.Seams{
			Committer: k, Reader: k, Leases: k, External: k,
			Runs: pluginPromptMirror{}, Logger: log,
		},
		Tenant: host.TenantConfig{
			Agent: host.AgentConfig{
				SystemPrompt: "You are the prompt-seam test agent.",
				Sections:     []host.PromptSection{{Name: "tenant_rules", Content: "Tenant rule text."}},
			},
			MCP: host.MCPConfig{Servers: map[string]host.MCPServerConfig{
				"promptfixture": {Command: os.Args[0]},
			}},
		},
		Options:   loadout.Options{Revision: 1},
		RuntimeID: "plugin-prompt-test",
		MCP: servicehost.MCPOptions{
			Entries: []mcp.ServerEntry{entry},
			ConnectOptions: mcp.ConnectOptions{
				ClientName:    "keel-plugin-prompt-test",
				ClientVersion: "1.0.0",
				Stderr:        io.Discard,
				Logger:        log,
			},
		},
	})
	if err != nil {
		t.Fatalf("boot runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(context.Background()); err != nil {
			t.Errorf("close runtime: %v", err)
		}
	})
	return rt
}

// The MCP stdio fixture: a child of this test binary speaking just enough
// JSON-RPC (initialize, tools/list) for the manager to connect and publish a
// prompt section. Under a plain `go test` run it skips, so it never interferes
// with the rest of the package (the pattern internal/mcp uses for its own
// stdio tests).

const pluginPromptMCPHelperEnv = "KEEL_CMDKEEEL_MCP_HELPER"

func TestPluginPromptMCPHelper(t *testing.T) {
	if os.Getenv(pluginPromptMCPHelperEnv) != "1" {
		t.Skip("helper process entry point; set " + pluginPromptMCPHelperEnv + "=1")
	}
	os.Exit(runPluginPromptMCPHelper())
}

func runPluginPromptMCPHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), mcp.DefaultMaxMessageBytes)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		message, err := mcp.ParseMessage(line)
		if err != nil {
			continue
		}
		request, ok := message.(*mcp.Request)
		if !ok {
			continue // notifications/initialized and other notifications
		}
		respond := func(result any) {
			data, err := json.Marshal(result)
			if err != nil {
				return
			}
			raw, err := json.Marshal(&mcp.SuccessResponse{JSONRPC: mcp.JSONRPCVersion, ID: request.ID, Result: data})
			if err != nil {
				return
			}
			_, _ = os.Stdout.Write(append(raw, '\n'))
		}
		switch request.Method {
		case "initialize":
			respond(map[string]any{
				"protocolVersion": mcp.LatestProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "prompt-fixture", "version": "1.0.0"},
			})
		case "tools/list":
			respond(map[string]any{"tools": []any{
				map[string]any{"name": "fixture_echo", "inputSchema": map[string]any{"type": "object"}},
			}})
		default:
			respond(map[string]any{"jsonrpc": mcp.JSONRPCVersion, "id": request.ID, "error": map[string]any{
				"code": mcp.CodeMethodNotFound, "message": "Method not found: " + request.Method,
			}})
		}
	}
	return 0
}

// pluginPromptMirror is the run-row mirror seam; nothing in this test reads it.
type pluginPromptMirror struct{}

func (pluginPromptMirror) GetRun(context.Context, uuid.UUID, uuid.UUID) (store.Run, error) {
	return store.Run{}, store.ErrNoRun
}
