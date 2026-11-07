-- 0001-kernel-fencing.sql
--
-- The contract kernel's additive schema change (change: keel-service-graph,
-- workflow W1; converged after the P0 adversarial review: the session row is
-- the single linearization point).
--
-- sessions gets the lease authority: owner, owner_epoch and lease_until live on
-- the same row that already serializes sequence allocation (SELECT ... FOR
-- UPDATE in BeginSession), so Claim / Renew / Commit / Finish all linearize on
-- one lock.
--
-- runs keeps a mirror (owner, owner_epoch, lease_until) of the same values for
-- two readers that predate the kernel: the legacy queue scan
-- (store.ClaimRun) and the run-status endpoint. The mirror is only ever written
-- inside a transaction that holds the session row lock, so it cannot diverge
-- from the authority.
--
-- Only new columns and one index are added; no existing column, payload or
-- query is modified.

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS owner text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS owner_epoch bigint NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS lease_until timestamptz;

ALTER TABLE runs ADD COLUMN IF NOT EXISTS owner text;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS owner_epoch bigint NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS runs_lease_reclaim ON runs (status, lease_until);
