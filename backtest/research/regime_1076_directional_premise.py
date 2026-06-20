"""Scope-1 evidence for #1076: does the regime label predict forward DIRECTION?

``regime_directional_policy`` (#779, scheduler/regime_directional_policy.go:5-16) bets a
live HL perps strategy long/short on the CURRENT regime label (long in ``trending_up``,
short in ``trending_down``). Its entire edge premise is regime -> forward-direction, and
``allowed_regimes`` directional entry-gating shares it. #1073 finding 1 refuted that premise
for the 7-state composite classifier on BTC/USDT 1h (0/35 block-shuffle tests).

This script generalizes the test so the premise is judged on the surface the policy actually
keys on, across a multi-asset / multi-timeframe universe:

  - BOTH classifiers a policy can key on: ``adx`` (3-state -- the policy-doc default form,
    ``trending_up``/``trending_down``/``ranging``) and ``composite`` (7-state -- the #1073
    surface).
  - per-STATE block-shuffle significance with Benjamini-Hochberg FDR (reuses
    backtest/regime_diagnostics.py:per_state_significance) so one state with real directional
    separation is not masked by a null group-level statistic.
  - per-state mean forward return + sign-vs-policy-direction, so a "significant but
    wrong-signed" state (separation that would LOSE money under the policy mapping) is
    distinguished from a genuine long/short edge.

A state is a candidate edge only when it is FDR-significant AND its gap sign matches the
policy's bet for that state (long states want gap > 0, short states gap < 0). The economic
walk-forward test (#1076 scope 2) is the real arbiter; this is the statistical screen.

Read-only; no live or Go path touched. Universe is fully CLI-parameterized.

Run (needs the trading_bot.db OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1076_directional_premise.py
    uv run --no-sync python backtest/research/regime_1076_directional_premise.py \
        --symbols BTC/USDT,ETH/USDT,SOL/USDT --timeframes 1h,4h --classifiers adx,composite
"""
from __future__ import annotations
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np

from regime import (
    compute_regime,
    compute_regime_composite,
    composite_feature_matrix,
    _DEFAULT_COMPOSITE_THRESHOLDS,
)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import forward_returns, separation, stability, per_state_significance
from regime_stats import benjamini_hochberg

# eval_windows split: "is"/"oos" are the recent forward-looking protocol windows
# (2025-06 -> 2026); the 2023/2024/2025H1 windows are historical. A durable, tradeable
# regime->direction edge must persist into the held-out forward windows, above all "oos"
# (2026-) — a state significant only in a historical window is in-sample/regime-specific
# overfit, not an edge the live policy can bank on today.
HELD_OUT_FORWARD = ("is", "oos")

DEFAULT_SYMBOLS = ("BTC/USDT", "ETH/USDT", "SOL/USDT")
DEFAULT_TIMEFRAMES = ("1h", "4h")
DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")
DEFAULT_HORIZONS = (1, 4, 8, 12, 24, 48, 72)
DEFAULT_CLASSIFIERS = ("adx", "composite")
COMPOSITE_PERIOD = 48        # matches #1073 / the live composite default lookback
ADX_PERIOD = 14              # Wilder standard for the 3-state directional classifier
ADX_THRESHOLD = 20.0         # compute_regime / _normalize_spec default


def _policy_direction(label: str) -> int:
    """The side regime_directional_policy bets for a state: +1 long, -1 short, 0 neutral."""
    if label.startswith("trending_up"):
        return +1
    if label.startswith("trending_down"):
        return -1
    return 0


def _label_stream(close_df, classifier, th):
    """Return (close array, per-bar label array, valid mask dropping warmup bars)."""
    if classifier == "composite":
        labels = compute_regime_composite(close_df, period=COMPOSITE_PERIOD,
                                          thresholds=th)["regime"].to_numpy()
        features = composite_feature_matrix(close_df, COMPOSITE_PERIOD, th).to_numpy()
        valid = ~np.isnan(features).any(axis=1)
    elif classifier == "adx":
        labels = compute_regime(close_df, period=ADX_PERIOD,
                                adx_threshold=ADX_THRESHOLD)["regime"].to_numpy()
        valid = np.ones(len(labels), dtype=bool)
        valid[:ADX_PERIOD] = False     # Wilder ADX warmup -> default 'ranging', not a real read
    else:
        raise SystemExit(f"unknown classifier {classifier!r}")
    return close_df["close"].to_numpy(), labels, valid


