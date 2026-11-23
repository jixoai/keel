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

-- Sessions are deleted from the user's view as a soft delete
-- (DELETE /v1/sessions/{id}): the session leaves the list while every
-- event/run/usage row stays — the ledger is the metering and audit truth and
-- is never deleted by a product call. archived_at is that mark; it is set
-- once and never cleared by the delete route (idempotent).
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS archived_at timestamptz;

-- Session execution binding (change keel-bot-identity-access, A4.2b3a).
-- The pre-A4 newest-selection projection (bound_agent_id / bound_team_id /
-- bound_seq / bound_at) is retired: a Session belongs to exactly one Bot
-- (sessions.bot_id, added with the A1 identity columns) and an input cannot
-- move that ownership. The columns and their partial indexes are dropped
-- outright — alpha zero compatibility: no shim, no dual read, no backfill.
ALTER TABLE sessions DROP COLUMN IF EXISTS bound_agent_id;
ALTER TABLE sessions DROP COLUMN IF EXISTS bound_team_id;
ALTER TABLE sessions DROP COLUMN IF EXISTS bound_seq;
ALTER TABLE sessions DROP COLUMN IF EXISTS bound_at;
DROP INDEX IF EXISTS sessions_bound_agent;
DROP INDEX IF EXISTS sessions_bound_team;

-- Fork origin (change keel-harness-v2, W8). A forked session inherits the
-- parent's ledger prefix up to fork_origin_seq and its file view from
-- fork_tree_hash (a layerfs tree object). The columns are a reference, not a
-- copy: no parent manifest rows and no bytes are duplicated, which is what
-- makes fork O(1) and keeps a parent deletion from freeing a child's blobs.
-- fork_config_hash / fork_tool_manifest_hash / fork_anchor_seq freeze the
-- parent attempt's run snapshot values at the origin, so the child can answer
-- "which configuration did I inherit" without replaying the parent log.
-- RLS: the columns live on sessions, which already carries FORCE ROW LEVEL
-- SECURITY and the tenant_isolation policy; no new table or policy is needed.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_parent_session_id uuid;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_origin_seq bigint NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_tree_hash text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_config_hash text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_tool_manifest_hash text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS fork_anchor_seq bigint NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS sessions_fork_parent
    ON sessions (tenant_id, fork_parent_session_id)
    WHERE fork_parent_session_id IS NOT NULL;

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

-- checkpoints are compaction projections, never a source of truth: the event
-- log stays complete and readable. upto_seq is the seq of the compaction event
-- that produced the row (the checkpoint is aligned with that fact); summary is
-- that event's summary. kept_from_seq is the seq of the first event the
-- compaction retained (the tail that must follow the summary on replay), NULL
-- until the host anchors it to committed events.
CREATE TABLE IF NOT EXISTS checkpoints (
    tenant_id     uuid NOT NULL,
    session_id    uuid NOT NULL,
    upto_seq      bigint NOT NULL,
    kept_from_seq bigint,
    summary       text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, upto_seq)
);

ALTER TABLE checkpoints ADD COLUMN IF NOT EXISTS kept_from_seq bigint;

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

-- Thumbnail lifecycle (change keel-ga §5 blob-lifecycle). thumb_status is
-- none (not an image / not requested), pending (queued), ready (thumb_blob_id
-- points at a blob row of its own) or failed (terminal; no retry storm). The
-- thumbnail bytes are an ordinary blob, so they use the same storage/read path
-- and sharing rules as every other object. thumb_updated_at is audit only.
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS thumb_status text NOT NULL DEFAULT 'none';
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS thumb_blob_id uuid;
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS thumb_updated_at timestamptz;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'blobs_thumb_status_check'
                     AND conrelid = 'blobs'::regclass) THEN
        ALTER TABLE blobs ADD CONSTRAINT blobs_thumb_status_check
            CHECK (thumb_status IN ('none', 'pending', 'ready', 'failed'));
    END IF;
END
$$;

-- The thumbnailer scans pending rows in creation order; the partial index keeps
-- the scan proportional to the queue, not to the table.
CREATE INDEX IF NOT EXISTS blobs_thumb_pending ON blobs (created_at)
    WHERE thumb_status = 'pending';

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

-- The FK protects the "reference first, sweep later" direction: the reference
-- INSERT takes a KEY SHARE lock on the blob row, so a sweeper's FOR UPDATE
-- cannot delete the row between the reference insert and the event commit.
-- The opposite interleaving (sweeper already holds the lock, reference INSERT
-- waits on the FK check after the delete commits) is a tolerated race: the store
-- treats SQLSTATE 23503 as "blob already gone" and the event commits with zero
-- refs (see blob_refs.go). ON DELETE CASCADE is the backstop for any delete
-- path that forgets to clear refs first.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'blob_refs_blob_fk'
                     AND conrelid = 'blob_refs'::regclass) THEN
        ALTER TABLE blob_refs ADD CONSTRAINT blob_refs_blob_fk
            FOREIGN KEY (tenant_id, blob_id)
            REFERENCES blobs (tenant_id, blob_id)
            ON DELETE CASCADE;
    END IF;
