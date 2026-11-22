// Package oar implements Keel's OAR contract v1: the out-of-band record
// stream for external agent runtimes.
//
// OAR is not a second agent runtime and not a second external-session model.
// An OAR runtime (Codex first) is an external tool: it runs as a sidecar that
// speaks the existing external-gateway agent_backend protocol
// (internal/gateway), owns its own conversation state, and reports what it did
// through a record stream that the host consumes out of band. The host never
// asks the runtime to create or update task cards: attribution is carried in
// the session-open config and echoed on every record, so an external call can
// be attributed to the task card that started it without injecting
// "please record a task card" into the model prompt.
//
// The contract has five parts, all frozen in docs/OAR.md:
//
//  1. capability matrix: a closed vocabulary declared in the initialize
//     response under capabilities.experimental.oar;
//  2. record stream: monotonic seq plus agentPath, with call/result/status
//     records that can be folded into a renderer projection;
//  3. typed rejection: an unsupported capability, a missing runtime or a
//     simulated runtime standing in for a live model is a typed error before
//     any session or model call starts;
//  4. process-group cleanup: the sidecar is spawned through
//     gateway.SpawnStdio (its own group), its own child runtimes are killed as
//     groups, stdin EOF is the first orphan defense and a pid file plus
//     gateway.KillTree is the last;
//  5. renderer seam: Projection is the stable record projection the W12
//     tool-renderer slot consumes; the web UI owns the slot, this package only
//     supplies the data shape and a checked-in fixture.
//
// The package is deliberately a leaf: it imports internal/gateway for the wire
// and process contract and nothing that writes the ledger. A host that wants
// OAR sessions recorded as external.event uses the existing externalgw path
// (see Config.ExternalBackendConfig); a developer inspecting the stream uses
// the journal and the oar-view command.
package oar
