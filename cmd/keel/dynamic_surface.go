package main

// The MCP half of the dynamic product surface (change feat-mcp-apps-events,
// P2.3). The product assembly is composed of two independently published
// halves:
//
//   - the MCP half: the prompt sections (mcp_servers / mcp_apps) and — in a
//     v1-style exposure — MCP tools. It is produced by internal/mcphost, which
//     fetches and validates the app documents, and published through
//     mcphost.Sinks;
//   - the plugin half: the plugin tools and prompt sections published by
//     harnessext.Runtime through Runtime.RefreshProductTools/RefreshProductPrompt.
//
// Both halves are merged into one republish (servicehost.Runtime composes
// them), so a plugin refresh keeps the MCP sections and an MCP refresh keeps
// the plugin tools/sections. This file owns the buffering between the two
// moments the halves become publishable: the MCP registry publishes while the
// service graph is still activating (mcpService.Start), before the
// servicehost.Runtime value exists.

import (
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/jixoai/keel/internal/agentcore"
	"github.com/jixoai/keel/internal/host"
	"github.com/jixoai/keel/internal/mcphost"
	"github.com/jixoai/keel/internal/servicehost"
)

// dynamicSurface is the MCP side of the publication bridge. It remembers the
// last surface the registry published and republishes it onto the servicehost
// runtime once that runtime exists (attach); before attach, publications are
// buffered, not lost.
type dynamicSurface struct {
	log *slog.Logger

	mu      sync.Mutex
	rt      *servicehost.Runtime
	surface mcphost.Surface
	have    bool
}

func newDynamicSurface(log *slog.Logger) *dynamicSurface {
	if log == nil {
		log = slog.Default()
	}
	return &dynamicSurface{log: log}
}

// sinks is the registry's publication sink set. Changed carries the complete
// republished surface, which is exactly the unit the runtime publishes: tools
// and prompt sections are never split across two publications.
func (d *dynamicSurface) sinks() mcphost.Sinks {
	return mcphost.Sinks{Changed: d.publish}
}

// publish stores the surface and forwards it to the runtime when attached.
func (d *dynamicSurface) publish(surface mcphost.Surface) {
	d.mu.Lock()
	d.surface, d.have = surface, true
	rt := d.rt
	d.mu.Unlock()
	if rt == nil {
		return
	}
	d.republish(rt, surface)
}

// attach binds the servicehost runtime and replays the buffered surface. It is
// called once, right after servicehost.Boot returns.
func (d *dynamicSurface) attach(rt *servicehost.Runtime) {
	if rt == nil {
		return
	}
	d.mu.Lock()
	d.rt = rt
	surface, have := d.surface, d.have
	d.mu.Unlock()
	if !have {
		return
	}
	d.republish(rt, surface)
}

// republish projects the registry types onto the assembly types and publishes.
// A failure is logged, not fatal: the previously published surface stays live
// (the assembly replaces wholesale, so a rejected publication changes nothing).
func (d *dynamicSurface) republish(rt *servicehost.Runtime, surface mcphost.Surface) {
	sections := make([]host.Section, 0, len(surface.Prompt))
	for _, section := range surface.Prompt {
		sections = append(sections, host.Section{Name: section.Name, Content: section.Content})
	}
	tools := append([]agentcore.AgentTool(nil), surface.Tools...)
	if err := rt.PublishMCPSurface(tools, sections); err != nil {
		d.log.Warn("publish the MCP surface", "generation", surface.Generation, "err", err)
	}
}

// configuredMCPAppOrigin reads the optional second origin of the View sandbox
// (KEEL_MCP_APP_ORIGIN). The value is a deployment input, never a build-time
// constant: an unset or malformed value turns the capability bit off rather
// than advertising an origin the deployment does not serve. The production
// domain is intentionally absent from the code.
func configuredMCPAppOrigin(log *slog.Logger) string {
	raw := strings.TrimSpace(os.Getenv("KEEL_MCP_APP_ORIGIN"))
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		log.Warn("KEEL_MCP_APP_ORIGIN is not a plain http(s) origin; the sandbox-origin capability stays off")
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
