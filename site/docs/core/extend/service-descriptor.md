# Write a service descriptor

This page is the working recipe from the spec: write a service descriptor and attach it to the
runtime so the service reaches `ACTIVE`. Everything here is verified against the repository's own
blueprint and tests.

## 1. The contract

A service is described by a `ServiceDescriptor` (`internal/servicegraph/descriptor.go:26`) and
constructed by a factory (`internal/servicegraph/lifecycle.go:49`):

```go
type Factory func(ctx context.Context, resolved Resolved) (Instance, error)

type Instance interface {
    Start(ctx context.Context, effects *EffectScope) error
    Stop(ctx context.Context) error
}

type ServiceDescriptor struct {
    Name     string   // unique in the loadout, e.g. "pi-loop"
    Version  string   // implementation identity; bump on change
    Provides []string // capabilities, e.g. "ctx.llm"
    Requires []string // must be served first; missing ⇒ PENDING
    Optional []string // missing ⇒ ACTIVE, degraded diagnostic
    Role     Role     // primary | fallback within a capability chain
    Effects  []string // named cleanups, documentation for operators
    Factory  Factory  // attached by the loadout builder
}
```

Rules that the graph enforces, and that a new service must respect:

- **The factory is pure construction.** Acquire resources only in a way `Stop` can release; do not
  register listeners/timers/registrations there.
- **Effects are registered in `Start`, through the scope.** `EffectScope.Register(kind, release)`
  (`internal/servicegraph/lifecycle.go:119`) collects the cleanups of this instance *in this
  generation*. Release runs in reverse registration order and a failing release does not stop the
  rest. A scope that is already closed returns `ErrScopeClosed` — the caller must release the
  effect itself and abort, otherwise the effect leaks past its generation.
- **Dependents bind through capabilities.** A dependent calls
  `servicegraph.BindAs[YourInterface](resolved, "ctx.your-cap")` and asserts the provider instance to
  a locally declared narrow interface — never to a concrete store or kernel handle.
- **The identity for reassembly is `(Name, Version, Provides, Requires, Optional, Role)`.** If any of
  those changed, the graph tears the old instance down and builds a new one; if only configuration
  changed, the instance is shared across generations. Bump `Version` when the implementation
  changes, or a reassembly will silently keep the stale instance.

## 2. Capability vocabulary

Capabilities come from `internal/loadout/loadout.go:37` (`ctx.store`, `ctx.llm`, `ctx.prompt`,
`ctx.mcp`, `ctx.tools`, `ctx.loop`, `ctx.compaction`, `ctx.session-projection`, `ctx.telemetry`,
`ctx.worker`, `ctx.sandbox`, `ctx.external-agent`, `ctx.external-tools`). If you add a capability,
add the constant next to the others — diagnostics speak these names, and a typo in a string is a
missing dependency, not a compile error.

## 3. The recipe

**Step 1 — declare the descriptor.** Follow the blueprint's style
(`internal/loadout/loadout.go:83`): declaration order is also the assembly order guidance.

```go
var defaultServices = []servicegraph.ServiceDescriptor{
    // … existing entries …
    {
        Name:     "audit-log",
        Version:  Version,                       // loadout.Version ("v1")
        Provides: []string{"ctx.audit"},
        Requires: []string{CapStore},            // needs the store seam first
        Optional: []string{CapTelemetry},        // works without telemetry
        Effects:  []string{"audit-writer"},
    },
}
```

**Step 2 — implement the instance.**

```go
type auditService struct {
    ports  []string          // resolved configuration
    writer *audit.Writer     // built in the factory
}

func newAuditService(cfg AuditOptions) func(ctx context.Context, r servicegraph.Resolved) (servicegraph.Instance, error) {
    return func(ctx context.Context, r servicegraph.Resolved) (servicegraph.Instance, error) {
        // pure construction: resolve dependencies, build objects, release in Stop
        mirror, err := servicegraph.BindAs[agent.RunMirror](r, loadout.CapStore)
        if err != nil { return nil, err }
        return &auditService{ /* … */ }, nil
    }
}

func (s *auditService) Start(ctx context.Context, effects *servicegraph.EffectScope) error {
    // side effects go here, scoped to this generation
    return effects.Register("audit-writer", func() error {
        s.writer.Close()
        return nil
    })
}

func (s *auditService) Stop(ctx context.Context) error { return nil } // effects already released
```

**Step 3 — attach the factory.** The hosting process supplies factories through
`loadout.Config.Provide`; `loadout.Default(cfg)` omits any service whose factory is `nil`
(`internal/loadout/loadout.go:226`). Note the two ways a service can legitimately be absent:

