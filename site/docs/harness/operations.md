# Operations

This page is the runbook for a Harness deployment: how to start it, how to look
at it, where its artifacts live, and how to upgrade it without losing a
session. It covers both the implemented surfaces (cited) and the planned plugin
root (marked).

## 1. `keel serve` — the Harness host

```bash
export KEEL_DSN='postgres://postgres:<password>@127.0.0.1:13741/runtime?sslmode=disable'
export KEEL_SERVE_TENANT=<tenant-uuid>
export KEEL_SECRET_KEY='<32-byte base64/hex>'   # signs the session + CSRF cookies
keel admin bootstrap --tenant "$KEEL_SERVE_TENANT"   # empty database, once
keel serve --dsn "$KEEL_DSN" --addr 127.0.0.1:13750
```

| Variable / flag | Meaning | Default |
| --- | --- | --- |
| `KEEL_DSN` / `--dsn` | PostgreSQL DSN (required) | — |
| `KEEL_SERVE_ADDR` / `--addr` | listen address | `127.0.0.1:13750` |
| `KEEL_SERVE_TENANT` / `--tenant` | the single tenant this process acts as (required when auth is on) | — |
| `KEEL_SECRET_KEY` | 32-byte base64/hex key for cookie + CSRF signing; also enables AES-256-GCM tenant secret storage | required (`auth=required`) |
| `KEEL_AUTH` / `--auth` | `required`, `local-only` or `off` | `required` |
| `KEEL_BOOTSTRAP_TOKEN` | one-time setup token for a container-like first boot | — |
| `KEEL_SERVE_WEB_DIR` / `--web-dir` | static web build directory | `web/dist` |
| `KEEL_MODEL` / `KEEL_PROVIDER` / `KEEL_BASE_URL` / `KEEL_PROTOCOL` / `KEEL_API_KEY` | model selection (fake provider when unset) | — |
| `KEEL_MODELS_CONFIG` / `--models-config` | tenant models document (providers/rules/selection) | — |
| `KEEL_SKILLS_ROOTS` | OS path list of skill roots (PI-compatible) | — |
| `KEEL_RUNTIME_ID` | runtime identity written into claims/audit (handoff) | hostname-pid |

(Source: `cmd/keel/main.go`, `cmd/keel/auth.go`, `docs/HANDOFF.md`.)

Operational facts worth knowing before you expose the port:

- **Authentication is a network middleware, not an API concern**: a human
  exchanges username + password for an HMAC-signed HttpOnly cookie and sends
  `X-Keel-CSRF` on writes; a machine presents a Bot-bound `keel_bot_...` key.
  The middleware injects the tenant and strips any client-supplied
  `X-Tenant-ID` (`internal/harnessapi/api.go`, `cmd/keel/auth.go`).
- **An empty database has no super admin.** `keel admin bootstrap` mints a
  one-time setup token; the browser completes setup and enters the app directly.
  Only a CLI/admin password reset leaves a temporary credential that must be
  changed at first login. There is no first-visitor-becomes-admin path.
- `local-only` answers loopback only and maps requests to a built-in
  local-admin; `off` additionally requires explicit `--insecure` and refuses a
  public bind (`cmd/keel/auth.go`).
- `keel serve` is **single-tenant** (`KEEL_SERVE_TENANT`); multi-tenant reuse
  needs the multi-tenant front (that is a product decision, not a flag).
- The process is stateless: every fact is in PostgreSQL (plus the CAS for
  large blobs), so `kill -9` is a supported failure mode, not an incident
  (`docs/ARCHITECTURE.md` §3).
- Repository convention: development ports live in **13610–13799**; `keel
  serve` uses 13750 by default and the TUI assumes the same origin
  (`cmd/keel/serve.go:66`).

### Static UI

`keel serve` serves the NSH web build from the configured directory; NSH
packages into a nested root (`…/web/<app>/<App>/index.html`) and the handler
resolves it once at boot rather than guessing per request
(`cmd/keel/static.go:16`). When the build is missing, non-API paths answer a
JSON 404 that names the missing artifact and the build command — never a blank
page (`cmd/keel/static.go:13`).

## 2. `keel tui`

```bash
# a Bot-bound key (machine client); a human uses an account session instead
keel tui --url http://127.0.0.1:13750 --token "$KEEL_AGENT_TOKEN"
# or inspect the resolved command without running it:
keel tui --print
```

The TUI is the same client (same TSX, same Harness API) rendered in a terminal
host; it is not a second implementation (`docs/WEBUI.md`). `KEEL_TUI_BIN`
points at the packaged TUI binary (`nsh package --target tui`), `KEEL_AGENT_URL`
at the running host (`cmd/keel/tui.go:22`). It deliberately does not render
plugin UIs — plugin rows open a web URL instead
([Plugin UI](/harness/plugins/ui)).

