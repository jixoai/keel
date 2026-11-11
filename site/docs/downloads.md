---
title: Download
---

# Download keel

keel ships as a single Go binary plus a static web client. Both come from the
same tagged commit, cross-compiled in CI — nothing is built on a laptop, so the
checksums below mean something.

**Current release: `v0.1.0`** (pre-1.0; expect breaking changes between minor versions.)

## What you get

| Artifact | What it is |
|---|---|
| `keel` | The product shell: `keel serve` (AgentAPI + web UI + SSE on one origin), `keel tui` (terminal client), `keel doctor`, `keel version` |
| `keel-web-0.1.0.zip` | The packaged web client. Unzip it and point `keel serve -web-dir <dir>` at it |

## Requirements

- **PostgreSQL 14+** — the event ledger lives there. `keel serve` needs a DSN.
- **A model endpoint** — any OpenAI-compatible API (or Anthropic), plus a key.
- **The web client** — provided separately (the binary does not embed it).

## Platform builds

| Platform | Download |
|---|---|
| macOS · Apple silicon | [keel-0.1.0-darwin-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-darwin-arm64.tar.gz) |
| macOS · Intel | [keel-0.1.0-darwin-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-darwin-amd64.tar.gz) |
| Linux · x86-64 | [keel-0.1.0-linux-amd64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-linux-amd64.tar.gz) |
| Linux · arm64 | [keel-0.1.0-linux-arm64.tar.gz](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-linux-arm64.tar.gz) |
| Windows · x86-64 | [keel-0.1.0-windows-amd64.zip](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-0.1.0-windows-amd64.zip) |
| Web client | [keel-web-0.1.0.zip](https://github.com/jixoai/keel/releases/download/v0.1.0/keel-web-0.1.0.zip) |

All assets: [the v0.1.0 release](https://github.com/jixoai/keel/releases/tag/v0.1.0) ·
newer releases: [releases](https://github.com/jixoai/keel/releases) ·
[`SHA256SUMS.txt`](https://github.com/jixoai/keel/releases/download/v0.1.0/SHA256SUMS.txt)

## Verify what you downloaded

```bash
curl -LO https://github.com/jixoai/keel/releases/download/v0.1.0/SHA256SUMS.txt
shasum -a 256 -c SHA256SUMS.txt --ignore-missing
```

## First run

```bash
# 1. the ledger needs a database
createdb keel

# 2. run the server (it hosts /v1, the web UI and SSE on one port)
export KEEL_DSN='postgres://postgres@127.0.0.1:5432/keel?sslmode=disable'
export KEEL_SERVE_TENANT="$(uuidgen | tr 'A-Z' 'a-z')"
export KEEL_SERVE_PASSWORD='choose-a-passphrase'
export KEEL_SERVE_WEB_DIR="$PWD/keel-web"   # the unzipped web client
export KEEL_MODEL='deepseek-v4.1-flash'
export KEEL_PROVIDER='openai'
export KEEL_BASE_URL='https://api.example.com/v1'
export KEEL_API_KEY='…'
keel serve -addr 127.0.0.1:13750
```

Open `http://127.0.0.1:13750`, enter the passphrase, and send a message.
`keel doctor` prints what the process resolved (config, build, artifacts) when
something does not start.

The terminal client talks to the same server:

```bash
keel tui --url http://127.0.0.1:13750
```

## Notes and honest limits

- **Nothing is code-signed.** On macOS, clear the quarantine flag once:
  `xattr -dr com.apple.quarantine ./keel`.
- **No bundled database.** Bring your own PostgreSQL; `keel` runs its own
  migrations on first start.
- **Pre-1.0.** The AgentAPI is versioned (`apiVersion: v1`) but the product
  surface still moves. The plugin contract is the stable part.
- **Mirrors.** If GitHub is slow, swap the host for `https://gh.fde2e.tweb.xin/`
  (self-built mirror, domestic) or `https://ghfast.top/https://github.com/…` —
  paths and checksums are identical.
