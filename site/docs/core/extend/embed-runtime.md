# Embed the runtime

"Embedding" Keel means running the core inside your own process and binary instead of using
`cmd/runtime` or `cmd/keel` as shipped. This page states what is possible, what the visibility rules
allow, and the exact boot sequence both shipped binaries use.

## 1. The visibility rule (read this first)

Every runtime package lives under `internal/`:

```
internal/kernel     internal/agent      internal/loop      internal/agentcore
internal/store      internal/servicegraph  internal/loadout   internal/servicehost
internal/agentapi   internal/agentext   internal/agentloop  internal/provider
internal/host       internal/httpapi    internal/blob       internal/gateway …
```

Go's `internal/` rule means these packages can only be imported by code inside the module
`github.com/jixoai/keel`. There is **no** published Go module API for third-party import, and the
project makes no versioning promise about internal packages. Practical options:

1. **Work in a checkout (or fork) of this repository** and add your own `cmd/<your-binary>`. This is
   the intended path and what `cmd/runtime` and `cmd/keel` themselves are: two assemblies of the
   same kernel.
2. **Vendor/replace the module** in a private build. The code is Apache-2.0, so this is legal; the
   boundary tests still keep the kernel pure, but you own the upgrade path.
3. **Do not embed the process; talk to it.** If what you need is "my application drives an agent",
   the supported contract is the HTTP face — [AgentAPI](/core/agentapi) plus SSE. That contract *is*
   versioned (`/v1`, additive) and is what the shipped clients use.

Options 1 and 2 are covered below; option 3 is the recommended default for anything that is not the
runtime itself.

## 2. What the two shipped binaries assemble

Both entry points build the same pieces in the same order; `cmd/runtime` is the full assembly,
`cmd/keel serve` is a single-tenant product shell over a subset.

`cmd/runtime` boot sequence (`cmd/runtime/main.go`):

1. `store.Open(ctx, store.Config{...})` — the pgx pool (24 conns by default, `internal/store/store.go:76`).
2. `kernel.NewPG(ctx, st.Pool())` — applies the base schema and kernel migrations
   (`cmd/runtime/main.go:105`, `internal/kernel/pg.go:68`).
3. `kernel.New(kernelLog, kernel.Options{Audit, Limits, OnCommit})` — the ledger object; `OnCommit`
   feeds metrics without blocking the write path (`cmd/runtime/main.go:137`,
   `internal/kernel/kernel.go:591`).
4. `servicehost.Boot(ctx, cfg)` — resolves the default loadout from the tenant document, applies it
   through the service-graph engine, asserts every run-path service is `ACTIVE`, and starts the
   host ticker that collects expired generations (`cmd/runtime/main.go:394`,
   `internal/servicehost/host.go:242`).
5. `httpapi.New(httpapi.Config{...})` — the HTTP/SSE shell reads its projection and write-path
   handles from the **service instances**, not from globals (`cmd/runtime/main.go:515`).

`servicehost.Config` is the embedding surface (`internal/servicehost/host.go:161`):

| Field | Meaning |
|---|---|
| `Seams` | `kernel.Committer`, `kernel.Reader`, `kernel.LeaseManager`, `kernel.ExternalSessions`, `agent.RunMirror`, `*quota.Limits`, logger (`internal/servicehost/host.go:69`) |
| `Tenant` | the tenant configuration document (`internal/host.TenantConfig`) the loadout is resolved from |
| `Options` | loadout snapshot header (`loadout.Options`: id, revision, version, summary, worker switch, run concurrency) |
| `RuntimeID` | the worker owner identity written into every claim (handoff identity) |
| `Prompt` / `MCP` / `LLM` / `External` / `Product` | hosting-process inputs: prompt preamble/sections, loaded mcp.json entries, provider credentials lookup, external sidecar supervision, product assembly hooks |
| `Metrics`, `DrainGate`, `RetireTimeout`, `CollectInterval` | observability, drain coordination, generation retirement |

## 3. Recipe A — same kernel, your own process shell

