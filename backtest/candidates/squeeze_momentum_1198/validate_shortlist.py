#!/usr/bin/env python3
"""Score the #1198 squeeze_momentum shortlist through the M1 harness, sharing
incumbent bars.

Equivalent to running eval_windows.py --candidate-json per candidate across all
five windows (identical functions, identical harness — evaluate_window threads
allowed_regimes / regime_windows_spec / profile_allocation natively) but in one
process so the incumbent-median bars are computed once per window instead of
once per candidate. Use the per-candidate eval_windows.py command from the
README (with --registry spot) to reproduce any single table independently.

Run from repo root:
  uv run --no-sync python backtest/candidates/squeeze_momentum_1198/validate_shortlist.py \
      [--candidates baseline.json,...] \
      [--json backtest/candidates/squeeze_momentum_1198/validation.json]
"""

import argparse
import json
import os
import sys

_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", ".."))          # backtest/
sys.path.insert(0, os.path.join(_HERE, "..", "..", "..", "shared_tools"))

from eval_windows import (DATASETS, WINDOWS, evaluate_window,   # noqa: E402
                          format_summary, format_window_report)

CANDIDATES = [
    "baseline.json",
    "adx_not_down.json",
    "m4_bear_off.json",
    "m4_bear_selective.json",
    "comp_up_family.json",
]


def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument("--json", default=None, dest="json_out")
    p.add_argument("--candidates", default=None,
                   help="Comma list of candidate JSON files in this dir "
                        "(default: the committed shortlist)")
    p.add_argument("--windows", default=None,
                   help=f"Comma list (default: all). Known: {', '.join(WINDOWS)}")
    args = p.parse_args(argv)

    window_names = ([w.strip() for w in args.windows.split(",") if w.strip()]
                    if args.windows else list(WINDOWS))
    candidates = ([c.strip() for c in args.candidates.split(",") if c.strip()]
                  if args.candidates else list(CANDIDATES))

    from registry_loader import load_registry
    reg = load_registry("spot")

    bars_memo = {}
    out = {}
    for fn in candidates:
        with open(os.path.join(_HERE, fn)) as fh:
            candidate = json.load(fh)
        label = fn[:-5]
        print(f"\n######## candidate: {label} ########")
        scores = []
        for wname in window_names:
            score = evaluate_window(reg, candidate, list(DATASETS), wname,
                                    1000.0, bars_memo)
            scores.append(score)
            print(format_window_report(score))
        print(format_summary(scores))
        out[label] = {"candidate": candidate, "window_scores": scores}

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
