-- FDE2E agent runtime schema (PostgreSQL 18).
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
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, seq)
);

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
    PERFORM pg_notify('fde2e_events',
        NEW.tenant_id::text || ':' || NEW.session_id::text || ':' || NEW.seq::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS events_notify ON events;
CREATE TRIGGER events_notify AFTER INSERT ON events
FOR EACH ROW EXECUTE FUNCTION notify_event();
