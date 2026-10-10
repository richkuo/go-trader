#!/usr/bin/env python3

import argparse
import json
import os
import sys

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

NAME = "money_flow_index_reversal"
MANIFEST = os.path.join(_HERE, "study_manifest.json")
CLOSE_REFS = [{"name": "tiered_tp_atr",
               "params": {"tp_tiers": [{"atr_multiple": 2.0, "close_fraction": 1.0}]}}]
BOUNDARY_FIXTURES = (
    {"lookback": 2, "oversold": 20.0, "overbought": 80.0},
    {"lookback": 198, "oversold": 20.0, "overbought": 80.0},
    {"lookback": 14, "oversold": 0.0, "overbought": 100.0},
)
MODES = ("entry", "composed", "regime")


def grid_params() -> list:
    defaults = {"lookback": 14, "oversold": 20.0, "overbought": 80.0}
    specs = [(k, list(v)) for k, v in DEFAULT_PARAM_RANGES[NAME].items()]
    return [p for _, p in expand_sweep(defaults, specs)] + [dict(p) for p in BOUNDARY_FIXTURES]


def frozen_frame(manifest: dict, key: str, bars: int):
    candles = om.load_candles(om.dataset_by_key(manifest, key))
    return candles[["open", "high", "low", "close", "volume"]].iloc[-bars:]


def parity_config(params: dict, mode: str, coin: str, registry: str, batched: bool) -> ParityConfig:
    kw = dict(strategy_name=NAME, params=dict(params), registry=registry, platform="hyperliquid",
              symbol=coin, timeframe="4h", batched=batched)
    if mode in ("composed", "regime"):
        kw.update(close_refs=json.loads(json.dumps(CLOSE_REFS)), direction="both")
    if mode == "regime":
        kw.update(regime_enabled=True, regime_period=14, regime_adx_threshold=20.0)
    return ParityConfig(**kw)


def cell(frame) -> dict:
    s = parity_summarize(frame)
    closing = frame[frame["live_close_fraction"] > 0]
    out = {
        "bars_compared": s["bars_compared"],
        "mismatches": s["mismatches"],
        "decision_agreement": s["decision_agreement"],
        "close_parity": s["close_parity"],
        "clean": s["clean"],
        "entry_long": int((frame["live_open_action"] == "long").sum()),
        "entry_short": int((frame["live_open_action"] == "short").sum()),
        "long_context_closes": int((closing["live_signal"] == -1).sum()),
        "short_context_closes": int((closing["live_signal"] == 1).sum()),
    }
    if "batch_mismatches" in s:
        out["batch_mismatches"] = s["batch_mismatches"]
    if not s["decision_agreement"]:
        out["first_mismatch"] = s.get("first_mismatch")
    return out


def run(registry: str, window, batched: bool, bars: int, datasets: list, params_list: list,
        modes: list) -> dict:
    manifest = om.load_manifest(MANIFEST)
    cells = []
    for key in datasets:
        coin = key.split()[0]
        df = frozen_frame(manifest, key, bars)
        for params in params_list:
            for mode in modes:
                cfg = parity_config(params, mode, coin, registry, batched)
                frame = compute_parity_frame(df, cfg=cfg, window=window)
                cells.append(dict(cell(frame), dataset=key, params=params, mode=mode))
    totals = {}
    for mode in modes:
        rows = [c for c in cells if c["mode"] == mode]
        totals[mode] = {
            "cells": len(rows),
            "empty_cells": sum(1 for c in rows if c["bars_compared"] == 0),
            "mismatches": sum(c["mismatches"] for c in rows),
            "batch_mismatches": sum(c.get("batch_mismatches", 0) for c in rows),
            "entry_long": sum(c["entry_long"] for c in rows),
            "entry_short": sum(c["entry_short"] for c in rows),
            "long_context_closes": sum(c["long_context_closes"] for c in rows),
            "short_context_closes": sum(c["short_context_closes"] for c in rows),
            "close_parity": sorted({c["close_parity"] for c in rows}),
            "unclean_cells": sum(1 for c in rows if not c["clean"]),
        }
    problems = []
    for mode, t in totals.items():
        if t["empty_cells"]:
            problems.append(f"{mode}: {t['empty_cells']} cells compared no bars")
        if t["mismatches"] or t["batch_mismatches"]:
            problems.append(f"{mode}: {t['mismatches']} decision and {t['batch_mismatches']} batch mismatches")
        if not (t["entry_long"] and t["entry_short"]):
            problems.append(f"{mode}: both entry directions not exercised")
        if mode != "entry" and not (t["long_context_closes"] and t["short_context_closes"]):
            problems.append(f"{mode}: both position sides not exercised")
        if t["unclean_cells"]:
            problems.append(f"{mode}: {t['unclean_cells']} cells not clean (close parity {t['close_parity']})")
    return {
        "strategy": NAME,
        "registry": registry,
        "window": window if window is not None else 0,
        "batched": batched,
        "bars": bars,
        "datasets": datasets,
        "close_refs": CLOSE_REFS,
        "manifest_sha256": om.sha256_file(MANIFEST),
        "clean": not problems,
        "problems": problems,
        "totals": totals,
        "cells": cells,
    }


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description="#1658 offline decision parity on frozen Hyperliquid candles")
    p.add_argument("--registry", choices=["futures"], required=True)
    p.add_argument("--window", type=int, required=True, help="200 = bounded live window; 0 = every prefix")
    p.add_argument("--batched", action="store_true",
                   help="Also compare the explicit-paper batch slot with the solo evaluator")
    p.add_argument("--bars", type=int, default=400)
    p.add_argument("--dataset", action="append", default=None)
    p.add_argument("--mode", action="append", choices=MODES, default=None)
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)
    if args.window not in (0,) and args.window < 30:
        p.error("--window must be 0 or at least 30")
    window = None if args.window == 0 else args.window
    result = run(args.registry, window, args.batched, args.bars,
                 args.dataset or ["BTC 4h", "ETH 4h", "SOL 4h"], grid_params(),
                 args.mode or list(MODES))
    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(result, fh, indent=1, sort_keys=True, default=str)
            fh.write("\n")
    for mode, t in sorted(result["totals"].items()):
        print(f"{mode}: cells={t['cells']} mismatches={t['mismatches']} batch_mismatches={t['batch_mismatches']} "
              f"entries long/short={t['entry_long']}/{t['entry_short']} "
              f"closes long/short ctx={t['long_context_closes']}/{t['short_context_closes']} "
              f"close_parity={t['close_parity']}")
    print("clean" if result["clean"] else "NOT CLEAN: " + "; ".join(result["problems"]))
    return 0 if result["clean"] else 1


if __name__ == "__main__":
    sys.exit(main())
