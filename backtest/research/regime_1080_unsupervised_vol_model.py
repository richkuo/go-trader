"""Reproducible evidence for #1080: a genuinely UNSUPERVISED volatility-state regime model
(HMM/GMM/k-means candidates) learned from the composite feature matrix, mapped to the 7
composite labels, selected by a walk-forward bake-off against the #1078 forward-volatility gate.

Each candidate (family x latent-count K) is fit on the in-sample window, decoded causally on
the held-out window, scored on forward VOLATILITY separation + stability, run through
regime_calibrate.gate_verdict, and checked for non-degeneracy on every eval window. The winner
is the highest held-out separation among gate-passing, non-degenerate candidates.

Run (needs the OHLCV cache reachable from shared_tools/):

    uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py
    uv run --no-sync python backtest/research/regime_1080_unsupervised_vol_model.py \
        --symbol ETH/USDT --timeframe 1h --json /tmp/regime_1080_eth.json

Parameterized by --symbol/--timeframe so #1083 (multi-asset) can reuse it. Read-only;
no live or Go path touched. Economic payoff (vs flat-ATR) is #1081, not decided here.
"""
from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
from regime import (compute_regime_composite, composite_feature_matrix,
                    _DEFAULT_COMPOSITE_THRESHOLDS)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import run_window
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA
from regime_hmm import forward_filter_labels
import regime_vol_model as rvm

DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")


def select_winner(candidates):
    """Eligible = gate ship AND non-degenerate on every eval window. Winner = max held-out
    model_kruskal_h, stability_gain as tiebreak. Returns None when none are eligible."""
    eligible = [c for c in candidates
                if c.get("verdict", {}).get("ship") and c.get("non_degenerate_all")]
    if not eligible:
        return None
    return max(eligible, key=lambda c: (c["model_kruskal_h"], c["stability_gain"]))


def _handrule_streams(symbol, timeframe, eval_windows, period, th):
    streams = {}
    for w in eval_windows:
        start, end = WINDOWS[w]
        df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                              start_date=start, end_date=end)
        feats = composite_feature_matrix(df, period, th).to_numpy()
        valid = ~np.isnan(feats).any(1)
        labels = compute_regime_composite(df, period=period, thresholds=th)["regime"].to_numpy()
        streams[w] = labels[valid]
    return streams


def _model_label_stream(symbol, timeframe, window, model, period, th):
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    valid = ~np.isnan(feats).any(1)
    labels, _ = forward_filter_labels(feats, model)
    return np.asarray(labels, dtype=object)[valid]


def run_bakeoff(symbol="BTC/USDT", timeframe="1h", *, in_sample="is", held_out="oos",
                eval_windows=DEFAULT_WINDOWS, families=("hmm", "gmm", "kmeans"),
                k_range=range(2, 8), period=48, filter_window=64, seed=0):
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    hr_streams = _handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))
    hr_held = run_window(symbol, timeframe, held_out, model=None, seed=seed, target="volatility")
    hr_tr = hr_held["stability"]["transition_rate"]
    start, end = WINDOWS[in_sample]
    fit_df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    fit_feats = composite_feature_matrix(fit_df, period, th).to_numpy()

    candidates = []
    for family in families:
        for k in k_range:
            model = rvm.fit_unsupervised(fit_feats, family=family, k=k,
                                         filter_window=filter_window, period=period,
                                         thresholds=th, seed=seed,
                                         fitted_on={"symbol": symbol, "timeframe": timeframe,
                                                    "window": in_sample})
            md = run_window(symbol, timeframe, held_out, model=model, seed=seed, target="volatility")
            verdict = gate_verdict(hr_held, md)
            nd = {w: rvm.non_degeneracy(_model_label_stream(symbol, timeframe, w, model, period, th),
                                        thresholds) for w in eval_windows}
            candidates.append({
                "family": family, "k": k, "verdict": verdict,
                "model_kruskal_h": md["h4"]["separation"]["kruskal_h"],
                "model_p_value": md["h4"]["significance"]["p_value"],
                "stability_gain": float(hr_tr - md["stability"]["transition_rate"]),
                "coverage": md["coverage"],
                "non_degeneracy": {w: nd[w] for w in eval_windows},
                "non_degenerate_all": all(nd[w]["ok"] for w in eval_windows),
                "states": model["states"], "mapping": model["mapping"],
            })
    winner = select_winner(candidates)
    return {
        "symbol": symbol, "timeframe": timeframe, "in_sample": in_sample,
        "held_out": held_out, "target": "volatility",
        "non_degeneracy_thresholds": vars(thresholds),
        "handrule_held_out": {"kruskal_h": hr_held["h4"]["separation"]["kruskal_h"],
                              "p_value": hr_held["h4"]["significance"]["p_value"],
                              "transition_rate": hr_tr,
                              "abstained": bool(hr_held["h4"]["significance"]["p_value"]
                                                > SIGNIFICANCE_ALPHA)},
        "candidates": candidates,
        "winner": ({"family": winner["family"], "k": winner["k"]} if winner else None),
    }


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1080 unsupervised vol-regime bake-off")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--in-sample", default="is")
    p.add_argument("--held-out", default="oos")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write the bake-off report JSON here")
    return p


def main(argv=None):
    import json
    args = build_parser().parse_args(argv)
    report = run_bakeoff(args.symbol, args.timeframe, in_sample=args.in_sample,
                         held_out=args.held_out, period=args.period,
                         filter_window=args.filter_window, seed=args.seed)
    text = json.dumps(report, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    w = report["winner"]
    print(f"\nWINNER: {w}" if w else "\nWINNER: none eligible (no gate-passing, "
          "non-degenerate candidate on this window)", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
