# Defects found during Windows acceptance of v0.6.0

Date: 2026-11-29 (Asia/Shanghai)
Machine: Windows 11 家庭版中文版, build 10.0.26300, AMD64 (Ryzen 7 8845HS)
Asset: `keel-0.6.0-windows-amd64.zip`, SHA-256
`44d3777847491459f18af30c219951ba4f0af1dde6ced8f96ba5ac43e5576274` (matches
release `SHA256SUMS.txt` and the GitHub asset digest).

v0.6.0 is **not** Windows-verified. It remains immutable; the fixes ship in
v0.6.1.

## P0 — No-argument first start hangs after PostgreSQL is ready

- Symptom: `keel.exe` (double-click / no args) extracts the runtime, initializes
  PostgreSQL, PostgreSQL logs `ready to accept connections`, but
  `127.0.0.1:13750` never listens; the process stays alive with no CPU growth.
  A 180s health poll times out.
- `keel db start` reproduces: exit 124 after 20s, while PostgreSQL is listening
  on `127.0.0.1:13741`.
- Root cause: `startDevDBCluster` used `exec.Command(...).CombinedOutput()` for
  `pg_ctl -w start`. On Windows the daemonized postmaster inherits the pipe
  write end, so `CombinedOutput` waits for EOF forever.
- Evidence: `../../fix-windows-pg-start/evidence/04a-start-hang-diagnosis.txt`,
  `04c-db-start-repro.txt`, `dev-pg.log`.
- Fix change: `fix-windows-pg-start` (startPGDaemon writes pg_ctl stdio to a
  file and uses Run; regression tests on Linux + Windows).

## P1 — Fresh product shell `/v1/ready` is 503 forever (missing handoff tables)

- Symptom: after the P0 fix, `/v1/health` 200 but `/v1/ready` stays 503 with
  `store: ready: missing tables: runtime_instances, handoffs`.
- Root cause: the readiness contract requires the handoff audit tables; only
  `cmd/runtime` applied `handoff.NewPGRegistry`, `cmd/keel serve` did not.
  This affects every fresh product database, not only Windows.
- Evidence: `../../fix-serve-ready-schema/evidence/repro-before-fix.txt`,
  `repro-ready-503.txt`.
- Fix change: `fix-serve-ready-schema` (serve applies the handoff schema after
  kernel migration; product E2E waits for `/v1/ready`).

## Non-blocking observation — released binary reports `(dirty)`

`keel.exe version` prints the tagged commit `01862dab... (dirty)`. The release
workflow generates embedded assets inside the checkout, so Go's VCS stamping
sees a modified tree. This is a provenance/release-hygiene nit, not a runtime
failure; the first line still reports `keel 0.6.0`. Not fixed in v0.6.1 (it is
independent of the Windows runtime fixes); candidate for a separate release
hygiene change.

## Not defects

- No code signing is expected: `Get-AuthenticodeSignature` is `NotSigned`.
- `gh`/`curl` downloads carry no Mark-of-the-Web; no SmartScreen prompt is
  expected on that path. Do not claim Windows signing or SmartScreen testing
  beyond what was actually observed.
