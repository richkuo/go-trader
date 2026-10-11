#!/usr/bin/env python3

import argparse
import gzip
import hashlib
import json
import math
import os
import statistics
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import offline_manifest as om
from backtester import aggregate_close_validations
from eval_windows import INCUMBENTS, evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES

SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "schaff_trend_cycle_1651_manifest.json")
EVIDENCE_DIR = os.path.join(_HERE, "evidence")
PARITY_PATH = os.path.join(_HERE, "parity_results.json")
HOUR_MS = 3_600_000
PARITY_BARS = 600
SOURCE_FILES = (
    "shared_strategies/open/schaff_trend_cycle.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/closed_bar.py",
    "shared_tools/strategy_composition.py",
    "shared_scripts/check_hyperliquid.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/parity_diff.py",
    "backtest/candidates/schaff_trend_cycle_1651/candidate_seed.json",
    "backtest/candidates/schaff_trend_cycle_1651/run_study.py",
)
EVIDENCE_INPUT_COLUMNS = ("timestamp", "open", "high", "low", "close", "volume")
EVIDENCE_VALUE_COLUMNS = (
    "stc_eval_ts_ms", "stc_macd", "stc_prev_macd", "stc_macd_slope", "stc_stoch1_raw", "stc_stoch1",
    "stc_stoch2_raw", "stc_value", "stc_prev_value", "stc_value_valid", "stc_prev_valid", "stc_valid",
    "stc_reason_code", "signal",
)


def load_seed(path: str = SEED_PATH) -> dict:
    with open(path) as fh:
        return json.load(fh)


SEED = load_seed()
CANDIDATE = SEED["name"]
ARMS = tuple(SEED["arms"])
CANDIDATE_ARMS = tuple(SEED["candidate_arms"])
MATCHED_ARM = SEED["matched_arm"]
COMPARATORS = tuple(SEED["comparator_arms"])
SELECTION_WINDOW = SEED["selection_window"]
HELD_OUT_WINDOW = SEED["held_out_window"]
BASE_COST = SEED["cost_runs"]["base"]["cost_multiplier"]
STRESS_COST = SEED["cost_runs"]["stress"]["cost_multiplier"]
MIN_TRAIN_POSITIONS = SEED["selection"]["min_train_independent_positions"]
MIN_TEST_POSITIONS = SEED["held_out"]["min_independent_positions_per_arm"]


def _sha(path: str) -> str:
    return om.sha256_file(path)


