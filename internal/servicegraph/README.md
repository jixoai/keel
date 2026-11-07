# servicegraph

The Keel service graph: statically enumerable service descriptors, immutable
loadout snapshots, dependency resolution, and the lifecycle that activates,
reassembles, and tears down service instances.

This package is deliberately independent of the rest of the runtime (`cmd`,
`httpapi`, `agent`, `store`): hosting processes bind concrete implementations
through `Factory` functions, and services receive narrow interfaces through
their resolved bindings. Integration into `cmd/runtime` is a separate concern.

```go
lo, err := loadout.FromTenantConfig(tenantCfg, provide) // internal/loadout
e := servicegraph.NewEngine(servicegraph.WithRetireTimeout(30*time.Second))
snap, err := e.Apply(ctx, lo)          // assemble; per-service failures are isolated
lease, err := e.Acquire()              // pin this generation for one run
inst, ok := lease.Lookup("pi-loop")    // the run's instance, stable across reassembly
lease.Release(ctx)                     // drop the reference; retiring instances drain
```

## Descriptors

Every replaceable service declares, statically (no runtime reflection):

| Field | Meaning |
|---|---|
| `Name` | unique service instance name (`"pi-loop"`) |
| `Version` | implementation identity (`"v1"`); **bump it whenever the implementation changes** — reassembly decides "same service?" by `(Name, Version, Provides, Requires, Optional, Role)` |
| `Provides` | capability names it supplies (`"ctx.llm"`, `"ctx.tools"`) |
| `Requires` | capabilities that must be served before it loads; a missing one keeps the service `PENDING` |
| `Optional` | capabilities it can run without; a missing one activates it **degraded** and records a diagnostic |
| `Role` | `primary` (default) or `fallback` in the provider chain of each capability it provides |
| `Factory` | builds the live instance; nil fails activation with a diagnostic |
| `Effects` | declared cleanup scope (documentation for operators and diagnostics) |

`Loadout` is an immutable, validated snapshot: `ID`, `Revision`, `Version`,
`Summary`, and the service set. `Revision` is the configuration version a run
pins; reassembly builds a *new* `Loadout` instead of editing one.

Acceptance of the default loadout is **R1 differential** (normalized HTTP
status/headers, event-log dumps, tool sequences; diff whitelist = time,
random IDs, log order): it is not a byte-equality claim.

## Resolution

`Resolve(loadout)` produces a `Graph`: provider chains per capability, the
dependency edge set, a deterministic activation order (Kahn, ties broken by
declaration order), and standby detection. It rejects — before anything
starts, as `*ResolveError` with readable issue codes — the following:

- a capability provided by several services without exactly one `primary`
  (the rest must be declared `fallback`); zero or several primaries is an
  error. A sole provider always serves, whatever role it declares.
- dependency cycles over the required **and** optional edges (an optional
  edge is an ordering and rebinding constraint too), reported as a path
  (`a -> b -> a`).
- structural problems (`empty-name`, `duplicate-service`, `missing-provides`,
  `duplicate-provides/requires/optional`, `requires-and-optional`,
  `self-dependency`, `invalid-role`).

Static questions are answerable without live instances: `Graph.MissingRequires`,
`Graph.MissingOptionals`, `Graph.IsStandby`, `Graph.Chain`,
`Graph.EffectiveProvider`, `Graph.Dependencies`, `Graph.Order`.

## Lifecycle

```
PENDING → LOADING → ACTIVE → RETIRING → UNLOADING → PENDING
                        ↘ FAILED (retried by a later Apply)
```

- `PENDING` is the resting state: never loaded, or waiting on a required
  capability (missing provider, or a provider that is itself PENDING/FAILED).
  A required dependency that is merely PENDING is waited for — the engine does
  not promote a fallback while the primary may still come up. A FAILED
  provider promotes the next fallback in the chain.
- `ACTIVE` instances have registered their effects through their
  `EffectScope`; the scope is the rollback contract. If `Start` fails, the
  engine releases the registered effects in reverse registration order and
  never calls `Stop` (the instance never fully started). `Stop` is called only
  for successfully started instances, after their effects are released.
- `RETIRING` is entered when a reassembly replaces (or removes) an instance
  that in-flight runs still reference through a `Lease`. The instance accepts
  no new runs but keeps serving its lease holders. When the last reference
  drains — or the configured drain deadline (`WithRetireTimeout`) passes — it
  is unloaded **in reverse dependency order** (dependents before providers).
  With zero references the RETIRING step completes immediately, i.e. plain
  reassembly behaves exactly like the pre-retirement engine.
- `FAILED` is per-service and isolated: already-started services stay ACTIVE,
  independents keep running, dependents stay PENDING with a `provider-failed`
  diagnostic, and a later Apply retries the failed service.
- `generation` increments on every accepted `Apply`; each instance records the
  generation it was loaded in, and diagnostics carry the generation they
  belong to. An unchanged instance keeps its generation.
- Standby providers (declared `fallback`, primary present) stay PENDING with a
  `standby` diagnostic; a promoted fallback reports `standby-serving`.

`Apply` returns `(*Snapshot, error)`: an error is either a rejected loadout
(`*ResolveError`, previous generation untouched) or an `*ActivationError`
listing failed services (the rest of the assembly is committed).

### Leases (in-flight run references)

