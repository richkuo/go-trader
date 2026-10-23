#!/usr/bin/env python3
import argparse
import bisect
import hashlib
import itertools
import json
import os
import random
import sys
from collections import Counter, defaultdict
from fractions import Fraction
from typing import Dict, List, Optional

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
for _p in (os.path.join(ROOT, "backtest"), os.path.join(ROOT, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import quote_replay as q
from inventory_market_making import spec_sha256

SOURCES = (
    "backtest/candidates/inventory_market_making_1636/run_study.py",
    "backtest/candidates/inventory_market_making_1636/protocol-lock.json",
    "backtest/quote_replay.py",
    "backtest/book_replay.py",
    "backtest/trade_flow_replay.py",
    "shared_strategies/open/inventory_market_making.py",
    "scheduler/quote_policy.go",
    "scheduler/quote_policy_spec.go",
    "scheduler/quote_replay.go",
    "scheduler/quote_paper_venue.go",
    "scheduler/quote_coordinator.go",
    "scheduler/quote_settlement.go",
)
DAY_MS = 86_400_000


def sha_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def rel(path: str) -> str:
    return os.path.relpath(os.path.abspath(path), ROOT)


def load_json(path: str):
    with open(path) as fh:
        return json.load(fh)


def check_inputs(proto: dict, manifest: dict, binary: Optional[str]) -> dict:
    missing: List[str] = []
    audit = {"policy_spec_sha256": spec_sha256(), "sources": {s: sha_file(os.path.join(ROOT, s)) for s in SOURCES}}
    if proto["policy_spec_sha256"] != spec_sha256():
        raise SystemExit("the protocol lock names a different policy specification than the registry")
    disc = q.discover(binary) if binary else q.discover()
    audit["discovery_spec_sha256"] = disc["spec_sha256"]
    audit["binary_discovery"] = disc["binary"]
    if not binary:
        missing.append("no go-trader binary was supplied, so no replay could run")
    for platform in ("spot", "futures"):
        before = os.path.join(HERE, f"discovery_before_{platform}.json")
        after = os.path.join(HERE, f"discovery_after_{platform}.json")
        audit[f"discovery_{platform}_unchanged"] = sha_file(before) == sha_file(after)
    lot = proto.get("lot_evidence") or {}
    if lot.get("path"):
        meta = load_json(os.path.join(ROOT, lot["path"]))
        for coin in proto["coins"]:
            got = meta["assets"][coin]["szDecimals"]
            if got != proto["lot"][coin]:
                raise SystemExit(f"the pinned venue meta gives {coin} szDecimals {got}, the protocol {proto['lot'][coin]}")
        audit["lot_evidence_sha256"] = sha_file(os.path.join(ROOT, lot["path"]))
    recordings = []
    for r in manifest.get("recordings") or []:
        path = os.path.join(ROOT, r["path"]) if not os.path.isabs(r["path"]) else r["path"]
        mpath = os.path.join(path, "run.manifest.json")
        if not os.path.isfile(mpath):
            missing.append(f"recording {r['path']} is not present (pinned manifest sha256 {r['manifest_sha256']})")
            continue
        if sha_file(mpath) != r["manifest_sha256"]:
            raise SystemExit(f"recording {r['path']} does not match its pinned manifest sha256")
        recordings.append(dict(r, abspath=path))
    funding = manifest.get("funding")
    if funding:
        fpath = os.path.join(ROOT, funding["path"]) if not os.path.isabs(funding["path"]) else funding["path"]
        if not os.path.isfile(fpath):
            missing.append(f"funding file {funding['path']} is not present")
            funding = None
        elif sha_file(fpath) != funding["sha256"]:
            raise SystemExit(f"funding file {funding['path']} does not match its pinned sha256")
        else:
            funding = dict(funding, abspath=fpath)
    if not recordings:
        missing.append("no pinned recording with raw executed trades and l2Book depth exists: queue-aware evidence needs a "
                       "bounded public `go-trader record-observations --kinds l2_book,trade_flow` recording of at least "
                       f"{proto['gates']['min_scored_days']} complete UTC days (the archive holds depth only)")
    if not funding:
        missing.append("no pinned public fundingHistory records cover the recording window")
    for key, text in sorted((proto.get("evidence") or {}).items()):
        if not text.get("verified"):
            missing.append(f"{key}: {text['note']}")
    return {"missing": missing, "audit": audit, "recordings": recordings, "funding": funding}


def eligible(proto: dict, recordings: List[dict]) -> List[dict]:
    out = []
    cycle = int(proto["config"]["interval_seconds"])
    warm = int(proto["base_params"]["volatility_window_seconds"]) * 1000 + 1000 + int(proto["base_params"]["max_input_age_ms"])
    cap = int(proto["eligibility"]["max_deadlines_per_run"])
    for r in recordings:
        rec = q.load_recording(r["abspath"], r["coin"])
        if rec["refusals"]:
            out.append({"recording": r, "rec": rec, "runs": [], "refusals": rec["refusals"]})
            continue
        runs = []
        for w in q.eligible_windows(rec, cycle, warm, int(proto["eligibility"]["min_window_minutes"]) * 60_000):
            first = w["first_deadline_ms"]
            while first <= w["last_deadline_ms"]:
                last = min(w["last_deadline_ms"], first + (cap - 1) * cycle * 1000)
                runs.append({"first_deadline_ms": first, "last_deadline_ms": last})
                first = last + cycle * 1000 + warm + cycle * 1000
                first = -(-first // (cycle * 1000)) * (cycle * 1000)
        out.append({"recording": r, "rec": rec, "runs": runs, "refusals": []})
    return out


def assign_splits(proto: dict, groups: List[dict]) -> Dict[str, List[dict]]:
    runs = [(run["first_deadline_ms"], g, run) for g in groups for run in g["runs"]]
    runs.sort(key=lambda x: (x[0], x[1]["recording"]["path"]))
    total = sum(r[2]["last_deadline_ms"] - r[2]["first_deadline_ms"] for r in runs)
    out = {"train": [], "validation": [], "held_out": []}
    acc = 0
    for first, g, run in runs:
        frac = acc / total if total else 0
        split = "train" if frac < proto["splits"]["train"] else ("validation" if frac < proto["splits"]["train"] + proto["splits"]["validation"] else "held_out")
        out[split].append({"group": g, "run": dict(run, label=split)})
        acc += run["last_deadline_ms"] - run["first_deadline_ms"]
    return out


def variants(proto: dict) -> List[dict]:
    grid = proto["grid"]
    keys = sorted(grid)
    out = []
    for combo in itertools.product(*(grid[k] for k in keys)):
        params = dict(zip(keys, combo))
        out.append({"name": ",".join(f"{k}={v}" for k, v in params.items()), "params": params})
    return out


def baselines(proto: dict) -> List[dict]:
    b = proto["baseline"]
    return [{"name": f"fixed_spread,min_spread_bps={m}", "params": {"risk_aversion": b["risk_aversion"], "volatility_multiplier": b["volatility_multiplier"],
                                                                     "min_spread_bps": m}} for m in b["min_spread_bps_grid"]]


def run_metrics(plan: dict, out: dict, ver: dict) -> dict:
    marks = {}
    for d in out.get("decisions") or []:
        if d.get("mark"):
            marks[d["deadline_ms"]] = float(d["mark"]["price"])
    deadlines = sorted(marks)
    fills = sorted(ver.get("_fills", []), key=lambda f: f["time_ms"])
    led = q.IndependentLedger(Fraction(int(q.QUOTE_POLICY_SPEC["fixed"]["capital"])))
    pays = sorted((p["time"], Fraction(p["amount"])) for p in ver["funding_payments"])
    equity, fi, pi = [], 0, 0
    for d in deadlines:
        while fi < len(fills) and fills[fi]["time_ms"] <= d:
            f = fills[fi]
            led.apply(f["side"], Fraction(f["size"]), Fraction(f["px"]), Fraction(f["fee"]), f["time_ms"])
            fi += 1
        while pi < len(pays) and pays[pi][0] <= d:
            led.cash += pays[pi][1]
            pi += 1
        sign = 1 if led.qty > 0 else -1 if led.qty < 0 else 0
        equity.append((d, float(led.cash) + float(led.qty) * marks[d] - float(led.cost) * sign))
    book = {k: Fraction(v) for k, v in ver["book"].items()}
    last_mark = marks[deadlines[-1]] if deadlines else 0.0
    mtm = float(book["qty"]) * last_mark - float(book["cost"]) * (1 if book["qty"] > 0 else -1 if book["qty"] < 0 else 0)
    taker = float(Fraction(plan["costs"]["taker_fee_bps"]) / 10000 + Fraction(plan["costs"]["slippage_reserve_bps"]) / 10000)
    terminal = abs(float(book["qty"])) * last_mark * taker
    initial = float(q.QUOTE_POLICY_SPEC["fixed"]["capital"])
    net_realized = float(book["cash"]) - initial
    peak, dd = initial, 0.0
    for _, e in equity:
        peak = max(peak, e)
        dd = max(dd, peak - e)
    kinds = Counter(e["kind"] for e in out.get("venue_events") or [])
    rejects = sum(1 for e in out.get("venue_events") or [] if e["kind"] == "place" and e["event"]["outcome"]["status"] == "rejected")
    roles = Counter(e["event"].get("liquidity", "") + ":" + e["event"].get("role", "") for e in out.get("venue_events") or [] if e["kind"] == "fill")
    turnover = sum(float(Fraction(f["size"]) * Fraction(f["px"])) for f in fills)
    durations = [rt["closed_ms"] - rt["opened_ms"] for rt in ver["round_trips"]]
    day = defaultdict(float)
    prev = initial
    for d, e in equity:
        day[d // DAY_MS] += e - prev
        prev = e
    book_recv = [b["recv_ms"] for b in plan["books"]]
    adverse = []
    for f in fills:
        if not f.get("_maker"):
            continue
        i = bisect.bisect_right(book_recv, f["time_ms"] + 5_000) - 1
        if i < 0:
            continue
        b = plan["books"][i]
        mid = (float(b["bids"][0][0]) + float(b["asks"][0][0])) / 2
        px = float(f["px"])
        adverse.append((px - mid) / px * 10_000 if f["side"] == "buy" else (mid - px) / px * 10_000)
    return {"deadlines": len(out.get("decisions") or []), "status": out.get("status"), "scored": bool(out.get("scored")),
            "unscored": out.get("unscored_reasons") or [], "ledger_ok": ver["ok"], "ledger_problems": ver["problems"][:5],
            "gross_realized": float(book["realized_gross"]), "fees": float(book["fees"]), "funding": float(book["funding"]),
            "net_realized": net_realized, "end_inventory": str(book["qty"]), "end_mark_to_market": mtm,
            "terminal_close_cost": terminal, "net_after_terminal_close": net_realized + mtm - terminal,
            "max_drawdown": dd, "round_trips": len(ver["round_trips"]), "fills": len(fills), "fill_roles": dict(roles),
            "rejects": rejects, "cancels": kinds.get("cancel", 0), "venue_cancels": kinds.get("venue_cancel", 0), "turnover": turnover,
            "mean_round_trip_ms": (sum(durations) / len(durations)) if durations else None,
            "adverse_selection_bps_5s": (sum(adverse) / len(adverse)) if adverse else None, "day_pnl": {str(k): v for k, v in sorted(day.items())},
            "timing": out.get("timing"), "pending_fills": len(ver["pending_fills"]), "pending_funding": len(ver["pending_funding"])}


def replay(binary: str, proto: dict, entry: dict, funding: List[dict], params: dict, label: str, work: str, costs=None, latency=None) -> dict:
    g, run = entry["group"], entry["run"]
    run_id = hashlib.sha256(json.dumps([g["recording"]["path"], run, params, label], sort_keys=True).encode()).hexdigest()[:24]
    os.makedirs(os.path.join(work, "state"), exist_ok=True)
    state = os.path.join(work, "state", run_id)
    plan = q.build_plan(g["rec"], proto, run, params, run_id, state, funding, costs=costs, latency=latency,
                        source={"recording": g["recording"]["path"], "manifest_sha256": g["recording"]["manifest_sha256"]})
    path = os.path.join(work, "plans", run_id + ".json")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as fh:
        json.dump(plan, fh)
    code, out, err = q.run_plan(binary, path)
    ver = q.verify(plan, out) if out.get("decisions") is not None else {"ok": False, "problems": [f"replay exit {code}: {err[-300:]}"], "book": {},
                                                                     "round_trips": [], "funding_payments": [], "pending_fills": [], "pending_funding": []}
    maker = {(e["event"]["fill"]["order_id"], e["event"]["fill"]["trade_id"]): e["event"].get("liquidity") == "maker"
             for e in out.get("venue_events") or [] if e["kind"] == "fill"}
    ver["_fills"] = [dict(f, _maker=maker.get((f["order_id"], f["trade_id"]), False)) for f in out.get("fills") or []]
    m = run_metrics(plan, out, ver) if ver["book"] else {"deadlines": 0, "status": out.get("status"), "scored": False,
                                                         "unscored": [f"replay exit {code}"], "ledger_ok": False, "net_after_terminal_close": 0.0,
                                                         "round_trips": 0, "day_pnl": {}, "max_drawdown": 0.0}
    m.update({"label": label, "run_id": run_id, "window": run, "params": params, "exit": code, "plan_sha256": sha_file(path)})
    return m


def aggregate(runs: List[dict]) -> dict:
    day = defaultdict(float)
    for r in runs:
        for k, v in r.get("day_pnl", {}).items():
            day[k] += v
    return {"net": sum(r.get("net_after_terminal_close", 0.0) for r in runs), "round_trips": sum(r.get("round_trips", 0) for r in runs),
            "max_drawdown": max([r.get("max_drawdown", 0.0) for r in runs] or [0.0]), "days": dict(sorted(day.items())),
            "scored": all(r.get("scored") and r.get("ledger_ok") for r in runs) and bool(runs),
            "unscored": sorted({u for r in runs for u in r.get("unscored", [])} | {p for r in runs for p in r.get("ledger_problems", [])})}


def bootstrap_lower(days: List[float], seed: int, n: int, block: int, level: float) -> Optional[float]:
    if not days:
        return None
    rng = random.Random(seed)
    k = len(days)
    stats = []
    for _ in range(n):
        sample = []
        while len(sample) < k:
            start = rng.randrange(k)
            sample.extend(days[(start + j) % k] for j in range(block))
        stats.append(sum(sample[:k]))
    stats.sort()
    return stats[int((1 - level) / 2 * n)]


def study(proto_path: str, binary: Optional[str], out_dir: str, manifest_path: Optional[str]) -> dict:
    proto = load_json(proto_path)
    manifest_path = manifest_path or os.path.join(ROOT, proto["data_manifest"])
    manifest = load_json(manifest_path)
    inputs = check_inputs(proto, manifest, binary)
    result = {"candidate": proto["candidate"], "issue": proto["issue"], "protocol_sha256": sha_file(proto_path),
              "data_manifest_sha256": sha_file(manifest_path), "audit": inputs["audit"], "missing_inputs": list(inputs["missing"]),
              "assumptions": proto["assumptions"], "gates": proto["gates"], "variants": [], "baselines": [], "selected": None,
              "held_out": None, "stress": None, "verdict": "INCONCLUSIVE", "reasons": []}
    work = os.path.abspath(out_dir)
    os.makedirs(work, exist_ok=True)
    groups = eligible(proto, inputs["recordings"]) if inputs["recordings"] and binary and inputs["funding"] else []
    for g in groups:
        result["missing_inputs"] += [f"recording {g['recording']['path']}: {r}" for r in g["refusals"]]
    splits = assign_splits(proto, groups) if groups else {"train": [], "validation": [], "held_out": []}
    result["splits"] = {k: [e["run"] for e in v] for k, v in splits.items()}
    if not all(splits.values()):
        result["reasons"].append("no eligible data fills all three chronological splits")
    if binary and all(splits.values()):
        funding = q.funding_records(inputs["funding"]["abspath"], proto["coins"][0])
        for v in variants(proto):
            runs = [replay(binary, proto, e, funding, v["params"], "train:" + v["name"], work) for e in splits["train"]]
            result["variants"].append({"name": v["name"], "params": v["params"], "train": aggregate(runs)})
        for b in baselines(proto):
            runs = [replay(binary, proto, e, funding, b["params"], "train:" + b["name"], work) for e in splits["train"]]
            result["baselines"].append({"name": b["name"], "params": b["params"], "train": aggregate(runs)})
        best = sorted(result["variants"], key=lambda v: (-v["train"]["net"], v["name"]))[0]
        base = sorted(result["baselines"], key=lambda v: (-v["train"]["net"], v["name"]))[0]
        val = aggregate([replay(binary, proto, e, funding, best["params"], "validation:" + best["name"], work) for e in splits["validation"]])
        held = aggregate([replay(binary, proto, e, funding, best["params"], "held_out:" + best["name"], work) for e in splits["held_out"]])
        held_base = aggregate([replay(binary, proto, e, funding, base["params"], "held_out:" + base["name"], work) for e in splits["held_out"]])
        stress = aggregate([replay(binary, proto, e, funding, best["params"], "stress:" + best["name"], work, costs=proto["stress_costs"],
                                   latency=proto["stress_latency"]) for e in splits["held_out"]])
        result["selected"] = {"name": best["name"], "params": best["params"], "validation": val}
        result["baseline_selected"] = {"name": base["name"], "params": base["params"]}
        result["held_out"] = {"candidate": held, "baseline": held_base}
        result["stress"] = stress
        gates = proto["gates"]
        days = sorted(set(held["days"]) | set(held_base["days"]))
        diff = [held["days"].get(d, 0.0) - held_base["days"].get(d, 0.0) for d in days]
        cand = [held["days"].get(d, 0.0) for d in days]
        bs = gates["bootstrap"]
        result["bootstrap"] = {f"block_{b}": {"candidate_lower": bootstrap_lower(cand, bs["seed"], bs["resamples"], b, bs["level"]),
                                              "improvement_lower": bootstrap_lower(diff, bs["seed"], bs["resamples"], b, bs["level"])} for b in bs["block_days"]}
        if not held["scored"] or not held_base["scored"] or not stress["scored"]:
            result["reasons"].append("a held-out, baseline or stress run is unscored: " + "; ".join((held["unscored"] + held_base["unscored"] + stress["unscored"])[:6]))
        if held["round_trips"] < gates["min_round_trips"]:
            result["reasons"].append(f"{held['round_trips']} completed held-out round trips < {gates['min_round_trips']}")
        if len(days) < gates["min_scored_days"]:
            result["reasons"].append(f"{len(days)} scored held-out UTC days < {gates['min_scored_days']}")
    if result["missing_inputs"]:
        result["reasons"] = list(result["missing_inputs"]) + result["reasons"]
    if not result["reasons"] and result["held_out"]:
        held, base, stress = result["held_out"]["candidate"], result["held_out"]["baseline"], result["stress"]
        initial = float(q.QUOTE_POLICY_SPEC["fixed"]["capital"])
        lows = [v for b in result["bootstrap"].values() for v in b.values()]
        passed = (held["net"] > 0 and held["net"] > base["net"] and all(v is not None and v > 0 for v in lows)
                  and held["max_drawdown"] <= proto["gates"]["max_drawdown_pct_of_initial_equity"] / 100 * initial and stress["net"] > 0)
        result["verdict"] = "PASS" if passed else "FAIL"
    result["edge_source"] = {"PASS": "unvalidated", "FAIL": "study_fail", "INCONCLUSIVE": "study_inconclusive"}[result["verdict"]]
    with open(os.path.join(work, "results.json"), "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True)
        fh.write("\n")
    with open(os.path.join(work, "REPORT.md"), "w") as fh:
        fh.write(render(result, proto))
    return result


def render(result: dict, proto: dict) -> str:
    lines = ["# inventory_market_making study (issue 1861)", "",
             f"Verdict: **{result['verdict']}**. Registry: `inventory_market_making` stays `no_edge` and `production_refused`; edge_source `{result['edge_source']}`. "
             "A PASS would describe only this frozen simulation and would give no live promotion.", ""]
    if result["reasons"]:
        lines += ["## Why", ""] + [f"- {r}" for r in result["reasons"]] + [""]
    lines += ["## Frozen protocol", "",
              f"- Protocol lock sha256 `{result['protocol_sha256']}`; data manifest sha256 `{result['data_manifest_sha256']}`.",
              f"- Policy specification sha256 `{result['audit']['policy_spec_sha256']}` (registry and discovery agree; binary discovery: {result['audit']['binary_discovery'] or 'not run'}).",
              f"- Coins {', '.join(proto['coins'])}; cycle {proto['config']['interval_seconds']} s; cap USD {proto['config']['max_inventory_usd']}; stop {proto['config']['stop_loss_pct']}%; capital USD {q.QUOTE_POLICY_SPEC['fixed']['capital']}, leverage {q.QUOTE_POLICY_SPEC['fixed']['leverage']}.",
              f"- Costs: maker {proto['costs']['maker_fee_bps']} bps, taker {proto['costs']['taker_fee_bps']} bps, slippage reserve {proto['costs']['slippage_reserve_bps']} bps, minimum order USD {proto['costs']['min_order_value_usd']}; stress doubles them with {proto['stress_latency']['place_ms']} ms latency.",
              f"- Queue model `{proto['queue']['model']}`, participation {proto['queue']['participation']} of each trade; latency {proto['latency']['place_ms']} ms place, {proto['latency']['cancel_ms']} ms cancel.",
              f"- Splits {proto['splits']}; eligibility: {proto['eligibility']['rule']}.",
              f"- Grid {proto['grid']} ({len(variants(proto))} variants); fixed-spread baseline {proto['baseline']} fitted on training only.",
              f"- Gates: {proto['gates']['min_round_trips']} held-out round trips, {proto['gates']['min_scored_days']} scored UTC days, positive held-out net and improvement, positive lower bootstrap bounds (seed {proto['gates']['bootstrap']['seed']}, {proto['gates']['bootstrap']['resamples']} resamples, blocks {proto['gates']['bootstrap']['block_days']} days), drawdown at most {proto['gates']['max_drawdown_pct_of_initial_equity']}% of initial equity, positive stress net.",
              "- Circuit-breaker and portfolio kill-switch triggers are frozen off in the runner, so their exits are outside these economics; the retained quote integration tests cover the latch branches.", "",
              "## Assumptions", ""] + [f"- `{k}`: {v}" for k, v in sorted(proto["assumptions"].items())] + [""]
    lines += ["## Results", ""]
    if not result["variants"]:
        lines += ["No variant was scored: no eligible input. The executable study, the integration evidence and this report are delivered; "
                  "no synthetic profitability evidence is reported.", ""]
    else:
        lines += ["| variant | train net USD | train round trips |", "|---|---|---|"]
        for v in result["variants"] + result["baselines"]:
            lines.append(f"| `{v['name']}` | {v['train']['net']:.4f} | {v['train']['round_trips']} |")
        held = result["held_out"]
        lines += ["", f"Selected `{result['selected']['name']}`; baseline `{result['baseline_selected']['name']}`.",
                  f"Held-out net {held['candidate']['net']:.4f} (baseline {held['baseline']['net']:.4f}), round trips {held['candidate']['round_trips']}, "
                  f"max drawdown {held['candidate']['max_drawdown']:.4f}; stress net {result['stress']['net']:.4f}; bootstrap {result['bootstrap']}.", ""]
    lines += ["## Source pins", ""] + [f"- `{k}` `{v}`" for k, v in sorted(result["audit"]["sources"].items())] + [""]
    lines += ["## Reproduce", "", "```bash", "BIN=\"$(mktemp -d)/go-trader\"", "go -C scheduler build -o \"$BIN\" .",
              "uv run --no-sync python backtest/candidates/inventory_market_making_1636/run_study.py \\",
              "    --protocol backtest/candidates/inventory_market_making_1636/protocol-lock.json \\",
              "    --binary \"$BIN\" --out-dir \"$(mktemp -d)\"", "```", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Frozen economic study for the production-refused inventory_market_making candidate (#1861)")
    ap.add_argument("--protocol", required=True)
    ap.add_argument("--binary")
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--data-manifest")
    args = ap.parse_args(argv)
    result = study(args.protocol, args.binary, args.out_dir, args.data_manifest)
    print(json.dumps({"verdict": result["verdict"], "reasons": result["reasons"][:6], "out_dir": os.path.abspath(args.out_dir)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
