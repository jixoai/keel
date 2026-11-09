package loadout

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

type stubInstance struct{ name string }

func (s *stubInstance) Start(context.Context, *servicegraph.EffectScope) error { return nil }
func (s *stubInstance) Stop(context.Context) error                             { return nil }

func stubFactory(name string) servicegraph.Factory {
	return func(context.Context, servicegraph.Resolved) (servicegraph.Instance, error) {
		return &stubInstance{name: name}, nil
	}
}

// provideAll returns a Provide function that binds factories for the named
// services only, mirroring how the hosting process decides what exists.
func provideAll(names ...string) func(string) servicegraph.Factory {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return func(name string) servicegraph.Factory {
		if set[name] {
			return stubFactory(name)
		}
		return nil
	}
}

func allServiceNames() []string {
	return []string{
		ServiceStore, ServiceLLM, ServicePrompt, ServiceMCP, ServiceTools,
		ServicePiLoop, ServiceCompaction, ServiceSessionProjection,
		ServiceTelemetry, ServiceWorker, ServiceSandbox,
		ServiceExternalAgent, ServiceExternalTools,
	}
}

func applyLoadout(t *testing.T, lo *servicegraph.Loadout) *servicegraph.Snapshot {
	t.Helper()
	e := servicegraph.NewEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("Apply: %v\nsnapshot:\n%s", err, snap)
	}
	return snap
}

func boolPtr(v bool) *bool { return &v }

func TestDefaultServicesBlueprintCoherent(t *testing.T) {
	services := DefaultServices()
	if len(services) == 0 {
		t.Fatal("blueprint is empty")
	}
	names := map[string]bool{}
	provided := map[string]bool{}
	for _, d := range services {
		if d.Name == "" || d.Version == "" {
			t.Fatalf("descriptor missing name/version: %+v", d)
		}
		if len(d.Provides) == 0 {
			t.Fatalf("service %s declares no provides", d.Name)
		}
		if names[d.Name] {
			t.Fatalf("service %s declared twice", d.Name)
		}
		names[d.Name] = true
		if d.Factory != nil {
			t.Fatalf("blueprint service %s ships a factory; hosting processes attach implemented ones", d.Name)
		}
		for _, capability := range d.Provides {
			provided[capability] = true
		}
	}
	// Every referenced capability must have a provider in the blueprint, or
	// the default loadout could never be fully assembled.
	for _, d := range services {
		caps := append(append([]string(nil), d.Requires...), d.Optional...)
		for _, capability := range caps {
			if !provided[capability] {
				t.Errorf("capability %q required/optional by %s has no provider in the blueprint", capability, d.Name)
			}
		}
	}
	for _, want := range allServiceNames() {
		if !names[want] {
			t.Errorf("service %q missing from the blueprint", want)
		}
	}
}

