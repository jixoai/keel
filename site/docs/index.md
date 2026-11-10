---
layout: home

hero:
  name: Keel
  text: An event-ledger core with a Harness on top
  tagline: Open-source agent runtime in Go. Stateless, multi-tenant, 1000 concurrent sessions inside a 2GB budget — kill the process and sessions continue.
  actions:
    - theme: brand
      text: Core — how it works
      link: /core/
    - theme: alt
      text: Harness — plugins and operations
      link: /harness/
    - theme: alt
      text: 30-second quickstart
      link: /core/#quickstart

features:
  - title: The ledger is the only truth
    details: Every state change is one append-only event row in PostgreSQL. Checkpoints and transcripts are projections; a run is a lease over a session, not an object in memory.
    link: /core/architecture
  - title: Stateless execution
    details: Kill -9 the process at any await point. A new owner takes the lease with a higher fencing epoch, rebuilds from the log and continues — no lost events, no duplicated final reply.
    link: /core/events
  - title: Real streaming backpressure
    details: Images stream from storage to base64 to HTTP without ever materialising in memory. Lane pools by request size, a global token bucket and per-write deadlines make bandwidth the explicit ceiling.
    link: /core/storage
  - title: Two extension surfaces
    details: The Core exposes services, loops, providers and external backends as replaceable seams. The Harness on top adds a plugin standard for tools, hooks, triggers and UI.
    link: /core/extend/
---

## What Keel is

Keel is a general-purpose, stateless, multi-tenant **agent runtime**. It exists to answer one
engineering question: what should execution, storage, streaming and backpressure look like when
you run a thousand long-lived sessions — each possibly carrying images, tool calls and long
contexts — without letting process memory grow with the session count and without losing
sessions across restarts.

The repository ships both main lines:

| Line | What it is | Entry point |
|---|---|---|
| **Core** | The ledger, the loop, the service graph, storage and the runtime gates | `./cmd/runtime`, `/core/` docs |
| **Harness** | The product layer over the Core: sessions, approvals, plugins, triggers, sub-agents, TUI and web UI | `./cmd/keel` (`keel serve`), `/harness/` docs |

Keel is a Go re-implementation of [PI](https://github.com/earendil-works/pi): interfaces, event
semantics and ecosystem artifacts (`SKILL.md`, MCP, compaction, telemetry) follow PI as the single
reference; only the runtime is replaced (PostgreSQL, SSE, multi-tenancy, a 2GB budget). The runtime
loop itself is assembled from the minimal necessary code moved from
[pigo](https://github.com/smallnest/pigo) (MIT), with the original copyright headers kept.

## 30-second quickstart

The commands below were run against the repository at the revision this site documents; the
response shapes shown are the actual ones.

```bash
# 1. a PostgreSQL 18 instance for the ledger
docker run -d --name keel-pg \
  -e POSTGRES_PASSWORD=runtime -e POSTGRES_DB=runtime \
  -p 13741:5432 postgres:18

# 2. build and start the Harness (product shell + API + worker)
git clone https://github.com/jixoai/keel.git && cd keel
go build -o keel ./cmd/keel
export KEEL_DSN='postgres://postgres:runtime@127.0.0.1:13741/runtime?sslmode=disable'
./keel serve --addr 127.0.0.1:13750 \
  --tenant 11111111-2222-3333-4444-555555555555 \
  --password devpass
```

```bash
# 3. talk to it (second terminal; the passphrase is the bearer token)
T=http://127.0.0.1:13750
TENANT=11111111-2222-3333-4444-555555555555
SID=$(curl -s -X POST -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  -H 'Content-Type: application/json' -d '{}' $T/v1/sessions | cut -d'"' -f4)

curl -s -X POST -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  -H 'Content-Type: application/json' -d '{"text":"hello keel"}' \
  $T/v1/sessions/$SID/input
# {"created":true,"requestId":"…","seq":1,"status":"committed","submissionId":"…"}

curl -s -H "Authorization: Bearer devpass" -H "X-Tenant-ID: $TENANT" \
  "$T/v1/sessions/$SID/events" | head -c 400
# {"events":[{"seq":1,"kind":"user.message",…},{"seq":2,"kind":"assistant.message",…},
#            {"seq":3,"kind":"run.finished","payload":{"status":"done",…}}]}
```

With no model configured, the runtime uses its deterministic **fake provider**, so the reply is
canned and the whole path — edge commit, claim, loop, commit, projection — still runs. Point
`KEEL_MODEL` (plus `KEEL_BASE_URL`/`KEEL_API_KEY`) at a real provider when you want real answers.
The complete runbook, including tests, load tests and the 2GB cgroup replay, is in
[`docs/RUNBOOK.md`](https://github.com/jixoai/keel/blob/main/docs/RUNBOOK.md).

## Where to go next

- **Core** — [what Keel is](/core/), then [architecture](/core/architecture),
  [events and protocol](/core/events), [AgentAPI](/core/agentapi),
  [storage and runtime](/core/storage), and the [extension map](/core/extend/).
- **Harness** — [what the Harness is](/harness/) and its plugin standard, starting at
  [plugins](/harness/plugins/).
- **Glossary** — one place for the vocabulary both lines share: [glossary](/glossary).

## Repository and license

- Source: `https://github.com/jixoai/keel` (Go module `github.com/jixoai/keel`, Go 1.27.3 per
  `go.mod:3`)
- License: Apache-2.0 ([`LICENSE`](https://github.com/jixoai/keel/blob/main/LICENSE)); files moved
  from pigo keep their MIT copyright notice
  ([`third_party/pigo-LICENSE`](https://github.com/jixoai/keel/blob/main/third_party/pigo-LICENSE),
  attribution in [`NOTICE`](https://github.com/jixoai/keel/blob/main/NOTICE)).
- Maturity: every capability is graded G0–G5 in
  [`docs/MATURITY.md`](https://github.com/jixoai/keel/blob/main/docs/MATURITY.md); GA means all
  capabilities reach G5.