END
$$;

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
-- 'queued' is the placement queue's waiting state: durable, but neither
-- claimable (the kernel's predicate matches only pending/running) nor counted
-- against a group/tenant slot until promotion turns it into 'pending'.
CREATE TABLE IF NOT EXISTS runs (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    status      text NOT NULL DEFAULT 'pending', -- queued|pending|running|done|failed
    lease_until timestamptz,
    attempts    int NOT NULL DEFAULT 0,
    cursor_seq  bigint NOT NULL DEFAULT 0,
    last_error  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id)
);

CREATE INDEX IF NOT EXISTS runs_claim ON runs (status, updated_at);

-- Bash background jobs (change keel-harness-v2, W2.2). The job itself is an
-- in-process goroutine plus its own gbash session; this table is the durable
-- fact a status query, a restart and the UI read. A restart cannot re-attach
-- the goroutine (it died with the process), so recovery converges every
-- running row to a terminal state with a reason. Output stays readable through
-- the blob references and the bounded head/tail snapshot.
CREATE TABLE IF NOT EXISTS bash_jobs (
    tenant_id        uuid NOT NULL,
    job_id           uuid NOT NULL,
    session_id       uuid,
    group_key        text NOT NULL DEFAULT '',
    command          text NOT NULL,
    cwd              text NOT NULL DEFAULT '',
    status           text NOT NULL,             -- running|exited|interrupted|canceled|failed
    exit_code        int,
    stdout_blob_id   uuid,
    stderr_blob_id   uuid,
    stdout_bytes     bigint NOT NULL DEFAULT 0,
    stderr_bytes     bigint NOT NULL DEFAULT 0,
    stdout_head      text NOT NULL DEFAULT '',
    stdout_tail      text NOT NULL DEFAULT '',
    stderr_head      text NOT NULL DEFAULT '',
    stderr_tail      text NOT NULL DEFAULT '',
    output_truncated boolean NOT NULL DEFAULT false,
    reason           text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz,
    PRIMARY KEY (tenant_id, job_id)
);

CREATE INDEX IF NOT EXISTS bash_jobs_running
    ON bash_jobs (tenant_id, status) WHERE status = 'running';
CREATE INDEX IF NOT EXISTS bash_jobs_session
    ON bash_jobs (tenant_id, session_id);
CREATE INDEX IF NOT EXISTS bash_jobs_created
    ON bash_jobs (tenant_id, created_at DESC);

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

-- Additive (change keel-model-catalog A): the masked preview a settings list
-- renders next to a credential name. It is computed at write time by
-- store.SecretPreview (first6 + "…" + last6 for values longer than 18 runes,
-- a fixed mask otherwise) and carries no length, full value or hash. NULL means
-- the row predates this column; a reader shows "已保存" instead of inventing a
-- preview. ADD COLUMN IF NOT EXISTS keeps the migration idempotent on both a
-- fresh database and an existing one.
ALTER TABLE tenant_secrets ADD COLUMN IF NOT EXISTS preview text;

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

-- MCP App audit (change feat-mcp-apps-events, P3.7): one row per bridged app
-- action and per refusal (document fetch/validation, tools/call,
-- resources/read, ui/message, ui/open-link, permission decisions, protocol
-- violations). detail is a bounded, identifier-only summary: message bodies,
-- credentials, tokens and full URL query strings MUST NOT enter this table
-- (spec「安全、同意与审计」). audit_id is the keyset cursor of
-- GET /v1/mcp/apps/audit.
CREATE TABLE IF NOT EXISTS mcp_app_audit (
    audit_id   bigserial PRIMARY KEY,
    ts         timestamptz NOT NULL DEFAULT now(),
    tenant_id  uuid,
    session_id uuid,
    server     text NOT NULL DEFAULT '',
    app_id     text NOT NULL DEFAULT '',
    action     text NOT NULL,
    result     text NOT NULL,
    detail     text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS mcp_app_audit_tenant ON mcp_app_audit (tenant_id, audit_id);
CREATE INDEX IF NOT EXISTS mcp_app_audit_app ON mcp_app_audit (app_id, audit_id);

-- MCP app model context (change feat-mcp-apps-events, P5.5). One overwrite-only
-- row per (tenant, session, app): ui/update-model-context replaces the previous
-- value, and the next attempt merges it into the prompt as <mcp_app_context>.
-- Deliberately NOT an event: app context is not a session fact, it has no
-- replay semantics and no run may depend on a historical version (design §3.6.3,
-- §7-R5 explicitly rejects the ledger approach).
CREATE TABLE IF NOT EXISTS mcp_app_contexts (
    tenant_id  uuid NOT NULL,
    session_id uuid NOT NULL,
    app_id     text NOT NULL,
    content    jsonb,
    structured jsonb,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, app_id)
);

CREATE INDEX IF NOT EXISTS mcp_app_contexts_session
    ON mcp_app_contexts (tenant_id, session_id, updated_at);

