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
"""

import argparse
import csv
import datetime
import gzip
import hashlib
import io
import json
import os
import subprocess
import sys

PLAN_SCHEMA = "go-trader.basket-replay-plan"
PLAN_VERSION = 1
RESULT_SCHEMA = "go-trader.basket-replay-result"
RESULT_VERSION = 1
ADAPTER_PROTOCOL = "go-trader.basket-replay"
ADAPTER_VERSION = 1
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


def load_plan(path):
    with open(path) as fh:
        plan = json.load(fh)
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


def run_adapter(binary, spec, timeout, stderr_path=None):
    blob = json.dumps(spec).encode()
    err = open(stderr_path, "wb") if stderr_path else subprocess.DEVNULL
    try:
        proc = subprocess.run([binary, "basket-replay", "--spec", "-"], input=blob, stdout=subprocess.PIPE,
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
    if not isinstance(out, dict) or out.get("protocol") != ADAPTER_PROTOCOL or out.get("version") != ADAPTER_VERSION:
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
    ap.add_argument("--timeout", type=int, default=3600, help="adapter timeout in seconds")
    ap.add_argument("--stderr", help="optional path for the adapter log")
    args = ap.parse_args(argv)
    if os.path.exists(args.out):
        print(f"basket_replay: {args.out} exists; a result record is never replaced", file=sys.stderr)
        return 2
    plan_path = os.path.abspath(args.plan)
    base = os.path.dirname(plan_path)
    try:
        plan = load_plan(plan_path)
        hashes = verify_inputs(plan, base)
        spec = build_spec(plan, base, "resume" if args.resume else "new")
    except (ReplayPlanError, OSError, ValueError, KeyError) as exc:
        rec = {"schema": RESULT_SCHEMA, "version": RESULT_VERSION, "status": "refused", "scored": False,
               "promotion_eligible": False, "refusal": {"reason": "plan_refused", "detail": [str(exc)]}}
        write_new(args.out, rec)
        print(f"basket_replay: refused: {exc}", file=sys.stderr)
        return 3
    code, out, err = run_adapter(os.path.abspath(args.binary), spec, args.timeout, args.stderr)
    rec = result_record(_sha256_file(plan_path), hashes, code, out, err)
    write_new(args.out, rec)
    print(f"basket_replay: status={rec['status']} scored={rec['scored']} out={args.out}")
    return 0 if rec["scored"] else 1


if __name__ == "__main__":
    sys.exit(main())
