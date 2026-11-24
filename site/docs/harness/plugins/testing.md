# Plugin testing

> **Implemented.** The rule, the four layers and the negative-control list below
> are enforced by the suites that ship: the manager, permissions, sidecar,
> extension, creation and client-slot tests all exist under
> `internal/harnessext/*_test.go`, `internal/harnessapi/*_test.go` and
> `web/src/plugins/`. The coverage matrix is `plugins/COVERAGE.md`, its
> machine-readable source is `plugins/keel.plugin-market/catalog.json`, and the
> gate is `node scripts/plugin-catalog.mjs --check` (wired into
> `.github/workflows/ci.yml` and `scripts/acceptance-all.sh`).

## 1. The rule

> Every scenario in the plugin specification has a test; **negative controls are
> mandatory**, not optional.

This is the same rule the service graph change imposed on itself: each spec
scenario has a test, and a passing happy path with no asserted failure mode is
not acceptance (`docs/SERVICE-GRAPH.md` §6). For plugins, "it loaded" is the
least interesting fact; "it left no residue when disabled, and it failed
closed when denied" is the interesting one.

## 2. Four test layers

| Layer | Question it answers | Runs as | Where it lives |
| --- | --- | --- | --- |
| **Unit** | do the pure pieces behave? manifest parsing, version-range gate, permission policy | `go test ./internal/…` | `internal/pluginpkg/*_test.go`, `internal/harnessext/plugin_manager_test.go`, `plugin_permissions_test.go` |
| **Integration** | does the plugin behave inside a real host? install → enable → tool call → hook → trigger → disable | `go test -race` against a real PG | `internal/harnessext/plugin_manager_test.go`, `e2e_test.go`, `approve_plugin_test.go`, `plugin_sidecar_test.go` |
| **Conformance** | does the system keep the contract across implementations/versions? | conformance suite | `internal/agentloop/conformance_suite_test.go`, `docs/PI-CONFORMANCE.md` |
| **Negative controls** | does each failure mode fail the *right* way? | explicit assertions on errors/state | the plugin suite above + `scripts/external-gateway-smoke.sh` |

### Unit

- Manifest: missing `id`/`version`/`kind`, bad id grammar, bad semver, unknown
  `kind`, `engines.keel` outside range — each fails with a **field-level
  reason** and leaves the plugin directory unadopted
  (`internal/pluginpkg/manifest_test.go`, validator at
  `internal/pluginpkg/manifest.go:653`).
- `provides.hooks` entries outside the closed set are rejected at registration,
  never silently ignored (`internal/harnessext/hooks.go:249`).
- Tool-name conflict detection is order-independent: whichever plugin is
  assembled second fails closed (`internal/harnessext/plugin.go:236`, `:454`).
- Permission policy: an undeclared tool/host/secret is a
  `*PermissionDeniedError` and one audit row
  (`internal/harnessext/plugin_permissions_test.go`).
- Extension declarations: an invalid slot/contribution is a field-level error,
  and a valid one is published on the inventory
  (`internal/harnessext/plugin_extension_test.go`).

### Integration

The implemented extension E2E is the template: *plugin registration → trigger
fire → hook delivery → sub-agent spawn → cancel of an online attempt*
(`internal/harnessext/e2e_test.go`, summarized in `docs/HARNESS.md` §5). The
plugin manager suite additionally proves the disable path: after disabling a
plugin that provided tool `x` and a webhook trigger, the tool is gone from the
model's tool set, the trigger no longer fires, and the inventory shows
`disabled` with no error residue (`internal/harnessext/plugin_manager_test.go`).

The install path is tested through its real gate: `202` → answer → retry with
`decisionId` → `201`, and a refusal → `409`
(`internal/harnessapi/plugin_approval_test.go`).

### Conformance

- **Loop conformance stays green.** A plugin cannot change the loop contract;
  the PI conformance suite is the gate
  (`internal/agentloop/conformance_suite_test.go`).
