package loadout

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/servicegraph"
)

// This file is the single-source adapter from a tenant configuration document
// to an immutable loadout. The document type is host.TenantConfig — the exact
// contract the admin API writes and validates — so the loadout never speaks a
// second configuration vocabulary: adding a tenant setting means adding it to
// internal/host, and this adapter decides which service it switches on.
//
// Resolution is a pure function of (document, hosting options): the same
// document always yields the same service set and the same summary. A
// configuration change produces a new revision that takes effect for the next
// run; a run that already started pins the old revision through its
// servicegraph.Lease.

// Options carries the hosting-process inputs that are not part of a tenant's
// configuration document. They describe how this process runs, not what the
// tenant asked for: whether it starts a worker and how many runs it executes at
// once, plus the snapshot identity recorded with each run.
type Options struct {
	// ID names the loadout; defaults to DefaultID.
	ID string
	// Revision is the tenant document revision the snapshot was resolved from;
	// defaults to 1. Runs pin it.
	Revision int64
	// Version names the assembly; defaults to "keel-default@v1".
	Version string
	// Worker starts the agent worker in this process (runtime -no-worker
	// disables it). The worker is a process capability, not a tenant setting.
	Worker bool
	// RunConcurrency is the process-wide worker concurrency (0 = runtime
	// default).
	RunConcurrency int
}

// EnabledServices resolves the tenant document to the default services it turns
// on, in blueprint (declaration) order.
//
// The mapping is the whole adapter:
//
//   - store, llm, prompt, tools, pi-loop and session-projection are the
//     irreducible run path and are always present;
//   - mcp switches on when the document lists at least one MCP server;
//   - compaction switches on only when compaction.enabled is explicitly true;
//   - telemetry switches on when telemetry.adapter names an adapter;
//   - external-agent switches on when the document declares an enabled agent
//     backend; external-tools switches on the same way for tool providers;
//   - worker switches on from the hosting Options (a process capability).
//
// sandbox has no implemented service and no tenant setting: it is never
// enabled, and the blueprint declares it optional so dependents record a
// degraded diagnostic instead of a fake factory.
func EnabledServices(cfg host.TenantConfig, opts Options) []string {
	enabled := map[string]bool{
		ServiceStore:             true,
		ServiceLLM:               true,
		ServicePrompt:            true,
		ServiceSessionProjection: true,
		ServiceTools:             true,
		ServicePiLoop:            true,
	}
	if len(cfg.MCP.Servers) > 0 {
		enabled[ServiceMCP] = true
	}
	if compactionEnabled(cfg.Compaction) {
		enabled[ServiceCompaction] = true
	}
	if telemetryEnabled(cfg.Telemetry.Adapter) {
		enabled[ServiceTelemetry] = true
	}
	if enabledExternalProviders(cfg.External.Backends) > 0 {
		enabled[ServiceExternalAgent] = true
	}
	if enabledExternalProviders(cfg.External.Tools) > 0 {
		enabled[ServiceExternalTools] = true
	}
	if opts.Worker {
		enabled[ServiceWorker] = true
	}
	var out []string
	for _, d := range defaultServices {
		if enabled[d.Name] {
			out = append(out, d.Name)
		}
	}
	return out
}

// Summary is the deterministic configuration digest recorded with the loadout
// revision. It is human-readable on purpose: handoff audits compare it, and it
// names only counts/flags — never a secret value or a secret reference.
func Summary(cfg host.TenantConfig, opts Options) string {
	agent := strings.TrimPrefix(strings.TrimSpace(cfg.Agent.Provider)+"/"+strings.TrimSpace(cfg.Agent.Model), "/")
	if agent == "" {
		agent = "fake"
	}
	telemetry := strings.TrimSpace(cfg.Telemetry.Adapter)
	if telemetry == "" {
		telemetry = "none"
	}
	parts := []string{
		"agent=" + agent,
		"skills=" + strconv.Itoa(skillCount(cfg.Skills)),
		"mcp=" + strconv.Itoa(len(cfg.MCP.Servers)),
		"hooks=" + strconv.Itoa(len(cfg.Hooks.Commands)),
		"compaction=" + strconv.FormatBool(compactionEnabled(cfg.Compaction)),
		"telemetry=" + telemetry,
		"tools=" + strconv.Itoa(len(cfg.Tools.Allow)),
		"sandbox=false",
		"external-backends=" + strconv.Itoa(enabledExternalProviders(cfg.External.Backends)),
		"external-tools=" + strconv.Itoa(enabledExternalProviders(cfg.External.Tools)),
		"external-unmetered=" + externalUnmetered(cfg.External.Quota.Unmetered),
		"quota-runs=" + strconv.Itoa(cfg.Quota.RunConcurrency),
		"quota-events=" + strconv.Itoa(cfg.Quota.EventsPerMinute),
		"worker=" + strconv.FormatBool(opts.Worker),
		"runs=" + strconv.Itoa(opts.RunConcurrency),
	}
	return strings.Join(parts, ";")
}

