-- 0003-external-handoff.sql
--
-- Loadout pinning on the run row (runtime-handoff spec: loadout 版本固定).
-- Every claim records the runtime's loadout id/revision so a handoff audit can
-- answer "which run completed on which revision/epoch". owner and owner_epoch
-- already exist (0001); these two columns complete the pin.
--
-- The external-session marks (state_owner, external_backend, external_ref,
-- external_status, metering) live in the store's base schema (internal/store/
-- schema.sql) because store.EnqueueRun's external-session guard reads them on
-- the native queue path; the kernel's external seam (external.go) documents
-- the same mark set. Keeping one source there avoids a runtime that would work
-- only until someone wires the kernel.
--
-- Only new columns are added; no existing column, payload or query is
-- modified.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_id text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_revision bigint NOT NULL DEFAULT 0;

