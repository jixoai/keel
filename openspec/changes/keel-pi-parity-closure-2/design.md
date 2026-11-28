# Design: keel-pi-parity-closure-2

- CompactionTask/checkpoint: durable kind/state/attempts/retry, fenced by the session/run lease (no independent task-lease row); PI entry/checkpoint semantics; crash matrix.
- Skills: `allowed-tools` intersects the actual toolset at execution; deny is typed and audited.
- MCP: bounded process/pool lifecycle, health/backoff, destructive tools require hook/approval; server crash does not take down a run.
- Usage ledger: provider usage/observed stream wired to the existing store, not a parallel truth.
- Nightly chaos: 10-minute random SIGKILL job with 0 lost events/0 duplicate final replies; conformance golden in CI.
- Capacity/docs: 2GB/1000 sessions/64 runs evidence refreshed; each capability has runbook.
- No SQLite; no hotel/BU.