`Engine.Acquire` pins the current generation: every ACTIVE instance gets a
reference and stays resolvable through `Lease.Lookup`, even after a later
reassembly swapped it out. `Lease.LoadoutID`/`Lease.Revision` carry the pinned
configuration identity for run metadata. New runs always pin the newest
generation — `RETIRING` accepts no new runs. A forced drain
(`CollectExpired`, also invoked by `Apply`/`Acquire`, or `Close`) invalidates
the outstanding leases (`Lease.Broken`); the holder must fail its run, and the
events are diagnosable as `drain-deadline-exceeded`.

Leases are an **in-process** mechanism. Cross-process upgrades are the runtime
handoff protocol's job: new runtime, drain, epoch+1, `Last-Event-ID` resume.

## Diagnostics

`Snapshot` carries per-service state, bindings, degraded capabilities,
failure reasons, effects, retiring rows, and structured diagnostics.
`Snapshot.Explain(name)` renders *why* a service is in its state, following
the waiting-on chain (`pi-loop → tools → missing ctx.tools`); `Snapshot.String`
renders a compact overview. Codes: `missing-required`, `provider-pending`,
`provider-failed`, `optional-missing`, `optional-failed`, `no-factory`,
`factory-failed`, `start-failed`, `effect-release-failed`, `stop-failed`,
`standby`, `standby-serving`, `retiring`, `generation-drained`,
`drain-deadline-exceeded`.

Any telemetry built on this engine must use low-cardinality labels (loadout
name, service, state) — **never** `revision`/`generation` as metric labels.

## Boundary with the contract kernel

The service graph is the *assembly* plane; the contract kernel is the *facts*
plane. They meet at narrow interfaces:

- A factory receives only its resolved bindings (`Resolved.Bind` /
  `BindAs[T]`): capability name → provider instance. Dependents assert locally
  declared narrow interfaces; nothing exposes the event store's write paths.
- Services **must not** append events, allocate `seq`, or bypass tenant/lease
  checks. They produce proposals or results; the kernel's `Commit` performs
  validation, idempotency, pairing, and the transaction. `requires ctx.store`
  in a capability list never means "own the store" — the store service exposes
  only the operations its consumers are allowed to use.
- Effects are local resources (listeners, timers, registrations, goroutines);
  they never span the ledger. A service that fails must leave zero residue
  outside its `EffectScope`.
- The engine has no database. Durable state — loadout revision per run,
  owner epoch, handoff audits — is recorded by the runtime/kernel, not here.

## Default loadout (internal/loadout)

`internal/loadout` resolves the production assembly without importing any
concrete implementation:

- `TenantConfig` (agent / skills / MCP servers / hooks / compaction /
  telemetry / tools / sandbox / secretRefs / worker) → `EnabledServices()` →
  descriptors → immutable `Loadout` with `Revision` + derived `Summary`.
  Configuration changes take effect for the next run; a run that already
  started pins the old revision through its lease.
- The blueprint (`DefaultServices()`) mirrors today's `cmd/runtime`
  boot sequence, with capability names shared by all diagnostics:

| service | provides | requires | optional | effects |
|---|---|---|---|---|
| `store` | `ctx.store` | — | — | pg-pool |
| `llm` | `ctx.llm` | — | — | provider-client |
| `prompt` | `ctx.prompt` | — | — | prompt-assembly |
| `mcp` | `ctx.mcp` | — | — | mcp-connections |
| `sandbox` | `ctx.sandbox` | — | — | sandbox-process |
| `tools` | `ctx.tools` | — | `ctx.mcp`, `ctx.sandbox` | tool-registry |
| `pi-loop` | `ctx.loop` | `ctx.llm`, `ctx.tools` | `ctx.compaction` | loop-stream |
| `compaction` | `ctx.compaction` | `ctx.llm` | `ctx.store` | compaction-worker |
| `session-projection` | `ctx.session-projection` | `ctx.store` | — | projection-cache |
| `telemetry` | `ctx.telemetry` | — | — | exporter |
| `worker` | `ctx.worker` | `ctx.store`, `ctx.loop`, `ctx.prompt`, `ctx.tools` | `ctx.telemetry` | worker-pool |

The hosting process supplies factories via `Provide`; a missing factory omits
the service and its consumers report missing/degraded dependencies instead of
running with a nil handle. Today's boot (store, llm, prompt, tools, pi-loop,
worker — no MCP config, no sandbox/telemetry/compaction) therefore assembles
as ACTIVE with explicit `optional-missing` diagnostics for the switched-off
capabilities.

`Default(Config{...})` remains the escape hatch for custom service sets
(e.g. a replacement loop, which must pass the PI conformance gate before it
may enter a production loadout).

## Test map

- `resolve_test.go` — validation, provider chains, primary/fallback, cycles,
  topo order, static missing/degraded analysis.
- `engine_test.go` — activation order, PENDING chains, degraded optionals,
  standby + fallback promotion, startup rollback isolation, provider
  replacement order, role flips, generation/effect-leak, close order.
- `retire_test.go` — RETIRING with leases, drain on last release, shared
  instances across generations, drain deadline force-unload, close.
- `race_test.go` — concurrent Apply/Snapshot/Explain, failing generations,
  effect-scope register/release races.
- `internal/loadout/loadout_test.go` — blueprint coherence, all/minimal
  assemblies, TenantConfig selection and summary, missing-store PENDING.
