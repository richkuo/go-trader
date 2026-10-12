#!/usr/bin/env python3

import argparse
import json
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
from eval_windows import expand_sweep
from optimizer import DEFAULT_PARAM_RANGES
from parity_diff import ParityConfig, compute_parity_frame
from parity_diff import summarize as parity_summarize

NAME = "elder_ray_pullback"
MANIFEST = os.path.join(_REPO, "backtest", "candidates", "elder_ray_pullback_1648_manifest.json")
SEED_PATH = os.path.join(_HERE, "candidate_seed.json")
DATASETS = ("BTC 4h", "ETH 4h", "SOL 4h")
INPUT_COLUMNS = ["open", "high", "low", "close", "volume"]
HORIZON = 64
LIVE_WINDOW = 200
COMPARE_COLUMNS = (
    "erp_ema", "erp_ema_prior", "erp_slope", "erp_bull_power", "erp_bear_power", "erp_setup_side",
    "erp_setup_age", "erp_ema_valid", "erp_slope_valid", "erp_valid", "erp_reason", "erp_reason_code",
    "erp_eval_ts_ms", "signal",
)
MODES = ("entry", "composed")
BOUNDARY_SPAN_STRIDE = 4


def load_seed() -> dict:
    with open(SEED_PATH) as fh:
        return json.load(fh)


SEED = load_seed()
DEFAULTS = dict(SEED["params"])
DOMAINS = SEED["parameter_domains"]
CLOSE_REFS = SEED["close_strategies"]


def need(params: dict) -> int:
    return HORIZON + params["slope_span"] + params["pullback_window"] + 1


def domain_endpoints() -> list:
    out = []
    for e in (DOMAINS["ema_period"]["min"], DOMAINS["ema_period"]["max"]):
        for s in (DOMAINS["slope_span"]["min"], DOMAINS["slope_span"]["max"]):
            for w in (DOMAINS["pullback_window"]["min"], DOMAINS["pullback_window"]["max"]):
                out.append({"ema_period": e, "slope_span": s, "pullback_window": w})
    return out


def grid_and_endpoints() -> list:
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(dict(DEFAULTS), specs)] + domain_endpoints()


def every_legal_combination() -> list:
    return [{"ema_period": e, "slope_span": s, "pullback_window": w}
            for e in range(DOMAINS["ema_period"]["min"], DOMAINS["ema_period"]["max"] + 1)
            for s in range(DOMAINS["slope_span"]["min"], DOMAINS["slope_span"]["max"] + 1)
            for w in range(DOMAINS["pullback_window"]["min"], DOMAINS["pullback_window"]["max"] + 1)]


def frozen_frame(manifest: dict, key: str, bars: int) -> pd.DataFrame:
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[INPUT_COLUMNS].iloc[-bars:]


def _frame(close, low, high) -> pd.DataFrame:
    close = np.asarray(close, dtype=float)
    df = pd.DataFrame({"open": close, "high": np.asarray(high, dtype=float), "low": np.asarray(low, dtype=float),
                       "close": close, "volume": np.full(len(close), 10.0)})
    df.index = pd.date_range("2026-01-01", periods=len(close), freq="4h")
    return df


def boundary_frames() -> dict:
    n = 300
    up = 100.0 + np.arange(n, dtype=float)
    up_low = up - 1.0
    for start, length in ((110, 1), (125, 4), (140, 8), (160, 16), (185, 17), (215, 30), (260, 3)):
        up_low[start:start + length] = up[start:start + length] - 12.0
    down = 1000.0 - np.arange(n, dtype=float)
    down_high = down + 1.0
    for start, length in ((110, 2), (125, 5), (140, 9), (160, 16), (185, 18), (215, 25), (260, 1)):
        down_high[start:start + length] = down[start:start + length] + 12.0
    step = np.array([100.0] * 100 + [200.0] * (n - 100))
    zigzag = 300.0 + 40.0 * np.sin(np.arange(n) / 9.0) + 0.3 * np.arange(n)
    zz_low = zigzag - 2.0 - 6.0 * (np.arange(n) % 5 == 0)
    zz_high = zigzag + 2.0 + 6.0 * (np.arange(n) % 7 == 0)
    return {
        "rising_pullbacks_through_expiry": _frame(up, up_low, up + 1.0),
        "falling_rallies_through_expiry": _frame(down, down - 1.0, down_high),
        "step_to_constant_zero_slope": _frame(step, step - 1e-9, step + 1.0),
        "oscillating_both_sides": _frame(zigzag, zz_low, zz_high),
    }


