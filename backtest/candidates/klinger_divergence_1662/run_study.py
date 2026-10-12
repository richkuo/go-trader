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
sys.path.insert(0, os.path.join(_REPO, "shared_strategies", "open"))

import offline_manifest as om
from backtester import aggregate_close_validations
from eval_windows import INCUMBENTS, evaluate_window, expand_sweep, manifest_datasets
from optimizer import DEFAULT_PARAM_RANGES

SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "klinger_divergence_1662_manifest.json")
EVIDENCE_DIR = os.path.join(_HERE, "evidence")
PARITY_PATH = os.path.join(_HERE, "parity_results.json")
HOUR_MS = 3_600_000
PARITY_BARS = 600
LIVE_WINDOW = 200
INDICATOR_TOLERANCE = 1e-9
SOURCE_FILES = (
    "shared_strategies/open/klinger_divergence.py",
    "shared_strategies/open/on_balance_volume.py",
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
    "backtest/candidates/klinger_divergence_1662/candidate_seed.json",
    "backtest/candidates/klinger_divergence_1662/run_study.py",
)
EVIDENCE_INPUT_COLUMNS = ("timestamp", "open", "high", "low", "close", "volume")
EVIDENCE_VALUE_COLUMNS = (
    "klinger_eval_ts_ms", "klinger_trend", "klinger_dm", "klinger_cm", "klinger_volume_force",
    "klinger_fast_ema", "klinger_slow_ema", "klinger_oscillator", "klinger_signal_line",
    "klinger_prev_oscillator", "klinger_prev_signal_line", "klinger_cross", "klinger_endpoint_valid",
    "klinger_long_state", "klinger_long_pivot1_ts_ms", "klinger_long_pivot1_price", "klinger_long_pivot1_oscillator",
    "klinger_long_pivot2_ts_ms", "klinger_long_pivot2_price", "klinger_long_pivot2_oscillator",
    "klinger_long_confirm_ts_ms", "klinger_long_expiry_ts_ms",
    "klinger_short_state", "klinger_short_pivot1_ts_ms", "klinger_short_pivot1_price",
    "klinger_short_pivot1_oscillator", "klinger_short_pivot2_ts_ms", "klinger_short_pivot2_price",
    "klinger_short_pivot2_oscillator", "klinger_short_confirm_ts_ms", "klinger_short_expiry_ts_ms",
    "klinger_valid", "klinger_reason_code", "signal",
)
AUDIT_COLUMNS = (
    "klinger_oscillator", "klinger_signal_line", "klinger_prev_oscillator", "klinger_prev_signal_line",
    "klinger_cross", "klinger_support_start_ts_ms",
)
INDICATOR_COLUMNS = ("klinger_oscillator", "klinger_signal_line", "klinger_prev_oscillator",
                     "klinger_prev_signal_line", "klinger_cross", "klinger_cm", "klinger_volume_force")
EXACT_COLUMNS = ("signal", "klinger_reason", "klinger_valid", "klinger_long_state", "klinger_short_state",
                 "klinger_long_pivot1_ts_ms", "klinger_long_pivot2_ts_ms", "klinger_short_pivot1_ts_ms",
                 "klinger_short_pivot2_ts_ms")


def load_seed(path: str = SEED_PATH) -> dict:
    with open(path) as fh:
        return json.load(fh)


SEED = load_seed()
CANDIDATE = SEED["name"]
ARMS = tuple(SEED["arms"])
CANDIDATE_ARMS = tuple(SEED["candidate_arms"])
COMPARISONS = {k: tuple(v) for k, v in SEED["comparisons"].items()}
SELECTION_WINDOW = SEED["selection_window"]
HELD_OUT_WINDOW = SEED["held_out_window"]
BASE_COST = SEED["cost_runs"]["base"]["cost_multiplier"]
STRESS_COST = SEED["cost_runs"]["stress"]["cost_multiplier"]
MIN_TRAIN_POSITIONS = SEED["selection"]["min_train_independent_positions"]
MIN_TEST_POSITIONS = SEED["held_out"]["min_independent_positions_per_candidate_arm"]
INCUMBENT_CONTRACT = SEED["incumbent_contract"]


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


def max_candidate_support() -> int:
    from klinger_divergence import validate_klinger_params, PARAM_NAMES
    out = []
    for _, params in _grid(CANDIDATE, SEED["params"]):
        out.append(validate_klinger_params(*(params[k] for k in PARAM_NAMES))["support"])
    out.append(validate_klinger_params(*(SEED["params"][k] for k in PARAM_NAMES))["support"])
    return max(out)


def _window_preflight(manifest: dict, ds: dict, window_name: str, need: int) -> dict:
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
    out["zero_range_rows"] = int((h == lo).sum())
    if bad:
        out["problems"].append(f"candles: {bad} rows with invalid prices, geometry or volume")
    out["required_warmup_bars"] = need
    if manifest["warmup_bars"] < need:
        out["problems"].append(f"candles: warm-up {manifest['warmup_bars']} bars is below the {need}-bar support")
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
    need = max_candidate_support()
    rows = [_window_preflight(manifest, ds, w, need) for ds in manifest["datasets"]
            for w in (SELECTION_WINDOW, HELD_OUT_WINDOW)]
    refused = [{"dataset": r["dataset"], "window": r["window"], "problems": r["problems"]}
               for r in rows if not r["complete"]]
    return {"complete": not refused, "refused": refused, "windows": rows, "max_candidate_support_bars": need}


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


def score_positions(score: dict) -> list:
    out = []
    for r in score["rows"]:
        if r["leg"] is None:
            continue
        for p in r["leg"]["execution"]["position_list"]:
            out.append(dict(p, dataset=r["dataset"]))
    return out


