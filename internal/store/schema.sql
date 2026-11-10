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

-- Tenant configuration document (internal/host defines its schema) and the
-- revision that changes on every accepted write. The revision is the change
-- identity consumers compare; no mtime/ETag semantics are layered on top.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS config jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS config_revision bigint NOT NULL DEFAULT 1;

CREATE TABLE IF NOT EXISTS sessions (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    next_seq    bigint NOT NULL DEFAULT 1,
    state       text NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id)
);

-- External sessions (change keel-service-graph, workflow W4; external-gateway
-- spec: 外部会话数据模型). A session whose state an external backend owns
-- carries its marks here: state_owner keel|external, the backend name and its
-- opaque session handle, the external_status lifecycle, and the metering mark
-- (external|none — a missing usage report is never recorded as zero).
--
-- External sessions never enter the native run/lease flow: they get no runs
-- row, store.EnqueueRun refuses them and no claim can return them. The kernel's
-- ExternalStore seam (internal/kernel/external.go) reads and writes the same
-- columns; they live in this base schema because the native queue path's guard
-- reads them.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS state_owner        text NOT NULL DEFAULT 'keel';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS external_backend   text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS external_ref       text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS external_status    text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS metering           text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS external_recovery  text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS external_updated_at timestamptz;

CREATE INDEX IF NOT EXISTS sessions_external_backend
    ON sessions (tenant_id, external_backend)
    WHERE state_owner = 'external';

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
    -- requestId idempotency is per session, exactly like the event log's
    -- dedupe scope. A tenant-wide key would let a requestId reused by another
    -- session return the first session's submission/seq (M3).
    PRIMARY KEY (tenant_id, session_id, request_id)
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

-- The requestId scope moved from (tenant, request_id) to
-- (tenant, session_id, request_id): submissions mirror the event log's
-- per-session idempotency, so the same requestId in two sessions is two
-- submissions. Existing rows are already unique under the new, wider key, so
-- the migration cannot lose or merge data. Idempotent across restarts.
DO $$
DECLARE
    pk_name text;
    pk_def  text;
BEGIN
    SELECT c.conname, pg_get_constraintdef(c.oid)
      INTO pk_name, pk_def
      FROM pg_constraint c
     WHERE c.conrelid = 'submissions'::regclass
       AND c.contype = 'p'
     LIMIT 1;
    IF pk_name IS NOT NULL AND pk_def NOT LIKE '%(tenant_id, session_id, request_id)%' THEN
        EXECUTE format('ALTER TABLE submissions DROP CONSTRAINT %I', pk_name);
        pk_name := NULL;
    END IF;
    IF pk_name IS NULL THEN
        ALTER TABLE submissions
            ADD CONSTRAINT submissions_pkey
            PRIMARY KEY (tenant_id, session_id, request_id);
    END IF;
END
$$;

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

-- Usage ledger: one row per provider request. request_id is generated by the
-- runner before the request is sent and persisted in the request metadata
-- event, so a replay reuses it and the ledger stays exactly-once. seq records
-- the event sequence that carried the request metadata (useful for audit but
-- not the identity: one seq can start several provider requests, e.g. the main
-- turn plus a compaction summary). There are deliberately no price columns:
-- prices live in contracts, the runtime only records objective token counts.
-- status is 'settled' for provider-reported usage and 'unaccounted' for a
-- request that may have been billed but whose usage was lost; an unaccounted
-- row is upgraded in place when the usage is recovered.
CREATE TABLE IF NOT EXISTS usage_ledger (
    tenant_id          uuid NOT NULL,
    session_id         uuid NOT NULL,
    request_id         text NOT NULL,
    seq                bigint NOT NULL,
    provider           text NOT NULL DEFAULT '',
    model              text NOT NULL DEFAULT '',
    input_tokens       bigint NOT NULL DEFAULT 0,
    output_tokens      bigint NOT NULL DEFAULT 0,
    cache_read_tokens  bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    reasoning_tokens   bigint NOT NULL DEFAULT 0,
    status             text NOT NULL DEFAULT 'settled',
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT usage_ledger_request_pkey PRIMARY KEY (tenant_id, session_id, request_id),
    CONSTRAINT usage_ledger_status_check CHECK (status IN ('settled', 'unaccounted')),
    CONSTRAINT usage_ledger_tokens_check CHECK (
        input_tokens >= 0 AND output_tokens >= 0 AND cache_read_tokens >= 0
        AND cache_write_tokens >= 0 AND reasoning_tokens >= 0
    )
);