- **The official suite is the matrix.** `plugins/COVERAGE.md` tracks every
  capability group and bundle against DSH with a closed status vocabulary
  (`delivered`, `partial`, `planned`, `registered`, `merged`, `absent`) and a
  closed wave set (`W0.5`, `W1`, `W2`, `W3`, `never`), so "W1 implemented"
  means the same thing for every row
  (`node scripts/plugin-catalog.mjs --check`).
- **Diff gates stay green.** The R1 differential (`cmd/r1diff`) pins the
  client-visible event stream; a plugin that changes default assembly must
  prove the diff is inside the allowlist (`docs/LOADOUT.md` §8).

### Negative controls (required list)

| Control | Injection | Required outcome | Asserted in |
| --- | --- | --- | --- |
| invalid manifest | drop a field / break id grammar | install refused, field-level reason, directory not adopted | `internal/pluginpkg/manifest_test.go` |
| version gate | `engines.keel` excludes the host | install **and** enable refused, range named | `internal/harnessext/plugin_manager_test.go` |
| missing dependency | enable B while `requires: [A]` is absent | state `disabled` (or `PENDING` inside an assembly), named reason, nothing half-loaded | `internal/harnessext/plugin_manager_test.go` |
| duplicate tool | two enabled plugins declare the same tool name | second plugin `FAILED`, first untouched | `internal/harnessext/plugin_manager_test.go` |
| sidecar crash | kill the sidecar process mid-run | its tool calls fail clearly, state `failed`, other plugins and the Harness unaffected, re-enable recovers | `internal/harnessext/plugin_sidecar_test.go` |
| permission denial | call an undeclared tool / read an undeclared secret | call fails clearly **and** an audit row exists | `internal/harnessext/plugin_permissions_test.go` |
| approval denied / timed out | answer `deny` (or never answer) | plugin not applied; audit records the refusal; fail-closed on timeout | `internal/harnessapi/plugin_approval_test.go` |
| creation-mode failure | make generation/validation fail | no half-installed state, error returned verbatim, temp artifacts cleaned | `internal/harnessext/creation_test.go` |
| host SIGKILL with a live sidecar | kill -9 the host | the child exits on stdin EOF (protocol obligation); the sweep gap is a **documented** known limit, asserted as such, not hidden | `docs/DSH-REFERENCE.md` §4 |

The sidecar rows reuse the machinery already exercised by
`scripts/external-gateway-smoke.sh` (15 steps, including a grandchild-process
liveness/reclamation assertion) and `cmd/keel-dsh-ref`'s fault switches
(`-crash-on-send`, `-ignore-cancel`, `-spawn-child`,
`docs/DSH-REFERENCE.md` §2) — the same injection style is used for plugin
sidecars.

## 3. UI testing

For `webview` plugins:

1. the package is served and the page loads from `/plugins/<id>/`, only for an
   enabled plugin (`cmd/keel/plugins_ui.go:17`);
2. the page reads its own state and calls its own tool through the bridge
   (`internal/harnessapi/plugin_bridge_test.go`);
3. the negatives hold: a non-bridged call, another plugin's tool, or a verb the
   contributions did not declare is refused with a typed error
   (`internal/harnessapi/plugin_bridge_test.go`,
   `plugin_bridge_capability_test.go`);
4. TUI assertion: the plugin row opens a URL, and **no** HTML/WebView renders in
   the TUI (`web/src/screens/plugins.tsx:13`).

## 4. Running the suites

```bash
# unit + integration (needs a PostgreSQL for the -race integration packages)
go test ./internal/pluginpkg/… ./internal/harnessext/… ./internal/harnessapi/…

# the plugin catalog gate (schema, references, COVERAGE §H freshness)
node scripts/plugin-catalog.mjs --check

# extension E2E and the full gate
scripts/acceptance-all.sh

# external gateway smoke (real runtime + reference sidecar, 15 steps)
scripts/external-gateway-smoke.sh

# client-side projection suite (plugin UI consumes the same projection)
cd packages/chat-surface && bun test

# site-side checks: bilingual parity, citations, dead links
cd site && pnpm build
```

A plugin package is expected to ship its own `make test` entry point that runs
layers 1, 2 and its own negative controls; conformance and UI checks are run by
the host's acceptance gate, not by the package author alone.
