#!/usr/bin/env python3
"""Summarize `go test -json` output: per-package pass/fail/skip and failing tests."""
import json
import sys
from collections import defaultdict

path = sys.argv[1] if len(sys.argv) > 1 else "/tmp/keel-conformance.json"
packages = defaultdict(lambda: {"pass": 0, "fail": 0, "skip": 0, "tests": [], "failed": []})
for line in open(path):
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
json.dump({"total": total, "packages": packages}, open("/tmp/keel-conformance-summary.json", "w"), indent=2)
sys.exit(1 if failed else 0)