-- --- retired product tables (change keel-bot-identity-access, A4.2b) --------
--
-- The Agent registry, its team document and the tenant api_keys credential are
-- retired. Alpha zero compatibility (L19): the product only reads bots /
-- team_bots / bot_api_keys, so the old tables are removed instead of being kept
-- as a second truth.
--
-- Policy at startup (this migration IS the startup path):
--   * an empty leftover table is dropped (no data is lost);
--   * a table that still holds rows refuses the migration with a rebuild
--     instruction, so a process can never start half-migrated and silently
--     ignore the operator's data. Deployments were told the upgrade is a
--     rebuild; a backup is the operator's responsibility.
-- No compatibility view, no dual read and no data backfill is written.
DO $$
BEGIN
    IF to_regclass('public.agents') IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM public.agents) THEN
            RAISE EXCEPTION 'agents: the legacy agent registry still holds rows; this build has no migration path. Back up the database, drop the agents/agent_teams/api_keys tables (or recreate the database) and re-create Bots/TeamBots through /v1/bots and /v1/team-bots';
        END IF;
        DROP TABLE public.agents CASCADE;
    END IF;
    IF to_regclass('public.agent_teams') IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM public.agent_teams) THEN
            RAISE EXCEPTION 'agent_teams: the legacy agent-team table still holds rows; this build has no migration path. Back up the database, drop the agents/agent_teams/api_keys tables (or recreate the database) and re-create Bots/TeamBots through /v1/bots and /v1/team-bots';
        END IF;
        DROP TABLE public.agent_teams CASCADE;
    END IF;
    IF to_regclass('public.api_keys') IS NOT NULL THEN
        IF EXISTS (SELECT 1 FROM public.api_keys) THEN
            RAISE EXCEPTION 'api_keys: the legacy tenant key table still holds rows; this build has no migration path. Back up the database, drop the agents/agent_teams/api_keys tables (or recreate the database) and authenticate with account passwords or Bot keys (bot_api_keys) instead';
        END IF;
        DROP TABLE public.api_keys CASCADE;
    END IF;
END
$$;

-- Per-tenant quota row. The run-concurrency and admitted-event budgets live in
-- the tenant configuration document (host.QuotaConfig, one vocabulary the admin
-- API writes); these columns are the dimensions the process edge enforces
-- before a request reaches a session: concurrent streams, outbound bytes per
-- second and the storage ceiling. Zero means unlimited for every column.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS quota_streams int NOT NULL DEFAULT 20;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS quota_bytes_per_sec bigint NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS quota_storage_bytes bigint NOT NULL DEFAULT 0;

