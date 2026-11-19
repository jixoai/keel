W2.5 approve + notify — evidence index (2026-11-19)

Scope
-----
Implement the `approve` and `notify` aggregate tools for real, backed by the
`keel.approval` / `keel.notify` builtin plugins, with durable facts, human-in-
the-loop waiting, validated provenance, multi-consumer notifications and a
single-use capability URL lifecycle.

Status
------
approve
- confirm / prompt / choice: implemented in internal/agentext/approve_plugin.go.
- confirm: durable decision with yes/no options; an answered "no" is a
  validated refusal; a timeout reports status=timed_out and safeSide.optionId=no.
- prompt: free text is the declared answer space (validated=true).
- choice: declared options are validated=true; the "other" free-text escape is
  validated=false, escape=true, carried in the tool result and the provenance.
- Human-in-the-loop: the tool opens a durable decision, blocks on Wait, and can
  never answer its own question. Answers arrive through the existing decisions
  API; the answer route now records provenance.
- Timeout: the persisted decision deadline (internal/agentext/decisions.go), not
  a tool-local guess; re-reading returns the same timed_out state.
- Cancel: a cancelled run durably cancels its open questions
  (DecisionBroker.CancelSession) and records status=cancelled.
- Provenance: internal/approval (table approval_provenance) records actor,
  channel, remoteAddr/userAgent/requestId context, opened/delivered/answered
  instants, validated and escape. A known actor can upgrade an "unknown" record
  but is never downgraded.
- Routes: GET /v1/sessions/{id}/approvals and
  GET /v1/sessions/{id}/approvals/{decisionId}; POST on the existing decisions
  route returns `provenance` for approval.* decisions.

notify
- push / list / ack: implemented in internal/agentext/notify_plugin.go over
  internal/notify (table notifications).
- Consumers: client (live, ack-capable), native (system target), url (single-use
  consumption link), img (delivery pixel). Per-consumer state; any ack consumes.
- Three-state window: a live ack-capable consumer that acks inside the window
  suppresses the fallback push; otherwise exactly one push is attempted after
  the window (online or not). The push claim is a durable compare-and-set, so a
  restart never pushes twice; a claimed-but-unrecorded push is recorded as the
  explicit gap "push_result_unknown".
- Web target without service-worker capability: explicit in-page degradation
  with gap "web_service_worker_unavailable"; no fake system push. A deployment
  with no Pusher reports "pusher_unavailable" for native/web attempts.
- URL lifecycle: 256-bit random token, SHA-256 stored at rest, per-consumer
  expiry (default 24h), single use; replay/expired -> 410, wrong token -> 404.
  The open/pixel routes need no tenant header (the token is the capability); a
  supplied tenant header is checked against the row.
- Facts: notification.ack (key notif-ack:<notificationId>:<consumerId>) and
  notification.push_sent (key notif-push:<notificationId>) through the kernel
  system lane; payloads are stable so retries dedupe.
- Routes: GET /v1/notifications, POST /v1/notifications/{id}/ack,
  GET /v1/notifications/{id}/open?token=..., GET /v1/notifications/{id}/pixel?token=...

Persistence
-----------
- internal/store/schema.sql: approval_provenance, notifications (+ indexes).
- internal/store/rls.go: both tables added to rlsTenantTables.
- Store implementations live in internal/approval/store.go and
  internal/notify/store.go (memory + PostgreSQL over store.Store.Pool()).
- Tenant isolation is enforced by every query's tenant predicate and by RLS for
  tenant-pool access; integration tests cover cross-tenant reads/acks.

Tool wiring / plugin lifecycle
------------------------------
- toolsurface.Backends gained Approve/Notify; the tool names are constant and a
  nil backend answers not_wired.
- Backends.keel.approval / keel.notify route through Manager.InvokeToolResult;
  a disabled plugin maps to plugin_disabled.
- Builtin manifests: internal/pluginpkg/builtin/keel.approval.yaml,
  keel.notify.yaml (internal tools only; provides.tools stays empty).
- cmd/keel/serve.go registers the two factories and wires the services.

Gates (final tree)
------------------
- go build ./...                                              exit=0
- go vet <changed packages>                                   exit=0
- KEEL_TEST_DSN=... go test ./...                             exit=0, 0 FAIL
- KEEL_TEST_DSN=... go test -race <changed packages>          exit=0, no DATA RACE
- ./scripts/agent-runtime-e2e.sh                              exit=0, PASS
- orphan check (keel serve / keel-agent-e2e)                  none

Logs: go-build.txt, go-vet.txt, test-full-dsn.txt, test-race.txt,
agent-runtime-e2e.txt, orphans.txt.

Test map (requirement -> test)
------------------------------
- approve three modes: internal/agentext/approve_plugin_test.go
- refusal / timeout / cancel: same file (answered "no", 1s timeout, ctx cancel)
- provenance audit + validated/escape: internal/approval/approval_test.go,
  internal/agentapi/approvals_notifications_test.go
- multi-consumer push/ack: internal/notify/notify_test.go,
  internal/agentext/notify_plugin_test.go
- URL single use / expiry / wrong token: internal/notify/notify_test.go,
  internal/agentapi/approvals_notifications_test.go
- cross-tenant isolation: internal/approval/store_pg_test.go,
  internal/notify/store_pg_test.go, internal/notify/notify_test.go,
  internal/agentapi/approvals_notifications_test.go
- event-key idempotency / no re-push after restart:
  internal/notify/notify_test.go
- tool/plugin_disabled/not_wired: internal/toolsurface/approve_notify_test.go
- plugin manifests match the Go surface: internal/agentext/plugin_builtin_test.go

Known gaps / explicit decisions
-------------------------------
1. No operating-system push implementation is wired in this backend slice. The
   Pusher seam is defined and tested with a fake; with no Pusher, native/web
   attempts are recorded as degraded with a named gap. The client-shell system
   notification work (W11) owns the real channel. Nothing claims a fake push.
2. Live delivery of a notification to an online client is not an SSE event yet
   (no notification.created kind was added in this wave). Clients read
   GET /v1/notifications. The online/ackcapable declaration in the push is the
   presence input to the window truth table.
3. Notification UI panel is out of scope for this backend slice.
4. The DB comment for notifications says the raw token is returned once; the
   retried push returns the durable row without URLs (tokens are hash-only).
