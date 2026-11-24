# Plugin permissions and audit

> **Implemented**, with a stated boundary. Two grants are enforced live today:
> `permissions.tools` at the tool dispatch point, and the sidecar environment
> allowlist at process construction. Every refusal is audited. `permissions.net`
> and `permissions.secrets` have a compiled policy, a gate and an audit path
> (`PermissionedBroker.Fetch` / `ReadSecret`), but **this build wires no
> host-mediated fetch or secret-read verb through them**, so nothing is refused
> by them yet. `permissions.fs` has no gate at all — it is declared and published
> only (`internal/harnessext/plugin_permissions.go:19`). This page keeps those
> distinctions explicit instead of describing the whole block as enforced.

## 1. The honest baseline first

keel does **not** sandbox plugin code, and it says so plainly — the same
position as the DSH model this design references:

- an **in-process** plugin is compiled into the host and runs with host
  privileges; the `permissions` block is a policy contract, enforced at the
  seams the host controls (tool dispatch and process spawning today;
  host-mediated fetch and secret resolution are built gates awaiting a routed
  verb) and recorded in the audit — it is not a memory sandbox;
- a **sidecar** plugin runs in its own process; that process boundary plus the
  environment whitelist *is* the isolation;
- installing a plugin means executing third-party code. Therefore
  **install/enable/upgrade require an explicit durable approval** — the whole
  point of the permission model is that "it can run" is a decision, not a
  default.

## 2. What the manifest declares

```yaml
permissions:
  fs: [workspace-write]        # file effect classes (declaration only, see §3)
  net: [api.example.com, …]    # allowed hosts
  secrets: [OPENAI_API_KEY, …] # allowed secret references (names only, never values)
  tools: [example_status, …]   # allowed tool calls
```

Enforcement points and their real status in this build:

| Grant | Enforced at | Status |
| --- | --- | --- |
| `tools` | the **dispatch point**: a call made on a plugin's behalf may name a tool only if the plugin provides it or `permissions.tools` declares it (checked at `Manager.InvokePluginTool`, not at registration) — the plugin UI bridge and creation mode both come through here | **live**: call fails with a definite error + audit row `(plugin, version, kind=tool, target, reason)` |
| `net` | the policy and gate exist (`PermissionedBroker.Fetch`: exact host, `*.suffix`, optional `:port`, `http`/`https` only), but no shipped host-mediated fetch verb routes through them | **gate ready, nothing routed**: a fetch that does not go through the broker is not checked |
| `secrets` | the policy and gate exist (`PermissionedBroker.ReadSecret`: only declared names resolve), but no shipped host-mediated secret read routes through them | **gate ready, nothing routed**; values are never logged when the gate is used |
| `fs` | **no gate at all** — the declaration is parsed, validated and published, nothing mediates file access | **declaration only** |
| sidecar env | process construction: the host's safe defaults plus exactly the declared **names**, values always from the host | **live**: names outside the list are absent from the child |

The sidecar row is implemented by the loader
(`internal/harnessext/plugin_sidecar.go:22`), and the env discipline it reuses
comes from the external gateway: a spawned process gets `MinimalEnv` plus
explicitly declared variables, with references resolved from `${NAME}` /
`env:NAME` / `secret:NAME` and **literal secrets refused at validation time**
(`internal/gateway/process.go:106`, `internal/servicehost/models.go:546`,
`internal/host/config.go:203`). The same discipline protects model credentials
today: `secret:` references fail closed when the tenant secret store is
unavailable — nothing is ever stored or served in plaintext
(`cmd/keel/serve.go:165`).

### The boundary, stated once

An in-process plugin runs in the host process; a sidecar is a host-controlled
process. **Neither is sandboxed.** What is enforced is every capability request
that actually crosses the host's own boundary on a path the host owns: tool
dispatch and the child environment. The fetch and secret gates are built and
audited but have no shipped verb in front of them, so treat them as the shape a
future mediated capability will take, not as a live filter.

Two consequences follow, and both are deliberate:

1. a sidecar that opens **its own** socket is not stopped by `permissions.net`
   — the declared host list governs what the *host* will fetch on its behalf;
   the rule a plugin author follows is "ask the host through the broker", and
   the loader's env allowlist keeps host credentials out of the process
   regardless;