-- Objective usage facts that are not provider token counts (spec: tenancy-
-- quotas / 计量事件). Token accounting has exactly one home — usage_ledger
-- above — and is never duplicated here; this table carries the byte dimensions
-- the ledger has no column for:
--
--   image_bytes   bytes of an image blob a tenant stored (upload completion);
--   storage_bytes bytes of any blob a tenant stored (upload completion).
--
-- dedupe_key makes recording idempotent: one row per (tenant, kind, blob).
-- There is deliberately no price, amount or currency column: prices live in
-- contracts, the runtime records objective quantities only.
CREATE TABLE IF NOT EXISTS tenant_usage (
    usage_id    bigserial PRIMARY KEY,
    tenant_id   uuid NOT NULL,
    kind        text NOT NULL,
    quantity    bigint NOT NULL,
    source      text NOT NULL DEFAULT '',
    session_id  uuid,
    blob_id     uuid,
    dedupe_key  text,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_usage_kind_check CHECK (kind IN ('image_bytes', 'storage_bytes')),
    CONSTRAINT tenant_usage_quantity_check CHECK (quantity >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS tenant_usage_dedupe
    ON tenant_usage (tenant_id, kind, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS tenant_usage_by_tenant_time
    ON tenant_usage (tenant_id, recorded_at DESC);
CREATE INDEX IF NOT EXISTS tenant_usage_by_kind
    ON tenant_usage (tenant_id, kind, recorded_at DESC);

-- --- task-log (change keel-harness-v2, spec task-log) -----------------------
--
-- Two read models, one truth. task_cards is the current state of a card and
-- task_events is the materialized projection of the six task.* ledger kinds;
-- both are rebuilt from the session ledger by internal/tasklog and neither is a
-- second source of truth (spec: 任务事件是账本投影). Writes go through the
-- ledger first: the projection is repaired from the log, never edited by hand.
--
-- The dependency tables (tasks/submissions) above are the durable state machine
-- of internal/durable and are unrelated: a "task card" is memory, a durable
-- "task" is scheduling bookkeeping.
CREATE TABLE IF NOT EXISTS task_cards (
    tenant_id      uuid NOT NULL,
    session_id     uuid NOT NULL,
    task_id        uuid NOT NULL,
    parent_task_id uuid,
    title          text NOT NULL DEFAULT '',
    status         text NOT NULL DEFAULT 'open',
    origin         text NOT NULL DEFAULT 'session',
    origin_seq     bigint NOT NULL DEFAULT 0,
    created_seq    bigint NOT NULL DEFAULT 0,
    updated_seq    bigint NOT NULL DEFAULT 0,
    closed_seq     bigint NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, task_id)
);

-- A task id is a tenant-wide handle (parentTaskId resolves across sessions), so
-- it is unique per tenant, not merely per session.
CREATE UNIQUE INDEX IF NOT EXISTS task_cards_tenant_task ON task_cards (tenant_id, task_id);
CREATE INDEX IF NOT EXISTS task_cards_parent ON task_cards (tenant_id, parent_task_id);
CREATE INDEX IF NOT EXISTS task_cards_session
    ON task_cards (tenant_id, session_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS task_events (
    tenant_id  uuid NOT NULL,
    session_id uuid NOT NULL,
    seq        bigint NOT NULL,
    task_id    uuid NOT NULL,
    kind       text NOT NULL,
    payload    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, seq)
);

CREATE INDEX IF NOT EXISTS task_events_task ON task_events (tenant_id, task_id, seq);

-- Approval provenance (change keel-harness-v2, W2.5): who answered an approval
-- question, from which channel, and whether the answer stayed inside the
-- declared answer space. The decision lifecycle itself lives in the decision
-- primitive (tasks rows); this table is the audit projection the HTTP answer
-- route and the approve tool write, so "who approved this, and how" is a
-- queryable fact instead of log archaeology.
CREATE TABLE IF NOT EXISTS approval_provenance (
    tenant_id    uuid NOT NULL,
    session_id   uuid NOT NULL,
    decision_id  uuid NOT NULL,
    kind         text NOT NULL,
    mode         text NOT NULL,
    status       text NOT NULL,
    option_id    text NOT NULL DEFAULT '',
    answer_text  text NOT NULL DEFAULT '',
    validated    boolean NOT NULL DEFAULT false,
    escape       boolean NOT NULL DEFAULT false,
    actor        text NOT NULL DEFAULT '',
    channel      text NOT NULL DEFAULT '',
    context      jsonb NOT NULL DEFAULT '{}'::jsonb,
    opened_at    timestamptz,
    delivered_at timestamptz,
    answered_at  timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, decision_id),
    CONSTRAINT approval_provenance_status_check
        CHECK (status IN ('open', 'answered', 'timed_out', 'cancelled'))
);

CREATE INDEX IF NOT EXISTS approval_provenance_by_session
    ON approval_provenance (tenant_id, session_id, updated_at DESC);

-- Notifications (change keel-harness-v2, W2.5): the durable row behind the
-- `notify` aggregate tool. `notification.ack` and `notification.push_sent` are
-- the event-ized facts; this row carries the consumer set and the one fallback
-- push. Consumption tokens are stored only as SHA-256 hashes (the raw token is
-- returned once, by the push that minted it), and push_attempted_at is the
-- compare-and-set that makes "push once" survive restarts.
CREATE TABLE IF NOT EXISTS notifications (
    tenant_id         uuid NOT NULL,
    notification_id   uuid NOT NULL,
    session_id        uuid NOT NULL,
    title             text NOT NULL DEFAULT '',
    body              text NOT NULL,
    status            text NOT NULL DEFAULT 'created',
    created_at        timestamptz NOT NULL,
    deadline          timestamptz NOT NULL,
    consumers         jsonb NOT NULL DEFAULT '[]'::jsonb,
    push_attempted_at timestamptz,
    push              jsonb,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, notification_id),
    CONSTRAINT notifications_status_check
        CHECK (status IN ('created', 'delivered', 'consumed'))
);

CREATE INDEX IF NOT EXISTS notifications_by_session
    ON notifications (tenant_id, session_id, created_at DESC);
CREATE INDEX IF NOT EXISTS notifications_due
    ON notifications (deadline)
    WHERE status <> 'consumed' AND push_attempted_at IS NULL;

-- --- file-plugin (change keel-harness-v2, W2.3) -----------------------------
--
-- The session's file view. file_manifest is the per-session path → content-hash
-- projection (the bytes live in layerfs/blobs; this row is the mount and the
-- cas precondition); session_snapshot records the tree hash captured at a seq,
-- so a fork/rollback can materialize the prefix. internal/fileplugin owns the
-- reads/writes; the DDL lives here because internal/store is the migration owner.
CREATE TABLE IF NOT EXISTS file_manifest (
    tenant_id    uuid NOT NULL,
    session_id   uuid NOT NULL,
    path         text NOT NULL,
    hash         text NOT NULL,
    mode         integer NOT NULL DEFAULT 420,
    size         bigint NOT NULL DEFAULT 0,
    updated_seq  bigint NOT NULL,
    last_task_id text,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, path)
);

CREATE INDEX IF NOT EXISTS file_manifest_by_hash
    ON file_manifest (tenant_id, hash);

CREATE TABLE IF NOT EXISTS session_snapshot (
    tenant_id  uuid NOT NULL,
    session_id uuid NOT NULL,
    seq        bigint NOT NULL,
    tree_hash  text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id, seq)
);

