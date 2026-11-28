# Repro — fresh keel serve reports /v1/ready 503

Date: 2026-11-29 (Asia/Shanghai), Windows 11 家庭版中文版 10.0.26300, AMD64.

## Observation

After the `pg_ctl` pipe fix, a fresh `keel start` on an isolated `KEEL_HOME`:

- `GET /v1/health` → 200 in 1.6s
- `GET /v1/ready` → 503 for the full 60s poll (and continued afterwards)
- service log: `store: ready: missing tables: runtime_instances, handoffs`
- `127.0.0.1:13750` and `127.0.0.1:13741` both listening; PostgreSQL connected

Raw:

- `repro-before-fix.txt` — fixed build log and first `/v1/ready` 503
- `repro-ready-503.txt` — 60s readiness poll with the missing-table log lines

## Root cause

`internal/store.CheckReady` includes `runtime_instances` and `handoffs` in
`requiredTables`. Those tables are created by `internal/handoff/schema` and
applied by `handoff.NewPGRegistry`. `cmd/runtime` calls it at boot; `cmd/keel
serve` did not. A fresh product database therefore never satisfied its own
readiness contract.

## Fix

`cmd/keel/serve.go` calls `handoff.NewPGRegistry(ctx, st.Pool())` after the
kernel migration (idempotent, same advisory lock as store/kernel migrations).
Failure is a boot error, not a permanently unready process.

## Fixed evidence

`fixed-ready.txt`: fresh `KEEL_HOME`, `/v1/health` 200 in 1.6s and `/v1/ready`
200 in 0.1s, both listeners up.
