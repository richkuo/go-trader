#!/usr/bin/env python3

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
sys.path.insert(0, os.path.join(_HERE, "..", ".."))
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from eval_windows import (DATASETS, dataset_key, run_leg,
                          validate_candidate)
from driver_common import candidate_leg_kwargs

DEFAULT_CANDIDATES = "baseline.json"


def effective_range(symbol, timeframe, window):
    from data_fetcher import load_cached_data
    df = load_cached_data(symbol, timeframe,
                          start_date=window[0], end_date=window[1])
    if df.empty:
        return None
    return {"first_bar": str(df.index[0]), "last_bar": str(df.index[-1]),
            "bars": int(len(df))}


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--start", default="2025-06-10",
                   help="window start (default: the #956 audit start)")
    p.add_argument("--end", default=None,
                   help="window end (default: latest cache — drifts; the "
                        "artifact records the effective per-dataset range)")
    p.add_argument("--candidates", default=DEFAULT_CANDIDATES,
                   help="comma list of candidate JSON files in this dir")
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from registry_loader import load_registry
    reg = load_registry("futures")

    window = (args.start, args.end)
    out = {"window": {"start": args.start, "end": args.end},
           "effective_range": {}, "candidates": {}}
    for symbol, tf in DATASETS:
        out["effective_range"][dataset_key(symbol, tf)] = effective_range(
            symbol, tf, window)

    for fn in [c.strip() for c in args.candidates.split(",") if c.strip()]:
        with open(os.path.join(_HERE, fn)) as fh:
            candidate = validate_candidate(json.load(fh))
        label = fn[:-5] if fn.endswith(".json") else fn
        legs = {}
        for symbol, tf in DATASETS:
            leg = run_leg(reg, candidate["name"], candidate.get("params"),
                          symbol, tf, window,
                          **candidate_leg_kwargs(candidate))
            legs[dataset_key(symbol, tf)] = leg
            print(f"{label:<18} {symbol} {tf}: sharpe {leg['sharpe']:+.2f}  "
                  f"ret {leg['return_pct']:+8.2f}%  DD {leg['max_dd_pct']:8.2f}%  "
                  f"B&H {leg['bh_return_pct']:+8.2f}%  #T {leg['trades']}")
        ls = list(legs.values())
        print(f"{label:<18} MEAN sharpe "
              f"{statistics.mean(l['sharpe'] for l in ls):+.3f}  "
              f"ret {statistics.mean(l['return_pct'] for l in ls):+7.2f}%  "
              f"vsB&H {statistics.mean(l['return_pct'] - l['bh_return_pct'] for l in ls):+7.2f}  "
              f"worstDD {min(l['max_dd_pct'] for l in ls):7.2f}%  "
              f"#T {sum(l['trades'] for l in ls)}\n")
        out["candidates"][label] = legs

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f"wrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
