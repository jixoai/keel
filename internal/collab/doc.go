// Package collab is the mode-agnostic collaboration contract between the
// Harness (Bot roles, sessions, permissions) and a collaboration mode
// (subagent, superagent, pipeline, peer review, …).
//
// Boundaries:
//
//   - A Bot is a role subject (Profile + ModelKind + Harness + permissions).
//     Collaboration positions (main, sub, worker, orchestrator) are mode
//     positions in a WorkItem/Session, never a new Bot kind. No type in this
//     package names a Bot kind.
//   - A mode declares its topology, work unit, roles, limits and capabilities
//     in a ModeDescriptor registered with a ModeRegistry at assembly time.
//     Duplicate and unknown ids are startup errors.
//   - Every session a mode creates MUST pass through Host. The Host validates
//     the tenant, the caller's bot.invoke authority, budget, depth/width,
//     concurrency and cancellation propagation, and writes audit. A mode MUST
//     NOT import internal/kernel or internal/store to create a Session itself.
//   - WorkItem/Assignment/Attention are ports, not a schema. The core package
//     ships an in-memory store for tests and a skeleton deployment; persistence
//     belongs to the mode change that needs it.
//   - Limits have one direction: a tenant ceiling is the hard upper bound and a
//     Bot/WorkItem/mode may only narrow it. Zero means "inherit" for a request
//     and "unlimited" for a ceiling.
//
// Dependency rule: this package imports only the standard library and
// github.com/google/uuid. Mode plugins interact with the types and the Host
// port; the product-layer adapters (harnessext, cmd/keel) own the concrete
// launcher, permission and persistence implementations.
package collab
