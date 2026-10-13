#!/usr/bin/env python3

import argparse
import json
import math
import os
import sys

import numpy as np
import pandas as pd

_HERE = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_HERE, "..", ".."))
_REPO = os.path.abspath(os.path.join(_BACKTEST, ".."))
sys.path.insert(0, _BACKTEST)
sys.path.insert(0, os.path.join(_REPO, "shared_tools"))

import offline_manifest as om
from closed_bar import TIMESTAMP_KIND_OPEN, declare_frame_cadence
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame
from parity_diff import summarize as parity_summarize

NAME = "elder_force_pullback"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "elder_force_pullback_1668_manifest.json")
SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
DATASETS = ("BTC 4h", "ETH 4h", "SOL 4h")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
LIVE_WINDOW = 200
TAIL_SIZES = (199, 200)
COMPARE_COLUMNS = (
    "efp_raw_force", "efp_seed_force", "efp_force", "efp_prev_force", "efp_trend_sma", "efp_trend_sma_lag",
    "efp_prev_trend_sma", "efp_prev_trend_sma_lag", "efp_uptrend", "efp_prev_uptrend", "efp_downtrend",
    "efp_prev_downtrend", "efp_cross", "efp_volume_eligible", "efp_valid", "efp_reason", "efp_reason_code",
    "efp_eval_ts_ms", "signal",
)
MODES = ("entry", "composed")
BOUNDARY_SPAN_STRIDE = 3


def load_seed() -> dict:
    with open(SEED_PATH) as fh:
        return json.load(fh)


SEED = load_seed()
DEFAULTS = dict(SEED["params"])
CLOSE_REFS = SEED["close_strategies"]
TOL = SEED["numerical"]["reference_tolerance"]


def need(params: dict) -> int:
    return max(params["recursion_length"] + 2, params["trend_period"] + params["slope_span"] + 1)


def domain_endpoints() -> list:
    out = []
    for f in (2, 19):
        for length in (10 * f, 197):
            for trend, span in ((2, 1), (2, 196), (197, 1), (100, 50)):
                for vw in (1, 0):
                    out.append({"force_period": f, "trend_period": trend, "slope_span": span,
                                "recursion_length": length, "volume_weighting": vw})
    return out


def grid_sets() -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    grid = [p for _, p in expand_sweep(dict(DEFAULTS), specs)]
    return grid + [dict(p, volume_weighting=0) for p in grid]


def runner_sets() -> list:
    picks = [dict(DEFAULTS), dict(DEFAULTS, volume_weighting=0)]
    for f, trend, span, length in ((8, 30, 3, 150), (8, 80, 10, 197), (13, 80, 10, 180), (13, 30, 3, 197)):
        base = {"force_period": f, "trend_period": trend, "slope_span": span, "recursion_length": length}
        picks += [dict(base, volume_weighting=1), dict(base, volume_weighting=0)]
    picks += [p for p in domain_endpoints() if p["trend_period"] in (2, 197)][:4]
    return picks


def _declare(df: pd.DataFrame, interval: str = "4h") -> pd.DataFrame:
    out = df.copy()
    declare_frame_cadence(out, interval, TIMESTAMP_KIND_OPEN)
    return out


def frozen_frame(manifest: dict, key: str, bars: int) -> pd.DataFrame:
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return _declare(candles[INPUT_COLUMNS].iloc[-bars:])


def _frame(close, volume, spread=0.5) -> pd.DataFrame:
    close = np.asarray(close, dtype=float)
    open_ = np.concatenate(([close[0]], close[:-1]))
    df = pd.DataFrame({"open": open_, "high": np.maximum(open_, close) + spread,
                       "low": np.minimum(open_, close) - spread, "close": close,
                       "volume": np.asarray(volume, dtype=float)})
    df.index = pd.date_range("2026-01-01", periods=len(close), freq="4h")
    return _declare(df)


