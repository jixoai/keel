-- Runtime handoff audit (change keel-service-graph, workflow W4).
--
-- runtime_instances: one row per runtime process. The runtime id is persistent
-- and is written into every claim as the owner; the row records the loadout
-- pin and the drain lifecycle so an operator can answer "which runtime started
-- when, under which loadout, drained in which mode and how many runs it had in
-- flight".
--
-- handoffs: the durable audit of a drain or of one session adoption. A
-- runtime-level drain row carries no tenant/session; a per-session adoption row
-- answers the spec's audit scenario: old/new runtime, loadout revision, epochs,
-- start/end times, in-flight runs.
--
-- Both tables are additive; nothing existing is modified.

CREATE TABLE IF NOT EXISTS runtime_instances (
    runtime_id     text PRIMARY KEY,
    loadout_id     text NOT NULL DEFAULT '',
    loadout_revision bigint NOT NULL DEFAULT 0,
    status         text NOT NULL DEFAULT 'starting', -- starting|ready|draining|stopped
    drain_mode     text,                             -- shutdown|handoff
    drain_deadline timestamptz,
    in_flight_runs int NOT NULL DEFAULT 0,
    started_at     timestamptz NOT NULL DEFAULT now(),
    ready_at       timestamptz,
    drain_started_at timestamptz,
    stopped_at     timestamptz,
    reason         text
);

CREATE INDEX IF NOT EXISTS runtime_instances_by_status
    ON runtime_instances (status, started_at DESC);

CREATE TABLE IF NOT EXISTS handoffs (
    handoff_id      text PRIMARY KEY,
    tenant_id       uuid,
    session_id      uuid,
    mode            text NOT NULL DEFAULT 'handoff',
    from_runtime    text NOT NULL DEFAULT '',
    to_runtime      text NOT NULL DEFAULT '',
    from_epoch      bigint NOT NULL DEFAULT 0,
    to_epoch        bigint NOT NULL DEFAULT 0,
    loadout_id      text NOT NULL DEFAULT '',
    loadout_revision bigint NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'started', -- started|completed|timed-out
    in_flight_runs  int NOT NULL DEFAULT 0,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    detail          text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS handoffs_by_session
    ON handoffs (tenant_id, session_id, started_at DESC)
    WHERE session_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS handoffs_by_runtime
    ON handoffs (from_runtime, started_at DESC);
