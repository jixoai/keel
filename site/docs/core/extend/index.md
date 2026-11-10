# Extension map

Keel has exactly two extension philosophies, and which one applies depends on **what kind of thing
you are changing**:

1. **Protocol / primitive changes** (a new execution primitive, a new transport, a new event kind,
   a new loop implementation) are in-repo, spec-first work. They touch the kernel or a protocol
   package and must go through the repository's OpenSpec process.
2. **Assembly / policy changes** (which provider, which tools, which services, which external
   backend) are configuration and service-graph work. They do not change the ledger.

Before choosing a seam, know the visibility rule: **every runtime package lives under `internal/`**,
so they can only be imported by code inside this module. "Embedding the runtime" therefore means
working in a checkout of this repository (or a fork) — see
[Embed the runtime](/core/extend/embed-runtime).

## Decision table

| I want to… | Seam | Page |
|---|---|---|
| Run the core inside my own process / binary | `internal/servicehost.Boot` + the kernel seams, or a new `cmd/` in a fork | [Embed the runtime](/core/extend/embed-runtime) |
| Add or replace a service (provider, projection, compaction, telemetry…) | `servicegraph.ServiceDescriptor` + `loadout.Config.Provide` | [Write a service descriptor](/core/extend/service-descriptor) |
| Replace the agent loop | the `agentloop.Loop` contract + the pinned PI conformance manifest (fail-closed) | [Write a service descriptor](/core/extend/service-descriptor) |
| Talk to an external agent runtime (a sidecar that owns its own sessions) | `AgentBackend` over the external gateway (`external.backends`) | [Connect an external backend](/core/extend/external-backend) |
| Expose an external tool server | `ToolProvider` (`external.tools`) | [Connect an external backend](/core/extend/external-backend) |
| Add a model provider or change protocols | `provider.StreamFn` + `provider.ResolveProvider` | [Write a provider adapter](/core/extend/provider-adapter) |
| Add a tool to the model's toolbox | `agenttool.ToolRegistry` (kernel side) or the host registry (`tools.allow` filters it) | [Embed the runtime](/core/extend/embed-runtime) |
| Add a product capability (plugin, hook, trigger, sub-agent, approval) | the Harness extension surface (`internal/agentext` today; plugin standard documented in the Harness line) | [Harness plugins](/harness/plugins/) |
| Add a new event kind | in-repo change: `kindRegistry` + `internal/store` constants + transcript/SSE/R1 rules move together | [Events and protocol](/core/events) |
| Change storage or add a table | in-repo change: `internal/store/schema.sql` or a numbered kernel migration | [Storage and runtime](/core/storage) |
| Build a UI | the Harness API + SSE (versioned `/v1`, capability discovery) | [AgentAPI reference](/core/agentapi) |

## The two hard rules

**A service may be replaced; a service may not change the ledger.** Services receive narrow seams
(`kernel.Committer`, `kernel.Reader`, `kernel.LeaseManager`, `kernel.ExternalSessions`,
`internal/kernel/seams.go`) and commit proposals; the kernel decides what becomes a fact. If your
change needs to write `events`, allocate `seq`, reorder commits, or bypass tenant/lease checks, you
are changing the kernel — which means a spec, a migration number and the R1 parity gate, not a
plugin.

**The conformance gate is fail-closed.** Any service that provides `ctx.loop` must present a PI
conformance manifest whose canonical hash equals its pin; a missing declaration is a rejection, not
a pass. If you replace the loop, run the suite and declare it deliberately
(`internal/loadout/conformance.go:55`, `internal/loadout/conformance.go:18`).

## Where the work lands

| Change | Files you touch | Gate you must run |
|---|---|---|
| New service descriptor | `internal/loadout/*`, your factory, tests | `go test -race ./internal/loadout ./internal/servicegraph ./internal/servicehost`; then `scripts/acceptance-all.sh` if the default assembly changed |
| New loop | `internal/agentloop` implementation + declaration | PI conformance suite + R1 diff + the loop conformance gate |
| External backend | tenant document (`external.backends`) + a provider process | `go test -race ./internal/gateway ./internal/externalgw` + `scripts/external-gateway-smoke.sh` |
| Provider adapter | `internal/provider` + registry entry | provider stream tests; real-model E2E if credentials allow |
| New event kind | `kindRegistry`, store constants, transcript/SSE/R1 rules | R1 diff (whitelist outside zero difference) + full `acceptance-all.sh` |

The gates themselves are documented in [Test and accept](/core/extend/testing).
