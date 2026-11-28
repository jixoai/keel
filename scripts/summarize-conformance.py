#!/usr/bin/env python3
"""Summarize `go test -json` output: per-package pass/fail/skip and failing tests.

With --golden, the run is checked against a committed baseline: every package in
the golden must still exist, its pass count must not shrink, and the total pass
count must not shrink. A golden is a floor, not an equality assertion: adding
tests is allowed, losing or skipping them is a gate failure.
"""
import argparse
import json
import sys
from collections import defaultdict

parser = argparse.ArgumentParser()
parser.add_argument("path", nargs="?", default="/tmp/keel-conformance.json")
parser.add_argument("--golden", default="", help="committed baseline summary JSON")
parser.add_argument("--out", default="/tmp/keel-conformance-summary.json")
args = parser.parse_args()

packages = defaultdict(lambda: {"pass": 0, "fail": 0, "skip": 0, "tests": [], "failed": []})
for line in open(args.path):
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        continue
    pkg = ev.get("Package", "")
    if not pkg:
        continue
    action = ev.get("Action")
    test = ev.get("Test")
    if test and action in ("pass", "fail", "skip"):
        entry = packages[pkg]
        entry[action] += 1
        entry["tests"].append({"name": test, "status": action})
        if action == "fail":
            entry["failed"].append(test)

total = {"pass": 0, "fail": 0, "skip": 0}
print(f"{'package':50} {'pass':>6} {'fail':>6} {'skip':>6}")
for pkg in sorted(packages):
    e = packages[pkg]
    for k in total:
        total[k] += e[k]
    print(f"{pkg:50} {e['pass']:6} {e['fail']:6} {e['skip']:6}")
print("-" * 72)
print(f"{'TOTAL':50} {total['pass']:6} {total['fail']:6} {total['skip']:6}")
failed = [(pkg, t) for pkg, e in packages.items() for t in e["failed"]]
if failed:
    print("\nFAILED:")
    for pkg, t in failed:
        print(f"  {pkg} :: {t}")

summary = {"total": total, "packages": dict(packages)}
with open(args.out, "w") as fh:
    json.dump(summary, fh, indent=2)
    fh.write("\n")

golden_failures = []
if args.golden:
    try:
        with open(args.golden) as fh:
            golden = json.load(fh)
    except Exception as exc:  # an unreadable golden is a gate failure
        golden_failures.append(f"golden unreadable ({args.golden}): {exc}")
        golden = {}
    golden_packages = golden.get("packages", {})
    for pkg, expected in golden_packages.items():
        current = packages.get(pkg)
        if current is None:
            golden_failures.append(f"package {pkg} disappeared from the conformance run")
            continue
        floor = int(expected.get("pass", 0))
        if current["pass"] < floor:
            golden_failures.append(
                f"{pkg}: pass {current['pass']} < golden floor {floor}")
    floor = int(golden.get("total", {}).get("pass", 0))
    if total["pass"] < floor:
        golden_failures.append(f"total pass {total['pass']} < golden floor {floor}")
    if golden_failures:
        print("\nGOLDEN FAILURES:")
        for failure in golden_failures:
            print(f"  {failure}")
    else:
        print(f"\nGOLDEN: PASS (total pass {total['pass']} >= floor {floor})")

sys.exit(1 if failed or golden_failures else 0)