-- --- placement (change keel-harness-v2, W2.1) -------------------------------
--
-- A Group is the harness's placement and collaboration unit: one worker process
-- holds it (one group per worker in this change), it carries its own run
-- concurrency slots and memory ceiling, and it may hold sessions of more than
-- one user (project team). Groups are tenant-scoped here because the runtime's
-- tenancy model still equates "user" with tenant; a cross-tenant project group
-- needs a deployment-scoped group row plus a membership table and is deferred.
--
-- max_concurrent_runs = 0 means "no group-level limit" (the deployment's worker
-- concurrency and the tenant quota still apply); memory_limit_bytes = 0 means
-- "unbounded". The memory number is the packing budget used by the pack
-- strategy, not cgroup enforcement: the group worker's cgroup belongs to the
-- process-isolation lane.
CREATE TABLE IF NOT EXISTS groups (
    tenant_id           uuid NOT NULL,
    group_id            uuid NOT NULL,
    name                text NOT NULL,
    max_concurrent_runs int  NOT NULL DEFAULT 0,
    memory_limit_bytes  bigint NOT NULL DEFAULT 0,
    status              text NOT NULL DEFAULT 'active',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id),
    CONSTRAINT groups_max_runs_check CHECK (max_concurrent_runs >= 0),
    CONSTRAINT groups_memory_check CHECK (memory_limit_bytes >= 0),
    CONSTRAINT groups_status_check CHECK (status IN ('active', 'draining', 'retired'))
);

-- A group always belongs to a provisioned tenant.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'groups_tenant_fk'
                     AND conrelid = 'groups'::regclass) THEN
        ALTER TABLE groups ADD CONSTRAINT groups_tenant_fk
            FOREIGN KEY (tenant_id) REFERENCES tenants (tenant_id);
    END IF;
END
$$;

-- Names are per tenant so "pack-default" and "user" are stable handles the
-- placement strategies can upsert by.
CREATE UNIQUE INDEX IF NOT EXISTS groups_name ON groups (tenant_id, name);
CREATE INDEX IF NOT EXISTS groups_by_status ON groups (tenant_id, status);

-- One row per session: the session's current placement fact. The row is not a
-- static binding: epoch is monotonic and lease_until bounds how long the named
-- worker may act for the session. An expired lease is adoptable by any worker;
-- a live one can only be renewed by its holder. A move to another group
-- requires the old lease to be expired (drained), so two groups cannot both
-- hold a live placement for the same session.
--
-- lease_until is NOT NULL: "no lease" is expressed as an already-expired
-- timestamp, which keeps the adoption predicate a single comparison.
CREATE TABLE IF NOT EXISTS session_placement (
    tenant_id   uuid NOT NULL,
    session_id  uuid NOT NULL,
    group_id    uuid NOT NULL,
    worker_id   text NOT NULL DEFAULT '',
    epoch       bigint NOT NULL DEFAULT 1,
    lease_until timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, session_id),
    CONSTRAINT session_placement_epoch_check CHECK (epoch > 0),
    CONSTRAINT session_placement_group_fk
        FOREIGN KEY (tenant_id, group_id)
        REFERENCES groups (tenant_id, group_id)
);

CREATE INDEX IF NOT EXISTS session_placement_group
    ON session_placement (tenant_id, group_id);
CREATE INDEX IF NOT EXISTS session_placement_lease
    ON session_placement (lease_until);

-- A run belongs to the group it was admitted into. The runs table stays the one
-- active-run account (there is no second slot ledger): placement admission
-- counts pending+running rows here, and this column is what makes the group
-- count correct across a later placement move. NULL marks a row enqueued
-- before placement admission existed (or by the legacy queue path); it still
-- counts against the tenant quota, never against a group's slots.
-- The column add is guarded by a catalog probe rather than ADD COLUMN IF NOT
-- EXISTS: schema.sql re-runs on every boot and in every test process, and an
-- ALTER TABLE would take ACCESS EXCLUSIVE on the hot runs table even when the
-- column is already there.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema()
                     AND table_name = 'runs' AND column_name = 'group_id') THEN
        ALTER TABLE runs ADD COLUMN group_id uuid;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'runs_group_fk'
                     AND conrelid = 'runs'::regclass) THEN
        ALTER TABLE runs ADD CONSTRAINT runs_group_fk
            FOREIGN KEY (tenant_id, group_id)
            REFERENCES groups (tenant_id, group_id);
    END IF;
END
$$;

-- Group slot counts and the tenant-wide quota sum are both predicates over the
-- same runs table; the partial indexes keep them proportional to active work,
-- not to the historical run count.
CREATE INDEX IF NOT EXISTS runs_active_group
    ON runs (tenant_id, group_id)
    WHERE group_id IS NOT NULL AND status IN ('pending', 'running');
CREATE INDEX IF NOT EXISTS runs_active_tenant
    ON runs (tenant_id)
    WHERE status IN ('pending', 'running');

-- --- run queue and ETA history (change keel-harness-v2, W2 queue+ETA) -------
--
-- A full group no longer refuses a run: the run row becomes 'queued', a durable
-- FIFO entry that holds no slot and that the kernel's claim predicate never
-- matches. queue_seq is the arrival order (a global sequence is enough because
-- positions are compared inside one group). memory_bytes is the admission
-- memory estimate: the pack strategy's headroom and the group memory ceiling
-- are sums over active rows, so the number travels with the run it describes.
--
-- Promotion is the single guarded transition queued -> pending. It runs in the
-- same transaction as the release that freed a slot (and on an attempt
-- settlement through the runtime's settlement hook), under the per-tenant
-- admission advisory lock, so a row is promoted exactly once and FIFO.
CREATE SEQUENCE IF NOT EXISTS runs_queue_seq;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema()
                     AND table_name = 'runs' AND column_name = 'queue_seq') THEN
        ALTER TABLE runs ADD COLUMN queue_seq bigint;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema()
                     AND table_name = 'runs' AND column_name = 'memory_bytes') THEN
        ALTER TABLE runs ADD COLUMN memory_bytes bigint NOT NULL DEFAULT 0;
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS runs_queued_fifo
    ON runs (tenant_id, group_id, queue_seq)
    WHERE status = 'queued';

