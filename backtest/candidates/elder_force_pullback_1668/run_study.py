#!/usr/bin/env python3

import argparse
import gzip
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
LOCK_PATH = os.path.join(_HERE, "protocol_lock.json")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "elder_force_pullback_1668_manifest.json")
EVIDENCE_DIR = os.path.join(_HERE, "evidence")
PARITY_PATHS = (os.path.join(_HERE, "parity_results.json"), os.path.join(_HERE, "parity_results_batched.json"))
HOUR_MS = 3_600_000
SOURCE_FILES = (
    "shared_strategies/open/elder_force_pullback.py",
    "shared_strategies/open/indicators_core.py",
    "shared_strategies/open/elder_ray_pullback.py",
    "shared_strategies/open/momentum_pro.py",
    "shared_strategies/open/registry.py",
    "shared_strategies/close/tiered_tp_pct.py",
    "shared_tools/strategy_composition.py",
    "shared_tools/closed_bar.py",
    "backtest/backtester.py",
    "backtest/eval_windows.py",
    "backtest/offline_manifest.py",
    "backtest/optimizer.py",
    "backtest/candidates/elder_force_pullback_1668/candidate_seed.json",
    "backtest/candidates/elder_force_pullback_1668/run_study.py",
)
EVIDENCE_INPUT_COLUMNS = ("timestamp", "open", "high", "low", "close", "volume")
EVIDENCE_VALUE_COLUMNS = (
    "efp_raw_force", "efp_seed_force", "efp_force", "efp_prev_force", "efp_trend_sma", "efp_trend_sma_lag",
    "efp_prev_trend_sma", "efp_prev_trend_sma_lag", "efp_uptrend", "efp_prev_uptrend", "efp_downtrend",
    "efp_prev_downtrend", "efp_cross", "efp_volume_eligible", "efp_valid", "efp_reason_code", "signal",
)


def load_seed(path: str = SEED_PATH) -> dict:
    with open(path) as fh:
        return json.load(fh)


SEED = load_seed()
CANDIDATE = SEED["name"]
ARMS = tuple(SEED["arms"])
ABLATION = next(a for a, spec in SEED["arms"].items() if spec["selection"] == "paired")
COMPARATORS = tuple(a for a in ARMS if a != CANDIDATE)
GRID_ARMS = tuple(a for a in ARMS if SEED["arms"][a]["selection"] == "grid")
SELECTION_WINDOW = SEED["selection_window"]
HELD_OUT_WINDOW = SEED["held_out_window"]
BASE_COST = SEED["cost_runs"]["base"]["cost_multiplier"]
STRESS_COST = SEED["cost_runs"]["stress"]["cost_multiplier"]
MIN_TRAIN_POSITIONS = SEED["selection"]["min_train_independent_positions"]
MIN_TEST_POSITIONS = SEED["held_out"]["min_independent_positions_per_arm"]
INCUMBENT_CONTRACT = SEED["incumbent_contract"]
UNCERTAINTY = SEED["uncertainty"]
SENSITIVITY = SEED["numerical"]["sensitivity"]


def _sha(path: str) -> str:
    return om.sha256_file(path)


def strategy_of(arm: str) -> str:
    return SEED["arms"][arm]["strategy"]


def paired_params(arm: str, params):
    if params is None:
        return None
    return dict(params, **SEED["arms"][arm].get("override", {}))


def arm_candidate(arm: str, params: dict) -> dict:
    return {
        "name": strategy_of(arm),
        "params": dict(params),
        "direction": SEED["arms"][arm]["direction"],
        "close_strategies": json.loads(json.dumps(SEED["close_strategies"])),
        "stop_loss_atr_mult": SEED["stop_loss_atr_mult"],
        "comparison_mode": None if SEED["comparison_mode"] == "strict" else SEED["comparison_mode"],
    }


def _grid(name: str, defaults: dict) -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[name].items()]
    return expand_sweep(dict(defaults), specs)


def check_grid(reg) -> dict:
    ranges = DEFAULT_PARAM_RANGES[CANDIDATE]
    frozen = SEED["optimizer_grid"]
    combos = _grid(CANDIDATE, SEED["params"])
    invalid = []
    for label, params in combos:
        for variant in (params, dict(params, volume_weighting=0)):
            try:
                reg._registry.validate_params(CANDIDATE, variant)
            except ValueError as exc:
                invalid.append({"params": variant, "error": str(exc)})
    return {"matches_frozen_grid": {k: list(v) for k, v in ranges.items()} == frozen,
            "combinations": len(combos), "invalid": invalid,
            "all_valid": not invalid and {k: list(v) for k, v in ranges.items()} == frozen}


def lock() -> dict:
    record = {
        "candidate_seed_sha256": _sha(SEED_PATH),
        "manifest_sha256": _sha(MANIFEST),
        "statement": "recorded before any window was scored; run refuses when either digest differs",
    }
    with open(LOCK_PATH, "w") as fh:
        json.dump(record, fh, indent=1, sort_keys=True)
        fh.write("\n")
    return record


