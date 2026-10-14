#!/usr/bin/env python3
import argparse
import hashlib
import itertools
import json
import math
import os
import statistics
import sys
from concurrent.futures import ProcessPoolExecutor

_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))
for _p in (os.path.join(_REPO, "backtest"), os.path.join(_REPO, "shared_tools"),
           os.path.join(_REPO, "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

import offline_manifest as om
from eval_windows import compute_incumbent_legs, incumbent_bars, run_leg, score_candidate

SPEC_PATH = os.path.join(_HERE, "candidate_spec.json")
RESULTS_PATH = os.path.join(_HERE, "results.json")
PARITY_PATH = os.path.join(_HERE, "parity_results.json")
REPORT_PATH = os.path.join(_HERE, "REPORT.md")
NAME = "bitcoin_lead_lag"
GRID_KEYS = ("lag_bars", "horizon_bars", "training_bars", "impulse_z", "min_r2", "min_edge_bps")
BASELINE_KEYS = ("lag_bars", "horizon_bars", "training_bars", "impulse_z")
SOURCE_FILES = (
    "shared_strategies/open/bitcoin_lead_lag.py",
    "shared_strategies/open/registry.py",
    "shared_tools/lead_lag_inputs.py",
    "shared_tools/closed_bar.py",
    "shared_tools/market_payload.py",
    "shared_tools/strategy_composition.py",
    "backtest/lead_lag_history.py",
    "backtest/eval_windows.py",
    "backtest/backtester.py",
    "backtest/offline_manifest.py",
    "backtest/candidates/bitcoin_lead_lag_1640/run_study.py",
    "backtest/candidates/bitcoin_lead_lag_1640/candidate_spec.json",
    "backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json",
    "backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json",
)


def load_spec() -> dict:
    with open(SPEC_PATH) as fh:
        return json.load(fh)


SPEC = load_spec()


def protocol_sha256() -> str:
    return hashlib.sha256(json.dumps(SPEC, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()


def file_sha256(rel: str) -> str:
    return om.sha256_file(os.path.join(_REPO, rel))


def manifest_path(study: str) -> str:
    return os.path.join(_REPO, SPEC["studies"][study]["manifest"])


def grid_points(study: str, arm: str) -> list:
    grid = SPEC["studies"][study]["grid"]
    mode = SPEC["arms"][arm]["signal_mode"]
    out, seen = [], set()
    for values in itertools.product(*(grid[k] for k in GRID_KEYS)):
        params = dict(zip(GRID_KEYS, values))
        params.update(SPEC["fixed_params"])
        params["signal_mode"] = mode
        key = tuple(params[k] for k in (GRID_KEYS if mode == "lead_lag" else BASELINE_KEYS))
        if key in seen:
            continue
        seen.add(key)
        out.append(params)
    return out


def contract() -> dict:
    c = SPEC["contract"]
    return {"close_strategies": json.loads(json.dumps(c["close_strategies"])), "direction": c["direction"],
            "stop_loss_atr_mult": c["stop_loss_atr_mult"], "capital": c["capital"]}


class DelayedRegistry:
    def __init__(self, reg, delay: int):
        self._reg = reg
        self._delay = int(delay)

    def __getattr__(self, item):
        return getattr(self._reg, item)

    def apply_strategy(self, name, df, params=None):
        out = self._reg.apply_strategy(name, df, params)
        if self._delay and name == NAME:
            out = out.copy()
            out["signal"] = out["signal"].shift(self._delay).fillna(0).astype(np.int64)
        return out


_WORKER = {}


def _worker_init():
    from registry_loader import load_registry
    _WORKER["reg"] = load_registry("futures")
    _WORKER["manifests"] = {s: om.load_manifest(manifest_path(s)) for s in SPEC["studies"]}


def _positions(leg: dict) -> list:
    out = []
    for p in (leg.get("execution") or {}).get("position_list") or []:
        out.append({"entry": p["entry_date"], "exit": p["exit_date"], "side": p["side"],
                    "net_pnl": round(float(p["net_pnl"]), 6), "fees": round(float(p["fees"]), 6),
                    "bars_held": int(p["bars_held"])})
    return out


def run_task(task: dict) -> dict:
    if not _WORKER:
        _worker_init()
    reg = _WORKER["reg"]
    if task.get("delay"):
        reg = DelayedRegistry(reg, task["delay"])
    manifest = _WORKER["manifests"][task["study"]]
    c = contract()
    leg = run_leg(reg, NAME, dict(task["params"]), task["follower"], manifest["interval"], (None, None),
                  capital=c["capital"], close_strategies=c["close_strategies"], direction=c["direction"],
                  stop_loss_atr_mult=c["stop_loss_atr_mult"],
                  manifest_ctx={"manifest": manifest, "window": task["window"], "cost_multiplier": task["cost"]})
    ex = leg.get("execution") or {}
    info = leg.get("manifest") or {}
    return {
        "key": task["key"],
        "sharpe": leg["sharpe"], "return_pct": leg["return_pct"], "max_dd_pct": leg["max_dd_pct"],
        "trades": leg["trades"], "liquidated": bool(leg.get("liquidated")),
        "funding_incomplete": bool(leg.get("funding_incomplete")),
        "positions": int(ex.get("positions") or 0), "long_positions": int(ex.get("long_positions") or 0),
        "short_positions": int(ex.get("short_positions") or 0), "turnover": ex.get("turnover"),
        "fees_usd": ex.get("fees_usd"), "funding_pnl_usd": ex.get("funding_pnl_usd"),
        "net_pnl_usd": ex.get("net_pnl_usd"), "rejected_entries": ex.get("rejected_entries"),
        "position_list": _positions(leg) if task.get("keep_positions") else None,
        "candle_coverage": info.get("candle_coverage"), "funding_coverage": info.get("funding_coverage"),
        "leader": info.get("leader"),
    }


def _task_key(study, arm, window, cost, delay, follower, params) -> str:
    return json.dumps([study, arm, window, cost, delay, follower, [params[k] for k in GRID_KEYS]],
                      separators=(",", ":"))


def run_tasks(tasks: list, workers: int) -> dict:
    out = {}
    if workers <= 1:
        for t in tasks:
            r = run_task(t)
            out[r["key"]] = r
        return out
    with ProcessPoolExecutor(max_workers=workers, initializer=_worker_init) as pool:
        for r in pool.map(run_task, tasks, chunksize=1):
            out[r["key"]] = r
    return out


def make_tasks(study, arm, window, cost, delay, params_list, keep_positions=False) -> list:
    tasks = []
    for params in params_list:
        for follower in SPEC["followers"]:
            tasks.append({"key": _task_key(study, arm, window, cost, delay, follower, params), "study": study,
                          "arm": arm, "window": window, "cost": cost, "delay": delay, "follower": follower,
                          "params": params, "keep_positions": keep_positions})
    return tasks


def summarize(results: dict, study, arm, window, cost, delay, params) -> dict:
    legs = {f: results[_task_key(study, arm, window, cost, delay, f, params)] for f in SPEC["followers"]}
    summary = {
        "params": {k: params[k] for k in GRID_KEYS},
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs.values()), 6),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs.values()), 6),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in legs.values()), 6),
        "positions": sum(l["positions"] for l in legs.values()),
        "long_positions": sum(l["long_positions"] for l in legs.values()),
        "short_positions": sum(l["short_positions"] for l in legs.values()),
        "liquidated": any(l["liquidated"] for l in legs.values()),
        "funding_incomplete": any(l["funding_incomplete"] for l in legs.values()),
        "legs": {f: {k: v for k, v in l.items() if k not in ("key", "position_list")} for f, l in legs.items()},
    }
    if all(l["position_list"] is not None for l in legs.values()):
        positions = [dict(p, follower=f) for f, l in legs.items() for p in l["position_list"]]
        summary["clusters"] = independent_clusters(positions)
        summary["concentration"] = concentration(positions)
        summary["position_list"] = sorted(positions, key=lambda p: (p["entry"], p["follower"]))
    return summary


