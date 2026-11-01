#!/usr/bin/env python3
"""Offline basket replay driver (issue 1791).

The driver owns frozen inputs and the result record only. It verifies the
hashes of frozen candle and funding files, cuts the replay window, prepares one
versioned step script and runs the dedicated Go adapter (`go-trader
basket-replay`), which drives the real paper basket coordinator, member
management and accounting owners on a simulated clock. Targets, transition
bounds, order sequencing, member management and recovery stay in Go; this file
never decides an order.

Look-ahead rules: each step at time T carries marks equal to the open of the bar
that opens at T (bar N+1 open after a bar N decision). Candle frames are native
closed bars; the adapter cuts them at the step time and never forward-fills a
series, so no higher-timeframe series is reindexed here.

A result is scored only when the adapter reports status "complete". Any process
error, adapter timeout, refusal, short frame, missing mark, unresolved funding or
unresolved order produces a diagnostic record with scored=false and
promotion_eligible=false; it is never a valid economic result.

Plan version 2 (issue 1881) runs one research pair through the version-2 offline
pair replay: it hashes the research configuration and every frozen input object
(evidence, funding, recorded quotes or books, management candles), passes them
by reference with their sizes, and records the adapter's mechanical result. A
pair result is never scored or promotion-eligible; the driver succeeds only when
the adapter exits 0 and reports mechanically_complete.

Isolated paper (issue 1882): --paper runs the same version-2 pair plan through
`go-trader basket-replay-paper`, which records every live public input and
process outcome before use; the plan adds a `collection` block and the paper
scenario fields, --config names the checked research egress file, --seal seals
a finished run, and --max-steps stops a resumable chunk. A paper run has no
wall timeout unless --timeout is given (offline keeps 3600 seconds). A plan with
a `recording` reference replays a sealed recording offline and reads its
manifest version. Funding policy belongs to paper plans, recording manifests
and frozen funding documents. Every invocation writes a new --out file, so each
chunk names its own result path.
"""

import argparse
import csv
import datetime
import gzip
import hashlib
import io
import json
import math
import os
import subprocess
import sys

PLAN_SCHEMA = "go-trader.basket-replay-plan"
PLAN_VERSION = 1
PAIR_PLAN_VERSION = 2
RESULT_SCHEMA = "go-trader.basket-replay-result"
RESULT_VERSION = 1
PAIR_RESULT_VERSION = 2
ADAPTER_PROTOCOL = "go-trader.basket-replay"
ADAPTER_VERSION = 1
PAIR_ADAPTER_VERSION = 2
INTERVAL_MS = {"1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000, "30m": 1_800_000,
               "1h": 3_600_000, "2h": 7_200_000, "4h": 14_400_000, "8h": 28_800_000,
               "12h": 43_200_000, "1d": 86_400_000}
REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

PLAN_KEYS = {"plan", "version", "run_id", "state_dir", "source", "frames", "funding", "window", "history_bars",
             "basket", "members", "target_input", "cost_policy", "lot_metadata", "portfolio_risk", "venue_script"}
WINDOW_KEYS = {"start", "end", "step_seconds"}
FILE_KEYS = {"path", "sha256"}
FUNDING_KEYS = {"path", "sha256", "from"}
SCRIPT_KEYS = {"venue", "queries", "save_failure"}
PAIR_PLAN_KEYS = {"plan", "version", "run_id", "state_dir", "source", "research_config", "objects", "scenario", "bounds"}
PAIR_PLAN_OPTIONAL = {"collection", "recording", "target_input"}
TARGET_INPUT_MODES = {"policy", "no_trade", "frozen_schedule"}
TARGET_LEG_KEYS = {"target_base_qty", "price_bound"}
COLLECTION_KEYS = {"from_ms", "through_ms", "drain_through_ms"}
OFFLINE_TIMEOUT_S = 3600
PAIR_OBJECT_KEYS = {"kind", "member_id", "path", "sha256"}
PAIR_OBJECT_KINDS = {"evidence", "funding", "quotes", "books", "candles"}


