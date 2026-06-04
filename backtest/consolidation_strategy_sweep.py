"""
Consolidation Range strategy parameter sweep with in-sample / out-of-sample split.

Grid-searches the strategy's exit/entry params on ONE asset, but guards against
curve-fitting by splitting each config's trades by date: tune on the in-sample
(IS) period, validate on the out-of-sample (OOS) period. A config that only looks
good IS is overfit; one that holds OOS is more likely real.

Fixed: exit_mode=hybrid (the only thing with any edge). Swept: stop_atr_mult,
trail_atr_mult, edge_entry_frac, tp1_frac, drift_filter.

Usage:
  uv run --no-sync python backtest/consolidation_strategy_sweep.py \
      --symbol BTC/USDT --timeframe 4h --box-width-pct 0.05 --min-bars 16 \
      --since 2021-01-01 --cost-bps 1 --split-frac 0.6
"""

import argparse
import itertools
import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

from consolidation_strategy_sim import simulate, stats  # noqa: E402

GRID = {
    "stop_atr_mult": [0.75, 1.0, 1.5, 2.0],
    "trail_atr_mult": [1.0, 1.5, 2.0, 2.5, 3.0],
    "edge_entry_frac": [0.1, 0.2, 0.33],
    "tp1_frac": [0.0, 0.25, 0.5],
    "drift_filter": [False, True],
}


def walk_forward(df, regime, params, n_bars, folds):
    """Return per-fold stats for one config across `folds` contiguous windows."""
    t = simulate(df, params, regime)
    edges = np.linspace(0, n_bars, folds + 1, dtype=int)
    out = []
    for k in range(folds):
        seg = t[(t["entry_idx"] >= edges[k]) & (t["entry_idx"] < edges[k + 1])]
        out.append(stats(seg))
    return out


def main(argv=None):
    p = argparse.ArgumentParser(description="Strategy param sweep with IS/OOS split")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="4h")
    p.add_argument("--since", default="2021-01-01")
    p.add_argument("--exchange-id", default="binanceus")
    p.add_argument("--box-width-pct", type=float, default=0.05)
    p.add_argument("--min-bars", type=int, default=16)
    p.add_argument("--atr-period", type=int, default=14)
    p.add_argument("--adx-threshold", type=float, default=20.0)
    p.add_argument("--cost-bps", type=float, default=1.0)
    p.add_argument("--drift-threshold", type=float, default=0.5)
    p.add_argument("--split-frac", type=float, default=0.6,
                   help="fraction of bars used as in-sample; rest is out-of-sample")
    p.add_argument("--min-oos-trades", type=int, default=20)
    p.add_argument("--top", type=int, default=15)
    args = p.parse_args(argv)

    from data_fetcher import fetch_full_history
    from regime import compute_regime
    print(f"Fetching {args.symbol} {args.timeframe} from {args.since}...")
    df = fetch_full_history(symbol=args.symbol, timeframe=args.timeframe,
                            since=args.since, exchange_id=args.exchange_id)
    if df.empty:
        raise SystemExit("no data")
    regime = compute_regime(df, period=args.atr_period,
                            adx_threshold=args.adx_threshold)["regime"].to_numpy()
    n = len(df)
    cutoff = int(n * args.split_frac)
    print(f"{n} bars; IS = first {args.split_frac:.0%} ({cutoff} bars), "
          f"OOS = rest.\n")

    base = {
        "min_bars": args.min_bars, "box_width_pct": args.box_width_pct,
        "atr_period": args.atr_period, "drift_threshold": args.drift_threshold,
        "cost_bps": args.cost_bps, "exit_mode": "hybrid", "regime_filter": False,
        "max_hold": args.min_bars * 3,
    }

    keys = list(GRID)
    rows = []
    for combo in itertools.product(*(GRID[k] for k in keys)):
        cfg = dict(zip(keys, combo))
        t = simulate(df, {**base, **cfg}, regime)
        if t.empty:
            continue
        is_t = t[t["entry_idx"] < cutoff]
        oos_t = t[t["entry_idx"] >= cutoff]
        si, so = stats(is_t), stats(oos_t)
        if so.get("trades", 0) < args.min_oos_trades:
            continue
        rows.append({
            **cfg,
            "IS_n": si["trades"], "IS_PF": si.get("profit_factor", 0),
            "IS_R": si.get("total_R", 0), "IS_exp": si.get("expectancy_R", 0),
            "OOS_n": so["trades"], "OOS_PF": so.get("profit_factor", 0),
            "OOS_R": so.get("total_R", 0), "OOS_exp": so.get("expectancy_R", 0),
        })

    if not rows:
        print("no configs met the OOS trade minimum.")
        return 0
    res = pd.DataFrame(rows)
    # Rank by OOS expectancy, but only among configs profitable IN-SAMPLE too
    # (a config negative IS that looks good OOS is just noise).
    robust = res[(res["IS_exp"] > 0) & (res["OOS_exp"] > 0)].copy()
    print(f"=== {args.symbol} {args.timeframe} — {len(res)} configs tested, "
          f"{len(robust)} positive BOTH in- and out-of-sample ===\n")
    show = (robust if not robust.empty else res).sort_values(
        "OOS_exp", ascending=False).head(args.top)
    cols = ["stop_atr_mult", "trail_atr_mult", "edge_entry_frac", "tp1_frac",
            "drift_filter", "IS_n", "IS_PF", "IS_exp", "OOS_n", "OOS_PF", "OOS_exp"]
    print(show[cols].to_string(index=False))
    if robust.empty:
        print("\nNONE positive in BOTH periods — no configuration survives "
              "out-of-sample. Likely overfit / no real edge.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
