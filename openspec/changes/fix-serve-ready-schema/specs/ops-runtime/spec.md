# ops-runtime

## ADDED Requirements

### Requirement: Product shell readiness applies its schema contract

`keel serve` / `keel start` MUST apply every schema its `/v1/ready` contract checks,
including the handoff audit tables, before the process can report ready. A fresh product
database MUST reach `/v1/ready` 200 without first running `cmd/runtime` or a manual
migration. When the migration fails, boot MUST fail with the schema error instead of
serving a permanently unready process.

#### Scenario: fresh product database
- **WHEN** `keel serve` starts against a database without `runtime_instances` or `handoffs`
- **THEN** it applies the handoff schema, starts serving, and `/v1/ready` returns 200

#### Scenario: existing database
- **WHEN** `keel serve` starts against a database that already has the handoff tables
- **THEN** the idempotent migration succeeds and readiness is unchanged

#### Scenario: migration failure
- **WHEN** the handoff schema cannot be applied
- **THEN** boot fails with a `handoff migrate` error and the process does not report
  ready
