package loadout

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jixoai/keel/internal/assembly"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

// TestAssemblyBaseMatchesTheBlueprint pins the production adapter: every
// default service is one base row, requirements are the blueprint capability
// edges, and the whole base activates (availability-driven) with no hand-written
// order.
func TestAssemblyBaseMatchesTheBlueprint(t *testing.T) {
	baseDocument, err := AssemblyBase()
	if err != nil {
		t.Fatalf("AssemblyBase: %v", err)
	}
	services := DefaultServices()
	if len(baseDocument.Rows) != len(services) {
		t.Fatalf("base rows = %d, want %d", len(baseDocument.Rows), len(services))
	}
	modeDocument, err := AssemblyMode(assembly.ModeServe)
	if err != nil {
		t.Fatalf("AssemblyMode: %v", err)
	}
	built, err := assembly.Build([]assembly.Document{baseDocument, modeDocument})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if built.Len() != len(services) {
		t.Fatalf("effective rows = %d, want %d", built.Len(), len(services))
	}
	if blocked := built.Blocked(); len(blocked) != 0 {
		t.Fatalf("base blocked rows = %+v", blocked)
	}
	for _, service := range services {
		effective, ok := built.Row(ServiceRowID(service.Name))
		if !ok {
			t.Fatalf("service row %s missing", service.Name)
		}
		if effective.Impl != service.Version {
			t.Fatalf("%s impl = %q, want %q", service.Name, effective.Impl, service.Version)
		}
		if effective.Layer != assembly.LayerHarness {
			t.Fatalf("%s layer = %q", service.Name, effective.Layer)
		}
	}

	worker, _ := built.Row(ServiceRowID(ServiceWorker))
	wantWorker := []assembly.RowID{
		ServiceRowID(ServiceStore), ServiceRowID(ServicePiLoop),
		ServiceRowID(ServicePrompt), ServiceRowID(ServiceTools),
	}
	if !reflect.DeepEqual(worker.Requires, wantWorker) {
		t.Fatalf("worker requires = %v, want %v", worker.Requires, wantWorker)
	}
	positions := map[assembly.RowID]int{}
	for index, step := range built.Activation() {
		positions[step.ID] = index
	}
	for _, prerequisite := range wantWorker {
		if positions[prerequisite] >= positions[ServiceRowID(ServiceWorker)] {
			t.Fatalf("%s activates after worker: %v", prerequisite, positions)
		}
	}
}

func TestAssemblyBaseRejectsDuplicateCapabilityProviders(t *testing.T) {
	_, err := assemblyBase([]servicegraph.ServiceDescriptor{
		{Name: "one", Version: Version, Provides: []string{"ctx.shared"}},
		{Name: "two", Version: Version, Provides: []string{"ctx.shared"}},
	})
	if err == nil || !strings.Contains(err.Error(), "both provide capability") {
		t.Fatalf("duplicate capability error = %v, want a fail-closed blueprint", err)
	}
}

