package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
	"github.com/jixoai/keel/internal/harnessext"
)

// collabTenantCeiling is the single-tenant deployment's hard cap for every
// collaboration mode (change keel-collab-core). A mode descriptor and a
// per-request budget may only narrow it; nothing can raise it at runtime.
//
// Depth is an explicit policy, not a numeric ceiling: the sub-agent contract
// encodes -1/unlimited and 0/forbidden, which the mode's request layer maps to
// collab.DepthPolicy before calling the Host. The tenant ceiling therefore
// allows the mode to choose that policy (unlimited at this level), while
// width, concurrency, turns, tokens and duration stay hard-capped.
var collabTenantCeiling = collab.Limits{
	Depth:         collab.UnlimitedDepth(),
	MaxChildren:   32,
	MaxConcurrent: 64,
	MaxTurns:      500,
	MaxTokens:     5_000_000,
	MaxDurationMs: 3_600_000,
}

// newCollabModes builds the assembly-time collaboration mode registry.
// Registration is the only way a mode becomes usable, so a duplicate id or a
// disable of an unknown id fails startup instead of serving a half-assembled
// catalogue.
//
// subagentMaxDepth is the deployment's sub-agent setting: -1 unlimited, 0
// forbidden, positive a concrete cap. It is already resolved from serveOptions
// (nil -> the 3-level default). The core Host receives the explicit three-state
// DepthPolicy; it never overloads zero as "default" or "unlimited".
func newCollabModes(tenantID uuid.UUID, subagentMaxDepth int, subagentMaxChildren int) (*collab.ModeRegistry, error) {
	registry, err := collab.NewModeRegistry(tenantID, collabTenantCeiling)
	if err != nil {
		return nil, fmt.Errorf("collaboration registry: %w", err)
	}
	subagent := collab.SubagentMode()
	switch {
	case subagentMaxDepth < 0:
		subagent.Limits.Depth = collab.UnlimitedDepth()
	case subagentMaxDepth == 0:
		subagent.Limits.Depth = collab.ForbiddenDepth()
	default:
		subagent.Limits.Depth = collab.LimitedDepth(subagentMaxDepth)
	}
	if subagentMaxChildren > 0 {
		subagent.Limits.MaxChildren = subagentMaxChildren
	}
	if err := registry.Register(subagent); err != nil {
		return nil, fmt.Errorf("register %q mode: %w", collab.ModeSubagent, err)
	}
	// Registration point for the super-agent mode. keel-collab-superagent owns
	// its worker, Source/Channel adapters and persistence; this change ships
	// the descriptor only and keeps it disabled, so the read-only catalogue is
	// complete without advertising an unimplemented topology as usable.
	if err := registry.Register(collab.SuperagentMode()); err != nil {
		return nil, fmt.Errorf("register %q mode: %w", collab.ModeSuperagent, err)
	}
	if err := registry.SetEnabled(collab.ModeSuperagent, false); err != nil {
		return nil, fmt.Errorf("disable %q mode: %w", collab.ModeSuperagent, err)
	}
	return registry, nil
}

// newCollabRegistries builds the per-tenant registry source. A request for a
// tenant resolves (and caches) that tenant's catalogue instead of reusing the
// assembly-time single-tenant object; the deployment's own tenant is simply the
// first one the Host asks for.
func newCollabRegistries(subagentMaxDepth int, subagentMaxChildren int) (*collab.TenantRegistries, error) {
	return collab.NewTenantRegistries(func(_ context.Context, tenantID uuid.UUID) (*collab.ModeRegistry, error) {
		return newCollabModes(tenantID, subagentMaxDepth, subagentMaxChildren)
	})
}

// resolveSubagentMaxDepth resolves the serve option. nil keeps the mode's
// 3-level default; an explicit value is used verbatim (-1 unlimited, 0
// forbidden, positive a cap).
func resolveSubagentMaxDepth(configured *int) int {
	if configured == nil {
		return harnessext.DefaultSubagentMaxDepth
	}
	return *configured
}

// resolveSubagentMaxChildren resolves the direct-children setting. nil keeps
// the mode default (8); the value is passed to both the runner (defensive
// check) and the mode descriptor (the Host's guard), so they cannot drift.
func resolveSubagentMaxChildren(configured *int) int {
	if configured == nil || *configured <= 0 {
		return harnessext.DefaultSubagentMaxChildren
	}
	return *configured
}