This is the "new cmd" path. The sketch below mirrors what `cmd/runtime` does; treat it as a
skeleton, not a copy-paste program (real code must handle errors, signals and shutdown).

```go
// cmd/youragent/main.go  (inside this repository)
package main

import (
    "context"
    "log/slog"

    "github.com/jixoai/keel/internal/kernel"
    "github.com/jixoai/keel/internal/quota"
    "github.com/jixoai/keel/internal/servicehost"
    "github.com/jixoai/keel/internal/store"
    "github.com/jixoai/keel/internal/loadout"
)

func main() {
    ctx := context.Background()
    log := slog.Default()

    // 1. the ledger's storage
    st, err := store.Open(ctx, store.Config{DSN: dsn, MaxConns: 24, MinConns: 8})
    if err != nil { panic(err) }
    defer st.Close()

    // 2. the kernel (applies migrations; the only writer of events)
    pgStore, err := kernel.NewPG(ctx, st.Pool())
    if err != nil { panic(err) }
    quotas := quota.New() // implements kernel.Limits
    kern := kernel.New(pgStore, kernel.Options{
        Audit: servicehost.NewAuditSink(log, servicehost.AuditOptions{}),
        Limits: quotas,
    })

    // 3. the service graph: the kernel seams are what services may see
    rt, err := servicehost.Boot(ctx, servicehost.Config{
        Seams: servicehost.Seams{
            Committer: kern, Reader: kern, Leases: kern,
            Runs: yourRunMirror, // agent.RunMirror adapter over the store
            Limits: quotas, Logger: log,
        },
        Tenant:    tenantDoc,     // internal/host.TenantConfig
        Options:   loadout.Options{ID: "default", Revision: 1},
        RuntimeID: "youragent-1",
    })
    if err != nil { panic(err) }   // fails when a run-path service is not ACTIVE

    // 4. your own HTTP face, or none at all
    select {}
}
```

Key points, all enforced by the code rather than by convention:

- `servicehost.Boot` fails if any service on the run path is not `ACTIVE`
  (`internal/servicehost/host.go:343`); a half-assembled graph never starts.
- You never receive the events table. Everything durable goes through `kern.Commit`.
- You can run the assembly **without a worker** (`Options.Worker = false`, or `-no-worker` on
  `cmd/runtime`) to serve API and projection only, and run workers as a separate fleet.

## 4. Recipe B — the ledger only

If you want the durable primitive without the graph, use `internal/store` + `internal/kernel`
directly. The contract you must honour:

- Commit `user.message` under the **edge class** (no fence), everything else under the session's
  **owner** token (claim → epoch → commit → finish/release).
- Carry exactly the idempotency key your kind declares (`requestId`/`generationId`/`toolCallId`).
- Treat the lease as the only mutable coordination: claim with `FOR UPDATE SKIP LOCKED`, renew,
  commit under the same token, finish with the attempt summary, release.

The run-loop semantics you would be re-implementing are specified in
[Architecture](/core/architecture) and `docs/KERNEL.md`; the kernel validates payload shapes at
commit time ([Events and protocol](/core/events) §8), so a hand-rolled writer cannot poison the log
with an undecodable payload.

## 5. Recipe C — a new service in the default assembly

Adding a service (rather than a whole process) is the subject of the next page:
[Write a service descriptor](/core/extend/service-descriptor).

## 6. What embedding does not give you

| Not available | Why | Alternative |
|---|---|---|
| A stable public Go API | runtime packages are `internal/`; there is no semver promise on them | HTTP [AgentAPI](/core/agentapi) with `/v1` + capability discovery |
| In-process hot-swap of services | the assembly is compiled in; hot-plugging old goroutines/listeners makes the 2GB bound unpredictable | [handoff](/core/architecture) |
| Multiple writers to one session | the one-writer-per-session rule is what makes recovery sound | leases and fencing (claim/epoch) |
| Process-level multi-tenancy in `keel serve` | `serve` is a single-tenant shell (`KEEL_SERVE_TENANT`); the multi-tenant runtime is `cmd/runtime` | `cmd/runtime` with per-request `X-Tenant-ID` |