def boundary_frames() -> dict:
    n = 420
    t = np.arange(n, dtype=float)
    rs = np.random.default_rng(1668)
    vol = rs.uniform(50.0, 500.0, n)
    rising = 100.0 + 0.4 * t + 6.0 * np.sin(t / 7.0)
    falling = 400.0 - 0.4 * t - 6.0 * np.sin(t / 7.0)
    flat = np.full(n, 250.0)
    zero_vol = vol.copy()
    zero_vol[::9] = 0.0
    defects = _frame(rising, vol)
    defects.iloc[230, defects.columns.get_loc("close")] = np.nan
    defects.iloc[300, defects.columns.get_loc("volume")] = -1.0
    gapped = _frame(rising, vol)
    gapped.index = pd.DatetimeIndex(list(gapped.index[:260]) + list(gapped.index[260:] + pd.Timedelta(hours=4)))
    walk = 300.0 + np.cumsum(rs.normal(0.0, 2.0, n)) + 0.3 * t * np.sign(np.sin(t / 60.0))
    return {
        "rising_pullbacks": _frame(rising, vol),
        "falling_rallies": _frame(falling, vol),
        "constant_closes": _frame(flat, vol),
        "zero_volume_every_ninth": _frame(rising, zero_vol),
        "defect_rows_and_recovery": defects,
        "cadence_gap_and_recovery": gapped,
        "random_walk_both_sides": _frame(np.maximum(walk, 5.0), vol),
    }


def reference(df: pd.DataFrame, params: dict) -> list:
    close = [float(x) for x in df["close"]]
    volume = [float(x) for x in df["volume"]]
    n = len(close)
    f_p, trend, span, length = (params["force_period"], params["trend_period"], params["slope_span"],
                                params["recursion_length"])
    weighted = params["volume_weighting"] == 1
    alpha = 2.0 / (f_p + 1.0)
    raw = [math.nan] + [(close[i] - close[i - 1]) * (volume[i] if weighted else 1.0) for i in range(1, n)]
    force = [math.nan] * n
    for t in range(length, n):
        e = raw[t - length + 1]
        for k in range(t - length + 2, t + 1):
            e = alpha * raw[k] + (1.0 - alpha) * e
        force[t] = e
    sma = [math.nan] * n
    for t in range(trend - 1, n):
        total = close[t - trend + 1]
        for j in range(t - trend + 2, t + 1):
            total = total + close[j]
        sma[t] = total / float(trend)
    req = need(params)
    rows = []
    for t in range(n):
        valid = t >= req - 1
        sig = 0
        if valid:
            up = all(close[x] > sma[x] and sma[x] > sma[x - span] for x in (t - 1, t))
            down = all(close[x] < sma[x] and sma[x] < sma[x - span] for x in (t - 1, t))
            vol_ok = volume[t] > 0 and volume[t - 1] > 0
            if force[t - 1] < 0 < force[t] and up and vol_ok:
                sig = 1
            elif force[t - 1] > 0 > force[t] and down and vol_ok:
                sig = -1
        rows.append({"valid": valid, "signal": sig, "force": force[t], "sma": sma[t]})
    return rows


def _close(a: float, b: float) -> bool:
    return abs(a - b) <= TOL["abs"] + TOL["rel"] * abs(b)


def reference_check(apply, df: pd.DataFrame, params: dict) -> dict:
    out = apply(df, params)
    ref = reference(df, params)
    mismatches = 0
    first = None
    for i, want in enumerate(ref):
        rec = out.iloc[i]
        ok = (rec["efp_valid"] == 1.0) == want["valid"] and int(rec["signal"]) == want["signal"]
        if ok and want["valid"]:
            ok = _close(float(rec["efp_force"]), want["force"]) and _close(float(rec["efp_trend_sma"]), want["sma"])
        if not ok:
            mismatches += 1
            if first is None:
                first = {"row": i, "want": want, "reason": str(rec["efp_reason"])}
    res = {"reference_rows": len(ref), "reference_mismatches": mismatches}
    if first is not None:
        res["first_reference_mismatch"] = first
    return res


def _rows_equal(a: pd.Series, b: pd.Series) -> bool:
    for col in COMPARE_COLUMNS:
        x, y = a[col], b[col]
        if isinstance(x, str) or isinstance(y, str):
            if x != y:
                return False
        else:
            fx, fy = float(x), float(y)
            if not ((math.isnan(fx) and math.isnan(fy)) or fx == fy):
                return False
    return True


def _part(df: pd.DataFrame, lo: int, hi: int) -> pd.DataFrame:
    part = df.iloc[lo:hi].copy()
    part.attrs = dict(df.attrs)
    return part


