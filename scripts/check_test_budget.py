#!/usr/bin/env python3
import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASELINE = ROOT / "scripts" / "test_budget_baseline.json"

GO_FUNC = re.compile(r"^func (Test\w+)\(", re.M)
GO_WORDING = re.compile(r"strings\.(Contains|HasPrefix|HasSuffix|EqualFold)\(")
GO_ASSERT = re.compile(r"\bt\.(Fatal|Fatalf|Error|Errorf)\(")
PY_FUNC = re.compile(r"^(?:    )?def (test_\w+)\(", re.M)
PY_ASSERT = re.compile(r"^\s*assert\b")
PY_WORDING = re.compile(r'^\s*assert\s+(?:not\s+)?["\'][^"\']*["\']\s+(?:not\s+)?in\b')


def go_bodies(text):
    starts = [(m.start(), m.group(1)) for m in GO_FUNC.finditer(text)]
    for i, (start, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[start:end]


def py_bodies(text):
    starts = [(m.start(), m.group(1)) for m in PY_FUNC.finditer(text)]
    for i, (start, name) in enumerate(starts):
        end = starts[i + 1][0] if i + 1 < len(starts) else len(text)
        yield name, text[start:end]


def go_wording_only(body):
    lines = body.splitlines()
    asserts = [ln for ln in lines if GO_ASSERT.search(ln)]
    if not asserts:
        return False
    conds = [ln for ln in lines if re.match(r"\s*if\b", ln)]
    if not conds:
        return False
    return all(GO_WORDING.search(ln) for ln in conds)


def py_wording_only(body):
    asserts = [ln for ln in body.splitlines() if PY_ASSERT.match(ln)]
    if not asserts:
        return False
    return all(PY_WORDING.match(ln) for ln in asserts)


def scan():
    hits = []
    for path in sorted((ROOT / "scheduler").glob("*_test.go")):
        for name, body in go_bodies(path.read_text()):
            if go_wording_only(body):
                hits.append(f"{path.relative_to(ROOT)}::{name}")
    py_roots = ["shared_tools", "shared_strategies", "platforms", "backtest", "shared_scripts", "scripts", ".github/scripts"]
    for root in py_roots:
        for path in sorted((ROOT / root).rglob("test_*.py")):
            for name, body in py_bodies(path.read_text()):
                if py_wording_only(body):
                    hits.append(f"{path.relative_to(ROOT)}::{name}")
    return hits


def compare(hits, baseline):
    hit_set = set(hits)
    base_set = set(baseline)
    return sorted(hit_set - base_set), sorted(base_set - hit_set)


def load_baseline():
    if not BASELINE.exists():
        return []
    data = json.loads(BASELINE.read_text())
    entries = data.get("wording_only_tests", [])
    if not isinstance(entries, list):
        raise SystemExit("baseline must list test identifiers; run --write-baseline")
    return entries


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--write-baseline", action="store_true")
    ap.add_argument("--list", action="store_true")
    args = ap.parse_args()
    hits = sorted(scan())
    if args.list:
        print("\n".join(hits))
    if args.write_baseline:
        BASELINE.write_text(json.dumps({"wording_only_tests": hits}, indent=2) + "\n")
        print(f"baseline written: {len(hits)} wording-only tests")
        return 0
    baseline = load_baseline()
    new, stale = compare(hits, baseline)
    print(f"wording-only tests: {len(hits)} (baseline {len(baseline)})")
    rc = 0
    if new:
        print("test budget exceeded: new tests assert only message wording", file=sys.stderr)
        for ident in new:
            print(f"  new: {ident}", file=sys.stderr)
        print("add an identifier to scripts/test_budget_baseline.json only when the wording drives an operator decision", file=sys.stderr)
        rc = 1
    if stale:
        print("baseline lists wording-only tests that no longer exist; run scripts/check_test_budget.py --write-baseline to tighten it", file=sys.stderr)
        for ident in stale:
            print(f"  stale: {ident}", file=sys.stderr)
        rc = 1
    return rc


if __name__ == "__main__":
    sys.exit(main())
