#!/usr/bin/env python3
"""Frozen study runner for cross_exchange_perp_arbitrage (#1883).

prepare imports a checked bundle. lock freezes protocol, plans, sources and the
binary without hashing the lock itself. run checks that lock before any replay
and writes an unscored inconclusive result when real evidence is missing.
render writes the report from that result and does not change the lock.
"""

import argparse
import datetime
import hashlib
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
BACKTEST = os.path.join(REPO, "backtest")
REPLAY = os.path.join(BACKTEST, "basket_replay.py")
LOCK_SCHEMA = "go-trader.pair-spread-study-lock"
RESULT_SCHEMA = "go-trader.pair-spread-study-result"
EXCLUDED_NAMES = {"protocol.lock.json", "results.json", "REPORT.md"}
EXCLUDED_DIRS = {"raw", "logs", "state", "__pycache__"}
SCOREABLE = {"evidence_missing", "mechanical_fixture"}
REPORT_FIELDS = (
    "source_hashes", "binary_hash", "data_coverage", "windows", "parameters", "costs",
    "baselines", "orientations", "latency", "failure_cases", "booked_net", "funding_policy",
    "recording_family", "oracle_visibility", "verdict",
)


class StudyRefusal(Exception):
    def __init__(self, stage, reason, detail):
        super().__init__(f"{stage}: {reason}: {detail}")
        self.stage = stage
        self.reason = reason
        self.detail = detail if isinstance(detail, list) else [detail]


class Study:
    def __init__(self, study_dir):
        self.dir = os.path.realpath(study_dir)
        self.protocol_path = os.path.join(self.dir, "protocol.json")
        self.lock_path = os.path.join(self.dir, "protocol.lock.json")
        self.manifest_path = os.path.join(self.dir, "data_manifest.json")
        self.plans_dir = os.path.join(self.dir, "plans")
        self.results_path = os.path.join(self.dir, "results.json")
        self.report_path = os.path.join(self.dir, "REPORT.md")


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def load_json(path):
    with open(path) as fh:
        return json.load(fh)


def dump_json(obj):
    return json.dumps(obj, indent=2, sort_keys=True) + "\n"


def write_new(path, obj):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    with os.fdopen(fd, "w") as fh:
        fh.write(dump_json(obj))


def field_reason(spec):
    if not isinstance(spec, dict):
        return "the report field is not an object"
    if "source" in spec and isinstance(spec["source"], str) and spec["source"]:
        return ""
    if "missing" in spec and isinstance(spec["missing"], str) and spec["missing"]:
        return ""
    return "the report field needs a source or a missing-evidence reason"


def validate_protocol(proto, spec_path=None):
    problems = []
    if proto.get("schema") != "go-trader.pair-spread-study-protocol" or proto.get("version") != 1:
        problems.append("protocol schema or version is not the pair spread study protocol")
    params = proto.get("parameters") if isinstance(proto.get("parameters"), dict) else {}
    required = params.get("required")
    if not isinstance(required, list) or not required:
        problems.append("parameters.required is missing")
    rules = proto.get("sample_rules") if isinstance(proto.get("sample_rules"), dict) else {}
    if not isinstance(rules.get("min_independent_pairs"), int) or rules.get("min_independent_pairs") <= 0:
        problems.append("sample_rules.min_independent_pairs is missing")
    coverage = rules.get("min_coverage_ratio")
    if isinstance(coverage, bool) or not isinstance(coverage, (int, float)) or not 0 < coverage <= 1:
        problems.append("sample_rules.min_coverage_ratio is missing")
    verdicts = proto.get("verdict_rules") if isinstance(proto.get("verdict_rules"), dict) else {}
    for key in ("pass", "fail", "inconclusive"):
        if not isinstance(verdicts.get(key), str) or not verdicts.get(key):
            problems.append(f"verdict_rules.{key} is missing")
    if not isinstance(proto.get("evidence_requirements"), list) or not proto.get("evidence_requirements"):
        problems.append("evidence_requirements are missing")
    fields = proto.get("report_fields") if isinstance(proto.get("report_fields"), dict) else {}
    for name in REPORT_FIELDS:
        why = field_reason(fields.get(name))
        if why:
            problems.append(f"report_fields.{name}: {why}")
    for key in ("windows", "baselines", "orientations", "latency_cases_ms", "mechanical_classification", "execution_sources"):
        if key not in proto:
            problems.append(f"{key} is missing")
    spec = proto.get("signal_spec") if isinstance(proto.get("signal_spec"), dict) else {}
    if not isinstance(spec.get("sha256"), str) or len(spec.get("sha256", "")) != 64:
        problems.append("signal_spec.sha256 is missing")
    elif spec_path and os.path.isfile(spec_path) and sha256_file(spec_path) != spec["sha256"]:
        problems.append("the signal specification differs from the hash the protocol freezes")
    if problems:
        raise StudyRefusal("protocol", "protocol_invalid", problems)
    return proto


