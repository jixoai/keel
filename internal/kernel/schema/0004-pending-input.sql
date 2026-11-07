-- 0004-pending-input.sql
--
-- Unread edge input, durably flagged on the run row (change keel-service-graph,
-- workflow W4; adversarial review B2).
--
-- The log order cannot tell a successor whether the newest assistant message
-- answered the newest user message: input that arrives while an attempt is
-- finishing sits BEFORE that attempt's answer in the log (the answer is
-- committed after it), so "the tail is an assistant message" is not a safe
-- "nothing is owed" signal, and the cursor has already been pushed past the
-- input by the run's own commits.
--
-- The flag is the durable fact:
--   * an edge-scoped user.message commit sets it in the same transaction that
--     queues the run (see pgTxn.Append);
--   * a claim reads it and does NOT clear it (adversarial review B3): an
--     attempt that is abandoned before finishing must leave the fact behind for
--     its successor, otherwise the next attempt sees an old assistant tail and
--     settles without answering the unread input;
--   * a finish recomputes it from its read-through probe, so a re-queued run
--     hands the next attempt a truthful "there is input you have not read".
--
-- It is additive and defaults to false for every existing row.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS pending_input boolean NOT NULL DEFAULT false;

-- The previous attempt's load high-water mark (adversarial review B5): input
-- with seq > read_watermark was never part of that attempt's transcript. The
-- flag alone says "something is unread"; the watermark says WHICH input is
-- unread, so the successor can present it to the model last instead of leaving
-- it buried behind an answer the previous attempt produced.
--
--   * a finish writes the read-through watermark it proved (the value the
--     worker passes as FinishRequest.ReadThrough);
--   * a failed or abandoned attempt leaves it unchanged (nothing was proven);
--   * a claim delivers it; it is never cleared by a claim.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS read_watermark bigint NOT NULL DEFAULT 0;

-- Same normalization as 0005: if an earlier iteration created these columns
-- without DEFAULT/NOT NULL, ADD COLUMN IF NOT EXISTS will not fix them, and the
-- edge wake INSERT (which sets neither column) would violate the constraint.
ALTER TABLE runs ALTER COLUMN pending_input SET DEFAULT false;
ALTER TABLE runs ALTER COLUMN read_watermark SET DEFAULT 0;
UPDATE runs SET pending_input = false WHERE pending_input IS NULL;
UPDATE runs SET read_watermark = 0 WHERE read_watermark IS NULL;
ALTER TABLE runs ALTER COLUMN pending_input SET NOT NULL;
ALTER TABLE runs ALTER COLUMN read_watermark SET NOT NULL;
