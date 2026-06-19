"""Reproducible evidence for #1073: the 7-state composite regime classifier has no
statistically real forward-RETURN (directional) separation to beat, but does strongly
separate forward VOLATILITY (its real job).

Three diagnostics, all reusing backtest/regime_diagnostics.py scorers against the
eval_windows splits:

  windows   per-window hand-rule forward-return separation + block-shuffle significance,
            alongside the in-sample k-means yardstick (model-free ceiling on the 4 features).
  horizons  hand-rule forward-return significance swept across horizons {1..72} x windows.
            (Finding 1: 0/35 block-shuffle significant -> no directional signal at any horizon.)
  vol       (A) hand-rule forward-VOLATILITY separation + significance -> strong & real.
            (B) in-sample-overfit k-means on the same 4 features, block-shuffle significance
                on forward RETURNS -> still fails (Finding 2: not feature poverty).

Run (needs the trading_bot.db OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1073_directional_negative_result.py
    uv run --no-sync python backtest/research/regime_1073_directional_negative_result.py \
        --symbol ETH/USDT --timeframe 1h --diagnostics horizons

Parameterized by --symbol/--timeframe so #1076 (multi-asset directional-premise validation)
can reuse it. Read-only; no live or Go path touched.
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
    compute_regime_composite,
    composite_feature_matrix,
    _DEFAULT_COMPOSITE_THRESHOLDS,
)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import (
    forward_returns,
    separation,
    stability,
    block_shuffle_pvalue,
    kmeans_yardstick,
    _kmeans,
)

DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")
DEFAULT_HORIZONS = (1, 4, 8, 12, 24, 48, 72)
PERIOD = 48


def _load(symbol, timeframe, window, thresholds):
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                          start_date=start, end_date=end)
    features = composite_feature_matrix(df, PERIOD, thresholds).to_numpy()
    labels = compute_regime_composite(df, period=PERIOD,
                                      thresholds=thresholds)["regime"].to_numpy()
    valid = ~np.isnan(features).any(axis=1)
    st = stability(labels[valid])
    mean_dwell = (float(np.mean(list(st["mean_dwell"].values())))
                  if st["mean_dwell"] else 1.0)
    return {
        "close": df["close"].to_numpy(),
        "features": features,
        "valid": valid,
        "vlabels": labels[valid],
        "mean_dwell": mean_dwell,
        "transition_rate": st["transition_rate"],
    }


def _fwd_realized_vol(close, h):
    log_ret = np.diff(np.log(close), prepend=np.log(close[0]))
    out = np.full(len(close), np.nan)
    for i in range(len(close) - h):
        out[i] = np.sqrt(np.sum(log_ret[i + 1: i + 1 + h] ** 2))
    return out


def diag_windows(symbol, timeframe, windows, th, horizon=4):
    print(f"=== per-window hand-rule forward-RETURN separation (h{horizon}) + "
          f"k-means feature ceiling ===")
    print(f"{'window':8s} {'n':>6s} {'HR_KWH':>8s} {'HR_p':>6s} {'sig?':>5s} | "
          f"{'km_best_k':>9s} {'km_KWH':>8s} | {'HR_tr':>6s}")
    print("-" * 72)
    for w in windows:
        d = _load(symbol, timeframe, w, th)
        fwd = forward_returns(d["close"], horizon)[d["valid"]]
        bl = max(int(3 * d["mean_dwell"]), horizon)
        sig = block_shuffle_pvalue(d["vlabels"], fwd, bl, seed=0)
        yd = kmeans_yardstick(d["features"], forward_returns(d["close"], horizon), seed=0)
        best_k = max(yd, key=lambda k: yd[k]["kruskal_h"])
        flag = "*" if sig["p_value"] <= 0.05 else " "
        print(f"{w:8s} {len(d['vlabels']):6d} {sig['kruskal_h']:8.2f} "
              f"{sig['p_value']:6.3f} {flag:>5s} | {best_k:9d} "
              f"{yd[best_k]['kruskal_h']:8.2f} | {d['transition_rate']:6.3f}")
    print("(k-means yardstick is IN-SAMPLE, no significance test -> overfit ceiling; "
          "see the 'vol' diagnostic Test B for its real significance.)\n")


def diag_horizons(symbol, timeframe, windows, horizons, th):
    print("=== hand-rule forward-RETURN significance swept across horizons "
          "(* = block-shuffle p<=0.05) ===")
    header = "window  " + "".join(f"  h{h:<3d}(p)  " for h in horizons)
    print(header)
    print("-" * len(header))
    n_sig = 0
    for w in windows:
        d = _load(symbol, timeframe, w, th)
        row = f"{w:7s} "
        for h in horizons:
            fwd = forward_returns(d["close"], h)[d["valid"]]
            bl = max(int(3 * d["mean_dwell"]), h)
            sig = block_shuffle_pvalue(d["vlabels"], fwd, bl, seed=0)
            flag = "*" if sig["p_value"] <= 0.05 else " "
            n_sig += sig["p_value"] <= 0.05
            row += f" {sig['kruskal_h']:5.1f}/{sig['p_value']:.2f}{flag}"
        print(row)
    print(f"\n{n_sig}/{len(windows) * len(horizons)} (window x horizon) tests "
          f"block-shuffle significant.\n")


def diag_vol(symbol, timeframe, windows, th):
    print("=== TEST A: hand-rule forward-VOLATILITY separation (realized vol) ===")
    print(f"{'window':8s} | {'volH4_KWH':>10s} {'p':>7s} | {'volH24_KWH':>11s} {'p':>7s}")
    print("-" * 54)
    loaded = {}
    for w in windows:
        d = _load(symbol, timeframe, w, th)
        loaded[w] = d
        row = f"{w:8s} |"
        for h in (4, 24):
            fv = _fwd_realized_vol(d["close"], h)[d["valid"]]
            bl = max(int(3 * d["mean_dwell"]), h)
            sig = block_shuffle_pvalue(d["vlabels"], fv, bl, seed=0)
            flag = "*" if sig["p_value"] <= 0.05 else " "
            row += f" {sig['kruskal_h']:10.1f} {sig['p_value']:.3f}{flag} |"
        print(row)

    print("\n=== TEST B: is the k-means feature 'ceiling' real? "
          "(in-sample fit, block-shuffle significance on forward RETURNS, h4) ===")
    print(f"{'window':8s} | {'k':>2s} {'km_KWH':>8s} {'blkshuf_p':>10s}  verdict")
    print("-" * 50)
    for w in windows:
        d = loaded[w]
        fwd_full = forward_returns(d["close"], 4)
        mask = ~np.isnan(d["features"]).any(1) & ~np.isnan(fwd_full)
        x, fr = d["features"][mask], fwd_full[mask]
        mean, std = x.mean(0), x.std(0)
        std[std < 1e-8] = 1.0
        z = (x - mean) / std
        k = 7
        cl = _kmeans(z, k, 0)
        bl = max(int(3 * d["mean_dwell"]), 4)
        sig = block_shuffle_pvalue(cl.astype(object), fr, bl, seed=0)
        verdict = "REAL" if sig["p_value"] <= 0.05 else "overfit/noise"
        print(f"{w:8s} | {k:2d} {sig['kruskal_h']:8.1f} "
              f"{sig['p_value']:10.3f}  {verdict}")
    print()


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1073 directional-separation negative-result evidence")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--windows", default=",".join(DEFAULT_WINDOWS),
                   help=f"comma-separated; known: {', '.join(WINDOWS)}")
    p.add_argument("--horizons", default=",".join(str(h) for h in DEFAULT_HORIZONS))
    p.add_argument("--diagnostics", default="windows,horizons,vol",
                   help="comma-separated subset of: windows, horizons, vol")
    return p


def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    windows = tuple(w.strip() for w in args.windows.split(",") if w.strip())
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f"unknown window {w}; known: {list(WINDOWS)}")
    horizons = tuple(int(h) for h in args.horizons.split(","))
    wanted = [d.strip() for d in args.diagnostics.split(",") if d.strip()]

    print(f"# {args.symbol} {args.timeframe}  (period={PERIOD}, platform={PLATFORM})\n")
    if "windows" in wanted:
        diag_windows(args.symbol, args.timeframe, windows, th)
    if "horizons" in wanted:
        diag_horizons(args.symbol, args.timeframe, windows, horizons, th)
    if "vol" in wanted:
        diag_vol(args.symbol, args.timeframe, windows, th)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
