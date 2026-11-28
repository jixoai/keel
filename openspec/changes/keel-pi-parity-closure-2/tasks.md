# Tasks: keel-pi-parity-closure-2

## 1. CompactionTask / checkpoint

- [ ] durable task kind/state/attempts/lease/retry
- [ ] PI entry/checkpoint semantics + crash matrix
- [ ] resume test: 0 lost events / 0 duplicate final replies

## 2. Skills allowed-tools

- [ ] execution-time intersection with actual toolset
- [ ] typed deny + audit + negative tests

## 3. MCP lifecycle

- [ ] bounded pool/health/backoff
- [ ] destructive-tool hook/approval
- [ ] server crash does not take down run

## 4. Usage ledger

- [ ] provider usage/observed stream wired to existing ledger
- [ ] run summary reads the same values

## 5. Nightly chaos / conformance

- [ ] 10-minute random SIGKILL job in CI
- [ ] conformance golden job on PI pin changes

## 6. Capacity/docs

- [ ] 2GB/1000 sessions/64 runs evidence refreshed
- [ ] per-capability runbook
- [ ] independent reviewer P0/P1=0; archive