func TestDefaultAllServicesAssemble(t *testing.T) {
	cfg := host.TenantConfig{
		Skills:     host.SkillsConfig{Roots: []string{"/skills"}},
		MCP:        host.MCPConfig{Servers: map[string]host.MCPServerConfig{"filesystem": {Command: "npx"}}},
		Compaction: host.CompactionConfig{Enabled: boolPtr(true)},
		Telemetry:  host.TelemetryConfig{Adapter: "memory"},
		Tools:      host.ToolsConfig{Allow: []string{"clock"}},
	}
	opts := Options{ID: "tenant-a", Revision: 7, Worker: true, RunConcurrency: 64}
	lo, err := FromTenantConfig(cfg, opts, provideAll(allServiceNames()...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	if lo.ID() != "tenant-a" || lo.Revision() != 7 || lo.Summary() != Summary(cfg, opts) {
		t.Fatalf("header: %s %d %q", lo.ID(), lo.Revision(), lo.Summary())
	}
	// The sandbox has no implementation and this document declares no external
	// providers; every other blueprint service is on.
	if lo.Len() != len(DefaultServices())-3 {
		t.Fatalf("len: %d, want %d", lo.Len(), len(DefaultServices())-3)
	}
	snap := applyLoadout(t, lo)
	for _, name := range []string{
		ServiceStore, ServiceLLM, ServicePrompt, ServiceMCP, ServiceTools,
		ServicePiLoop, ServiceCompaction, ServiceTelemetry, ServiceWorker,
		ServiceSessionProjection,
	} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE:\n%s", name, snap)
		}
	}
	if _, ok := snap.Service(ServiceSandbox); ok {
		t.Fatal("sandbox has no implementation and must stay out of the default loadout")
	}
	worker, _ := snap.Service(ServiceWorker)
	if worker.Bindings[CapStore] != ServiceStore ||
		worker.Bindings[CapLoop] != ServicePiLoop ||
		worker.Bindings[CapPrompt] != ServicePrompt ||
		worker.Bindings[CapTools] != ServiceTools ||
		worker.Bindings[CapTelemetry] != ServiceTelemetry {
		t.Fatalf("worker bindings: %v", worker.Bindings)
	}
	loop, _ := snap.Service(ServicePiLoop)
	if loop.Bindings[CapLLM] != ServiceLLM || loop.Bindings[CapTools] != ServiceTools || loop.Bindings[CapCompaction] != ServiceCompaction {
		t.Fatalf("pi-loop bindings: %v", loop.Bindings)
	}
	tools, _ := snap.Service(ServiceTools)
	if tools.Bindings[CapPrompt] != ServicePrompt {
		t.Fatalf("tools bindings: %v", tools.Bindings)
	}
	if snap.LoadoutID != "tenant-a" || snap.Revision != 7 || snap.Summary == "" {
		t.Fatalf("snapshot header: %+v", snap)
	}
}