def verify_lock() -> dict:
    if not os.path.isfile(LOCK_PATH):
        raise SystemExit("protocol_lock.json is missing; run `run_study.py lock` before scoring")
    with open(LOCK_PATH) as fh:
        record = json.load(fh)
    if record["candidate_seed_sha256"] != _sha(SEED_PATH) or record["manifest_sha256"] != _sha(MANIFEST):
        raise SystemExit("candidate_seed.json or the manifest changed after the protocol lock; refusing to score")
    return record


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
    out["zero_volume_rows"] = int((v == 0).sum())
    if bad:
        out["problems"].append(f"candles: {bad} rows with invalid prices, geometry or volume")
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


def bootstrap_interval(values: list) -> dict:
    if len(values) < 2:
        return {"positions": len(values), "mean": None, "low": None, "high": None}
    rng = np.random.default_rng(UNCERTAINTY["rng_seed"])
    arr = np.asarray(values, dtype=float)
    draws = rng.integers(0, len(arr), size=(UNCERTAINTY["resamples"], len(arr)))
    means = arr[draws].mean(axis=1)
    lo, hi = UNCERTAINTY["interval"]
    return {"positions": len(values), "mean": round(float(arr.mean()), 4),
            "low": round(float(np.quantile(means, lo)), 4), "high": round(float(np.quantile(means, hi)), 4)}


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
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in legs), 4),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in legs), 4),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in legs), 4),
        "worst_max_dd_pct": round(max(abs(l["max_dd_pct"]) for l in legs), 4),
        "net_pnl_usd": round(sum(v["net_pnl_usd"] for v in per_dataset.values()), 4),
        "fees_usd": round(sum(v["fees_usd"] for v in per_dataset.values()), 4),
        "funding_pnl_usd": round(sum(v["funding_pnl_usd"] for v in per_dataset.values()), 4),
        "mean_turnover": round(statistics.mean(v["turnover"] for v in per_dataset.values()), 4),
        "positions": len(positions),
        "long_positions": sum(1 for p in positions if p["side"] == "long"),
        "short_positions": sum(1 for p in positions if p["side"] == "short"),
        "independent_positions": independent_positions(positions),
        "long_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "long"), 4),
        "short_net_pnl_usd": round(sum(p["net_pnl"] for p in positions if p["side"] == "short"), 4),
        "mean_position_net_pnl_bootstrap": bootstrap_interval([p["net_pnl"] for p in positions]),
        "exit_reasons": dict(sorted(exit_reasons.items())),
        "liquidated_legs": sum(1 for l in legs if l.get("liquidated")),
        "top_dataset_abs_pnl_share": (round(max(abs_ds) / sum(abs_ds), 4) if sum(abs_ds) > 0 else None),
        "top5_position_abs_pnl_share": (round(sum(abs_pos[:5]) / sum(abs_pos), 4) if sum(abs_pos) > 0 else None),
        "coverage_complete": all(v["candle_coverage_complete"] and v["funding_coverage_complete"]
                                 and not v["funding_incomplete"] for v in per_dataset.values()),
        "funding_incomplete_legs": sum(1 for v in per_dataset.values() if v["funding_incomplete"]),
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


LIVE_STATUS = ("edge_status=no_edge regardless of outcome; paper needs exactly one explicit --mode=paper, "
               "live or missing mode needs allow_no_edge: true; no acknowledgement, promotion or deployment")


def _outcome(outcome: str, **kw) -> dict:
    return dict(kw, outcome=outcome, live_status=LIVE_STATUS, edge_source=SEED["verdict"]["edge_source"][outcome])


