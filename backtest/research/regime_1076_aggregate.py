"""Aggregate the #1076 scope-1 battery JSON dumps and apply ONE global multiple-comparisons
correction across the entire (classifier, asset, timeframe, window, horizon, state) family.

per_state_significance corrects FDR only within a single cell; each separate battery run
corrects only within its own universe. The honest family-wide screen pools every directional
test from every dump and corrects once. Run after the battery dumps exist:

    uv run --no-sync python backtest/research/regime_1076_aggregate.py \
        /tmp/wf1076_core.json /tmp/wf1076_btc_extra.json /tmp/wf1076_alt4h.json
"""
from __future__ import annotations
import json
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
sys.path.insert(0, _BACKTEST)
from regime_stats import benjamini_hochberg

HELD_OUT_FORWARD = ("is", "oos")


def main(argv=None) -> int:
    paths = argv if argv is not None else sys.argv[1:]
    if not paths:
        raise SystemExit("usage: regime_1076_aggregate.py <dump.json> [<dump.json> ...]")
    rows = []
    universe = []
    for p in paths:
        with open(p) as fh:
            d = json.load(fh)
        rows.extend(d["rows"])
        u = d.get("universe", {})
        universe.append(f"{os.path.basename(p)}: {u.get('symbols')} x {u.get('timeframes')} "
                        f"(n_perm={u.get('n_perm')})")

    directional = [r for r in rows if r["policy_dir"] != 0]
    n = len(directional)
    pvals = [r["p_value"] for r in directional]
    bh = benjamini_hochberg(pvals, alpha=0.05)
    bonf = 0.05 / n if n else 0.0

    n_bh = sum(bh)
    n_bonf = sum(p <= bonf for p in pvals)
    bh_aligned = sum(b and r["sign_aligned"] for b, r in zip(bh, directional))
    bonf_aligned = sum((r["p_value"] <= bonf) and r["sign_aligned"] for r in directional)
    within = [r for r in directional if r["candidate_edge"]]
    within_held = [r for r in within if r["window"] in HELD_OUT_FORWARD]
    within_oos = [r for r in within if r["window"] == "oos"]

    print("=" * 80)
    print("UNIFIED #1076 SCOPE-1 SCREEN — global correction across the FULL battery")
    print("=" * 80)
    for u in universe:
        print(f"  {u}")
    print(f"\ntotal directional-state tests pooled: {n}")
    print(f"  within-cell candidate edges (uncorrected family-wide): {len(within)} "
          f"(held-out is/oos {len(within_held)}, oos {len(within_oos)})")
    print(f"  GLOBAL Benjamini-Hochberg q=0.05:  {n_bh} survive ({bh_aligned} policy-aligned)")
    print(f"  GLOBAL Bonferroni p<= {bonf:.2e}:  {n_bonf} survive ({bonf_aligned} policy-aligned)")
    print()
    survivors = [r for b, r in zip(bh, directional) if b]
    if survivors:
        print("GLOBAL-BH SURVIVORS:")
        for r in sorted(survivors, key=lambda x: x["p_value"]):
            print(f"  {r['classifier']:10s} {r['symbol']:9s} {r['timeframe']:4s} "
                  f"{r['window']:7s} h{r['horizon']:<3d} {r['state']:22s} "
                  f"p={r['p_value']:.4f} gap={r['gap']:+.5f} aligned={r['sign_aligned']}")
    else:
        print("NO directional state survives global correction anywhere in the tested universe.")
        print("=> the regime->direction premise has no statistically real, multiplicity-")
        print("   honest forward-return separation on any (classifier, asset, timeframe,")
        print("   window, horizon) tested. Consistent with #1073 finding 1, now generalized.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
