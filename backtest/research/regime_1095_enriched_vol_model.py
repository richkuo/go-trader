"""Reproducible evidence for #1095: extend the #1080 unsupervised vol-regime bake-off to fit on an
ENRICHED feature matrix — the four canonical hand-rule inputs PLUS signals the hand-rule ignores
(funding rate, volume z-score, a higher-timeframe range_eff) — and report whether any candidate now
clears the same forward-volatility gate without degenerating, plus a feature-subset ABLATION that
attributes any separation gain to a specific added feature.

Why: the #1080 negative result (no unsupervised model beats the hand-rule) was produced fitting
every candidate on exactly the hand-rule's OWN four features. Clustering can only find structure in
its inputs, so that result is partly a consequence of the input set. This harness gives the models a
genuine chance by feeding extra causal signals, then re-runs the unchanged #1080 gate + anti-gaming
guards (forward-volatility separation, non-degeneracy locked from the hand-rule's worst window,
Bonferroni-corrected selection across the WHOLE sweep, now including the ablation arms).

Fairness: each subset's enriched matrix has its own NaN warmup (funding / HTF / volume), so the
candidate AND the hand-rule baseline are BOTH scored on that subset's identical valid-bar mask —
gate_verdict then compares like-for-like retained bars. Non-degeneracy thresholds stay locked from
the CANONICAL hand-rule (the incumbent the candidate must beat).

Run (needs the OHLCV cache; funding cache/network optional — funding-bearing subsets that can't be
built are reported "unavailable", never fit on nothing):

    uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py
    uv run --no-sync python backtest/research/regime_1095_enriched_vol_model.py \
        --symbol ETH/USDT --timeframe 1h --json /tmp/regime_1095_eth.json

Parameterized by --symbol/--timeframe (so #1083 multi-asset can reuse it). Read-only; no live or Go
path touched. Live wiring of an enriched model is #1074, not solved here (see LIVE_WIRING_DELTA).
"""
from __future__ import annotations
import os, sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import importlib.util

import numpy as np

from regime import (compute_regime_composite, composite_feature_matrix,
                    _DEFAULT_COMPOSITE_THRESHOLDS)
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import score_labels
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA
import regime_vol_model as rvm
from regime_enriched_features import (CANONICAL_COLUMNS, ENRICHED_COLUMNS, ENRICHED_EXTRA_COLUMNS,
                                      LIVE_WIRING_DELTA, enriched_feature_matrix,
                                      canonical_indices_for, decode_with_model)

DEFAULT_WINDOWS = ("is", "oos", "2023", "2024", "2025H1")
MIN_VALID_BARS = 50  # below this a subset's matrix is too warmed-out to fit/score -> "unavailable"

# Feature subsets swept. Every subset keeps the canonical four FIRST so states still name from them.
# "canonical" reproduces #1080 as a sanity arm; each single-feature arm isolates one added signal;
# "all_enriched" stacks them. The ablation reads separation gain across these arms.
SUBSETS = {
    "canonical": list(CANONICAL_COLUMNS),
    "funding": CANONICAL_COLUMNS + ["funding_rate"],
    "volume": CANONICAL_COLUMNS + ["volume_z"],
    "htf": CANONICAL_COLUMNS + ["htf_range_eff"],
    "all_enriched": list(ENRICHED_COLUMNS),
}


def _load_1080():
    """Reuse #1080's select_winner + bonferroni_alpha (SSoT for the family-wise correction) and its
    canonical hand-rule streams for the non-degeneracy thresholds."""
    path = os.path.join(_THIS_DIR, "regime_1080_unsupervised_vol_model.py")
    spec = importlib.util.spec_from_file_location("regime_1080_for_1095", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _funding_for_window(coin, window):
    """Hyperliquid funding history covering a window; None on any failure (no cache/network in CI)
    so funding-bearing subsets degrade to 'unavailable' rather than crashing the sweep."""
    try:
        from funding_fetcher import load_cached_funding
        start, end = WINDOWS[window]
        return load_cached_funding(coin, start, end)
    except Exception:  # noqa: BLE001 — research harness must not die on a funding miss
        return None


def _enriched(symbol, timeframe, window, period, th, columns, funding, *, htf_multiple, vol_window):
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                          start_date=WINDOWS[window][0], end_date=WINDOWS[window][1])
    mat = enriched_feature_matrix(df, period, th, funding=funding, vol_window=vol_window,
                                  htf_multiple=htf_multiple, columns=columns)
    return df, mat


