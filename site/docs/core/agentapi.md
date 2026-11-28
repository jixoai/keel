# Harness API reference

The **Harness API** is the `/v1` product surface. Its wire-protocol name, `AgentAPI`, is
retained as a technical alias (see the [glossary](/glossary)).
This page is the endpoint reference with request/response shapes. Shapes are taken from the
handlers that emit them; examples marked *verified* were captured from a running `keel serve` at
the documented revision.

## 1. Two HTTP faces, one process shell

| Face | Package | Purpose | Who serves it |
|---|---|---|---|
| **Core shell** | `internal/httpapi` | the kernel-direct surface: tenants, sessions, input, events, SSE, run rows, files, runtime drain | `cmd/runtime` (runtime) and `cmd/keel` (product subset) |
| **Harness API** | `internal/harnessapi` | the product surface: session listing, cancel, decisions, plugins, triggers, sub-agents, capabilities | `cmd/keel serve` |

Both mount under `/v1`; a breaking shape change would require `/v2`. New capabilities are
additive, and clients discover them — see §6.

Auth differs by deployment, and it is a network middleware concern, never a kernel one:

- **`keel serve`**: a human exchanges username + password at `POST /v1/auth/login` for an
  HMAC-signed HttpOnly cookie and sends `X-Keel-CSRF` on writes; a machine presents a Bot-bound
  `keel_bot_...` key as `Authorization: Bearer`. The server injects the tenant and strips any
  client-supplied `X-Tenant-ID` (`docs/HARNESS.md`, `cmd/keel/auth.go`). An empty database has no
  super admin: `keel admin bootstrap` mints a one-time setup token; `--auth local-only` maps
  loopback requests to a built-in local admin, and `off` needs explicit `--insecure`.
- **`cmd/runtime`**: `X-Tenant-ID` names the tenant on every request; administrator endpoints
  additionally require `Authorization: Bearer <KEEL_ADMIN_KEY>`. `X-Tenant-ID` is a row id, never
  an identity credential; with no admin key configured the admin API is closed (503).

## 2. Core shell endpoints

From `internal/httpapi/server.go:209`:

| Method | Path | Semantics |
|---|---|---|
| `GET` | `/v1/health` | liveness; always 200, includes goroutine/heap counters |
| `GET` | `/v1/ready` | PG ping + running-schema check (tables and kernel migration columns); 503 + `Retry-After` when not ready or draining |
| `GET` | `/v1/metrics` · `GET` `/metrics` | JSON snapshot and Prometheus text 0.0.4; fixed label cardinality, no tenant/session/run labels |
| `POST` | `/v1/tenants` | create a tenant (admin key); body `{"name":"…"}`, id from `X-Tenant-ID` |
| `PUT`/`GET` | `/v1/tenants/{id}/config` | replace / read the tenant configuration document; unknown fields or bad values 400, oversized body 413, unknown tenant 404 |
| `GET` | `/v1/tenants/{id}/usage` | token counts grouped by session/provider/model plus totals (counts only — no prices) |
| `POST` | `/v1/sessions` | create a session; returns `{"sessionId":"…"}` |
| `POST` | `/v1/sessions/{id}/input` | commit an input; body `{"text","requestId?","delivery?:queue\|guide","selection?"}`. **201** new commit, **200** idempotent repeat, **202** when the run is waiting for a group slot (`queued{position,etaSeconds}`); 429 is reserved for the tenant quota / memory ceiling, never for queueing (`internal/httpapi/server.go:497`) |
| `POST` | `/v1/sessions/{id}/retry` | re-ask an earlier input; the ledger records `retryOf` with the earlier seq |
| `POST` | `/v1/sessions/{id}/run` | queue a run without new input (404 for missing/cross-tenant sessions, and it does not create a runs row for them) |
| `GET` | `/v1/sessions/{id}/run` | run row: `status`/`attempts`/`cursorSeq`/`lastError`/`leaseUntil` |
| `GET` | `/v1/sessions/{id}/run/loadout` | the loadout pin recorded at finish: `loadoutId`/`revision`/`generation`/`ownerEpoch` |
| `POST` | `/v1/sessions/{id}/events` | append an event — accepts `user.message` only, anything else 400, cross-tenant/missing 404 |
| `GET` | `/v1/sessions/{id}/events` | committed events with `after`/`limit` (snapshot and resume) |
| `GET` | `/v1/sessions/{id}/events/stream` | SSE; `after`/`Last-Event-ID` resume, `partials=1` for token frames |
| `POST` | `/v1/files` | upload a blob (multipart or raw body); returns `fileId` + `url` |
| `GET` | `/v1/files/{id}` · `/v1/files/{id}/meta` | stream the bytes (Range/offset resume) / metadata |
| `POST` | `/v1/mcp/servers/{name}/signin` · `/v1/mcp/callback` | MCP OAuth sign-in start and callback (fail-closed without `KEEL_SECRET_KEY` etc.) |
| `POST` | `/v1/runtime/drain` · `GET` `/v1/runtime/status` | explicit drain (`{"mode":"handoff"\|"shutdown","deadlineMs":N}`) and runtime observability |

