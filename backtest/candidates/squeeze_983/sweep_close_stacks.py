#!/usr/bin/env python3

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from eval_windows import DATASETS, WINDOWS, dataset_key, run_leg
from optimizer import (DEFAULT_CLOSE_STACK_SPECS,
                       generate_close_stack_grid)

STRATEGY = "squeeze_momentum"


def score_stack(reg, stack, window, capital=1000.0):
    legs = {}
    for symbol, timeframe in DATASETS:
        legs[dataset_key(symbol, timeframe)] = run_leg(
            reg, STRATEGY, None, symbol, timeframe, window, capital=capital,
            close_strategies=stack["close_strategies"] or None,
            direction="long",
            stop_loss_atr_mult=stack["stop_loss_atr_mult"],
            trailing_stop_atr_mult=stack["trailing_stop_atr_mult"],
        )
    present = [l for l in legs.values() if l is not None]
    return {
        "label": stack["label"],
        "stack": {k: stack[k] for k in
                  ("close_strategies", "stop_loss_atr_mult",
                   "trailing_stop_atr_mult")},
        "legs": legs,
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present), 3),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present), 3),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present), 2),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in present), 2),
        "total_trades": sum(l["trades"] for l in present),
    }


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--window", default="is", choices=list(WINDOWS))
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from registry_loader import load_registry
    reg = load_registry("spot")

    grid = generate_close_stack_grid(DEFAULT_CLOSE_STACK_SPECS)
    window = WINDOWS[args.window]
    print(f"screening {len(grid)} close stacks on window {args.window} "
          f"{window}, entry frozen at registry defaults")

    rows = []
    for i, stack in enumerate(grid):
        row = score_stack(reg, stack, window)
        rows.append(row)
        print(f"[{i+1:>2}/{len(grid)}] {row['label']:<58} "
              f"DDadj {row['mean_ddadj']:>7.3f}  Sharpe {row['mean_sharpe']:>6.2f}  "
              f"ret {row['mean_return_pct']:>7.2f}%  worstDD {row['worst_max_dd_pct']:>7.2f}%  "
              f"#T {row['total_trades']}")

    rows.sort(key=lambda r: r["mean_ddadj"], reverse=True)
    print(f"\n== ranked by mean DDadj (window {args.window}) ==")
    for r in rows:
        print(f"{r['label']:<58} DDadj {r['mean_ddadj']:>7.3f}  "
              f"Sharpe {r['mean_sharpe']:>6.2f}  ret {r['mean_return_pct']:>7.2f}%  "
              f"worstDD {r['worst_max_dd_pct']:>7.2f}%  #T {r['total_trades']}")

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump({"window": args.window, "window_range": list(window),
                       "strategy": STRATEGY, "rows": rows}, fh, indent=2,
                      default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
