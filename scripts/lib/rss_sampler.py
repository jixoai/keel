#!/usr/bin/env python3
"""VmRSS process-tree sampler and budget gate for the Keel backend.

Measurement protocol (openspec/changes/keel-harness-v2 acceptance §0; the W0
RSS sampler item and W11 G4):

  RSS = sum of ``VmRSS`` over the backend process tree (root + all descendants),
  sampled at a fixed interval <= 10s. ``VmRSS`` excludes page cache.

Sources:
  * Linux: ``/proc/<pid>/status:VmRSS`` (kB). The process table from ``ps``
    supplies the tree links; each member's VmRSS is read from procfs. A member
    whose status is unreadable (permission) fails closed; a member that exits
    between the ps snapshot and the read contributes 0 and is recorded.
  * darwin (or any platform without procfs): ``ps`` resident set (KiB). macOS
    has no ``/proc`` and no ``VmRSS`` field, so ``ps`` RSS is the documented
    platform equivalent; ``ps`` RSS does not include page cache either. The
    summary names the source, so a value is never mistaken for a Linux reading.

Thresholds (decimal MB, 1e6 bytes, matching the existing cgroup gates):
  * peak > ceiling (default 500MB)            -> FAIL, exit 1
  * target (300MB) < peak <= ceiling          -> PASS, ``withinTarget: false``
                                                (the spec's "above target" band)
  * peak <= target                            -> PASS, ``withinTarget: true``

Fail-closed rules: an unmeasurable tree (no ``/proc`` and no ``ps``, root gone,
unreadable non-zombie member, zero bytes), or a tree that disappears mid-window,
yields ``verdict: UNMEASURABLE`` and exit 2. There is no path that reports PASS
without at least one real, complete sample.

Target identity: by PID, or by PORT. The port path asks the service's own
``/v1/health`` for its pid and cross-checks the TCP listener with ``lsof``; a
host-side container proxy or a stale foreign process therefore fails closed
instead of silently becoming the measured tree. Unrelated processes are never
summed.

This module is importable without side effects. Run it as a script with
``python3 -I`` so user site-packages and the script directory stay out of
``sys.path``.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import statistics
import subprocess
import sys
import time
import urllib.request
from collections import defaultdict

SCHEMA = "keel.rss-sampler.v1"

EXIT_PASS = 0
EXIT_BREACH = 1
EXIT_UNMEASURABLE = 2

MB = 1_000_000
DEFAULT_CEILING_MB = 500
DEFAULT_TARGET_MB = 300
DEFAULT_INTERVAL_SEC = 1
DEFAULT_MIN_SAMPLES = 3
MAX_INTERVAL_SEC = 10

SOURCE_NOTE = {
    "proc-status": "/proc/<pid>/status:VmRSS (kB); VmRSS excludes page cache",
    "ps-rss": (
        "ps resident set (KiB); macOS has no /proc/VmRSS, so ps RSS is the "
        "platform equivalent (page cache is not included)"
    ),
}


class Unmeasurable(Exception):
    """The target tree cannot be measured; never turn this into a PASS."""


class IdentityMismatch(Exception):
    """The port does not belong to the process that answered /v1/health."""


def _kv_field(text: str, key: str) -> str | None:
    for line in text.splitlines():
        name, sep, value = line.partition(":")
        if sep and name.strip() == key:
            return value.strip()
    return None


def parse_proc_status(text: str, pid: int | None = None) -> dict:
    """Parse a Linux ``/proc/<pid>/status`` document.

    ``rssKB`` is ``None`` when the document has no ``VmRSS`` line (zombies omit
    it). Whether that is acceptable is decided against the ps state in
    :func:`measure_tree`; a live member without VmRSS fails the measurement.
    """
    name = _kv_field(text, "Name")
    if name is None:
        raise Unmeasurable("status has no Name line")
    ppid_raw = _kv_field(text, "PPid")
    if ppid_raw is None or not ppid_raw.isdigit():
        raise Unmeasurable(f"status has no numeric PPid line ({ppid_raw!r})")
    state = (_kv_field(text, "State") or "").split(" ")[0]
    rss_raw = _kv_field(text, "VmRSS")
    rss_kb = None
    if rss_raw is not None:
        parts = rss_raw.split()
        if not parts or not parts[0].isdigit():
            raise Unmeasurable(f"VmRSS is not a number ({rss_raw!r})")
        unit = parts[1].lower() if len(parts) > 1 else "kb"
        if unit != "kb":
            raise Unmeasurable(f"VmRSS unit is {unit!r}, expected kB")
        rss_kb = int(parts[0])
    return {
        "pid": pid,
        "ppid": int(ppid_raw),
        "comm": name,
        "state": state,
        "rssKB": rss_kb,
    }


def parse_ps_table(text: str) -> list[dict]:
    """Parse ``ps -A -o pid=,ppid=,rss=,state=,comm=`` output.

    ``comm`` may contain spaces (macOS prints executable paths), so only the
    first four columns are split strictly. Any malformed row raises: a silently
    dropped row could understate the tree.
    """
    rows = []
    for line in text.splitlines():
        if not line.strip():
            continue
        fields = line.split(None, 4)
        if len(fields) < 5:
            raise Unmeasurable(f"unparseable ps row: {line!r}")
        pid, ppid, rss, state, comm = fields
        if not (pid.isdigit() and ppid.isdigit() and rss.isdigit()):
            raise Unmeasurable(f"non-numeric ps row: {line!r}")
        rows.append({
            "pid": int(pid),
            "ppid": int(ppid),
            "rssKB": int(rss),
            "state": state,
            "comm": comm.strip(),
        })
    if not rows:
        raise Unmeasurable("ps produced no rows")
    return rows


def snapshot_from_ps() -> dict[int, dict]:
    """Return the process table keyed by pid (ps RSS in KiB)."""
    ps = shutil.which("ps")
    if ps is None:
        raise Unmeasurable("ps not found on PATH")
    try:
        proc = subprocess.run(
            [ps, "-A", "-o", "pid=,ppid=,rss=,state=,comm="],
            capture_output=True,
            text=True,
            timeout=15,
            env={**os.environ, "LC_ALL": "C"},
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise Unmeasurable(f"ps failed: {exc}") from exc
    if proc.returncode != 0:
        raise Unmeasurable(f"ps exited {proc.returncode}: {proc.stderr.strip()[:200]}")
    return {row["pid"]: row for row in parse_ps_table(proc.stdout)}


def snapshot_from_proc(proc_root: str = "/proc") -> tuple[dict[int, dict], dict[int, str]]:
    """Read ``/proc/<pid>/status`` for every readable process (VmRSS in kB).

    Returns ``(entries, unreadable)``. An unreadable row cannot be proven to be
    outside the target tree, so :func:`sample_once` refuses to certify a tree
    built only from procfs when any row was unreadable.
    """
    try:
        names = os.listdir(proc_root)
    except OSError as exc:
        raise Unmeasurable(f"cannot list {proc_root}: {exc}") from exc
    entries: dict[int, dict] = {}
    unreadable: dict[int, str] = {}
    for name in names:
        if not name.isdigit():
            continue
        pid = int(name)
        path = os.path.join(proc_root, name, "status")
        try:
            with open(path, encoding="utf-8", errors="replace") as fh:
                entries[pid] = parse_proc_status(fh.read(), pid)
        except (OSError, Unmeasurable) as exc:
            unreadable[pid] = str(exc)
            continue
    if not entries:
        raise Unmeasurable(f"no readable processes under {proc_root}")
    return entries, unreadable


def read_vmrss(pid: int, proc_root: str = "/proc") -> int | None:
    """Read one member's VmRSS in kB.

    Returns ``None`` when the process exited between the ps snapshot and this
    read (its memory is gone, so it adds 0 to the sum). Permission errors and
    documents without VmRSS fail closed: an unreadable live member would
    understate the tree.
    """
    path = os.path.join(proc_root, str(pid), "status")
    try:
        with open(path, encoding="utf-8", errors="replace") as fh:
            row = parse_proc_status(fh.read(), pid)
    except FileNotFoundError:
        return None
    except PermissionError as exc:
        raise Unmeasurable(f"cannot read /proc/{pid}/status: {exc}") from exc
    except OSError as exc:
        raise Unmeasurable(f"cannot read /proc/{pid}/status: {exc}") from exc
    if row["rssKB"] is None:
        raise Unmeasurable(f"/proc/{pid}/status has a live state but no VmRSS reading")
    return row["rssKB"]


def tree_members(entries: dict[int, dict], root: int) -> list[int]:
    """Return root + all descendants present in the process table."""
    if root not in entries:
        raise Unmeasurable(f"root pid {root} is not in the process table (already gone?)")
    children: dict[int, list[int]] = defaultdict(list)
    for pid, row in entries.items():
        children[row["ppid"]].append(pid)
    members: list[int] = []
    seen: set[int] = set()
    stack = [root]
    while stack:
        pid = stack.pop()
        if pid in seen:
            continue
        seen.add(pid)
        members.append(pid)
        stack.extend(children.get(pid, []))
    return members


def is_zombie(state: str) -> bool:
    return state[:1] == "Z"


def measure_tree(entries: dict[int, dict], root: int, vmrss=None, overhead_root: int | None = None) -> dict:
    """Sum resident bytes over one snapshot's tree.

    ``vmrss(pid)`` overrides the per-row ``rssKB`` (the Linux path reads it from
    procfs per member). ``overhead_root`` (the sampler's own pid) and its
    subtree are measurement cost, not workload: they are excluded and recorded
    in ``excludedOverhead`` so a ``--pid`` that happens to be an ancestor of the
    sampler cannot inflate the total. Zombies are recorded, never counted. A
    live member without a reading makes the sum incomplete and fails the
    measurement.
    """
    members = tree_members(entries, root)
    excluded: set[int] = set()
    if overhead_root is not None and overhead_root != root and overhead_root in entries:
        excluded = set(tree_members(entries, overhead_root))
    processes = []
    zombies = []
    exited = []
    skipped = []
    for pid in members:
        if pid in excluded:
            skipped.append(pid)
            continue
        row = entries[pid]
        if is_zombie(row["state"]):
            zombies.append(pid)
            continue
        if vmrss is None:
            rss_kb = row["rssKB"]
            comm = row["comm"]
        else:
            rss_kb = vmrss(pid)
            comm = row["comm"]
            if rss_kb is None:
                exited.append(pid)
                continue
        if rss_kb is None:
            raise Unmeasurable(f"pid {pid} ({comm}) has no VmRSS reading")
        processes.append({
            "pid": pid,
            "ppid": row["ppid"],
            "comm": comm,
            "rssKB": rss_kb,
            "rssBytes": rss_kb * 1024,
        })
    if not processes:
        raise Unmeasurable("process tree has no measurable (non-zombie) member")
    total = sum(p["rssBytes"] for p in processes)
    if total <= 0:
        raise Unmeasurable("process tree reports zero resident bytes")
    processes.sort(key=lambda p: (-p["rssBytes"], p["pid"]))
    return {
        "totalBytes": total,
        # memberCount counts what was actually summed; zombies and the sampler's
        # own subtree are reported separately instead of inflating the tree.
        "memberCount": len(processes),
        "zombiePids": zombies,
        "exitedDuringSample": exited,
        "excludedOverhead": skipped,
        "processes": processes,
        "sampledAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }


def platform_source() -> tuple[str, str]:
    """Return (platform, measurement source) or raise Unmeasurable."""
    name = "linux" if sys.platform.startswith("linux") else sys.platform
    if sys.platform.startswith("linux") and os.path.isdir("/proc"):
        return name, "proc-status"
    if shutil.which("ps") is not None:
        return name, "ps-rss"
    raise Unmeasurable(f"no /proc and no ps on {sys.platform}: resident memory is unmeasurable")


def sample_once(root: int) -> dict:
    platform, source = platform_source()
    if source == "proc-status":
        # ps is the preferred tree truth: it lists processes whose status may
        # be unreadable, so a hidden child cannot silently vanish from the sum.
        # Only when ps is missing or unparseable (busybox) does the proc walk
        # become the tree; that walk is certifiable only if every /proc row was
        # readable -- otherwise a hidden descendant cannot be ruled out and the
        # sample fails closed.
        try:
            entries = snapshot_from_ps()
            tree_source = "ps+proc"
        except Unmeasurable as ps_exc:
            try:
                entries, unreadable = snapshot_from_proc()
            except Unmeasurable as proc_exc:
                raise Unmeasurable(
                    f"no usable process table: {ps_exc}; {proc_exc}"
                ) from ps_exc
            if unreadable:
                raise Unmeasurable(
                    f"cannot enumerate hidden children ({len(unreadable)} unreadable /proc row(s), "
                    f"ps unavailable: {ps_exc}); install procps ps or run as root"
                ) from ps_exc
            tree_source = "proc-only"
        snapshot = measure_tree(entries, root, vmrss=read_vmrss, overhead_root=os.getpid())
        snapshot["treeSource"] = tree_source
    else:
        snapshot = measure_tree(snapshot_from_ps(), root, overhead_root=os.getpid())
        snapshot["treeSource"] = "ps"
    snapshot["platform"] = platform
    snapshot["source"] = source
    return snapshot


def pid_exists(pid: int) -> bool:
    if pid <= 0:
        return False
    if os.path.isdir(f"/proc/{pid}"):
        return True
    ps = shutil.which("ps")
    if ps is None:
        return False
    try:
        proc = subprocess.run(
            [ps, "-o", "pid=", "-p", str(pid)],
            capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return proc.returncode == 0 and proc.stdout.strip().isdigit()


def resolve_port(port: int, host: str, timeout: float = 3.0) -> tuple[int, str]:
    """Resolve a port to the backend pid and verify the listener identity.

    The service's own ``/v1/health`` is the authority for "which backend is
    this"; ``lsof`` cross-checks that the socket on this port belongs to the
    same pid. With a host-side container proxy the two differ, and sampling the
    proxy would measure the wrong tree -- that fails closed and tells the
    caller to run inside the container with ``--pid``.
    """
    url = f"http://{host}:{port}/v1/health"
    # An explicit no-proxy opener: a local Fake-IP/proxy environment must not
    # reroute a loopback identity probe.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(url, timeout=timeout) as response:
            body = json.loads(response.read().decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - every failure is an identity failure
        raise IdentityMismatch(f"cannot read {url}: {exc}") from exc
    health_pid = body.get("pid") if isinstance(body, dict) else None
    if not isinstance(health_pid, int) or health_pid <= 0:
        raise IdentityMismatch(f"{url} returned no usable pid ({body!r})")
    if not pid_exists(health_pid):
        raise IdentityMismatch(f"{url} answered pid {health_pid}, but that process does not exist here")
    lsof = shutil.which("lsof")
    if lsof is None:
        return health_pid, "health-only (lsof unavailable; pid from the service's own /v1/health)"
    try:
        proc = subprocess.run(
            [lsof, "-nP", f"-iTCP:{port}", "-sTCP:LISTEN", "-t"],
            capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.SubprocessError) as exc:
        raise IdentityMismatch(f"lsof failed: {exc}") from exc
    listeners = sorted({int(x) for x in proc.stdout.split() if x.strip().isdigit()})
    if not listeners:
        raise IdentityMismatch(
            f"{url} answered pid {health_pid}, but lsof sees no listener on port {port}"
        )
    if health_pid not in listeners:
        raise IdentityMismatch(
            f"port {port}: /v1/health says pid {health_pid}, lsof listener(s) {listeners}; "
            "refusing to sample a proxied/foreign process (run inside the container with --pid)"
        )
    if len(listeners) > 1:
        raise IdentityMismatch(f"port {port} has multiple listeners {listeners}; refusing an ambiguous target")
    return health_pid, "health+lsof"


def collect_samples(root: int, duration: float, interval: float, stop_file: str | None = None) -> dict:
    """Sample until the window elapses or ``stop_file`` appears.

    Any measurement error after a successful sample ends the window with that
    error recorded; the caller turns it into UNMEASURABLE, never into a PASS
    (a backend that vanishes mid-window has no certifiable peak).
    """
    started = time.monotonic()
    samples: list[dict] = []
    errors: list[str] = []
    sample_times: list[float] = []
    stop_reason = "duration"
    tick = 0
    while True:
        try:
            snapshot = sample_once(root)
        except Unmeasurable as exc:
            errors.append(str(exc))
            break
        samples.append(snapshot)
        sample_times.append(time.monotonic() - started)
        elapsed = sample_times[-1]
        if stop_file and os.path.exists(stop_file):
            stop_reason = "stop-file"
            break
        if elapsed >= duration:
            break
        # Absolute deadlines: a slow ps must not compound into interval drift
        # (tick n is due at n*interval from the window start). A missed
        # deadline samples again as soon as possible (0.05s floor); the real
        # spacing is recorded in observedIntervalSec either way.
        tick += 1
        if tick * interval > duration + 1e-9:
            break  # the next sample would land outside the window
        wait = tick * interval - elapsed
        time.sleep(max(0.05, wait))
    gaps = [b - a for a, b in zip(sample_times, sample_times[1:])]
    return {
        "samples": samples,
        "errors": errors,
        "stopReason": stop_reason,
        "durationSec": round(time.monotonic() - started, 3),
        # The achieved spacing: a host whose ps/sampling costs more than the
        # requested interval reports it, so "sampled every 1s" is never assumed.
        "observedIntervalSec": round(statistics.median(gaps), 3) if gaps else None,
    }


def _mb(value_bytes: int | float) -> float:
    return round(value_bytes / MB, 2)


def build_summary(
    *,
    mode: str,
    platform: str,
    source: str,
    target: dict,
    collected: dict,
    ceiling_mb: int = DEFAULT_CEILING_MB,
    target_mb: int = DEFAULT_TARGET_MB,
    steady_samples: int = 3,
    min_samples: int = DEFAULT_MIN_SAMPLES,
    workload: dict | None = None,
) -> dict:
    """Build the machine-readable summary plus its verdict and exit code.

    Precedence: an incomplete measurement is UNMEASURABLE (exit 2) even if the
    partial peak would breach -- a breach that was not completely measured
    cannot be certified either; a finished window over the ceiling is FAIL
    (exit 1).
    """
    samples = collected.get("samples", [])
    errors = collected.get("errors", [])
    ceiling_bytes = ceiling_mb * MB
    target_bytes = target_mb * MB
    totals = [s["totalBytes"] for s in samples]
    last = samples[-1] if samples else None
    if last is not None and target.get("comm") is None:
        for proc in last["processes"]:
            if proc["pid"] == target.get("pid"):
                target["comm"] = proc["comm"]
                break

    measurement_failures = [f"measurement aborted: {e}" for e in errors]
    threshold_failures: list[str] = []
    notes: list[str] = []
    if not totals:
        measurement_failures.append("no RSS sample was taken; refusing to certify an unknown value")
    elif collected.get("stopReason") == "duration" and len(totals) < min_samples:
        # A window that produced fewer samples than the protocol demands cannot
        # certify a peak: the interval/stop logic missed its own schedule.
        measurement_failures.append(
            f"only {len(totals)} sample(s); the protocol needs >= {min_samples} for a window"
        )

    summary = {
        "schema": SCHEMA,
        "mode": mode,
        "platform": platform,
        "source": source,
        "sourceNote": SOURCE_NOTE.get(source, source),
        "target": target,
        "workload": workload,
        "sampling": {
            "intervalSec": collected.get("intervalSec"),
            "observedIntervalSec": collected.get("observedIntervalSec"),
            "requestedDurationSec": collected.get("requestedDurationSec"),
            "actualDurationSec": collected.get("durationSec"),
            "samples": len(samples),
            "sampleErrors": errors,
            "stopReason": collected.get("stopReason"),
            "sampledAt": last["sampledAt"] if last else None,
        },
        "processes": last["processes"] if last else [],
        "processTree": {
            "memberCount": last["memberCount"] if last else 0,
            "zombiePids": last["zombiePids"] if last else [],
            "exitedDuringSample": last["exitedDuringSample"] if last else [],
            "excludedOverhead": last.get("excludedOverhead", []) if last else [],
        },
        "totals": None,
        "thresholds": {
            "ceilingMB": ceiling_mb,
            "targetMB": target_mb,
            "withinTarget": None,
        },
        "verdict": "UNMEASURABLE",
        "exitCode": EXIT_UNMEASURABLE,
        "failures": [],
        "notes": [],
    }

    if totals:
        peak = max(totals)
        steady = statistics.fmean(totals[-max(1, steady_samples):])
        within = peak <= target_bytes
        breached = peak > ceiling_bytes
        summary["totals"] = {
            "peakMB": _mb(peak),
            "peakBytes": peak,
            "minMB": _mb(min(totals)),
            "p50MB": _mb(statistics.median(totals)),
            "steadyMB": _mb(steady),
            "lastMB": _mb(totals[-1]),
        }
        summary["thresholds"]["withinTarget"] = within
        if breached:
            threshold_failures.append(
                f"peaked at {_mb(peak):.2f} MB, above the {ceiling_mb} MB hard ceiling"
            )
        elif not within:
            notes.append(
                f"peak {_mb(peak):.2f} MB is inside the {ceiling_mb} MB ceiling but above the "
                f"{target_mb} MB target; recorded as above-target pass"
            )
        else:
            notes.append(f"peak {_mb(peak):.2f} MB is within the {target_mb} MB target")

    summary["failures"] = measurement_failures + threshold_failures
    summary["notes"] = notes
    if measurement_failures:
        summary["verdict"] = "UNMEASURABLE"
        summary["exitCode"] = EXIT_UNMEASURABLE
    elif threshold_failures:
        summary["verdict"] = "FAIL"
        summary["exitCode"] = EXIT_BREACH
    else:
        summary["verdict"] = "PASS"
        summary["exitCode"] = EXIT_PASS
    return summary


def merge_workload(
    summary: dict,
    bench: dict | None,
    *,
    bench_error: str | None = None,
    expected_sessions: int,
    expected_runs: int,
    expected_duration_sec: float,
    require_images: bool = True,
    min_ratio: float = 0.9,
) -> dict:
    """Attach the observed workload to the RSS summary and gate its validity.

    A quiet run would otherwise pass the RSS gate on a near-empty backend: the
    load must prove it actually reached the documented profile (sessions, run
    concurrency, duration, text and image traffic, zero hard SSE errors), and
    the sampler window must cover at least 90% of the load window.
    """
    merged = dict(summary)
    failures: list[str] = []
    observed: dict = {}
    if bench_error is not None:
        failures.append(f"loadtest result unreadable: {bench_error}")
    elif not isinstance(bench, dict):
        failures.append("loadtest result is not a JSON object")
    else:
        status = bench.get("statusCounts") or {}
        throttled = int(status.get("429", 0))
        sse_errors = int(bench.get("sseErrors", 0))
        hard = sse_errors - throttled
        observed = {
            "sessions": bench.get("sessions"),
            "sseSessions": bench.get("sseSessions"),
            "durationSec": bench.get("durationSec"),
            "texts": bench.get("texts"),
            "imagesOK": bench.get("imagesOK"),
            "imagesErr": bench.get("imagesErr"),
            "runsActiveMax": bench.get("runsActiveMax"),
            "runsPendingMax": bench.get("runsPendingMax"),
            "streamsActiveMax": bench.get("streamsActiveMax"),
            "sseErrors": sse_errors,
            "statusCounts429": throttled,
            "hardErrors": hard,
            "drainWaitSec": bench.get("drainWaitSec"),
            "throughputMBs": bench.get("throughputMBs"),
        }
        if observed["sessions"] != expected_sessions:
            failures.append(
                f"loadtest sessions = {observed['sessions']}, want {expected_sessions} (empty-run guard)"
            )
        if observed["sseSessions"] != expected_sessions:
            failures.append(
                f"established SSE sessions = {observed['sseSessions']}, want {expected_sessions}"
            )
        duration = observed["durationSec"]
        if not isinstance(duration, (int, float)) or duration < min_ratio * expected_duration_sec:
            failures.append(
                f"loadtest durationSec = {duration}, want >= {min_ratio * expected_duration_sec:.1f} "
                f"of {expected_duration_sec:.0f}s (short run)"
            )
        if not isinstance(observed["texts"], int) or observed["texts"] <= 0:
            failures.append(f"texts = {observed['texts']}, want > 0 (the load was idle)")
        if require_images and (not isinstance(observed["imagesOK"], int) or observed["imagesOK"] <= 0):
            failures.append(
                f"imagesOK = {observed['imagesOK']}, want > 0 (the image channel was never exercised)"
            )
        if not isinstance(observed["runsActiveMax"], int) or observed["runsActiveMax"] < expected_runs:
            failures.append(
                f"runsActiveMax = {observed['runsActiveMax']}, want >= {expected_runs} "
                "(run concurrency was never reached)"
            )
        if hard != 0:
            failures.append(
                f"hard SSE errors = {hard} (sseErrors {sse_errors} - 429 {throttled}), want 0"
            )

    sampling = merged.get("sampling") or {}
    actual = sampling.get("actualDurationSec")
    samples = sampling.get("samples")
    interval = sampling.get("intervalSec") or DEFAULT_INTERVAL_SEC
    if not isinstance(actual, (int, float)) or actual < min_ratio * expected_duration_sec:
        failures.append(
            f"sampling window {actual}s does not cover {min_ratio * expected_duration_sec:.1f}s "
            f"of the {expected_duration_sec:.0f}s load"
        )
    wanted_samples = max(2, int((min_ratio * expected_duration_sec) // interval) - 1)
    if not isinstance(samples, int) or samples < wanted_samples:
        failures.append(
            f"samples = {samples}, want >= {wanted_samples} at a {interval}s interval "
            "(sampler did not cover the load)"
        )

    merged["workloadObserved"] = observed
    merged["workloadCheck"] = {
        "pass": not failures,
        "expected": {
            "sessions": expected_sessions,
            "concurrentRuns": expected_runs,
            "durationSec": expected_duration_sec,
            "imagesRequired": require_images,
        },
        "failures": failures,
    }
    merged["failures"] = list(merged.get("failures") or []) + failures
    if merged.get("verdict") == "UNMEASURABLE":
        # No complete RSS measurement: exit 2 stays authoritative; workload
        # problems are reported but do not downgrade the measurement verdict.
        pass
    elif failures:
        merged["verdict"] = "FAIL"
        merged["exitCode"] = EXIT_BREACH
    return merged


def human_lines(summary: dict) -> list[str]:
    lines = []
    target = summary.get("target") or {}
    where = []
    if target.get("pid") is not None:
        where.append(f"pid={target['pid']}")
    if target.get("port") is not None:
        where.append(f"port={target['port']}")
    if target.get("identity"):
        where.append(f"identity={target['identity']}")
    if target.get("comm"):
        where.append(f"comm={target['comm']}")
    lines.append(
        f"RSS sampler: mode={summary['mode']} platform={summary['platform']} "
        f"source={summary['source']} {' '.join(where)}"
    )
    sampling = summary["sampling"]
    observed = sampling.get("observedIntervalSec")
    spacing = f" (observed {observed}s)" if observed is not None else ""
    lines.append(
        f"RSS sampler: {sampling['samples']} sample(s) at {sampling['intervalSec']}s interval"
        f"{spacing} over {sampling['actualDurationSec']}s ({sampling['stopReason']}), "
        f"tree={summary['processTree']['memberCount']} process(es)"
    )
    totals = summary.get("totals")
    thresholds = summary["thresholds"]
    if totals:
        lines.append(
            f"RSS sampler: peak={totals['peakMB']}MB ceiling={thresholds['ceilingMB']}MB "
            f"target={thresholds['targetMB']}MB steady={totals['steadyMB']}MB p50={totals['p50MB']}MB "
            f"withinTarget={thresholds['withinTarget']}"
        )
    else:
        lines.append(f"RSS sampler: no usable sample (ceiling={thresholds['ceilingMB']}MB)")
    observed = summary.get("workloadObserved") or {}
    check = summary.get("workloadCheck")
    if check:
        lines.append(
            "RSS sampler: workload sessions={sessions} sseSessions={sseSessions} "
            "runsActiveMax={runsActiveMax} durationSec={durationSec} texts={texts} imagesOK={imagesOK} "
            "hardErrors={hardErrors}".format(**{
                key: observed.get(key, "?") for key in (
                    "sessions", "sseSessions", "runsActiveMax", "durationSec",
                    "texts", "imagesOK", "hardErrors",
                )
            })
        )
    for failure in summary["failures"]:
        lines.append(f"RSS sampler: {failure}")
    for note in summary.get("notes") or []:
        lines.append(f"RSS sampler: NOTE: {note}")
    lines.append(f"RSS VERDICT: {summary['verdict']} (exit {summary['exitCode']})")
    return lines


def write_json(path: str, document: dict) -> None:
    parent = os.path.dirname(path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    tmp = f"{path}.tmp.{os.getpid()}"
    with open(tmp, "w", encoding="utf-8") as fh:
        json.dump(document, fh, ensure_ascii=False, indent=2)
        fh.write("\n")
    os.replace(tmp, path)


def _positive_int(parser: argparse.ArgumentParser, name: str, value: object) -> int:
    try:
        number = int(value)
    except (TypeError, ValueError):
        parser.error(f"{name} must be an integer (got {value!r})")
    if number <= 0:
        parser.error(f"{name} must be > 0 (got {number})")
    return number


def _sample(args: argparse.Namespace, parser: argparse.ArgumentParser) -> int:
    if (args.pid is None) == (args.port is None):
        parser.error("exactly one of --pid or --port is required")
    if args.interval < 1 or args.interval > MAX_INTERVAL_SEC:
        parser.error(f"--interval must be 1..{MAX_INTERVAL_SEC} seconds (protocol: sample at least every 10s)")
    if args.duration <= 0:
        parser.error("--duration must be > 0")
    if args.ceiling_mb <= 0 or args.target_mb <= 0:
        parser.error("--ceiling-mb and --target-mb must be > 0")
    if args.target_mb > args.ceiling_mb:
        parser.error("--target-mb cannot exceed --ceiling-mb")
    if args.steady_samples <= 0:
        parser.error("--steady-samples must be > 0")
    if args.min_samples < 2:
        parser.error("--min-samples must be >= 2 (a single reading is not a window)")

    target: dict = {"pid": args.pid, "port": args.port, "identity": None, "comm": None}
    workload = None
    if args.workload_sessions is not None or args.workload_load_duration is not None:
        workload = {"sessions": args.workload_sessions, "loadDurationSec": args.workload_load_duration}

    try:
        platform_name, source = platform_source()
    except Unmeasurable as exc:
        platform_name = "linux" if sys.platform.startswith("linux") else sys.platform
        source = "unavailable"
        collected = {
            "samples": [], "errors": [str(exc)], "stopReason": "unmeasurable",
            "durationSec": 0.0, "intervalSec": args.interval,
            "requestedDurationSec": args.duration,
        }
        summary = build_summary(
            mode=args.mode, platform=platform_name, source=source, target=target,
            collected=collected, ceiling_mb=args.ceiling_mb, target_mb=args.target_mb,
            steady_samples=args.steady_samples, min_samples=args.min_samples,
            workload=workload,
        )
        return _emit(summary, args.out)

    collected: dict = {
        "samples": [],
        "errors": [],
        "stopReason": "identity",
        "durationSec": 0.0,
    }
    identity_error = None
    root_pid = args.pid
    if args.port is not None:
        try:
            root_pid, identity = resolve_port(args.port, args.host)
        except IdentityMismatch as exc:
            identity_error = str(exc)
        else:
            target["pid"] = root_pid
            target["identity"] = identity
    else:
        target["identity"] = "pid"
        if not pid_exists(root_pid):
            identity_error = f"pid {root_pid} does not exist"

    if identity_error is None:
        collected = collect_samples(root_pid, args.duration, args.interval, args.stop_file)
        collected["intervalSec"] = args.interval
        collected["requestedDurationSec"] = args.duration
    else:
        collected["errors"] = [identity_error]
        collected["intervalSec"] = args.interval
        collected["requestedDurationSec"] = args.duration

    summary = build_summary(
        mode=args.mode, platform=platform_name, source=source, target=target,
        collected=collected, ceiling_mb=args.ceiling_mb, target_mb=args.target_mb,
        steady_samples=args.steady_samples, min_samples=args.min_samples,
        workload=workload,
    )
    return _emit(summary, args.out)


def _merge(args: argparse.Namespace, parser: argparse.ArgumentParser) -> int:
    try:
        with open(args.summary, encoding="utf-8") as fh:
            summary = json.load(fh)
    except (OSError, ValueError) as exc:
        print(f"merge: cannot read summary {args.summary}: {exc}", file=sys.stderr)
        return EXIT_UNMEASURABLE
    if not isinstance(summary, dict):
        print(f"merge: summary {args.summary} is not a JSON object", file=sys.stderr)
        return EXIT_UNMEASURABLE
    bench = None
    bench_error = None
    try:
        with open(args.bench, encoding="utf-8") as fh:
            bench = json.load(fh)
    except (OSError, ValueError) as exc:
        bench_error = f"{args.bench}: {exc}"
    expected_sessions = _positive_int(parser, "--expected-sessions", args.expected_sessions)
    expected_runs = _positive_int(parser, "--expected-runs", args.expected_runs)
    if args.expected_duration <= 0:
        parser.error("--expected-duration must be > 0")
    merged = merge_workload(
        summary, bench, bench_error=bench_error,
        expected_sessions=expected_sessions, expected_runs=expected_runs,
        expected_duration_sec=args.expected_duration,
        require_images=not args.allow_no_images,
    )
    write_json(args.out, merged)
    for line in human_lines(merged):
        print(line, file=sys.stderr)
    print(json.dumps(merged, ensure_ascii=False))
    return int(merged["exitCode"])


def _emit(summary: dict, out: str | None) -> int:
    if out:
        write_json(out, summary)
    for line in human_lines(summary):
        print(line, file=sys.stderr)
    print(json.dumps(summary, ensure_ascii=False))
    return int(summary["exitCode"])


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="rss_sampler.py",
        description="Sample and enforce the Keel backend process-tree VmRSS budget.",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    sp = sub.add_parser("sample", help="sample a running backend tree")
    sp.add_argument("--pid", type=int, default=None, help="root process id of the backend tree")
    sp.add_argument("--port", type=int, default=None, help="backend port; pid comes from /v1/health")
    sp.add_argument("--host", default="127.0.0.1", help="host for the /v1/health probe")
    sp.add_argument("--duration", type=float, default=10.0, help="sampling window seconds")
    sp.add_argument(
        "--interval", type=int, default=DEFAULT_INTERVAL_SEC,
        help=f"sample interval seconds (1..{MAX_INTERVAL_SEC})",
    )
    sp.add_argument("--ceiling-mb", type=int, default=DEFAULT_CEILING_MB, help="hard ceiling, decimal MB")
    sp.add_argument(
        "--target-mb", type=int, default=DEFAULT_TARGET_MB,
        help="target, decimal MB (above-target is a recorded pass)",
    )
    sp.add_argument("--steady-samples", type=int, default=3, help="trailing samples averaged into steadyMB")
    sp.add_argument("--min-samples", type=int, default=DEFAULT_MIN_SAMPLES,
                    help="minimum samples for a certifiable window")
    sp.add_argument("--stop-file", default=None, help="stop the window early when this file appears")
    sp.add_argument("--mode", default="attach", help="label recorded in the summary (attach|profile)")
    sp.add_argument("--workload-sessions", type=int, default=None, help="workload metadata: SSE sessions")
    sp.add_argument("--workload-load-duration", type=int, default=None, help="workload metadata: load seconds")
    sp.add_argument("--out", default=None, help="write the summary JSON here; stdout also gets one JSON line")

    mp = sub.add_parser("merge", help="attach loadtest evidence and gate its validity")
    mp.add_argument("--summary", required=True, help="the sampler summary JSON")
    mp.add_argument("--bench", required=True, help="the loadtest bench JSON")
    mp.add_argument("--expected-sessions", required=True, help="exact SSE sessions the load must establish")
    mp.add_argument("--expected-runs", required=True, help="concurrent runs the load must reach")
    mp.add_argument("--expected-duration", required=True, type=float, help="requested load seconds")
    mp.add_argument("--allow-no-images", action="store_true", help="do not require the image channel")
    mp.add_argument("--out", required=True, help="merged summary JSON")

    args = parser.parse_args(argv)
    if args.command == "sample":
        return _sample(args, parser)
    return _merge(args, parser)


if __name__ == "__main__":
    sys.exit(main())
