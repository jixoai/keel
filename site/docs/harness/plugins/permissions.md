# Plugin permissions and audit

> **Planned** enforcement (allowlists, approvals, audit) per
> `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md` ("权限与沙箱",
> "审计"). The credential-reference discipline and the process/environment
> isolation it builds on are **implemented** in `internal/gateway`,
> `internal/host` and `internal/servicehost`; those are cited inline.

## 1. The honest baseline first

keel does **not** sandbox plugin code, and it says so plainly — the same
position as the DSH model this design references:

- an **in-process** plugin is compiled into the host and runs with host
  privileges; the `permissions` block is a policy contract, enforced at the
  seams the host controls (tool dispatch, secret resolution, process spawning)
  and recorded in the audit — it is not a memory sandbox;
- a **sidecar** plugin runs in its own process; that process boundary plus the
  environment whitelist *is* the isolation;
- installing a plugin means executing third-party code. Therefore
  **install/enable/upgrade require an explicit durable approval** — the whole
  point of the permission model is that "it can run" is a decision, not a
  default.

## 2. What the manifest declares

```yaml
permissions:
  fs: [workspace-write]        # file effect classes
  net: [api.example.com, …]    # allowed hosts
  secrets: [OPENAI_API_KEY, …] # allowed secret references (names only, never values)
  tools: [example_status, …]   # allowed tool calls
```

Enforcement points (planned), each with the failure mode the spec requires:

| Grant | Enforced at | On violation |
| --- | --- | --- |
| `tools` | tool dispatch: the caller's plugin id is checked against its allowlist before execution | call fails with a clear error + audit `(plugin, action, target, result)` |
| `net` | the outbound seams the host controls (provider calls today; declared host set for future direct egress) | refused + audit |
| `secrets` | secret reference resolution: only declared names may resolve | refused + audit; values are never logged |
| `fs` | file-effect class checks around workspace access | refused + audit |
| sidecar env | process construction: `MinimalEnv` + declared names only | names outside the list are absent from the child |

The last row is already implemented for the external gateway and applies
unchanged to sidecars: a spawned process gets `MinimalEnv` plus explicitly
declared variables, with references resolved from `${NAME}` / `env:NAME` /
`secret:NAME` and **literal secrets refused at validation time**
(`internal/gateway/process.go:106`, `internal/servicehost/models.go:546`,
`internal/host/config.go:203`). The same discipline protects model credentials
today: `secret:` references fail closed when the tenant secret store is
unavailable — nothing is ever stored or served in plaintext
(`cmd/keel/serve.go:165`).

## 3. Approval: the decision primitive does the asking

Install/enable/upgrade are durable, answerable questions; the Harness already
has the primitive for exactly this shape ([Approvals and
questions](/harness/approvals-and-questions)):

```
install request ──▶ decision (kind=approval, options=[allow, deny])
                      │
                      ├─ answered allow  ──▶ manifest + gate + hash validated → installed → audit
                      ├─ answered deny   ──▶ not applied; audit records the refusal
                      └─ timed out / no answer ──▶ not applied (fail-closed)
```

Rules the spec makes explicit (`spec: 权限与沙箱`): approval gates
install/enable/upgrade; a denial means the plugin does not take effect; and the
approval itself is a durable row with an answer journal (never a transient
dialog). Timeouts fail closed.

## 4. Audit

Every lifecycle action (`install`/`enable`/`disable`/`uninstall`/`update`) and
every permission refusal records **time, operator, plugin id, version, action,
result**, and is queryable through `GET /v1/plugins/audit`
(`spec: 审计`). The current codebase already demonstrates the pattern at two
levels:

- the trigger audit keeps per-fire history and the last-N audit window
  (`internal/agentext/triggers.go:86`, `:135`);
- the kernel audit sink projects every rejection into structured log fields
  (reason/op/tenant/session/kind/owner/epoch) with rate limiting
  (`internal/servicehost/audit.go`, `docs/LOADOUT.md` §6).

The plugin audit reuses this style: bounded records, identifiers only, no
payloads or secrets.

## 5. Residual risks, stated

These are known and deliberately not hidden (`spec` + `docs/DSH-REFERENCE.md`
§4):

| Risk | Why it exists | Mitigation path |
| --- | --- | --- |
| In-process plugin code can misbehave beyond its declared permissions | no memory sandbox | trusted-by-construction for official plugins; third-party logic belongs in a sidecar |
| A sidecar that ignores stdin EOF can outlive a killed host | normal reclamation runs only on the close path | protocol obligation (child exits on EOF) + planned pidfile sweep / external supervisor |
| No per-plugin CPU/memory budget yet | the budget is a deployment concern today | planned per-backend budget at spawn (cgroup/rlimit) with activation refused on overrun |
| Secret references are names, not scopes | a granted name is a granted value | declare the narrowest names; values never enter logs or the ledger |

## 6. What a reviewer should be able to check

For any enabled plugin, the following must be answerable from the API surface
alone:

1. what it declares (manifest) — [Manifest](/harness/plugins/manifest);
2. what it is allowed to touch (permissions) — this page;
3. what it actually did (audit) — §4;
4. who approved it and when (decision row) — §3;
5. what it provides right now (snapshot: tools/hooks/triggers/diagnostics) —
   `GET /v1/plugins` (`internal/agentapi/ext.go:18`).
