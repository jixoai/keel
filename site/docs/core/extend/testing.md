# Test and accept

Keel's definition of "done" is evidence-based: a gate either has raw output or the claim is not
made. This page lists the gates, what each one proves, and the commands that reproduce them.

## 1. The everyday gates (run before any commit)

```bash
gofmt -l .                 # must print nothing
go vet ./...
KEEL_TEST_DSN="$KEEL_TEST_DSN" go test -race ./...
go build ./...
```

These are exactly what CI runs (`.github/workflows/ci.yml`: gofmt zero-output, vet, race tests with a
`postgres:18` service, build; plus `docker build -t keel:ci .` and a scheduled chaos job). Local
equivalents are in `docs/RUNBOOK.md:359`. Integration tests (blob/HTTP) skip themselves when
`KEEL_TEST_DSN` is unset — but note the corollary: a green run *without* the DSN proves much less.

## 2. One command for the whole battery

```bash
KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=/tmp/keel-evidence \
  scripts/acceptance-all.sh
```

`scripts/acceptance-all.sh` runs every gate serially, **fail-fast**, and writes raw evidence into
`$KEEL_EVIDENCE_DIR` (`scripts/acceptance-all.sh:1`):

| Gate | Evidence file | What it proves |
|---|---|---|
| `go test -race ./...` | `race-full.log` | the whole tree is race-clean |
| PI conformance | `conformance.json`, `conformance-summary.txt` | the loop still matches the pinned PI behaviour suite |
| R1 differential | `r1-live.txt`, `r1/` | the default assembly behaves identically to the baseline binary on the same input and database |
| R1 negative control | `r1-negative.txt` | the differential gate *can* fail (a stale-rule probe must FAIL; a gate that cannot fail is not a gate) |
| Stateless E2E | `e2e-8of8.json` | idempotency, SSE cursor, blob resume, agent reply, kill during model call, kill during tool execution, 100×2000 event continuity on a fresh database |
| SIGTERM drain | `drain-smoke.txt` | health stays reachable, new writes answer 503, the SSE `drain` frame arrives, the process exits, and the restart produces exactly one reply |
| In-flight input | `inflight.txt` | input committed after a claim is really answered (the tool call runs exactly once) |
| External gateway smoke | `external-smoke.log` | 15 steps against a real runtime + PG + reference sidecar, ending in `SMOKE PASS (15 steps)` with 0 leftover processes |
| Chaos, kill mode | `perf/chaos-kill.json` | random SIGKILL for 10 minutes: every input answered once, run states converge |
| Chaos, handoff mode | `perf/chaos-handoff.json` | takeover while crashing, with `runtime_instances`/`handoffs`/`owner_epoch` as proof of who took over |
| 2GB / 1000 sessions | `perf/final-1000.json` | cgroup peak inside the 2GB limit, 0 hard stream errors, 429s accounted as admission backpressure |
| Dual-process handoff RSS | `perf/handoff-dual.json` | the two-process window (old draining + new claiming) also stays inside the budget |

Two disciplines make the evidence trustworthy:

- **Artifacts are truncated before each run.** Re-running in the same directory must never leave two
  runs' output concatenated — a stale artifact is a lying artifact (`scripts/acceptance-all.sh:52`).
- **Every gate owns a dedicated database.** Tools that TRUNCATE/DROP refuse to run against a shared
  library: `cmd/e2e -reset` requires a dedicated database, `r1diff` requires the name to contain
  `r1diff`, and the load generators require `e2e/test/smoke/perf/accept` in the name
  (`docs/RUNBOOK.md:182`). One old-schema migration against a shared database is enough to delete
  rows another thread is using — that accident is recorded in the runbook.

## 3. Running the individual gates

```bash
# stateless E2E (7 scenarios, real SIGKILL)
go build -o /tmp/keel-e2e ./cmd/e2e
KEEL_DSN="$KEEL_TEST_DSN" /tmp/keel-e2e -bin /tmp/keel-runtime -port 13745

# loop conformance (the PI suite caliber)
scripts/conformance.sh

# relocation parity: baseline binary vs candidate, same script, normalized diff
bash scripts/build-r1-baseline.sh e7361f8 /tmp/keel-r1-base
go run ./cmd/r1diff -old-bin /tmp/keel-r1-base -new-bin /tmp/keel-r1-cand \
  -dsn 'postgres://…/keel_r1diff?sslmode=disable' -out /tmp/r1 \
  -port-old 13782 -port-new 13783 -allow openspec/changes/archive/keel-service-graph/r1-allow.json

# SIGTERM drain / in-flight input / 2GB perf / dual handoff / chaos
KEEL_DRAIN_BIN=/tmp/keel-runtime bash scripts/acceptance-drain.sh
KEEL_INFLIGHT_BIN=/tmp/keel-runtime bash scripts/acceptance-inflight.sh
bash scripts/acceptance-perf.sh 1000 60s
bash scripts/acceptance-handoff-rss.sh
go build -o /tmp/keel-chaos ./cmd/chaos && /tmp/keel-chaos -bin /tmp/keel-runtime \
  -dsn 'postgres://…/keel_chaos_e2e?sslmode=disable' -port 13795 -sessions 8 -duration 10m \
  -kill-every 20s -mode kill
```

The R1 baseline is built from a **fixed commit** (`build-r1-baseline.sh e7361f8 …`), not from
`main` — `main` moves, and the baseline must not.

## 4. Which gate does my change need?

| You changed | Minimum gate |
|---|---|
| a package's internals, no assembly change | `gofmt`/`vet` + `go test -race ./...` |
| the default assembly, kernel, or the HTTP contract | the full `scripts/acceptance-all.sh` (the R1 differential is mandatory) |
| the event vocabulary or commit validation | R1 differential **plus** the E2E and chaos gates — the transcript/SSE rules move with the kind |
| an external provider integration | `go test -race ./internal/gateway ./internal/externalgw` + `scripts/external-gateway-smoke.sh` |
| a provider adapter | provider tests + the real-model E2E path (`docs/REAL-MODEL-E2E.md`) |
| docs only | none of the above; the site build gates (`pnpm --dir site build`) apply to the site |

## 5. The maturity ladder

Per-capability gates are defined in `docs/MATURITY.md:8`: **G0** spec, **G1** single-machine
functional (fake provider, no network), **G2** crash consistency (kill -9 at every await point, in
CI), **G3** observability (one `run.finished` per run, fixed-cardinality metrics, structured logs
without sensitive content), **G4** PI alignment (recorded PI streams replayed identically, real
`SKILL.md`/MCP/hook artifacts work unchanged), **G5** scale and operations (2GB / 1000 sessions /
64 concurrent runs, 10-minute random kill, drain/readiness, docs + examples + security review).

Two rules from that document are worth repeating because they shape how you report work:

- **No skipping.** An ability is not "recoverable" until G2 passes and not "operable" until G3
  passes; `docs/MATURITY.md:37` also carries the honest gap column.
- **G4 uses real upstream artifacts** — a real `SKILL.md`, a real MCP server, a real hook
  configuration — never a self-made sample (`docs/MATURITY.md:51`).

## 6. Reporting an acceptance run

The project's delivery standard for evidence is an HTML report with per-step screenshots and
assertions (this site's own change uses the same discipline). For a runtime acceptance run, keep the
raw `$KEEL_EVIDENCE_DIR` next to the summary: the JSON/LOG artifacts are what make the claims
checkable, and every verdict line in `acceptance-all.log` names the artifact that backs it.