def summarize(score: dict, window_bars: dict, expected: int) -> dict:
    rows = [r for r in score["rows"] if r["leg"] is not None]
    if not rows:
        return {"datasets": 0, "expected_datasets": expected, "positions": 0, "independent_positions": 0,
                "long_positions": 0, "short_positions": 0}
    legs = [r["leg"] for r in rows]
    positions = score_positions(score)
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
        "rejected_entries": sum(v["rejected_entries"] for v in per_dataset.values()),
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


def continuous_series(frame: pd.DataFrame, p: dict) -> tuple:
    high = frame["high"].to_numpy(dtype=float)
    low = frame["low"].to_numpy(dtype=float)
    close = frame["close"].to_numpy(dtype=float)
    volume = frame["volume"].to_numpy(dtype=float)
    n = len(close)
    af = 2.0 / (p["fast_period"] + 1)
    asl = 2.0 / (p["slow_period"] + 1)
    ag = 2.0 / (p["signal_period"] + 1)
    osc = np.full(n, np.nan)
    sig = np.full(n, np.nan)
    ok = np.zeros(n, dtype=bool)
    trend = -1.0
    dm = float(high[0] - low[0])
    cm = dm
    defined = cm > 0
    vf = volume[0] * 2.0 * ((dm / cm if cm > 0 else 0.0) - 1.0) * trend * 100.0
    fe = se = vf
    s = fe - se
    osc[0], sig[0], ok[0] = fe - se, s, defined
    hlc = (high + low) + close
    for j in range(1, n):
        nt = 1.0 if hlc[j] > hlc[j - 1] else -1.0
        d = float(high[j] - low[j])
        cm = cm + d if nt == trend else float(high[j - 1] - low[j - 1]) + d
        trend = nt
        if not cm > 0:
            defined = False
        vf = volume[j] * 2.0 * ((d / cm if cm > 0 else 0.0) - 1.0) * trend * 100.0
        fe = af * vf + (1.0 - af) * fe
        se = asl * vf + (1.0 - asl) * se
        o = fe - se
        s = ag * o + (1.0 - ag) * s
        osc[j], sig[j], ok[j] = o, s, defined and math.isfinite(o) and math.isfinite(s)
    return osc, sig, ok


def continuous_decisions(frame: pd.DataFrame, params: dict) -> np.ndarray:
    import klinger_divergence as kd
    p = kd.validate_klinger_params(*(params[k] for k in kd.PARAM_NAMES))
    osc, sig, ok = continuous_series(frame, p)
    n = len(osc)
    osc = np.where(ok, osc, np.nan)
    sig = np.where(ok, sig, np.nan)
    prev_ok = np.r_[False, ok[:-1]]
    prev_osc = np.r_[np.nan, osc[:-1]]
    prev_sig = np.r_[np.nan, sig[:-1]]
    cross = np.zeros(n, dtype=np.int64)
    with np.errstate(invalid="ignore"):
        both = ok & prev_ok
        cross[both & (prev_osc <= prev_sig) & (osc > sig)] = 1
        cross[both & (prev_osc >= prev_sig) & (osc < sig)] = -1
    row_ok = np.ones(n, dtype=bool)
    high = frame["high"].to_numpy(dtype=float)
    low = frame["low"].to_numpy(dtype=float)
    lp = kd._strict_pivots(low, row_ok, p["left_span"], p["right_span"], lows=True)
    hp = kd._strict_pivots(high, row_ok, p["left_span"], p["right_span"], lows=False)
    ls = kd._side_setups(lp, low, osc, ok, cross, p, True, n)
    ss = kd._side_setups(hp, high, osc, ok, cross, p, False, n)
    ts = np.arange(n, dtype=float)
    lrow = kd._side_rows(ls, n, ts, 1, low, osc, p["setup_expiry"])["state"]
    srow = kd._side_rows(ss, n, ts, 1, high, osc, p["setup_expiry"])["state"]
    le, se = lrow == kd.STATE_EMITTED, srow == kd.STATE_EMITTED
    out = np.zeros(n, dtype=np.int64)
    out[le & ~se] = 1
    out[se & ~le] = -1
    out[~ok] = 0
    return out, ok


