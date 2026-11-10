# What Keel is

Keel is a **general-purpose, stateless, multi-tenant agent runtime** written in Go. This page is
the entry point to the **Core** main line: what the core is responsible for, what it refuses to do,
how to run it, and where the rest of the documentation leads.

The product layer on top of the core is called the **Harness**; it has its own main line starting
at [What the Harness is](/harness/). Protocol names — `AgentAPI`, `AgentBackend` — keep their
original spelling and are aliased to the Harness (see the [glossary](/glossary)).

## The problem it solves

You want to run many long-lived sessions at once — each possibly carrying images, tool calls and
long contexts — while keeping three properties that naive designs lose:

1. **Memory must not grow with the session count.** Process memory is bounded by configuration
   (a 2GB budget), not by how many sessions exist.
2. **A restart must not lose sessions.** `kill -9` the process and every session continues from
   where it was, without duplicating the visible final reply.
3. **Backpressure must be real.** Network bandwidth is the first explicit ceiling; slow clients can
   be cancelled and can resume.

The design targets that follow from this are recorded in `README.md` and `docs/ARCHITECTURE.md:6`:

| Dimension | Target |
|---|---|
| Process memory ceiling | **2GB** (Go RSS, all tenants) |
| Tenants | 100–200 (logical isolation, not 200 processes) |
| Concurrency per tenant | 5–10 sessions |
| Total sessions | **1000 persistent sessions**, each possibly with images |
| Streaming | image: storage → base64 → HTTP, fully streamed; ≤64KB buffer per stream |
| Statelessness | `kill -9`, restart, sessions continue (SSE `Last-Event-ID` / offset resume) |

## The one rule that shapes everything

> **The ledger cannot be swapped; the workbench can. Swapping the workbench is done by a shift
> change (handoff), not by hot-plugging.**

- **The ledger** is the contract kernel: event order, idempotency, tool-call pairing, lease and
  ownership, tenant boundaries, crash-recovery truth.
- **The workbench** is the service graph: agent loop, prompt, tools, LLM provider, session
  projection, compaction, MCP, telemetry, sandbox.
- **The shift change** is runtime handoff: a new runtime starts with a new loadout, the old one
  drains and releases its leases, the new one continues from the ledger.

This is why Keel is not "everything is a plugin": services commit *proposals* to the kernel, and
only the kernel decides what becomes a fact. The full statement of the rule, the layer diagram and
the replaceable / non-replaceable table are in [Architecture](/core/architecture).

## What the core does not do

The kernel boundary is fixed by tests, not by convention: `internal/architecture/boundary_test.go:14`
asserts with `go list -deps` that kernel packages (loop, agentcore, agenttool, kernel, agent, store)
never import product packages (agentapi, agentext, servicehost, cmd/keel). Not in the kernel:

- authentication (a network middleware concern),
- session listing, stop and approval semantics,
- plugin lifecycle, trigger scheduling, sub-agent orchestration,
- token-level partials,
- any UI.

Product capabilities live above the kernel and are assembled by `keel serve` or by the service
graph — see [Extension map](/core/extend/) for how to add them.

## Quickstart

Prerequisites: Docker (for PostgreSQL 18) and Go 1.27.3 (`go.mod:3`).

```bash
# 1. the ledger's database
docker run -d --name keel-pg \
  -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 postgres:18

# 2. build the Harness and start it (fake provider when no model is configured)
git clone https://github.com/jixoai/keel.git && cd keel
go build -o keel ./cmd/keel
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
./keel serve --addr 127.0.0.1:13750 \
  --tenant 11111111-2222-3333-4444-555555555555 \
  --password devpass
```

```bash
# 3. create a session, submit input, read the committed event log
T=http://127.0.0.1:13750
TENANT=11111111-2222-3333-4444-555555555555
SID=$(curl -s -X POST -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  -H 'Content-Type: application/json' -d '{}' $T/v1/sessions | cut -d'"' -f4)

curl -s -X POST -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  -H 'Content-Type: application/json' -d '{"text":"hello keel"}' \
  $T/v1/sessions/$SID/input
curl -s -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  "$T/v1/sessions/$SID/events"
```

Three responses you will actually get (verified against this revision):

```json
// POST /v1/sessions
{"sessionId":"586897e3-cec9-40d7-a5b0-3a9344773000"}

// POST /v1/sessions/{id}/input
{"created":true,"requestId":"9e5088a0-892c-4af3-b123-43c8a6c7fcf4","seq":1,"status":"committed","submissionId":"05c9689a-f556-45cf-8996-9b08c56144e0"}

// GET /v1/sessions/{id}/events
{"events":[
  {"seq":1,"kind":"user.message","payload":{"text":"hello keel","requestId":"…"}},
  {"seq":2,"kind":"assistant.message","payload":{"role":"assistant","content":[{"text":"fake provider received: hello keel","type":"text"}],"inputSeqs":[1],"stopReason":"end_turn"}},
  {"seq":3,"kind":"run.finished","payload":{"status":"done","turns":1,"provider":"fake","model":"fake","stopReason":"end_turn","durationMs":24}}}]
```

The ledger path is exercised end to end even with the fake provider: the input is committed as an
edge event, the worker claims the run, the loop answers, and the attempt summary is written before
settling. Swap in a real provider with `--model`/`--base-url`/`--api-key` (or `KEEL_MODEL` and
friends) when you want real answers.

To run the plain runtime (Core only — no product shell) instead:

```bash
go build -o keel-runtime ./cmd/runtime
KEEL_DSN="$KEEL_DSN" ./keel-runtime -addr 127.0.0.1:13740 -cas /tmp/keel-cas
```

`cmd/runtime` and `cmd/keel` are different assemblies of the same kernel; see
[Embed the runtime](/core/extend/embed-runtime) for the boot sequence of each.

## Reading map

The Core main line is five pages of theory plus six pages of practice:

| Page | Question it answers |
|---|---|
| [Architecture](/core/architecture) | What are the ledger, the kernel, the service graph, loadout, handoff and backpressure, and where is the line between them? |
| [Events and protocol](/core/events) | What are the event kinds, how is a commit classified, how is idempotency enforced, what does SSE guarantee? |
| [AgentAPI reference](/core/agentapi) | Which HTTP endpoints exist on each face, with what request/response shapes and error codes? |
| [Storage and runtime](/core/storage) | How is data stored (PostgreSQL, chunks, CAS), how are requests pooled, and what is the memory budget? |
| [Extension map](/core/extend/) | I want to change X — which seam do I use? |
| [Embed the runtime](/core/extend/embed-runtime) · [Write a service descriptor](/core/extend/service-descriptor) · [Connect an external backend](/core/extend/external-backend) · [Write a provider adapter](/core/extend/provider-adapter) · [Test and accept](/core/extend/testing) | The four extension paths, each with code, commands and acceptance criteria. |

Two background documents in the repository are worth reading alongside these pages:
`docs/KERNEL.md` (kernel boundary and the ledger contract) and `docs/SERVICE-GRAPH.md` (why the
graph and the handoff work this way).

## Status and honesty note

Keel is a working runtime under active hardening, not a finished GA product. `docs/MATURITY.md`
grades every capability G0–G5 with the evidence each gate requires, and states plainly what is
still missing (for example: per-tenant loadout hot-switching, service-ised sandbox/hooks, and CPU /
memory budgets for external sidecars). Where this site describes something as done, it was
verified against the code at the documented revision; where something is a plan, the page says so.
