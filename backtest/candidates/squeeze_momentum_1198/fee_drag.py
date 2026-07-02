#!/usr/bin/env python3
"""Fee-drag screen for the #1198 squeeze_momentum shortlist (README fee-gate
step).

squeeze_momentum's audit economics are fee-negative: the M5 screen (#999,
docs/research/fee-audit-m5.md) measured gross -0.29%/leg vs net -3.69%/leg on
spot — the strategy loses money gross AND the friction makes it worse. A regime
gate that only removes losing entries should CUT trades (unlike the #983 close
stacks, which added legs), but a gate that fragments the setups into
re-entries pays the same churn tax. This driver re-runs each candidate twice
per audit dataset on the continuous audit window — default friction vs zero
friction (the documented #999 overrides: ``commission_pct=0.0,
slippage_pct=0.0``) — and reports the gross/net split, drag in percentage
points, and annualized trade rate, per candidate and vs baseline.

Every leg threads the candidate's regime state via
driver_common.candidate_leg_kwargs (a gated candidate would otherwise silently
score ungated). ``summarize_fee_drag`` is imported from this directory's
driver_common (inlined there — see its docstring — so the study is
self-contained rather than reaching into a twin directory as #1165 reached
into breakout_984).

Run from repo root:
  uv run --no-sync python backtest/candidates/squeeze_momentum_1198/fee_drag.py \
      [--start 2025-06-10] [--end YYYY-MM-DD] \
      [--candidates baseline.json,...] \
      [--json backtest/candidates/squeeze_momentum_1198/fee_drag_shortlist.json]
"""

import argparse
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from driver_common import candidate_leg_kwargs, summarize_fee_drag  # noqa: E402

DEFAULT_CANDIDATES = "baseline.json"


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--start", default="2025-06-10",
                   help="window start (default: the #956 audit start)")
    p.add_argument("--end", default=None,
                   help="window end (default: latest cache)")
    p.add_argument("--candidates", default=DEFAULT_CANDIDATES,
                   help="comma list of candidate JSON files in this dir")
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from eval_windows import DATASETS, dataset_key, run_leg, validate_candidate
    from registry_loader import load_registry
    reg = load_registry("spot")

    window = (args.start, args.end)
    out = {"window": {"start": args.start, "end": args.end}, "candidates": {}}

    for fn in [c.strip() for c in args.candidates.split(",") if c.strip()]:
        with open(os.path.join(_HERE, fn)) as fh:
            candidate = validate_candidate(json.load(fh))
        label = fn[:-5] if fn.endswith(".json") else fn
        common = candidate_leg_kwargs(candidate)
        gross_legs, net_legs, per_dataset = [], [], {}
        for symbol, tf in DATASETS:
            net = run_leg(reg, candidate["name"], candidate.get("params"),
                          symbol, tf, window, **common)
            gross = run_leg(reg, candidate["name"], candidate.get("params"),
                            symbol, tf, window, **common,
                            commission_pct=0.0, slippage_pct=0.0)
            net_legs.append(net)
            gross_legs.append(gross)
            per_dataset[dataset_key(symbol, tf)] = {
                "gross_return_pct": None if gross is None else gross["return_pct"],
                "net_return_pct": None if net is None else net["return_pct"],
                "trades": None if net is None else net["trades"],
            }
        summary = summarize_fee_drag(gross_legs, net_legs)
        out["candidates"][label] = {"summary": summary,
                                    "per_dataset": per_dataset}
        s = summary or {}
        print(f"{label:<22} gross {s.get('mean_gross_return_pct')!s:>8}%  "
              f"net {s.get('mean_net_return_pct')!s:>8}%  "
              f"drag {s.get('drag_pp')!s:>6}pp  "
              f"#T {s.get('trades')!s:>5}  "
              f"T/yr {s.get('trades_per_year')!s:>6}")

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