def _rows_equal(a: pd.Series, b: pd.Series) -> bool:
    for col in COMPARE_COLUMNS:
        x, y = a[col], b[col]
        if isinstance(x, str) or isinstance(y, str):
            if x != y:
                return False
        elif not (float(x) == float(y)):
            return False
    return True


def independent_check(apply, df: pd.DataFrame, params: dict, ends, frames=("prefix", "span", "window"),
                      prefix_ends=None) -> dict:
    full = apply(df, params)
    span = need(params)
    compared = mismatches = 0
    first = None
    for end in sorted(set(ends) | set(prefix_ends or ())):
        want = full.iloc[end - 1]
        parts = []
        if "prefix" in frames or (prefix_ends is not None and end in prefix_ends):
            parts.append(("prefix", df.iloc[:end]))
        if "span" in frames and end >= span:
            parts.append(("span", df.iloc[end - span:end]))
        if "window" in frames and end >= LIVE_WINDOW:
            parts.append(("window", df.iloc[end - LIVE_WINDOW:end]))
        for label, part in parts:
            compared += 1
            got = apply(part, params).iloc[-1]
            if not _rows_equal(got, want):
                mismatches += 1
                if first is None:
                    first = {"end": int(end), "frame": label,
                             "want": {c: str(want[c]) for c in COMPARE_COLUMNS},
                             "got": {c: str(got[c]) for c in COMPARE_COLUMNS}}
    valid = full["erp_valid"].to_numpy() == 1.0
    reasons = full["erp_reason"].to_numpy()
    out = {
        "comparisons": compared,
        "mismatches": mismatches,
        "valid_bars": int(valid.sum()),
        "entry_long": int((full["signal"].to_numpy() == 1).sum()),
        "entry_short": int((full["signal"].to_numpy() == -1).sum()),
        "setup_expired": int((reasons == "setup_expired").sum()),
        "trend_cleared": int((reasons == "trend_cleared").sum()),
        "zero_power_recoveries": int(((full["signal"].to_numpy() == 1) & (full["erp_bear_power"].to_numpy() == 0.0)).sum()
                                     + ((full["signal"].to_numpy() == -1) & (full["erp_bull_power"].to_numpy() == 0.0)).sum()),
        "zero_slope_clears": int(((reasons == "trend_cleared") & (full["erp_slope"].to_numpy() == 0.0)).sum()),
    }
    if first is not None:
        out["first_mismatch"] = first
    return out