2. an in-process plugin can misbehave beyond its declared budget. Official
   in-process code is trusted by construction; third-party logic belongs in a
   sidecar.

## 3. Approval: the decision primitive does the asking

Install/enable/upgrade are durable, answerable questions; the Harness already
has the primitive for exactly this shape ([Approvals and
questions](/harness/approvals-and-questions)):

```
POST /v1/plugins/install {"path": ...}                        → 202 {"approvalRequired": true, "decision": …}
POST /v1/plugin-approvals/{decisionId} {"optionId": "allow"}   (the human answers)
POST /v1/plugins/install {"path": ..., "decisionId": ...}      → 201 (or 409 refused)

                      ├─ allow   ──▶ manifest + gate + hash validated → installed → audit
                      ├─ deny    ──▶ not applied; audit records the refusal
                      └─ timeout / no answer ──▶ not applied (fail-closed)
```

Rules, each one visible in the code:

- the decision id is derived from `(tenant, action, requestId-or-target)`, so a
  question opened for one action or one package cannot authorize another
  (`internal/harnessapi/plugin_approval.go:412`);
- a refusal (deny, or a deadline that passed) never reaches the manager and is
  recorded in the audit with result `refused`;
- the approval itself is a durable row with an answer journal — never a
  transient dialog; timeouts fail closed.

## 4. Audit

Every lifecycle action (`install`/`enable`/`disable`/`uninstall`/`update`) and
every permission refusal records **time, operator, plugin id, version, action,
result**, and is queryable through `GET /v1/plugins/audit`
(`internal/harnessapi/plugins.go:256`). A refusal is a
`*PermissionDeniedError` (`errors.Is ErrPermissionDenied`) and writes one row
with action `deny`, result `refused` and detail `{kind,target,reason}` — the
same table the lifecycle actions use (`internal/store/schema.sql:603`).

The plugin audit reuses the style the codebase already applies elsewhere:
bounded records, identifiers only, no payloads or secrets. The trigger audit
keeps per-fire history and the last-N window
(`internal/harnessext/triggers.go:86`, `:135`), and the kernel audit sink
projects every rejection into structured log fields with rate limiting
(`internal/servicehost/audit.go`, `docs/LOADOUT.md` §6).

## 5. Residual risks, stated

These are known and deliberately not hidden (`docs/DSH-REFERENCE.md` §4):

| Risk | Why it exists | Mitigation path |
| --- | --- | --- |
| In-process plugin code can misbehave beyond its declared permissions | no memory sandbox | trusted-by-construction for official plugins; third-party logic belongs in a sidecar |
| A sidecar's own outbound network calls are not filtered | `permissions.net` governs host-mediated fetches; the child is a normal process | env allowlist keeps credentials out; routing a fetch through `PermissionedBroker.Fetch` is where the check would land |
| `permissions.net` / `permissions.secrets` are not refused live | the gates exist but no shipped host verb routes through them | wire the mediated verbs; until then the declaration states intent, and the audit can only see what is routed |
| A sidecar that ignores stdin EOF can outlive a killed host | normal reclamation runs only on the close path; there is no pidfile sweep | protocol obligation (child exits on EOF) + a planned pidfile sweep / external supervisor |
| No per-plugin CPU/memory budget yet | the budget is a deployment concern today | planned per-backend budget at spawn (cgroup/rlimit) with activation refused on overrun |
| `permissions.fs` is declaration-only | keel mediates no file access for plugins yet | a future mediated file API is where the check lands; until then the declaration states intent |
| Secret references are names, not scopes | a granted name is a granted value | declare the narrowest names; values never enter logs or the ledger |

## 6. What a reviewer should be able to check

For any enabled plugin, the following must be answerable from the API surface
alone:

1. what it declares (manifest) — [Manifest](/harness/plugins/manifest);
2. what it is allowed to touch (permissions) — this page;
3. what it actually did (audit) — §4;
4. who approved it and when (decision row) — §3;
5. what it provides right now (snapshot: tools/hooks/triggers/diagnostics) —
   `GET /v1/plugins` (`internal/harnessapi/plugins.go:56`).