-- Idempotent migration from the unreleased seq-keyed shape. The table had no
-- production writer, but developer databases and tests may already contain it.
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS request_id text;
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'settled';
UPDATE usage_ledger
SET request_id = 'legacy-' || tenant_id::text || '-' || session_id::text || '-' || seq::text
WHERE request_id IS NULL;
ALTER TABLE usage_ledger ALTER COLUMN request_id SET NOT NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'usage_ledger'::regclass
          AND conname = 'usage_ledger_pkey'
          AND pg_get_constraintdef(oid) LIKE '%(tenant_id, session_id, seq)%'
    ) THEN
        ALTER TABLE usage_ledger DROP CONSTRAINT usage_ledger_pkey;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'usage_ledger'::regclass
          AND conname = 'usage_ledger_request_pkey'
    ) THEN
        ALTER TABLE usage_ledger
            ADD CONSTRAINT usage_ledger_request_pkey
            PRIMARY KEY (tenant_id, session_id, request_id);
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'usage_ledger'::regclass
          AND conname = 'usage_ledger_status_check'
    ) THEN
        ALTER TABLE usage_ledger
            ADD CONSTRAINT usage_ledger_status_check
            CHECK (status IN ('settled', 'unaccounted'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'usage_ledger'::regclass
          AND conname = 'usage_ledger_tokens_check'
    ) THEN
        ALTER TABLE usage_ledger
            ADD CONSTRAINT usage_ledger_tokens_check
            CHECK (
                input_tokens >= 0 AND output_tokens >= 0 AND cache_read_tokens >= 0
                AND cache_write_tokens >= 0 AND reasoning_tokens >= 0
            );
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS usage_ledger_by_tenant_time
    ON usage_ledger (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS usage_ledger_by_session_seq
    ON usage_ledger (tenant_id, session_id, seq);
-- MCP OAuth (internal/mcp/oauth). Pending rows are the browser step of one
-- sign-in: PKCE verifier, redirect URI, and expiry. They are consumed exactly
-- once, so a replayed authorization response cannot exchange a code twice.
CREATE TABLE IF NOT EXISTS mcp_oauth_pending (
    tenant_id         uuid NOT NULL,
    server            text NOT NULL,
    state             text NOT NULL,
    code_verifier     text NOT NULL,
    redirect_uri      text NOT NULL,
    authorization_url text NOT NULL,
    scopes            text,
    expires_at        timestamptz NOT NULL,
    consumed_at       timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, server, state)
);

-- The callback route of the browser redirect cannot carry X-Tenant-ID, so a
-- state (32 random bytes) is the capability that finds its pending row.
CREATE UNIQUE INDEX IF NOT EXISTS mcp_oauth_pending_state ON mcp_oauth_pending (state);

-- At most one active browser step per server. Concurrent sign-in starts are
-- also serialized by a transaction advisory lock; this index is the backstop.
-- Leftover duplicates (from before this index existed) are consumed first.
UPDATE mcp_oauth_pending p SET consumed_at = now()
WHERE p.consumed_at IS NULL AND EXISTS (
    SELECT 1 FROM mcp_oauth_pending q
    WHERE q.tenant_id = p.tenant_id AND q.server = p.server AND q.consumed_at IS NULL
      AND (q.created_at, q.state) > (p.created_at, p.state)
);
CREATE UNIQUE INDEX IF NOT EXISTS mcp_oauth_pending_one_active
    ON mcp_oauth_pending (tenant_id, server)
    WHERE consumed_at IS NULL;
CREATE INDEX IF NOT EXISTS mcp_oauth_pending_active
    ON mcp_oauth_pending (tenant_id, server, expires_at DESC)
    WHERE consumed_at IS NULL;

-- Tenant model settings (change keel-model-settings): one row per tenant holds
-- the accepted models-document override; the history table is the append-only
-- audit of every accepted transition (source 'stored' for a write,
-- 'deployment' for the reset that cleared the override). The revision is the
-- change identity clients compare and it never decreases: it is derived over
-- both tables, so a cleared override still advances it. No foreign key ties the
-- rows to `tenants`: the settings belong to the tenant the session names, and a
-- deployment that never created a tenants row still has settings.
CREATE TABLE IF NOT EXISTS model_settings (
    tenant_id  uuid PRIMARY KEY,
    revision   bigint NOT NULL,
    document   jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- document is NULL for a reset row: at that revision the override was cleared
-- and the deployment document applies again. The removed override stays
-- readable in the previous history row, so the chain reconstructs every state.
CREATE TABLE IF NOT EXISTS model_settings_history (
    tenant_id  uuid NOT NULL,
    revision   bigint NOT NULL,
    document   jsonb,
    source     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, revision)
);

-- Tenant secrets (change keel-model-settings, 密钥管理): named credentials a
-- tenant's model configuration references with `secret:NAME`. ciphertext is
-- AES-256-GCM (the same store.SecretBox envelope as MCP OAuth, keyed by
-- KEEL_SECRET_KEY and bound to (tenant, name) as additional authenticated
-- data); the plaintext never appears in a column, a response, a log or an
-- error. A deployment without KEEL_SECRET_KEY has no secret storage at all:
-- nothing is ever written in plaintext as a fallback.
CREATE TABLE IF NOT EXISTS tenant_secrets (
    tenant_id  uuid NOT NULL,
    name       text NOT NULL,
    ciphertext bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, name)
);

