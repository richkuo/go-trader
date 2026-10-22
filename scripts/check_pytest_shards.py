#!/usr/bin/env python3
import argparse
import collections
import json
import os
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"
LOG_NAME = re.compile(r"^(?P<suite>[a-z_]+)-(?P<group>[1-9][0-9]*)-of-(?P<splits>[1-9][0-9]*)\.jsonl$")
SUITE_NAME = re.compile(r"^[a-z_]+$")


class ShardError(Exception):
    pass


class NodeidCollector:
    def __init__(self):
        self.nodeids = None

    def pytest_collection_finish(self, session):
        self.nodeids = [item.nodeid for item in session.items]


def collect(suite, out, pytest_args):
    sys.path[:] = [p for p in sys.path if pathlib.Path(p or ".").resolve() != SCRIPTS]
    os.chdir(ROOT)
    import pytest

    collector = NodeidCollector()
    code = pytest.main(["--collect-only", "-qq", *pytest_args], plugins=[collector])
    if int(code) != 0 or collector.nodeids is None:
        raise ShardError(f"{suite}: pytest collection exited {int(code)}")
    if not collector.nodeids:
        raise ShardError(f"{suite}: pytest collected no tests")
    repeated = sorted(n for n, c in collections.Counter(collector.nodeids).items() if c > 1)
    if repeated:
        raise ShardError(f"{suite}: pytest collected these node ids more than once: " + ", ".join(repeated))
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps({"suite": suite, "nodeids": collector.nodeids}, indent=1) + "\n")
    print(f"{suite}: collected {len(collector.nodeids)} tests into {out}")


def load_collection(suite, path):
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError) as exc:
        raise ShardError(f"{suite}: cannot read the collection {path}: {exc}")
    if not isinstance(data, dict) or data.get("suite") != suite or not isinstance(data.get("nodeids"), list):
        raise ShardError(f"{suite}: {path} is not a collection of suite {suite}")
    nodeids = data["nodeids"]
    if not nodeids or len(set(nodeids)) != len(nodeids) or not all(isinstance(n, str) for n in nodeids):
        raise ShardError(f"{suite}: {path} has no tests, a repeated node id, or a non-string node id")
    return nodeids


def shard_logs(suite, logs_dir):
    found = {}
    splits = set()
    for path in sorted(logs_dir.iterdir()) if logs_dir.is_dir() else []:
        m = LOG_NAME.match(path.name)
        if not m or m["suite"] != suite:
            continue
        found[int(m["group"])] = path
        splits.add(int(m["splits"]))
    if not found:
        raise ShardError(f"{suite}: no shard report logs in {logs_dir}")
    if len(splits) != 1:
        raise ShardError(f"{suite}: shard report logs disagree on the number of shards: {sorted(splits)}")
    total = splits.pop()
    gaps = [f"group {g} of {total} has no report log" for g in range(1, total + 1) if g not in found]
    gaps += [f"group {g} is above the {total} shards" for g in sorted(found) if g > total]
    return total, sorted(found.items()), gaps


def read_log(suite, path):
    reports = []
    starts = 0
    finishes = []
    try:
        lines = path.read_text().splitlines()
    except OSError as exc:
        raise ShardError(f"{suite}: cannot read {path}: {exc}")
    for lineno, line in enumerate(lines, 1):
        try:
            rec = json.loads(line)
        except ValueError:
            raise ShardError(f"{suite}: {path.name} line {lineno} is not JSON")
        kind = rec.get("$report_type")
        if kind == "SessionStart":
            starts += 1
        elif kind == "SessionFinish":
            finishes.append(rec.get("exitstatus"))
        elif kind == "TestReport":
            reports.append(rec)
    if starts != 1 or len(finishes) != 1:
        raise ShardError(f"{suite}: {path.name} holds {starts} session starts and {len(finishes)} session ends; "
                         "the shard did not finish")
    return reports, finishes[0]