def continuous_reference(reg, manifest: dict, params: dict) -> dict:
    out = {"params": params, "datasets": []}
    keys = ("scored_rows", "finite_valid_rows", "finite_zero_cm_rows", "decision_mismatches",
            "finite_invalid_hold", "continuous_invalid", "seed_divergence", "unclassified",
            "finite_entries", "continuous_entries", "entries_both", "same_side_entries")
    totals = {k: 0 for k in keys}
    for ds in manifest["datasets"]:
        candles = om.load_candles(ds)
        cont_sig, cont_ok = continuous_decisions(candles, params)
        osc_c, sig_c, _ = continuous_series(candles, {**params, **{}})
        ts_all = candles["timestamp"].to_numpy(dtype=np.int64)
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            finite = reg.apply_strategy(CANDIDATE, frame, params)
            pos = np.searchsorted(ts_all, frame["timestamp"].to_numpy(dtype=np.int64))
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            fsig = finite["signal"].to_numpy()
            freason = finite["klinger_reason"].to_numpy()
            fvalid = finite["klinger_valid"].to_numpy() == 1.0
            csig = cont_sig[pos]
            cok = cont_ok[pos]
            fosc = finite["klinger_oscillator"].to_numpy()
            cosc = osc_c[pos]
            both = scored & (finite["klinger_endpoint_valid"].to_numpy() == 1.0) & np.isfinite(cosc)
            diff = np.abs(fosc[both] - cosc[both])
            scale = np.maximum(1.0, np.abs(cosc[both]))
            mism = scored & (fsig != csig)
            finite_hold = mism & ~fvalid & (csig != 0)
            cont_invalid = mism & ~cok & (fsig != 0)
            seed_div = mism & fvalid & cok
            unclassified = mism & ~finite_hold & ~cont_invalid & ~seed_div
            rec = {
                "dataset": ds["key"], "window": window_name, "scored_rows": int(scored.sum()),
                "finite_valid_rows": int((scored & fvalid).sum()),
                "finite_zero_cm_rows": int((scored & (freason == "zero_cm")).sum()),
                "finite_entries": int((scored & (fsig != 0)).sum()),
                "continuous_entries": int((scored & (csig != 0)).sum()),
                "entries_both": int((scored & (fsig != 0) & (csig != 0)).sum()),
                "same_side_entries": int((scored & (fsig != 0) & (fsig == csig)).sum()),
                "decision_mismatches": int(mism.sum()),
                "finite_invalid_hold": int(finite_hold.sum()),
                "continuous_invalid": int(cont_invalid.sum()),
                "seed_divergence": int(seed_div.sum()),
                "unclassified": int(unclassified.sum()),
                "oscillator_abs_diff_max": float(diff.max()) if len(diff) else None,
                "oscillator_abs_diff_median": float(np.median(diff)) if len(diff) else None,
                "oscillator_rel_diff_p95": float(np.percentile(diff / scale, 95)) if len(diff) else None,
                "oscillator_sign_agreement": (float(np.mean(np.sign(fosc[both]) == np.sign(cosc[both])))
                                              if len(diff) else None),
            }
            out["datasets"].append(rec)
            for key in keys:
                totals[key] += rec[key]
    totals["mismatch_share_of_scored_rows"] = (round(totals["decision_mismatches"] / totals["scored_rows"], 6)
                                               if totals["scored_rows"] else None)
    out["totals"] = totals
    out["all_mismatches_classified"] = totals["unclassified"] == 0
    return out


def _close_enough(a: float, b: float) -> bool:
    if not (math.isfinite(a) or math.isfinite(b)):
        return (math.isnan(a) and math.isnan(b)) or a == b
    if not (math.isfinite(a) and math.isfinite(b)):
        return False
    return abs(a - b) <= INDICATOR_TOLERANCE * max(1.0, abs(a), abs(b))


def _exact(a, b) -> bool:
    if isinstance(a, float) and isinstance(b, float) and math.isnan(a) and math.isnan(b):
        return True
    return a == b


def indicator_parity(reg, manifest: dict, params: dict) -> dict:
    import klinger_divergence as kd
    support = kd.validate_klinger_params(*(params[k] for k in kd.PARAM_NAMES))["support"]
    runs = []
    for ds in manifest["datasets"]:
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            data = frame[list(EVIDENCE_INPUT_COLUMNS[1:])].copy()
            data.attrs = dict(frame.attrs)
            full = reg.apply_strategy(CANDIDATE, data, params)
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            ends = np.flatnonzero((ts >= win["start_ms"]) & (ts < win["end_ms"]))
            counts = {"prefix": 0, "tail200": 0, "exact_support": 0}
            bad = {"prefix": [], "tail200": [], "exact_support": []}
            entries = 0
            for k in ends:
                k = int(k)
                want = full.iloc[k]
                entries += int(want["signal"] != 0)
                for kind, start in (("prefix", 0), ("tail200", k + 1 - LIVE_WINDOW),
                                    ("exact_support", k + 1 - support)):
                    part = data.iloc[start:k + 1].copy()
                    part.attrs = dict(data.attrs)
                    got = reg.apply_strategy(CANDIDATE, part, params).iloc[-1]
                    counts[kind] += 1
                    ok = all(_close_enough(float(got[c]), float(want[c])) for c in INDICATOR_COLUMNS)
                    ok = ok and all(_exact(got[c], want[c]) for c in EXACT_COLUMNS)
                    if not ok and len(bad[kind]) < 5:
                        bad[kind].append(str(frame.index[k]))
                    elif not ok:
                        bad[kind].append(None)
            runs.append({"dataset": ds["key"], "window": window_name, "endpoints": len(ends),
                         "entries_compared": entries, "compared": counts,
                         "mismatches": {k: len(v) for k, v in bad.items()},
                         "first_mismatches": {k: [x for x in v if x][:5] for k, v in bad.items()}})
    return {"params": params, "support_bars": support, "tolerance": INDICATOR_TOLERANCE,
            "indicator_columns": list(INDICATOR_COLUMNS), "exact_columns": list(EXACT_COLUMNS),
            "runs": runs, "clean": all(sum(r["mismatches"].values()) == 0 for r in runs)}


