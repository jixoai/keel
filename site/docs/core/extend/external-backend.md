# Connect an external backend

An **external backend** is an agent runtime that owns its own session state and is driven by Keel
over a versioned protocol. Keel does not reimplement it, does not import it, and does not claim
native recovery for it. This page covers the protocol, the configuration vocabulary, the
state-ownership marks, failure isolation, and the gaps the project documents honestly.

## 1. Two provider kinds, one wire layer

The external gateway (`internal/gateway`) speaks JSON-RPC 2.0 over **stdio** or **Streamable HTTP**
with a versioned, private method set (`internal/gateway/protocol.go:32`):

```go
const ProtocolVersion = "2026-11-07"           // date-based, like MCP revisions
var SupportedProtocolVersions = []string{ProtocolVersion}
```

Two provider kinds exist, and one connection serves exactly one kind:

- **AgentBackend** — an external agent runtime (for example a DSH-compatible sidecar) whose sessions
  Keel tracks but does not own.
- **ToolProvider** — a stateless external tool server whose tools join the prompt registry.

The transport, framing and request lifecycle are reused from MCP; **the method semantics are not
MCP** (`internal/gateway/protocol.go:26`). Only the transport is shared — do not expect an MCP server
to answer these methods.

## 2. The method set (AgentBackend)

| Method | Direction | Meaning |
|---|---|---|
| `initialize` | request | negotiate `protocolVersion`, provider kind and capabilities; an incompatible version must refuse activation — the client will not activate a connection whose revision it cannot speak |
| `session.open` | request | open an external session; returns the backend's own `sessionId` (opaque to Keel), `stateOwner=external`, `metering`, `lastSeq` |
| `session.resume` | request | resume a session; `resumed=false` puts the session into `backend-unavailable` — Keel does not fake recovery |
| `message.send` | request | deliver one message; returns a `turnId` |
| `event.stream` | request with id (subscription) / notification without id (event) | subscribe to events / push one event |
| `cancel` | request | cancel a turn; a backend that cannot cancel **must** error explicitly (never report success) |
| `health` | request | `{status: ok\|degraded\|unavailable}` |
| `close` | request | with `sessionId`: close that session; without: graceful shutdown |

Protocol-reserved error codes exist for the three conditions a provider would otherwise invent
private ones for: `-32001` timeout, `-32002` cancelled, `-32003` unavailable
(`internal/gateway/protocol.go:48`).

## 3. Configuration: one vocabulary

External providers are declared in the same tenant configuration document as everything else
(`host.TenantConfig.External`), never in a second vocabulary (`docs/SERVICE-GRAPH.md:147`):

```json
{
  "external": {
    "backends": {
      "dsh": {
        "transport": "stdio",
        "command": "node",
        "args": ["/opt/keel/dsh-keel-sidecar.mjs"],
        "env": {
          "DSH_PROVIDER": "env:DSH_PROVIDER",
          "DSH_MODEL": "env:DSH_MODEL",
          "DSH_API_KEY": "secret:DSH_API_KEY"
        },
        "timeoutMs": 60000,
        "maxSessions": 16
      }
    },
    "tools": {
      "docs": {
        "transport": "http",
        "url": "https://tools.example.com/mcp",
        "headers": { "Authorization": "secret:DOCS_TOKEN" },
        "timeoutMs": 30000
      }
    },
    "quota": { "unmetered": "deny", "maxSessionsPerBackend": 32 }
  }
}
```

(The full file is `docs/examples/external-gateway.json`.) What the boot process does with it:

- `KEEL_EXTERNAL_CONFIG` can point the runtime's boot document at that file; the loadout only turns
  the services on or off — it does not invent fields (`docs/SERVICE-GRAPH.md:149`).
- `external.backends` non-empty ⇒ the `external-agent` service activates; `external.tools`
  non-empty ⇒ `external-tools` activates and its tools join the single prompt registry
  (`docs/LOADOUT.md:66`).
- **Secret references only.** `env`/`headers` values accept exactly `${NAME}`, `env:NAME` or
  `secret:NAME`; a literal secret — including `Bearer <token>` — is rejected and never stored in
  the schema (`docs/HOST.md:61`).
- **Environment is minimal.** A stdio provider gets `MinimalEnv` (an allowlist: PATH, HOME, …)
  plus exactly the variables it declared (`internal/gateway/process.go:103`); `RedactEnv` scrubs
  the audit copy. The reference sidecar has `-forbid-env=A,B` / `-require-env=C` flags to prove
  this in tests.
- **`external.quota.unmetered` is an explicit choice.** With metering declared as `external` or
  `none` by the backend, the tenant must decide: `allow` records `metering=none` and lets sessions
  open; `deny` refuses them. A missing usage report is never silently recorded as zero
  (`docs/SERVICE-GRAPH.md:163`).