def independent_check(apply, df: pd.DataFrame, params: dict, ends, prefix_ends=None) -> dict:
    full = apply(df, params)
    span = need(params)
    valid = full["efp_valid"].to_numpy() == 1.0
    compared = mismatches = 0
    first = None
    for end in sorted(set(ends)):
        want = full.iloc[end - 1]
        parts = []
        if prefix_ends is None or end in prefix_ends:
            parts.append(("prefix", _part(df, 0, end)))
        if valid[end - 1]:
            for size in (span,) + TAIL_SIZES:
                if end >= size:
                    parts.append((f"tail{size}", _part(df, end - size, end)))
        for label, part in parts:
            compared += 1
            got = apply(part, params).iloc[-1]
            if not _rows_equal(got, want):
                mismatches += 1
                if first is None:
                    first = {"end": int(end), "frame": label,
                             "want": {c: str(want[c]) for c in COMPARE_COLUMNS},
                             "got": {c: str(got[c]) for c in COMPARE_COLUMNS}}
    reasons = full["efp_reason"].to_numpy()
    out = {"comparisons": compared, "mismatches": mismatches, "valid_bars": int(valid.sum()),
           "entry_long": int((full["signal"].to_numpy() == 1).sum()),
           "entry_short": int((full["signal"].to_numpy() == -1).sum()),
           "reasons": {k: int((reasons == k).sum()) for k in sorted(set(reasons))}}
    if first is not None:
        out["first_mismatch"] = first
    return out


def _merge_reasons(total: dict, add: dict) -> None:
    for k, v in add.items():
        total[k] = total.get(k, 0) + v


def _frozen_unit(apply, manifest: dict, key: str, bars: int) -> list:
    cells = []
    df = frozen_frame(manifest, key, bars)
    for params in grid_sets() + domain_endpoints():
        ends = range(1, len(df) + 1)
        cell = independent_check(apply, df, params, ends)
        cell.update(reference_check(apply, df, params))
        cells.append(dict(cell, dataset=key, params=params))
    return cells