func TestAssemblyModeOnlyServeIsWired(t *testing.T) {
	if _, err := AssemblyMode(assembly.ModeServe); err != nil {
		t.Fatalf("serve mode: %v", err)
	}
	for _, mode := range []assembly.Mode{assembly.ModeHeadless, assembly.ModeTUI, assembly.ModeSDK} {
		if _, err := AssemblyMode(mode); err == nil {
			t.Fatalf("mode %q should fail until its bundle is wired (W8/W9)", mode)
		} else if !strings.Contains(err.Error(), "W8/W9") {
			t.Fatalf("mode %q error does not name the deferral: %v", mode, err)
		}
	}
	if _, err := AssemblyMode("browser"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

// TestAssembledServicesMatchesEnabledServices pins the adapter against the
// existing document→service mapping. If the two ever diverge, one of them is
// lying about what the deployment assembles.
func TestAssembledServicesMatchesEnabledServices(t *testing.T) {
	cases := []struct {
		name string
		cfg  host.TenantConfig
		opts Options
	}{
		{name: "empty", cfg: host.TenantConfig{}, opts: Options{}},
		{name: "worker", cfg: host.TenantConfig{}, opts: Options{Worker: true}},
		{
			name: "mcp",
			cfg: host.TenantConfig{MCP: host.MCPConfig{Servers: map[string]host.MCPServerConfig{
				"docs": {URL: "https://example.invalid/mcp"},
			}}},
		},
		{
			name: "compaction",
			cfg:  host.TenantConfig{Compaction: host.CompactionConfig{Enabled: boolPtr(true)}},
		},
		{
			name: "compaction explicitly off",
			cfg:  host.TenantConfig{Compaction: host.CompactionConfig{Enabled: boolPtr(false)}},
		},
		{
			name: "telemetry",
			cfg:  host.TenantConfig{Telemetry: host.TelemetryConfig{Adapter: "memory"}},
		},
		{
			name: "external backends and tools",
			cfg: host.TenantConfig{External: host.ExternalConfig{
				Backends: map[string]host.ExternalProviderConfig{"dsh": {Command: "/bin/dsh"}},
				Tools:    map[string]host.ExternalProviderConfig{"docs": {Command: "/bin/docs"}},
			}},
		},
		{
			name: "models document",
			cfg: host.TenantConfig{Models: json.RawMessage(`{
				"selection": {"providerId": "deepseek", "modelId": "deepseek-v4.1-flash"}
			}`)},
			opts: Options{Worker: true},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			want := EnabledServices(test.cfg, test.opts)
			got, err := AssembledServices(test.cfg, test.opts)
			if err != nil {
				t.Fatalf("AssembledServices: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("assembled services = %v, want %v", got, want)
			}
		})
	}
}

func TestAssemblyConfigIsAnOrderedOverlay(t *testing.T) {
	cfg := host.TenantConfig{
		MCP: host.MCPConfig{Servers: map[string]host.MCPServerConfig{
			"docs": {URL: "https://example.invalid/mcp"},
		}},
		Compaction: host.CompactionConfig{Enabled: boolPtr(true)},
	}
	configDocument, err := AssemblyConfig(cfg, Options{Worker: true})
	if err != nil {
		t.Fatalf("AssemblyConfig: %v", err)
	}
	rows := map[assembly.RowID]assembly.Row{}
	positions := map[assembly.RowID]int{}
	for index, row := range configDocument.Rows {
		rows[row.ID] = row
		positions[row.ID] = index
	}
	for _, service := range []string{ServiceMCP, ServiceCompaction, ServiceWorker} {
		if _, overridden := rows[ServiceRowID(service)]; overridden {
			t.Fatalf("%s is enabled and needs no config override: %+v", service, rows[ServiceRowID(service)])
		}
	}
	for _, service := range []string{ServiceSandbox, ServiceTelemetry, ServiceExternalAgent, ServiceExternalTools} {
		row, ok := rows[ServiceRowID(service)]
		if !ok || !row.Absent {
			t.Fatalf("%s should be an absent config row, got %+v", service, row)
		}
	}
	// Overlay rows must be in blueprint order or Build rejects them; the config
	// adapter never reorders.
	baseDocument, _ := AssemblyBase()
	basePositions := map[assembly.RowID]int{}
	for index, row := range baseDocument.Rows {
		basePositions[row.ID] = index
	}
	last := -1
	for _, row := range configDocument.Rows {
		position := basePositions[row.ID]
		if position <= last {
			t.Fatalf("config rows are not in effective order at %s", row.ID)
		}
		last = position
	}

	modeDocument, err := AssemblyMode(assembly.ModeServe)
	if err != nil {
		t.Fatalf("AssemblyMode: %v", err)
	}
	built, err := assembly.Build([]assembly.Document{baseDocument, modeDocument, configDocument})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, service := range []string{ServiceMCP, ServiceCompaction, ServiceWorker} {
		row, _ := built.Row(ServiceRowID(service))
		if row.Absent {
			t.Fatalf("%s should be active", service)
		}
	}
	sandbox, _ := built.Row(ServiceRowID(ServiceSandbox))
	if !sandbox.Absent || sandbox.Source != assembly.StageConfig {
		t.Fatalf("sandbox = %+v, want an absent config row", sandbox)
	}
	if blocked := built.Blocked(); len(blocked) != 0 {
		t.Fatalf("blocked = %+v", blocked)
	}
}

func TestAssemblyConfigCarriesDocumentDerivedImplementationVersions(t *testing.T) {
	cfg := host.TenantConfig{Models: json.RawMessage(`{
		"selection": {"providerId": "deepseek", "modelId": "deepseek-v4.1-flash"}
	}`)}
	configDocument, err := AssemblyConfig(cfg, Options{})
	if err != nil {
		t.Fatalf("AssemblyConfig: %v", err)
	}
	var found bool
	for _, row := range configDocument.Rows {
		if row.ID != ServiceRowID(ServiceLLM) {
			continue
		}
		found = true
		if !strings.Contains(row.Impl, "+models:") {
			t.Fatalf("llm config override = %+v, want the document-derived version", row)
		}
	}
	if !found {
		t.Fatal("the models document must override the llm row implementation")
	}

	baseDocument, _ := AssemblyBase()
	built, err := assembly.Build([]assembly.Document{baseDocument, configDocument})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	effective, _ := built.Row(ServiceRowID(ServiceLLM))
	if !effective.Overridden || effective.Source != assembly.StageConfig {
		t.Fatalf("llm provenance = %s/%v", effective.Source, effective.Overridden)
	}
}
