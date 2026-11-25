// Package superagent implements the first work-queue collaboration mode on top
// of internal/collab.
//
// A super-agent is not a larger sub-agent and it is not a Bot kind. It is an
// orchestration position: external Source events become Triggers, an
// orchestrator accepts them as durable Tickets, Assignments bind Tickets to
// Worker Bot sessions, and structured Attention routes human decisions back to
// the conversation that asked. Ticket/Assignment/Subscription are collaboration
// records, not Session properties, so a worker session may end or be replaced
// while the Ticket and its history stay.
//
// The package is deliberately storage-agnostic: Store embeds collab.WorkStore
// and adds only the super-agent records (subscriptions, source cursors, seen
// events, outbox). MemoryStore is the test/degraded implementation; the
// PostgreSQL adapter lives in internal/store and is assembled by cmd/keel.
package superagent