-- Observed attempt durations: the ONLY source of the queue ETA average. A
-- sample is written when an attempt settles (the worker's settlement hook); the
-- placement store retains the newest samples per group and averages a window of
-- them. No history means "ETA unknown" (never a fabricated zero).
CREATE TABLE IF NOT EXISTS run_duration_samples (
    tenant_id   uuid NOT NULL,
    group_id    uuid NOT NULL,
    duration_ms bigint NOT NULL,
    observed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT run_duration_samples_duration_check CHECK (duration_ms > 0),
    CONSTRAINT run_duration_samples_group_fk
        FOREIGN KEY (tenant_id, group_id)
        REFERENCES groups (tenant_id, group_id)
);

CREATE INDEX IF NOT EXISTS run_duration_samples_group
    ON run_duration_samples (tenant_id, group_id, observed_at DESC);

-- Scheduled-trigger catch-up cursor (change keel-harness-v2, W10). One row per
-- (tenant, trigger): the newest scheduled window the engine processed. It is
-- the durable "last-run" a restart compares against, so an interval/cron
-- trigger can replay the windows that elapsed while the process was down
-- (bounded by the engine's catch-up limit/window). A trigger without a row has
-- never fired and MUST NOT catch up from zero.
--
-- No secret material is stored here: webhook HMAC keys are configuration, not
-- runtime state.
CREATE TABLE IF NOT EXISTS trigger_state (
    tenant_id         uuid NOT NULL,
    trigger_id        text NOT NULL,
    last_scheduled_at timestamptz NOT NULL,
    last_fired_at     timestamptz NOT NULL DEFAULT now(),
    last_status       text NOT NULL DEFAULT '',
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, trigger_id)
);
-- --- bot-identity-access (change keel-bot-identity-access, A1) --------------
--
-- Account / Role / Profile / Bot / TeamBot domain, permission grants,
-- ReBAC-lite relations and the credential/audit tables. Alpha zero-compat:
-- these are new tables. The old agents/agent_teams/api_keys rows are neither
-- read nor migrated here; their retirement is A4/A5.
--
-- Every table below is tenant-scoped and listed in rlsTenantTables
-- (internal/store/rls.go). bot_api_keys and auth_sessions are looked up by a
-- credential before the tenant is known, so those reads use the operator
-- bypass path exactly like api_keys (see store.LookupAPIKey and
-- store.OperatorScoped); their normal request path stays tenant-scoped.
--
-- Identity types are deliberately mixed:
--   * account/role/profile/group/grant ids are uuid;
--   * bot/team_bot ids are text handles ("support-1"), the same stable,
--     URL-safe identity grammar the agent registry used and the presentation
--     the design's Bot/TeamBot examples use;
--   * resource_relations ids are text so one generic relation table can name a
--     uuid resource (session, task) and a text resource (bot, team_bot) without
--     a second column or a widening cast. The design's relation snippet shows
--     uuid; the entity examples and the existing agent-id grammar are text.

CREATE TABLE IF NOT EXISTS accounts (
    tenant_id      uuid NOT NULL,
    account_id     uuid NOT NULL,
    username       text NOT NULL,
    display_name   text NOT NULL,
    kind           text NOT NULL DEFAULT 'human',
    password_hash  text,
    is_super_admin boolean NOT NULL DEFAULT false,
    disabled_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, account_id),
    CONSTRAINT accounts_kind_check CHECK (kind IN ('human', 'bot', 'service'))
);

-- Usernames are unique per tenant under case folding; the original spelling is
-- kept for display.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_username
    ON accounts (tenant_id, lower(username));

-- Role is identity and soul only: it never grants a permission (L11/L12).
CREATE TABLE IF NOT EXISTS roles (
    tenant_id   uuid NOT NULL,
    role_id     uuid NOT NULL,
    name        text NOT NULL,
    instruction text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, role_id)
);

CREATE INDEX IF NOT EXISTS roles_by_tenant ON roles (tenant_id, updated_at DESC);

-- A profile is the Account + Role binding; it carries no runtime state.
CREATE TABLE IF NOT EXISTS profiles (
    tenant_id  uuid NOT NULL,
    profile_id uuid NOT NULL,
    account_id uuid NOT NULL,
    role_id    uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, profile_id),
    CONSTRAINT profiles_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES accounts (tenant_id, account_id),
    CONSTRAINT profiles_role_fk FOREIGN KEY (tenant_id, role_id)
        REFERENCES roles (tenant_id, role_id)
);

CREATE INDEX IF NOT EXISTS profiles_by_account ON profiles (tenant_id, account_id);
CREATE INDEX IF NOT EXISTS profiles_by_role ON profiles (tenant_id, role_id);

