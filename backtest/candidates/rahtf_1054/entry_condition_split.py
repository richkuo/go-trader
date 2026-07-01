#!/usr/bin/env python3
"""Entry-condition split for the #1054 regime_adaptive_htf M1 noise verdict.

Step 3 of the issue asks, IF the gross edge survives the noise check, which
entry/regime condition produces the positive-gross trades. The edge did not
survive (backtest/gross_edge_noise.py: permutation p=0.39 on the M5 slices,
NO_POSITIVE_EDGE pooled across all five windows), so this split is
DESCRIPTIVE evidence for the deprecate write-up, not a selectivity search:
it shows the trade universe has essentially one entry condition
(ranging_volatile fades), i.e. there is no second regime axis a selectivity
knob could isolate without slicing a 37-trade sample below any defensible
inference bar.

Method: re-runs the fee audit's zero-friction gross legs through
``eval_windows.run_leg(keep_trades=True)`` (harness-identical trade universe),
re-applies the strategy to the same cached slice to recover its own
``rah_label`` / ``rah_raw_label`` columns, and joins each trade to the label
at its SIGNAL bar — the bar strictly before the entry fill, per the engine's
signal-at-N-fills-at-N+1 contract (the fill bar's label can already have
flipped; joining there would misattribute boundary entries).

Run from repo root:
  uv run --no-sync python backtest/candidates/rahtf_1054/entry_condition_split.py \\
      --json backtest/candidates/rahtf_1054/entry_condition_split.json
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
from collections import defaultdict
from typing import List, Optional

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)
sys.path.insert(0, os.path.join(_BACKTEST_DIR, "..", "shared_tools"))

from eval_windows import (  # noqa: E402  (path bootstrap above)
    DATASETS,
    DEFAULT_CAPITAL,
    WINDOWS,
    dataset_key,
    run_leg,
)

STRATEGY = "regime_adaptive_htf"
DEFAULT_WINDOWS = ("is", "oos")  # the M5 screen slices under adjudication


def signal_bar_labels(df_signals, entry_dates: List[str]) -> List[dict]:
    """Label each entry with rah_label/rah_raw_label at its SIGNAL bar.

    The engine fills a bar-N signal at bar N+1's open, so the entry decision
    was gated on bar N's confirmed label — one index position before the
    trade's ``entry_date``. A fill at the first bar (no prior bar) or an
    entry_date missing from the index labels as "?" rather than guessing.
    """
    import pandas as pd

    out = []
    index = df_signals.index
    for ed in entry_dates:
        ts = pd.Timestamp(ed)
        row = {"entry_date": ed, "signal_label": "?", "fill_label": "?"}
        if ts in index:
            pos = index.get_loc(ts)
            row["fill_label"] = str(df_signals["rah_label"].iloc[pos])
            if pos > 0:
                row["signal_label"] = str(df_signals["rah_label"].iloc[pos - 1])
        out.append(row)
    return out


def split_stats(samples: List[dict], key: str) -> dict:
    """Mean/count/positive-count of per-trade gross returns grouped by key."""
    groups = defaultdict(list)
    for s in samples:
        groups[s[key]].append(s["pnl_pct"])
    return {
        k: {
            "n": len(v),
            "mean": round(statistics.fmean(v), 4),
            "n_pos": sum(1 for x in v if x > 0),
        }
        for k, v in sorted(groups.items())
    }


def collect(window_names: List[str]) -> List[dict]:
    from data_fetcher import load_cached_data
    from registry_loader import load_registry

    reg = load_registry("spot")
    params = reg.STRATEGY_REGISTRY[STRATEGY]["default_params"]

    samples: List[dict] = []
    for wname in window_names:
        start, end = WINDOWS[wname]
        for symbol, timeframe in DATASETS:
            leg = run_leg(reg, STRATEGY, None, symbol, timeframe,
                          (start, end), capital=DEFAULT_CAPITAL,
                          direction="long", commission_pct=0.0,
                          slippage_pct=0.0, keep_trades=True)
            if leg is None or not leg.get("trade_samples"):
                continue
            df = load_cached_data(symbol, timeframe, start_date=start,
                                  end_date=end)
            df_signals = reg.apply_strategy(STRATEGY, df, params)
            labels = signal_bar_labels(
                df_signals, [t["entry_date"] for t in leg["trade_samples"]])
            for t, lab in zip(leg["trade_samples"], labels):
                samples.append({
                    "window": wname,
                    "dataset": dataset_key(symbol, timeframe),
                    "timeframe": timeframe,
                    "entry_date": t["entry_date"],
                    "pnl_pct": t["pnl_pct"],
                    "signal_label": lab["signal_label"],
                    "fill_label": lab["fill_label"],
                })
    return samples


def main(argv: Optional[List[str]] = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS))
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
    unknown = [w for w in window_names if w not in WINDOWS]
    if unknown:
        raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")

    samples = collect(window_names)
    by_signal_label = split_stats(samples, "signal_label")
    by_timeframe = split_stats(samples, "timeframe")
    by_window = split_stats(samples, "window")

    print(f"{STRATEGY} gross trades on {', '.join(window_names)}: "
          f"n={len(samples)}")
    for title, split in (("signal-bar rah_label", by_signal_label),
                         ("timeframe", by_timeframe),
                         ("window", by_window)):
        print(f"\nby {title}:")
        for k, v in split.items():
            print(f"  {k:<22} n={v['n']:>3}  mean {v['mean']:+.3f}%/trade  "
                  f"positive {v['n_pos']}/{v['n']}")

    if args.json_out:
        payload = {
            "strategy": STRATEGY,
            "windows": window_names,
            "n_trades": len(samples),
            "by_signal_label": by_signal_label,
            "by_timeframe": by_timeframe,
            "by_window": by_window,
            "samples": samples,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=1, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
