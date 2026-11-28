# keel 0.6.1 — Windows first-start fix

keel 0.6.1 fixes two defects found by real Windows 11 acceptance of v0.6.0. v0.6.0
assets are unchanged; use 0.6.1 or later on Windows.

## Fixes

- **Windows first start no longer hangs after PostgreSQL is ready.**
  `pg_ctl -w start` was captured through a pipe. On Windows the daemonized
  postmaster inherits the pipe write end, so the parent waited for EOF forever:
  PostgreSQL logged `ready to accept connections`, but keel never listened on
  `127.0.0.1:13750`. 0.6.1 writes pg_ctl output to `<KEEL_HOME>\dev-pg.log.ctl`
  and waits only for `pg_ctl`; `keel db start` and the double-click/one-command
  start reach `/v1/ready`.
- **Fresh `keel serve` now reaches `/v1/ready` 200.**
  The readiness contract includes the handoff audit tables
  (`runtime_instances`, `handoffs`), but only the runtime applied that schema.
  The product shell now applies the same idempotent handoff migration after the
  kernel migration.

## Verification

- Windows 11 x64 real-machine: one-command start, `/v1/health`,
  `/v1/ready`, `/v1/capabilities`, web client, session/message, full process and
  PostgreSQL restart, board task persistence, and a path with spaces/Chinese
  characters.
- Linux/Windows regression tests for the pg_ctl stdio path; product E2E now
  waits for `/v1/ready`.
- v0.6.0 tag and assets were not rebuilt or replaced.

No code signing is involved. `SHA256SUMS.txt` covers every asset; on Windows
use `keel.exe update --check` (or download the zip) to move from 0.6.0.