## 3. Plugin directory

| Aspect | Today | Planned |
| --- | --- | --- |
| Where plugins come from | compiled into the binary (`keel.example`, `keel.creation`, `keel.testkit`, the registry plugins — `internal/pluginpkg/builtin/`) | `<plugins-root>/<id>/` packages discovered at boot |
| Enable/disable | process assembly (`harnessext.Runtime.Apply`) | lifecycle API + approval ([Lifecycle](/harness/plugins/lifecycle)) |
| State | `active`/`pending`/`failed`/`stopped` per plugin, generation counter, diagnostics | plus `installed`/`disabled`, install/uninstall/update audit |
| Visible at | `GET /v1/plugins` + web settings › Plugins (`internal/harnessapi/ext.go:18`, `web/src/screens/settings.tsx:1148`) | same endpoints extended, not replaced |

The planned plugins root is a **managed directory**: packages are copied into
it, content-hashed at install, and never referenced in place
([Manifest](/harness/plugins/manifest)). Back it up like any other state
directory; the ledger does not contain plugin binaries.

## 4. Diagnostics

| Surface | What it answers |
| --- | --- |
| `keel doctor` | build identity, config pairing (secret/tenant/auth mode), web artifact, TUI artifact; with `--db`: database reachability and schema presence. Read-only — never migrates, never writes, never starts the worker (`cmd/keel/doctor.go`) |
| `keel version` | module version, VCS revision (+dirty), Go toolchain (`cmd/keel/main.go:80`) |
| `GET /v1/health` | process liveness |
| `GET /v1/ready` | store readiness (migrations applied) |
| `GET /v1/metrics` (JSON) and `GET /metrics` (Prometheus) | stream/scheduler/admission counters, run metrics |
| `GET /v1/plugins` | assembled surface: generation, plugin states, tools, hooks summary, diagnostics |
| `GET /v1/triggers` | trigger inventory + audit windows (`internal/harnessapi/ext.go:39`) |
| `GET /v1/sessions/{id}/run` and `/run/loadout` | run row (status/attempts/cursor/lastError) and the pinned loadout (`docs/LOADOUT.md` §5) |
| `POST /v1/runtime/drain`, `GET /v1/runtime/status` | drain control and observability (`docs/HANDOFF.md`) |
| `GET /v1/sessions/{id}/decisions` | open/answered/timed-out approvals |

Logs are structured (`log/slog`); kernel rejections are projected into
structured audit fields with rate limiting, so "why did this commit fail" is a
log query, not a debugging session (`internal/servicehost/audit.go`,
`docs/LOADOUT.md` §6).

## 5. Upgrade and rollback

Upgrades are **handoff**, not restart (`docs/HANDOFF.md`; full description in
[Recovery and compaction](/harness/recovery-and-compaction) §3):

```bash
# 1) start the new binary (same DSN, distinct KEEL_RUNTIME_ID)
# 2) drain the old process (human session cookie + CSRF, or a Bot key)
curl -XPOST $old/v1/runtime/drain -b "$JAR" -H "X-Keel-CSRF: $CSRF" \
  -d '{"mode":"handoff","deadlineMs":30000}'
# 3) watch it go quiet:
curl -s -b "$JAR" $old/v1/runtime/status     # claiming=false, inFlightRuns=0
# 4) SSE clients reconnect on `event: drain` and resume from Last-Event-ID
```

Mandatory discipline:

- **never let two writers coexist** — old binaries bypass the fence; drain the
  old process before the new one takes over, on upgrades *and* rollbacks
  (`docs/HANDOFF.md`, `internal/kernel/cutover_test.go`);
- `handoff` settles in-flight runs at their commit boundary and clears leases;
  `shutdown` (SIGTERM) abandons them and the new process waits out one lease
  TTL;
- rollback = drain → stop new → start old; **do not roll back data** — the
  ledger is append-only and old binaries skip unknown kinds when reading
  (`docs/HANDOFF.md`, the "rollback manual" section).

## 6. Capacity notes

The Harness inherits the Core's budget (2GB, ~1000 concurrent sessions) and the
stream path is the usual place to look first: new streams are token-bucket
admitted (429 + `Retry-After`), the scheduler separates control frames from
bulk blobs, and slow clients are cut by write/progress deadlines
([Streaming and partials](/harness/streaming-and-partial) §3). For sizing
evidence and the measured 1000-session run see `README.md` and
`docs/ARCHITECTURE.md` §8.