-- A Bot is Profile + ModelKind + Harness. The document-shaped columns
-- (default_model, harness) are validated by internal/botteam, not by a second
-- SQL dialect: model kinds are extensible, so no CHECK fixes the vocabulary in
-- a migration.
CREATE TABLE IF NOT EXISTS bots (
    tenant_id     uuid NOT NULL,
    bot_id        text NOT NULL,
    name          text NOT NULL,
    profile_id    uuid NOT NULL,
    model_kind    text NOT NULL,
    default_model jsonb NOT NULL,
    harness       jsonb NOT NULL DEFAULT '{}'::jsonb,
    archived      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, bot_id),
    CONSTRAINT bots_profile_fk FOREIGN KEY (tenant_id, profile_id)
        REFERENCES profiles (tenant_id, profile_id)
);

CREATE INDEX IF NOT EXISTS bots_by_tenant ON bots (tenant_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS bots_by_profile ON bots (tenant_id, profile_id);

-- A TeamBot is Mission + Roster. It is a resource, not a permission subject:
-- no account_id, no profile_id, no model_kind, no session ownership (L12).
-- revision/content_hash are the optimistic guard and change identity derived by
-- botteam.Seal; the store never invents them.
CREATE TABLE IF NOT EXISTS team_bots (
    tenant_id    uuid NOT NULL,
    team_bot_id  text NOT NULL,
    name         text NOT NULL,
    mission      text NOT NULL,
    roster       jsonb NOT NULL DEFAULT '[]'::jsonb,
    revision     bigint NOT NULL DEFAULT 0,
    content_hash text NOT NULL DEFAULT '',
    archived     boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, team_bot_id),
    CONSTRAINT team_bots_revision_check CHECK (revision >= 0)
);

CREATE INDEX IF NOT EXISTS team_bots_by_tenant ON team_bots (tenant_id, updated_at DESC);

-- Permission groups are presets of (key, scope) grants. Membership and grants
-- are additive only: there is no deny row and no nested group (L5).
CREATE TABLE IF NOT EXISTS permission_groups (
    tenant_id  uuid NOT NULL,
    group_id   uuid NOT NULL,
    name       text NOT NULL,
    builtin    boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS permission_groups_name
    ON permission_groups (tenant_id, lower(name));

CREATE TABLE IF NOT EXISTS permission_group_members (
    tenant_id  uuid NOT NULL,
    group_id   uuid NOT NULL,
    account_id uuid NOT NULL,
    added_by   uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, group_id, account_id),
    CONSTRAINT permission_group_members_group_fk FOREIGN KEY (tenant_id, group_id)
        REFERENCES permission_groups (tenant_id, group_id) ON DELETE CASCADE,
    CONSTRAINT permission_group_members_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS permission_group_members_by_account
    ON permission_group_members (tenant_id, account_id);

CREATE TABLE IF NOT EXISTS permission_group_grants (
    tenant_id      uuid NOT NULL,
    group_id       uuid NOT NULL,
    permission_key text NOT NULL,
    scope          text NOT NULL,
    PRIMARY KEY (tenant_id, group_id, permission_key, scope),
    CONSTRAINT permission_group_grants_group_fk FOREIGN KEY (tenant_id, group_id)
        REFERENCES permission_groups (tenant_id, group_id) ON DELETE CASCADE,
    CONSTRAINT permission_group_grants_scope_check
        CHECK (scope IN ('own', 'team', 'tenant', 'all'))
);

-- Direct account grants only. Group authorization lives exclusively in
-- permission_group_grants; allowing a group subject here would create a second
-- truth and a silent no-op write (L5 / A1 decision 5). The effective set is the
-- union of account direct grants and every group's grants; the highest scope per
-- key wins.
CREATE TABLE IF NOT EXISTS permission_grants (
    tenant_id      uuid NOT NULL,
    grant_id       uuid NOT NULL,
    subject_type   text NOT NULL,
    subject_id     uuid NOT NULL,
    permission_key text NOT NULL,
    scope          text NOT NULL,
    granted_by     uuid NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, grant_id),
    CONSTRAINT permission_grants_subject_check
        CHECK (subject_type = 'account'),
    CONSTRAINT permission_grants_scope_check
        CHECK (scope IN ('own', 'team', 'tenant', 'all'))
);

-- Existing alpha databases may carry the earlier two-value CHECK. Tighten it
-- (no compatibility shim: group rows, if any, make startup fail loudly).
ALTER TABLE permission_grants DROP CONSTRAINT IF EXISTS permission_grants_subject_check;
ALTER TABLE permission_grants ADD CONSTRAINT permission_grants_subject_check
    CHECK (subject_type = 'account');

CREATE UNIQUE INDEX IF NOT EXISTS permission_grants_unique
    ON permission_grants (tenant_id, subject_type, subject_id, permission_key, scope);
CREATE INDEX IF NOT EXISTS permission_grants_by_subject
    ON permission_grants (tenant_id, subject_type, subject_id);