def _load(symbol, timeframe, window, classifier, th):
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                          start_date=start, end_date=end)
    if len(df) <= max(COMPOSITE_PERIOD, ADX_PERIOD) + 5:
        return None
    close, labels, valid = _label_stream(df, classifier, th)
    vlabels = labels[valid]
    st = stability(vlabels)
    mean_dwell = (float(np.mean(list(st["mean_dwell"].values())))
                  if st["mean_dwell"] else 1.0)
    return {"close": close, "valid": valid, "vlabels": vlabels, "mean_dwell": mean_dwell}


def run(symbols, timeframes, windows, horizons, classifiers, th, n_perm, seed):
    """Returns a flat list of per-(classifier,symbol,tf,window,horizon,state) result rows."""
    rows = []
    for classifier in classifiers:
        for symbol in symbols:
            for timeframe in timeframes:
                for window in windows:
                    d = _load(symbol, timeframe, window, classifier, th)
                    if d is None:
                        continue
                    for h in horizons:
                        fwd = forward_returns(d["close"], h)[d["valid"]]
                        if np.isnan(fwd).all():
                            continue
                        bl = max(int(3 * d["mean_dwell"]), h)
                        per_state = per_state_significance(d["vlabels"], fwd, bl,
                                                           n_perm=n_perm, seed=seed)
                        sep = separation(d["vlabels"], fwd)["per_state"]
                        for state, r in per_state.items():
                            pol = _policy_direction(state)
                            gap = float(r["gap"])
                            # candidate edge: FDR-significant AND gap sign matches policy bet.
                            # bool() casts numpy bool_ -> Python bool so the row JSON-dumps.
                            aligned = bool(pol != 0 and np.sign(gap) == pol)
                            rows.append({
                                "classifier": classifier, "symbol": symbol,
                                "timeframe": timeframe, "window": window, "horizon": int(h),
                                "state": str(state), "gap": gap,
                                "mean_fwd": float(sep.get(state, {}).get("mean", float("nan"))),
                                "p_value": float(r["p_value"]),
                                "fdr_reject": bool(r["fdr_reject"]),
                                "policy_dir": int(pol), "sign_aligned": aligned,
                                "candidate_edge": bool(r["fdr_reject"] and aligned),
                            })
    return rows


