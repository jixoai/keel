package loadout

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jixoai/keel/internal/assembly"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

// This file is the bounded production adapter from the default service
// blueprint onto the W0.5 assembly model. It is an adapter, not a second
// runtime: internal/servicegraph remains the loader/activator, internal/loadout
// remains the single service blueprint, and internal/assembly only decides
// which rows the blueprint keeps after the mode and configuration overlay.
//
// What is real here:
//   - every default service is a base row (keel.<service>#service) whose
//     requires edges are derived from the blueprint's capability dependencies;
//   - the serve mode bundle is explicit (empty overrides today);
//   - the tenant document maps onto the config stage: services the document
//     does not enable are absent rows, and document-derived implementation
//     versions (the llm models slice) are impl overrides;
//   - FromTenantConfig derives the selected services from the assembled active
//     rows, so a blocked dependency fails loadout construction instead of
//     activating half-wired.
//
// What remains W8/W9 (named in docs/ASSEMBLY.md): reading config.toml from
// disk (the current configuration source is host.TenantConfig), the
// headless/tui/sdk mode bundles, and registering real local/remote seam
// providers for startup capability checks.

// ServiceRowID is the stable assembly row id of one default service:
// keel.<service>#service. The id is part of the assembly contract, so it must
// not be derived from a position.
func ServiceRowID(service string) assembly.RowID {
	row, ok := assembly.ParseRowID("keel." + service + "#service")
	if !ok {
		return assembly.RowID("keel." + service + "#service")
	}
	return row
}

// AssemblyBase builds the base bundle from the default service blueprint.
// Showcasing the four-layer model on the live service graph is the point: the
// base declares every default service and its availability edges, and the
// later stages may only remove or re-implement rows.
func AssemblyBase() (assembly.Document, error) {
	return assemblyBase(DefaultServices())
}

// assemblyBase is the testable form: the production blueprint and a synthetic
// one take the identical path, so capability-shape failures are pinned without
// mutating the package-level blueprint.
func assemblyBase(services []servicegraph.ServiceDescriptor) (assembly.Document, error) {
	providerByCapability := map[string]string{}
	for _, service := range services {
		for _, capability := range service.Provides {
			if existing, exists := providerByCapability[capability]; exists && existing != service.Name {
				return assembly.Document{}, fmt.Errorf(
					"loadout: services %q and %q both provide capability %q; a single-capability row must have one provider",
					existing, service.Name, capability)
			}
			providerByCapability[capability] = service.Name
		}
	}
	rows := make([]assembly.Row, 0, len(services))
	for _, service := range services {
		id := ServiceRowID(service.Name)
		if !id.Valid() {
			return assembly.Document{}, fmt.Errorf("loadout: service %q does not map to a valid assembly row id", service.Name)
		}
		requires := make([]assembly.RowID, 0, len(service.Requires))
		for _, capability := range service.Requires {
			provider, ok := providerByCapability[capability]
			if !ok {
				return assembly.Document{}, fmt.Errorf(
					"loadout: service %q requires capability %q that no blueprint service provides",
					service.Name, capability)
			}
			requires = append(requires, ServiceRowID(provider))
		}
		rows = append(rows, assembly.Row{
			ID:       id,
			Layer:    assembly.LayerHarness,
			Impl:     service.Version,
			Requires: requires,
		})
	}
	return assembly.Document{Stage: assembly.StageBase, Name: "keel-base", Rows: rows}, nil
}

// AssemblyMode returns the mode bundle for a run mode. Only the serve bundle is
// wired today: servicehost is the server process, and the base blueprint is
// the serve assembly. The other three bundles are W8/W9 work; asking for them
// fails instead of returning a silently empty bundle that claims to be
// headless/tui/sdk.
func AssemblyMode(mode assembly.Mode) (assembly.Document, error) {
	if _, ok := assembly.ParseMode(string(mode)); !ok {
		return assembly.Document{}, fmt.Errorf("loadout: unknown run mode %q", mode)
	}
	if mode != assembly.ModeServe {
		return assembly.Document{}, fmt.Errorf("loadout: mode %q bundle is not wired yet (W8/W9)", mode)
	}
	return assembly.Document{Stage: assembly.StageMode, Name: string(assembly.ModeServe)}, nil
}

// AssemblyConfig maps the tenant configuration document onto the config stage.
// A service the document does not enable becomes an absent row; a service whose
// implementation version is document-derived (the llm models slice) becomes an
// impl override. Rows stay in blueprint order, so an overlay cannot reorder the
// assembly.
func AssemblyConfig(cfg host.TenantConfig, opts Options) (assembly.Document, error) {
	enabled := map[string]bool{}
	for _, name := range EnabledServices(cfg, opts) {
		enabled[name] = true
	}
	versions := serviceVersions(cfg)
	rows := make([]assembly.Row, 0, len(defaultServices))
	for _, service := range defaultServices {
		id := ServiceRowID(service.Name)
		if !id.Valid() {
			return assembly.Document{}, fmt.Errorf("loadout: service %q does not map to a valid assembly row id", service.Name)
		}
		if !enabled[service.Name] {
			rows = append(rows, assembly.Row{ID: id, Absent: true})
			continue
		}
		if version := versions[service.Name]; version != "" {
			rows = append(rows, assembly.Row{ID: id, Impl: version})
		}
	}
	return assembly.Document{Stage: assembly.StageConfig, Name: "tenant-config", Rows: rows}, nil
}

// AssembledServices resolves the tenant document through base → serve mode →
// config and returns the active default services in blueprint order. It is the
// authoritative service selection used by FromTenantConfig: the overlay decides
// what exists, and an active row whose dependency did not survive the overlay
// fails the loadout instead of activating degraded silently.
func AssembledServices(cfg host.TenantConfig, opts Options) ([]string, error) {
	baseDocument, err := AssemblyBase()
	if err != nil {
		return nil, err
	}
	modeDocument, err := AssemblyMode(assembly.ModeServe)
	if err != nil {
		return nil, err
	}
	configDocument, err := AssemblyConfig(cfg, opts)
	if err != nil {
		return nil, err
	}
	built, err := assembly.Prepare([]assembly.Document{baseDocument, modeDocument, configDocument}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("loadout: assemble services: %w", err)
	}
	if blocked := built.Blocked(); len(blocked) > 0 {
		return nil, fmt.Errorf("loadout: assembly blocked: %s", describeBlocked(blocked))
	}
	serviceByRow := map[assembly.RowID]string{}
	for _, service := range defaultServices {
		serviceByRow[ServiceRowID(service.Name)] = service.Name
	}
	active := map[string]bool{}
	for _, row := range built.Active() {
		if service, ok := serviceByRow[row.ID]; ok {
			active[service] = true
		}
	}
	services := make([]string, 0, len(active))
	for _, service := range defaultServices {
		if active[service.Name] {
			services = append(services, service.Name)
		}
	}
	return services, nil
}

func describeBlocked(blocked []assembly.BlockedRow) string {
	parts := make([]string, 0, len(blocked))
	for _, row := range blocked {
		missing := make([]string, 0, len(row.Missing))
		for _, id := range row.Missing {
			missing = append(missing, string(id))
		}
		sort.Strings(missing)
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", row.ID, row.Reason, strings.Join(missing, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
