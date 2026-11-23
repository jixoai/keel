# W0 RSS sampler (VmRSS process-tree memory gate) — Evidence

> Date: 2026-11-23. Worktree: `.worktrees/keel-w0-rss`, branch `task/keel-w0-rss`
> (based on main `1297837`). Scope: the W0 "RSS 采样器" item (`tasks.md:56-57`) and
> the RSS half of W11 G4 (`tasks.md:414`). **This file does not claim W11 G4 done**: G4
> also requires group OOM isolation and the front-end surface VM quota assertion,
> which are out of this slice (see §7).

## 1. What was delivered

| Item | Where | Gate |
|---|---|---|
| VmRSS sampler + threshold protocol (≤500MB hard, ≤300MB target) | `scripts/lib/rss_sampler.py` | `--self-test` (37 fixtures) |
| Gate wrapper: attach (port/pid) and profile modes, machine-readable summary | `scripts/acceptance-rss.sh` | live attach run (§4) |
| Opt-in wiring in the full battery (default SKIP) | `scripts/acceptance-all.sh` §8b | segment runs (§5) |
| CI-able parser/sum/threshold check (no process, no network) | `.github/workflows/ci.yml` | CI step `VmRSS sampler fixtures` |
| Fixture tests (47 cases incl. the Linux proc path, overhead exclusion and cadence) | `scripts/lib/test_rss_sampler.py` | `python3 -I scripts/lib/test_rss_sampler.py` |
| Runbook (protocol, platform behavior, flags, exit codes) | `docs/RUNBOOK.md` §「VmRSS 进程树内存门」 | doc review |
| `.gitignore` `__pycache__/` | `.gitignore` | — |

Not touched: product Go (`cmd/`, `internal/`), `web/`, schema, plugins,
`openspec/changes/keel-harness-v2/tasks.md` (maintainer updates the board).

## 2. Measurement protocol implemented

From `openspec/changes/keel-harness-v2/acceptance.md` §0 and `specs/harness-core/spec.md`
「内存测量协议」:

- RSS = **sum of `VmRSS` over the backend process tree** (root pid + all descendants),
  interval ≤10s (`--interval` is capped at 10), `VmRSS` excludes page cache.
- Thresholds use decimal MB (1e6 bytes, the same unit as the existing cgroup gates):
  `>500MB` → FAIL (exit 1); `300–500MB` → PASS with `withinTarget:false` («above target»);
  `≤300MB` → PASS `withinTarget:true`.
- cgroup `memory.peak` (includes page cache) is **never** mixed into the RSS verdict;
  `--cgroup <container>` attaches it as a separate `cgroup` object for the v1 comparison.
- Target identity: `--port` reads the backend's own `GET /v1/health` `pid` and cross-checks
  it with `lsof -iTCP:<port> -sTCP:LISTEN -t`. Mismatch (host-side container proxy, foreign
  listener, multiple listeners) → UNMEASURABLE, with the message pointing at `--pid` inside
  the container. `--pid` verifies existence first.
- Only the resolved root and its descendants are summed; a sibling process is never counted
  (fixture `TreeSumTests.test_sums_root_and_descendants_and_ignores_unrelated`).
- Zombies are recorded (`processTree.zombiePids`) and contribute 0. The sampler's own
  subtree (`processTree.excludedOverhead`, e.g. the `python3` sampler and its `ps` helper) is
  excluded: measurement cost is not workload, and `--pid <ancestor-of-sampler>` must not inflate
  the total. A live member without a reading, a vanished root, zero resident bytes, or an aborted
  window → UNMEASURABLE (exit 2). An incomplete measurement never degrades into PASS or into
  FAIL-of-a-number-it-did-not-see.
- Cadence uses absolute deadlines (tick n due at n·interval), so a slow `ps` cannot accumulate
  drift; the achieved spacing is reported as `sampling.observedIntervalSec`, and spacing beyond
  the protocol's 10s maximum is UNMEASURABLE (the host did not follow the protocol).

## 3. Platform behavior (explicit, no silent pass)