def report(rows, classifiers):
    directional = [r for r in rows if r["policy_dir"] != 0]   # trending_* states only
    n_dir = len(directional)
    n_fdr = sum(r["fdr_reject"] for r in directional)
    candidates = [r for r in directional if r["candidate_edge"]]

    print("=" * 78)
    print("PER-STATE DIRECTIONAL SIGNIFICANCE SUMMARY (#1076 scope 1)")
    print("=" * 78)
    print(f"directional-state tests (trending_up*/trending_down* only): {n_dir}")
    print(f"  FDR-significant (any sign):                {n_fdr}")
    print(f"  FDR-significant AND policy-sign-aligned:   {len(candidates)}  <- candidate edges")
    print()

    # per-classifier breakdown
    for c in classifiers:
        cr = [r for r in directional if r["classifier"] == c]
        if not cr:
            continue
        cf = sum(r["fdr_reject"] for r in cr)
        cc = sum(r["candidate_edge"] for r in cr)
        wrong = sum(r["fdr_reject"] and not r["sign_aligned"] for r in cr)
        print(f"[{c:9s}] {len(cr):4d} tests | FDR-sig {cf:3d} "
              f"(aligned {cc}, wrong-signed {wrong})")
    print()

    # GLOBAL multiple-comparisons correction. per_state_significance applies BH only
    # WITHIN a (classifier,symbol,tf,window,horizon) cell. Running ~N such cells is a
    # family of N*states tests, so within-cell "significant" hits are expected by chance.
    # The honest screen corrects across the WHOLE directional family.
    pvals = [r["p_value"] for r in directional]
    n = len(pvals)
    global_bh = benjamini_hochberg(pvals, alpha=0.05) if pvals else []
    bonf_thresh = 0.05 / n if n else 0.0
    n_global_bh = sum(global_bh)
    n_bonf = sum(p <= bonf_thresh for p in pvals)
    # aligned survivors under each global correction
    bh_aligned = sum(b and r["sign_aligned"] for b, r in zip(global_bh, directional))
    bonf_aligned = sum((r["p_value"] <= bonf_thresh) and r["sign_aligned"]
                       for r in directional)
    print("GLOBAL multiple-comparisons correction across ALL "
          f"{n} directional-state tests:")
    print(f"  Benjamini-Hochberg FDR q=0.05:  {n_global_bh:3d} survive "
          f"({bh_aligned} policy-aligned)")
    print(f"  Bonferroni  (p<= {bonf_thresh:.2e}): {n_bonf:3d} survive "
          f"({bonf_aligned} policy-aligned)")
    print()

    # Held-out-forward persistence: candidate edges that land in is/oos (2025-06->2026),
    # the windows the live policy must work in. Historical-only hits are overfit.
    held = [r for r in candidates if r["window"] in HELD_OUT_FORWARD]
    oos = [r for r in candidates if r["window"] == "oos"]
    print("Within-cell candidate edges by window class:")
    print(f"  held-out forward (is/oos): {len(held):2d}    of which oos(2026-): {len(oos):2d}")
    print(f"  historical (2023/2024/2025H1): {len(candidates) - len(held):2d}")
    print()

    if candidates:
        print("WITHIN-CELL candidate edges (FDR-sig + policy-aligned; NOT globally corrected):")
        print(f"{'clf':10s} {'sym':9s} {'tf':4s} {'win':7s} {'h':>3s} {'state':22s} "
              f"{'gap':>10s} {'mean_fwd':>10s} {'p':>7s} {'gBH':>4s}")
        print("-" * 100)
        bh_set = {id(r) for b, r in zip(global_bh, directional) if b}
        for r in sorted(candidates, key=lambda x: x["p_value"]):
            print(f"{r['classifier']:10s} {r['symbol']:9s} {r['timeframe']:4s} "
                  f"{r['window']:7s} {r['horizon']:3d} {r['state']:22s} "
                  f"{r['gap']:10.5f} {r['mean_fwd']:10.5f} {r['p_value']:7.3f} "
                  f"{'Y' if id(r) in bh_set else '.':>4s}")
        print("(gBH = survives GLOBAL Benjamini-Hochberg across the whole battery.)")
    else:
        print("NO within-cell candidate edges on any tested cell. The premise holds nowhere here.")
    print()


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1076 scope-1: regime->direction premise screen")
    p.add_argument("--symbols", default=",".join(DEFAULT_SYMBOLS))
    p.add_argument("--timeframes", default=",".join(DEFAULT_TIMEFRAMES))
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS),
                   help=f"comma-separated; known: {', '.join(WINDOWS)}")
    p.add_argument("--horizons", default=",".join(str(h) for h in DEFAULT_HORIZONS))
    p.add_argument("--classifiers", default=",".join(DEFAULT_CLASSIFIERS),
                   help="comma-separated subset of: adx, composite")
    p.add_argument("--n-perm", type=int, default=500)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--out", default="", help="optional path to dump all result rows as JSON")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    symbols = tuple(s.strip() for s in args.symbols.split(",") if s.strip())
    timeframes = tuple(t.strip() for t in args.timeframes.split(",") if t.strip())
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    horizons = tuple(int(h) for h in args.horizons.split(","))
    classifiers = tuple(c.strip() for c in args.classifiers.split(",") if c.strip())

    print(f"# universe: {list(symbols)} x {list(timeframes)} x {list(windows)}")
    print(f"# classifiers={list(classifiers)} horizons={list(horizons)} "
          f"n_perm={args.n_perm} platform={PLATFORM}\n")
    rows = run(symbols, timeframes, windows, horizons, classifiers, th,
               args.n_perm, args.seed)
    report(rows, classifiers)
    if args.out:
        import json
        with open(args.out, "w") as fh:
            json.dump({"universe": {"symbols": list(symbols), "timeframes": list(timeframes),
                                    "windows": list(windows), "horizons": list(horizons),
                                    "classifiers": list(classifiers), "n_perm": args.n_perm,
                                    "platform": PLATFORM}, "rows": rows}, fh, indent=2)
        print(f"# wrote {len(rows)} rows -> {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
