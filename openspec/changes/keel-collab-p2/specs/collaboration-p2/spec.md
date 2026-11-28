# collaboration-p2

## ADDED Requirements

### Requirement: Assignment and Link boundaries are typed

Assignment rebinding and Link creation MUST validate WorkItem boundaries and return typed errors. Cross-tenant or closed-target operations MUST be rejected.

#### Scenario: cross-workitem rebind
- **WHEN** an assignment is rebound to a closed WorkItem
- **THEN** the operation returns a typed conflict and leaves the original assignment intact

### Requirement: Tenant registry is request-scoped

Collaboration resolution MUST use a request-scoped tenant registry; package-global mutable tenant state is forbidden.

#### Scenario: two tenants in one process
- **WHEN** two requests for different tenants run concurrently
- **THEN** each resolves only its own registry

### Requirement: Checkpoint dedupe survives prune

Checkpoint summary hashes MUST be canonical, and requestId dedupe records MUST be independent of checkpoint pruning with bounded TTL cleanup.

#### Scenario: prune then retry
- **WHEN** old checkpoints are pruned and the same requestId is retried
- **THEN** dedupe still returns the original result and no duplicate checkpoint is created
