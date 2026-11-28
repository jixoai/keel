# plugin-split

## ADDED Requirements

### Requirement: Board and superagent are separate plugins

Board MUST own the task/channel/thread substrate and its storage. Superagent MUST be a separate plugin that owns ticket/assignment/attention/outbox state and declares a dependency on board. Board MUST NOT import superagent.

#### Scenario: load order
- **WHEN** both plugins are enabled
- **THEN** board loads first and exposes its service; superagent loads only after board is available

#### Scenario: board unavailable
- **WHEN** board is disabled or fails to load
- **THEN** superagent is `PENDING` with a missing-dependency reason and does not half-load

### Requirement: Plugin storage ownership is explicit

Only board and superagent get independent domain stores. The plugin platform MUST provide shared tenant-scoped plugin state; other plugins MUST use platform/core services rather than creating new domain databases.

#### Scenario: storage surface
- **WHEN** auditing plugin storage
- **THEN** board/superagent have their own adapters, and commands/attachment/telemetry/UI/MCP/skills do not add independent domain schemas

### Requirement: No compatibility bridge

The old `superagentboard` mirror/bridge and any board ⇄ superagent direct table access MUST be removed. The new dependency is through the board plugin's public service interface only.

#### Scenario: dependency isolation
- **WHEN** superagent reads or writes a board task
- **THEN** it calls the board plugin service, not board PostgreSQL tables