| Platform | Source used | Notes |
|---|---|---|
| Linux | `/proc/<pid>/status` `VmRSS` (kB) | Tree membership from `ps -A -o pid,ppid,rss,state,comm` (procsps/busybox-compatible). Without `ps`, a proc-only tree is certifiable **only** when no `/proc` row was unreadable; otherwise UNMEASURABLE (a hidden child cannot be ruled out). |
| macOS (this host) | `ps` resident set (KiB) | No procfs and no `VmRSS` field; `ps` RSS is the platform equivalent and does not include page cache. The summary names it: `"source": "ps-rss"`, `sourceNote` explains the substitution. A macOS/ps reading is never labelled as a Linux `VmRSS` reading. |
| neither procfs nor `ps` | none | `platform_source()` raises; verdict `UNMEASURABLE`, exit 2 (fixture `PlatformFallbackTests`). |

Summary always carries `source` (`proc-status` / `ps-rss` / `unavailable`), `sampling`
(interval, requested/actual window, sample count, `stopReason`, `sampleErrors`) and the
per-member `processes` list, so a reader can audit what was summed.

## 4. Gate output (live, macOS)

### 4.1 Fixture suite (no process, no network)

```text
$ bash scripts/acceptance-rss.sh --self-test
... 37 tests ...
Ran 37 tests in 0.018s

OK
```

47 cases: `/proc` status parsing (VmRSS kB, PPid, State, zombie w/o VmRSS, bad unit);
`ps` table parsing (macOS paths with spaces, truncated rows); tree walk (root+descendants,
depth 4, unrelated sibling excluded, zombie not counted, sampler-overhead subtree excluded
only when it lies inside the target tree); `read_vmrss` (member value,
exited member → 0, unreadable member → Unmeasurable); `snapshot_from_proc` over a synthetic
procfs dir; Linux `sample_once`: ps-tree + per-member VmRSS (15MB tree next to a 90MB
sibling → 15MB), proc-only fallback when ps is absent, proc-only refused when a row is
unreadable; threshold bands (300 / 450 / 500 / 501 MB, above-target band,
incomplete-beats-breach, cadence beyond the 10s protocol maximum, declining
steady/p50); merge workload gate (valid, quiet, short,
unreadable bench, hard SSE errors, sampling window too short); sampling cadence (absolute
deadlines, stop-file early exit, observed spacing); platform fallback (no procfs
and no ps → UNMEASURABLE); CLI argument/identity contract.

### 4.2 Attach mode against a real multi-process tree

**(a) Port identity + descendant summing** — fixture `python3` (not product code) serving
`GET /v1/health` (`{"pid": <parent>}`) with one `sleep` child on 127.0.0.1:13771:

```text
$ scripts/acceptance-rss.sh --port 13771 --duration 5 --interval 1 --out attach-pass.json
attach: port 13771 is listening; resolving its owner via /v1/health + lsof
RSS sampler: mode=attach platform=darwin source=ps-rss pid=14140 port=13771 identity=health+lsof comm=python3
RSS sampler: 6 sample(s) at 1s interval (observed 1.0s) over 5.097s (duration), tree=2 process(es)
RSS sampler: peak=21.23MB ceiling=500MB target=300MB steady=19.93MB p50=20.35MB withinTarget=True
RSS sampler: NOTE: peak 21.23 MB is within the 300 MB target
RSS VERDICT: PASS (exit 0)
```

```json
{
  "target": {"pid": 14140, "port": 13771, "identity": "health+lsof", "comm": "python3"},
  "sampling": {"intervalSec": 1, "observedIntervalSec": 1.0, "actualDurationSec": 5.097, "samples": 6, "stopReason": "duration"},
  "processes": [
    {"pid": 14140, "ppid": 14137, "comm": "python3", "rssKB": 18464},
    {"pid": 14142, "ppid": 14140, "comm": "sleep",  "rssKB": 192}
  ],
  "totals": {"peakMB": 21.23, "minMB": 19.1, "p50MB": 20.35, "steadyMB": 19.93},
  "thresholds": {"ceilingMB": 500, "targetMB": 300, "withinTarget": true},
  "verdict": "PASS", "exitCode": 0, "source": "ps-rss"
}
```

**(b) Cadence + sampler overhead** — sampling a shell whose child chain contains the
sampler itself. The sampler's own subtree is excluded (`processTree.excludedOverhead`)
so `--pid $$` cannot inflate the number; the achieved spacing is reported
(`observedIntervalSec`), not assumed:

```text
$ scripts/acceptance-rss.sh --pid $$ --duration 5 --min-samples 3 --out attach-final.json
RSS sampler: mode=attach platform=darwin source=ps-rss pid=3281 identity=pid comm=/bin/zsh
RSS sampler: 6 sample(s) at 1s interval (observed 0.994s) over 5.104s (duration), tree=2 process(es)
RSS sampler: peak=5.18MB ceiling=500MB target=300MB steady=3.65MB p50=4.32MB withinTarget=True
RSS VERDICT: PASS (exit 0)
```