Verified response shapes (`created` is what separates 201 from an idempotent 200; a queued run
answers 202 with the same body plus a `queued` block — `internal/httpapi/server.go:924`):

```json
// POST /v1/sessions
{"sessionId":"a681bd12-b772-462f-b4ce-122b69977818"}

// POST /v1/sessions/{id}/input   (201 when created, 200 when the requestId repeats)
{"created":true,"requestId":"9e5088a0-892c-4af3-b123-43c8a6c7fcf4","seq":1,"status":"committed","submissionId":"05c9689a-f556-45cf-8996-9b08c56144e0"}

// POST /v1/sessions/{id}/input   (202: the run is queued behind a full group slot)
{"created":true,"requestId":"…","seq":2,"status":"committed","submissionId":"…",
 "queued":{"position":3,"active":2,"limit":2,"etaKnown":true,"etaSeconds":41}}

// GET /v1/sessions/{id}/run
{"attempts":1,"cursorSeq":3,"lastError":"","leaseUntil":null,"status":"done"}
```

## 3. Harness API endpoints

From `internal/harnessapi/api.go:386`:

| Method | Path | Semantics |
|---|---|---|
| `GET` | `/v1/capabilities` | capability document (§6) |
| `GET` | `/v1/providers` · `POST` `/v1/providers/{id}/test` · `GET` `/v1/models` | configured providers (plus a live test) and models (product configuration) |
| `GET` | `/v1/sessions?limit=&after=` | session list, keyset pagination, `runStatus` overlay |
| `GET` | `/v1/sessions/search?q=&scope=&limit=` | bounded transcript/session search; `scope=messages\|sessions`, 400 without `q` |
| `GET` | `/v1/sessions/{id}/export` | bounded transcript export, page bytes capped by `exportPageBytes` |
| `GET` | `/v1/sessions/{id}/deliverables` | the manifest-backed delivery projection (files the session produced) |
| `GET` | `/v1/sessions/{id}/task-panel` · `/v1/sessions/{id}/file-preview` | the run surface's task tree/event stream and a manifest-checked file page |
| `POST` | `/v1/sessions/{id}/cancel` | stop the current attempt: idempotent; 202 while `cancelling`, 409 for an external session, 404 unknown, 501 when the seam is not wired (cross-process) |
| `GET`/`POST` | `/v1/sessions/{id}/decisions` | list / open an approval decision |
| `GET`/`POST` | `/v1/sessions/{id}/decisions/{decisionId}` | read / answer (idempotent; a second identical answer returns `answered` and produces no new event; 409 on conflict) |
| `GET` | `/v1/plugins` | plugin inventory: `revision`/`generation`/`plugins[]`/`tools[]`/`prompt[]`/`enabled[]`/`hooks`/`lifecycle`/`limits` |
| `POST` | `/v1/plugins/install` · `/v1/plugins/{id}/enable\|disable\|uninstall\|update` | the lifecycle writes; a write that needs a durable approval answers **202** with a decision id and is repeated with `decisionId` |
| `GET` | `/v1/plugins/audit` · `/v1/plugin-approvals/{decisionId}` | the plugin audit trail and the durable approval record |
| `GET`/`POST` | `/v1/plugins/{id}/bridge/…` | the `keel.plugin.*` bridge: `state`, `call`, `approve`, `subscribe` and the per-plugin approval pair |
| `GET` | `/v1/triggers` · `POST` `/v1/triggers/{id}/fire` | trigger audit and manual fire with an idempotency key |
| `GET` | `/v1/subagents` · `GET` `/v1/subagents/{id}` | sub-agent list / detail |
| `POST` | `/v1/sessions/{id}/subagents` | spawn a sub-agent (`wait:true` waits synchronously; a disconnect does not cancel it) |
| `GET` | `/v1/quota` · `GET` `/v1/auth/whoami` · `POST` `/v1/admin/tenants` | tenant quota, caller identity/effective grants, tenant provisioning |