def verdict(gate: dict, arms: dict, held: dict) -> dict:
    if not gate["complete"]:
        return _outcome("inconclusive", checks={"coverage_complete": False},
                        reason="coverage gate refused " + ", ".join(f"{r['dataset']} {r['window']}"
                                                                    for r in gate["refused"]))
    unselected = [n for n in ARMS if held[n]["selected"] is None]
    if unselected:
        return _outcome("inconclusive", checks={"coverage_complete": True, "every_arm_selected": False},
                        reason="no eligible training selection for " + ", ".join(unselected))
    cand = held[CANDIDATE]["selected"]["base"]
    stress = held[CANDIDATE]["selected"]["stress"]
    comps = {c: held[c]["selected"]["base"] for c in COMPARATORS}
    checks = {"coverage_complete": all(held[n]["selected"][k].get("coverage_complete", False)
                                       for n in ARMS for k in ("base", "stress")),
              "every_arm_selected": True}
    for name in ARMS:
        checks[f"{name}_independent_positions_sufficient"] = (
            held[name]["selected"]["base"]["independent_positions"] >= MIN_TEST_POSITIONS)
    if SEED["held_out"]["candidate_requires_both_directions"]:
        checks["candidate_long_and_short_occur"] = cand["long_positions"] > 0 and cand["short_positions"] > 0
    performance = {
        "net_positive_base": cand["net_pnl_usd"] > 0,
        "net_positive_stress": stress["net_pnl_usd"] > 0,
    }
    for c, s in comps.items():
        performance[f"beats_{c}_sharpe"] = cand["mean_sharpe"] > s["mean_sharpe"]
        performance[f"beats_{c}_ddadj"] = cand["mean_ddadj"] > s["mean_ddadj"]
    if not all(checks.values()):
        short = sorted(k for k, ok in checks.items() if not ok)
        return _outcome("inconclusive", checks=checks, performance_checks_descriptive=performance,
                        reason="held-out coverage or sample gate not met: " + ", ".join(short))
    checks.update(performance)
    if all(checks.values()):
        return _outcome("pass", checks=checks, reason="every frozen held-out check passed")
    failed = sorted(k for k, ok in checks.items() if not ok)
    return _outcome("fail", checks=checks, reason="failed frozen checks: " + ", ".join(failed))


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


def _declared(frame: pd.DataFrame, interval: str) -> pd.DataFrame:
    from closed_bar import TIMESTAMP_KIND_OPEN, declare_frame_cadence
    out = frame.copy()
    declare_frame_cadence(out, interval, TIMESTAMP_KIND_OPEN)
    return out


def sensitivity_cell(reg, frame: pd.DataFrame, params: dict) -> dict:
    out = reg.apply_strategy(CANDIDATE, frame, params)
    close = frame["close"].to_numpy(dtype=float)
    volume = frame["volume"].to_numpy(dtype=float)
    weight = volume if int(params["volume_weighting"]) == 1 else np.ones(len(close))
    raw = np.full(len(close), np.nan)
    raw[1:] = (close[1:] - close[:-1]) * weight[1:]
    alpha = 2.0 / (int(params["force_period"]) + 1.0)
    unrestricted = np.full(len(close), np.nan)
    e = raw[1]
    unrestricted[1] = e
    for t in range(2, len(close)):
        e = alpha * raw[t] + (1.0 - alpha) * e
        unrestricted[t] = e
    scale = np.full(len(close), np.nan)
    scale[1:] = np.maximum(1.0, np.maximum.accumulate(np.abs(raw[1:])))
    valid = out["efp_valid"].to_numpy() == 1.0
    finite = out["efp_force"].to_numpy(dtype=float)
    prev_unres = np.concatenate(([np.nan], unrestricted[:-1]))
    diff = np.abs(finite - unrestricted)
    norm = diff / scale
    sign_changed = valid & (np.sign(finite) != np.sign(unrestricted))
    up_ok = (out["efp_uptrend"].to_numpy() == 1.0) & (out["efp_prev_uptrend"].to_numpy() == 1.0)
    down_ok = (out["efp_downtrend"].to_numpy() == 1.0) & (out["efp_prev_downtrend"].to_numpy() == 1.0)
    vol_ok = out["efp_volume_eligible"].to_numpy() == 1.0
    alt = np.zeros(len(close), dtype=np.int64)
    alt[valid & (prev_unres < 0) & (unrestricted > 0) & up_ok & vol_ok] = 1
    alt[valid & (prev_unres > 0) & (unrestricted < 0) & down_ok & vol_ok] = -1
    signal = out["signal"].to_numpy(dtype=np.int64)
    entry_changed = valid & (alt != signal)
    worst = int(np.nanargmax(np.where(valid, norm, -1.0))) if valid.any() else None
    return {
        "params": params,
        "valid_bars": int(valid.sum()),
        "max_normalized_difference": float(np.nanmax(np.where(valid, norm, np.nan))) if valid.any() else None,
        "max_abs_difference": float(np.nanmax(np.where(valid, diff, np.nan))) if valid.any() else None,
        "worst_bar_ts_ms": int(frame["timestamp"].iloc[worst]) if worst is not None else None,
        "sign_changes": int(sign_changed.sum()),
        "entry_decision_changes": int(entry_changed.sum()),
        "entries_fixed": int((signal != 0).sum()),
        "entries_unrestricted": int((alt != 0).sum()),
        "within_tolerance": bool(valid.any() and float(np.nanmax(np.where(valid, norm, np.nan)))
                                 <= SENSITIVITY["tolerance"]),
    }