`processes` = `[{"pid":3281,"comm":"/bin/zsh","rssKB":2336},{"pid":3308,"comm":"bash","rssKB":1232}]`,
`excludedOverhead` = `[3314, 3420]` (the python3 sampler and its `/bin/ps` helper).

### 4.3 Negative controls

| Control | Command | Result |
|---|---|---|
| Ceiling breach | `--port 13771 --ceiling-mb 5 --target-mb 5` | `FAIL`, exit 1: `peaked at 20.27 MB, above the 5 MB hard ceiling` |
| Above-target band | `--port 13771 --target-mb 1` (ceiling 500) | `PASS`, exit 0, `withinTarget:false`, NOTE «above the 1 MB target» |
| Unknown pid | `--pid 999999999` | `UNMEASURABLE`, exit 2: `pid 999999999 does not exist` + `no RSS sample was taken` |
| Dead port | `--port 13779` (nothing listening) | `UNMEASURABLE`, exit 2: `cannot read http://127.0.0.1:13779/v1/health` |
| Proxied port | test-only proxy on 13772 forwarding `/v1/health` to the fixture | `UNMEASURABLE`, exit 2: `/v1/health says pid 14140, lsof listener(s) [14337]; refusing to sample a proxied/foreign process` |
| Both `--pid` and `--port` | — | exit 2 (`give either --pid or --port, not both`) |
| Unknown flag / non-integer window | — | exit 2 (`unknown argument`, e.g. `--duration 10s` is accepted, `--duration x` is not) |
| Target dies mid-window | `--pid <shell>` that is SIGKILLed after 1.5s of a 5s window | exit 2: `root pid … is not in the process table (already gone?)` + `only 1 sample(s); the protocol needs >= 3` — partial peak is not certified |
| Unreachable PostgreSQL in profile mode | `--profile` with psql absent and the docker daemon unresponsive | exit 2 after the bounded probe: `no reachable PostgreSQL: install psql or start the dockerized keel-pg (docker daemon probe failed)` (does not hang; probe is watchdog-bounded) |
| Window shorter than `--min-samples` | `--duration 2 --min-samples 5` | exit 2 before sampling: `--duration 2 with --interval 1 yields ~3 sample(s); raise --duration or lower --min-samples` |

### 4.4 Workload validation (profile merge) with a real sample + fixture bench

The RSS value below is a real sample; the bench document is a fixture used only to
exercise the workload validator (it is **not** presented as a product measurement).

```text
# expected 1000 sessions / 64 runs / 3s, bench reports 1000/0 SSE, 64 texts, 20 images, runsActiveMax 64
RSS sampler: workload sessions=1000 sseSessions=1000 runsActiveMax=64 durationSec=3.0 texts=64 imagesOK=20 hardErrors=0
RSS VERDICT: PASS (exit 0)

# same RSS, quiet bench (0 SSE, 0 texts, 0 images, 0 runs, 5s "duration")
RSS sampler: established SSE sessions = 0, want 1000
RSS sampler: texts = 0, want > 0 (the load was idle)
RSS sampler: imagesOK = 0, want > 0 (the image channel was never exercised)
RSS sampler: runsActiveMax = 0, want >= 64 (run concurrency was never reached)
RSS VERDICT: FAIL (exit 1)

# a sampler window shorter than the load
RSS sampler: sampling window 3.046s does not cover 54.0s of the 60s load
RSS sampler: samples = 4, want >= 53 at a 1s interval (sampler did not cover the load)
RSS VERDICT: FAIL (exit 1)
```

## 5. `acceptance-all.sh` wiring (opt-in, default SKIP)

Segment (§8b) isolated and run with the real `run`/`say` helpers from `acceptance-all.sh`:

```text
$ KEEL_ACCEPTANCE_RSS=0 ...            # default
SKIP: VmRSS process-tree gate (set KEEL_ACCEPTANCE_RSS=1 for the 1000-session profile, or =attach with KEEL_RSS_PORT/KEEL_RSS_PID)

$ KEEL_ACCEPTANCE_RSS=attach KEEL_RSS_PORT=13771 ...
-- VmRSS process-tree gate (attach)
PASS: VmRSS process-tree gate (attach) (/tmp/.../perf/rss-gate.log)

$ KEEL_ACCEPTANCE_RSS=attach KEEL_RSS_PORT=13772   # proxied port
FAIL: VmRSS process-tree gate (attach) — RSS VERDICT: UNMEASURABLE (exit 2)

$ KEEL_ACCEPTANCE_RSS=1 ...                        # this host: no PostgreSQL
-- VmRSS process-tree gate (profile)
FAIL: VmRSS process-tree gate (profile) — RSS GATE FATAL: no reachable PostgreSQL: install psql or start the dockerized keel-pg (docker daemon probe failed)
```

The profile segment failing here is the intended behaviour: an opted-in gate that cannot
measure fails the battery instead of skipping.

- `KEEL_ACCEPTANCE_RSS=1` runs `scripts/acceptance-rss.sh --profile` with
  `KEEL_RSS_DB=keel_perf_rss` and `KEEL_RSS_OUT=$KEEL_EVIDENCE_DIR/perf/rss-gate.json`.
- `KEEL_ACCEPTANCE_RSS=attach` requires `KEEL_RSS_PORT` or `KEEL_RSS_PID`; a missing one is
  a battery failure, not a skip.
- Any other value (including the default `0`) prints an explicit SKIP line: the local
  battery stays light, and the gate is never silently dropped from a documented run.
- The segment removes `perf/rss-gate.json` before running, so a failed opted-in gate cannot
  leave the previous run's measurement in the evidence directory.

## 6. Reported gap (why there is no 1000-session RSS number in this file)

The profile mode (native runtime + `cmd/loadtest`, 1000 SSE / 64 runs / 1MiB images) needs
PostgreSQL. On this host (2026-11-23):

- `psql` is not installed;
- `docker ps`/`docker exec` hang and `/_ping` on the Docker socket answers
  `Docker Desktop is unable to start`; port 13741 (the dockerized `keel-pg`) is refused.

So the profile path was exercised only up to its fail-closed boundary (§4.3 last row),
and **no product RSS number is claimed**. The sampler's correctness is proven by the
fixture suite plus live host-process runs; the 1000-session number must be captured on a
host with reachable PostgreSQL (or a native PG):

```bash
KEEL_TEST_DSN='postgres://…' scripts/acceptance-rss.sh --profile \
  --out <evidence>/rss-profile.json
# or via the battery:
KEEL_ACCEPTANCE_RSS=1 KEEL_TEST_DSN='postgres://…' KEEL_EVIDENCE_DIR=<dir> \
  scripts/acceptance-all.sh
```

`totals.peakMB` from that run is the number to quote for W11 G4's RSS half, together
with `source` and `sampling` (the spec requires the interval and process list to be stated).

Deliberately not done here (out of this slice, still open for G4): per-group decomposition
(group workers do not exist yet in the measured tree), group OOM non-propagation, front-end
surface VM quota assertion. G4 stays unchecked on the board.

## 7. Self-review notes

- `tasks.md` untouched (maintainer owns the board); no Go/web/schema/plugin files changed.
- Exit codes are 0/1/2 with no third state; every failure path prints the reason to stderr
  and the same JSON to stdout/`--out`.
- `--interval` capped at 10s (protocol), `--min-samples` ≥ 2, a duration-only window with
  fewer than `--min-samples` samples is UNMEASURABLE.
- `python3 -I` everywhere a script is executed, so the sampler never imports a planted
  module from its directory or user site-packages; fixtures run without starting a process
  or touching the network.
- Profile mode cleans what it starts: sampler killed, runtime SIGTERM→SIGKILL, database dropped,
  port re-checked; the cleanup runs on the error path as well (trap).
- Live fixture processes (`fake_backend.py`, `proxy.py` on 13771/13772) were stopped and
  both ports verified free; no `keel-rss-*` binary process remains.

## 8. Reproduce

```bash
bash scripts/acceptance-rss.sh --self-test                      # fixtures, no process
scripts/acceptance-rss.sh --port 13740 --duration 10s           # attach to a running backend
scripts/acceptance-rss.sh --pid <pid> --duration 10s --out /tmp/x.json
KEEL_TEST_DSN='postgres://…' scripts/acceptance-rss.sh --profile
KEEL_TEST_DSN='postgres://…' KEEL_ACCEPTANCE_RSS=1 KEEL_EVIDENCE_DIR=<dir> scripts/acceptance-all.sh
```
