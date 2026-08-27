from __future__ import annotations
import math
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

DEFAULT_BAKEOFF_MIN_N_PERM = 1000


def bonferroni_alpha(n_candidates):
    return SIGNIFICANCE_ALPHA / max(1, int(n_candidates))


def min_n_perm_for_alpha(alpha):
    return max(1, math.ceil(1.0 / float(alpha)) - 1)


def resolve_bakeoff_n_perm(n_candidates, requested=None):
    alpha = bonferroni_alpha(n_candidates)
    floor = min_n_perm_for_alpha(alpha)
    if requested is None:
        return max(DEFAULT_BAKEOFF_MIN_N_PERM, min_n_perm_for_alpha(alpha / 2.0))
    requested = int(requested)
    if requested < floor:
        raise ValueError(
            f"n_perm={requested} cannot satisfy the Bonferroni-corrected alpha "
            f"{alpha:.6f} over {n_candidates} candidates: the minimum achievable "
            f"permutation p-value is 1/{requested + 1} ~ {1.0 / (requested + 1):.6f} "
            f"> alpha, so no candidate could ever pass. Use n_perm >= {floor}.")
    return requested


def permutation_steps_to_alpha(p_value, n_perm, alpha=SIGNIFICANCE_ALPHA):
    scale = int(n_perm) + 1
    count = int(round(float(p_value) * scale))
    limit = int(math.floor(float(alpha) * scale + 1e-9))
    return limit - count


def verdict_knife_edge(steps):
    return abs(int(steps)) <= 1


def structurally_ineligible_reason(k, thresholds):
    if int(k) < int(thresholds.min_active_labels):
        return (f"k={int(k)} can emit at most {int(k)} distinct labels < min_active_labels="
                f"{int(thresholds.min_active_labels)}: non-degeneracy is unsatisfiable")
    return None


def bonferroni_denominator(candidates):
    return sum(1 for c in candidates if not c.get("structurally_ineligible"))


def select_winner(candidates):
    if not candidates:
        return None
    alpha = bonferroni_alpha(bonferroni_denominator(candidates))
    eligible = [c for c in candidates
                if not c.get("structurally_ineligible")
                and c.get("verdict", {}).get("ship") and c.get("non_degenerate_all")
                and c.get("model_p_value") is not None and c["model_p_value"] <= alpha]
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
                k_range=range(2, 8), period=48, filter_window=64, seed=0, n_perm=None):
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    hr_streams = _handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))
    plan = [(family, k) for family in families for k in k_range]
    ineligible_reasons = {cell: structurally_ineligible_reason(cell[1], thresholds)
                          for cell in plan}
    denominator = sum(1 for cell in plan if not ineligible_reasons[cell])
    alpha = bonferroni_alpha(denominator)
    n_perm = resolve_bakeoff_n_perm(denominator, requested=n_perm)
    ineligible_report = [{"family": f, "k": k, "reason": ineligible_reasons[(f, k)]}
                         for f, k in plan if ineligible_reasons[(f, k)]]
    for entry in ineligible_report:
        print(f"NOTE: candidate {entry['family']}:k={entry['k']} is structurally ineligible "
              f"and excluded from the Bonferroni denominator — {entry['reason']}",
              file=sys.stderr)
    print(f"NOTE: n_perm={n_perm} (min achievable p {1.0 / (n_perm + 1):.6f}) vs "
          f"Bonferroni-corrected alpha {alpha:.6f} over {denominator} structurally "
          f"eligible of {len(plan)} swept candidates", file=sys.stderr)
    hr_held = run_window(symbol, timeframe, held_out, model=None, seed=seed,
                         target="volatility", n_perm=n_perm)
    hr_tr = hr_held["stability"]["transition_rate"]
    hr_p = hr_held["h4"]["significance"]["p_value"]
    start, end = WINDOWS[in_sample]
    fit_df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    fit_feats = composite_feature_matrix(fit_df, period, th).to_numpy()

    candidates = []
    for family, k in plan:
        model = rvm.fit_unsupervised(fit_feats, family=family, k=k,
                                     filter_window=filter_window, period=period,
                                     thresholds=th, seed=seed,
                                     fitted_on={"symbol": symbol, "timeframe": timeframe,
                                                "window": in_sample})
        md = run_window(symbol, timeframe, held_out, model=model, seed=seed,
                        target="volatility", n_perm=n_perm)
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
            "structurally_ineligible": bool(ineligible_reasons[(family, k)]),
            "structural_ineligibility_reason": ineligible_reasons[(family, k)],
            "states": model["states"], "mapping": model["mapping"],
        })
    for c in candidates:
        c["passes_bonferroni"] = (not c["structurally_ineligible"]
                                  and c["model_p_value"] is not None
                                  and c["model_p_value"] <= alpha)
    winner = select_winner(candidates)
    incumbent_steps = permutation_steps_to_alpha(hr_p, n_perm)
    return {
        "symbol": symbol, "timeframe": timeframe, "in_sample": in_sample,
        "held_out": held_out, "target": "volatility",
        "candidate_count": len(candidates),
        "significance_alpha": SIGNIFICANCE_ALPHA,
        "bonferroni_alpha": alpha,
        "bonferroni_denominator": denominator,
        "bonferroni_denominator_policy": (
            "structurally ineligible candidates (k < incumbent-derived min_active_labels) are "
            "scored for evidence but excluded from the family-wise denominator"),
        "structurally_ineligible": ineligible_report,
        "n_perm": int(n_perm),
        "min_achievable_p_value": 1.0 / (n_perm + 1),
        "non_degeneracy_thresholds": vars(thresholds),
        "handrule_held_out": {"kruskal_h": hr_held["h4"]["separation"]["kruskal_h"],
                              "p_value": hr_p,
                              "transition_rate": hr_tr,
                              "abstained": bool(hr_p > SIGNIFICANCE_ALPHA),
                              "permutation_steps_to_alpha": int(incumbent_steps),
                              "knife_edge": bool(verdict_knife_edge(incumbent_steps))},
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
    p.add_argument("--n-perm", type=int, default=None,
                   help="block-shuffle permutation count for the significance arm (default: "
                        "auto-resolved so the Bonferroni-corrected alpha is achievable with "
                        "headroom; explicit values below the achievability floor are rejected)")
    p.add_argument("--json", default=None, help="write the bake-off report JSON here")
    return p


def main(argv=None):
    import json
    args = build_parser().parse_args(argv)
    report = run_bakeoff(args.symbol, args.timeframe, in_sample=args.in_sample,
                         held_out=args.held_out, period=args.period,
                         filter_window=args.filter_window, seed=args.seed,
                         n_perm=args.n_perm)
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