// TestDefaultMinimalAssemblyDegraded mirrors today's cmd/runtime boot: store,
// llm, prompt, tools, pi-loop, session-projection and worker, with no MCP
// config, sandbox, telemetry or compaction.
func TestDefaultMinimalAssemblyDegraded(t *testing.T) {
	minimal := []string{
		ServiceStore, ServiceLLM, ServicePrompt, ServiceTools,
		ServicePiLoop, ServiceSessionProjection, ServiceWorker,
	}
	cfg := host.TenantConfig{}
	opts := Options{Worker: true, RunConcurrency: 64}
	lo, err := FromTenantConfig(cfg, opts, provideAll(minimal...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	snap := applyLoadout(t, lo)
	for _, name := range minimal {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE degraded:\n%s", name, snap)
		}
	}
	for _, absent := range []string{ServiceMCP, ServiceSandbox, ServiceTelemetry, ServiceCompaction} {
		if _, ok := snap.Service(absent); ok {
			t.Fatalf("%s must be absent", absent)
		}
	}
	prompt, _ := snap.Service(ServicePrompt)
	if strings.Join(prompt.Degraded, ",") != CapExternalTools+","+CapMCP+","+CapSandbox {
		t.Fatalf("prompt degraded: %v", prompt.Degraded)
	}
	loop, _ := snap.Service(ServicePiLoop)
	if strings.Join(loop.Degraded, ",") != CapCompaction {
		t.Fatalf("pi-loop degraded: %v", loop.Degraded)
	}
	worker, _ := snap.Service(ServiceWorker)
	if strings.Join(worker.Degraded, ",") != CapTelemetry {
		t.Fatalf("worker degraded: %v", worker.Degraded)
	}
	diags := snap.DiagnosticsFor(ServicePrompt)
	found := false
	for _, d := range diags {
		if d.Code == servicegraph.DiagOptionalMissing && strings.Contains(d.Message, "ctx.mcp") {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt diagnostics should explain the missing mcp capability: %+v", diags)
	}
	if explain := snap.Explain(ServicePiLoop); !strings.Contains(explain, CapCompaction) {
		t.Fatalf("pi-loop explain should mention compaction:\n%s", explain)
	}
}

func TestDefaultMissingStoreKeepsDependentsPending(t *testing.T) {
	// Everything except the store: worker and session-projection depend on it,
	// pi-loop does not.
	names := []string{ServiceLLM, ServicePrompt, ServiceTools, ServicePiLoop, ServiceWorker, ServiceSessionProjection}
	lo, err := FromTenantConfig(host.TenantConfig{}, Options{Worker: true}, provideAll(names...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	e := servicegraph.NewEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("missing provider is a PENDING condition, not an activation failure: %v", err)
	}
	if !snap.Active(ServicePiLoop) {
		t.Fatalf("pi-loop does not need the store and must run:\n%s", snap)
	}
	for _, name := range []string{ServiceWorker, ServiceSessionProjection} {
		if snap.State(name) != servicegraph.StatePending {
			t.Fatalf("%s should be PENDING, got %s", name, snap.State(name))
		}
	}
	explain := snap.Explain(ServiceWorker)
	if !strings.Contains(explain, CapStore) || !strings.Contains(explain, "missing-required") {
		t.Fatalf("worker explain should name the missing capability:\n%s", explain)
	}
}

func TestDefaultMissingPromptKeepsToolsPending(t *testing.T) {
	// The tool registry is the host assembly's registry; without a prompt
	// service, tools must wait instead of building a second registry.
	names := []string{ServiceStore, ServiceLLM, ServiceTools, ServicePiLoop, ServiceSessionProjection}
	lo, err := FromTenantConfig(host.TenantConfig{}, Options{}, provideAll(names...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	e := servicegraph.NewEngine()
	snap, err := e.Apply(context.Background(), lo)
	if err != nil {
		t.Fatalf("missing provider is a PENDING condition, not an activation failure: %v", err)
	}
	for _, name := range []string{ServiceTools, ServicePiLoop} {
		if snap.State(name) != servicegraph.StatePending {
			t.Fatalf("%s should be PENDING without prompt, got %s", name, snap.State(name))
		}
	}
	if explain := snap.Explain(ServiceTools); !strings.Contains(explain, CapPrompt) {
		t.Fatalf("tools explain should name the missing prompt capability:\n%s", explain)
	}
}

func TestDefaultHeaderDefaults(t *testing.T) {
	lo, err := FromTenantConfig(host.TenantConfig{}, Options{}, provideAll(ServiceLLM))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	if lo.ID() != DefaultID || lo.Revision() != 1 || lo.Version() != "keel-default@v1" {
		t.Fatalf("defaults: %s %d %s", lo.ID(), lo.Revision(), lo.Version())
	}
	if lo.Len() != 1 {
		t.Fatalf("len: %d", lo.Len())
	}
}

func TestTenantConfigSummary(t *testing.T) {
	cfg := host.TenantConfig{
		Agent:      host.AgentConfig{Provider: "openai", Model: "gpt-5"},
		Skills:     host.SkillsConfig{Roots: []string{"/a", "/b"}},
		MCP:        host.MCPConfig{Servers: map[string]host.MCPServerConfig{"filesystem": {Command: "npx"}}},
		Hooks:      host.HooksConfig{Commands: []host.HookCommand{{Event: "tool_execution_start", Command: []string{"./hook.sh"}}}},
		Compaction: host.CompactionConfig{Enabled: boolPtr(true), KeepRecentTokens: 1000},
		Telemetry:  host.TelemetryConfig{Adapter: "memory"},
		Tools:      host.ToolsConfig{Allow: []string{"clock", "echo"}},
		Quota:      host.QuotaConfig{RunConcurrency: 4, EventsPerMinute: 120},
	}
	opts := Options{Worker: true, RunConcurrency: 64}
	first := Summary(cfg, opts)
	if first != Summary(cfg, opts) {
		t.Fatal("Summary must be deterministic")
	}
	for _, want := range []string{
		"agent=openai/gpt-5", "skills=2", "mcp=1", "hooks=1", "compaction=true",
		"telemetry=memory", "tools=2", "sandbox=false", "quota-runs=4",
		"quota-events=120", "worker=true", "runs=64",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("summary missing %q: %s", want, first)
		}
	}
	if got := Summary(host.TenantConfig{}, Options{}); got != "agent=fake;skills=0;mcp=0;hooks=0;compaction=false;telemetry=none;tools=0;sandbox=false;external-backends=0;external-tools=0;external-unmetered=none;quota-runs=0;quota-events=0;worker=false;runs=0" {
		t.Fatalf("empty config summary: %s", got)
	}
	// Hook commands may carry credentials; only their count enters the digest.
	if strings.Contains(first, "hook.sh") {
		t.Fatal("summary must not include hook commands")
	}
	// An explicitly disabled skill root set reports zero skills, not the roots.
	disabled := host.SkillsConfig{Enabled: boolPtr(false), Roots: []string{"/a", "/b"}}
	if got := Summary(host.TenantConfig{Skills: disabled}, Options{}); !strings.Contains(got, "skills=0") {
		t.Fatalf("disabled skills should report 0: %s", got)
	}
}

func TestFromTenantConfigSelection(t *testing.T) {
	full := host.TenantConfig{
		MCP:        host.MCPConfig{Servers: map[string]host.MCPServerConfig{"filesystem": {Command: "npx"}}},
		Compaction: host.CompactionConfig{Enabled: boolPtr(true)},
		Telemetry:  host.TelemetryConfig{Adapter: "memory"},
	}
	opts := Options{ID: "tenant-a", Revision: 3, Worker: true, RunConcurrency: 64}
	lo, err := FromTenantConfig(full, opts, provideAll(allServiceNames()...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	if lo.ID() != "tenant-a" || lo.Revision() != 3 || lo.Summary() != Summary(full, opts) {
		t.Fatalf("header: %s %d %q", lo.ID(), lo.Revision(), lo.Summary())
	}
	snap := applyLoadout(t, lo)
	for _, name := range []string{
		ServiceStore, ServiceLLM, ServicePrompt, ServiceMCP, ServiceTools,
		ServicePiLoop, ServiceCompaction, ServiceTelemetry, ServiceWorker,
		ServiceSessionProjection,
	} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE:\n%s", name, snap)
		}
	}
	if _, ok := snap.Service(ServiceSandbox); ok {
		t.Fatal("sandbox is not enabled by a tenant document (no implementation yet)")
	}
}

func TestFromTenantConfigMinimal(t *testing.T) {
	// Mirrors today's cmd/runtime boot: no MCP config, no sandbox, no
	// telemetry, no compaction; worker on.
	opts := Options{Worker: true, RunConcurrency: 64}
	lo, err := FromTenantConfig(host.TenantConfig{}, opts, provideAll(allServiceNames()...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	snap := applyLoadout(t, lo)
	for _, name := range []string{
		ServiceStore, ServiceLLM, ServicePrompt, ServiceSessionProjection,
		ServiceTools, ServicePiLoop, ServiceWorker,
	} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE degraded:\n%s", name, snap)
		}
	}
	for _, absent := range []string{ServiceMCP, ServiceSandbox, ServiceTelemetry, ServiceCompaction} {
		if _, ok := snap.Service(absent); ok {
			t.Fatalf("%s must be disabled by this config", absent)
		}
	}
	prompt, _ := snap.Service(ServicePrompt)
	if strings.Join(prompt.Degraded, ",") != CapExternalTools+","+CapMCP+","+CapSandbox {
		t.Fatalf("prompt degraded: %v", prompt.Degraded)
	}
}

func TestFromTenantConfigRejectsInvalidDocument(t *testing.T) {
	bad := host.TenantConfig{Quota: host.QuotaConfig{RunConcurrency: -1}}
	if _, err := FromTenantConfig(bad, Options{}, provideAll(allServiceNames()...)); err == nil ||
		!strings.Contains(err.Error(), "quota.runConcurrency") {
		t.Fatalf("invalid tenant document must be rejected with the field path, got %v", err)
	}
}

// TestModelConfigurationVersionsTheLLMService pins the reassembly identity of
// the llm service: a document that carries provider configuration must produce
// a distinguishable version (so a Reload rebuilds the resolver instead of
// sharing the stale instance), the same document must stay stable, and a
// document without provider configuration must keep the blueprint version.
func TestModelConfigurationVersionsTheLLMService(t *testing.T) {
	llmVersion := func(t *testing.T, cfg host.TenantConfig) string {
		t.Helper()
		lo, err := FromTenantConfig(cfg, Options{}, provideAll(allServiceNames()...))
		if err != nil {
			t.Fatalf("FromTenantConfig: %v", err)
		}
		desc, ok := lo.Service(ServiceLLM)
		if !ok {
			t.Fatal("llm service missing from the loadout")
		}
		return desc.Version
	}

	baseline := llmVersion(t, host.TenantConfig{})
	modelsA := llmVersion(t, host.TenantConfig{
		Models: json.RawMessage(`{"selection":{"providerId":"a6api","modelId":"deepseek-v4.1-flash"}}`),
	})
	if modelsA == baseline {
		t.Fatal("a models document must change the llm service version")
	}
	modelsB := llmVersion(t, host.TenantConfig{
		Models: json.RawMessage(`{"selection":{"providerId":"deepseek","modelId":"deepseek-v4.1-flash"}}`),
	})
	if modelsB == modelsA {
		t.Fatal("two different models documents must produce different llm versions")
	}
	if again := llmVersion(t, host.TenantConfig{
		Models: json.RawMessage(`{"selection":{"providerId":"a6api","modelId":"deepseek-v4.1-flash"}}`),
	}); again != modelsA {
		t.Fatalf("the version must be deterministic: %q != %q", again, modelsA)
	}
	// The legacy flat agent fields are part of the same slice: the resolver
	// reads them, so a change must be visible to reassembly too.
	legacy := llmVersion(t, host.TenantConfig{Agent: host.AgentConfig{Model: "deepseek-v4.1-flash", Protocol: "openai"}})
	if legacy == baseline {
		t.Fatal("a legacy agent model must change the llm service version")
	}
}

// TestExternalServicesSelectedByDocument pins the document -> service mapping
// for the external gateway: one enabled provider selects its service, an
// explicitly disabled provider selects nothing, and the summary records the
// explicit unmetered policy.
func TestExternalServicesSelectedByDocument(t *testing.T) {
	cfg := host.TenantConfig{
		External: host.ExternalConfig{
			Backends: map[string]host.ExternalProviderConfig{
				"dsh":      {Command: "/bin/dsh"},
				"disabled": {Command: "/bin/off", Enabled: boolPtr(false)},
			},
			Tools: map[string]host.ExternalProviderConfig{
				"docs": {Command: "/bin/docs"},
			},
			Quota: host.ExternalQuotaConfig{Unmetered: host.ExternalUnmeteredAllow, MaxSessionsPerBackend: 4},
		},
	}
	services := EnabledServices(cfg, Options{})
	selected := map[string]bool{}
	for _, name := range services {
		selected[name] = true
	}
	if !selected[ServiceExternalAgent] || !selected[ServiceExternalTools] {
		t.Fatalf("external services not selected: %v", services)
	}
	summary := Summary(cfg, Options{})
	for _, want := range []string{"external-backends=1", "external-tools=1", "external-unmetered=allow"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	lo, err := FromTenantConfig(cfg, Options{}, provideAll(allServiceNames()...))
	if err != nil {
		t.Fatalf("FromTenantConfig: %v", err)
	}
	snap := applyLoadout(t, lo)
	for _, name := range []string{ServiceExternalAgent, ServiceExternalTools} {
		if !snap.Active(name) {
			t.Fatalf("%s should be ACTIVE: %s", name, snap)
		}
	}
	// A document with only disabled providers selects neither service.
	empty := host.TenantConfig{External: host.ExternalConfig{
		Backends: map[string]host.ExternalProviderConfig{"off": {Command: "/bin/off", Enabled: boolPtr(false)}},
	}}
	for _, name := range EnabledServices(empty, Options{}) {
		if name == ServiceExternalAgent || name == ServiceExternalTools {
			t.Fatalf("disabled providers selected %s", name)
		}
	}
}

// TestSummaryNamesTheModelsDocumentIdentity pins the digest's honesty for a
// document-driven deployment: a tenant configured through a models document has
// no flat agent fields, and describing a real model run as "agent=fake" would
// misreport it.
func TestSummaryNamesTheModelsDocumentIdentity(t *testing.T) {
	cfg := host.TenantConfig{Models: json.RawMessage(`{
	  "selection": {"providerId": "s2api", "modelId": "deepseek-v4.1-flash"}
	}`)}
	summary := Summary(cfg, Options{})
	if !strings.Contains(summary, "agent=s2api/deepseek-v4.1-flash") {
		t.Fatalf("summary = %q, want the document's default selection", summary)
	}
	if empty := Summary(host.TenantConfig{}, Options{}); !strings.Contains(empty, "agent=fake") {
		t.Fatalf("an unconfigured deployment is the fake path: %q", empty)
	}
}