def protocol_sha256() -> str:
    blob = json.dumps(SEED, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(blob).hexdigest()


def arm_strategy(arm: str) -> str:
    return SEED["arms"][arm]["strategy"]


def arm_candidate(arm: str, params: dict) -> dict:
    return {
        "name": arm_strategy(arm),
        "params": dict(params),
        "direction": SEED["arms"][arm]["direction"],
        "close_strategies": json.loads(json.dumps(SEED["close_strategies"])),
        "stop_loss_atr_mult": SEED["stop_loss_atr_mult"],
        "comparison_mode": None if SEED["comparison_mode"] == "strict" else SEED["comparison_mode"],
    }


def _grid(name: str, defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[name].items()]
    return expand_sweep(dict(defaults), specs)


def _window_preflight(manifest: dict, ds: dict, window_name: str) -> dict:
    out = {"dataset": ds["key"], "window": window_name, "problems": []}
    win = manifest["windows"][window_name]
    step = manifest["interval_ms"]
    try:
        frame, _, cov = om.window_frame(manifest, ds, window_name)
    except om.ManifestError as exc:
        out["problems"].append(f"candles: {exc}")
        out["complete"] = False
        return out
    out["candle_coverage"] = cov
    ts = frame["timestamp"].to_numpy(dtype=np.int64)
    expected_rows = manifest["warmup_bars"] + cov["expected_bars"]
    if not cov["complete"] or cov["warmup_missing_bars"]:
        out["problems"].append(
            f"candles: {cov['missing_bars']} window bars and {cov['warmup_missing_bars']} warm-up bars missing")
    if len(ts) != expected_rows or (len(ts) > 1 and not bool((np.diff(ts) == step).all())):
        out["problems"].append(f"candles: {len(ts)} rows off the exact {manifest['interval']} grid "
                               f"(expected {expected_rows})")
    in_window = ts[(ts >= win["start_ms"]) & (ts < win["end_ms"])]
    if len(in_window) == 0 or int(in_window[0]) != win["start_ms"] or int(in_window[-1]) != win["end_ms"] - step:
        out["problems"].append("candles: window edges do not match the window start and end")
    o, h, lo, c, v = (frame[k].to_numpy(dtype=float) for k in ("open", "high", "low", "close", "volume"))
    with np.errstate(invalid="ignore"):
        ok = (np.isfinite(o) & np.isfinite(h) & np.isfinite(lo) & np.isfinite(c) & np.isfinite(v)
              & (o > 0) & (h > 0) & (lo > 0) & (c > 0) & (h >= lo) & (o <= h) & (o >= lo)
              & (c <= h) & (c >= lo) & (v >= 0))
    bad = int((~ok).sum())
    out["invalid_rows"] = bad
    if bad:
        out["problems"].append(f"candles: {bad} rows with invalid prices, geometry or volume")
    if manifest["warmup_bars"] < 193:
        out["problems"].append(f"candles: warm-up {manifest['warmup_bars']} bars is below the 193-bar requirement")
    funding = om.load_funding(ds)
    fcov = om.funding_coverage(funding, win["start_ms"], win["end_ms"])
    out["funding_coverage"] = fcov
    if not fcov["complete"]:
        out["problems"].append(f"funding: {fcov['missing_hours']} hourly records missing (left-closed window)")
    first_slot = (win["start_ms"] - step) // HOUR_MS
    last_slot = (win["end_ms"] - step) // HOUR_MS
    have = set() if funding is None else set((funding["timestamp"].to_numpy(dtype=np.int64) // HOUR_MS).tolist())
    missing_slots = [s for s in range(first_slot, last_slot + 1) if s not in have]
    out["accrual_span"] = {
        "first_slot_ms": first_slot * HOUR_MS, "last_slot_ms": last_slot * HOUR_MS,
        "expected_slots": last_slot - first_slot + 1, "missing_slots": len(missing_slots),
    }
    if missing_slots:
        out["problems"].append(f"funding: {len(missing_slots)} hourly records missing in the right-closed accrual span")
    out["complete"] = not out["problems"]
    return out


def preflight(manifest: dict) -> dict:
    rows = [_window_preflight(manifest, ds, w) for ds in manifest["datasets"]
            for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)]
    refused = [{"dataset": r["dataset"], "window": r["window"], "problems": r["problems"]}
               for r in rows if not r["complete"]]
    return {"complete": not refused, "refused": refused, "windows": rows}


def independent_positions(positions: list) -> int:
    spans = sorted((pd.Timestamp(p["entry_date"]), pd.Timestamp(p["exit_date"])) for p in positions)
    count = 0
    latest_exit = None
    for entry, exit_ in spans:
        if latest_exit is None or entry > latest_exit:
            count += 1
            latest_exit = exit_
        else:
            latest_exit = max(latest_exit, exit_)
    return count


def summarize(score: dict, window_bars: dict, expected: int) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    if not rows:
        return {"datasets": 0, "expected_datasets": expected, "positions": 0, "independent_positions": 0,
                "long_positions": 0, "short_positions": 0}
    legs = [r["leg"] for r in rows]
    positions = []
    per_dataset = {}
    for r in rows:
        leg = r["leg"]
        ex = leg["execution"]
        per_dataset[r["dataset"]] = {
            "return_pct": leg["return_pct"],
            "sharpe": leg["sharpe"],
            "max_dd_pct": leg["max_dd_pct"],
            "ddadj": leg["ddadj"],
            "liquidated": bool(leg.get("liquidated")),
            "net_pnl_usd": ex["net_pnl_usd"],
            "fees_usd": ex["fees_usd"],
            "funding_pnl_usd": ex["funding_pnl_usd"],
            "turnover": ex["turnover"],
            "positions": ex["positions"],
            "long_positions": ex["long_positions"],
            "short_positions": ex["short_positions"],
            "time_in_market": round(ex["bars_in_market"] / window_bars[r["dataset"]], 4),
            "rejected_entries": ex.get("rejected_entries", 0),
            "skipped_partial_closes": ex.get("skipped_partial_closes", 0),
            "cost_model": leg["manifest"]["cost_model"],
            "execution_spec": leg["manifest"]["execution_spec"],
            "candle_coverage_complete": leg["manifest"]["candle_coverage"]["complete"],
            "funding_coverage_complete": leg["manifest"]["funding_coverage"]["complete"],
            "funding_incomplete": bool(leg.get("funding_incomplete")),
        }
        for p in ex["position_list"]:
            positions.append(dict(p, dataset=r["dataset"]))
    abs_ds = [abs(v["net_pnl_usd"]) for v in per_dataset.values()]
    abs_pos = sorted((abs(p["net_pnl"]) for p in positions), reverse=True)
    exit_reasons = {}
    for p in positions:
        for reason in p["exit_reasons"]:
            key = (reason or "").split(":")[0] or "unknown"
            exit_reasons[key] = exit_reasons.get(key, 0) + 1
    return {
        "datasets": len(rows),
        "expected_datasets": expected,
        "mean_return_pct": round(statistics.mean(leg["return_pct"] for leg in legs), 4),
        "mean_sharpe": round(statistics.mean(leg["sharpe"] for leg in legs), 4),
        "mean_ddadj": round(statistics.mean(leg["ddadj"] for leg in legs), 4),
        "worst_max_dd_pct": round(max(abs(leg["max_dd_pct"]) for leg in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "independent_positions": independent_positions(positions),
        "exit_reasons": dict(sorted(exit_reasons.items())),
        "liquidated_legs": sum(1 for leg in legs if leg.get("liquidated")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 and not v["funding_incomplete"] for v in per_dataset.values()),
        "per_dataset": per_dataset,
        "close_validation": score.get("close_validation"),
    }


def incumbent_record(score: dict) -> dict:
    return {
        "m1_verdict": score.get("verdict"),
        "mean_bar_sharpe": score.get("mean_bar_sharpe"),
        "mean_bar_ddadj": score.get("mean_bar_ddadj"),
        "bars": score.get("bars"),
    }


def eligible(summary: dict) -> bool:
    return (summary["datasets"] == summary["expected_datasets"]
            and summary.get("coverage_complete", False)
            and summary.get("liquidated_legs", 1) == 0
            and summary["independent_positions"] >= MIN_TRAIN_POSITIONS)


def select(rows: list) -> dict:
    pool = [(i, r) for i, r in enumerate(rows) if eligible(r["summary"])]
    if not pool:
        return {"rule_outcome": "no_eligible_combo", "selected": None, "params": None, "eligible_combos": 0}
    best = max(pool, key=lambda kv: (kv[1]["summary"]["mean_sharpe"], kv[1]["summary"]["mean_ddadj"], -kv[0]))[1]
    return {"rule_outcome": "selected", "selected": best["label"], "params": best["params"],
            "eligible_combos": len(pool)}


def neighbors(params: dict, rows: list, name: str) -> dict:
    ranges = DEFAULT_PARAM_RANGES[name]
    near = []
    for r in rows:
        diffs = [abs(list(grid).index(params[k]) - list(grid).index(r["params"][k])) for k, grid in ranges.items()]
        if sum(diffs) == 1:
            near.append(r["summary"].get("mean_sharpe"))
    near = [v for v in near if v is not None]
    return {"neighbor_count": len(near),
            "neighbor_median_sharpe": round(statistics.median(near), 4) if near else None,
            "neighbor_min_sharpe": round(min(near), 4) if near else None}


def _rank(values: list) -> list:
    order = sorted(range(len(values)), key=lambda i: values[i])
    ranks = [0.0] * len(values)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and values[order[j + 1]] == values[order[i]]:
            j += 1
        for k in range(i, j + 1):
            ranks[order[k]] = (i + j) / 2.0
        i = j + 1
    return ranks


def spearman(a: list, b: list):
    if len(a) < 3:
        return None
    ra, rb = _rank(a), _rank(b)
    ma, mb = statistics.mean(ra), statistics.mean(rb)
    num = sum((x - ma) * (y - mb) for x, y in zip(ra, rb))
    den = (sum((x - ma) ** 2 for x in ra) * sum((y - mb) ** 2 for y in rb)) ** 0.5
    return round(num / den, 4) if den else None


def continuous_series(close: np.ndarray, fast: int, slow: int, cycle: int, smooth: float) -> tuple:
    n = len(close)
    af = 2.0 / (fast + 1)
    asl = 2.0 / (slow + 1)
    macd = np.full(n, np.nan)
    ef = es = float(close[0])
    macd[0] = 0.0
    for k in range(1, n):
        ef = af * float(close[k]) + (1.0 - af) * ef
        es = asl * float(close[k]) + (1.0 - asl) * es
        macd[k] = ef - es
    kept_from = slow - 1

    def stage(series, start):
        out = np.full(n, np.nan)
        raw_prev = None
        smoothed = None
        for k in range(start + cycle - 1, n):
            win = series[k - cycle + 1:k + 1]
            lo, hi = float(np.min(win)), float(np.max(win))
            if hi - lo > 0:
                raw = 100.0 * (float(series[k]) - lo) / (hi - lo)
            elif raw_prev is not None:
                raw = raw_prev
            else:
                continue
            raw_prev = raw
            smoothed = raw if smoothed is None else smoothed + smooth * (raw - smoothed)
            out[k] = smoothed
        return out

    s1 = stage(macd, kept_from)
    first = int(np.flatnonzero(np.isfinite(s1))[0])
    stc = stage(s1, first)
    return stc, macd


def continuous_reference(reg, manifest: dict, params: dict) -> dict:
    p = (params["fast_period"], params["slow_period"], params["cycle_length"], params["smoothing"])
    lower, upper = params["lower_threshold"], params["upper_threshold"]
    out = {"params": params, "datasets": []}
    totals = {"scored_rows": 0, "finite_valid_rows": 0, "finite_zero_range_rows": 0, "decision_mismatches": 0,
              "finite_zero_range_hold": 0, "seed_divergence": 0, "unclassified": 0,
              "finite_entries": 0, "continuous_entries": 0}
    for ds in manifest["datasets"]:
        candles = om.load_candles(ds)
        close = candles["close"].to_numpy(dtype=float)
        stc_c, macd_c = continuous_series(close, *p)
        prev_c = np.r_[np.nan, stc_c[:-1]]
        prev_m = np.r_[np.nan, macd_c[:-1]]
        with np.errstate(invalid="ignore"):
            cont_sig = np.where((prev_c <= lower) & (stc_c > lower) & (macd_c > prev_m), 1,
                                np.where((prev_c >= upper) & (stc_c < upper) & (macd_c < prev_m), -1, 0))
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            finite = reg.apply_strategy(CANDIDATE, frame, params)
            ts_all = candles["timestamp"].to_numpy(dtype=np.int64)
            pos = np.searchsorted(ts_all, frame["timestamp"].to_numpy(dtype=np.int64))
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            fsig = finite["signal"].to_numpy()
            freason = finite["stc_reason"].to_numpy()
            fvalid = finite["stc_valid"].to_numpy() == 1.0
            csig = cont_sig[pos]
            fval = finite["stc_value"].to_numpy()
            cval = stc_c[pos]
            both = scored & fvalid & np.isfinite(cval)
            diff = np.abs(fval[both] - cval[both])
            mism = scored & (fsig != csig)
            zero_hold = mism & (freason == "zero_range")
            seed_div = mism & fvalid
            unclassified = mism & ~zero_hold & ~seed_div
            rec = {
                "dataset": ds["key"], "window": window_name, "scored_rows": int(scored.sum()),
                "finite_valid_rows": int((scored & fvalid).sum()),
                "finite_zero_range_rows": int((scored & (freason == "zero_range")).sum()),
                "finite_entries": int((scored & (fsig != 0)).sum()),
                "continuous_entries": int((scored & (csig != 0)).sum()),
                "decision_mismatches": int(mism.sum()),
                "finite_zero_range_hold": int(zero_hold.sum()),
                "seed_divergence": int(seed_div.sum()),
                "unclassified": int(unclassified.sum()),
                "value_abs_diff_max": float(diff.max()) if len(diff) else None,
                "value_abs_diff_mean": float(diff.mean()) if len(diff) else None,
                "value_abs_diff_p95": float(np.percentile(diff, 95)) if len(diff) else None,
                "rows_value_diff_over_1": int((diff > 1.0).sum()),
            }
            out["datasets"].append(rec)
            for key in totals:
                totals[key] += rec[key]
    totals["mismatch_share_of_scored_rows"] = (round(totals["decision_mismatches"] / totals["scored_rows"], 6)
                                               if totals["scored_rows"] else None)
    totals["zero_range_share_of_scored_rows"] = (round(totals["finite_zero_range_rows"] / totals["scored_rows"], 6)
                                                 if totals["scored_rows"] else None)
    out["totals"] = totals
    out["all_mismatches_classified"] = totals["unclassified"] == 0
    return out


def parity_check(manifest: dict, params: dict) -> dict:
    from parity_diff import ParityConfig, compute_parity_frame, summarize as parity_summarize
    out = []
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        tail = frame[list(EVIDENCE_INPUT_COLUMNS[1:])].iloc[-PARITY_BARS:].copy()
        tail.attrs = dict(frame.attrs)
        for batched in (False, True):
            cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                               platform="hyperliquid", symbol=ds["coin"], timeframe=manifest["interval"],
                               close_refs=json.loads(json.dumps(SEED["close_strategies"])), direction="both",
                               batched=batched)
            pf = compute_parity_frame(tail, cfg=cfg, window=200)
            summary = parity_summarize(pf)
            summary.pop("close_validation", None)
            out.append({
                "dataset": ds["key"], "mode": "batched_paper" if batched else "composer", "window": 200,
                "first_bar": str(tail.index[0]), "last_bar": str(tail.index[-1]),
                "long_opens": int((pf["live_open_action"] == "long").sum()),
                "short_opens": int((pf["live_open_action"] == "short").sum()),
                "closing_bars": int((pf["live_close_fraction"] > 0).sum()),
                **summary,
            })
    return {"params": params, "close_strategies": SEED["close_strategies"], "runs": out,
            "clean": all(r["clean"] and r.get("batch_clean", True) for r in out)}


LIVE_STATUS = ("edge_status=no_edge regardless of outcome; paper needs exactly one explicit --mode=paper, "
               "live or missing mode needs allow_no_edge: true; no acknowledgement, promotion or deployment")


def verdict(gate: dict, arms: dict, held: dict, continuous: dict) -> dict:
    if not gate["complete"]:
        return {"outcome": "inconclusive", "live_status": LIVE_STATUS, "checks": {"coverage_complete": False},
                "reason": "coverage gate refused " + ", ".join(f"{r['dataset']} {r['window']}" for r in gate["refused"])}
    unselected = [n for n in ARMS if arms[n]["selection"]["selected"] is None]
    if unselected:
        return {"outcome": "inconclusive", "live_status": LIVE_STATUS,
                "checks": {"coverage_complete": True, "every_arm_selected": False},
                "reason": "no eligible training selection for " + ", ".join(unselected)}
    checks = {"coverage_complete": all(held[n]["selected"][k].get("coverage_complete", False)
                                       for n in ARMS for k in ("base", "stress")),
              "every_arm_selected": True,
              "continuous_mismatches_classified": bool(continuous and continuous["all_mismatches_classified"])}
    for name in ARMS:
        checks[f"{name}_independent_positions_sufficient"] = (
            held[name]["selected"]["base"]["independent_positions"] >= MIN_TEST_POSITIONS)
    both = [a for a in CANDIDATE_ARMS if SEED["arms"][a]["direction"] == "both"]
    if SEED["held_out"]["bidirectional_arm_requires_both_directions"]:
        for arm in both:
            s = held[arm]["selected"]["base"]
            checks[f"{arm}_long_and_short_occur"] = s["long_positions"] > 0 and s["short_positions"] > 0
    matched = held[MATCHED_ARM]["selected"]["base"]
    comps = {c: held[c]["selected"]["base"] for c in COMPARATORS}
    performance = {}
    for arm in CANDIDATE_ARMS:
        performance[f"{arm}_net_positive_base"] = held[arm]["selected"]["base"]["net_pnl_usd"] > 0
        performance[f"{arm}_net_positive_stress"] = held[arm]["selected"]["stress"]["net_pnl_usd"] > 0
    performance[f"{MATCHED_ARM}_beats_comparators_sharpe"] = all(
        matched["mean_sharpe"] > c["mean_sharpe"] for c in comps.values())
    performance[f"{MATCHED_ARM}_beats_comparators_ddadj"] = all(
        matched["mean_ddadj"] > c["mean_ddadj"] for c in comps.values())
    if not all(checks.values()):
        short = sorted(k for k, ok in checks.items() if not ok)
        return {"outcome": "inconclusive", "live_status": LIVE_STATUS, "checks": checks,
                "performance_checks_descriptive": performance,
                "reason": "held-out coverage, sample or reference gate not met: " + ", ".join(short)}
    checks.update(performance)
    if all(checks.values()):
        return {"outcome": "pass", "live_status": LIVE_STATUS, "checks": checks,
                "reason": "every frozen held-out check passed"}
    failed = sorted(k for k, ok in checks.items() if not ok)
    return {"outcome": "fail", "live_status": LIVE_STATUS, "checks": checks,
            "reason": "failed frozen checks: " + ", ".join(failed)}


def _manifest_record(manifest: dict) -> dict:
    return {
        "path": os.path.relpath(manifest["path"], _REPO),
        "sha256": _sha(manifest["path"]),
        "study": manifest["study"],
        "provenance": manifest["provenance"],
        "interval": manifest["interval"],
        "warmup_bars": manifest["warmup_bars"],
        "windows": {k: {"start": v["start"], "end": v["end"], "role": v["role"]}
                    for k, v in manifest["windows"].items()},
        "costs": manifest["costs"],
        "acquisition": manifest["acquisition"],
        "datasets": [{"key": d["key"], "size_decimals": d["size_decimals"],
                      "half_spread_bps": d["half_spread_bps"],
                      "candles_path": d["candles"]["path"], "candles_sha256": d["candles"]["sha256"],
                      "funding_path": d["funding"]["path"] if d["funding"] else None,
                      "funding_sha256": d["funding"]["sha256"] if d["funding"] else None}
                     for d in manifest["datasets"]],
        "venue_reference": {
            "meta_sha256": manifest["venue_reference"]["meta"]["sha256"],
            "l2_snapshot_sha256": manifest["venue_reference"].get("l2_snapshot", {}).get("sha256"),
            "documents": manifest["venue_reference"]["documents"],
        },
    }


def _fmt_full(value) -> str:
    return repr(float(value)) if isinstance(value, (float, np.floating)) else str(value)


def write_evidence(reg, manifest: dict, params: dict) -> list:
    os.makedirs(EVIDENCE_DIR, exist_ok=True)
    out = []
    for ds in manifest["datasets"]:
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            result = reg.apply_strategy(CANDIDATE, frame, params)
            name = f"{ds['coin']}_{window_name}_selected.csv.gz"
            path = os.path.join(EVIDENCE_DIR, name)
            header = list(EVIDENCE_INPUT_COLUMNS) + list(EVIDENCE_VALUE_COLUMNS) + ["stc_reason", "scored"]
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            lines = [",".join(header)]
            for i in range(len(result)):
                row = result.iloc[i]
                cells = [str(int(row["timestamp"]))]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_INPUT_COLUMNS[1:]]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_VALUE_COLUMNS[:-1]]
                cells += [str(int(row["signal"])), str(row["stc_reason"]), str(int(scored[i]))]
                lines.append(",".join(cells))
            payload = ("\n".join(lines) + "\n").encode("utf-8")
            with open(path, "wb") as raw:
                with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
                    fh.write(payload)
            reasons = pd.Series(result["stc_reason"].to_numpy()[scored]).value_counts().to_dict()
            out.append({"dataset": ds["key"], "window": window_name, "params": params,
                        "path": os.path.relpath(path, _REPO), "sha256": _sha(path), "rows": len(result),
                        "scored_rows": int(scored.sum()),
                        "entry_signals_scored": int((result["signal"].to_numpy() != 0)[scored].sum()),
                        "scored_reason_counts": {str(k): int(v) for k, v in sorted(reasons.items())}})
    return out


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    manifest = om.load_manifest(manifest_path)
    gate = preflight(manifest)
    result = {
        "study": manifest["study"],
        "issue": SEED["issue"],
        "seed_sha256": _sha(SEED_PATH),
        "protocol_sha256": protocol_sha256(),
        "seed": SEED,
        "manifest": _manifest_record(manifest),
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "incumbents": list(INCUMBENTS),
        "coverage_gate": gate,
    }
    if not gate["complete"]:
        result["verdict"] = verdict(gate, {}, {}, {})
        return result

    reg = load_registry("futures")
    datasets = manifest_datasets(manifest)
    window_bars = {}
    for ds in manifest["datasets"]:
        for w in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            _, _, cov = om.window_frame(manifest, ds, w)
            window_bars.setdefault(w, {})[ds["key"]] = cov["present_bars"]
    memo: dict = {}
    incumbents = {}

    def score(arm, params, window, mult=BASE_COST):
        s = evaluate_window(reg, arm_candidate(arm, params), datasets, window, SEED["capital"], memo,
                            manifest=manifest, cost_multiplier=mult)
        incumbents.setdefault(f"{window} x{mult:g}", incumbent_record(s))
        return summarize(s, window_bars[window], len(datasets))

    arms = {}
    for arm in ARMS:
        name = arm_strategy(arm)
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        train_rows = []
        for label, params in _grid(name, defaults):
            summary = score(arm, params, SELECTION_WINDOW)
            summary.pop("per_dataset", None)
            train_rows.append({"label": label, "params": params, "summary": summary})
        choice = select(train_rows)
        arms[arm] = {
            "strategy": name,
            "defaults": defaults,
            "direction": SEED["arms"][arm]["direction"],
            "grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[name].items()},
            "train_sweep": train_rows,
            "selection": choice,
            "selected_plateau": neighbors(choice["params"], train_rows, name) if choice["selected"] else None,
        }

    held = {}
    for arm in ARMS:
        params = arms[arm]["selection"]["params"]
        defaults = arms[arm]["defaults"]
        entry = {"selected": None, "defaults": {
            "params": defaults,
            "base": score(arm, defaults, HELD_OUT_WINDOW),
            "stress": score(arm, defaults, HELD_OUT_WINDOW, STRESS_COST),
        }}
        if params is not None:
            entry["selected"] = {
                "params": params,
                "base": score(arm, params, HELD_OUT_WINDOW),
                "stress": score(arm, params, HELD_OUT_WINDOW, STRESS_COST),
            }
        held[arm] = entry

    variants = []
    for arm in CANDIDATE_ARMS:
        for row in arms[arm]["train_sweep"]:
            test = score(arm, row["params"], HELD_OUT_WINDOW)
            variants.append({"arm": arm, "label": row["label"], "params": row["params"],
                             "train_eligible": eligible(row["summary"]),
                             "train_mean_sharpe": row["summary"].get("mean_sharpe"),
                             "train_independent_positions": row["summary"]["independent_positions"],
                             "test_mean_sharpe": test.get("mean_sharpe"),
                             "test_mean_ddadj": test.get("mean_ddadj"),
                             "test_net_pnl_usd": test.get("net_pnl_usd"),
                             "test_positions": test["positions"],
                             "test_independent_positions": test["independent_positions"]})
    stability = {}
    for arm in CANDIDATE_ARMS:
        rows = [v for v in variants if v["arm"] == arm]
        scored = [v for v in rows if v["train_mean_sharpe"] is not None and v["test_mean_sharpe"] is not None]
        stability[arm] = {
            "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in scored],
                                                      [v["test_mean_sharpe"] for v in scored]),
            "test_net_positive_variants": sum(1 for v in rows if (v["test_net_pnl_usd"] or 0) > 0),
            "train_eligible_variants": sum(1 for v in rows if v["train_eligible"]),
            "variants": len(rows),
        }

    validations = []
    for entry in held.values():
        for block in (entry["selected"], entry["defaults"]):
            for key in ("base", "stress"):
                if block is not None and block.get(key) is not None:
                    validations.append(block[key].get("close_validation"))
    evidence_arm = next((a for a in CANDIDATE_ARMS if SEED["arms"][a]["direction"] == "both"), MATCHED_ARM)
    evidence_params = arms[evidence_arm]["selection"]["params"] or arms[evidence_arm]["defaults"]
    continuous = continuous_reference(reg, manifest, evidence_params)
    parity = parity_check(manifest, evidence_params)
    with open(PARITY_PATH, "w") as fh:
        json.dump(parity, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    result.update({
        "arms": arms,
        "held_out": held,
        "incumbent_reference": incumbents,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "evidence_arm": evidence_arm,
        "evidence": write_evidence(reg, manifest, evidence_params),
        "continuous_reference": continuous,
        "parity": {"path": os.path.relpath(PARITY_PATH, _REPO), "sha256": _sha(PARITY_PATH),
                   "clean": parity["clean"]},
        "verdict": verdict(gate, arms, held, continuous),
        "close_validation": aggregate_close_validations(validations),
    })
    return result


def _fmt(v, nd=2):
    if v is None:
        return "-"
    if isinstance(v, bool):
        return "yes" if v else "no"
    if isinstance(v, float):
        if not math.isfinite(v):
            return str(v)
        return f"{v:.{nd}f}"
    return str(v)


def _summary_row(name: str, label: str, s: dict) -> str:
    if not s or not s.get("datasets"):
        return f"| `{name}` | {label} | no scored dataset |" + " |" * 11
    return (f"| `{name}` | {label} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['mean_return_pct'])} | "
            f"{_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | "
            f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['independent_positions']} | "
            f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} |")


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    seed = result["seed"]
    lines = [
        "# Schaff Trend Cycle entries: held-out result (#1651)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Availability: {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification "
        "is in `README.md` and `candidate_seed.json` "
        f"(file sha256 `{result['seed_sha256']}`, protocol sha256 `{result['protocol_sha256']}`).",
        "",
        "Funding dependency #1731 is complete (merged pull request #1745). This study reuses its manifest "
        "funding attachment and adds no funding option or funding accounting. No Binance US proxy leg and no "
        "cache-based `--funding` run is part of this study.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{m['path']}` sha256 `{m['sha256']}`",
        f"- Provenance: {m['provenance']['kind']}: {m['provenance']['label']}",
        f"- Data reuse: {m['acquisition'].get('reused_from', '-')}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored; the evaluator needs 193)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(
        f"- Explicit-close arms (candidates and comparators) use the manifest execution specification: taker "
        f"{c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage {c['slippage_bps']} bps "
        f"per side plus the per-coin half spread, szDecimals lot flooring, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding booked hourly.")
    lines.append(
        "- M1 incumbents run without explicit closes, so they use the flat model (taker fee as commission, half "
        "spread plus slippage as one slippage figure) with the same attached funding. Their execution geometry "
        "is not identical to the explicit-close arms.")
    lines.append(
        "- Unmodeled: queue position, partial fills, order-book depth beyond the frozen half spread, funding "
        "rate changes inside an hour, liquidation-engine fees and venue outages.")
    for d in m["datasets"]:
        lines.append(f"- `{d['key']}`: szDecimals {d['size_decimals']}, half spread {d['half_spread_bps']} bps, "
                     f"candles `{d['candles_path']}` `{d['candles_sha256'][:16]}`, funding `{d['funding_path']}` "
                     f"`{(d['funding_sha256'] or '')[:16]}`")
    gate = result["coverage_gate"]
    lines += ["", "## Coverage gate", "",
              f"Complete: {_fmt(gate['complete'])}. Windows checked: {len(gate['windows'])}.", "",
              "| Dataset | Window | Candle bars present/expected | Warm-up missing | Funding hours present/expected | "
              "Accrual slots missing | Complete |",
              "|---|---|---|---|---|---|---|"]
    for r in gate["windows"]:
        cc = r.get("candle_coverage") or {}
        fc = r.get("funding_coverage") or {}
        ac = r.get("accrual_span") or {}
        lines.append(f"| {r['dataset']} | {r['window']} | {cc.get('present_bars', '-')}/{cc.get('expected_bars', '-')} | "
                     f"{cc.get('warmup_missing_bars', '-')} | {fc.get('present_hours', '-')}/{fc.get('expected_hours', '-')} | "
                     f"{ac.get('missing_slots', '-')} | {_fmt(r['complete'])} |")
    for r in gate["refused"]:
        lines.append(f"- REFUSED {r['dataset']} {r['window']}: {'; '.join(r['problems'])}")
    lines += [
        "",
        "## Protocol",
        "",
        f"- Close owner `{seed['close_strategies'][0]['name']}` with its default ladder "
        f"`{json.dumps(seed['close_default_ladder'], sort_keys=True)}`, comparison mode `{seed['comparison_mode']}`, stop owner "
        f"fixed entry ATR x{seed['stop_loss_atr_mult']}, capital ${seed['capital']:.0f}; signals on bar N fill at "
        "the bar N+1 open.",
        "- Arms: " + ", ".join(f"`{n}` (`{a['strategy']}`, {a['direction']})" for n, a in seed["arms"].items()) + ".",
        f"- Selection on `{seed['selection_window']}` only: {seed['selection']['eligible']}; order "
        f"{'; '.join(seed['selection']['order'])}.",
        f"- Held-out `{seed['held_out_window']}`: at least {MIN_TEST_POSITIONS} independent positions for every "
        "selected arm; the bidirectional candidate arm needs both long and short positions.",
        f"- Cost runs: base x{BASE_COST:g} ({seed['cost_runs']['base']['meaning']}); stress x{STRESS_COST:g} "
        f"({seed['cost_runs']['stress']['meaning']}).",
        f"- Pass rule: {seed['verdict']['pass']}.",
        "- The comparators keep their registered long-only entries. A comparator long-exit signal is never "
        "treated as a short entry, so the long-only comparison uses the matched long-only candidate arm and the "
        "bidirectional arm is reported separately.",
    ]
    if "arms" not in result:
        lines += ["", "No selection or score was computed because the coverage gate refused a window.", ""]
        return "\n".join(lines)
    lines += ["", "## Selections (train)", "",
              "| Arm | Direction | Eligible combos | Selected params | Train mean Sharpe | Train independent positions | "
              "Neighbour median Sharpe |",
              "|---|---|---|---|---|---|---|"]
    for arm in ARMS:
        a = result["arms"][arm]
        sel = a["selection"]
        row = next((r for r in a["train_sweep"] if r["label"] == sel.get("selected")), None)
        plateau = a["selected_plateau"] or {}
        lines.append(f"| `{arm}` | {a['direction']} | {sel['eligible_combos']}/{len(a['train_sweep'])} | "
                     f"{('`' + json.dumps(sel['params'], sort_keys=True) + '`') if sel['params'] else 'none'} | "
                     f"{_fmt(row['summary']['mean_sharpe'], 3) if row else '-'} | "
                     f"{row['summary']['independent_positions'] if row else '-'} | "
                     f"{_fmt(plateau.get('neighbor_median_sharpe'), 3)} |")
    header = ("| Arm | Run | Net $ | Mean ret % | Mean Sharpe | Mean DDadj | Worst DD % | Fees $ | Funding $ | "
              "Turnover | Positions (L/S) | Independent | Top dataset share | Top-5 share |")
    sep = "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"
    lines += ["", "## Held-out comparison", "",
              "Selected rows are verdict inputs. Default rows are descriptive and cannot change the verdict.", "",
              header, sep]
    for arm in ARMS:
        t = result["held_out"][arm]
        if t["selected"] is None:
            lines.append(f"| `{arm}` | no selection |" + " |" * 12)
        else:
            lines.append(_summary_row(arm, f"selected x{BASE_COST:g}", t["selected"]["base"]))
            lines.append(_summary_row(arm, f"selected x{STRESS_COST:g}", t["selected"]["stress"]))
        lines.append(_summary_row(arm, f"defaults x{BASE_COST:g}", t["defaults"]["base"]))
        lines.append(_summary_row(arm, f"defaults x{STRESS_COST:g}", t["defaults"]["stress"]))
    for arm in CANDIDATE_ARMS:
        sel_c = result["held_out"][arm]["selected"]
        if sel_c is None:
            continue
        lines += ["", f"## `{arm}` per dataset (held-out, selected, base cost)", "",
                  "| Dataset | Net $ | Ret % | Sharpe | Max DD % | Fees $ | Funding $ | Positions (L/S) | "
                  "Time in market | Rejected entries | Skipped partial closes | Cost model |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for ds, s in sorted(sel_c["base"]["per_dataset"].items()):
            lines.append(f"| {ds} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | "
                         f"{_fmt(s['max_dd_pct'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                         f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} | "
                         f"{s['cost_model']} |")
    lines += ["", "## M1 incumbent reference (separate from the verdict)", "",
              "Median of the M1 incumbents (" + ", ".join(f"`{n}`" for n in result["incumbents"]) +
              ") per dataset, registry defaults, no explicit close, flat cost model with attached funding.", "",
              "| Window and cost | Dataset | Median Sharpe | Median DDadj | Legs | Funding incomplete |",
              "|---|---|---|---|---|---|"]
    for key, rec in sorted(result["incumbent_reference"].items()):
        for ds, bar in sorted((rec.get("bars") or {}).items()):
            if bar is None:
                lines.append(f"| {key} | {ds} | - | - | 0 | - |")
                continue
            lines.append(f"| {key} | {ds} | {_fmt(bar['sharpe'], 3)} | {_fmt(bar['ddadj'], 3)} | {bar['n']} | "
                         f"{_fmt(bar['funding_incomplete'])} |")
    lines += ["", "## Verdict checks", ""]
    for k, ok in sorted(v["checks"].items()):
        lines.append(f"- {k}: {_fmt(ok)}")
    if v.get("performance_checks_descriptive"):
        lines += ["", "A coverage, sample or reference gate stopped the verdict before the performance gates. The "
                  "same performance checks, descriptive only and not a verdict input:", ""]
        for k, ok in sorted(v["performance_checks_descriptive"].items()):
            lines.append(f"- {k}: {_fmt(ok)}")
    cr = result["continuous_reference"]
    tot = cr["totals"]
    lines += [
        "",
        "## Finite replay versus continuous history",
        "",
        f"Parameters `{json.dumps(cr['params'], sort_keys=True)}` (the selected `{result['evidence_arm']}` "
        "parameters). The continuous reference runs the same equations once over the whole manifest frame and "
        "carries the previous normalized value through a zero range. The finite evaluator restarts both EMAs "
        "and both recursive smoothings 192 bars back for every timestamp and holds on any zero range.",
        "",
        f"- Scored rows {tot['scored_rows']}; finite valid rows {tot['finite_valid_rows']}; finite zero_range "
        f"holds {tot['finite_zero_range_rows']} (share {_fmt(tot['zero_range_share_of_scored_rows'], 4)}).",
        f"- Entries: finite {tot['finite_entries']}, continuous {tot['continuous_entries']}.",
        f"- Decision mismatches {tot['decision_mismatches']} (share {_fmt(tot['mismatch_share_of_scored_rows'], 4)}): "
        f"finite_zero_range_hold {tot['finite_zero_range_hold']}, seed_divergence {tot['seed_divergence']}, "
        f"unclassified {tot['unclassified']}.",
        "- Cause: a zero range happens when a cycle window of MACD or of the first smoothed stage is exactly "
        "constant, which in a finite replay mostly occurs where the recursion is seeded at 0 or 100 inside a "
        "monotonic stretch. The continuous series carries through it; the finite series refuses to invent a "
        "value. Seed divergence is the remaining EMA and smoothing start-up effect of the 192-bar replay.",
        "",
        "| Dataset | Window | Scored | Finite valid | Zero range | Finite entries | Continuous entries | "
        "Mismatches | Zero-range holds | Seed divergence | Unclassified | Max abs value diff | Rows diff > 1 |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for r in cr["datasets"]:
        lines.append(f"| {r['dataset']} | {r['window']} | {r['scored_rows']} | {r['finite_valid_rows']} | "
                     f"{r['finite_zero_range_rows']} | {r['finite_entries']} | {r['continuous_entries']} | "
                     f"{r['decision_mismatches']} | {r['finite_zero_range_hold']} | {r['seed_divergence']} | "
                     f"{r['unclassified']} | {_fmt(r['value_abs_diff_max'], 6)} | {r['rows_value_diff_over_1']} |")
    st = result["parameter_stability"]
    lines += ["", "## Parameter stability and unsuccessful variants", ""]
    for arm, s in st.items():
        lines.append(f"- `{arm}`: training-eligible variants {s['train_eligible_variants']}/{s['variants']}; "
                     f"Spearman train vs held-out mean Sharpe {_fmt(s['spearman_train_vs_test_sharpe'], 3)}; "
                     f"held-out net-positive variants {s['test_net_positive_variants']}/{s['variants']}")
    lines += [
        "",
        "| Arm | Variant | Train eligible | Train Sharpe | Train independent | Held-out Sharpe | Held-out DDadj | "
        "Held-out net $ | Held-out positions | Held-out independent |",
        "|---|---|---|---|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| `{row['arm']}` | {row['label']} | {_fmt(row['train_eligible'])} | "
                     f"{_fmt(row['train_mean_sharpe'], 3)} | {row['train_independent_positions']} | "
                     f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_ddadj'], 3)} | "
                     f"{_fmt(row['test_net_pnl_usd'])} | {row['test_positions']} | {row['test_independent_positions']} |")
    cv = result.get("close_validation") or {}
    lines += ["", "## Close validation", "",
              f"- Modes `{', '.join(cv.get('modes') or [str(cv.get('mode'))])}`, eligibility "
              f"`{cv.get('close_eligibility')}`, parity status `{cv.get('parity_status')}`, incomplete parity "
              f"{_fmt(cv.get('incomplete_parity'))}.",
              "- `unverified` is the strict, eligible capability status (no approximation and no refusal). It is "
              "not a ledger-verified claim that these exits equal live exits."]
    par = result["parity"]
    lines += ["", "## Decision parity", "",
              f"`{par['path']}` sha256 `{par['sha256'][:16]}`, clean {_fmt(par['clean'])}. The last "
              f"{PARITY_BARS} held-out bars of every dataset, a 200-row live window, the selected `{result['evidence_arm']}` "
              "parameters and the frozen close owner: the composer mode compares the full-frame decisions with the "
              "per-bar live composer; the batched paper mode also compares a solo and a batched explicit-paper "
              "`check_hyperliquid.py` slot. Neither mode proves admission or timed closure; those are proved by the "
              "subprocess scenarios in `shared_scripts/test_schaff_trend_cycle_checks.py`."]
    lines += ["", "## Full-precision evidence", "",
              "Every input row and every evaluator value of the evidence parameters, written with `repr` floats.", "",
              "| Dataset | Window | File | sha256 | Rows | Scored rows | Scored entry signals | Scored reasons |",
              "|---|---|---|---|---|---|---|---|"]
    for e in result["evidence"]:
        reasons = ", ".join(f"{k} {v}" for k, v in e["scored_reason_counts"].items())
        lines.append(f"| {e['dataset']} | {e['window']} | `{e['path']}` | `{e['sha256'][:16]}` | {e['rows']} | "
                     f"{e['scored_rows']} | {e['entry_signals_scored']} | {reasons} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines += ["", "---", "Created with LLM: Opus 5.5 | high | Harness: Claude Code", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1651 frozen held-out study driver (suggest-only)")
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    p.add_argument("--render-only", action="store_true",
                   help="Re-render the report from the results JSON without rerunning the study")
    p.add_argument("--protocol-hash", action="store_true", help="Print the frozen protocol sha256 and exit")
    args = p.parse_args(argv)
    if args.protocol_hash:
        print(protocol_sha256())
        return 0
    if args.render_only:
        with open(args.json_out) as fh:
            result = json.load(fh)
    else:
        try:
            result = run(args.manifest)
        except om.ManifestError as exc:
            print(f"manifest error: {exc}", file=sys.stderr)
            return 1
        with open(args.json_out, "w") as fh:
            json.dump(result, fh, indent=1, sort_keys=True, default=str)
            fh.write("\n")
    with open(args.report, "w") as fh:
        fh.write(render(result))
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']})")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
