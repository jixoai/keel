#!/usr/bin/env python3
"""Fixture tests for the VmRSS process-tree sampler (scripts/lib/rss_sampler.py).

No process is started and no network is touched: every case feeds synthetic
/proc status documents or ``ps`` tables to the real parser/sum/threshold logic.
Run directly (``python3 -I scripts/lib/test_rss_sampler.py``) or through the
gate wrapper (``scripts/acceptance-rss.sh --self-test``); CI runs the wrapper.

cases:
  * /proc status parsing: VmRSS kB, PPid, State, zombie without VmRSS, bad units
  * ps table parsing: macOS paths with spaces, malformed rows
  * tree walk: root+descendants only (unrelated pids never summed), depth > 1
  * sampler overhead subtree excluded; missing/unreadable members fail closed
  * Linux branch: ps tree + per-member VmRSS; proc-only fallback/refusal
  * thresholds: <=300 on target, 300-500 above-target PASS, >500 FAIL,
    incomplete measurement = UNMEASURABLE even when the partial peak breaches
  * merge gate: quiet/short/empty load and a sampler window that missed the load
  * cadence: absolute deadlines, stop-file early exit, observed spacing
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True  # importing the sampler must not litter __pycache__
HERE = os.path.dirname(os.path.abspath(__file__))
SPEC = importlib.util.spec_from_file_location("rss_sampler", os.path.join(HERE, "rss_sampler.py"))
assert SPEC and SPEC.loader, "rss_sampler.py not found next to the test"
RSS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RSS)


def _status(name: str, pid: int, ppid: int, state: str, rss_kb: int | None) -> str:
    lines = [f"Name:\t{name}", f"Umask:\t0022", f"State:\t{state} (sleeping)", f"Tgid:\t{pid}",
             f"Pid:\t{pid}", f"PPid:\t{ppid}"]
    if rss_kb is not None:
        lines.append(f"VmRSS:\t  {rss_kb} kB")
    return "\n".join(lines) + "\n"


def _table(rows: list[dict]) -> dict[int, dict]:
    return {row["pid"]: row for row in rows}


def _entry(pid: int, ppid: int, rss_kb: int | None, comm: str = "runtime", state: str = "S") -> dict:
    return {"pid": pid, "ppid": ppid, "comm": comm, "state": state, "rssKB": rss_kb}


def _summary(total_bytes: int, *, ceiling_mb: int = 500, target_mb: int = 300,
             samples: int = 3, error: str | None = None, duration: float = 65.0) -> dict:
    """Build a summary from a synthetic tree of exactly ``total_bytes``.

    RSS is reported in KiB, so the fixture divides before building the tree;
    the sum then lands within one KiB of the requested byte total. ``duration``
    is the sampler window (65s by default so it covers a 60s load).
    """
    entries = _table([_entry(100, 1, total_bytes // 1024)])
    out = []
    # A real 1s-interval window yields roughly one sample per second; the merge
    # gate checks that count, so the fixture must look like a real window.
    count = max(samples, max(1, int(duration) - 1))
    for _ in range(count):
        if total_bytes <= 0:
            entries[100]["rssKB"] = 1
        out.append(RSS.measure_tree(entries, 100))
    if total_bytes <= 0:
        # The tree has to fail outright: rebuild the broken snapshot for real.
        out = []
    collected = {"samples": out, "errors": [error] if error else [], "intervalSec": 1,
                 "requestedDurationSec": duration, "durationSec": duration,
                 "stopReason": "duration"}
    return RSS.build_summary(mode="test", platform="darwin", source="ps-rss",
                             target={"pid": 100, "port": None, "identity": "pid", "comm": None},
                             collected=collected, ceiling_mb=ceiling_mb, target_mb=target_mb)


class ProcStatusTests(unittest.TestCase):
    def test_parses_vmrss_ppid_state(self):
        row = RSS.parse_proc_status(_status("runtime", 42, 7, "S", 2048), 42)
        self.assertEqual(row["pid"], 42)
        self.assertEqual(row["ppid"], 7)
        self.assertEqual(row["state"], "S")
        self.assertEqual(row["rssKB"], 2048)
        self.assertEqual(row["comm"], "runtime")

    def test_zombie_without_vmrss_is_identity_only(self):
        row = RSS.parse_proc_status(_status("defunct", 43, 42, "Z", None), 43)
        self.assertIsNone(row["rssKB"])
        self.assertIs(RSS.is_zombie(row["state"]), True)

    def test_missing_vmrss_on_live_process_would_be_rejected_by_tree(self):
        entries = _table([_entry(1, 0, 4096), _entry(2, 1, None)])  # live member, no reading
        with self.assertRaises(RSS.Unmeasurable):
            RSS.measure_tree(entries, 1)

    def test_rejects_bad_unit(self):
        text = _status("runtime", 42, 7, "S", None).replace("PPid:\t7", "PPid:\t7\nVmRSS:\t 12 MB")
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_proc_status(text, 42)

    def test_rejects_missing_ppid_or_name(self):
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_proc_status("Pid: 1\nPPid: 0\n", 1)
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_proc_status("Name: x\nPPid: nope\n", 1)


class ProcRootTests(unittest.TestCase):
    """snapshot_from_proc against a synthetic procfs directory."""

    def test_reads_readable_rows_and_records_unreadable(self):
        with tempfile.TemporaryDirectory() as tmp:
            for pid, ppid, rss in ((100, 1, 10_000), (101, 100, 2_000)):
                path = os.path.join(tmp, str(pid))
                os.mkdir(path)
                with open(os.path.join(path, "status"), "w", encoding="utf-8") as fh:
                    fh.write(_status("runtime", pid, ppid, "S", rss))
            entries, unreadable = RSS.snapshot_from_proc(tmp)
            self.assertEqual(set(entries), {100, 101})
            self.assertEqual(entries[101]["ppid"], 100)
            self.assertEqual(unreadable, {})

    def test_unreadable_row_is_recorded_not_dropped(self):
        with tempfile.TemporaryDirectory() as tmp:
            ok = os.path.join(tmp, "100")
            os.mkdir(ok)
            with open(os.path.join(ok, "status"), "w", encoding="utf-8") as fh:
                fh.write(_status("runtime", 100, 1, "S", 10_000))
            denied = os.path.join(tmp, "101")
            os.mkdir(denied)
            with open(os.path.join(denied, "status"), "w", encoding="utf-8") as fh:
                fh.write(_status("hidden", 101, 100, "S", 999))
            os.chmod(os.path.join(denied, "status"), 0)
            try:
                entries, unreadable = RSS.snapshot_from_proc(tmp)
                if os.geteuid() == 0:
                    self.assertEqual(unreadable, {})  # root reads it anyway
                else:
                    self.assertIn(101, unreadable)
                    self.assertNotIn(101, entries)
            finally:
                os.chmod(os.path.join(denied, "status"), 0o600)

    def test_empty_root_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(RSS.Unmeasurable):
                RSS.snapshot_from_proc(tmp)

    def test_read_vmrss_reads_member_and_reports_exit(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.mkdir(os.path.join(tmp, "100"))
            with open(os.path.join(tmp, "100", "status"), "w", encoding="utf-8") as fh:
                fh.write(_status("runtime", 100, 1, "S", 12_345))
            self.assertEqual(RSS.read_vmrss(100, tmp), 12_345)
            self.assertIsNone(RSS.read_vmrss(999, tmp))  # exited between ps and read

    def test_read_vmrss_rejects_unreadable_member(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.mkdir(os.path.join(tmp, "101"))
            path = os.path.join(tmp, "101", "status")
            with open(path, "w", encoding="utf-8") as fh:
                fh.write(_status("hidden", 101, 100, "S", 7))
            os.chmod(path, 0)
            try:
                if os.geteuid() != 0:
                    with self.assertRaises(RSS.Unmeasurable):
                        RSS.read_vmrss(101, tmp)
            finally:
                os.chmod(path, 0o600)


class PsTableTests(unittest.TestCase):
    def test_parses_rows_and_paths_with_spaces(self):
        text = (
            "    1     0  4096 Ss   /sbin/launchd\n"
            "  100     1 12345 S    /Applications/Keel Runtime.app/Contents/MacOS/keel\n"
            "  101   100  2048 S+   keel worker\n"
        )
        rows = RSS.parse_ps_table(text)
        self.assertEqual(len(rows), 3)
        self.assertEqual(rows[1]["comm"], "/Applications/Keel Runtime.app/Contents/MacOS/keel")
        self.assertEqual(rows[1]["rssKB"], 12345)
        self.assertEqual(rows[2]["state"], "S+")

    def test_rejects_truncated_or_non_numeric_rows(self):
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_ps_table("  100     1\n")
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_ps_table("  100     1  abc S x\n")
        with self.assertRaises(RSS.Unmeasurable):
            RSS.parse_ps_table("\n \n")


class TreeSumTests(unittest.TestCase):
    def test_sums_root_and_descendants_and_ignores_unrelated(self):
        entries = _table([
            _entry(100, 1, 10_000, "runtime"),
            _entry(101, 100, 20_000, "group-a"),
            _entry(102, 101, 5_000, "layerfs"),
            _entry(900, 1, 99_000, "unrelated"),  # sibling of the root: never summed
        ])
        snap = RSS.measure_tree(entries, 100)
        self.assertEqual(snap["totalBytes"], 35_000 * 1024)
        self.assertEqual(snap["memberCount"], 3)
        pids = {p["pid"] for p in snap["processes"]}
        self.assertEqual(pids, {100, 101, 102})

    def test_zombie_member_is_recorded_not_counted(self):
        entries = _table([
            _entry(100, 1, 10_000),
            _entry(101, 100, 0, "worker", state="Z"),
        ])
        snap = RSS.measure_tree(entries, 100)
        self.assertEqual(snap["totalBytes"], 10_000 * 1024)
        self.assertEqual(snap["zombiePids"], [101])

    def test_missing_root_fails(self):
        with self.assertRaises(RSS.Unmeasurable):
            RSS.measure_tree(_table([_entry(101, 100, 1)]), 100)

    def test_zero_bytes_fails(self):
        with self.assertRaises(RSS.Unmeasurable):
            RSS.measure_tree(_table([_entry(100, 1, 0)]), 100)

    def test_vmrss_override_skips_exited_member(self):
        entries = _table([_entry(100, 1, 10_000), _entry(101, 100, 999)])
        readings = {100: 10_000, 101: None}
        snap = RSS.measure_tree(entries, 100, vmrss=lambda pid: readings[pid])
        self.assertEqual(snap["totalBytes"], 10_000 * 1024)
        self.assertEqual(snap["exitedDuringSample"], [101])

    def test_deeper_tree_with_vmrss_override(self):
        entries = _table([
            _entry(100, 1, 0, "control"),
            _entry(101, 100, 0, "group-a"),
            _entry(102, 101, 0, "layerfs"),
            _entry(103, 102, 0, "sub"),
        ])
        readings = {100: 4_000, 101: 3_000, 102: 2_000, 103: 1_000}
        snap = RSS.measure_tree(entries, 100, vmrss=lambda pid: readings[pid])
        self.assertEqual(snap["totalBytes"], 10_000 * 1024)
        self.assertEqual(snap["memberCount"], 4)

    def test_vmrss_override_zero_reads_zero(self):
        entries = _table([_entry(100, 1, 999)])
        with self.assertRaises(RSS.Unmeasurable):
            RSS.measure_tree(entries, 100, vmrss=lambda pid: 0)

    def test_sampler_overhead_subtree_is_excluded(self):
        # Sample a backend shell (100) whose child chain includes the sampler
        # (200 -> 201). Measurement cost must not be reported as workload.
        entries = _table([
            _entry(100, 1, 10_000), _entry(101, 100, 5_000),
            _entry(200, 101, 9_000), _entry(201, 200, 1_000),
        ])
        snap = RSS.measure_tree(entries, 100, overhead_root=200)
        self.assertEqual(snap["totalBytes"], 15_000 * 1024)
        self.assertEqual(snap["excludedOverhead"], [200, 201])
        self.assertEqual(snap["memberCount"], 2)

    def test_unrelated_sampler_is_not_listed_as_excluded(self):
        # A sampler outside the target tree was never summed, so nothing is
        # "excluded"; the member set and the total are untouched.
        entries = _table([_entry(100, 1, 10_000), _entry(200, 1, 9_000)])
        snap = RSS.measure_tree(entries, 100, overhead_root=200)
        self.assertEqual(snap["totalBytes"], 10_000 * 1024)
        self.assertEqual(snap["excludedOverhead"], [])

    def test_overhead_equals_root_is_not_excluded(self):
        # When the sampler IS the target (--pid $$ from the same shell), the
        # whole tree is the measurement subject; nothing may be dropped.
        entries = _table([_entry(100, 1, 10_000), _entry(101, 100, 5_000)])
        snap = RSS.measure_tree(entries, 100, overhead_root=100)
        self.assertEqual(snap["totalBytes"], 15_000 * 1024)
        self.assertEqual(snap["excludedOverhead"], [])


class SamplingWindowTests(unittest.TestCase):
    """collect_samples against a real (cheap) process; no network, no long sleep."""

    def test_short_window_produces_a_real_sample_and_reports_spacing(self):
        # Sampling this test process is cheap and deterministic enough; the
        # window is 0.3s so the suite stays fast.
        snap = RSS.sample_once(os.getpid())
        self.assertGreater(snap["totalBytes"], 0)
        collected = RSS.collect_samples(os.getpid(), duration=0.3, interval=1)
        self.assertGreaterEqual(len(collected["samples"]), 1)
        self.assertEqual(collected["errors"], [])
        self.assertEqual(collected["stopReason"], "duration")
        if len(collected["samples"]) > 1:
            self.assertIsNotNone(collected["observedIntervalSec"])

    def test_stop_file_ends_the_window_immediately(self):
        with tempfile.TemporaryDirectory() as tmp:
            stop = os.path.join(tmp, "stop")
            open(stop, "w").close()  # pre-existing marker: first sample ends it
            collected = RSS.collect_samples(os.getpid(), duration=5, interval=1, stop_file=stop)
            self.assertEqual(collected["stopReason"], "stop-file")
            self.assertEqual(len(collected["samples"]), 1)


class ThresholdTests(unittest.TestCase):
    def test_on_target_passes(self):
        summary = _summary(300 * 1_000_000)
        self.assertEqual(summary["verdict"], "PASS")
        self.assertEqual(summary["exitCode"], RSS.EXIT_PASS)
        self.assertIs(summary["thresholds"]["withinTarget"], True)
        self.assertLessEqual(summary["totals"]["peakMB"], 300.0)

    def test_above_target_band_passes_without_target(self):
        summary = _summary(450 * 1_000_000)
        self.assertEqual(summary["verdict"], "PASS")
        self.assertIs(summary["thresholds"]["withinTarget"], False)
        self.assertTrue(any("above the 300 MB target" in n for n in summary["notes"]))

    def test_ceiling_breach_fails(self):
        summary = _summary(501 * 1_000_000)
        self.assertEqual(summary["verdict"], "FAIL")
        self.assertEqual(summary["exitCode"], RSS.EXIT_BREACH)
        self.assertTrue(any("hard ceiling" in f for f in summary["failures"]))

    def test_exactly_at_ceiling_passes(self):
        summary = _summary(500 * 1_000_000)
        self.assertEqual(summary["verdict"], "PASS")
        self.assertIs(summary["thresholds"]["withinTarget"], False)

    def test_no_sample_is_unmeasurable_not_pass(self):
        collected = {"samples": [], "errors": ["no /proc and no ps"], "intervalSec": 1,
                     "requestedDurationSec": 10, "durationSec": 0, "stopReason": "unmeasurable"}
        summary = RSS.build_summary(mode="test", platform="plan9", source="unavailable",
                                    target={"pid": 1, "port": None, "identity": "pid", "comm": None},
                                    collected=collected)
        self.assertEqual(summary["verdict"], "UNMEASURABLE")
        self.assertEqual(summary["exitCode"], RSS.EXIT_UNMEASURABLE)
        self.assertIsNone(summary["totals"])

    def test_incomplete_measurement_wins_over_breach(self):
        # One real sample already breaches; the tree then vanished. The value
        # cannot be certified, so the verdict is UNMEASURABLE (exit 2), not FAIL.
        collected = {"samples": [RSS.measure_tree(_table([_entry(100, 1, 600 * 1000)]), 100)],
                     "errors": ["root pid 100 is not in the process table (already gone?)"],
                     "intervalSec": 1, "requestedDurationSec": 10, "durationSec": 1,
                     "stopReason": "duration"}
        summary = RSS.build_summary(mode="test", platform="darwin", source="ps-rss",
                                    target={"pid": 100, "port": None, "identity": "pid", "comm": None},
                                    collected=collected)
        self.assertEqual(summary["verdict"], "UNMEASURABLE")
        self.assertEqual(summary["exitCode"], RSS.EXIT_UNMEASURABLE)
        self.assertTrue(any("measurement aborted" in f for f in summary["failures"]))

    def test_steady_and_p50_are_computed(self):
        totals = [10 * 1_000_000, 20 * 1_000_000, 30 * 1_000_000]
        entries = _table([_entry(100, 1, 0)])
        samples = []
        for total in totals:
            entries[100]["rssKB"] = total // 1024
            samples.append(RSS.measure_tree(entries, 100))
        collected = {"samples": samples, "errors": [], "intervalSec": 1,
                     "requestedDurationSec": 10, "durationSec": 10, "stopReason": "duration"}
        summary = RSS.build_summary(mode="test", platform="darwin", source="ps-rss",
                                    target={"pid": 100, "port": None, "identity": "pid", "comm": None},
                                    collected=collected, steady_samples=3)
        self.assertEqual(summary["totals"]["peakMB"], 30.0)
        self.assertEqual(summary["totals"]["p50MB"], 20.0)
        self.assertAlmostEqual(summary["totals"]["steadyMB"], 20.0, places=2)


class MergeTests(unittest.TestCase):
    def _bench(self, **overrides) -> dict:
        bench = {
            "sessions": 1000, "sseSessions": 1000, "durationSec": 60.0, "texts": 64,
            "imagesOK": 20, "imagesErr": 0, "runsActiveMax": 64, "runsPendingMax": 64,
            "streamsActiveMax": 1000, "sseErrors": 5, "statusCounts": {"429": 5},
            "drainWaitSec": 1.0, "throughputMBs": 20.0,
        }
        bench.update(overrides)
        return bench

    def test_valid_workload_keeps_pass(self):
        merged = RSS.merge_workload(_summary(300 * 1_000_000), self._bench(),
                                    expected_sessions=1000, expected_runs=64,
                                    expected_duration_sec=60)
        self.assertEqual(merged["verdict"], "PASS")
        self.assertTrue(merged["workloadCheck"]["pass"])

    def test_empty_load_fails_even_under_budget(self):
        merged = RSS.merge_workload(
            _summary(200 * 1_000_000),
            self._bench(sessions=3, sseSessions=0, texts=0, imagesOK=0, runsActiveMax=0, durationSec=2.0),
            expected_sessions=1000, expected_runs=64, expected_duration_sec=60,
        )
        self.assertEqual(merged["verdict"], "FAIL")
        self.assertEqual(merged["exitCode"], RSS.EXIT_BREACH)
        self.assertFalse(merged["workloadCheck"]["pass"])
        self.assertTrue(any("empty-run guard" in f or "sessions" in f for f in merged["failures"]))

    def test_sampler_window_shorter_than_load_fails(self):
        summary = _summary(250 * 1_000_000)
        summary["sampling"]["actualDurationSec"] = 10
        merged = RSS.merge_workload(summary, self._bench(), expected_sessions=1000,
                                    expected_runs=64, expected_duration_sec=60)
        self.assertEqual(merged["verdict"], "FAIL")
        self.assertTrue(any("does not cover" in f for f in merged["failures"]))

    def test_unreadable_bench_fails(self):
        merged = RSS.merge_workload(_summary(250 * 1_000_000), None,
                                    bench_error="/tmp/bench.json: bad JSON",
                                    expected_sessions=1000, expected_runs=64,
                                    expected_duration_sec=60)
        self.assertEqual(merged["verdict"], "FAIL")

    def test_unmeasurable_rss_stays_unmeasurable(self):
        summary = _summary(0, error="root pid 100 is not in the process table")
        merged = RSS.merge_workload(summary, self._bench(), expected_sessions=1000,
                                    expected_runs=64, expected_duration_sec=60)
        self.assertEqual(merged["verdict"], "UNMEASURABLE")
        self.assertEqual(merged["exitCode"], RSS.EXIT_UNMEASURABLE)

    def test_hard_sse_errors_fail(self):
        merged = RSS.merge_workload(_summary(250 * 1_000_000),
                                    self._bench(sseErrors=4, statusCounts={"429": 1}),
                                    expected_sessions=1000, expected_runs=64,
                                    expected_duration_sec=60)
        self.assertEqual(merged["verdict"], "FAIL")
        self.assertTrue(any("hard SSE errors" in f for f in merged["failures"]))


class LinuxProcPathTests(unittest.TestCase):
    """The Linux branch: ps supplies the tree, procfs supplies per-member VmRSS."""

    def _proc_tree(self, root_dir: str, rows: list[tuple[int, int, int]]) -> None:
        for pid, ppid, rss_kb in rows:
            d = os.path.join(root_dir, str(pid))
            os.mkdir(d)
            with open(os.path.join(d, "status"), "w", encoding="utf-8") as fh:
                fh.write(_status("runtime", pid, ppid, "S", rss_kb))

    def test_ps_tree_with_vmrss_reads_only_members(self):
        # root 100 and child 101 are the tree; 900 is a sibling and must not count.
        tree = [(100, 1, 10_000), (101, 100, 5_000)]
        with tempfile.TemporaryDirectory() as tmp:
            self._proc_tree(tmp, tree + [(900, 1, 90_000)])
            orig = (RSS.platform_source, RSS.snapshot_from_ps, RSS.read_vmrss)
            try:
                RSS.platform_source = lambda: ("linux", "proc-status")
                RSS.snapshot_from_ps = lambda: _table([
                    _entry(100, 1, 0), _entry(101, 100, 0), _entry(900, 1, 0),
                ])
                RSS.read_vmrss = lambda pid, proc_root="/proc": orig[2](pid, tmp)
                snap = RSS.sample_once(100)
            finally:
                RSS.platform_source, RSS.snapshot_from_ps, RSS.read_vmrss = orig
        self.assertEqual(snap["totalBytes"], 15_000 * 1024)
        self.assertEqual(snap["treeSource"], "ps+proc")
        self.assertEqual(snap["source"], "proc-status")
        self.assertEqual([p["pid"] for p in snap["processes"]], [100, 101])

    def test_proc_only_tree_when_ps_is_absent_and_all_rows_readable(self):
        with tempfile.TemporaryDirectory() as tmp:
            self._proc_tree(tmp, [(100, 1, 10_000), (101, 100, 5_000)])
            orig = (RSS.platform_source, RSS.snapshot_from_ps, RSS.snapshot_from_proc, RSS.read_vmrss)
            try:
                RSS.platform_source = lambda: ("linux", "proc-status")

                def no_ps():
                    raise RSS.Unmeasurable("ps missing")

                RSS.snapshot_from_ps = no_ps
                RSS.snapshot_from_proc = lambda proc_root="/proc": orig[2](tmp)
                RSS.read_vmrss = lambda pid, proc_root="/proc": orig[3](pid, tmp)
                snap = RSS.sample_once(100)
            finally:
                (RSS.platform_source, RSS.snapshot_from_ps, RSS.snapshot_from_proc,
                 RSS.read_vmrss) = orig
        self.assertEqual(snap["totalBytes"], 15_000 * 1024)
        self.assertEqual(snap["treeSource"], "proc-only")

    def test_proc_only_refused_when_rows_are_unreadable(self):
        with tempfile.TemporaryDirectory() as tmp:
            self._proc_tree(tmp, [(100, 1, 10_000)])
            denied = os.path.join(tmp, "101")
            os.mkdir(denied)
            with open(os.path.join(denied, "status"), "w", encoding="utf-8") as fh:
                fh.write(_status("hidden", 101, 100, "S", 999))
            os.chmod(os.path.join(denied, "status"), 0)
            orig = (RSS.platform_source, RSS.snapshot_from_ps)
            try:
                RSS.platform_source = lambda: ("linux", "proc-status")

                def no_ps():
                    raise RSS.Unmeasurable("ps missing")

                RSS.snapshot_from_ps = no_ps
                if os.geteuid() != 0:
                    with self.assertRaises(RSS.Unmeasurable):
                        RSS.sample_once(100)
            finally:
                RSS.platform_source, RSS.snapshot_from_ps = orig
                os.chmod(os.path.join(denied, "status"), 0o600)

    def test_native_tree_sums_only_root_and_descendants(self):
        """ps rows for the whole machine; only 100's subtree is summed."""
        rows = [
            (100, 1, 12_000),   # root
            (101, 100, 6_000),  # group worker child
            (102, 101, 3_000),  # layerfs grandchild
            (900, 1, 99_000),   # unrelated
        ]
        entries = _table([_entry(pid, ppid, rss, "proc") for pid, ppid, rss in rows])
        snap = RSS.measure_tree(entries, 100)
        self.assertEqual(snap["totalBytes"], 21_000 * 1024)
        self.assertNotIn(900, [p["pid"] for p in snap["processes"]])


