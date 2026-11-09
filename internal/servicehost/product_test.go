package servicehost

// Product assembly points (keel-agent): the extension surface touches the run
// path only through these two decorators plus the dynamic tool refresh. The
// tests pin that a kernel-only deployment (no Product options) stays untouched
// and that a product deployment really does wrap what it says it wraps.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/loadout"
	"github.com/jixoai/keel/internal/provider"
	"github.com/jixoai/keel/internal/servicegraph"
	"github.com/jixoai/keel/internal/telemetry"
)

func productTestConfig(product ProductOptions) Config {
	return Config{
		Seams:   Seams{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		Product: product,
	}
}

func TestProductTelemetryWrapInstalled(t *testing.T) {
	wrapped := false
	cfg := productTestConfig(ProductOptions{WrapTelemetry: func(inner telemetry.Context) telemetry.Context {
		wrapped = true
		if inner == nil {
			t.Fatal("the base telemetry context must be supplied")
		}
		return inner
	}})
	instance, err := newTelemetryFactory(cfg)(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service, ok := instance.(*telemetryService)
	if !ok || service.RunTelemetry() == nil || service.RunTelemetry().Context == nil {
		t.Fatalf("telemetry service = %T", instance)
	}
	if !wrapped {
		t.Fatal("Product.WrapTelemetry was not applied")
	}

	// Kernel-only deployment: no wrapper, the plain memory adapter is used.
	instance, err = newTelemetryFactory(productTestConfig(ProductOptions{}))(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	if got := instance.(*telemetryService).RunTelemetry().Context; got == nil {
		t.Fatal("default telemetry context is nil")
	}
}

func TestProductStreamWrapInstalled(t *testing.T) {
	var seen bool
	cfg := productTestConfig(ProductOptions{WrapStream: func(inner provider.StreamFn) provider.StreamFn {
		seen = true
		if inner == nil {
			t.Fatal("the base stream must be supplied")
		}
		return inner
	}})
	instance, err := newLLMFactory(cfg.Tenant, cfg)(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	service, ok := instance.(*llmService)
	if !ok || service.StreamFn() == nil {
		t.Fatalf("llm service = %T", instance)
	}
	if !seen {
		t.Fatal("Product.WrapStream was not applied to the fake provider path")
	}
}

func TestProductExtraToolsJoinTheRegistry(t *testing.T) {
	// ExtraTools is a static contribution; a duplicate model-facing name is a
	// fail-closed assembly error (two tools cannot share one name).
	tool := productStubTool{name: "product_tool"}
	prompt, err := newPromptFactory(productTestConfig(ProductOptions{ExtraTools: []agentcore.AgentTool{tool}}))(context.Background(), servicegraph.Resolved{})
	if err != nil {
		t.Fatal(err)
	}
	registry := prompt.(*promptService).Tools()
	if _, ok := registry.Get("product_tool"); !ok {
		t.Fatal("product tool is not in the host registry")
	}
	dup := newPromptFactory(productTestConfig(ProductOptions{ExtraTools: []agentcore.AgentTool{tool, tool}}))
	if _, err := dup(context.Background(), servicegraph.Resolved{}); err == nil {
		t.Fatal("a duplicate product tool must fail host assembly")
	}
}

// TestRefreshProductToolsPublishesDynamicTools pins the plugin-tool refresh: a
// plugin tool joins the one host registry, builtins survive the swap, and an
// unload removes it again (the registry is replaced, never mutated in place).
func TestRefreshProductToolsPublishesDynamicTools(t *testing.T) {
	rt := bootForTest(t, host.TenantConfig{}, loadout.Options{})
	if err := rt.RefreshProductTools([]agentcore.AgentTool{productStubTool{name: "plugin_tool"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); !ok {
		t.Fatal("dynamic plugin tool is not in the host registry")
	}
	if _, ok := rt.Assembly().Tools().Get("clock"); !ok {
		t.Fatal("RefreshProductTools dropped the builtin tools")
	}
	if err := rt.RefreshProductTools(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.Assembly().Tools().Get("plugin_tool"); ok {
		t.Fatal("an unloaded plugin tool survived the refresh")
	}
}

type productStubTool struct{ name string }

func (t productStubTool) Name() string            { return t.name }
func (t productStubTool) Description() string     { return "stub" }
func (t productStubTool) Schema() json.RawMessage { return nil }
func (t productStubTool) ExecutionMode() agentcore.ToolExecutionMode {
	return agentcore.ToolExecutionParallel
}
func (t productStubTool) Execute(context.Context, string, json.RawMessage, agentcore.ToolUpdateFunc) (agentcore.AgentToolResult, error) {
	return agentcore.AgentToolResult{}, nil
}
