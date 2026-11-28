# Tasks: keel-pi-parity-closure-2

## 1. CompactionTask / checkpoint

- [x] durable task kind/state/attempts/retry, fenced by the session/run lease (no independent task-lease row)
- [x] PI entry/checkpoint semantics + crash matrix
- [x] resume test: 0 lost events / 0 duplicate final replies

## 2. Skills allowed-tools

- [x] execution-time intersection with actual toolset
- [x] typed deny + audit + negative tests

## 3. MCP lifecycle

- [x] bounded pool/health/backoff
- [x] destructive-tool hook/approval
- [x] server crash does not take down run

## 4. Usage ledger

- [x] provider usage/observed stream wired to existing ledger
- [x] run summary reads the same values

## 5. Nightly chaos / conformance

- [ ] 10-minute random SIGKILL job in CI (wired in `.github/workflows/ci.yml`; not executed on GitHub Actions from this environment — local 600s run PASS)
- [ ] conformance golden job on PI pin changes (wired in `.github/workflows/conformance.yml`; not executed on GitHub Actions — local golden run PASS)

## 6. Capacity/docs

- [x] 2GB/1000 sessions/64 runs evidence refreshed
- [x] per-capability runbook
- [ ] independent reviewer P0/P1=0; archive

> Evidence: `evidence/execution-2026-11-28.md` (tests, conformance golden,
> formal 600s chaos PASS, 2GB/1000/64 PASS), `RUNBOOK.md`. Review/archive is
> intentionally deferred to the independent reviewer. Lease note: the task row
> has no independent lease; the kernel session/run lease is the fencing
> authority (see RUNBOOK §1).
