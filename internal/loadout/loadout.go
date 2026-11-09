// Package loadout builds the default Keel service loadout: the canonical set
// of services that reproduce cmd/runtime's assembly (store, llm, prompt,
// tools, mcp, pi-loop, telemetry, worker, ...), expressed as servicegraph
// descriptors.
//
// The package has no dependency on internal/store or any concrete
// implementation: the hosting process supplies factories through Config.
// Services are declared by capability, and a service whose factory is not
// provided is omitted from the loadout — its dependents then report missing or
// degraded dependencies instead of silently running with a nil handle.
package loadout

import (
	"github.com/jixoai/keel/internal/servicegraph"
)

// Service names of the default assembly.
const (
	ServiceStore             = "store"
	ServiceLLM               = "llm"
	ServicePrompt            = "prompt"
	ServiceMCP               = "mcp"
	ServiceTools             = "tools"
	ServicePiLoop            = "pi-loop"
	ServiceCompaction        = "compaction"
	ServiceSessionProjection = "session-projection"
	ServiceTelemetry         = "telemetry"
	ServiceWorker            = "worker"
	ServiceSandbox           = "sandbox"
	ServiceExternalAgent     = "external-agent"
	ServiceExternalTools     = "external-tools"
)

// Capability names of the default assembly. These are the vocabulary of the
// service graph: descriptors provide and require them, diagnostics speak them.
const (
	CapStore             = "ctx.store"
	CapLLM               = "ctx.llm"
	CapPrompt            = "ctx.prompt"
	CapMCP               = "ctx.mcp"
	CapTools             = "ctx.tools"
	CapLoop              = "ctx.loop"
	CapCompaction        = "ctx.compaction"
	CapSessionProjection = "ctx.session-projection"
	CapTelemetry         = "ctx.telemetry"
	CapWorker            = "ctx.worker"
	CapSandbox           = "ctx.sandbox"
	CapExternalAgent     = "ctx.external-agent"
	CapExternalTools     = "ctx.external-tools"
)

// Version is the descriptor version stamped on every default service. Any
// implementation change must bump it: the graph treats (name, version,
// provides, requires, optional, role) as the identity that decides whether a
// service is reloaded (see servicegraph identityEqual).
const Version = "v1"

// DefaultID is the loadout id used when Config.ID is empty.
const DefaultID = "default"

// DefaultServices returns the blueprint of the default assembly: descriptors
// without factories, in declaration order. Default attaches factories to these.
func DefaultServices() []servicegraph.ServiceDescriptor {
	out := make([]servicegraph.ServiceDescriptor, 0, len(defaultServices))
	for _, d := range defaultServices {
		d.Provides = append([]string(nil), d.Provides...)
		d.Requires = append([]string(nil), d.Requires...)
		d.Optional = append([]string(nil), d.Optional...)
		d.Effects = append([]string(nil), d.Effects...)
		out = append(out, d)
	}
	return out
}