def _boundary_unit(apply, label: str) -> dict:
    df = boundary_frames()[label]
    totals = {"frame": label, "combinations": 0, "comparisons": 0, "mismatches": 0, "reference_mismatches": 0,
              "entry_long": 0, "entry_short": 0, "reasons": {}}
    first = None
    for params in [dict(DEFAULTS), dict(DEFAULTS, volume_weighting=0)] + domain_endpoints():
        span = need(params)
        ends = list(range(1, len(df) + 1, BOUNDARY_SPAN_STRIDE)) + [len(df)]
        prefix_ends = {span - 1, span, (span + len(df)) // 2, len(df)}
        cell = independent_check(apply, df, params, ends, prefix_ends=prefix_ends)
        if label in ("rising_pullbacks", "falling_rallies", "constant_closes", "zero_volume_every_ninth",
                     "random_walk_both_sides"):
            totals["reference_mismatches"] += reference_check(apply, df, params)["reference_mismatches"]
        totals["combinations"] += 1
        for k in ("comparisons", "mismatches", "entry_long", "entry_short"):
            totals[k] += cell[k]
        _merge_reasons(totals["reasons"], cell["reasons"])
        if first is None and cell.get("first_mismatch"):
            first = dict(cell["first_mismatch"], params=params)
    if first is not None:
        totals["first_mismatch"] = first
    return totals


def parity_config(params: dict, mode: str, coin: str, batched: bool) -> ParityConfig:
    kw = dict(strategy_name=NAME, params=dict(params), registry="futures", platform="hyperliquid",
              symbol=coin, timeframe="4h", batched=batched)
    if mode == "composed":
        kw.update(close_refs=json.loads(json.dumps(CLOSE_REFS)), direction="both",
                  stop_kwargs={"stop_loss_atr_mult": SEED["stop_loss_atr_mult"]}, comparison_mode="strict")
    return ParityConfig(**kw)


def runner_cell(frame) -> dict:
    s = parity_summarize(frame)
    closing = frame[frame["live_close_fraction"] > 0] if not frame.empty else frame
    out = {
        "bars_compared": s["bars_compared"],
        "mismatches": s["mismatches"],
        "decision_agreement": s["decision_agreement"],
        "close_parity": s["close_parity"],
        "clean": s["clean"],
        "entry_long": int((frame["live_open_action"] == "long").sum()) if not frame.empty else 0,
        "entry_short": int((frame["live_open_action"] == "short").sum()) if not frame.empty else 0,
        "long_context_closes": int((closing["live_signal"] == -1).sum()) if not frame.empty else 0,
        "short_context_closes": int((closing["live_signal"] == 1).sum()) if not frame.empty else 0,
    }
    if "batch_mismatches" in s:
        out["batch_mismatches"] = s["batch_mismatches"]
        out["batch_clean"] = s.get("batch_clean")
    if not s["decision_agreement"]:
        out["first_mismatch"] = s.get("first_mismatch")
    return out


def _runner_unit(manifest: dict, key: str, bars: int, batched: bool) -> list:
    cells = []
    coin = key.split()[0]
    df = frozen_frame(manifest, key, bars)
    for params in runner_sets():
        for mode in MODES:
            for window in (LIVE_WINDOW, None):
                frame = compute_parity_frame(df, cfg=parity_config(params, mode, coin, batched),
                                             window=window, stride=1)
                cells.append(dict(runner_cell(frame), dataset=key, params=params, mode=mode,
                                  window=window if window is not None else 0))
    return cells


_WORKER = {}


def _unit(kind: str, key: str, bars: int, batched: bool):
    if not _WORKER:
        from registry_loader import load_registry
        reg = load_registry("futures")
        _WORKER["manifest"] = om.load_manifest(MANIFEST)
        _WORKER["apply"] = lambda df, params: reg.apply_strategy(NAME, df, params)
    if kind == "frozen":
        out = _frozen_unit(_WORKER["apply"], _WORKER["manifest"], key, bars)
    elif kind == "boundary":
        out = _boundary_unit(_WORKER["apply"], key)
    else:
        out = _runner_unit(_WORKER["manifest"], key, bars, batched)
    print(f"{kind} {key}: done", file=sys.stderr, flush=True)
    return out


def run_units(bars: int, batched: bool, workers: int) -> dict:
    from concurrent.futures import ProcessPoolExecutor
    units = ([("frozen", k) for k in DATASETS] + [("boundary", k) for k in boundary_frames()]
             + [("runner", k) for k in DATASETS])
    if workers <= 1:
        outs = [_unit(kind, key, bars, batched) for kind, key in units]
    else:
        with ProcessPoolExecutor(max_workers=workers) as pool:
            futures = [pool.submit(_unit, kind, key, bars, batched) for kind, key in units]
            outs = [f.result() for f in futures]
    frozen, boundary, runner = [], [], []
    for (kind, _), out in zip(units, outs):
        if kind == "frozen":
            frozen.extend(out)
        elif kind == "boundary":
            boundary.append(out)
        else:
            runner.extend(out)
    return {"independent": {"frozen_cells": frozen, "boundary_frames": boundary}, "runner": {"cells": runner}}


def problems_of(result: dict, batched: bool) -> list:
    problems = []
    ind = result["independent"]
    frozen = ind["frozen_cells"]
    if any(c["comparisons"] == 0 for c in frozen):
        problems.append("independent: a frozen cell compared no bars")
    bad = sum(c["mismatches"] for c in frozen) + sum(b["mismatches"] for b in ind["boundary_frames"])
    if bad:
        problems.append(f"independent: {bad} prefix or tail mismatches")
    ref_bad = sum(c["reference_mismatches"] for c in frozen) + sum(b["reference_mismatches"]
                                                                    for b in ind["boundary_frames"])
    if ref_bad:
        problems.append(f"reference: {ref_bad} rows differ from the independent scalar reference")
    if not (sum(c["entry_long"] for c in frozen) and sum(c["entry_short"] for c in frozen)):
        problems.append("independent: frozen frames lack long or short entry witnesses")
    reasons = {}
    for b in ind["boundary_frames"]:
        _merge_reasons(reasons, b["reasons"])
    for key in ("entry_long", "entry_short", "zero_force", "trend_unconfirmed", "volume_ineligible",
                "insufficient_history", "nonfinite_input", "negative_volume", "cadence_gap"):
        if not reasons.get(key):
            problems.append(f"independent: boundary frames have no {key} witness")
    for mode in MODES:
        for window in (LIVE_WINDOW, 0):
            rows = [c for c in result["runner"]["cells"] if c["mode"] == mode and c["window"] == window]
            label = f"runner {mode} window={window}"
            if not rows or any(c["bars_compared"] == 0 for c in rows):
                problems.append(f"{label}: a cell compared no bars")
            mism = sum(c["mismatches"] for c in rows)
            bmism = sum(c.get("batch_mismatches", 0) for c in rows)
            if mism or bmism:
                problems.append(f"{label}: {mism} decision and {bmism} batch mismatches")
            if not (sum(c["entry_long"] for c in rows) and sum(c["entry_short"] for c in rows)):
                problems.append(f"{label}: both entry directions not exercised")
            if mode == "composed" and not (sum(c["long_context_closes"] for c in rows)
                                           and sum(c["short_context_closes"] for c in rows)):
                problems.append(f"{label}: both position sides not exercised")
            if any(not c["clean"] for c in rows):
                problems.append(f"{label}: unclean cells")
            if batched and any(c.get("batch_clean") is False for c in rows):
                problems.append(f"{label}: batch not clean")
    return problems


def run(bars: int, batched: bool, workers: int = 1) -> dict:
    result = {
        "strategy": NAME,
        "registry": "futures",
        "batched": batched,
        "bars": bars,
        "datasets": list(DATASETS),
        "close_refs": CLOSE_REFS,
        "stop_loss_atr_mult": SEED["stop_loss_atr_mult"],
        "live_window": LIVE_WINDOW,
        "tail_sizes": ["required_history"] + list(TAIL_SIZES),
        "stride": 1,
        "boundary_span_stride": BOUNDARY_SPAN_STRIDE,
        "reference_tolerance": TOL,
        "manifest_sha256": om.sha256_file(MANIFEST),
        "seed_sha256": om.sha256_file(SEED_PATH),
        "source_sha256": {p: om.sha256_file(os.path.join(_REPO, p)) for p in (
            "shared_strategies/open/elder_force_pullback.py", "shared_strategies/open/indicators_core.py",
            "shared_strategies/open/registry.py", "shared_tools/strategy_composition.py",
            "backtest/parity_diff.py", "backtest/candidates/elder_force_pullback_1668/run_parity.py")},
        "parameter_sets": {"grid_and_endpoints": len(grid_sets()) + len(domain_endpoints()),
                           "runner": len(runner_sets())},
    }
    result.update(run_units(bars, batched, workers))
    result["problems"] = problems_of(result, batched)
    result["clean"] = not result["problems"]
    return result


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1668 offline decision parity on frozen Hyperliquid candles")
    p.add_argument("--batched", action="store_true",
                   help="Also compare the explicit-paper batch slot with the solo evaluator in the runner")
    p.add_argument("--bars", type=int, default=320)
    p.add_argument("--json", default=None, dest="json_out")
    p.add_argument("--workers", type=int, default=1, help="Process count; results do not depend on it")
    args = p.parse_args(argv)
    if args.bars < LIVE_WINDOW + 1:
        p.error(f"--bars must be at least {LIVE_WINDOW + 1}")
    result = run(args.bars, args.batched, args.workers)
    out = args.json_out or os.path.join(_HERE, "parity_results_batched.json" if args.batched
                                        else "parity_results.json")
    with open(out, "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    ind = result["independent"]
    frozen = ind["frozen_cells"]
    print(f"independent frozen: cells={len(frozen)} comparisons={sum(c['comparisons'] for c in frozen)} "
          f"mismatches={sum(c['mismatches'] for c in frozen)} "
          f"reference_mismatches={sum(c['reference_mismatches'] for c in frozen)} "
          f"entries long/short={sum(c['entry_long'] for c in frozen)}/{sum(c['entry_short'] for c in frozen)}")
    for b in ind["boundary_frames"]:
        print(f"boundary {b['frame']}: combinations={b['combinations']} comparisons={b['comparisons']} "
              f"mismatches={b['mismatches']} reference_mismatches={b['reference_mismatches']} "
              f"entries long/short={b['entry_long']}/{b['entry_short']} reasons={json.dumps(b['reasons'], sort_keys=True)}")
    for mode in MODES:
        for window in (LIVE_WINDOW, 0):
            rows = [c for c in result["runner"]["cells"] if c["mode"] == mode and c["window"] == window]
            print(f"runner {mode} window={window}: cells={len(rows)} bars={sum(c['bars_compared'] for c in rows)} "
                  f"mismatches={sum(c['mismatches'] for c in rows)} "
                  f"batch_mismatches={sum(c.get('batch_mismatches', 0) for c in rows)} "
                  f"entries long/short={sum(c['entry_long'] for c in rows)}/{sum(c['entry_short'] for c in rows)} "
                  f"closes long/short ctx={sum(c['long_context_closes'] for c in rows)}/"
                  f"{sum(c['short_context_closes'] for c in rows)}")
    print(f"wrote {out}")
    print("clean" if result["clean"] else "NOT CLEAN: " + "; ".join(result["problems"]))
    return 0 if result["clean"] else 1


if __name__ == "__main__":
    sys.exit(main())
