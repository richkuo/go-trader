#!/usr/bin/env python3
"""Fee-drag screen for the #984 shortlist (README fee-gate step).

breakout's audit economics are fee-marginal: the M5 screen (#999,
docs/research/fee-audit-m5.md) measured gross +5.28%/leg vs net -1.54%/leg —
6.82pp of friction drag over 260 trades (44.3/yr) at ~0.31%/leg. Any stack
that cuts drawdown by exiting earlier/more often adds legs, and every added
leg spends part of that same thin gross edge. This driver re-runs each
candidate twice per audit dataset on the continuous audit window — default
friction vs zero friction (the documented #999 overrides:
``commission_pct=0.0, slippage_pct=0.0``) — and reports the gross/net split,
drag in percentage points, and annualized trade rate, per candidate and
vs baseline.

The aggregation is a pure function (``summarize_fee_drag``) over leg dicts so
it is unit-testable without data access
(backtest/tests/test_breakout_984_fee_drag.py).

Run from repo root:
  uv run --no-sync python backtest/candidates/breakout_984/fee_drag.py \
      [--start 2025-06-10] [--end YYYY-MM-DD] \
      [--candidates baseline.json,...] \
      [--json backtest/candidates/breakout_984/fee_drag_shortlist.json]
"""

import argparse
import json
import os
import statistics
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))
# trailing_tp_ratchet.py imports its sibling _helpers at evaluate time.
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..",
                                "shared_strategies", "close"))

DEFAULT_CANDIDATES = "baseline.json"


def summarize_fee_drag(gross_legs, net_legs):
    """Collapse paired (gross, net) leg dicts into the fee-drag summary.

    Pure: takes two equal-length lists of leg dicts (each needs
    ``return_pct``, ``trades``, ``span_days``; ``None`` legs are dropped
    pairwise) and returns mean gross/net return %, drag in pp, summed trades,
    and trades/yr annualized over the summed calendar span. Returns None when
    no paired legs survive.
    """
    pairs = [(g, n) for g, n in zip(gross_legs, net_legs)
             if g is not None and n is not None]
    if not pairs:
        return None
    gross = [g["return_pct"] for g, _ in pairs]
    net = [n["return_pct"] for _, n in pairs]
    trades = sum(n["trades"] for _, n in pairs)
    span_days = sum(float(n.get("span_days") or 0.0) for _, n in pairs)
    mean_gross = statistics.mean(gross)
    mean_net = statistics.mean(net)
    return {
        "legs": len(pairs),
        "mean_gross_return_pct": round(mean_gross, 2),
        "mean_net_return_pct": round(mean_net, 2),
        "drag_pp": round(mean_gross - mean_net, 2),
        "trades": trades,
        "trades_per_year": (round(trades / (span_days / 365.25), 1)
                            if span_days > 0 else None),
    }


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
    reg = load_registry("futures")

    window = (args.start, args.end)
    out = {"window": {"start": args.start, "end": args.end}, "candidates": {}}

    for fn in [c.strip() for c in args.candidates.split(",") if c.strip()]:
        with open(os.path.join(_HERE, fn)) as fh:
            candidate = validate_candidate(json.load(fh))
        label = fn[:-5] if fn.endswith(".json") else fn
        common = dict(
            close_strategies=candidate.get("close_strategies"),
            direction=candidate.get("direction") or "long",
            invert_signal=bool(candidate.get("invert_signal")),
            stop_loss_atr_mult=candidate.get("stop_loss_atr_mult"),
            trailing_stop_atr_mult=candidate.get("trailing_stop_atr_mult"),
        )
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