def _score(df, labels, mat, target="volatility"):
    """Score a label stream on the enriched subset's mask (mat carries the NaN warmup that defines
    which bars both arms are judged on)."""
    return score_labels(df["close"].to_numpy(), labels, mat.to_numpy(dtype=float), target=target)


def _valid_count(mat):
    arr = mat.to_numpy(dtype=float)
    return int((~np.isnan(arr).any(axis=1)).sum())


def run_bakeoff(symbol="BTC/USDT", timeframe="1h", *, in_sample="is", held_out="oos",
                eval_windows=DEFAULT_WINDOWS, families=("hmm", "gmm", "kmeans"),
                k_range=range(2, 8), subsets=None, period=48, filter_window=64,
                htf_multiple=4, vol_window=None, seed=0):
    subsets = dict(subsets) if subsets is not None else dict(SUBSETS)
    m1080 = _load_1080()
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    coin = symbol.split("/")[0]

    # Non-degeneracy thresholds: locked from the CANONICAL incumbent's worst window (anti-gaming),
    # identical reference for every subset (#1080's helper).
    hr_streams = m1080._handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))

    funding_by_window = {w: _funding_for_window(coin, w) for w in eval_windows}

    candidates = []
    subset_status = {}
    for sub_name, columns in subsets.items():
        cidx = canonical_indices_for(columns)
        needs_funding = "funding_rate" in columns
        # Build the held-out + fit matrices for this subset once.
        fit_df, fit_mat = _enriched(symbol, timeframe, in_sample, period, th, columns,
                                    funding_by_window.get(in_sample),
                                    htf_multiple=htf_multiple, vol_window=vol_window)
        held_df, held_mat = _enriched(symbol, timeframe, held_out, period, th, columns,
                                      funding_by_window.get(held_out),
                                      htf_multiple=htf_multiple, vol_window=vol_window)
        if _valid_count(fit_mat) < MIN_VALID_BARS or _valid_count(held_mat) < MIN_VALID_BARS:
            subset_status[sub_name] = ("unavailable: too few valid bars after warmup"
                                       + (" (funding cache/network missing?)" if needs_funding else ""))
            continue
        subset_status[sub_name] = "ok"
        # Hand-rule baseline scored on THIS subset's held-out mask (fair like-for-like).
        hr_labels = compute_regime_composite(held_df, period=period, thresholds=th)["regime"].to_numpy()
        hr_held = _score(held_df, hr_labels, held_mat)
        hr_tr = hr_held["stability"]["transition_rate"]

        fit_feats = fit_mat.to_numpy(dtype=float)
        for family in families:
            for k in k_range:
                model = rvm.fit_unsupervised(fit_feats, family=family, k=k,
                                             filter_window=filter_window, period=period,
                                             thresholds=th, seed=seed,
                                             feature_names=columns, canonical_indices=cidx,
                                             fitted_on={"symbol": symbol, "timeframe": timeframe,
                                                        "window": in_sample, "subset": sub_name})
                labels, _ = decode_with_model(held_mat, model)
                md = _score(held_df, labels, held_mat)
                verdict = gate_verdict(hr_held, md)
                nd = {}
                for w in eval_windows:
                    wdf, wmat = _enriched(symbol, timeframe, w, period, th, columns,
                                          funding_by_window.get(w),
                                          htf_multiple=htf_multiple, vol_window=vol_window)
                    wlabels, _ = decode_with_model(wmat, model)
                    valid = ~np.isnan(wmat.to_numpy(dtype=float)).any(axis=1)
                    nd[w] = rvm.non_degeneracy(np.asarray(wlabels, dtype=object)[valid], thresholds)
                candidates.append({
                    "subset": sub_name, "columns": list(columns), "family": family, "k": k,
                    "verdict": verdict,
                    "model_kruskal_h": md["h4"]["separation"]["kruskal_h"],
                    "model_p_value": md["h4"]["significance"]["p_value"],
                    "handrule_kruskal_h": hr_held["h4"]["separation"]["kruskal_h"],
                    "stability_gain": float(hr_tr - md["stability"]["transition_rate"]),
                    "coverage": md["coverage"],
                    "non_degeneracy": {w: nd[w] for w in eval_windows},
                    "non_degenerate_all": all(nd[w]["ok"] for w in eval_windows),
                    "states": model["states"], "mapping": model["mapping"],
                })

    alpha = m1080.bonferroni_alpha(len(candidates)) if candidates else SIGNIFICANCE_ALPHA
    for c in candidates:
        c["passes_bonferroni"] = (c["model_p_value"] is not None and c["model_p_value"] <= alpha)
    winner = m1080.select_winner(candidates)

    # Ablation: best held-out separation per subset among non-degenerate candidates, and the gain
    # over the canonical arm — attributes any improvement to the added feature(s).
    ablation = {}
    for sub_name in subsets:
        elig = [c for c in candidates if c["subset"] == sub_name and c["non_degenerate_all"]]
        best = max(elig, key=lambda c: c["model_kruskal_h"], default=None)
        ablation[sub_name] = {
            "status": subset_status.get(sub_name, "unavailable"),
            "best_kruskal_h": (best["model_kruskal_h"] if best else None),
            "best_candidate": ({"family": best["family"], "k": best["k"]} if best else None),
            "any_ships": any(c["subset"] == sub_name and c["verdict"].get("ship")
                             and c["non_degenerate_all"] and c["passes_bonferroni"]
                             for c in candidates),
        }
    canon_best = ablation.get("canonical", {}).get("best_kruskal_h")
    for sub_name, info in ablation.items():
        info["separation_gain_vs_canonical"] = (
            (info["best_kruskal_h"] - canon_best)
            if (info["best_kruskal_h"] is not None and canon_best is not None) else None)

    return {
        "issue": 1095, "symbol": symbol, "timeframe": timeframe, "in_sample": in_sample,
        "held_out": held_out, "target": "volatility", "htf_multiple": htf_multiple,
        "candidate_count": len(candidates),
        "significance_alpha": SIGNIFICANCE_ALPHA, "bonferroni_alpha": alpha,
        "non_degeneracy_thresholds": vars(thresholds),
        "subset_status": subset_status,
        "ablation": ablation,
        "candidates": candidates,
        "winner": ({"subset": winner["subset"], "family": winner["family"], "k": winner["k"]}
                   if winner else None),
        "live_wiring_delta": LIVE_WIRING_DELTA,
    }


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1095 enriched unsupervised vol-regime bake-off")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--in-sample", default="is")
    p.add_argument("--held-out", default="oos")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--htf-multiple", type=int, default=4)
    p.add_argument("--vol-window", type=int, default=None, help="volume z-score window (default: period)")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write the bake-off report JSON here")
    return p


def main(argv=None):
    import json
    args = build_parser().parse_args(argv)
    report = run_bakeoff(args.symbol, args.timeframe, in_sample=args.in_sample,
                         held_out=args.held_out, period=args.period,
                         filter_window=args.filter_window, htf_multiple=args.htf_multiple,
                         vol_window=args.vol_window, seed=args.seed)
    text = json.dumps(report, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    w = report["winner"]
    if w:
        print(f"\nWINNER: {w}", file=sys.stderr)
    else:
        print("\nWINNER: none eligible (no gate-passing, non-degenerate, Bonferroni-clearing "
              "candidate on this window) — second negative result; see 'ablation' for whether any "
              "added feature improved separation even without shipping.", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
