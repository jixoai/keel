package main

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jixoai/keel/internal/collab"
)

// collabTenantCeiling is the single-tenant deployment's hard cap for every
// collaboration mode (change keel-collab-core). A mode descriptor and a
// per-request budget may only narrow it; nothing can raise it at runtime.
//
// These numbers are deliberately separate from the existing sub-agent budget:
// the change freezes the contract first, and keel-collab-subagent maps the
// runner's current behavior onto the mode limits it declares.
var collabTenantCeiling = collab.Limits{
	MaxDepth:      8,
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
func newCollabModes(tenantID uuid.UUID) (*collab.ModeRegistry, error) {
	registry, err := collab.NewModeRegistry(tenantID, collabTenantCeiling)
	if err != nil {
		return nil, fmt.Errorf("collaboration registry: %w", err)
	}
	if err := registry.Register(collab.SubagentMode()); err != nil {
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
