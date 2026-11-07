// Package handoff implements Keel's runtime handoff protocol: replacing the
// process that serves a session without losing it.
//
// The protocol (docs/HANDOFF.md, openspec change keel-service-graph, spec
// runtime-handoff) is built on the contract kernel, not on the process:
//
//  1. the new runtime starts, registers itself and reaches ready;
//  2. the old runtime begins a drain (shutdown or handoff);
//  3. it stops claiming, drains its in-flight runs and re-persists every
//     committed fact;
//  4. it releases the lease (or leaves it to expire, recorded as a timeout);
//  5. the new runtime claims the session and receives epoch+1;
//  6. it rebuilds from the event log and continues; SSE clients resume from
//     their Last-Event-ID cursor.
//
// Fencing is what makes the swap safe: the kernel rejects every owner-scoped
// commit that carries an old epoch, so a drained runtime can neither write a
// late fact nor resurrect its claim. Edge-scoped input (user.message) keeps
// landing during a handoff drain and is picked up by the new owner.
//
// # Drain modes
//
//	shutdown  SIGTERM / deployment stop. Stop claiming, reject new writes
//	          (the HTTP layer answers 503 + Retry-After), close SSE streams,
//	          abandon in-flight runs; the lease expires and the next process
//	          recovers the session.
//	handoff   upgrade / loadout change. Stop claiming, but KEEP accepting
//	          edge-scoped input; in-flight runs drain to a safe point or the
//	          deadline; the new runtime takes over with epoch+1.
//
// The two modes are distinguishable in the durable audit (runtime_instances
// and handoffs rows), in the logs and in the HTTP gate: handoff never reuses
// shutdown's "reject input" branch.
//
// # Scope
//
// A Coordinator is one runtime instance: it owns the runtime id (persisted and
// used as the claim owner), the drain state, the in-flight run count and the
// handoff audit. Its Kernel field is the kernel slice the protocol drives; a
// Coordinator without one is audit-only (it can register, drain and report but
// not adopt sessions).
package handoff
