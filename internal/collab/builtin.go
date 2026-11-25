package collab

// Sub-agent mode defaults. The cross-change contract (change
// keel-collab-subagent §「深度语义与上限」) fixes the default maximum depth at
// 3: main is depth 0, so a chain may reach depth 3 and depth 3 may not spawn a
// depth-4 child. The user-facing -1/unlimited and 0/forbidden settings are
// interpreted by the sub-agent mode implementation; this descriptor carries the
// positive default only, because the core Limits zero means "inherit".
const (
	DefaultSubagentMaxDepth    = 3
	DefaultSubagentMaxChildren = 8
)

// SubagentMode is the built-in parent/child delegation descriptor. It declares
// the mode's shape and limits; the existing harnessext.SubagentRunner is the
// first Host launcher adapter, not the mode definition itself.
func SubagentMode() ModeDescriptor {
	return ModeDescriptor{
		ID:          ModeSubagent,
		Title:       "Sub-agent",
		Description: "A parent session delegates bounded tasks to child sessions and collects a structured result.",
		Roles: []RoleSpec{
			{Name: "main", Description: "The session that owns the delegation.", Min: 1, Max: 1},
			{Name: "sub", Description: "A delegated child session.", Min: 0, Max: 0},
		},
		Topology:     TopologyTree,
		WorkUnit:     WorkUnitDelegation,
		Inputs:       []string{"task", "botId", "budget"},
		Outputs:      []string{"summary", "artifacts", "usage"},
		Capabilities: []Capability{CapabilityAttention},
		// These are mode ceilings/defaults. The registry narrows them by the
		// tenant ceiling; a request narrows them further. MaxDepth is the
		// default depth cap (3), not an arbitrary wider ceiling.
		Limits: Limits{
			MaxDepth:      DefaultSubagentMaxDepth,
			MaxChildren:   DefaultSubagentMaxChildren,
			MaxConcurrent: 16,
			MaxTurns:      100,
			MaxTokens:     1_000_000,
			MaxDurationMs: 600_000,
		},
		UI: UIEntry{Label: "Sub-agents", Route: "/subagents"},
	}
}

// SuperagentMode is the built-in work-queue/ticket descriptor. It ships as a
// descriptor only in this change: the super-agent worker, Source adapters and
// persistence land in keel-collab-superagent. Registering the descriptor keeps
// the read-only catalogue complete without advertising an unimplemented
// topology as usable (keel serve disables it).
func SuperagentMode() ModeDescriptor {
	return ModeDescriptor{
		ID:          ModeSuperagent,
		Title:       "Super-agent",
		Description: "A work queue turns external events into tickets, assigns them to worker Bots and tracks attention.",
		Roles: []RoleSpec{
			{Name: "orchestrator", Description: "Owns the queue and assigns tickets.", Min: 1, Max: 1},
			{Name: "worker", Description: "Executes one assigned ticket.", Min: 0, Max: 0},
		},
		Topology:     TopologyWorkQueue,
		WorkUnit:     WorkUnitTicket,
		Inputs:       []string{"source", "ticket", "assignment"},
		Outputs:      []string{"ticketResult", "attention", "links"},
		Capabilities: []Capability{CapabilityAttention, CapabilitySource, CapabilityChannel},
		Limits: Limits{
			MaxDepth:      4,
			MaxChildren:   32,
			MaxConcurrent: 32,
			MaxTurns:      200,
			MaxTokens:     2_000_000,
			MaxDurationMs: 3_600_000,
		},
		UI: UIEntry{Label: "Super-agent", Route: "/superagent"},
	}
}

// BuiltinModes returns the descriptors this build knows. The host decides
// which are enabled; this function never performs the registration itself.
func BuiltinModes() []ModeDescriptor {
	return []ModeDescriptor{SubagentMode(), SuperagentMode()}
}
