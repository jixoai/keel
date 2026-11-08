// Package agentext is the keel-agent extension surface: the product layer that
// makes the kernel loop usable by an agent product without touching the kernel
// itself.
//
// # Boundary
//
// The kernel (internal/kernel, internal/loop, internal/agentcore,
// internal/agenttool, internal/agent) only knows events, leases, tools and the
// transcript. Everything here is product policy and lives outside it:
//
//   - plugins: declarative provides/requires/tools/hooks/triggers/effects,
//     assembled against a loadout-like runtime and unloaded in reverse order;
//   - hooks: the first-phase closed event set (subagent.*, trigger.*, turn.*,
//     decision.*), delivered asynchronously with a bounded queue so a hook can
//     never block the ledger;
//   - decisions: approval/clarify primitives with idempotent answers, durable
//     rows (projected onto the existing tasks bookkeeping table, never a new
//     event kind) and an answer journal;
//   - triggers: interval/cron/webhook triggers with an idempotency key, an
//     audit trail and bounded retries; a fire starts a turn through the edge
//     commit seam (user.message), so the durable log stays the only truth;
//   - sub-agents: a spawn primitive that creates an independent child session,
//     starts its run through the same edge seam, enforces a budget and returns
//     a structured result to the parent;
//   - partials: a process-local, non-persistent token-level bypass that never
//     enters the event log and never moves a Last-Event-ID cursor.
//
// # Purity
//
// Nothing in this package is imported by a kernel package. The dependency
// direction is product → kernel: agentext consumes kernel.Committer,
// kernel.Reader and kernel.LeaseManager-shaped seams and must never become a
// dependency of them (the kernel boundary test enforces it).
//
// # Deliberate seams (wired by the kernel-side owner)
//
// Two observations are only available with a small kernel-side hook, so they
// are exposed as explicit seams rather than guessed:
//
//   - RunControl.BeginRun: whoever executes a run registers its cancel func,
//     which makes POST /v1/sessions/{id}/cancel stop an in-process attempt.
//     Without it, a cancel of a run owned by another process answers 501 and a
//     settled/queued run answers its durable status (never a fake success).
//   - PartialHub.Publish: the token deltas of a provider stream. The
//     servicehost product assembly wraps the provider stream
//     (servicehost.ProductOptions.WrapStream) and the run's loop integration
//     publishes into this hub; until then the transport, invariants and tests
//     are complete and the producer is a no-op.
package agentext