-- Encrypted OAuth state: client registration, tokens, PKCE verifier, discovery.
-- payload is AES-256-GCM ciphertext (nonce prepended); the key comes from
-- KEEL_SECRET_KEY and is never stored next to the row. scopes and expires_at
-- are plaintext for operators; both are derived from the encrypted document.
CREATE TABLE IF NOT EXISTS mcp_oauth_credentials (
    tenant_id  uuid NOT NULL,
    server     text NOT NULL,
    payload    bytea NOT NULL,
    scopes     text,
    expires_at timestamptz,
    version    int NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, server)
);


-- Plugin inventory (change keel-plugin-core): one row per plugin id known to
-- this deployment. Plugin enablement is a property of the process, not of a
-- tenant (one plugins root, one assembled tool surface), so the table carries
-- no tenant_id; the lifecycle *audit* below does, because "who did it" is a
-- tenant-scoped fact. path is always inside the managed plugins root; a row
-- whose package is missing or invalid keeps its state so the reason is
-- visible instead of the plugin silently disappearing.
CREATE TABLE IF NOT EXISTS plugins (
    id           text PRIMARY KEY,
    version      text NOT NULL,
    kind         text NOT NULL,
    state        text NOT NULL DEFAULT 'installed',
    bundled      boolean NOT NULL DEFAULT false,
    source       jsonb NOT NULL DEFAULT '{}'::jsonb,
    manifest     jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash text NOT NULL DEFAULT '',
    path         text NOT NULL DEFAULT '',
    diagnostics  jsonb NOT NULL DEFAULT '[]'::jsonb,
    installed_at timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    enabled_at   timestamptz,
    disabled_at  timestamptz
);

-- Plugin lifecycle audit: one row per state-changing action (install / enable /
-- disable / uninstall / update) and per refused action, written before the HTTP
-- response. audit_id is the keyset cursor of GET /v1/plugins/audit: it orders
-- identically to ts and never repeats.
CREATE TABLE IF NOT EXISTS plugin_audit (
    audit_id  bigserial PRIMARY KEY,
    ts        timestamptz NOT NULL DEFAULT now(),
    tenant_id uuid,
    actor     text NOT NULL DEFAULT '',
    plugin_id text NOT NULL,
    version   text NOT NULL DEFAULT '',
    action    text NOT NULL,
    result    text NOT NULL,
    detail    jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS plugin_audit_tenant ON plugin_audit (tenant_id, audit_id);
CREATE INDEX IF NOT EXISTS plugin_audit_plugin ON plugin_audit (plugin_id, audit_id);