def verify(suite, collected_path, logs_dir, durations_path, durations_out):
    collected = load_collection(suite, collected_path)
    collected_set = set(collected)
    total, logs, gaps = shard_logs(suite, logs_dir)
    runs = collections.Counter()
    shards_of = collections.defaultdict(list)
    reported = set()
    failed = set()
    skipped = set()
    passed = set()
    durations = collections.defaultdict(float)
    bad_exit = []
    for group, path in logs:
        reports, exitstatus = read_log(suite, path)
        if exitstatus != 0:
            bad_exit.append(f"group {group} exited {exitstatus}")
        for rec in reports:
            nodeid = rec.get("nodeid")
            when = rec.get("when")
            outcome = rec.get("outcome")
            reported.add(nodeid)
            if when == "setup":
                runs[nodeid] += 1
                shards_of[nodeid].append(group)
            if outcome == "failed":
                failed.add(nodeid)
            elif outcome == "skipped":
                skipped.add(nodeid)
            elif when == "call" and outcome == "passed":
                passed.add(nodeid)
            duration = rec.get("duration")
            if isinstance(duration, (int, float)) and duration >= 0:
                durations[nodeid] += duration
    missing = sorted(n for n in collected if runs[n] == 0)
    repeated = sorted(n for n in collected if runs[n] > 1)
    unexpected = sorted(n for n in reported if n not in collected_set)
    failed_ids = sorted(failed)
    skipped_ids = skipped - failed
    passed_ids = passed - failed - skipped
    print(f"{suite}: {total} shards, {len(collected)} collected, {sum(runs.values())} runs, "
          f"{len(passed_ids)} passed, {len(skipped_ids)} skipped, {len(failed_ids)} failed")
    if durations_out is not None:
        durations_out.parent.mkdir(parents=True, exist_ok=True)
        durations_out.write_text(json.dumps(
            {n: round(durations[n], 4) for n in sorted(collected) if n in durations}, indent=4) + "\n")
    stale_note(suite, durations_path, collected_set)
    problems = []
    if gaps:
        problems.append(("shard report log problems", gaps))
    if missing:
        problems.append(("tests no shard ran", missing))
    if repeated:
        problems.append(("tests more than one shard or run ran",
                         [f"{n} (groups {shards_of[n]})" for n in repeated]))
    if unexpected:
        problems.append(("tests a shard ran that the collection does not hold", unexpected))
    if failed_ids:
        problems.append(("failed tests", failed_ids))
    if bad_exit:
        problems.append(("shards that did not exit 0", bad_exit))
    if not problems and len(passed_ids) + len(skipped_ids) != len(collected):
        problems.append(("count mismatch", [f"passed {len(passed_ids)} + skipped {len(skipped_ids)} "
                                            f"!= collected {len(collected)}"]))
    for label, items in problems:
        print(f"::error::{suite}: {len(items)} {label}", file=sys.stderr)
        for item in items:
            print(f"  {item}", file=sys.stderr)
    return 1 if problems else 0


def stale_note(suite, durations_path, collected_set):
    if durations_path is None:
        return
    try:
        recorded = set(json.loads(durations_path.read_text()))
    except (OSError, ValueError):
        recorded = set()
    unrecorded = len(collected_set - recorded)
    gone = len(recorded - collected_set)
    if unrecorded or gone:
        print(f"{suite}: {durations_path} has no duration for {unrecorded} collected tests and {gone} entries for tests that no longer exist; "
              "shards may be uneven, and no test is dropped (see SKILL.md, Python CI and the test budget)")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="command", required=True)
    c = sub.add_parser("collect")
    c.add_argument("suite")
    c.add_argument("--out", type=pathlib.Path, required=True)
    v = sub.add_parser("verify")
    v.add_argument("suite")
    v.add_argument("--collected", type=pathlib.Path, required=True)
    v.add_argument("--logs", type=pathlib.Path, required=True)
    v.add_argument("--durations", type=pathlib.Path)
    v.add_argument("--durations-out", type=pathlib.Path)
    argv = sys.argv[1:]
    pytest_args = argv[argv.index("--") + 1:] if "--" in argv else []
    args = ap.parse_args(argv[:argv.index("--")] if "--" in argv else argv)
    if not SUITE_NAME.match(args.suite):
        print(f"suite name must match {SUITE_NAME.pattern}: {args.suite}", file=sys.stderr)
        return 2
    try:
        if args.command == "collect":
            collect(args.suite, args.out.resolve(), pytest_args)
            return 0
        if pytest_args:
            print("verify takes no pytest arguments", file=sys.stderr)
            return 2
        return verify(args.suite, args.collected, args.logs, args.durations, args.durations_out)
    except ShardError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
