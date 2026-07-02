#!/usr/bin/env python3
"""Continuous-audit-window headline for the #1198 squeeze_momentum shortlist.

Runs candidate JSONs from this directory on the CONTINUOUS #956 audit window
(2025-06-10 -> latest cache by default) via the M1 harness (eval_windows
.run_leg, audit-identical fees/slippage) — the mandatory stitched comparison
(the #983/#984 lesson: IS/held-out wins can evaporate, or worsen the DD, once
the windows are stitched back together; squeeze_momentum's own #983 close
stacks died exactly here). This window is deliberately NOT in
eval_windows.WINDOWS: protocol scoring stays segmented (is/oos/held-out); this
driver exists only for the stitched headline that reproduces #983's -58.5%
worst DD baseline and measures each gate against it.

Every leg threads the candidate's regime state (allowed_regimes /
regime_windows_spec / profile_allocation) via
driver_common.candidate_leg_kwargs — a gated candidate run without it would
silently score the UNGATED entry here.

``--end`` defaults to None (latest cache), so headline numbers drift as the
cache DB gains bars. The artifact therefore records the requested window AND
the effective per-dataset data range (first/last bar actually loaded) — diff
``effective_range`` against the committed artifact before comparing numbers.

Run from repo root:
  uv run --no-sync python backtest/candidates/squeeze_momentum_1198/audit_headline.py \
      [--start 2025-06-10] [--end YYYY-MM-DD] \
      [--candidates baseline.json,...] \
      [--json backtest/candidates/squeeze_momentum_1198/audit_window_headline.json]
"""

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from eval_windows import (DATASETS, dataset_key, run_leg,      # noqa: E402
                          validate_candidate)
from driver_common import candidate_leg_kwargs                 # noqa: E402

DEFAULT_CANDIDATES = "baseline.json"


def effective_range(symbol, timeframe, window):
    """First/last bar timestamps the cache actually yields for the window."""
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
    reg = load_registry("spot")

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