class PlatformFallbackTests(unittest.TestCase):
    """The "cannot measure this platform" path must be UNMEASURABLE, not PASS."""

    def test_no_proc_no_ps_is_unmeasurable(self):
        orig_platform, orig_which = RSS.sys.platform, RSS.shutil.which
        try:
            RSS.sys.platform = "plan9"
            RSS.shutil.which = lambda name: None  # neither procfs nor ps exists
            with self.assertRaises(RSS.Unmeasurable):
                RSS.platform_source()
            summary = RSS.build_summary(
                mode="attach", platform="plan9", source="unavailable",
                target={"pid": 1, "port": None, "identity": "pid", "comm": None},
                collected={"samples": [], "errors": ["no /proc and no ps"], "stopReason": "unmeasurable",
                           "durationSec": 0},
            )
            self.assertEqual(summary["verdict"], "UNMEASURABLE")
            self.assertEqual(summary["exitCode"], RSS.EXIT_UNMEASURABLE)
        finally:
            RSS.sys.platform = orig_platform
            RSS.shutil.which = orig_which

    def test_source_is_named_in_the_summary(self):
        _, source = RSS.platform_source()
        self.assertIn(source, RSS.SOURCE_NOTE)
        summary = _summary(250 * 1_000_000)
        self.assertEqual(summary["source"], "ps-rss")
        self.assertIn("ps RSS", summary["sourceNote"])