-- ReBAC-lite. Relation vocabulary is closed (owner/team_member/viewer/editor)
-- and is checked here; resource_type stays open so new resource kinds need no
-- migration. The primary key covers the five-tuple under the tenant.
CREATE TABLE IF NOT EXISTS resource_relations (
    tenant_id     uuid NOT NULL,
    resource_type text NOT NULL,
    resource_id   text NOT NULL,
    relation      text NOT NULL,
    subject_type  text NOT NULL,
    subject_id    text NOT NULL,
    created_by    uuid NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, resource_type, resource_id, relation, subject_type, subject_id),
    CONSTRAINT resource_relations_relation_check
        CHECK (relation IN ('owner', 'team_member', 'viewer', 'editor')),
    CONSTRAINT resource_relations_subject_check
        CHECK (subject_type IN ('account', 'group', 'team_bot'))
);

-- The evaluator asks "which relations does this subject have?"; the primary key
-- already serves "which subjects relate to this resource?".
CREATE INDEX IF NOT EXISTS resource_relations_by_subject
    ON resource_relations (tenant_id, subject_type, subject_id);

-- Browser auth sessions (A2 owns the cookie/Argon2id path). The unique
-- session_id index is the pre-tenant lookup path; a cookie names its session,
-- not its tenant.
CREATE TABLE IF NOT EXISTS auth_sessions (
    tenant_id      uuid NOT NULL,
    session_id     uuid NOT NULL,
    account_id     uuid NOT NULL,
    issued_at      timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    revoked_at     timestamptz,
    user_agent_hash bytea,
    ip_hash         bytea,
    PRIMARY KEY (tenant_id, session_id),
    CONSTRAINT auth_sessions_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS auth_sessions_id ON auth_sessions (session_id);
CREATE INDEX IF NOT EXISTS auth_sessions_by_account
    ON auth_sessions (tenant_id, account_id, expires_at DESC);

-- Append-only auth audit. account_id is nullable because a failed login may
-- name no existing account; actor_account_id is the operator for super-admin
-- actions. No FK: an audit row must outlive a disabled/renamed account.
CREATE TABLE IF NOT EXISTS auth_events (
    tenant_id        uuid NOT NULL,
    event_id         bigserial NOT NULL,
    account_id       uuid,
    actor_account_id uuid,
    kind             text NOT NULL,
    result           text NOT NULL DEFAULT 'ok',
    detail           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, event_id)
);

CREATE INDEX IF NOT EXISTS auth_events_by_account
    ON auth_events (tenant_id, account_id, event_id);
CREATE INDEX IF NOT EXISTS auth_events_by_actor
    ON auth_events (tenant_id, actor_account_id, created_at DESC, event_id DESC);
CREATE INDEX IF NOT EXISTS auth_events_by_created
    ON auth_events (tenant_id, created_at DESC);
-- The 记录 read orders by (created_at, event_id) and pages with a keyset cursor;
-- this composite index is the one the bounded query uses.
CREATE INDEX IF NOT EXISTS auth_events_by_created_event
    ON auth_events (tenant_id, created_at DESC, event_id DESC);

-- Bot credentials (L18). A key binds to one (account, bot) pair; the plaintext
-- is returned once and only its SHA-256 hash plus a display prefix are stored.
-- key_hash is unique globally because the lookup happens before the tenant is
-- known; the tenant is the answer, not the question (same shape as api_keys).
CREATE TABLE IF NOT EXISTS bot_api_keys (
    tenant_id   uuid NOT NULL,
    key_id      uuid NOT NULL,
    key_hash    bytea NOT NULL,
    key_prefix  text NOT NULL,
    account_id  uuid NOT NULL,
    bot_id      text NOT NULL,
    label       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    revoked_at  timestamptz,
    last_used_at timestamptz,
    PRIMARY KEY (tenant_id, key_id),
    CONSTRAINT bot_api_keys_hash_len_check CHECK (octet_length(key_hash) = 32),
    CONSTRAINT bot_api_keys_account_fk FOREIGN KEY (tenant_id, account_id)
        REFERENCES accounts (tenant_id, account_id) ON DELETE CASCADE,
    CONSTRAINT bot_api_keys_bot_fk FOREIGN KEY (tenant_id, bot_id)
        REFERENCES bots (tenant_id, bot_id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS bot_api_keys_hash ON bot_api_keys (key_hash);
CREATE INDEX IF NOT EXISTS bot_api_keys_by_bot
    ON bot_api_keys (tenant_id, bot_id, created_at DESC);

-- Session ownership projections (A4 closes the non-null contract; A1 only adds
-- the nullable columns so new code can start writing them without touching the
-- old run path). account_id is derived from the Bot's profile; created_by
-- records the initiator (human or trigger); team_bot_id is an optional mission
-- snapshot, never a permission subject.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS bot_id text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS account_id uuid;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS created_by_account_id uuid;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS team_bot_id text;

CREATE INDEX IF NOT EXISTS sessions_by_bot
    ON sessions (tenant_id, bot_id) WHERE bot_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sessions_by_account
    ON sessions (tenant_id, account_id) WHERE account_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sessions_by_created_by
    ON sessions (tenant_id, created_by_account_id) WHERE created_by_account_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS sessions_by_team_bot
    ON sessions (tenant_id, team_bot_id) WHERE team_bot_id IS NOT NULL;
