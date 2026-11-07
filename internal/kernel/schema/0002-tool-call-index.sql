-- 0002-tool-call-index.sql
--
-- Pairing lookup indexes (P1-4 of the implementation review). SessionTxn's
-- FindToolCall runs while the session write lock is held, so it must not walk
-- the whole log:
--
--   events_assistant_tool_calls (GIN, jsonb_path_ops, partial)
--     lets the jsonpath probe find a message by tool call id in O(log n) on
--     large tables. jsonb_path_ops hashes path/value pairs, so the index stays
--     proportional to the JSON structure, not to the message size.
--
--   events_assistant_scan (btree, partial, seq DESC)
--     makes the backwards walk step only over assistant messages, which is the
--     window that actually matters: a tool result refers to a call in one of
--     the newest assistant messages. On a session-scoped query the planner
--     prefers this path (the GIN alternative would re-check many heap rows
--     across the whole table), and with it the common case stops at the first
--     assistant message.
--
-- Both indexes are additive. If they are missing the query still answers
-- correctly (a filtered reverse scan over the session), only slower.

CREATE INDEX IF NOT EXISTS events_assistant_tool_calls
    ON events USING gin (payload jsonb_path_ops)
    WHERE kind = 'assistant.message';

CREATE INDEX IF NOT EXISTS events_assistant_scan
    ON events (tenant_id, session_id, seq DESC)
    WHERE kind = 'assistant.message';