class CliContractTests(unittest.TestCase):
    """The CLI's identity/argument contract, without starting a backend."""

    def test_requires_port_or_pid(self):
        with self.assertRaises(SystemExit):
            with open(os.devnull, "w") as null, contextlib.redirect_stdout(io.StringIO()):
                stderr, sys.stderr = sys.stderr, null
                try:
                    RSS.main(["sample", "--duration", "1"])
                finally:
                    sys.stderr = stderr

    def test_unknown_pid_is_unmeasurable(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "summary.json")
            # PID 0/nonexistent: the CLI must fail closed with exit 2.
            with contextlib.redirect_stdout(io.StringIO()), \
                    contextlib.redirect_stderr(io.StringIO()):
                rc = RSS.main(["sample", "--pid", "999999999", "--duration", "1", "--out", out])
            self.assertEqual(rc, RSS.EXIT_UNMEASURABLE)
            with open(out, encoding="utf-8") as fh:
                summary = json.load(fh)
            self.assertEqual(summary["verdict"], "UNMEASURABLE")

    def test_interval_upper_bound(self):
        with self.assertRaises(SystemExit):
            with open(os.devnull, "w") as null, contextlib.redirect_stdout(io.StringIO()):
                stderr, sys.stderr = sys.stderr, null
                try:
                    RSS.main(["sample", "--pid", str(os.getpid()), "--duration", "1", "--interval", "11"])
                finally:
                    sys.stderr = stderr


if __name__ == "__main__":
    unittest.main(verbosity=2)