def decision_parity(manifest: dict, params: dict) -> dict:
    from parity_diff import ParityConfig, compute_parity_frame, summarize as parity_summarize
    out = []
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        tail = frame[list(EVIDENCE_INPUT_COLUMNS[1:])].iloc[-PARITY_BARS:].copy()
        tail.attrs = dict(frame.attrs)
        for batched in (False, True):
            for window in (LIVE_WINDOW, None):
                cfg = ParityConfig(strategy_name=CANDIDATE, params=dict(params), registry="futures",
                                   platform="hyperliquid", symbol=ds["coin"], timeframe=manifest["interval"],
                                   close_refs=json.loads(json.dumps(SEED["close_strategies"])), direction="both",
                                   batched=batched)
                pf = compute_parity_frame(tail, cfg=cfg, window=window)
                summary = parity_summarize(pf)
                summary.pop("close_validation", None)
                out.append({
                    "dataset": ds["key"], "mode": "batched_paper" if batched else "composer",
                    "window": window if window is not None else "every_prefix",
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


def verdict(gate: dict, arms: dict, held: dict, continuous: dict, parity_clean: bool) -> dict:
    if not gate["complete"]:
        return {"outcome": "inconclusive", "edge_source": "study_inconclusive", "live_status": LIVE_STATUS,
                "checks": {"coverage_complete": False},
                "reason": "coverage gate refused " + ", ".join(f"{r['dataset']} {r['window']}" for r in gate["refused"])}
    unselected = [n for n in ARMS if arms[n]["selection"]["params"] is None]
    if unselected:
        return {"outcome": "inconclusive", "edge_source": "study_inconclusive", "live_status": LIVE_STATUS,
                "checks": {"coverage_complete": True, "every_arm_selected": False},
                "reason": "no eligible training selection for " + ", ".join(unselected)}
    checks = {"coverage_complete": all(held[n]["selected"][k].get("coverage_complete", False)
                                       for n in ARMS for k in ("base", "stress")),
              "every_arm_selected": True,
              "no_liquidated_leg": all(held[n]["selected"][k].get("liquidated_legs", 1) == 0
                                       for n in ARMS for k in ("base", "stress")),
              "continuous_mismatches_classified": bool(continuous and continuous["all_mismatches_classified"]),
              "parity_clean": bool(parity_clean)}
    for arm in CANDIDATE_ARMS:
        checks[f"{arm}_independent_positions_sufficient"] = (
            held[arm]["selected"]["base"]["independent_positions"] >= MIN_TEST_POSITIONS)
    if SEED["held_out"]["bidirectional_arm_requires_both_directions"]:
        for arm in CANDIDATE_ARMS:
            if SEED["arms"][arm]["direction"] == "both":
                s = held[arm]["selected"]["base"]
                checks[f"{arm}_long_and_short_occur"] = s["long_positions"] > 0 and s["short_positions"] > 0
    performance = {}
    for arm in CANDIDATE_ARMS:
        base = held[arm]["selected"]["base"]
        performance[f"{arm}_net_positive_base"] = base["net_pnl_usd"] > 0
        performance[f"{arm}_net_positive_stress"] = held[arm]["selected"]["stress"]["net_pnl_usd"] > 0
        for comp in COMPARISONS[arm]:
            other = held[comp]["selected"]["base"]
            performance[f"{arm}_return_above_{comp}"] = base["mean_return_pct"] > other["mean_return_pct"]
            performance[f"{arm}_drawdown_not_worse_than_{comp}"] = base["worst_max_dd_pct"] <= other["worst_max_dd_pct"]
    if not all(checks.values()):
        short = sorted(k for k, ok in checks.items() if not ok)
        return {"outcome": "inconclusive", "edge_source": "study_inconclusive", "live_status": LIVE_STATUS,
                "checks": checks, "performance_checks_descriptive": performance,
                "reason": "held-out coverage, sample, reference or parity gate not met: " + ", ".join(short)}
    checks.update(performance)
    if all(checks.values()):
        return {"outcome": "pass", "edge_source": "unvalidated", "live_status": LIVE_STATUS, "checks": checks,
                "reason": "every frozen held-out check passed"}
    failed = sorted(k for k, ok in checks.items() if not ok)
    return {"outcome": "fail", "edge_source": "study_fail", "live_status": LIVE_STATUS, "checks": checks,
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


def write_evidence(reg, manifest: dict, arm: str, params: dict) -> list:
    os.makedirs(EVIDENCE_DIR, exist_ok=True)
    out = []
    for ds in manifest["datasets"]:
        for window_name in (SELECTION_WINDOW, HELD_OUT_WINDOW):
            frame, win, _ = om.window_frame(manifest, ds, window_name)
            result = reg.apply_strategy(CANDIDATE, frame, params)
            name = f"{ds['coin']}_{window_name}_{arm}.csv.gz"
            path = os.path.join(EVIDENCE_DIR, name)
            header = list(EVIDENCE_INPUT_COLUMNS) + list(EVIDENCE_VALUE_COLUMNS) + ["klinger_reason", "scored"]
            ts = frame["timestamp"].to_numpy(dtype=np.int64)
            scored = (ts >= win["start_ms"]) & (ts < win["end_ms"])
            lines = [",".join(header)]
            cols = {c: result[c].to_numpy() for c in list(EVIDENCE_INPUT_COLUMNS) + list(EVIDENCE_VALUE_COLUMNS)}
            reasons = result["klinger_reason"].to_numpy()
            for i in range(len(result)):
                cells = [str(int(cols["timestamp"][i]))]
                cells += [_fmt_full(cols[c][i]) for c in EVIDENCE_INPUT_COLUMNS[1:]]
                cells += [_fmt_full(cols[c][i]) for c in EVIDENCE_VALUE_COLUMNS[:-1]]
                cells += [str(int(cols["signal"][i])), str(reasons[i]), str(int(scored[i]))]
                lines.append(",".join(cells))
            payload = ("\n".join(lines) + "\n").encode("utf-8")
            with open(path, "wb") as raw:
                with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as fh:
                    fh.write(payload)
            counts = pd.Series(reasons[scored]).value_counts().to_dict()
            out.append({"dataset": ds["key"], "window": window_name, "arm": arm, "params": params,
                        "path": os.path.relpath(path, _REPO), "sha256": _sha(path), "rows": len(result),
                        "scored_rows": int(scored.sum()),
                        "entry_signals_scored": int((result["signal"].to_numpy() != 0)[scored].sum()),
                        "scored_reason_counts": {str(k): int(v) for k, v in sorted(counts.items())}})
    return out


def entry_audit(reg, manifest: dict, arm: str, params: dict, positions: list) -> list:
    frames = {}
    for ds in manifest["datasets"]:
        frame, _, _ = om.window_frame(manifest, ds, HELD_OUT_WINDOW)
        frames[ds["key"]] = (frame, reg.apply_strategy(CANDIDATE, frame, params))
    rows = []
    for p in sorted(positions, key=lambda x: (x["entry_date"], x["dataset"])):
        frame, result = frames[p["dataset"]]
        entry = pd.Timestamp(p["entry_date"])
        if entry.tzinfo is None and result.index.tz is not None:
            entry = entry.tz_localize(result.index.tz)
        pos = int(result.index.get_loc(entry))
        decision = result.iloc[pos - 1]
        side_key = "long" if p["side"] == "long" else "short"
        rec = {
            "dataset": p["dataset"], "side": p["side"], "decision_bar": str(result.index[pos - 1]),
            "entry_bar": str(result.index[pos]), "exit_date": p["exit_date"],
            "decision_signal": int(decision["signal"]), "decision_reason": str(decision["klinger_reason"]),
            "fill_after_decision_bar": bool(pos >= 1 and result.index[pos] > result.index[pos - 1]),
            "entry_open": float(frame["open"].iloc[pos]),
            "net_pnl": p["net_pnl"], "fees": p["fees"], "bars_held": p["bars_held"],
            "exit_reasons": p["exit_reasons"],
        }
        for col in AUDIT_COLUMNS:
            rec[col] = float(decision[col])
        for field in ("pivot1_ts_ms", "pivot1_price", "pivot1_oscillator", "pivot2_ts_ms", "pivot2_price",
                      "pivot2_oscillator", "confirm_ts_ms", "expiry_ts_ms"):
            rec[f"{side_key}_{field}"] = float(decision[f"klinger_{side_key}_{field}"])
        rows.append(rec)
    return rows


def run(manifest_path: str) -> dict:
    from registry_loader import load_registry
    manifest = om.load_manifest(manifest_path)
    gate = preflight(manifest)
    result = {
        "study": manifest["study"],
        "issue": SEED["issue"],
        "tracking_group": SEED["tracking_group"],
        "seed_sha256": _sha(SEED_PATH),
        "protocol_sha256": protocol_sha256(),
        "seed": SEED,
        "manifest": _manifest_record(manifest),
        "source_sha256": {p: _sha(os.path.join(_REPO, p)) for p in SOURCE_FILES},
        "incumbents": list(INCUMBENTS),
        "incumbent_contract": INCUMBENT_CONTRACT,
        "coverage_gate": gate,
    }
    if not gate["complete"]:
        result["verdict"] = verdict(gate, {}, {}, {}, False)
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
    raw_scores = {}

    def score(arm, params, window, mult=BASE_COST, keep=False):
        s = evaluate_window(reg, arm_candidate(arm, params), datasets, window, SEED["capital"], memo,
                            manifest=manifest, cost_multiplier=mult, incumbent_contract=INCUMBENT_CONTRACT,
                            intrabar_resolution=SEED["intrabar_resolution"])
        incumbents.setdefault(f"{window} x{mult:g}", incumbent_record(s))
        if keep:
            raw_scores[(arm, window, mult)] = s
        return summarize(s, window_bars[window], len(datasets))

    arms = {}
    for arm in ARMS:
        spec = SEED["arms"][arm]
        name = arm_strategy(arm)
        defaults = dict(reg.STRATEGY_REGISTRY[name]["default_params"])
        entry = {"strategy": name, "defaults": defaults, "direction": spec["direction"],
                 "selection_kind": spec["selection"]}
        if spec["selection"] == "grid":
            train_rows = []
            for label, params in _grid(name, defaults):
                summary = score(arm, params, SELECTION_WINDOW)
                summary.pop("per_dataset", None)
                train_rows.append({"label": label, "params": params, "summary": summary})
            choice = select(train_rows)
            entry.update({
                "grid": {k: list(v) for k, v in DEFAULT_PARAM_RANGES[name].items()},
                "train_sweep": train_rows,
                "selection": choice,
                "selected_plateau": neighbors(choice["params"], train_rows, name) if choice["selected"] else None,
            })
        arms[arm] = entry
    for arm in ARMS:
        spec = SEED["arms"][arm]
        if spec["selection"] != "inherit":
            continue
        parent = arms[spec["inherits"]]["selection"]
        params = None if parent["params"] is None else dict(parent["params"], **spec["override"])
        arms[arm]["selection"] = {"rule_outcome": "inherited" if params else "parent_unselected",
                                  "selected": f"{spec['inherits']} selection with {spec['override']}",
                                  "params": params, "eligible_combos": None}
        arms[arm]["train"] = score(arm, params, SELECTION_WINDOW) if params else None
        arms[arm]["selected_plateau"] = None

    held = {}
    for arm in ARMS:
        params = arms[arm]["selection"]["params"]
        defaults = arms[arm]["defaults"]
        if SEED["arms"][arm]["selection"] == "inherit":
            defaults = dict(defaults, **SEED["arms"][arm]["override"])
        entry = {"selected": None, "defaults": {
            "params": defaults,
            "base": score(arm, defaults, HELD_OUT_WINDOW),
            "stress": score(arm, defaults, HELD_OUT_WINDOW, STRESS_COST),
        }}
        if params is not None:
            entry["selected"] = {
                "params": params,
                "base": score(arm, params, HELD_OUT_WINDOW, keep=True),
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
                             "test_mean_return_pct": test.get("mean_return_pct"),
                             "test_mean_ddadj": test.get("mean_ddadj"),
                             "test_worst_max_dd_pct": test.get("worst_max_dd_pct"),
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
    audits = {}
    evidence = []
    for arm in CANDIDATE_ARMS + tuple(a for a in ARMS if SEED["arms"][a]["selection"] == "inherit"):
        params = arms[arm]["selection"]["params"]
        if params is None:
            continue
        positions = score_positions(raw_scores[(arm, HELD_OUT_WINDOW, BASE_COST)])
        audits[arm] = entry_audit(reg, manifest, arm, params, positions)
        if arm in CANDIDATE_ARMS:
            evidence += write_evidence(reg, manifest, arm, params)
    evidence_params = arms["klinger_both"]["selection"]["params"] or arms["klinger_both"]["defaults"]
    continuous = continuous_reference(reg, manifest, evidence_params)
    parity = {"decision": decision_parity(manifest, evidence_params),
              "indicator": indicator_parity(reg, manifest, evidence_params)}
    parity["clean"] = parity["decision"]["clean"] and parity["indicator"]["clean"]
    with open(PARITY_PATH, "w") as fh:
        json.dump(parity, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    result.update({
        "arms": arms,
        "held_out": held,
        "incumbent_reference": incumbents,
        "candidate_variants_held_out": variants,
        "parameter_stability": stability,
        "entry_audit": audits,
        "evidence_params": evidence_params,
        "evidence": evidence,
        "continuous_reference": continuous,
        "parity": {"path": os.path.relpath(PARITY_PATH, _REPO), "sha256": _sha(PARITY_PATH),
                   "clean": parity["clean"], "decision_clean": parity["decision"]["clean"],
                   "indicator_clean": parity["indicator"]["clean"]},
        "verdict": verdict(gate, arms, held, continuous, parity["clean"]),
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
        "# Klinger Oscillator divergence entries: held-out result (#1662)",
        "",
        f"Verdict: **{v['outcome'].upper()}**. {v['reason']}.",
        "",
        f"Recorded edge source: `{v['edge_source']}`. Availability: {v['live_status']}.",
        "",
        "Generated by `run_study.py` from `results.json`; do not edit by hand. The frozen specification "
        "is in `README.md` and `candidate_seed.json` "
        f"(file sha256 `{result['seed_sha256']}`, protocol sha256 `{result['protocol_sha256']}`).",
        "",
        "Funding dependency #1731 and the On Balance Volume evaluator from #1657 are present at the baseline. "
        "This study reuses the manifest funding attachment and adds no funding option or funding accounting. "
        "No Binance US proxy leg and no cache-based `--funding` run is part of this study.",
        "",
        "## Inputs",
        "",
        f"- Manifest `{m['path']}` sha256 `{m['sha256']}`",
        f"- Provenance: {m['provenance']['kind']}: {m['provenance']['label']}",
        f"- Data reuse: {m['acquisition'].get('reused_from', '-')}",
        f"- Interval {m['interval']}, warm-up {m['warmup_bars']} bars (not scored; the largest candidate support "
        f"in the grid is {result['coverage_gate']['max_candidate_support_bars']} bars)",
    ]
    for name, w in sorted(m["windows"].items(), key=lambda kv: kv[1]["start"]):
        lines.append(f"- Window `{name}` [{w['start']}, {w['end']}): {w['role']}")
    c = m["costs"]
    lines.append(
        f"- Every arm uses the manifest execution specification: taker {c['taker_fee_pct']*100:.3f}%, maker "
        f"{c['maker_fee_pct']*100:.3f}%, slippage {c['slippage_bps']} bps per side plus the per-coin half spread, "
        f"szDecimals lot flooring, minimum order ${c['min_notional_usd']:.0f} with a "
        f"{c['min_notional_margin']*100:.0f}% margin, funding booked hourly from the manifest funding files. The "
        "half spread comes from one stored order-book snapshot, so it is an estimate of the historical spread.")
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
              "Zero-range rows | Funding hours present/expected | Accrual slots missing | Complete |",
              "|---|---|---|---|---|---|---|---|---|"]
    for r in gate["windows"]:
        cc = r.get("candle_coverage") or {}
        fc = r.get("funding_coverage") or {}
        ac = r.get("accrual_span") or {}
        lines.append(f"| {r['dataset']} | {r['window']} | {cc.get('present_bars', '-')}/{cc.get('expected_bars', '-')} | "
                     f"{cc.get('warmup_missing_bars', '-')} | {r.get('zero_volume_rows', '-')} | "
                     f"{r.get('zero_range_rows', '-')} | {fc.get('present_hours', '-')}/{fc.get('expected_hours', '-')} | "
                     f"{ac.get('missing_slots', '-')} | {_fmt(r['complete'])} |")
    for r in gate["refused"]:
        lines.append(f"- REFUSED {r['dataset']} {r['window']}: {'; '.join(r['problems'])}")
    close_ref = seed["close_strategies"][0]
    lines += [
        "",
        "## Protocol",
        "",
        f"- Close owner `{close_ref['name']}` with params `{json.dumps(close_ref['params'], sort_keys=True)}`, "
        f"comparison mode `{seed['comparison_mode']}`, intrabar resolution `{seed['intrabar_resolution']}`, stop "
        f"owner fixed entry ATR x{seed['stop_loss_atr_mult']}, capital ${seed['capital']:.0f} per dataset; signals "
        "on bar N fill at the bar N+1 open.",
        "- Arms: " + ", ".join(f"`{n}` (`{seed['arms'][n]['strategy']}`, {seed['arms'][n]['direction']}, "
                               f"{seed['arms'][n]['selection']})" for n in ARMS) + ".",
        "- Comparisons: " + "; ".join(f"`{a}` against " + ", ".join(f"`{c}`" for c in comps)
                                      for a, comps in COMPARISONS.items()) + ".",
        f"- Selection on `{seed['selection_window']}` only: {seed['selection']['eligible']}; clusters: "
        f"{seed['selection']['independent_positions']}; order {'; '.join(seed['selection']['order'])}. Each "
        "ablation inherits its candidate arm's selected parameters and changes only `divergence_test` to false.",
        f"- Held-out `{seed['held_out_window']}`: at least {MIN_TEST_POSITIONS} independent position clusters for "
        "each primary candidate arm; the bidirectional arm needs long and short positions.",
        f"- Cost runs: base x{BASE_COST:g} ({seed['cost_runs']['base']['meaning']}); stress x{STRESS_COST:g} "
        f"({seed['cost_runs']['stress']['meaning']}).",
        f"- Pass rule: {seed['verdict']['pass']}.",
        "- `volume_weighted` registers long-only entries, so its comparison uses the matched long-only arm. A "
        "comparator long-exit signal is never treated as a short entry.",
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
        if a["selection_kind"] == "grid":
            row = next((r for r in a["train_sweep"] if r["label"] == sel.get("selected")), None)
            summ = row["summary"] if row else None
            combos = f"{sel['eligible_combos']}/{len(a['train_sweep'])}"
        else:
            summ = a.get("train")
            combos = "inherited"
        plateau = a.get("selected_plateau") or {}
        lines.append(f"| `{arm}` | {a['direction']} | {combos} | "
                     f"{('`' + json.dumps(sel['params'], sort_keys=True) + '`') if sel['params'] else 'none'} | "
                     f"{_fmt(summ['mean_sharpe'], 3) if summ and summ.get('datasets') else '-'} | "
                     f"{summ['independent_positions'] if summ else '-'} | "
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
    for arm in ARMS:
        sel_c = result["held_out"][arm]["selected"]
        if sel_c is None or not sel_c["base"].get("datasets"):
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
              ") per dataset at registry defaults under the common contract `" +
              json.dumps(result["incumbent_contract"], sort_keys=True) + "` with attached funding. The contract is "
              "long-only, so it matches the long-only comparison; the bidirectional arm has no matched incumbent.", "",
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
        lines += ["", "A coverage, sample, reference or parity gate stopped the verdict before the performance gates. "
                  "The same performance checks, descriptive only and not a verdict input:", ""]
        for k, ok in sorted(v["performance_checks_descriptive"].items()):
            lines.append(f"- {k}: {_fmt(ok)}")
    cr = result["continuous_reference"]
    tot = cr["totals"]
    lines += [
        "",
        "## Finite replay versus continuous history",
        "",
        f"Parameters `{json.dumps(cr['params'], sort_keys=True)}` (the selected `klinger_both` parameters). The "
        "continuous reference runs the same recurrence once over the whole manifest frame from its first row and "
        "applies the same pivot, pairing, divergence, cross, expiry and consumption rules. The finite evaluator "
        "restarts trend, cm, both EMAs and the signal line `replay_bars` back for every endpoint.",
        "",
        f"- Scored rows {tot['scored_rows']}; finite valid rows {tot['finite_valid_rows']}; finite zero_cm holds "
        f"{tot['finite_zero_cm_rows']}.",
        f"- Entries: finite {tot['finite_entries']}, continuous {tot['continuous_entries']}, both on the same bar "
        f"{tot['entries_both']}, same side {tot['same_side_entries']}.",
        f"- Decision mismatches {tot['decision_mismatches']} (share {_fmt(tot['mismatch_share_of_scored_rows'], 4)}): "
        f"finite_invalid_hold {tot['finite_invalid_hold']}, continuous_invalid {tot['continuous_invalid']}, "
        f"seed_divergence {tot['seed_divergence']}, unclassified {tot['unclassified']}.",
        "- The cm accumulation restarts at the replay seed, and the volume-force factor depends on dm / cm, so the "
        "finite oscillator differs from the continuous one even far from the seed; no equivalence with any "
        "platform's built-in Klinger implementation is claimed.",
        "",
        "| Dataset | Window | Scored | Finite valid | Finite entries | Continuous entries | Same side | "
        "Mismatches | Seed divergence | Unclassified | Median abs osc diff | p95 rel osc diff | Sign agreement |",
        "|---|---|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for r in cr["datasets"]:
        lines.append(f"| {r['dataset']} | {r['window']} | {r['scored_rows']} | {r['finite_valid_rows']} | "
                     f"{r['finite_entries']} | {r['continuous_entries']} | {r['same_side_entries']} | "
                     f"{r['decision_mismatches']} | {r['seed_divergence']} | {r['unclassified']} | "
                     f"{_fmt(r['oscillator_abs_diff_median'], 3)} | {_fmt(r['oscillator_rel_diff_p95'], 4)} | "
                     f"{_fmt(r['oscillator_sign_agreement'], 4)} |")
    st = result["parameter_stability"]
    lines += ["", "## Parameter stability and unsuccessful variants", ""]
    for arm in CANDIDATE_ARMS:
        s = st[arm]
        lines.append(f"- `{arm}`: training-eligible variants {s['train_eligible_variants']}/{s['variants']}; "
                     f"Spearman train vs held-out mean Sharpe {_fmt(s['spearman_train_vs_test_sharpe'], 3)}; "
                     f"held-out net-positive variants {s['test_net_positive_variants']}/{s['variants']}")
    lines += [
        "",
        "Every candidate grid variant scored on the held-out window (descriptive; selection used train only).",
        "",
        "| Arm | Variant | Train eligible | Train Sharpe | Train independent | Held-out Sharpe | Held-out ret % | "
        "Held-out worst DD % | Held-out net $ | Held-out positions | Held-out independent |",
        "|---|---|---|---|---|---|---|---|---|---|---|",
    ]
    for row in result["candidate_variants_held_out"]:
        lines.append(f"| `{row['arm']}` | {row['label']} | {_fmt(row['train_eligible'])} | "
                     f"{_fmt(row['train_mean_sharpe'], 3)} | {row['train_independent_positions']} | "
                     f"{_fmt(row['test_mean_sharpe'], 3)} | {_fmt(row['test_mean_return_pct'])} | "
                     f"{_fmt(row['test_worst_max_dd_pct'])} | {_fmt(row['test_net_pnl_usd'])} | "
                     f"{row['test_positions']} | {row['test_independent_positions']} |")
    lines += ["", "## Per-entry audit (held-out, selected, base cost)", "",
              "Each row joins a simulated position to its decision bar in the evaluator output. Every fill is at "
              "the open of the bar after its decision bar.", ""]
    for arm in (a for a in ARMS if a in result["entry_audit"]):
        rows = result["entry_audit"][arm]
        late = sum(1 for r in rows if not r["fill_after_decision_bar"])
        mismatched = sum(1 for r in rows if r["decision_signal"] != (1 if r["side"] == "long" else -1))
        lines += [f"### `{arm}`: {len(rows)} positions; fills not after the decision bar {late}; decision signal "
                  f"not matching the side {mismatched}", "",
                  "| Dataset | Side | Decision bar | Reason | Osc | Signal line | Pivot 1 price | Pivot 1 osc | "
                  "Pivot 2 price | Pivot 2 osc | Net $ | Exit |",
                  "|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for r in rows:
            s = "long" if r["side"] == "long" else "short"
            lines.append(f"| {r['dataset']} | {r['side']} | {r['decision_bar']} | {r['decision_reason']} | "
                         f"{_fmt(r['klinger_oscillator'], 1)} | {_fmt(r['klinger_signal_line'], 1)} | "
                         f"{_fmt(r[f'{s}_pivot1_price'], 4)} | {_fmt(r[f'{s}_pivot1_oscillator'], 1)} | "
                         f"{_fmt(r[f'{s}_pivot2_price'], 4)} | {_fmt(r[f'{s}_pivot2_oscillator'], 1)} | "
                         f"{_fmt(r['net_pnl'])} | {', '.join(sorted(set(x.split(':')[0] for x in r['exit_reasons'])))} |")
        lines.append("")
    cv = result.get("close_validation") or {}
    lines += ["## Close validation", "",
              f"- Modes `{', '.join(cv.get('modes') or [str(cv.get('mode'))])}`, eligibility "
              f"`{cv.get('close_eligibility')}`, parity status `{cv.get('parity_status')}`, incomplete parity "
              f"{_fmt(cv.get('incomplete_parity'))}.",
              "- `unverified` is the strict, eligible capability status (no approximation and no refusal). It is "
              "not a ledger-verified claim that these exits equal live exits."]
    par = result["parity"]
    lines += ["", "## Parity", "",
              f"`{par['path']}` sha256 `{par['sha256'][:16]}`, clean {_fmt(par['clean'])} (decision "
              f"{_fmt(par['decision_clean'])}, indicator {_fmt(par['indicator_clean'])}).",
              f"- Decision parity: the last {PARITY_BARS} held-out bars of every dataset, the selected `klinger_both` "
              "parameters and the frozen close owner, direction both, with a 200-row live window and with every "
              "prefix; the composer mode compares full-frame decisions with the per-bar live composer and the "
              "batched paper mode also compares solo and batched explicit-paper `check_hyperliquid.py` slots.",
              f"- Indicator parity: for every scored row of both windows, the full-frame row equals the row computed "
              f"from its own prefix, its trailing 200 rows and its exact support span (oscillator values within "
              f"{INDICATOR_TOLERANCE:g} relative tolerance; signal, reason, validity, setup states and pivot "
              "timestamps exactly).",
              "- Admission and timed closed-bar behavior are proved by `shared_scripts/test_klinger_divergence_checks.py`."]
    lines += ["", "## Full-precision evidence", "",
              "Every input row and every evaluator value of the selected candidate parameters, written with `repr` "
              "floats.", "",
              "| Arm | Dataset | Window | File | sha256 | Rows | Scored rows | Scored entry signals | Scored reasons |",
              "|---|---|---|---|---|---|---|---|---|"]
    for e in result["evidence"]:
        reasons = ", ".join(f"{k} {v}" for k, v in e["scored_reason_counts"].items())
        lines.append(f"| `{e['arm']}` | {e['dataset']} | {e['window']} | `{e['path']}` | `{e['sha256'][:16]}` | "
                     f"{e['rows']} | {e['scored_rows']} | {e['entry_signals_scored']} | {reasons} |")
    lines += ["", "## Source hashes", ""]
    for path, digest in sorted(result["source_sha256"].items()):
        lines.append(f"- `{path}` `{digest}`")
    lines += ["", "---", "Created with LLM: Opus 5.5 | high | Harness: Claude Code", ""]
    return "\n".join(lines)


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1662 frozen held-out study driver (suggest-only)")
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