```go
lo, err := loadout.Default(loadout.Config{
    ID:       "default",
    Revision: 2,
    Provide: func(service string) servicegraph.Factory {
        switch service {
        case loadout.ServiceStore: return storeFactory
        case "audit-log":          return newAuditService(auditCfg) // your service
        default:                   return nil                        // declared-but-off
        }
    },
    LoopConformance: []loadout.LoopConformance{{Service: loadout.ServicePiLoop, Manifest: agentloop.Manifest()}},
})
```

**Step 4 — apply it and check the diagnostics.**

```go
eng := servicegraph.NewEngine(servicegraph.WithRetireTimeout(30*time.Second))
snap, err := eng.Apply(ctx, lo)      // internal/servicegraph/engine.go:139
if err != nil { … }                   // startup failure: effects of the failing service rolled back
lease, _ := eng.Acquire()             // a run pins generation + revision (engine.go:270)
inst, ok := eng.Lookup("audit-log")   // inspect what is live (engine.go:367)
```

`Snapshot()` reports each service's state and diagnostics — that is where "PENDING: waiting for
ctx.store" or a degraded optional dependency appears. `servicehost.Boot` turns a non-`ACTIVE`
run-path service into a startup failure (`internal/servicehost/host.go:343`), so a broken descriptor
cannot be silently ignored in production assembly.

**Step 5 — retire paths.** When a reload changes your service, the old generation goes `RETIRING`
(no new runs), and is unloaded in reverse dependency order once its references drain or the retire
timeout expires. `Engine.CollectExpired` must be called periodically — the host ticker does this
(`engine.go:303`, `docs/LOADOUT.md:114`). If your effects can outlive a generation, they will show
up as leaked registrations in diagnostics after the retire deadline.

## 4. Descriptors that are deliberately incomplete

Two shapes in the blueprint are worth copying rather than "fixing":

- **`sandbox`: declared, no factory.** `ServiceSandbox` is in the blueprint with
  `Provides: ["ctx.sandbox"]` and no implementation anywhere. It is an optional dependency of
  `prompt`, so its absence is a degraded diagnostic and nothing blocks
  (`docs/LOADOUT.md:48`). If you implement a sandbox, you add a factory — you do not add a service
  name.
- **`prompt` and `tools`: one object, two capabilities.** `tools` requires `prompt` because the
  registry must be a view of the same host object; two registries would fork on `RefreshMCP`
  (`docs/LOADOUT.md:44`).

## 5. Replacing the loop (the special case)

`ctx.loop` is the one capability with an extra gate. Any service providing `ctx.loop` must declare a
PI conformance manifest whose canonical hash matches its pin; a loop with no declaration, no pin, or
a mismatched manifest is rejected **before the loadout exists**
(`internal/loadout/conformance.go:55`). The default blueprint declares
`agentloop.Manifest()` and pins `loadout.PinnedPiLoopManifestHash`
(`internal/loadout/conformance.go:18`).

```go
// The default loop is fine; here is what a replacement must do:
cases := agentloop.Manifest()             // the suite your loop passed (agentloop/manifest.go:72)
hash := agentloop.ManifestHash()          // canonical hash of those cases (manifest.go:79)
lo, err := loadout.Default(loadout.Config{
    Provide:         provideFactories,     // your loop's factory for loadout.ServicePiLoop
    LoopConformance: []loadout.LoopConformance{{Service: loadout.ServicePiLoop, Manifest: cases}},
    LoopPins:        map[string]string{loadout.ServicePiLoop: hash},
})
```

The gate is a claim-checker, not a proof: it makes "this loop passed the pinned suite" a mandatory,
hash-checked statement. Do not weaken the suite to make a loop pass — the pin is a literal in the
repository (`internal/loadout/conformance.go:18`) and a test fails when the manifest drifts.

## 6. Acceptance for a new service

| Gate | Command |
|---|---|
| Unit / race | `go test -race ./internal/servicegraph ./internal/loadout ./internal/servicehost` |
| Assembly parity | R1 differential: same input, same database, zero difference outside the whitelist (`cmd/r1diff`, `docs/RUNBOOK.md:162`) |
| Full acceptance | `KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence scripts/acceptance-all.sh` |
| If the default assembly changed | the R1 gate is mandatory — it is what proves "the default loadout reproduces the previous behaviour" |

See [Test and accept](/core/extend/testing) for what each gate covers and how long it runs.