Identity, permissions and Bots (change `keel-bot-identity-access`; full table in
`docs/HARNESS.md` §2):

| Method | Path | Semantics |
|---|---|---|
| POST/GET | `/v1/auth/login`, `/v1/auth/logout`, `/v1/auth/session`, `/v1/auth/password`, `/v1/auth/bootstrap` | account login/logout, CSRF + bootstrap state, password change (forced only for a temporary password issued by a CLI/admin reset), one-time setup token |
| GET/POST | `/v1/accounts`, `POST …/disable`, `POST …/password-reset`, `GET/POST/DELETE …/groups` | Account (login/permission subject), disable/reset/password-group writes |
| GET/POST/PATCH/DELETE | `/v1/roles`, `/v1/profiles` | Role (identity only, no permissions), Profile (Account + Role) |
| GET/POST/PATCH/DELETE | `/v1/permission-groups`, `/v1/permissions` | permission-key registry, scopes, groups and effective grants |
| GET/POST/PATCH | `/v1/bots`, `/v1/bots/{id}`, `/v1/bots/{id}/archive`, `/v1/bots/{id}/invoke` | Bot CRUD/archive/invoke; a Session belongs to a Bot → Account |
| GET/POST/DELETE | `/v1/bots/{id}/keys` | issue/list/revoke a Bot-bound key (plaintext shown once) |
| GET/POST/PATCH | `/v1/team-bots`, `/v1/team-bots/{id}`, `/v1/team-bots/{id}/roster`, `/v1/team-bots/{id}/start` | TeamBot = Mission + Roster; not a permission subject |

The retired `/v1/agents` and `/v1/teams` routes answer 404 with no alias.

Listing and plugin response shapes (abridged):

```json
// GET /v1/sessions?limit=2
{"hasMore":false,"nextCursor":"","sessions":[
  {"sessionId":"a681bd12-…","createdAt":"2026-11-10T03:48:24Z","updatedAt":"…","lastSeq":0,"runStatus":"idle","lastMessageAt":null,"title":""},
  {"sessionId":"586897e3-…","createdAt":"…","updatedAt":"…","lastSeq":3,"runStatus":"done","lastMessageAt":"…","title":"hello keel"}]}

// GET /v1/plugins   (the serve process registers the builtin plugins by default)
{"revision":3,"generation":1,"lifecycle":"dynamic",
 "enabled":["keel.example"],
 "tools":["example_audit_tail","example_status"],
 "prompt":[],
 "hooks":{"subscriptions":3,"emitted":0,"delivered":0,"dropped":0,"failed":0,"queued":0,"capacity":1024},
 "plugins":[{"id":"keel.example","version":"1.0.0","kind":"in-process","state":"enabled",
             "bundled":true,"source":{"type":"builtin"},
             "provides":{"tools":["example_status","example_audit_tail"],
                         "hooks":["trigger.after","decision.answered","subagent.finished"],
                         "triggers":["keel.example.ping"]},
             "tools":["example_audit_tail","example_status"],
             "hooks":["trigger.after","decision.answered","subagent.finished"]}],
 "limits":{"auditPageMax":200}}
```

The lifecycle states are the closed set `installed/enabled/disabled/failed/invalid`
(`internal/harnessext/plugin_manager.go:31`); `uninstalled` only appears in the audit trail, because
the row is deleted. An install/enable/update that needs a durable approval answers 202 and the same
request is repeated with the `decisionId` (`internal/harnessapi/plugins.go:13`).

## 4. External (AgentBackend) endpoints

| Method | Path | Semantics |
|---|---|---|
| `POST` | `/v1/external/sessions` | open an external session; body `{"backend":"dsh","reference":"manual-1"}` |
| `GET` | `/v1/external/sessions` · `/{id}` | list / query; the query answers the compatibility marks (`stateOwner`, `externalStatus`, `metering`, `nativeRecovery:false`) |
| `POST` | `/v1/external/sessions/{id}/messages` | send a message to the backend; returns `{"turnId":"…","recorded":true}` |
| `POST` | `/v1/external/sessions/{id}/cancel` · `/{id}/close` | cancel a turn / close the session |
| `GET` | `/v1/external/backends` | backend inventory and health |

External sessions never enter the native run/lease flow; their events are Keel's compatibility log
(`external.event`). The full protocol and its honest gaps are on
[Connect an external backend](/core/extend/external-backend).

## 5. Errors and honest refusals