def independent_clusters(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry"]), pd.Timestamp(p["exit"])) for p in positions)
    count, latest = 0, None
    for entry, exit_ in spans:
        if latest is None or entry > latest:
            count += 1
            latest = exit_
        else:
            latest = max(latest, exit_)
    return count


def concentration(positions: list) -> dict:
    pnl = sorted((p["net_pnl"] for p in positions), reverse=True)
    gains = [x for x in pnl if x > 0]
    total = sum(pnl)
    return {
        "positions": len(pnl),
        "net_pnl_usd": round(total, 6),
        "top5_share_of_gains": round(sum(gains[:5]) / sum(gains), 6) if gains else None,
        "largest_gain_usd": round(gains[0], 6) if gains else None,
        "largest_loss_usd": round(pnl[-1], 6) if pnl and pnl[-1] < 0 else None,
        "win_rate": round(sum(1 for x in pnl if x > 0) / len(pnl), 6) if pnl else None,
    }


def eligible(summary: dict) -> bool:
    return (not summary["liquidated"] and not summary["funding_incomplete"]
            and summary["positions"] >= 20)


def select(rows: list):
    best = None
    for order, row in enumerate(rows):
        if not eligible(row):
            continue
        rank = (-row["mean_sharpe"], -row["mean_return_pct"], order)
        if best is None or rank < best[0]:
            best = (rank, row)
    return None if best is None else best[1]


def neighbors(study: str, selected: dict, rows: list) -> dict:
    grid = SPEC["studies"][study]["grid"]
    by_params = {tuple(r["params"][k] for k in GRID_KEYS): r for r in rows}
    base = [selected["params"][k] for k in GRID_KEYS]
    found = []
    for i, k in enumerate(GRID_KEYS):
        values = grid[k]
        pos = values.index(base[i])
        for step in (-1, 1):
            j = pos + step
            if 0 <= j < len(values):
                probe = list(base)
                probe[i] = values[j]
                row = by_params.get(tuple(probe))
                if row is not None:
                    found.append(row)
    positive = sum(1 for r in found if r["mean_return_pct"] > 0)
    return {"neighbors": len(found), "positive_return": positive,
            "mean_return_pct": round(statistics.mean(r["mean_return_pct"] for r in found), 6) if found else None}


def spearman(a: list, b: list):
    if len(a) < 3:
        return None
    ra = pd.Series(a).rank().to_numpy()
    rb = pd.Series(b).rank().to_numpy()
    if np.std(ra) == 0 or np.std(rb) == 0:
        return None
    return round(float(np.corrcoef(ra, rb)[0, 1]), 6)


def coverage_gate(study: str) -> dict:
    manifest = om.load_manifest(manifest_path(study))
    limit = SPEC["gates"]["coverage_max_missing_fraction"]
    out = {"datasets": {}, "problems": []}
    for ds in manifest["datasets"]:
        for window in manifest["windows"]:
            _, _, cov = om.window_frame(manifest, ds, window)
            frame, win, _ = om.window_frame(manifest, ds, window)
            fcov = om.funding_coverage(om.load_funding(ds), win["start_ms"], win["end_ms"])
            out["datasets"][f"{ds['key']} {window}"] = {"candles": cov, "funding": fcov}
            missing = cov["missing_bars"] / cov["expected_bars"] if cov["expected_bars"] else 1.0
            if missing > limit:
                out["problems"].append(f"{ds['key']} {window}: {cov['missing_bars']} of {cov['expected_bars']} bars missing")
            if cov["warmup_missing_bars"]:
                out["problems"].append(f"{ds['key']} {window}: {cov['warmup_missing_bars']} warm-up bars missing")
            if not fcov["complete"]:
                out["problems"].append(f"{ds['key']} {window}: funding {fcov['missing_hours']} of {fcov['expected_hours']} hours missing")
    need = max(support_of(p) for arm in SPEC["arms"] for p in grid_points(study, arm))
    out["max_support_bars"] = need
    if need > manifest["warmup_bars"]:
        out["problems"].append(f"warm-up {manifest['warmup_bars']} bars is shorter than the largest support {need}")
    out["complete"] = not out["problems"]
    return out


def support_of(params: dict) -> int:
    from lead_lag_inputs import support_bars
    return support_bars(params["lag_bars"], params["horizon_bars"], params["training_bars"])


def _sealed_payload(frame: pd.DataFrame, leader: pd.DataFrame, follower: str, timeframe: str, i: int, rows: int,
                    interval: int) -> dict:
    def build(df, coin):
        lo = i - rows + 1
        out_rows, timing = [], []
        for k in range(lo, i + 2):
            if k >= len(df):
                break
            open_ms = int(df.index[k].value // 1_000_000)
            closed = k <= i
            ts = open_ms + interval - 1 if closed else open_ms
            r = df.iloc[k]
            out_rows.append([ts, float(r["open"]), float(r["high"]), float(r["low"]), float(r["close"]),
                             float(r["volume"])])
            timing.append([open_ms, open_ms + interval - 1, closed])
        return {"rows": out_rows, "required": len(out_rows), "bars": len(out_rows), "coverage_short": False,
                "first_open_ms": timing[0][0], "last_open_ms": timing[-1][0], "last_close_ms": timing[-1][1],
                "last_recv_at_ms": cutoff, "source": "ws", "ready": True, "forming_bar_included": True,
                "timing": {"rule": "hyperliquid_native_close", "interval_ms": interval, "bars": timing}}
    boundary = int(frame.index[i].value // 1_000_000) + interval
    cutoff = boundary + 1_000
    return {"version": 1, "snapshot_id": f"parity/{boundary}", "generation": 1, "sealed_at_ms": cutoff,
            "decision_cutoff_ms": cutoff, "feed_complete": True,
            "frames": {f"{follower}|{timeframe}": build(frame, follower), f"BTC|{timeframe}": build(leader, "BTC")}}


def parity(study: str, params: dict, window: str) -> dict:
    from close_registry_loader import evaluate as close_evaluate
    from closed_bar import FRAME_DECISION_TIME_ATTR, declare_frame_cadence, timings_timestamp_kind
    from lead_lag_history import manifest_leader_frame, manifest_leader_params
    from lead_lag_inputs import prepare_sealed_inputs
    from registry_loader import load_registry
    from strategy_composition import evaluate_open_close, finalize_decision
    reg = load_registry("futures")
    manifest = om.load_manifest(manifest_path(study))
    tf = manifest["interval"]
    interval = manifest["interval_ms"]
    S = support_of(params)
    run_params = dict(params)
    run_params["expected_interval_ms"] = interval
    cols = ["signal", "bll_reason", "bll_event_id"]
    out = {"study": study, "window": window, "params": {k: params[k] for k in GRID_KEYS}, "followers": {}}
    leader_frame, _, _ = manifest_leader_frame(manifest, window)
    c = contract()
    for follower in SPEC["followers"]:
        ds = om.dataset_by_key(manifest, f"{follower} {tf}")
        frame, win, _ = om.window_frame(manifest, ds, window)
        obs, _ = manifest_leader_params(manifest, window, frame, follower)
        full = reg.apply_strategy(NAME, frame, {**run_params, **obs})
        opens = frame["timestamp"].to_numpy()
        scored = np.flatnonzero((opens >= win["start_ms"]) & (opens < win["end_ms"]))
        entries = [int(k) for k in scored if full["signal"].iloc[k] != 0]
        sample = sorted(set(int(k) for k in scored[::SPEC["parity"]["sample_every_rows"]]) | set(entries))
        ids = [full["bll_event_id"].iloc[k] for k in entries]
        tail_bad, sealed_bad, checked_sealed = [], [], 0
        lf = leader_frame.reindex(frame.index)
        for i in sample:
            if i - S + 1 < 0:
                continue
            tail = reg.apply_strategy(NAME, frame.iloc[i - S + 1:i + 1], {**run_params, **obs})
            if tail[cols].iloc[-1].tolist() != full[cols].iloc[i].tolist():
                tail_bad.append(int(opens[i]))
            if lf.iloc[i - S + 1:i + 2]["close"].isna().any() or i + 1 >= len(frame):
                continue
            payload = _sealed_payload(frame, lf, follower, tf, i, S, interval)
            rows, timings, ctx = prepare_sealed_inputs(payload, follower, tf, S)
            if rows is None:
                sealed_bad.append([int(opens[i]), ctx.get("hold_reason")])
                continue
            df = pd.DataFrame(rows, columns=["timestamp", "open", "high", "low", "close", "volume"])
            df.index = pd.to_datetime(df["timestamp"], unit="ms")
            declare_frame_cadence(df, tf, timings_timestamp_kind(rows, timings))
            df.attrs[FRAME_DECISION_TIME_ATTR] = {"cutoff_ms": ctx["cutoff_ms"], "boundary_ms": ctx["boundary_ms"]}
            evaluation = evaluate_open_close(
                reg.apply_strategy, reg.get_strategy, df, NAME, NAME, [r["name"] for r in c["close_strategies"]], "",
                params={**run_params, "leader_context": ctx}, close_evaluate=close_evaluate,
                close_params_by_name={r["name"]: r["params"] for r in c["close_strategies"]})
            decision = finalize_decision(evaluation, "")
            want = {1: "long", -1: "short", 0: "none"}[int(full["signal"].iloc[i])]
            last = evaluation.open_result_df.iloc[-1]
            checked_sealed += 1
            if decision["open_action"] != want or last["bll_event_id"] != full["bll_event_id"].iloc[i]:
                sealed_bad.append([int(opens[i]), decision["open_action"], want])
        out["followers"][follower] = {
            "sampled_rows": len(sample), "entry_rows": len(entries), "sealed_checked": checked_sealed,
            "tail_mismatches": tail_bad, "sealed_mismatches": sealed_bad,
            "duplicate_event_ids": sorted({x for x in ids if ids.count(x) > 1}),
        }
    out["clean"] = all(not f["tail_mismatches"] and not f["sealed_mismatches"] and not f["duplicate_event_ids"]
                       for f in out["followers"].values())
    return out


def incumbents(study: str, window: str) -> dict:
    from registry_loader import load_registry
    reg = load_registry("futures")
    manifest = om.load_manifest(manifest_path(study))
    datasets = [(f, manifest["interval"]) for f in SPEC["followers"]]
    legs = compute_incumbent_legs(reg, datasets, (None, None), SPEC["contract"]["capital"],
                                  manifest_ctx={"manifest": manifest, "window": window, "cost_multiplier": 1.0})
    return {"bars": incumbent_bars(legs),
            "legs": {ds: {n: (None if leg is None else {k: leg[k] for k in ("sharpe", "return_pct", "max_dd_pct",
                                                                            "ddadj", "trades")})
                          for n, leg in sorted(per.items())} for ds, per in legs.items()}}


def m1_score(summary: dict, bars: dict, study: str) -> dict:
    manifest_tf = om.load_manifest(manifest_path(study))["interval"]
    legs = {}
    for f, leg in summary["legs"].items():
        ret, dd = leg["return_pct"], leg["max_dd_pct"]
        legs[f"{f} {manifest_tf}"] = {"sharpe": leg["sharpe"], "ddadj": (-100.0 if leg["liquidated"] else
                                                                          (round(ret / abs(dd), 3) if dd else 0.0)),
                                       "trades": leg["trades"], "liquidated": leg["liquidated"],
                                       "funding_incomplete": leg["funding_incomplete"]}
    score = score_candidate(legs, bars)
    return {k: v for k, v in score.items() if k != "rows"}


def study_block(study: str, workers: int) -> dict:
    arms = list(SPEC["arms"])
    base, stress = SPEC["costs"]["base_cost_multiplier"], SPEC["costs"]["stress_cost_multiplier"]
    train, test = SPEC["windows"]["selection"], SPEC["windows"]["held_out"]
    block = {"coverage": coverage_gate(study), "arms": {}}
    tasks = []
    for arm in arms:
        tasks += make_tasks(study, arm, train, base, 0, grid_points(study, arm))
    results = run_tasks(tasks, workers)
    selected = {}
    for arm in arms:
        rows = [summarize(results, study, arm, train, base, 0, p) for p in grid_points(study, arm)]
        pick = select(rows)
        selected[arm] = pick
        block["arms"][arm] = {"train_grid": rows, "selected": None if pick is None else pick["params"],
                              "selected_train": pick,
                              "neighbors": None if pick is None else neighbors(study, pick, rows)}
    held = []
    for arm in arms:
        if selected[arm] is None:
            continue
        p = _with_fixed(selected[arm]["params"], arm)
        held += make_tasks(study, arm, test, base, 0, [p], keep_positions=True)
        held += make_tasks(study, arm, test, stress, 0, [p], keep_positions=True)
    if selected["candidate"] is not None:
        p = _with_fixed(selected["candidate"]["params"], "candidate")
        for delay in SPEC["latency_bars"]:
            if delay:
                held += make_tasks(study, "candidate", test, base, delay, [p], keep_positions=True)
    held += make_tasks(study, "candidate", test, base, 0,
                       [q for q in grid_points(study, "candidate")
                        if selected["candidate"] is None or q != _with_fixed(selected["candidate"]["params"], "candidate")])
    hres = run_tasks(held, workers)
    results.update(hres)
    for arm in arms:
        if selected[arm] is None:
            continue
        p = _with_fixed(selected[arm]["params"], arm)
        block["arms"][arm]["held_out_base"] = summarize(results, study, arm, test, base, 0, p)
        block["arms"][arm]["held_out_stress"] = summarize(results, study, arm, test, stress, 0, p)
    if selected["candidate"] is not None:
        p = _with_fixed(selected["candidate"]["params"], "candidate")
        block["latency"] = {str(d): _latency_row(summarize(results, study, "candidate", test, base, d, p))
                            for d in SPEC["latency_bars"]}
        test_rows = [summarize(results, study, "candidate", test, base, 0, q) for q in grid_points(study, "candidate")]
        train_rows = block["arms"]["candidate"]["train_grid"]
        block["held_out_grid"] = [{"params": r["params"], "mean_return_pct": r["mean_return_pct"],
                                   "mean_sharpe": r["mean_sharpe"], "positions": r["positions"],
                                   "worst_max_dd_pct": r["worst_max_dd_pct"]} for r in test_rows]
        block["train_test_spearman_return"] = spearman([r["mean_return_pct"] for r in train_rows],
                                                       [r["mean_return_pct"] for r in test_rows])
        block["held_out_grid_positive_share"] = round(
            sum(1 for r in test_rows if r["mean_return_pct"] > 0) / len(test_rows), 6)
        block["parity"] = parity(study, p, test)
    block["incumbents"] = incumbents(study, test)
    if selected["candidate"] is not None:
        block["m1"] = m1_score(block["arms"]["candidate"]["held_out_base"], block["incumbents"]["bars"], study)
    block["verdict"] = study_verdict(block)
    return block


def _with_fixed(params: dict, arm: str) -> dict:
    out = dict(params)
    out.update(SPEC["fixed_params"])
    out["signal_mode"] = SPEC["arms"][arm]["signal_mode"]
    return out


def _latency_row(summary: dict) -> dict:
    return {k: summary.get(k) for k in ("mean_return_pct", "mean_sharpe", "worst_max_dd_pct", "positions",
                                         "clusters", "long_positions", "short_positions")}


def study_verdict(block: dict) -> dict:
    gates = SPEC["gates"]
    reasons_inconclusive, misses = [], []
    if not block["coverage"]["complete"]:
        reasons_inconclusive += block["coverage"]["problems"]
    cand = block["arms"]["candidate"]
    if cand["selected"] is None:
        reasons_inconclusive.append("the candidate has no eligible train selection")
        return {"verdict": "inconclusive", "inconclusive": reasons_inconclusive, "misses": misses}
    hb, hs = cand["held_out_base"], cand["held_out_stress"]
    for arm, data in block["arms"].items():
        for key in ("held_out_base", "held_out_stress"):
            if key in data and data[key]["funding_incomplete"]:
                reasons_inconclusive.append(f"{arm} {key} has incomplete funding")
    if hb["clusters"] < gates["min_heldout_clusters"]:
        reasons_inconclusive.append(f"{hb['clusters']} held-out position clusters, below {gates['min_heldout_clusters']}")
    if not hb["long_positions"] or not hb["short_positions"]:
        reasons_inconclusive.append("the held-out candidate lacks long or short positions")
    if not block.get("parity", {}).get("clean"):
        reasons_inconclusive.append("decision parity failed")
    if not hb["mean_return_pct"] > 0:
        misses.append(f"base-cost mean net return {hb['mean_return_pct']}% is not above 0")
    if not hs["mean_return_pct"] > 0:
        misses.append(f"stress-cost mean net return {hs['mean_return_pct']}% is not above 0")
    for arm in ("follower_momentum", "btc_trend_filter"):
        other = block["arms"][arm].get("held_out_base")
        if other is None:
            misses.append(f"{arm} has no held-out leg to compare (no eligible train selection)")
        elif not hb["mean_return_pct"] > other["mean_return_pct"]:
            misses.append(f"mean net return {hb['mean_return_pct']}% is not above {arm} {other['mean_return_pct']}%")
    if abs(hb["worst_max_dd_pct"]) > gates["max_drawdown_pct"]:
        misses.append(f"worst follower drawdown {hb['worst_max_dd_pct']}% exceeds {gates['max_drawdown_pct']}%")
    verdict = "inconclusive" if reasons_inconclusive else ("pass" if not misses else "fail")
    return {"verdict": verdict, "inconclusive": reasons_inconclusive, "misses": misses}


def overall_verdict(primary: dict, secondary: dict) -> dict:
    p, s = primary["verdict"]["verdict"], secondary["verdict"]["verdict"]
    notes = []
    if p == "inconclusive":
        return {"verdict": "inconclusive", "notes": ["the primary study is inconclusive"]}
    if p == "fail":
        return {"verdict": "fail", "notes": ["the primary study misses a pass condition"]}
    if s == "inconclusive":
        return {"verdict": "inconclusive", "notes": ["the primary study passes but the secondary study is inconclusive"]}
    sec = secondary["arms"]["candidate"].get("held_out_base")
    if sec is None or not sec["mean_return_pct"] > 0:
        notes.append("the secondary venue-price candidate is not net positive at base cost")
        return {"verdict": "fail", "notes": notes}
    return {"verdict": "pass", "notes": ["every frozen pass condition holds"]}


def run(workers: int) -> dict:
    result = {
        "issue": 1640, "strategy": NAME, "protocol_sha256": protocol_sha256(),
        "sources": {rel: file_sha256(rel) for rel in SOURCE_FILES},
        "manifests": {},
        "studies": {},
    }
    for study in SPEC["studies"]:
        manifest = om.load_manifest(manifest_path(study))
        result["manifests"][study] = {
            "path": SPEC["studies"][study]["manifest"], "provenance": manifest["provenance"],
            "interval": manifest["interval"], "warmup_bars": manifest["warmup_bars"],
            "windows": {k: {"start": v["start"], "end": v["end"]} for k, v in manifest["windows"].items()},
            "costs": manifest["costs"],
            "datasets": {d["key"]: {"candles_sha256": d["candles"]["sha256"],
                                    "funding_sha256": (d["funding"] or {}).get("sha256"),
                                    "size_decimals": d["size_decimals"], "half_spread_bps": d["half_spread_bps"]}
                         for d in manifest["datasets"]},
        }
        result["studies"][study] = study_block(study, workers)
    result["verdict"] = overall_verdict(result["studies"]["primary_1h"], result["studies"]["secondary_4h"])
    result["edge_source"] = {"pass": "unvalidated", "fail": "study_fail",
                             "inconclusive": "study_inconclusive"}[result["verdict"]["verdict"]]
    return result


def _f(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, float):
        return f"{v:.{nd}f}"
    return str(v)


def render(result: dict) -> str:
    lines = [
        "# Bitcoin lead-lag entries: frozen study report (#1640)",
        "",
        f"Verdict: **{result['verdict']['verdict'].upper()}** ({'; '.join(result['verdict']['notes'])}). "
        f"Registry: `bitcoin_lead_lag` stays `no_edge` and `production_refused`; edge_source `{result['edge_source']}`.",
        "",
        "Every value below comes from `results.json`, which `run_study.py run` writes and `run_study.py "
        "--render-only` renders. The protocol is `candidate_spec.json` (sha256 "
        f"`{result['protocol_sha256']}`). Research only: nothing here changes live defaults, deployed configuration "
        "or admission.",
        "",
        "## Reproduce",
        "",
        "```",
        "uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_1h.json",
        "uv run --no-sync python backtest/offline_manifest.py verify --manifest backtest/candidates/bitcoin_lead_lag_1640_manifest_4h.json",
        "uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --protocol-hash",
        "uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py run --workers 4",
        "uv run --no-sync python backtest/candidates/bitcoin_lead_lag_1640/run_study.py --render-only",
        "```",
        "",
        "## Inputs",
        "",
    ]
    for study, m in result["manifests"].items():
        lines.append(f"- `{study}`: `{m['path']}` ({m['provenance']['kind']}: {m['provenance']['label']}); interval "
                     f"{m['interval']}, warm-up {m['warmup_bars']} bars, windows "
                     + ", ".join(f"{k} [{v['start']}, {v['end']})" for k, v in m["windows"].items())
                     + f"; costs {json.dumps(m['costs'], sort_keys=True)}.")
        for key, d in sorted(m["datasets"].items()):
            lines.append(f"  - {key}: candles `{d['candles_sha256']}`, funding `{d['funding_sha256']}`, "
                         f"szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps")
    lines += ["", "Source sha256 at run time:", ""]
    for rel, sha in sorted(result["sources"].items()):
        lines.append(f"- `{rel}`: `{sha}`")
    lines += ["", "Limitations: the 1h study prices a Binance US spot proxy for the Hyperliquid perpetual; the spread "
              "comes from one stored L2 snapshot; slippage is a fixed 5 bps per side; lot flooring and the $10 minimum "
              "come from the manifest execution spec. Funding is the stored Hyperliquid hourly series charged as a cost. "
              "Fills happen at the next bar open; latency rows delay the fill by whole bars. The windows were used by "
              "earlier studies, so this is retrospective evidence.", ""]
    for study, block in result["studies"].items():
        lines += [f"## {study}", "", f"Study verdict: **{block['verdict']['verdict']}**.", ""]
        for r in block["verdict"]["inconclusive"]:
            lines.append(f"- inconclusive: {r}")
        for r in block["verdict"]["misses"]:
            lines.append(f"- miss: {r}")
        cov = block["coverage"]
        lines += ["", f"Coverage gate: {'complete' if cov['complete'] else 'failed'}; largest support "
                  f"{cov['max_support_bars']} bars.", ""]
        lines += ["| dataset window | expected | present | missing | warm-up missing | funding hours missing |",
                  "|---|---|---|---|---|---|"]
        for key, c in sorted(cov["datasets"].items()):
            lines.append(f"| {key} | {c['candles']['expected_bars']} | {c['candles']['present_bars']} | "
                         f"{c['candles']['missing_bars']} | {c['candles']['warmup_missing_bars']} | "
                         f"{c['funding']['missing_hours']} |")
        lines += ["", "### Selections (train)", "", "| arm | selected params | mean Sharpe | mean net % | positions | eligible grid points |",
                  "|---|---|---|---|---|---|"]
        for arm, data in block["arms"].items():
            st = data["selected_train"]
            n_ok = sum(1 for r in data["train_grid"] if eligible(r))
            lines.append(f"| {arm} | {json.dumps(data['selected'], sort_keys=True) if data['selected'] else '-'} | "
                         f"{_f(st['mean_sharpe'] if st else None, 3)} | {_f(st['mean_return_pct'] if st else None)} | "
                         f"{st['positions'] if st else '-'} | {n_ok} of {len(data['train_grid'])} |")
        lines += ["", "### Held-out (test)", "",
                  "| arm | cost | mean net % | mean Sharpe | worst DD % | positions | long/short | clusters | turnover (ETH, SOL) | funding USD (ETH, SOL) |",
                  "|---|---|---|---|---|---|---|---|---|---|"]
        for arm, data in block["arms"].items():
            for key, label in (("held_out_base", "base"), ("held_out_stress", "stress x2")):
                h = data.get(key)
                if h is None:
                    continue
                legs = h["legs"]
                lines.append(
                    f"| {arm} | {label} | {_f(h['mean_return_pct'])} | {_f(h['mean_sharpe'], 3)} | "
                    f"{_f(h['worst_max_dd_pct'])} | {h['positions']} | {h['long_positions']}/{h['short_positions']} | "
                    f"{h.get('clusters', '-')} | "
                    + ", ".join(_f(legs[f]["turnover"]) for f in SPEC["followers"]) + " | "
                    + ", ".join(_f(legs[f]["funding_pnl_usd"]) for f in SPEC["followers"]) + " |")
        cand = block["arms"]["candidate"]
        if cand.get("held_out_base"):
            hb = cand["held_out_base"]
            lines += ["", "Per follower (candidate, base cost):", ""]
            for f, leg in hb["legs"].items():
                lines.append(f"- {f}: net {_f(leg['return_pct'])}%, Sharpe {_f(leg['sharpe'], 3)}, max DD "
                             f"{_f(leg['max_dd_pct'])}%, positions {leg['positions']} ({leg['long_positions']} long, "
                             f"{leg['short_positions']} short), fees ${_f(leg['fees_usd'])}, funding "
                             f"${_f(leg['funding_pnl_usd'])}, funding coverage complete "
                             f"{(leg['funding_coverage'] or {}).get('complete')}, leader rows matched "
                             f"{((leg['leader'] or {}).get('coverage') or {}).get('leader_rows_matched')} of "
                             f"{((leg['leader'] or {}).get('coverage') or {}).get('follower_rows')}")
            lines += ["", f"Concentration: {json.dumps(hb.get('concentration'), sort_keys=True)}", ""]
            lines += ["Latency decay (candidate, base cost):", "", "| extra bars | mean net % | mean Sharpe | positions |",
                      "|---|---|---|---|"]
            for d, row in block.get("latency", {}).items():
                lines.append(f"| {d} | {_f(row['mean_return_pct'])} | {_f(row['mean_sharpe'], 3)} | {row['positions']} |")
            nb = cand["neighbors"]
            lines += ["", f"Parameter stability: {nb['positive_return']} of {nb['neighbors']} one-step train neighbors "
                      f"have a positive mean train return (mean {_f(nb['mean_return_pct'])}%); the train/test Spearman "
                      f"rank correlation of mean net return over the {len(block['held_out_grid'])} candidate grid points is "
                      f"{_f(block['train_test_spearman_return'], 3)}; {_f(100 * block['held_out_grid_positive_share'], 1)}% "
                      "of grid points are net positive on the held-out window.", ""]
            par = block["parity"]
            lines += [f"Decision parity on the held-out window: {'clean' if par['clean'] else 'FAILED'}."]
            for f, row in par["followers"].items():
                lines.append(f"- {f}: {row['sampled_rows']} sampled rows ({row['entry_rows']} entries), "
                             f"{row['sealed_checked']} sealed-payload composer checks, tail mismatches "
                             f"{len(row['tail_mismatches'])}, sealed mismatches {len(row['sealed_mismatches'])}, "
                             f"duplicate event ids {len(row['duplicate_event_ids'])}")
            m1 = block.get("m1") or {}
            lines += ["", f"M1 incumbent comparison (report only): candidate mean Sharpe {_f(m1.get('mean_sharpe'), 3)} "
                      f"vs incumbent median bar {_f(m1.get('mean_bar_sharpe'), 3)}; mean ddadj {_f(m1.get('mean_ddadj'), 3)} "
                      f"vs {_f(m1.get('mean_bar_ddadj'), 3)}; M1 verdict `{m1.get('verdict')}`.", ""]
        lines += ["### Every candidate grid point on the held-out window (base cost)", "",
                  "| params | mean net % | mean Sharpe | positions | worst DD % |", "|---|---|---|---|---|"]
        for r in block.get("held_out_grid", []):
            lines.append(f"| {json.dumps(r['params'], sort_keys=True)} | {_f(r['mean_return_pct'])} | "
                         f"{_f(r['mean_sharpe'], 3)} | {r['positions']} | {_f(r['worst_max_dd_pct'])} |")
        lines.append("")
    lines += ["---", "Created with LLM: Opus 5.5 | high | Harness: Claude Code", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="#1640 frozen Bitcoin lead-lag study (suggest-only)")
    ap.add_argument("command", nargs="?", default="run", choices=("run", "verify"))
    ap.add_argument("--workers", type=int, default=1)
    ap.add_argument("--protocol-hash", action="store_true")
    ap.add_argument("--render-only", action="store_true")
    args = ap.parse_args(argv)
    if args.protocol_hash:
        print(protocol_sha256())
        return 0
    if args.render_only:
        with open(RESULTS_PATH) as fh:
            result = json.load(fh)
        with open(REPORT_PATH, "w") as fh:
            fh.write(render(result))
        return 0
    if args.command == "verify":
        for study in SPEC["studies"]:
            print(json.dumps(coverage_gate(study), sort_keys=True))
        return 0
    result = run(args.workers)
    with open(RESULTS_PATH, "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True)
        fh.write("\n")
    with open(REPORT_PATH, "w") as fh:
        fh.write(render(result))
    print(json.dumps(result["verdict"], sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