def source_map(study, proto):
    found = {}
    for dirpath, dirnames, filenames in os.walk(study.dir):
        dirnames[:] = sorted(d for d in dirnames if d not in EXCLUDED_DIRS and not d.startswith("."))
        for name in sorted(filenames):
            if name in EXCLUDED_NAMES or name.startswith(".") or name.endswith((".pyc", ".pyo")):
                continue
            full = os.path.join(dirpath, name)
            if not os.path.isfile(full):
                continue
            found[os.path.relpath(full, REPO)] = sha256_file(full)
    missing = []
    for rel in proto.get("execution_sources") or []:
        path = os.path.join(REPO, rel)
        if os.path.isfile(path):
            found[rel] = sha256_file(path)
        else:
            missing.append(rel)
            found[rel] = None
    if missing:
        raise StudyRefusal("sources", "source_missing", [f"execution source {rel} is missing" for rel in missing])
    return found


def repo_dirty(rels):
    inside = []
    for rel in rels:
        path = os.path.realpath(os.path.join(REPO, rel))
        try:
            if os.path.commonpath([path, REPO]) != REPO:
                continue
        except ValueError:
            continue
        if os.path.isfile(path):
            inside.append(os.path.relpath(path, REPO))
    if not inside:
        return []
    out = subprocess.check_output(["git", "-C", REPO, "status", "--porcelain", "--", *inside], text=True)
    return [line for line in out.splitlines() if line.strip()]