def run_independent(manifest: dict, apply, bars: int) -> dict:
    cells = []
    for key in DATASETS:
        df = frozen_frame(manifest, key, bars)
        for params in grid_and_endpoints():
            ends = range(need(params), len(df) + 1)
            cells.append(dict(independent_check(apply, df, params, ends), dataset=key, params=params))
    boundary = []
    frames = boundary_frames()
    combos = every_legal_combination()
    for label, df in frames.items():
        totals = {"frame": label, "combinations": 0, "comparisons": 0, "mismatches": 0, "entry_long": 0,
                  "entry_short": 0, "setup_expired": 0, "trend_cleared": 0, "zero_slope_clears": 0}
        first = None
        for params in combos:
            span = need(params)
            ends = list(range(span, len(df) + 1, BOUNDARY_SPAN_STRIDE)) + [len(df)]
            prefix_ends = {span, (span + len(df)) // 2, len(df)}
            cell = independent_check(apply, df, params, ends, frames=("span",), prefix_ends=prefix_ends)
            totals["combinations"] += 1
            for k in ("comparisons", "mismatches", "entry_long", "entry_short", "setup_expired", "trend_cleared",
                      "zero_slope_clears"):
                totals[k] += cell[k]
            if first is None and cell.get("first_mismatch"):
                first = dict(cell["first_mismatch"], params=params)
        if first is not None:
            totals["first_mismatch"] = first
        boundary.append(totals)
    return {"frozen_cells": cells, "boundary_frames": boundary}


def parity_config(params: dict, mode: str, coin: str, batched: bool) -> ParityConfig:
    kw = dict(strategy_name=NAME, params=dict(params), registry="futures", platform="hyperliquid",
              symbol=coin, timeframe="4h", batched=batched)
    if mode == "composed":
        kw.update(close_refs=json.loads(json.dumps(CLOSE_REFS)), direction="both")
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


def run_runner(manifest: dict, bars: int, batched: bool) -> dict:
    cells = []
    for key in DATASETS:
        coin = key.split()[0]
        df = frozen_frame(manifest, key, bars)
        for params in grid_and_endpoints():
            for mode in MODES:
                for window in (LIVE_WINDOW, None):
                    frame = compute_parity_frame(df, cfg=parity_config(params, mode, coin, batched),
                                                 window=window, stride=1)
                    cells.append(dict(runner_cell(frame), dataset=key, params=params, mode=mode,
                                      window=window if window is not None else 0))
    return {"cells": cells}


def problems_of(result: dict, batched: bool) -> list:
    problems = []
    ind = result["independent"]
    frozen = ind["frozen_cells"]
    if any(c["comparisons"] == 0 for c in frozen):
        problems.append("independent: a frozen cell compared no bars")
    bad = sum(c["mismatches"] for c in frozen) + sum(b["mismatches"] for b in ind["boundary_frames"])
    if bad:
        problems.append(f"independent: {bad} prefix, span or window mismatches")
    if not (sum(c["entry_long"] for c in frozen) and sum(c["entry_short"] for c in frozen)):
        problems.append("independent: frozen frames lack long or short entry witnesses")
    bnd = ind["boundary_frames"]
    for key in ("entry_long", "entry_short", "setup_expired", "trend_cleared", "zero_slope_clears"):
        if not sum(b[key] for b in bnd):
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


def run(bars: int, batched: bool) -> dict:
    from registry_loader import load_registry
    manifest = om.load_manifest(MANIFEST)
    reg = load_registry("futures")
    result = {
        "strategy": NAME,
        "registry": "futures",
        "batched": batched,
        "bars": bars,
        "datasets": list(DATASETS),
        "close_refs": CLOSE_REFS,
        "live_window": LIVE_WINDOW,
        "stride": 1,
        "boundary_span_stride": BOUNDARY_SPAN_STRIDE,
        "manifest_sha256": om.sha256_file(MANIFEST),
        "seed_sha256": om.sha256_file(SEED_PATH),
        "source_sha256": {p: om.sha256_file(os.path.join(_REPO, p)) for p in (
            "shared_strategies/open/elder_ray_pullback.py", "shared_strategies/open/registry.py",
            "shared_tools/strategy_composition.py", "backtest/parity_diff.py",
            "backtest/candidates/elder_ray_pullback_1648/run_parity.py")},
        "parameter_sets": {"grid_and_endpoints": len(grid_and_endpoints()),
                           "every_legal_combination": len(every_legal_combination())},
        "independent": run_independent(manifest, lambda df, params: reg.apply_strategy(NAME, df, params), bars),
        "runner": run_runner(manifest, bars, batched),
    }
    result["problems"] = problems_of(result, batched)
    result["clean"] = not result["problems"]
    return result


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1648 offline decision parity on frozen Hyperliquid candles")
    p.add_argument("--batched", action="store_true",
                   help="Also compare the explicit-paper batch slot with the solo evaluator in the runner")
    p.add_argument("--bars", type=int, default=300)
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)
    if args.bars < LIVE_WINDOW + 1:
        p.error(f"--bars must be at least {LIVE_WINDOW + 1}")
    result = run(args.bars, args.batched)
    out = args.json_out or os.path.join(_HERE, "parity_results_batched.json" if args.batched
                                        else "parity_results.json")
    with open(out, "w") as fh:
        json.dump(result, fh, indent=1, sort_keys=True, default=str)
        fh.write("\n")
    ind = result["independent"]
    print(f"independent frozen: cells={len(ind['frozen_cells'])} "
          f"comparisons={sum(c['comparisons'] for c in ind['frozen_cells'])} "
          f"mismatches={sum(c['mismatches'] for c in ind['frozen_cells'])} "
          f"entries long/short={sum(c['entry_long'] for c in ind['frozen_cells'])}/"
          f"{sum(c['entry_short'] for c in ind['frozen_cells'])}")
    for b in ind["boundary_frames"]:
        print(f"boundary {b['frame']}: combinations={b['combinations']} comparisons={b['comparisons']} "
              f"mismatches={b['mismatches']} entries long/short={b['entry_long']}/{b['entry_short']} "
              f"expired={b['setup_expired']} cleared={b['trend_cleared']} zero_slope_clears={b['zero_slope_clears']}")
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
