-- Keel agent runtime schema (PostgreSQL 18).
-- One database, tenant_id on every row, composite indexes. No schema-per-tenant.
-- See openspec/changes/runtime-foundation/specs/tenant-isolation/spec.md.

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id            uuid PRIMARY KEY,
    name                 text NOT NULL,
    quota_streams        int  NOT NULL DEFAULT 20,
    quota_bytes_per_sec  bigint NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    next_seq    bigint NOT NULL DEFAULT 1,
    state       text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id)
);

CREATE TABLE IF NOT EXISTS events (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    seq         bigint NOT NULL,
    kind        text NOT NULL,
    payload     jsonb NOT NULL,
    dedupe_key  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, seq)
);

-- Tool results (and other replayable effects) are at-least-once at the execution
-- layer but exactly-once in the log: the same dedupe key can only be inserted
-- once per session.
ALTER TABLE events ADD COLUMN IF NOT EXISTS dedupe_key text;
CREATE UNIQUE INDEX IF NOT EXISTS events_dedupe
    ON events (tenant_id, session_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS checkpoints (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    upto_seq    bigint NOT NULL,
    summary     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, upto_seq)
);

CREATE TABLE IF NOT EXISTS submissions (
    tenant_id     uuid NOT NULL,
    request_id    text NOT NULL,
    session_id    uuid NOT NULL,
    submission_id uuid NOT NULL,
    status        text NOT NULL DEFAULT 'pending',
    seq           bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, request_id)
);

CREATE TABLE IF NOT EXISTS tasks (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    task_id     uuid NOT NULL,
    kind        text NOT NULL,
    state       jsonb NOT NULL DEFAULT '{}'::jsonb,
    restarts    int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, task_id)
);

-- Durable task state machine (internal/durable). `state` stays the live
-- checkpoint envelope; these columns carry the identity and lifecycle fields
-- the scheduler needs without rewriting the whole JSON document.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS version  int NOT NULL DEFAULT 1;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS input    jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS checkpoint jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS outcome  jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS status   text NOT NULL DEFAULT 'pending';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS owner    uuid;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS started_at timestamptz;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS ended_at   timestamptz;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS abort_requested    boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS background        boolean NOT NULL DEFAULT false;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS abandon_on_restart boolean NOT NULL DEFAULT false;

CREATE INDEX IF NOT EXISTS tasks_live ON tasks (tenant_id, session_id, status)
    WHERE status IN ('pending', 'running', 'waiting', 'sleeping', 'completing');

ALTER TABLE submissions ADD COLUMN IF NOT EXISTS type      text;
ALTER TABLE submissions ADD COLUMN IF NOT EXISTS entry_id  bigint;
ALTER TABLE submissions ADD COLUMN IF NOT EXISTS answer_id bigint;
ALTER TABLE submissions ADD COLUMN IF NOT EXISTS reason    text;
ALTER TABLE submissions ADD COLUMN IF NOT EXISTS detail    jsonb;
ALTER TABLE submissions ADD COLUMN IF NOT EXISTS updated_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS submissions_by_session ON submissions (tenant_id, session_id, created_at);

CREATE TABLE IF NOT EXISTS blobs (
    tenant_id     uuid NOT NULL,
    blob_id       uuid NOT NULL,
    sha256        bytea NOT NULL,
    size          bigint NOT NULL,
    mime          text NOT NULL,
    storage_kind  smallint NOT NULL,             -- 1=pg_chunked 2=local_cas
    lane_class    smallint NOT NULL,             -- 1=small 2=large
    ref_count     int NOT NULL DEFAULT 0,
    complete      boolean NOT NULL DEFAULT false,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, blob_id),
    UNIQUE (tenant_id, sha256)
);

-- 64KiB chunks. STORAGE EXTERNAL keeps already-compressed image bytes out of
-- pglz/TOAST compression (the CPU cost buys nothing for PNG/JPEG/WebP).
CREATE TABLE IF NOT EXISTS blob_chunks (
    tenant_id   uuid NOT NULL,
    blob_id     uuid NOT NULL,
    seq         int NOT NULL,
    bytes       bytea NOT NULL,
    PRIMARY KEY (tenant_id, blob_id, seq)
);

ALTER TABLE blob_chunks ALTER COLUMN bytes SET STORAGE EXTERNAL;

-- Refcount bookkeeping is keyed by the owning event so double-increment is
-- impossible (an event can reference a blob once).
CREATE TABLE IF NOT EXISTS blob_refs (
    tenant_id   uuid NOT NULL,
    blob_id     uuid NOT NULL,
    session_id  uuid NOT NULL,
    event_seq   bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, blob_id, session_id, event_seq)
);

-- Session listings and quota checks.
CREATE INDEX IF NOT EXISTS sessions_by_tenant ON sessions (tenant_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS blobs_by_expiry ON blobs (expires_at) WHERE ref_count = 0;

-- Wakeup channel for SSE. The payload is "tenant:session:seq"; it is only a
-- hint (the subscriber re-reads events after its cursor), so a dropped
-- notification costs one heartbeat interval, never data.
CREATE OR REPLACE FUNCTION notify_event() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('keel_events',
        NEW.tenant_id::text || ':' || NEW.session_id::text || ':' || NEW.seq::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS events_notify ON events;
CREATE TRIGGER events_notify AFTER INSERT ON events
FOR EACH ROW EXECUTE FUNCTION notify_event();

-- One run row per session. Workers claim it with FOR UPDATE SKIP LOCKED; a
-- crashed process leaves the lease behind and any worker may reclaim it.
CREATE TABLE IF NOT EXISTS runs (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    status      text NOT NULL DEFAULT 'pending', -- pending|running|done|failed
    lease_until timestamptz,
    attempts    int NOT NULL DEFAULT 0,
    cursor_seq  bigint NOT NULL DEFAULT 0,
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id)
);

CREATE INDEX IF NOT EXISTS runs_claim ON runs (status, updated_at);