def git_commit():
    try:
        return subprocess.check_output(["git", "-C", REPO, "rev-parse", "HEAD"], text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        return None


def tool_versions(binary):
    out = {"python": sys.version.split()[0]}
    for key, cmd in (("go", ["go", "version"]), ("uv", ["uv", "--version"])):
        try:
            out[key] = subprocess.check_output(cmd, text=True, stderr=subprocess.DEVNULL).strip()
        except (OSError, subprocess.CalledProcessError):
            out[key] = None
    if binary and os.path.isfile(binary):
        try:
            out["go_trader"] = subprocess.check_output([binary, "version"], text=True, stderr=subprocess.DEVNULL, timeout=60).strip()
        except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
            out["go_trader"] = None
    return out


def cmd_validate(study):
    proto = load_json(study.protocol_path)
    spec = os.path.join(REPO, proto.get("signal_spec", {}).get("path", ""))
    validate_protocol(proto, spec)
    print("protocol valid")


def cmd_prepare(study, bundle):
    if not bundle:
        raise StudyRefusal("prepare", "bundle_missing", "prepare needs --bundle")
    meta_path = os.path.join(bundle, "bundle.json")
    plan_path = os.path.join(bundle, "plan.json")
    if not os.path.isfile(meta_path) or not os.path.isfile(plan_path):
        raise StudyRefusal("prepare", "bundle_invalid", "the bundle needs bundle.json and plan.json")
    meta = load_json(meta_path)
    if meta.get("classification") != "mechanical_fixture":
        raise StudyRefusal("prepare", "bundle_invalid", "only a labeled mechanical_fixture bundle can be imported")
    dest = os.path.join(study.plans_dir, "mechanical")
    if os.path.exists(dest):
        raise StudyRefusal("prepare", "bundle_exists", "plans/mechanical already exists")
    os.makedirs(dest)
    for name in sorted(os.listdir(bundle)):
        if name == "bundle.json":
            continue
        src = os.path.join(bundle, name)
        if os.path.isfile(src):
            shutil.copy2(src, os.path.join(dest, name))
    plan = load_json(os.path.join(dest, "plan.json"))
    plan["state_dir"] = "RUNTIME"
    with open(os.path.join(dest, "plan.json"), "w") as fh:
        fh.write(dump_json(plan))
    objects = []
    for obj in plan.get("objects") or []:
        full = os.path.join(dest, obj["path"])
        objects.append({"kind": obj.get("kind"), "path": os.path.relpath(full, study.dir), "sha256": sha256_file(full), "size": os.path.getsize(full)})
    manifest = load_json(study.manifest_path) if os.path.isfile(study.manifest_path) else {}
    notes = list(manifest.get("standalone_notes") or [])
    for note in meta.get("standalone_notes") or []:
        if note not in notes:
            notes.append(note)
    manifest.update({
        "schema": "go-trader.pair-spread-study-data", "version": 1, "classification": "mechanical_fixture",
        "objects": objects, "funding_policy": meta.get("funding_policy"), "recording_family": meta.get("recording_family"),
        "standalone_notes": notes, "source_identity": meta.get("source_identity"),
    })
    with open(study.manifest_path, "w") as fh:
        fh.write(dump_json(manifest))
    print(f"prepared {dest}")


def cmd_lock(study, binary, allow_dirty=False):
    if os.path.exists(study.lock_path):
        raise StudyRefusal("lock", "lock_exists", "protocol.lock.json exists; a lock is never replaced")
    if not binary or not os.path.isfile(binary):
        raise StudyRefusal("lock", "binary_missing", "lock needs the built go-trader binary")
    proto = load_json(study.protocol_path)
    spec_path = os.path.join(REPO, proto["signal_spec"]["path"])
    validate_protocol(proto, spec_path)
    plans = {}
    for dirpath, _, filenames in os.walk(study.plans_dir):
        for name in sorted(filenames):
            if name.endswith(".json"):
                full = os.path.join(dirpath, name)
                plans[os.path.relpath(full, REPO)] = sha256_file(full)
    lock = {
        "schema": LOCK_SCHEMA, "version": 1,
        "locked_at_utc": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "commit": git_commit(), "protocol_sha256": sha256_file(study.protocol_path),
        "signal_spec_sha256": sha256_file(spec_path), "data_manifest_sha256": sha256_file(study.manifest_path),
        "plans": plans, "sources": source_map(study, proto), "binary_sha256": sha256_file(binary),
        "tools": tool_versions(binary), "excluded": sorted(EXCLUDED_NAMES | EXCLUDED_DIRS),
    }
    dirty = repo_dirty(list(lock["sources"]) + list(plans))
    if dirty and not allow_dirty:
        raise StudyRefusal("lock", "lock_dirty", dirty)
    if dirty:
        lock["dirty"] = dirty
    for rel in plans:
        text = open(os.path.join(REPO, rel)).read()
        if "protocol.lock.json" in text:
            raise StudyRefusal("lock", "lock_cycle", f"{rel} names the lock file")
    write_new(study.lock_path, lock)
    print(f"locked {study.lock_path}")


def check_lock(study, binary, check_binary=True):
    if not os.path.isfile(study.lock_path):
        raise StudyRefusal("lock", "lock_missing", "protocol.lock.json is missing; run lock before run")
    lock = load_json(study.lock_path)
    proto = load_json(study.protocol_path)
    spec_path = os.path.join(REPO, proto["signal_spec"]["path"])
    validate_protocol(proto, spec_path)
    problems = []
    if sha256_file(study.protocol_path) != lock.get("protocol_sha256"):
        problems.append("protocol.json changed")
    if sha256_file(spec_path) != lock.get("signal_spec_sha256"):
        problems.append("the signal specification changed")
    if sha256_file(study.manifest_path) != lock.get("data_manifest_sha256"):
        problems.append("data_manifest.json changed")
    plans = {}
    for dirpath, _, filenames in os.walk(study.plans_dir):
        for name in sorted(filenames):
            if name.endswith(".json"):
                full = os.path.join(dirpath, name)
                plans[os.path.relpath(full, REPO)] = sha256_file(full)
    if plans != lock.get("plans"):
        problems.append("a plan or schedule changed")
    try:
        sources = source_map(study, proto)
    except StudyRefusal as exc:
        problems.extend(exc.detail)
        sources = None
    if sources is not None and sources != lock.get("sources"):
        problems.append("the execution source set changed")
    if check_binary and (not binary or not os.path.isfile(binary) or sha256_file(binary) != lock.get("binary_sha256")):
        problems.append("the binary changed")
    if problems:
        raise StudyRefusal("lock", "lock_mismatch", problems)
    return proto, lock


def ledger_delta(row):
    gross = row.get("pnl_gross") in (1, True)
    fee = float(row.get("exchange_fee") or 0)
    realized = float(row.get("realized_pnl") or 0)
    if gross:
        return realized - fee
    if row.get("is_close") in (1, True):
        return realized
    return -fee


def reconcile_export(export):
    trades = export.get("trades") or []
    if not trades:
        return []
    strategies = {row.get("id") or row.get("strategy_id"): row for row in (export.get("strategies") or [])}
    by_id = {}
    for row in trades:
        sid = row.get("strategy_id")
        by_id[sid] = by_id.get(sid, 0.0) + ledger_delta(row)
    problems = []
    for sid, delta in sorted(by_id.items()):
        book = strategies.get(sid) or {}
        initial = book.get("initial_capital")
        cash = book.get("cash")
        if initial is None or cash is None:
            problems.append(f"{sid} has trades but no cash reading")
            continue
        if abs((float(initial) + delta) - float(cash)) > 1e-6:
            problems.append(f"{sid} cash {cash} does not match initial {initial} plus booked ledger {delta}")
    return problems


def replay_plan(study, binary, work, plan_path):
    plan = load_json(plan_path)
    if plan.get("state_dir") != "RUNTIME":
        raise StudyRefusal("run", "plan_path", f"{plan_path} does not keep a runtime state path")
    dest = os.path.join(work, "replay")
    state = os.path.join(work, "state")
    if os.path.exists(dest) or os.path.exists(state):
        raise StudyRefusal("run", "state_exists", "the work directory already holds a replay or state path")
    shutil.copytree(os.path.dirname(plan_path), dest)
    copied = load_json(os.path.join(dest, "plan.json"))
    copied["state_dir"] = state
    copy_path = os.path.join(dest, "plan.json")
    with open(copy_path, "w") as fh:
        fh.write(dump_json(copied))
    out_path = os.path.join(work, "result.json")
    proc = subprocess.run([sys.executable, REPLAY, "--plan", copy_path, "--binary", binary, "--out", out_path, "--timeout", "900"],
                          cwd=REPO, capture_output=True, text=True)
    if not os.path.isfile(out_path):
        raise StudyRefusal("run", "replay_failed", [proc.stderr[-4000:], proc.stdout[-2000:]])
    return load_json(out_path), proc.returncode, state


def evidence_gaps(manifest):
    reasons = [item["reason"] for item in manifest.get("missing") or [] if isinstance(item, dict) and item.get("reason")]
    if manifest.get("classification") == "evidence_missing":
        reasons.append("real OKX history, cross-venue depth, capability, fees or funding evidence is not frozen")
    return sorted(set(reasons))


def cmd_run(study, binary, work):
    if os.path.exists(study.results_path):
        raise StudyRefusal("run", "results_exist", "results.json exists; a new evaluation needs a new directory")
    proto, _lock = check_lock(study, binary)
    work = os.path.realpath(work)
    if os.path.commonpath([work, study.dir]) == study.dir:
        raise StudyRefusal("run", "work_inside_study", "the work directory must sit outside the locked study directory")
    manifest = load_json(study.manifest_path)
    classification = manifest.get("classification")
    if classification not in SCOREABLE:
        raise StudyRefusal("run", "unscored_classification",
                           f"classification {classification!r} has no scoring path; this runner writes inconclusive only for evidence_missing and mechanical_fixture")
    gaps = evidence_gaps(manifest)
    experiments = []
    comparisons = []
    mechanical = manifest.get("classification") == "mechanical_fixture"
    plan_path = os.path.join(study.plans_dir, "mechanical", "plan.json")
    if mechanical and os.path.isfile(plan_path):
        os.makedirs(work, exist_ok=True)
        rec, code, _state = replay_plan(study, binary, work, plan_path)
        adapter = rec.get("adapter_output") or {}
        export = adapter.get("export") or {}
        problems = reconcile_export(export) if export else []
        comparison = adapter.get("comparison") or {}
        experiments.append({
            "id": "mechanical", "adapter_exit_code": code, "status": rec.get("status"),
            "mechanically_complete": rec.get("mechanically_complete") is True,
            "reconciled": not problems, "reconcile_problems": problems,
            "comparison_eligible": comparison.get("eligible") is True, "economic_claim": False,
            "incomplete_reasons": adapter.get("incomplete_reasons"), "refusal": rec.get("refusal"),
        })
        if comparison.get("hash"):
            comparisons.append({"id": "mechanical", "hash": comparison.get("hash"), "eligible": comparison.get("eligible") is True, "economic_claim": False})
        gaps.append(proto["mechanical_classification"])
        gaps.extend(problems)
    reasons = sorted(set(gaps))
    result = {
        "schema": RESULT_SCHEMA, "version": 1, "verdict": "inconclusive", "scored": False, "promotion_eligible": False,
        "reasons": reasons, "lock_sha256": sha256_file(study.lock_path), "experiments": experiments, "comparisons": comparisons,
        "booked_net": None, "funding_policy": manifest.get("funding_policy"), "recording_family": manifest.get("recording_family"),
        "standalone_notes": manifest.get("standalone_notes") or [], "edge_source": proto.get("edge_source"),
        "oracle_note": "a research receipt price is not a historical venue oracle price",
    }
    write_new(study.results_path, result)
    print(f"verdict {result['verdict']} scored={result['scored']} out={study.results_path}")


def cmd_render(study):
    check_lock(study, None, check_binary=False)
    if not os.path.isfile(study.results_path):
        raise StudyRefusal("render", "results_missing", "results.json is missing; run the study before render")
    result = load_json(study.results_path)
    proto = load_json(study.protocol_path)
    lock = load_json(study.lock_path)
    lines = [
        "# cross_exchange_perp_arbitrage study",
        "",
        f"**Verdict: {result.get('verdict')}.** `scored` is {str(result.get('scored')).lower()}. `promotion_eligible` is {str(result.get('promotion_eligible')).lower()}. `edge_source` stays `{result.get('edge_source')}`.",
        "",
        "## Lock",
        "",
        f"- Commit recorded by the lock: `{lock.get('commit')}`.",
        *([f"- Dirty sources: `{item}`." for item in lock["dirty"]] if lock.get("dirty") else []),
        f"- Binary sha256: `{lock.get('binary_sha256')}`.",
        f"- Protocol sha256: `{lock.get('protocol_sha256')}`.",
        f"- Signal specification sha256: `{lock.get('signal_spec_sha256')}`.",
        f"- Result lock sha256: `{result.get('lock_sha256')}`.",
        "",
        "## Evidence",
        "",
    ]
    for reason in result.get("reasons") or []:
        lines.append(f"- {reason}")
    lines.extend(["", "## Report fields", ""])
    for name in REPORT_FIELDS:
        spec = proto["report_fields"][name]
        if "source" in spec:
            lines.append(f"- {name}: source `{spec['source']}`.")
        else:
            lines.append(f"- {name}: missing. {spec['missing']}")
    lines.extend([
        "",
        "## Baselines and orientations",
        "",
        "The declared baselines are no trade and a frozen two-leg schedule. The declared orientations are long Hyperliquid short OKX, and long OKX short Hyperliquid.",
        "A scheduled slot with no entry holds. The candidate policy is not a fallback. A lifecycle conflict holds and does not force a transition.",
        "",
        "## Funding and recordings",
        "",
        f"Funding policy on this result: `{result.get('funding_policy')}`. Recording family: `{result.get('recording_family')}`.",
        result.get("oracle_note") or "",
        "The driver accepts recording manifest versions 1, 2 and 3. A family label changed on a log does not make that log a different family.",
        "scenario.funding_policy is only for a paper collection plan. An offline experiment reads the policy from the recording manifest or the frozen funding document.",
        "",
        "## Booked results",
        "",
        "No booked net is claimed. An immediate-or-cancel order can stay unfilled when its rounded policy bound excludes the worst consumed price level.",
        "Adapter comparison eligibility is not study equality and does not clear an incomplete reason.",
        "",
        "## Commands",
        "",
        "```",
        "uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py validate",
        "uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py lock --binary <go-trader>",
        "uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py run --binary <go-trader> --work <fresh-dir>",
        "uv run --no-sync python backtest/candidates/cross_exchange_perp_arbitrage_1655/run_study.py render",
        "```",
        "",
        "Production refusal stays enabled. This report does not change a deployed configuration, credential, default, or live trading path.",
        "",
    ])
    text = "\n".join(lines)
    with open(study.report_path, "w") as fh:
        fh.write(text)
    print(f"rendered {study.report_path}")


def cmd_cache(manifest_path, cache):
    if BACKTEST not in sys.path:
        sys.path.insert(0, BACKTEST)
    import archive_cache
    manifest = load_json(manifest_path)
    try:
        first = archive_cache.fetch_manifest(cache, manifest)
        base = first["cache"]
        for entry in manifest["files"]:
            if entry.get("status") == "present":
                archive_cache.verified_object(base, entry["key"], entry)
        second = archive_cache.fetch_manifest(cache, manifest)
    except archive_cache.ArchiveError as exc:
        raise StudyRefusal("cache", "cache_refused", str(exc)) from exc
    print(dump_json({"first": first, "second": second}))


def main(argv=None):
    parser = argparse.ArgumentParser(description="Frozen pair spread study (#1883)")
    parser.add_argument("--study", default=HERE)
    sub = parser.add_subparsers(dest="cmd", required=True)
    sub.add_parser("validate")
    prep = sub.add_parser("prepare")
    prep.add_argument("--bundle")
    lock = sub.add_parser("lock")
    lock.add_argument("--binary", required=True)
    lock.add_argument("--allow-dirty", action="store_true")
    run = sub.add_parser("run")
    run.add_argument("--binary", required=True)
    run.add_argument("--work", required=True)
    sub.add_parser("render")
    sub.add_parser("check")
    cache = sub.add_parser("cache-check")
    cache.add_argument("--manifest", required=True)
    cache.add_argument("--cache", required=True)
    args = parser.parse_args(argv)
    study = Study(args.study)
    try:
        if args.cmd == "validate":
            cmd_validate(study)
        elif args.cmd == "prepare":
            cmd_prepare(study, os.path.abspath(args.bundle) if args.bundle else None)
        elif args.cmd == "lock":
            cmd_lock(study, os.path.abspath(args.binary), args.allow_dirty)
        elif args.cmd == "run":
            cmd_run(study, os.path.abspath(args.binary), os.path.abspath(args.work))
        elif args.cmd == "render":
            cmd_render(study)
        elif args.cmd == "check":
            check_lock(study, None, check_binary=False)
            print("lock matches")
        elif args.cmd == "cache-check":
            cmd_cache(os.path.abspath(args.manifest), os.path.abspath(args.cache))
    except StudyRefusal as exc:
        print(f"study refused at {exc.stage}: {exc.reason}: {exc.detail}", file=sys.stderr)
        return 3
    return 0


if __name__ == "__main__":
    sys.exit(main())
