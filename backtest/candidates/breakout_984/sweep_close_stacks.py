#!/usr/bin/env python3
"""IS-window close-stack screen for breakout (#984, M2 on a frozen entry).

Expands the #996 default close-stack grid (DEFAULT_CLOSE_STACK_SPECS — audit
baseline, ATR stops, trailing stops, tiered-TP ladders x optional SL), freezes
the entry at registry defaults, and scores every stack on the six audit
datasets over the protocol IS window via the M1 harness (eval_windows.run_leg,
audit-identical fees/slippage). Ranks by mean DD-adjusted return — the #984
headline metric. Selection happens HERE (IS only); the shortlist is then
judged once on protocol OOS + held-out windows through eval_windows.py.

`breakout` is futures-registry-only and emits signal=-1 on breakdowns without
being in bidirectionalPerpsStrategies, so every leg pins direction="long":
with close refs the open/close engine path would otherwise open shorts on raw
signal=-1 (#996). Note the structural asymmetry this creates — stacks with
close refs REPLACE the breakdown exit (the -1 is masked to 0 on the engine
path), while stop-only/trail-only stacks keep the signal exit and add a stop
on top of it.

Run from repo root:
  uv run --no-sync python backtest/candidates/breakout_984/sweep_close_stacks.py \
      [--window is] [--json backtest/candidates/breakout_984/sweep_is.json]
"""

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from eval_windows import DATASETS, WINDOWS, dataset_key, run_leg  # noqa: E402
from optimizer import (DEFAULT_CLOSE_STACK_SPECS,                  # noqa: E402
                       generate_close_stack_grid)

STRATEGY = "breakout"


def score_stack(reg, stack, window, capital=1000.0):
    legs = {}
    for symbol, timeframe in DATASETS:
        legs[dataset_key(symbol, timeframe)] = run_leg(
            reg, STRATEGY, None, symbol, timeframe, window, capital=capital,
            close_strategies=stack["close_strategies"] or None,
            # Frozen long-leg entry universe: with close refs the engine path
            # would open shorts on raw signal=-1 (#996) — pin direction.
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
    reg = load_registry("futures")

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
