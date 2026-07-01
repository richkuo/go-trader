#!/usr/bin/env python3
"""M2 walk-forward fold stability for #984 (README step 3).

walk_forward_optimize (#996) with the breakout open params FROZEN (singleton
ranges from futures-registry defaults) and the 25-stack default close grid,
selecting by dd_adjusted_return — so the only thing each train fold picks is
the close stack. 5 folds over 2023-01-01 -> 2026-01-01 on BTC/ETH/SOL 1h.
direction="long" is pinned (breakout emits signal=-1 breakdowns; the engine
path would open shorts on raw -1, #996).

Run from repo root:
  uv run --no-sync python backtest/candidates/breakout_984/walkforward.py \
      [--json backtest/candidates/breakout_984/walkforward_folds.json]
"""

import argparse
import collections
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from optimizer import (DEFAULT_CLOSE_STACK_SPECS,             # noqa: E402
                       generate_close_stack_grid, walk_forward_optimize)

STRATEGY = "breakout"
DATASETS = [("BTC/USDT", "1h"), ("ETH/USDT", "1h"), ("SOL/USDT", "1h")]
START, END = "2023-01-01", "2026-01-01"
N_SPLITS = 5


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry("futures")
    defaults = reg.STRATEGY_REGISTRY[STRATEGY]["default_params"]
    frozen = {k: [v] for k, v in defaults.items()}
    grid = generate_close_stack_grid(DEFAULT_CLOSE_STACK_SPECS)

    out = {}
    for symbol, tf in DATASETS:
        df = load_cached_data(symbol, tf, start_date=START, end_date=END)
        res = walk_forward_optimize(df, STRATEGY, frozen, n_splits=N_SPLITS,
                                    optimize_metric="dd_adjusted_return",
                                    symbol=symbol, timeframe=tf,
                                    registry="futures", close_stack_grid=grid,
                                    direction="long", verbose=False)
        folds = res.get("window_results") or []
        picks = [w["best_close_stack"] for w in folds]
        out[f"{symbol} {tf}"] = {"folds": folds, "picks": picks}
        print(f"\n{symbol} {tf}: fold winners (train-selected by DDadj):")
        for w in folds:
            t = w.get("test_result") or {}
            print(f"  fold {w['fold']}: {w['best_close_stack']:<48} "
                  f"test ret {t.get('total_return_pct')}%  "
                  f"dd {t.get('max_drawdown_pct')}%")
        print("  most common:", collections.Counter(picks).most_common(3))

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