class ReplayPlanError(Exception):
    pass


def _utc_ms(text):
    try:
        ts = datetime.datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
    except (TypeError, ValueError) as exc:
        raise ReplayPlanError(f"{text!r} is not an RFC 3339 UTC time like 2025-09-12T00:00:00Z") from exc
    return int(ts.timestamp() * 1000)


def _iso(ms):
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _strict_keys(obj, allowed, label):
    if not isinstance(obj, dict):
        raise ReplayPlanError(f"{label} is not an object")
    extra = sorted(set(obj) - allowed)
    if extra:
        raise ReplayPlanError(f"{label} has unknown fields {extra}")


def _resolve(base, path):
    return path if os.path.isabs(path) else os.path.normpath(os.path.join(base, path))


def _read_csv(path):
    opener = gzip.open if path.endswith(".gz") else open
    with opener(path, "rt", newline="") as fh:
        return list(csv.DictReader(fh))


def _unique_pairs(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ReplayPlanError(f"plan names the key {key!r} twice in one object")
        out[key] = value
    return out


def read_plan(path):
    with open(path) as fh:
        return json.load(fh, object_pairs_hook=_unique_pairs)


def is_pair_plan(plan):
    return isinstance(plan, dict) and plan.get("plan") == PLAN_SCHEMA and plan.get("version") == PAIR_PLAN_VERSION


def check_plan(plan):
    if is_pair_plan(plan):
        return load_pair_plan(plan)
    _strict_keys(plan, PLAN_KEYS, "plan")
    if plan.get("plan") != PLAN_SCHEMA or plan.get("version") != PLAN_VERSION:
        raise ReplayPlanError(f"plan is {plan.get('plan')!r} version {plan.get('version')!r}; this driver reads "
                              f"{PLAN_SCHEMA!r} version {PLAN_VERSION}")
    for key in ("run_id", "state_dir", "source", "frames", "window", "basket", "members", "target_input"):
        if key not in plan:
            raise ReplayPlanError(f"plan is missing {key!r}")
    _strict_keys(plan["window"], WINDOW_KEYS, "window")
    for key, spec in plan["frames"].items():
        _strict_keys(spec, FILE_KEYS, f"frames[{key}]")
    for coin, spec in (plan.get("funding") or {}).items():
        _strict_keys(spec, FUNDING_KEYS, f"funding[{coin}]")
    for at, spec in (plan.get("venue_script") or {}).items():
        _utc_ms(at)
        _strict_keys(spec, SCRIPT_KEYS, f"venue_script[{at}]")
    return plan


def load_pair_plan(plan):
    _strict_keys(plan, PAIR_PLAN_KEYS | PAIR_PLAN_OPTIONAL, "plan")
    for key in sorted(PAIR_PLAN_KEYS):
        if key not in plan:
            raise ReplayPlanError(f"plan is missing {key!r}")
    if "collection" in plan:
        _strict_keys(plan["collection"], COLLECTION_KEYS, "collection")
        for key in sorted(COLLECTION_KEYS):
            value = plan["collection"].get(key)
            if isinstance(value, bool) or not isinstance(value, int):
                raise ReplayPlanError(f"collection.{key} must be an integer millisecond time")
        if "recording" in plan:
            raise ReplayPlanError("a paper plan records live and takes no recording reference")
    if "recording" in plan:
        _strict_keys(plan["recording"], FILE_KEYS, "recording")
    if "target_input" in plan and ("collection" in plan or "recording" in plan):
        raise ReplayPlanError("target_input is refused with a paper collection or a sealed recording")
    if "target_input" in plan:
        _check_target_input_shape(plan["target_input"])
    _strict_keys(plan["research_config"], FILE_KEYS, "research_config")
    if not isinstance(plan["objects"], list) or not plan["objects"]:
        raise ReplayPlanError("objects must be a nonempty list")
    for i, obj in enumerate(plan["objects"]):
        _strict_keys(obj, PAIR_OBJECT_KEYS, f"objects[{i}]")
        if obj.get("kind") not in PAIR_OBJECT_KINDS:
            raise ReplayPlanError(f"objects[{i}] kind {obj.get('kind')!r} is not one of {sorted(PAIR_OBJECT_KINDS)}")
        path = obj.get("path")
        if not isinstance(path, str) or os.path.isabs(path) or os.path.normpath(path) != path or path.startswith(".."):
            raise ReplayPlanError(f"objects[{i}] path {path!r} must be a clean relative path under the plan directory")
    return plan


def verify_pair_inputs(plan, base):
    hashes, objects = {}, []
    path = _resolve(base, plan["research_config"]["path"])
    got = _sha256_file(path)
    if got != plan["research_config"]["sha256"]:
        raise ReplayPlanError(f"research_config file {path!r} has sha256 {got}, the plan freezes {plan['research_config']['sha256']}")
    hashes["research_config"] = got
    with open(path) as fh:
        research = json.load(fh, object_pairs_hook=_unique_pairs)
    for obj in plan["objects"]:
        full = _resolve(base, obj["path"])
        if not os.path.isfile(full):
            raise ReplayPlanError(f"object {obj['path']!r} is missing")
        got = _sha256_file(full)
        if got != obj["sha256"]:
            raise ReplayPlanError(f"object {obj['path']!r} has sha256 {got}, the plan freezes {obj['sha256']}")
        hashes[f"{obj['kind']}[{obj.get('member_id', '')}]"] = got
        ref = {"kind": obj["kind"], "path": obj["path"], "sha256": got, "size_bytes": os.path.getsize(full)}
        if obj.get("member_id"):
            ref["member_id"] = obj["member_id"]
        objects.append(ref)
    return hashes, research, objects


def _finite_number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def _canonical_slot(text):
    if not isinstance(text, str) or not text.isdigit() or text[0] == "0":
        return None
    return int(text)


def _check_target_input_shape(ti):
    if ti is None:
        raise ReplayPlanError("target_input must be an object when present; null is refused")
    _strict_keys(ti, {"mode", "schedule"}, "target_input")
    mode = ti.get("mode")
    if mode not in TARGET_INPUT_MODES:
        raise ReplayPlanError(f"target_input.mode {mode!r} must be one of {sorted(TARGET_INPUT_MODES)}")
    if mode in ("policy", "no_trade"):
        if "schedule" in ti:
            raise ReplayPlanError(f"target_input mode {mode} carries no schedule")
        return
    schedule = ti.get("schedule")
    if not isinstance(schedule, dict):
        raise ReplayPlanError("target_input mode frozen_schedule needs a schedule object; null is refused")


def _validate_pair_target_input(plan, research):
    ti = plan["target_input"]
    _check_target_input_shape(ti)
    if ti["mode"] != "frozen_schedule":
        return
    pairs = research.get("pairs") if isinstance(research, dict) else None
    if not isinstance(pairs, list) or len(pairs) != 1 or not isinstance(pairs[0], dict):
        raise ReplayPlanError("target_input needs research_config to name exactly one pair")
    members = pairs[0].get("members")
    if not isinstance(members, list) or len(members) != 2:
        raise ReplayPlanError("target_input needs exactly the two configured members")
    ids = []
    for m in members:
        if not isinstance(m, dict) or not isinstance(m.get("strategy_id"), str) or m["strategy_id"] in ids:
            raise ReplayPlanError("target_input needs two distinct configured member ids")
        ids.append(m["strategy_id"])
    limits = pairs[0].get("limits") if isinstance(pairs[0].get("limits"), dict) else {}
    cadence = limits.get("decision_cadence_ms")
    if isinstance(cadence, bool) or not isinstance(cadence, int) or cadence <= 0:
        raise ReplayPlanError("target_input needs limits.decision_cadence_ms as a positive integer")
    start, end = _utc_ms(plan["bounds"]["start"]), _utc_ms(plan["bounds"]["end"])
    if end < start:
        raise ReplayPlanError("bounds.end must not be before bounds.start")
    for key, entry in ti["schedule"].items():
        slot = _canonical_slot(key)
        if slot is None:
            raise ReplayPlanError(f"target_input.schedule key {key!r} is not a canonical positive decimal millisecond time")
        if slot % cadence or not (slot < end and slot + cadence > start):
            raise ReplayPlanError(f"target_input.schedule slot {key} is not a {cadence} ms cadence slot inside the replay lifetime")
        if not isinstance(entry, dict) or set(entry) != set(ids):
            raise ReplayPlanError(f"target_input.schedule[{key}] must name exactly the configured members {sorted(ids)}")
        for member_id in sorted(ids):
            leg = entry[member_id]
            if not isinstance(leg, dict) or set(leg) != TARGET_LEG_KEYS:
                raise ReplayPlanError(f"target_input.schedule[{key}].{member_id} must contain exactly target_base_qty and price_bound")
            if not _finite_number(leg["target_base_qty"]):
                raise ReplayPlanError(f"target_input.schedule[{key}].{member_id} target_base_qty must be a finite JSON number")
            if not _finite_number(leg["price_bound"]) or not leg["price_bound"] > 0:
                raise ReplayPlanError(f"target_input.schedule[{key}].{member_id} price_bound must be a finite JSON number above zero")


def build_pair_spec(plan, base, mode, research, objects):
    """Pass the frozen pair inputs by reference; every decision stays in the Go adapter."""
    spec = {
        "protocol": ADAPTER_PROTOCOL, "version": PAIR_ADAPTER_VERSION, "mode": mode,
        "run": {"run_id": plan["run_id"], "state_dir": plan["state_dir"], "source": plan["source"]},
        "research_config": research,
        "input_manifest": {"root": os.path.abspath(base), "objects": objects},
        "scenario": plan["scenario"], "bounds": plan["bounds"],
    }
    if "collection" in plan:
        spec["collection"] = plan["collection"]
    elif isinstance(plan["scenario"], dict) and "funding_policy" in plan["scenario"]:
        raise ReplayPlanError("scenario.funding_policy belongs to a paper plan with a collection block; an offline replay reads the policy from its recording manifest or frozen funding document")
    if "target_input" in plan:
        _validate_pair_target_input(plan, research)
        spec["target_input"] = plan["target_input"]
    if "recording" in plan:
        path = _resolve(base, plan["recording"]["path"])
        got = _sha256_file(path)
        if got != plan["recording"]["sha256"]:
            raise ReplayPlanError(f"recording {path!r} has sha256 {got}, the plan freezes {plan['recording']['sha256']}")
        with open(path) as fh:
            manifest = json.load(fh, object_pairs_hook=_unique_pairs)
        if not isinstance(manifest, dict) or manifest.get("kind") != "go-trader.paper-recording" or isinstance(manifest.get("v"), bool) or manifest.get("v") not in (1, 2, 3, 4):
            raise ReplayPlanError("recording manifest must be a go-trader.paper-recording object of version 1, 2, 3 or 4")
        spec["recording"] = {"version": manifest["v"], "path": plan["recording"]["path"], "sha256": got, "size_bytes": os.path.getsize(path)}
    return spec


def pair_result_record(plan_sha, input_hashes, code, out, process_error):
    rec = {"schema": RESULT_SCHEMA, "version": PAIR_RESULT_VERSION, "plan_sha256": plan_sha, "inputs": input_hashes,
           "adapter_exit_code": code, "scored": False, "promotion_eligible": False, "mechanically_complete": False}
    if process_error:
        rec.update(status="process_error", refusal={"reason": "process_error", "detail": [process_error]})
        return rec
    rec["status"] = out.get("status")
    rec["adapter_output"] = out
    rec["mechanically_complete"] = out.get("mechanically_complete") is True and code == 0
    rec["incomplete_reasons"] = list(out.get("incomplete_reasons") or [])
    rec["comparison"] = out.get("comparison")
    if out.get("refusal") or out.get("error"):
        refusal = out.get("refusal") or {}
        detail = list(refusal.get("detail") or [])
        if out.get("error"):
            detail.append(out["error"])
        rec["refusal"] = {"reason": refusal.get("reason") or f"status_{out.get('status')}", "detail": detail}
    return rec


def verify_inputs(plan, base):
    hashes = {}
    for label, spec in [(f"frames[{k}]", v) for k, v in sorted(plan["frames"].items())] + \
                       [(f"funding[{k}]", v) for k, v in sorted((plan.get("funding") or {}).items())]:
        path = _resolve(base, spec["path"])
        if not os.path.isfile(path):
            raise ReplayPlanError(f"{label} file {path!r} is missing")
        got = _sha256_file(path)
        if got != spec["sha256"]:
            raise ReplayPlanError(f"{label} file {path!r} has sha256 {got}, the plan freezes {spec['sha256']}")
        hashes[label] = got
    return hashes


def build_spec(plan, base, mode):
    """Cut the frozen window and build the adapter step script; no trading decision is made here."""
    win = plan["window"]
    start, end = _utc_ms(win["start"]), _utc_ms(win["end"])
    step_ms = int(win["step_seconds"]) * 1000
    if end <= start or step_ms <= 0:
        raise ReplayPlanError("window end must be after start and step_seconds must be positive")
    history = int(plan.get("history_bars") or 0)
    if history <= 0:
        raise ReplayPlanError("history_bars must be positive")
    members = plan["members"]
    candles, opens = {}, {}
    for key, spec in sorted(plan["frames"].items()):
        coin, _, tf = key.partition("|")
        ivl = INTERVAL_MS.get(tf)
        if ivl is None:
            raise ReplayPlanError(f"frames[{key}] timeframe {tf!r} is not supported")
        if step_ms % ivl or start % ivl:
            raise ReplayPlanError(f"frames[{key}]: steps must land on bar opens; step_seconds and window start "
                                  f"must be multiples of {ivl // 1000}s so each mark is a bar open")
        lo = start - history * ivl
        rows = []
        for r in _read_csv(_resolve(base, spec["path"])):
            t = int(r["timestamp"])
            if lo <= t < end:
                rows.append([t, float(r["open"]), float(r["high"]), float(r["low"]), float(r["close"]),
                             float(r["volume"])])
        rows.sort(key=lambda r: r[0])
        candles[key] = rows
        opens[coin] = {r[0]: r[1] for r in rows}
    funding = {}
    for coin, spec in sorted((plan.get("funding") or {}).items()):
        lo = _utc_ms(spec["from"])
        recs = []
        for r in _read_csv(_resolve(base, spec["path"])):
            t = int(r["timestamp"])
            if lo <= t < end:
                recs.append([t, float(r["rate"])])
        recs.sort(key=lambda r: r[0])
        funding[coin] = {"from_ms": lo, "to_ms": recs[-1][0] if recs else 0, "records": recs}
    script = plan.get("venue_script") or {}
    step_times = {_iso(t) for t in range(start, end, step_ms)}
    unmatched = sorted(at for at in script if at not in step_times)
    if unmatched:
        raise ReplayPlanError(f"venue_script keys {unmatched} are not step times of the window "
                              f"[{win['start']}, {win['end']}) every {int(win['step_seconds'])}s; "
                              f"a scripted outcome must apply to exactly one step")
    steps = []
    for t in range(start, end, step_ms):
        marks = {}
        for m in members:
            px = opens.get(m["coin"], {}).get(t)
            if px is not None:
                marks[m["coin"]] = px
        step = {"at": _iso(t), "marks": marks}
        extra = script.get(_iso(t))
        if extra:
            step.update({k: v for k, v in extra.items() if v})
        steps.append(step)
    spec = {
        "protocol": ADAPTER_PROTOCOL, "version": ADAPTER_VERSION, "mode": mode,
        "run": {"run_id": plan["run_id"], "state_dir": plan["state_dir"], "source": plan["source"]},
        "basket": plan["basket"], "members": members, "target_input": plan["target_input"],
        "lot_metadata": plan.get("lot_metadata") or {}, "candles": candles, "funding": funding, "steps": steps,
    }
    for key in ("cost_policy", "portfolio_risk"):
        if plan.get(key) is not None:
            spec[key] = plan[key]
    return spec


def run_adapter(binary, spec, timeout, stderr_path=None, version=ADAPTER_VERSION, extra=None, command="basket-replay"):
    blob = json.dumps(spec).encode()
    err = open(stderr_path, "wb") if stderr_path else subprocess.DEVNULL
    try:
        proc = subprocess.run([binary, command, "--spec", "-"] + list(extra or []), input=blob, stdout=subprocess.PIPE,
                              stderr=err, cwd=REPO_ROOT, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        return None, None, f"the adapter did not finish within {timeout}s"
    finally:
        if stderr_path:
            err.close()
    try:
        out = json.loads(proc.stdout.decode() or "null")
    except ValueError as exc:
        return proc.returncode, None, f"the adapter output is not JSON: {exc}"
    if not isinstance(out, dict) or out.get("protocol") != ADAPTER_PROTOCOL or out.get("version") != version:
        return proc.returncode, None, "the adapter output does not carry the replay protocol"
    return proc.returncode, out, None


def result_record(plan_sha, input_hashes, code, out, process_error):
    rec = {"schema": RESULT_SCHEMA, "version": RESULT_VERSION, "plan_sha256": plan_sha, "inputs": input_hashes,
           "adapter_exit_code": code, "scored": False, "promotion_eligible": False}
    if process_error:
        rec.update(status="process_error", refusal={"reason": "process_error", "detail": [process_error]})
        return rec
    rec["status"] = out.get("status")
    rec["adapter_output"] = out
    if out.get("status") != "complete" or code != 0:
        refusal = out.get("refusal") or {}
        detail = list(refusal.get("detail") or [])
        if out.get("error"):
            detail.append(out["error"])
        detail.extend((out.get("coverage") or {}).get("gaps") or [])
        rec["refusal"] = {"reason": refusal.get("reason") or f"status_{out.get('status')}", "detail": detail}
        return rec
    rec["scored"] = True
    rec["promotion_eligible"] = True
    curve = []
    for st in out.get("steps") or []:
        ex = st.get("exposure") or {}
        curve.append({"at": st["at"], "equity_usd": ex.get("basket_equity_usd"), "gross_usd": ex.get("gross_usd"),
                      "net_usd": ex.get("net_usd")})
    rec["equity_curve"] = curve
    last = (out.get("steps") or [{}])[-1]
    rec["members"] = last.get("members") or []
    rec["capital_model"] = out.get("capital_model")
    rec["cost"] = out.get("cost")
    return rec


def write_new(path, obj):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
    with os.fdopen(fd, "w") as fh:
        fh.write(json.dumps(obj, indent=2, sort_keys=True) + "\n")


def main(argv=None):
    ap = argparse.ArgumentParser(description="Run one offline basket replay through the Go adapter (issue 1791). "
                                             "Research-only: reads frozen files, writes one result record and the "
                                             "run's own temporary state, changes no live state.")
    ap.add_argument("--plan", required=True, help="go-trader.basket-replay-plan JSON")
    ap.add_argument("--binary", required=True, help="go-trader binary built from this checkout")
    ap.add_argument("--out", required=True, help="new result JSON path (never replaced)")
    ap.add_argument("--resume", action="store_true", help="resume the run from its saved adapter state")
    ap.add_argument("--timeout", type=int, default=None, help="adapter timeout in seconds (offline default 3600; a paper run has none unless given)")
    ap.add_argument("--stderr", help="optional path for the adapter log")
    ap.add_argument("--paper", action="store_true", help="record a live isolated paper run (basket-replay-paper) from a version-2 plan with a collection block")
    ap.add_argument("--config", help="absolute research egress file for --paper")
    ap.add_argument("--seal", action="store_true", help="seal a finished --paper run and export it")
    ap.add_argument("--max-steps", dest="max_steps", type=int, default=0, help="stop after this many further cycles; the run stays resumable")
    args = ap.parse_args(argv)
    if os.path.exists(args.out):
        print(f"basket_replay: {args.out} exists; a result record is never replaced", file=sys.stderr)
        return 2
    if args.paper != bool(args.config) or (args.seal and not args.paper) or args.max_steps < 0 or (args.timeout is not None and args.timeout <= 0):
        print("basket_replay: --paper needs --config, --config and --seal need --paper, and --max-steps and --timeout must be positive", file=sys.stderr)
        return 2
    timeout = args.timeout if args.timeout is not None else (None if args.paper else OFFLINE_TIMEOUT_S)
    plan_path = os.path.abspath(args.plan)
    base = os.path.dirname(plan_path)
    mode = "resume" if args.resume else "new"
    pair = False
    try:
        plan = read_plan(plan_path)
        pair = is_pair_plan(plan)
        plan = check_plan(plan)
        if pair:
            if args.paper != ("collection" in plan):
                raise ReplayPlanError("--paper needs a plan with a collection block, and a collection block needs --paper")
            hashes, research, objects = verify_pair_inputs(plan, base)
            spec = build_pair_spec(plan, base, mode, research, objects)
        elif args.paper:
            raise ReplayPlanError("--paper needs a version-2 pair plan")
        else:
            hashes = verify_inputs(plan, base)
            spec = build_spec(plan, base, mode)
    except (ReplayPlanError, OSError, ValueError, KeyError) as exc:
        rec = {"schema": RESULT_SCHEMA, "version": PAIR_RESULT_VERSION if pair else RESULT_VERSION, "status": "refused", "scored": False,
               "promotion_eligible": False, "refusal": {"reason": "plan_refused", "detail": [str(exc)]}}
        if pair:
            rec["mechanically_complete"] = False
        write_new(args.out, rec)
        print(f"basket_replay: refused: {exc}", file=sys.stderr)
        return 3
    if pair:
        extra = ["--max-steps", str(args.max_steps)] if args.max_steps else []
        command = "basket-replay"
        if args.paper:
            command = "basket-replay-paper"
            extra = ["--config", os.path.abspath(args.config)] + (["--seal"] if args.seal else []) + extra
        code, out, err = run_adapter(os.path.abspath(args.binary), spec, timeout, args.stderr, PAIR_ADAPTER_VERSION, extra, command)
        rec = pair_result_record(_sha256_file(plan_path), hashes, code, out, err)
        if args.paper:
            rec["paper"] = True
            rec["recording"] = (out or {}).get("recording")
        write_new(args.out, rec)
        print(f"basket_replay: status={rec['status']} mechanically_complete={rec['mechanically_complete']} scored=False out={args.out}")
        return 0 if rec["mechanically_complete"] else 1
    code, out, err = run_adapter(os.path.abspath(args.binary), spec, timeout, args.stderr, extra=["--max-steps", str(args.max_steps)] if args.max_steps else None)
    rec = result_record(_sha256_file(plan_path), hashes, code, out, err)
    write_new(args.out, rec)
    print(f"basket_replay: status={rec['status']} scored={rec['scored']} out={args.out}")
    return 0 if rec["scored"] else 1


if __name__ == "__main__":
    sys.exit(main())
