#!/usr/bin/env python3
"""IS-window regime-gate screen for squeeze_momentum (#1198, M4 on a frozen
entry).

Both #1165 arms in one screen, re-run against squeeze_momentum (#983, same
"DD is regime exposure, not exit quality" verdict, -58.5% worst DD). Entry AND
close stack frozen (registry-default params, open-signal-as-close, no SL/TP —
the only protocol IS+OOS pass per #983/#984): Arm A gates WHEN the entry may
fire (`allowed_regimes` over the legacy ADX and composite 9-state classifiers
via `regime_windows_spec`), Arm B moves the regime response into the position
(#998 two-profile allocation, flat-only switch). Scores every candidate on the
six audit datasets over the protocol IS window via the M1 harness
(eval_windows.run_leg, audit-identical fees/slippage), ranked by mean
DD-adjusted return. Selection happens HERE (IS only); the shortlist is then
judged once on protocol OOS + held-out windows through validate_shortlist.py.

squeeze_momentum emits signal=-1 on downside squeeze releases without being in
bidirectionalPerpsStrategies, so every leg pins direction="long" (#996, same
as the #983 close-stack sweep); the -1 stays the (frozen) exit on the plain
long/flat path. The regime gate blocks entries only — closes always execute —
and the M4 switch commits only from flat, so an open position keeps its
opening profile's exit until it closes.

Registry is SPOT (squeeze_momentum's #983 baseline + M5 fee audit registry —
see driver_common.py). The four #1165 drivers were strategy-parameterized
(--strategy/--registry/--direction) for exactly this re-run; only the M4
"off"/"selective" param sets in driver_common.py are strategy-specific.

Run from repo root:
  uv run --no-sync python backtest/candidates/squeeze_momentum_1198/sweep_regime_gates.py \
      [--window is] [--comp-plateau-allowed trending_up_clean] \
      [--json backtest/candidates/squeeze_momentum_1198/sweep_is.json]
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

from eval_windows import (DATASETS, WINDOWS, dataset_key, run_leg,  # noqa: E402
                          validate_candidate)
from driver_common import (build_composite_period_plateau,  # noqa: E402
                           build_gate_grid, build_gate_threshold_plateau,
                           build_profile_grid, candidate_leg_kwargs)


def score_candidate_row(reg, strategy, candidate, window, capital=1000.0):
    spec = {k: v for k, v in candidate.items() if k != "label"}
    spec["name"] = strategy
    spec.setdefault("direction", "long")
    validate_candidate(spec)
    legs = {}
    for symbol, timeframe in DATASETS:
        legs[dataset_key(symbol, timeframe)] = run_leg(
            reg, strategy, spec.get("params"), symbol, timeframe, window,
            capital=capital, **candidate_leg_kwargs(spec))
    present = [l for l in legs.values() if l is not None]
    return {
        "label": candidate["label"],
        "candidate": spec,
        "legs": legs,
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present), 3),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present), 3),
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present), 2),
        "worst_max_dd_pct": round(min(l["max_dd_pct"] for l in present), 2),
        "total_trades": sum(l["trades"] for l in present),
    }


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--strategy", default="squeeze_momentum")
    p.add_argument("--registry", choices=["spot", "futures"], default="spot")
    p.add_argument("--direction", default="long", choices=["long", "short"])
    p.add_argument("--window", default="is", choices=list(WINDOWS))
    p.add_argument("--plateau-allowed", default=None,
                   help="Comma list of ADX labels; adds the gate-threshold "
                        "plateau rows (15/25/30) for that allowed set")
    p.add_argument("--comp-plateau-allowed", default=None,
                   help="Comma list of composite labels; adds the "
                        "classifier-period plateau rows (10/21/28) for that "
                        "allowed set")
    p.add_argument("--grid", default="full", choices=["full", "plateau-only"],
                   help="'plateau-only' skips the base grid (already "
                        "committed in sweep_is.json) and runs just the "
                        "requested plateau rows")
    p.add_argument("--json", default=None, dest="json_out")
    args = p.parse_args(argv)

    from registry_loader import load_registry
    reg = load_registry(args.registry)

    grid = ([] if args.grid == "plateau-only"
            else build_gate_grid() + build_profile_grid())
    if args.plateau_allowed:
        allowed = [s.strip() for s in args.plateau_allowed.split(",") if s.strip()]
        grid += build_gate_threshold_plateau(allowed)
    if args.comp_plateau_allowed:
        allowed = [s.strip() for s in args.comp_plateau_allowed.split(",")
                   if s.strip()]
        grid += build_composite_period_plateau(allowed)
    for c in grid:
        c["direction"] = args.direction

    window = WINDOWS[args.window]
    print(f"screening {len(grid)} regime-gate candidates on window "
          f"{args.window} {window}, entry+close frozen at registry defaults")

    rows = []
    for i, candidate in enumerate(grid):
        row = score_candidate_row(reg, args.strategy, candidate, window)
        rows.append(row)
        print(f"[{i+1:>2}/{len(grid)}] {row['label']:<24} "
              f"DDadj {row['mean_ddadj']:>7.3f}  Sharpe {row['mean_sharpe']:>6.2f}  "
              f"ret {row['mean_return_pct']:>7.2f}%  worstDD {row['worst_max_dd_pct']:>7.2f}%  "
              f"#T {row['total_trades']}")

    rows.sort(key=lambda r: r["mean_ddadj"], reverse=True)
    print(f"\n== ranked by mean DDadj (window {args.window}) ==")
    for r in rows:
        print(f"{r['label']:<24} DDadj {r['mean_ddadj']:>7.3f}  "
              f"Sharpe {r['mean_sharpe']:>6.2f}  ret {r['mean_return_pct']:>7.2f}%  "
              f"worstDD {r['worst_max_dd_pct']:>7.2f}%  #T {r['total_trades']}")

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump({"window": args.window, "window_range": list(window),
                       "strategy": args.strategy, "registry": args.registry,
                       "rows": rows}, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
