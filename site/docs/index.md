---
layout: home

hero:
  name: Keel
  text: An event-ledger core with a Harness on top
  tagline: Open-source agent runtime in Go. Stateless, multi-tenant, 1000 concurrent sessions inside a 2GB budget — kill the process and sessions continue.
  actions:
    - theme: brand
      text: Download v0.5.0
      link: /downloads
    - theme: alt
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
# 1. download the single binary for your platform from /downloads, then:
chmod +x keel

# 2. macOS: generate ~/Applications/Keel.app (ad-hoc signed locally)
./keel install

# or run it in place on any platform (opens the browser when ready)
./keel start
```

One binary carries PostgreSQL 18, the web client and the README. There is no
Docker requirement, no `KEEL_DSN`, no `createdb`, no setup token: `keel start`
extracts the runtime under `$KEEL_HOME/runtime`, initializes a loopback-only
cluster, serves the Harness with `auth=local-only`, and opens
`http://127.0.0.1:13750`. `keel db status|stop|reset` manages the local cluster;
`keel uninstall` removes the app but keeps data (`--purge` deletes it).

Source builds (`git clone`, `go build`) still work and use the same `keel start`;
see the repository README. A Docker compose variant keeps PostgreSQL as a
service instead of embedding it.

Open `http://127.0.0.1:13750`, enter the setup token plus the username and password for the first
super admin: that password is the final credential, so the browser enters the app directly. Only a
temporary password minted by a CLI/admin reset forces a change at first login
(`cmd/keel/auth.go:568`; the flag is set only by a password reset — `internal/store/auth_events.go:173`). There is no passphrase and no
first-visitor-becomes-admin path. Machines use a Bot-bound key (`keel_bot_...`), never a browser
login. The exact login/CSRF requests and every endpoint shape are in the
[AgentAPI reference](/core/agentapi).

With no model configured, the runtime uses its deterministic **fake provider**, so the reply is
canned and the whole path — edge commit, claim, loop, commit, projection — still runs. Point
`KEEL_MODEL` (plus `KEEL_BASE_URL`/`KEEL_API_KEY`) at a real provider when you want real answers.
The complete runbook, including tests, load tests and the 2GB cgroup replay, is in
[`docs/RUNBOOK.md`](https://github.com/jixoai/keel/blob/main/docs/RUNBOOK.md).

## Where to go next

- **Core** — [what Keel is](/core/), then [architecture](/core/architecture),
  [events and protocol](/core/events), [Harness API](/core/agentapi),
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
