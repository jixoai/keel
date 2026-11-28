# pi-parity-closure

## ADDED Requirements

### Requirement: Durable compaction tasks

Compaction work MUST be represented as durable tasks with kind/state/attempts/retry, fenced by the session/run lease (the architecture has no independent task-lease row), and MUST resume after a crash without duplicate final replies.

#### Scenario: crash during compaction
- **WHEN** the process is killed during a compaction task and restarted
- **THEN** the task is reclaimed and exactly one final assistant message exists

### Requirement: Skills enforce allowed-tools at execution

A skill's `allowed-tools` MUST be intersected with the actual registered toolset; a disallowed call MUST be rejected and audited.

#### Scenario: skill calls a disallowed tool
- **WHEN** a skill invokes a tool outside its allowed-tools
- **THEN** the call is rejected with a typed error and no tool side effect occurs

### Requirement: MCP lifecycle is bounded

MCP connections MUST have bounded pools, health checks, reconnect backoff and destructive-tool approval. A server crash MUST NOT take down the run.

#### Scenario: MCP server crashes
- **WHEN** an MCP server exits during a run
- **THEN** the run receives a typed tool error and remains resumable

### Requirement: Usage ledger is the observed stream

Provider usage and observed stream events MUST be written to the existing usage ledger; no second source of truth.

#### Scenario: provider usage
- **WHEN** a provider returns token usage
- **THEN** the ledger records it and the run summary reads the same values

### Requirement: Nightly chaos and conformance are gated

CI MUST run a nightly 10-minute random SIGKILL chaos job and the PI conformance golden; failures MUST be visible, not skipped.

#### Scenario: chaos run
- **WHEN** nightly chaos kills the process repeatedly
- **THEN** the job asserts zero lost events and zero duplicate final replies