// defaultServices mirrors cmd/runtime's boot sequence:
//
//	store → (llm, prompt, mcp) → tools → pi-loop → worker
//
// with telemetry, compaction, session-projection and sandbox available as
// attach points. Optional capabilities that are absent produce degraded
// diagnostics instead of blocking activation (sandbox, mcp, compaction,
// telemetry).
var defaultServices = []servicegraph.ServiceDescriptor{
	{
		Name:     ServiceStore,
		Version:  Version,
		Provides: []string{CapStore},
		Effects:  []string{"pg-pool"},
	},
	{
		Name:     ServiceLLM,
		Version:  Version,
		Provides: []string{CapLLM},
		Effects:  []string{"provider-client"},
	},
	{
		Name:     ServicePrompt,
		Version:  Version,
		Provides: []string{CapPrompt},
		// The host assembly consumes MCP tools/sections when MCP is enabled and
		// records a degraded diagnostic when it is not. sandbox is declared
		// optional although no sandbox service is implemented: dependents keep
		// running degraded instead of blocking on a service that cannot exist.
		// external-tools is the same kind of optional attachment: its
		// stateless tools join the one registry the run path uses.
		Optional: []string{CapMCP, CapSandbox, CapExternalTools},
		Effects:  []string{"prompt-assembly"},
	},
	{
		Name:     ServiceMCP,
		Version:  Version,
		Provides: []string{CapMCP},
		Effects:  []string{"mcp-connections"},
	},
	{
		Name:     ServiceSandbox,
		Version:  Version,
		Provides: []string{CapSandbox},
		Effects:  []string{"sandbox-process"},
	},
	{
		Name: ServiceTools,
		// The tool registry is built by the host assembly (skills, builtin and
		// MCP tools live in one registry), so the tools service is a view over
		// that host rather than a second, diverging registry.
		Version:  Version,
		Provides: []string{CapTools},
		Requires: []string{CapPrompt},
		Effects:  []string{"tool-registry"},
	},
	{
		Name:     ServicePiLoop,
		Version:  Version,
		Provides: []string{CapLoop},
		Requires: []string{CapLLM, CapTools},
		Optional: []string{CapCompaction},
		Effects:  []string{"loop-stream"},
	},
	{
		Name:     ServiceCompaction,
		Version:  Version,
		Provides: []string{CapCompaction},
		Requires: []string{CapLLM},
		Optional: []string{CapStore},
		Effects:  []string{"compaction-worker"},
	},
	{
		Name:     ServiceSessionProjection,
		Version:  Version,
		Provides: []string{CapSessionProjection},
		Requires: []string{CapStore},
		Effects:  []string{"projection-cache"},
	},
	{
		Name:     ServiceTelemetry,
		Version:  Version,
		Provides: []string{CapTelemetry},
		Effects:  []string{"exporter"},
	},
	{
		// External tool providers are stateless: they join the prompt's tool
		// registry (hence prompt's optional dependency) and their results go
		// back through the runner and the kernel's Commit. No session state.
		Name:     ServiceExternalTools,
		Version:  Version,
		Provides: []string{CapExternalTools},
		Effects:  []string{"external-tool-connections"},
	},
	{
		// External agent backends own their session state; the service needs
		// the durable store (external-session marks are persisted there) but
		// never the run/lease flow. It is a sibling of pi-loop, not a
		// replacement: native runs keep using pi-loop.
		Name:     ServiceExternalAgent,
		Version:  Version,
		Provides: []string{CapExternalAgent},
		Requires: []string{CapStore},
		Effects:  []string{"external-agent-connections"},
	},
	{
		Name:     ServiceWorker,
		Version:  Version,
		Provides: []string{CapWorker},
		Requires: []string{CapStore, CapLoop, CapPrompt, CapTools},
		Optional: []string{CapTelemetry},
		Effects:  []string{"worker-pool"},
	},
}

// Config is the constructor input for Default.
type Config struct {
	// ID names the loadout; defaults to DefaultID.
	ID string
	// Revision is the configuration version; defaults to 1.
	Revision int64
	// Version names the assembly; defaults to "keel-default@v1".
	Version string
	// Summary is the configuration digest (see Summary); recorded in
	// snapshots and run metadata.
	Summary string
	// Provide returns the factory for a default service by name. Returning
	// nil omits the service from the loadout. A nil Provide omits everything.
	Provide func(service string) servicegraph.Factory
	// LoopConformance overrides the blueprint's PI conformance declaration per
	// loop service. A loop service present in the loadout with no declaration
	// (here or in the blueprint) is rejected: the gate is fail-closed.
	LoopConformance []LoopConformance
	// LoopPins overrides the pinned PI conformance manifest hash per loop
	// service. The blueprint pins pi-loop to PinnedPiLoopManifestHash.
	LoopPins map[string]string
	// ServiceVersions overrides the implementation version of individual
	// services. The tenant-document adapter derives them for services whose
	// behavior is a function of the document (the llm service resolves the
	// run's provider configuration), so a reassembly rebuilds those instead of
	// sharing the stale instance.
	ServiceVersions map[string]string
}

// Default builds the default loadout, attaching the factories supplied by the
// hosting process. Services without a factory are omitted; their absence shows
// up as missing/degraded diagnostics in the assembled graph.
//
// Every loop service that survives factory selection must present a pinned PI
// conformance manifest (see ValidateLoopConformance); a loop that has not
// passed the pinned suite fails loadout construction instead of entering.
func Default(cfg Config) (*servicegraph.Loadout, error) {
	if cfg.ID == "" {
		cfg.ID = DefaultID
	}
	if cfg.Revision == 0 {
		cfg.Revision = 1
	}
	if cfg.Version == "" {
		cfg.Version = "keel-default@v1"
	}
	services := make([]servicegraph.ServiceDescriptor, 0, len(defaultServices))
	for _, d := range defaultServices {
		if version := cfg.ServiceVersions[d.Name]; version != "" {
			d.Version = version
		}
		var factory servicegraph.Factory
		if cfg.Provide != nil {
			factory = cfg.Provide(d.Name)
		}
		if factory == nil {
			continue
		}
		d.Factory = factory
		services = append(services, d)
	}
	if err := validateConfigLoopConformance(services, cfg.LoopConformance, cfg.LoopPins); err != nil {
		return nil, err
	}
	return servicegraph.NewLoadout(servicegraph.LoadoutConfig{
		ID:       cfg.ID,
		Revision: cfg.Revision,
		Version:  cfg.Version,
		Summary:  cfg.Summary,
		Services: services,
	})
}