def sensitivity(reg, manifest: dict, params_sets: dict) -> dict:
    cells = []
    for ds in manifest["datasets"]:
        frame = _declared(om.load_candles(ds), manifest["interval"])
        for label, params in params_sets.items():
            for flag in (1, 0):
                cell = sensitivity_cell(reg, frame, dict(params, volume_weighting=flag))
                cells.append(dict(cell, dataset=ds["key"], label=label, rows=len(frame)))
    return {"tolerance": SENSITIVITY["tolerance"], "definition": SENSITIVITY["normalized_difference"],
            "unrestricted": SENSITIVITY["unrestricted"], "cells": cells,
            "all_within_tolerance": all(c["within_tolerance"] for c in cells)}


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
            header = list(EVIDENCE_INPUT_COLUMNS) + list(EVIDENCE_VALUE_COLUMNS) + ["efp_reason", "scored"]
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            lines = [",".join(header)]
            for i in range(len(result)):
                row = result.iloc[i]
                cells = [str(int(row["timestamp"]))]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_INPUT_COLUMNS[1:]]
                cells += [_fmt_full(row[c]) for c in EVIDENCE_VALUE_COLUMNS[:-1]]
                cells += [str(int(row["signal"])), str(row["efp_reason"]), str(int(scored[i]))]
                lines.append(",".join(cells))
            payload = ("\n".join(lines) + "\n").encode("utf-8")
            with open(path, "wb") as raw:
                with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
                    fh.write(payload)
            reasons = result["efp_reason"].to_numpy()[scored]
            out.append({"dataset": ds["key"], "window": window_name, "params": params,
                        "path": os.path.relpath(path, _REPO), "sha256": _sha(path), "rows": len(result),
                        "scored_rows": int(scored.sum()),
                        "entry_signals_scored": int((result["signal"].to_numpy() != 0)[scored].sum()),
                        "scored_reasons": {k: int((reasons == k).sum()) for k in sorted(set(reasons))}})
    return out


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    lock_record = verify_lock()
    manifest = om.load_manifest(manifest_path)
    reg = load_registry("futures")
    grid_check = check_grid(reg)
    if not grid_check["all_valid"]:
        raise SystemExit(f"optimizer grid differs from the frozen grid or holds invalid combinations: {grid_check}")
    gate = preflight(manifest)
    result = {
        "study": manifest["study"],
        "issue": SEED["issue"],
        "seed_sha256": _sha(SEED_PATH),
        "protocol_lock": lock_record,
        "seed": SEED,
        "manifest": _manifest_record(manifest),
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "incumbents": list(INCUMBENTS),
        "incumbent_contract": INCUMBENT_CONTRACT,
        "optimizer_grid_check": grid_check,
        "coverage_gate": gate,
    }
    if not gate["complete"]:
        result["verdict"] = verdict(gate, {}, {})
        return result

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
                            manifest=manifest, cost_multiplier=mult, incumbent_contract=INCUMBENT_CONTRACT,
                            intrabar_resolution=SEED["intrabar_resolution"], funding_mode=SEED["funding"])
        incumbents.setdefault(f"{window} x{mult:g}", incumbent_record(s))
        return summarize(s, window_bars[window], len(datasets))

    arms = {}
    for name in GRID_ARMS:
        strategy = strategy_of(name)
        defaults = dict(reg.STRATEGY_REGISTRY[strategy]["default_params"])
        train_rows = []
        for label, params in _grid(strategy, defaults):
            summary = score(name, params, SELECTION_WINDOW)
            summary.pop("per_dataset", None)
            train_rows.append({"label": label, "params": params, "summary": summary})
        choice = select(train_rows)
        arms[name] = {
            "strategy": strategy,
            "defaults": defaults,
            "direction": SEED["arms"][name]["direction"],
            "grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[strategy].items()},
            "train_sweep": train_rows,
            "selection": choice,
            "selected_plateau": neighbors(choice["params"], train_rows, strategy) if choice["selected"] else None,
        }
    partner = SEED["arms"][ABLATION]["paired_with"]
    ablation_params = paired_params(ABLATION, arms[partner]["selection"]["params"])
    ablation_train = score(ABLATION, ablation_params, SELECTION_WINDOW) if ablation_params else None
    if ablation_train is not None:
        ablation_train.pop("per_dataset", None)
    arms[ABLATION] = {
        "strategy": strategy_of(ABLATION),
        "defaults": paired_params(ABLATION, arms[partner]["defaults"]),
        "direction": SEED["arms"][ABLATION]["direction"],
        "paired_with": partner,
        "selection": {"rule_outcome": "paired" if ablation_params else "no_partner_selection",
                      "selected": "paired" if ablation_params else None, "params": ablation_params},
        "train_summary": ablation_train,
    }

    held = {}
    for name in ARMS:
        params = arms[name]["selection"]["params"]
        defaults = arms[name]["defaults"]
        entry = {"selected": None, "defaults": {
            "params": defaults,
            "base": score(name, defaults, HELD_OUT_WINDOW),
            "stress": score(name, defaults, HELD_OUT_WINDOW, STRESS_COST),
        }}
        if params is not None:
            entry["selected"] = {
                "params": params,
                "base": score(name, params, HELD_OUT_WINDOW),
                "stress": score(name, params, HELD_OUT_WINDOW, STRESS_COST),
            }
        held[name] = entry

    variants = []
    for row in arms[CANDIDATE]["train_sweep"]:
        test = score(CANDIDATE, row["params"], HELD_OUT_WINDOW)
        ablated = score(ABLATION, paired_params(ABLATION, row["params"]), HELD_OUT_WINDOW)
        variants.append({"label": row["label"], "params": row["params"],
                         "train_eligible": eligible(row["summary"]),
                         "train_mean_sharpe": row["summary"].get("mean_sharpe"),
                         "train_independent_positions": row["summary"]["independent_positions"],
                         "test_mean_sharpe": test.get("mean_sharpe"),
                         "test_mean_ddadj": test.get("mean_ddadj"),
                         "test_net_pnl_usd": test.get("net_pnl_usd"),
                         "test_positions": test["positions"],
                         "test_independent_positions": test["independent_positions"],
                         "ablation_test_mean_sharpe": ablated.get("mean_sharpe"),
                         "ablation_test_net_pnl_usd": ablated.get("net_pnl_usd"),
                         "ablation_test_positions": ablated["positions"]})
    scored = [v for v in variants if v["train_mean_sharpe"] is not None and v["test_mean_sharpe"] is not None]
    stability = {
        "spearman_train_vs_test_sharpe": spearman([v["train_mean_sharpe"] for v in scored],
                                                  [v["test_mean_sharpe"] for v in scored]),
        "test_net_positive_variants": sum(1 for v in variants if (v["test_net_pnl_usd"] or 0) > 0),
        "ablation_test_net_positive_variants": sum(1 for v in variants if (v["ablation_test_net_pnl_usd"] or 0) > 0),
        "train_eligible_variants": sum(1 for v in variants if v["train_eligible"]),
        "variants": len(variants),
    }

    validations = []
    for entry in held.values():
        for block in (entry["selected"], entry["defaults"]):
            for key in ("base", "stress"):
                if block is not None and block.get(key) is not None:
                    validations.append(block[key].get("close_validation"))
    evidence_params = arms[CANDIDATE]["selection"]["params"] or arms[CANDIDATE]["defaults"]
    sens_sets = {"seed": dict(SEED["params"])}
    if arms[CANDIDATE]["selection"]["params"]:
        sens_sets["selected"] = dict(arms[CANDIDATE]["selection"]["params"])
    result.update({
        "arms": arms,
        "held_out": held,
        "incumbent_reference": incumbents,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "numerical_sensitivity": sensitivity(reg, manifest, sens_sets),
        "evidence": write_evidence(reg, manifest, evidence_params),
        "verdict": verdict(gate, arms, held),
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


def _sci(v):
    return "-" if v is None else f"{v:.3e}"


def _summary_row(name: str, label: str, s: dict) -> str:
    if not s or not s.get("datasets"):
        return f"| `{name}` | {label} | no scored dataset |" + " |" * 11
    return (f"| `{name}` | {label} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['mean_return_pct'])} | "
            f"{_fmt(s['mean_sharpe'], 3)} | {_fmt(s['mean_ddadj'], 3)} | {_fmt(s['worst_max_dd_pct'])} | "
            f"{_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | {_fmt(s['mean_turnover'])} | "
            f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | {s['independent_positions']} | "
            f"{_fmt(s['top_dataset_abs_pnl_share'])} | {_fmt(s['top5_position_abs_pnl_share'])} |")


def _parity_lines() -> list:
    lines = ["", "## Decision parity (from `run_parity.py`)", ""]
    found = False
    for path in PARITY_PATHS:
        if not os.path.isfile(path):
            continue
        found = True
        with open(path) as fh:
            p = json.load(fh)
        ind = p["independent"]
        frozen = ind["frozen_cells"]
        lines.append(f"- `{os.path.basename(path)}` (batched {_fmt(p['batched'])}, last {p['bars']} bars per "
                     f"dataset, {p['parameter_sets']['grid_and_endpoints']} parameter sets): clean "
                     f"{_fmt(p['clean'])}; reference cells {len(frozen)}, reference mismatches "
                     f"{sum(c['reference_mismatches'] for c in frozen)}; prefix, span, 199- and 200-row comparisons "
                     f"{sum(c['comparisons'] for c in frozen)}, mismatches {sum(c['mismatches'] for c in frozen)}; "
                     f"boundary frames {len(ind['boundary_frames'])}, mismatches "
                     f"{sum(b['mismatches'] for b in ind['boundary_frames'])}.")
        for mode in ("entry", "composed"):
            for window in (200, 0):
                rows = [c for c in p["runner"]["cells"] if c["mode"] == mode and c["window"] == window]
                lines.append(f"  - `parity_diff` {mode} window={window}: cells {len(rows)}, bars "
                             f"{sum(c['bars_compared'] for c in rows)}, decision mismatches "
                             f"{sum(c['mismatches'] for c in rows)}, batch mismatches "
                             f"{sum(c.get('batch_mismatches', 0) for c in rows)}")
        if p["problems"]:
            lines.append("  - problems: " + "; ".join(p["problems"]))
    if not found:
        lines.append("No parity result file is present.")
    return lines


def render(result: dict) -> str:
    v = result["verdict"]
    m = result["manifest"]
    seed = result["seed"]
    lines = [
        "# Elder Force Index pullback entries: held-out result (#1668)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Registry evidence source for this outcome: `{v['edge_source']}`. {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification "
        "is in `README.md` and `candidate_seed.json` "
        f"(sha256 `{result['seed_sha256']}`, locked in `protocol_lock.json` before any score).",
        "",
        "Funding dependency #1731 is complete (merged pull request #1745). This study reuses its manifest "
        "funding attachment and adds no funding option or funding accounting.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{m['path']}` sha256 `{m['sha256']}`",
        f"- Provenance: {m['provenance']['kind']}: {m['provenance']['label']}",
        f"- Volume units: {m['provenance'].get('volume_units', '-')}",
        f"- Data reuse: {m['acquisition'].get('reused_from', '-')}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(
        f"- Every arm uses the manifest execution specification: taker "
        f"{c['taker_fee_pct']*100:.3f}%, maker {c['maker_fee_pct']*100:.3f}%, slippage {c['slippage_bps']} bps "
        f"per side plus the per-coin half spread, szDecimals lot flooring, minimum order "
        f"${c['min_notional_usd']:.0f} with a {c['min_notional_margin']*100:.0f}% margin, funding charged hourly.")
    lines.append(
        "- M1 incumbents run under the matched incumbent contract (the same explicit close, stop, comparison "
        "mode and the manifest execution specification, direction both) with the same attached funding.")
    lines.append(
        "- Candles and volume are historical Hyperliquid venue candles. The half spread and venue metadata are "
        "current pinned snapshots applied to every historical bar. No proxy candles or proxy volume are used.")
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
              "| Dataset | Window | Candle bars present/expected | Warm-up missing | Zero-volume rows | "
              "Funding hours present/expected | Accrual slots missing | Complete |",
              "|---|---|---|---|---|---|---|---|"]
    for r in gate["windows"]:
        cc = r.get("candle_coverage") or {}
        fc = r.get("funding_coverage") or {}
        ac = r.get("accrual_span") or {}
        lines.append(f"| {r['dataset']} | {r['window']} | {cc.get('present_bars', '-')}/{cc.get('expected_bars', '-')} | "
                     f"{cc.get('warmup_missing_bars', '-')} | {r.get('zero_volume_rows', '-')} | "
                     f"{fc.get('present_hours', '-')}/{fc.get('expected_hours', '-')} | "
                     f"{ac.get('missing_slots', '-')} | {_fmt(r['complete'])} |")
    for r in gate["refused"]:
        lines.append(f"- REFUSED {r['dataset']} {r['window']}: {'; '.join(r['problems'])}")
    lines += [
        "",
        "## Protocol",
        "",
        f"- Close owner `{seed['close_strategies'][0]['name']}` with the explicit ladder "
        f"`{json.dumps(seed['close_strategies'][0]['params']['tp_tiers'], sort_keys=True)}`, comparison mode "
        f"`{seed['comparison_mode']}`, intrabar `{seed['intrabar_resolution']}`, funding `{seed['funding']}`, sole "
        f"stop owner fixed entry ATR x{seed['stop_loss_atr_mult']}, capital ${seed['capital']:.0f}; signals on "
        "bar N fill at the bar N+1 open.",
        "- Arms: " + "; ".join(f"`{n}` ({seed['arms'][n]['strategy']}, {seed['arms'][n]['direction']}, "
                               f"{seed['arms'][n]['selection']})" for n in ARMS) + ".",
        f"- Selection on `{seed['selection_window']}` only: {seed['selection']['eligible']}; order "
        f"{'; '.join(seed['selection']['order'])}. The volume ablation reuses the candidate's selected "
        "parameters with `volume_weighting` 0.",
        f"- Held-out `{seed['held_out_window']}`: at least {MIN_TEST_POSITIONS} independent positions for every "
        "selected arm; the candidate needs both long and short positions.",
        f"- Cost runs: base x{BASE_COST:g} ({seed['cost_runs']['base']['meaning']}); stress x{STRESS_COST:g} "
        f"({seed['cost_runs']['stress']['meaning']}).",
        f"- Pass rule: {seed['verdict']['pass']}.",
        f"- Numerical tolerances fixed before scoring: reference abs {seed['numerical']['reference_tolerance']['abs']} "
        f"+ rel {seed['numerical']['reference_tolerance']['rel']}; zero-crossing and trend comparisons use no "
        f"epsilon; sensitivity tolerance {SENSITIVITY['tolerance']}.",
    ]
    grid = result.get("optimizer_grid_check") or {}
    lines.append(f"- Optimizer grid: {grid.get('combinations', '-')} combinations, matches the frozen grid "
                 f"{_fmt(grid.get('matches_frozen_grid'))}, invalid combinations (both weightings) "
                 f"{len(grid.get('invalid', []))}.")
    if "arms" not in result:
        lines += ["", "No selection or score was computed because the coverage gate refused a window.", ""]
        return "\n".join(lines)
    lines += ["", "## Selections (train)", "",
              "| Arm | Direction | Eligible combos | Selected params | Train mean Sharpe | Train independent positions | "
              "Neighbour median Sharpe |",
              "|---|---|---|---|---|---|---|"]
    for name in ARMS:
        arm = result["arms"][name]
        sel = arm["selection"]
        if name == ABLATION:
            ts = arm.get("train_summary") or {}
            lines.append(f"| `{name}` | {arm['direction']} | paired | "
                         f"{('`' + json.dumps(sel['params'], sort_keys=True) + '`') if sel['params'] else 'none'} | "
                         f"{_fmt(ts.get('mean_sharpe'), 3)} | {ts.get('independent_positions', '-')} | - |")
            continue
        row = next((r for r in arm["train_sweep"] if r["label"] == sel.get("selected")), None)
        plateau = arm["selected_plateau"] or {}
        lines.append(f"| `{name}` | {arm['direction']} | {sel['eligible_combos']}/{len(arm['train_sweep'])} | "
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
    for name in ARMS:
        t = result["held_out"][name]
        if t["selected"] is None:
            lines.append(f"| `{name}` | no selection |" + " |" * 12)
        else:
            lines.append(_summary_row(name, f"selected x{BASE_COST:g}", t["selected"]["base"]))
            lines.append(_summary_row(name, f"selected x{STRESS_COST:g}", t["selected"]["stress"]))
        lines.append(_summary_row(name, f"defaults x{BASE_COST:g}", t["defaults"]["base"]))
        lines.append(_summary_row(name, f"defaults x{STRESS_COST:g}", t["defaults"]["stress"]))
    sel_c = result["held_out"][CANDIDATE]["selected"]
    if sel_c is not None:
        lines += ["", "## Candidate per dataset (held-out, selected, base cost)", "",
                  "| Dataset | Net $ | Ret % | Sharpe | Max DD % | Fees $ | Funding $ | Positions (L/S) | "
                  "Time in market | Rejected entries | Skipped partial closes | Cost model |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for ds, s in sorted(sel_c["base"]["per_dataset"].items()):
            lines.append(f"| {ds} | {_fmt(s['net_pnl_usd'])} | {_fmt(s['return_pct'])} | {_fmt(s['sharpe'], 3)} | "
                         f"{_fmt(s['max_dd_pct'])} | {_fmt(s['fees_usd'])} | {_fmt(s['funding_pnl_usd'])} | "
                         f"{s['positions']} ({s['long_positions']}/{s['short_positions']}) | "
                         f"{_fmt(s['time_in_market'])} | {s['rejected_entries']} | {s['skipped_partial_closes']} | "
                         f"{s['cost_model']} |")
    lines += ["", "## Directional results and uncertainty (held-out, selected, base cost; descriptive)", "",
              "| Arm | Long net $ | Short net $ | Mean net $ per position | Bootstrap 5% | Bootstrap 95% |",
              "|---|---|---|---|---|---|"]
    for name in ARMS:
        sel = result["held_out"][name]["selected"]
        if sel is None or not sel["base"].get("datasets"):
            lines.append(f"| `{name}` | - | - | - | - | - |")
            continue
        b = sel["base"]
        boot = b["mean_position_net_pnl_bootstrap"]
        lines.append(f"| `{name}` | {_fmt(b['long_net_pnl_usd'])} | {_fmt(b['short_net_pnl_usd'])} | "
                     f"{_fmt(boot['mean'])} | {_fmt(boot['low'])} | {_fmt(boot['high'])} |")
    lines += ["", f"Bootstrap: {UNCERTAINTY['method']}, {UNCERTAINTY['resamples']} resamples, seed "
              f"{UNCERTAINTY['rng_seed']}; {UNCERTAINTY['role']}."]
    lines += ["", "## M1 incumbent reference (separate from the verdict)", "",
              "Median of the M1 incumbents (" + ", ".join(f"`{n}`" for n in result["incumbents"]) +
              ") per dataset, registry defaults, under the matched incumbent contract `" +
              json.dumps(result["incumbent_contract"], sort_keys=True) + "` with attached funding.", "",
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
        lines += ["", "The sample gate stopped the verdict before the performance gates. The same performance "
                  "checks, descriptive only and not a verdict input:", ""]
        for k, ok in sorted(v["performance_checks_descriptive"].items()):
            lines.append(f"- {k}: {_fmt(ok)}")
    st = result["parameter_stability"]
    lines += [
        "",
        "## Parameter stability and unsuccessful variants",
        "",
        f"- Training-eligible candidate variants: {st['train_eligible_variants']}/{st['variants']}",
        f"- Spearman rank correlation of train vs held-out mean Sharpe: {_fmt(st['spearman_train_vs_test_sharpe'], 3)}",
        f"- Held-out net-positive variants: {st['test_net_positive_variants']}/{st['variants']} "
        f"(paired volume ablation: {st['ablation_test_net_positive_variants']}/{st['variants']})",
        "",
        "| Variant | Train eligible | Train Sharpe | Train independent | Held-out Sharpe | Held-out DDadj | "
        "Held-out net $ | Held-out positions | Held-out independent | Ablation Sharpe | Ablation net $ |",
        "|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| {row['label']} | {_fmt(row['train_eligible'])} | {_fmt(row['train_mean_sharpe'], 3)} | "
                     f"{row['train_independent_positions']} | {_fmt(row['test_mean_sharpe'], 3)} | "
                     f"{_fmt(row['test_mean_ddadj'], 3)} | {_fmt(row['test_net_pnl_usd'])} | "
                     f"{row['test_positions']} | {row['test_independent_positions']} | "
                     f"{_fmt(row['ablation_test_mean_sharpe'], 3)} | {_fmt(row['ablation_test_net_pnl_usd'])} |")
    sens = result["numerical_sensitivity"]
    lines += ["", "## Fixed-length versus unrestricted average", "",
              f"Definition: {sens['definition']}. Unrestricted: {sens['unrestricted']}. Tolerance "
              f"{sens['tolerance']} (fixed before scoring). All cells within tolerance: "
              f"{_fmt(sens['all_within_tolerance'])}. This comparison is descriptive; the finite contract stays.", "",
              "| Dataset | Params | Weighting | Valid bars | Max normalized diff | Max abs diff | Sign changes | "
              "Entry changes | Entries fixed/unrestricted | Within tolerance |",
              "|---|---|---|---|---|---|---|---|---|---|"]
    for cell in sens["cells"]:
        lines.append(f"| {cell['dataset']} | {cell['label']} | {cell['params']['volume_weighting']} | "
                     f"{cell['valid_bars']} | {_sci(cell['max_normalized_difference'])} | "
                     f"{_sci(cell['max_abs_difference'])} | {cell['sign_changes']} | "
                     f"{cell['entry_decision_changes']} | {cell['entries_fixed']}/{cell['entries_unrestricted']} | "
                     f"{_fmt(cell['within_tolerance'])} |")
    lines += _parity_lines()
    cv = result.get("close_validation") or {}
    lines += ["", "## Close validation", "",
              f"- Modes `{', '.join(cv.get('modes') or [str(cv.get('mode'))])}`, eligibility "
              f"`{cv.get('close_eligibility')}`, parity status `{cv.get('parity_status')}`, incomplete parity "
              f"{_fmt(cv.get('incomplete_parity'))}.",
              "- `unverified` is the strict, eligible capability status (no approximation and no refusal). It is "
              "not a ledger-verified claim that these exits equal live exits.",
              "", "## Full-precision evidence", "",
              "Every input row and every evaluator value of the evidence parameters, written with `repr` floats.", "",
              "| Dataset | Window | File | sha256 | Rows | Scored rows | Scored entry signals | Scored reasons |",
              "|---|---|---|---|---|---|---|---|"]
    for e in result["evidence"]:
        reasons = ", ".join(f"{k} {n}" for k, n in e["scored_reasons"].items())
        lines.append(f"| {e['dataset']} | {e['window']} | `{e['path']}` | `{e['sha256'][:16]}` | {e['rows']} | "
                     f"{e['scored_rows']} | {e['entry_signals_scored']} | {reasons} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines += ["", "---", "Created with LLM: Opus 5.5 | high | Harness: Claude Code", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1668 frozen held-out study driver (suggest-only)")
    p.add_argument("command", choices=("lock", "run", "render"))
    p.add_argument("--manifest", default=MANIFEST)
    p.add_argument("--json", default=os.path.join(_HERE, "results.json"), dest="json_out")
    p.add_argument("--report", default=os.path.join(_HERE, "REPORT.md"))
    args = p.parse_args(argv)
    if args.command == "lock":
        record = lock()
        print(f"locked candidate_seed.json {record['candidate_seed_sha256']} manifest {record['manifest_sha256']}")
        return 0
    if args.command == "render":
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
    print(f"verdict: {result['verdict']['outcome']} ({result['verdict']['reason']}); "
          f"edge_source {result['verdict']['edge_source']}")
    print(f"wrote {args.json_out} and {args.report}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
