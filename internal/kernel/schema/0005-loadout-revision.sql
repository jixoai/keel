-- 0005-loadout-revision.sql
--
-- Run-level loadout pin (change: keel-service-graph, workflow W2).
--
-- A run executes under one immutable service loadout snapshot. The worker
-- records the snapshot identity and the fencing epoch it finished with, so an
-- audit/handoff reader can answer "which loadout revision/generation did this
-- run finish under" without reading process memory.
--
-- The new columns are additive and nullable: rows settled before this change
-- (and rows settled by a process that does not adopt the service graph) keep
-- NULL, and the run-status response shape is unchanged. The owner epoch is
-- recorded separately from runs.owner_epoch because the latter is the live
-- lease mirror and is cleared on settle.
--
--   loadout_id           snapshot id ("default")
--   loadout_revision     tenant configuration revision the snapshot resolved
--   loadout_generation   in-process service graph generation that served it
--   loadout_owner_epoch  fencing epoch of the claim that settled the run

ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_id text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_revision bigint;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_generation bigint NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS loadout_owner_epoch bigint NOT NULL DEFAULT 0;

-- Normalize historical column states: an earlier iteration of this migration
-- added these columns without NOT NULL, and ADD COLUMN IF NOT EXISTS is a no-op
-- once the column exists, so a database that saw that iteration keeps NULLs
-- that the read model cannot scan. Bounded to the NULL rows and idempotent.
ALTER TABLE runs ALTER COLUMN loadout_id SET DEFAULT '';
ALTER TABLE runs ALTER COLUMN loadout_revision SET DEFAULT 0;
ALTER TABLE runs ALTER COLUMN loadout_generation SET DEFAULT 0;
ALTER TABLE runs ALTER COLUMN loadout_owner_epoch SET DEFAULT 0;
UPDATE runs SET loadout_id = '' WHERE loadout_id IS NULL;
UPDATE runs SET loadout_revision = 0 WHERE loadout_revision IS NULL;
UPDATE runs SET loadout_generation = 0 WHERE loadout_generation IS NULL;
UPDATE runs SET loadout_owner_epoch = 0 WHERE loadout_owner_epoch IS NULL;
ALTER TABLE runs ALTER COLUMN loadout_revision SET NOT NULL;
ALTER TABLE runs ALTER COLUMN loadout_generation SET NOT NULL;
ALTER TABLE runs ALTER COLUMN loadout_owner_epoch SET NOT NULL;