| Status | Meaning on this API |
|---|---|
| 400 | malformed request or a payload the kernel would refuse |
| 401 | missing/invalid credentials, or a missing/invalid `X-Tenant-ID` |
| 404 | unknown session/tenant for this tenant (a foreign tenant's resource answers exactly the same way) |
| 409 | conflicting state: external session on a native endpoint, a decision already answered, or a native edge write into an external session |
| 413 | request body over the configured limit |
| 429 | capacity refusal: the tenant run quota or a memory budget is spent; retry after `Retry-After`. A full *group slot* is not a 429 — it is the 202 queued admission above (`internal/httpapi/server.go:849`) |
| 501 | the operation is not wired in this process (for example cancel across processes) — the API refuses instead of pretending |
| 503 | not ready, draining, or an unwired product seam ("decisions are not wired", "triggers are not wired") |
| 502 | sub-agent or backend failure |

Two refusals are deliberate design, not gaps: **501 for cross-process cancel** (the cancel
registration lives in the process executing the attempt) and **503 for unwired seams** (a
capability discovery flag is `false` at the same time, so a client can hide the control).

## 6. Capabilities and downgrade

The discovery document is the only correct way for a client to learn what this deployment serves
(`internal/harnessapi/api.go:556`). The shape (abridged — the booleans shown describe a fully wired
`keel serve`; a deployment without a seam reports the same key as `false`, and deployment-wired
blocks appear only when their seam is wired):

```json
{"apiVersion":"v1","serviceVersion":"keel-agent/0.1.0",
 "features":{"decisions":true,"hooks":true,"partials":true,"plugins":true,
             "plugins.manage":true,"plugins.audit":true,"plugins.approvals":true,"plugins.bridge":true,
             "sessions.cancel":true,"sessions.list":true,
             "session.search":true,"session.export":true,"session.deliverables":true,
             "session.filePreview":true,"task.panel":true,"team.tasks":true,
             "settings.models":true,"settings.secrets":false,"settings.skills":true,
             "skills.completion":true,"subagents":true,"triggers":true,
             "mcp.apps":true,"mcp.apps.sandboxOrigin":false,"mcp.apps.context":true,
             "mcp.apps.tools":true,"mcp.apps.resources":true,"mcp.apps.message":true,
             "mcp.apps.links":true,"mcp.events":false},
 "limits":{"sessionsPageDefault":50,"sessionsPageMax":200,"subagentsPageMax":200,
           "teamTasksPageDefault":50,"teamTasksPageMax":200,
           "searchPageDefault":20,"searchPageMax":100,
           "exportPageDefault":200,"exportPageMax":500,"exportPageBytes":524288,
           "deliverablesDefault":100,"deliverablesMax":500},
 "hooks":["subagent.started","subagent.finished","trigger.before","trigger.after",
          "turn.started","turn.finished","decision.opened","decision.answered","mcp.surface.changed"],
 "plugins":["keel.example"],"pluginRevision":3,
 "configFile":{"exists":true,"revision":2,"hash":"…","anchorEventSeq":0,"rotations":1}}
```

`features` is a flat map, and the dotted prefixes are the grouping a client should follow:
`plugins.*` (inventory, lifecycle writes, audit, durable approvals, the webview bridge),
`session.*` (search, export, deliverables, file preview), `task.panel`, `settings.*` (models,
secrets, skills), `mcp.*` (apps and events) and `team.tasks`. `configFile` is a block rather than a
flag: it is present only when the deployment loaded a `config.toml` file loader.

Rules a client should follow:

- Treat `features.*` as the switch: a `false` flag means hide the control, not "try and see".
- Treat `hooks` as a closed set. The hook vocabulary is closed by design; a plugin subscribing to a
  name outside this list is a configuration error, not an extension point.
- Never infer features from `serviceVersion`; the binary version moves independently of `/v1`.

## 7. SSE contract (summary)

The full semantics are in [Events and protocol](/core/events); the operational essentials:

| Property | Guarantee |
|---|---|
| `id:` on a frame | the committed event `seq` — resumable via `Last-Event-ID` / `after` |
| token partial frames (`partials=1`) | `event: partial`, **no** `id:`, never stored, never replayed |
| drain | `event: drain` with `{"retry_after_ms":N}` after unread events are flushed; reconnect with `Last-Event-ID` |
| ordering | strictly increasing `seq`, no gaps, no rollback |
| wake-up | `LISTEN/NOTIFY` plus heartbeat; a missed notification costs a heartbeat, never data |