// FromTenantConfig resolves a tenant document into an immutable loadout: the
// document decides which default services exist, the hosting process supplies
// their factories through provide. Services whose factory is missing are
// omitted; their dependents report missing/degraded dependencies instead of
// silently running without them.
//
// The document is validated here as well: a boot-time default and an
// administrator write go through exactly the same rules.
func FromTenantConfig(cfg host.TenantConfig, opts Options, provide func(service string) servicegraph.Factory) (*servicegraph.Loadout, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("loadout: %w", err)
	}
	enabled := map[string]bool{}
	for _, name := range EnabledServices(cfg, opts) {
		enabled[name] = true
	}
	filtered := func(name string) servicegraph.Factory {
		if provide == nil || !enabled[name] {
			return nil
		}
		return provide(name)
	}
	return Default(Config{
		ID:              opts.ID,
		Revision:        opts.Revision,
		Version:         opts.Version,
		Summary:         Summary(cfg, opts),
		Provide:         filtered,
		ServiceVersions: serviceVersions(cfg),
	})
}

// serviceVersions derives per-service implementation versions from the tenant
// document. A service whose instance behavior is a function of the document
// must change its version with that content: reassembly compares
// (name, version, provides, requires, optional, role) and never the factory, so
// an unchanged version would keep sharing the old instance — and its frozen
// configuration — with the new generation.
func serviceVersions(cfg host.TenantConfig) map[string]string {
	digest := modelConfigDigest(cfg)
	if digest == "" {
		return nil
	}
	return map[string]string{ServiceLLM: Version + "+models:" + digest}
}

// modelConfigDigest digests the model-configuration slice of the document: the
// models section plus the legacy flat agent fields the same resolver reads. It
// is empty when the document carries neither, so a deployment without provider
// configuration keeps the blueprint version (and reload sharing) unchanged.
//
// The digest never contains the values themselves, only a one-way hash of them.
func modelConfigDigest(cfg host.TenantConfig) string {
	models := bytes.TrimSpace(cfg.Models)
	legacy := strings.Join([]string{
		cfg.Agent.Provider, cfg.Agent.Model, cfg.Agent.BaseURL,
		cfg.Agent.Protocol, cfg.Agent.APIKeyRef,
	}, "\x00")
	if len(models) == 0 && strings.Trim(legacy, "\x00") == "" {
		return ""
	}
	hash := sha256.New()
	hash.Write(models)
	hash.Write([]byte{0})
	hash.Write([]byte(legacy))
	return hex.EncodeToString(hash.Sum(nil)[:6])
}

func compactionEnabled(cfg host.CompactionConfig) bool {
	return cfg.Enabled != nil && *cfg.Enabled
}

func skillCount(cfg host.SkillsConfig) int {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return 0
	}
	return len(cfg.Roots)
}

func telemetryEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "off", "disabled":
		return false
	}
	return true
}

// enabledExternalProviders counts the external providers that are declared and
// not explicitly disabled. A section whose every provider is disabled selects
// no service, exactly like an empty section.
func enabledExternalProviders(providers map[string]host.ExternalProviderConfig) int {
	count := 0
	for _, provider := range providers {
		if provider.ProviderEnabled() {
			count++
		}
	}
	return count
}

// externalUnmetered renders the explicit external metering policy for the
// summary; an unset policy on a backend-less document reads "none".
func externalUnmetered(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return strings.TrimSpace(value)
}