## 4. State ownership: marked, never faked

External sessions live in the same `sessions` table but carry marks that keep them out of the native
machinery (`internal/store/schema.sql:32`):

| Mark | Meaning |
|---|---|
| `state_owner=external` | the backend owns the state; Keel records facts, it does not own recovery |
| `external_backend`, `external_ref` | which backend, and its opaque session handle |
| `external_status` | lifecycle: `active`, `closed`, `backend-unavailable` (queryable terminal state) |
| `metering` | `external` (backend reports usage) or `none` (explicitly unmetered) |

Hard boundaries the kernel enforces (fail-closed, at the commit gate rather than in HTTP
pre-checks):

- External sessions **never enter the native run/lease flow**: no `runs` row is created, the claim
  predicate filters `state_owner <> 'external'`, and even a pre-existing dirty run row is never
  claimed (`docs/SERVICE-GRAPH.md:208`).
- A native edge write into an external session is refused with **409** (`NativeEdge` check inside
  the session-row lock); `POST /v1/sessions/{id}/run` refuses as well.
- The external commit class is symmetric: `external.event` is only accepted for sessions that are
  external and not closed.
- **SSE replays only what Keel recorded.** Backend pushes are projected into `external.event`
  records (payload carries `backend`/`externalRef`/`stateOwner`/`direction`/…), and the ordinary
  events endpoints replay those — no live passthrough of backend frames
  (`docs/SERVICE-GRAPH.md:159`).

## 5. Failure isolation

The service graph owns the lifecycle: effects release in reverse order, generations increment, and a
broken provider **degrades without dragging the runtime down** — reconnects follow
`gateway.RestartPolicy` backoff (`docs/SERVICE-GRAPH.md:152`). Specifically:

- A backend crash or dead connection marks all its non-closed sessions `backend-unavailable`
  (a queryable terminal state, not a hang).
- After reconnect, Keel attempts `session.resume` for known sessions; `resumed=false` keeps them
  unavailable.
- Cancellation that cannot be delivered is an error, not a success — the API maps it to a 5xx
  instead of pretending the turn stopped.
- Closing a stdio provider is: close stdin → grace period → process-group SIGTERM → SIGKILL;
  the smoke test asserts **zero grandchild processes** remain
  (`docs/SERVICE-GRAPH.md:168`).

## 6. Reference implementation and reproducibility

| Artifact | What it is |
|---|---|
| `cmd/keel-dsh-ref` | a minimal reference sidecar implementing the method set; flags simulate slow streams, ignored cancels, crashes, handshake timeouts, version mismatch, env cleaning, grandchild processes (`docs/DSH-REFERENCE.md:41`) |
| `scripts/external-gateway-smoke.sh` | 15-step smoke against a real runtime + real PostgreSQL + the reference sidecar; expects `SMOKE PASS (15 steps)` |
| `docs/examples/external-gateway.json` | the configuration sample above |
| `docs/DSH-REFERENCE.md` | the manual path for a real DSH (`@deepseek-ai/dsh@0.2.1-rc.5`), including the required protocol bridge and a troubleshooting table |

Quick automated check:

```bash
go build -o /tmp/keel-dsh-ref ./cmd/keel-dsh-ref
/tmp/keel-dsh-ref -kind=agent -usage -prompt=ref:      # speak the protocol directly on stdio
scripts/external-gateway-smoke.sh                      # end-to-end, 15 steps, 0 leftover processes
```

## 7. Known gaps (the project's own register)

Copied from the honest gap table in `docs/DSH-REFERENCE.md:228`:

| # | Gap | Current state | Direction |
|---|---|---|---|
| 1 | sidecar CPU/memory budgets are not implemented in Go | `maxSessions` limits session count only; CPU/memory/fd/process limits are the deployment's job (cgroup, ulimit, node isolation) | wire a per-backend budget into the gateway spawn/transport layer (cgroup v2 or rlimit), refuse activation over budget, audit it |
| 2 | **no sweep for leftover sidecars after the runtime is SIGKILLed** | normal reclamation goes through `StdioTransport.Close`; when the runtime is SIGKILLed that path never runs. `gateway.KillTree` and `RedactEnv` exist but have no production call site. The only guard is the protocol contract: parent death ⇒ stdin EOF ⇒ the sidecar exits itself | pidfile + sweep on next start (with pid-reuse validation), or an external supervisor (systemd `KillMode=control-group`, container PID namespace + tini, k8s pod-level reclamation) |

Risk if unaddressed: a sidecar that ignores stdin EOF, calls `setsid`, or detaches grandchildren can
keep holding model credentials and network connections, while a new runtime's `maxSessions`
accounting starts from zero — undercounting live sessions. SIGKILL is a designed input for this
runtime (`cmd/chaos`), not a theoretical scenario, so treat this as an open hardening item when you
deploy sidecars.
