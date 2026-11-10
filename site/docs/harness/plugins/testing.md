# Plugin testing

> **Planned** requirements for plugin authors and the plugin system itself,
> derived from `openspec/changes/keel-plugin-core/specs/plugin-system/spec.md`.
> The testing *discipline* and every runner named below are **implemented** in
> this repository; they are the convention a plugin suite must follow.

## 1. The rule

> Every scenario in the plugin specification must have a test; **negative
> controls are mandatory**, not optional.

This is the same rule the service graph change imposed on itself: each spec
scenario has a test, and a passing happy path with no asserted failure mode is
not acceptance (`docs/SERVICE-GRAPH.md` §6). For plugins, "it loaded" is the
least interesting fact; "it left no residue when disabled, and it failed
closed when denied" is the interesting one.

## 2. Four test layers

| Layer | Question it answers | Runs as | Existing precedent |
| --- | --- | --- | --- |
| **Unit** | do the pure pieces behave? manifest parsing, version-range gate, cut points | `go test ./internal/…` | `internal/agentext/agentext_test.go` |
| **Integration** | does the plugin behave inside a real host? enable → tool call → hook → trigger → disable | `go test -race` against a real PG | `internal/agentext/e2e_test.go` |
| **Conformance** | does the system keep the contract across implementations/versions? | conformance suite | `internal/loop/conformance_suite_test.go`, `docs/PI-CONFORMANCE.md` |
| **Negative controls** | does each failure mode fail the *right* way? | explicit assertions on errors/state | gateway smoke + fail-closed tests |

### Unit

- Manifest: missing `id`/`version`/`kind`, bad id grammar, bad semver, unknown
  `kind`, `engines.keel` outside range — each must fail with a **field-level
  reason** and leave the plugin directory unadopted (`spec: an illegal manifest is rejected`).
- `provides.hooks` entries outside the closed set must be rejected at
  registration, never silently ignored (`internal/agentext/hooks.go:249`).
- Tool-name conflict detection is order-independent: whichever plugin is
  assembled second fails closed (`internal/agentext/plugin.go:236`, `:454`).

### Integration

The implemented extension E2E is the template: *plugin registration → trigger
fire → hook delivery → sub-agent spawn → cancel of an online attempt*
(`internal/agentext/e2e_test.go`, summarized in `docs/HARNESS.md` §5). A
plugin-suite integration test must additionally prove the disable
path: after disabling a plugin that provided tool `x` and a webhook trigger,
the tool is gone from the model's tool set, the trigger no longer fires, and
the inventory shows `disabled` with no error residue (`spec: disabling revokes immediately`).

### Conformance

- **Loop conformance stays green.** A plugin cannot change the loop contract;
  the PI conformance suite is the gate (`internal/agentloop/conformance_suite_test.go`).
- **Offer the same suite to plugin authors.** An official plugin suite
  (planned, `plugins/COVERAGE.md`) runs the same scenarios against each plugin
  so "W1 implemented" means the same thing for every row of the matrix.
- **Diff gates stay green.** The R1 differential (`cmd/r1diff`) pins the
  client-visible event stream; a plugin that changes default assembly must
  prove the diff is inside the allowlist (`docs/LOADOUT.md` §8).

### Negative controls (required list)

| Control | Injection | Required outcome |
| --- | --- | --- |
| invalid manifest | drop a field / break id grammar | install refused, field-level reason, directory not adopted |
| version gate | `engines.keel` excludes the host | install **and** enable refused, range named |
| missing dependency | enable B while `requires: [A]` is absent | state `disabled`, named reason, nothing half-loaded |
| duplicate tool | two enabled plugins declare the same tool name | second plugin `FAILED`, first untouched |
| sidecar crash | kill the sidecar process mid-run | its tool calls fail clearly, state `failed`, other plugins and the Harness unaffected, re-enable recovers (`spec: sidecar crash isolation`) |
| permission denial | call an undeclared tool / read an undeclared secret | call fails clearly **and** an audit row exists (`spec: over-reach is denied`) |
| approval denied / timed out | answer `deny` (or never answer) | plugin not applied; audit records the refusal; fail-closed on timeout |
| creation-mode failure | make generation/validation fail | no half-installed state, error returned verbatim, temp artifacts cleaned (`spec: a failed generation rolls back`) |
| host SIGKILL with a live sidecar | kill -9 the host | the child exits on stdin EOF (protocol obligation); a sweep gap is a **documented** known limit, asserted as such, not hidden (`docs/DSH-REFERENCE.md` §4) |

The sidecar rows reuse the machinery already exercised by
`scripts/external-gateway-smoke.sh` (15 steps, including a grandchild-process
liveness/reclamation assertion) and `cmd/keel-dsh-ref`'s fault switches
(`-crash-on-send`, `-ignore-cancel`, `-spawn-child`,
`docs/DSH-REFERENCE.md` §2) — the same injection style is expected for plugin
sidecars.

## 3. UI testing

For `webview` plugins (planned):

1. serve the package and assert the page actually loads from `/plugins/<id>/`;
2. assert the page can read its own state and call its own tool through the
   bridge (`spec: plugin UI works`);
3. assert the negative: attempting a non-bridged call (host secret access,
   another plugin's tool) fails;
4. TUI assertion: the plugin row opens a URL, and **no** HTML/WebView renders
   in the TUI (`spec: TUI behavior`).

## 4. Running the suites

```bash
# unit + integration (needs a PostgreSQL for the -race integration packages)
go test ./internal/agentext/… ./internal/servicegraph/… ./internal/loadout/…

# extension E2E and the full gate
scripts/acceptance-all.sh

# external gateway smoke (real runtime + reference sidecar, 15 steps)
scripts/external-gateway-smoke.sh

# client-side projection suite (plugin UI consumes the same projection)
cd packages/chat-surface && bun test

# site-side checks: bilingual parity, citations, dead links
cd site && pnpm build   # build-time link and page checks
```

A plugin package is expected to ship its own `make test` entry point that runs
layers 1, 2 and its own negative controls; conformance and UI checks are run by
the host's acceptance gate, not by the package author alone.
