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

EVIDENCE STATUS (downgraded, #1095 item 4c / PR #1168): the OOS run reported below measured
the hand-rule incumbent at p=10/201~=0.0498 with n_perm=200 -- a knife-edge pass, flagged as
such by this script's own knife_edge/permutation_steps_to_alpha audit fields (added in #1160).
Re-measured at n_perm=1799 in #1095, the incumbent's OOS forward-volatility separation is NOT
significant: p=0.105 (canonical/funding/volume masks) / p=0.113 (htf/all_enriched masks),
knife_edge=false. gate_verdict's incumbent_trustworthy check already abstains on an
untrustworthy incumbent regardless of n_perm, so no runtime behavior changes -- but any reader
of this script's original evidence should not treat the p=0.0498 result as replicating.
"""
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

# Auto-resolved permutation-count floor (#1160): the corrected alpha must be ACHIEVABLE by the
# permutation statistic that is thresholded against it, with headroom — 200 permutations bottom
# out at p = 1/201 ~ 0.005 > 0.05/18, making the eligibility arm unsatisfiable by construction.
DEFAULT_BAKEOFF_MIN_N_PERM = 1000


def bonferroni_alpha(n_candidates):
    """Family-wise significance threshold for a sweep of n_candidates: SIGNIFICANCE_ALPHA split
    across every candidate independently tested against the gate's significance arm. Bounds the
    chance that SOME candidate clears significance by luck as the sweep widens (#1083)."""
    return SIGNIFICANCE_ALPHA / max(1, int(n_candidates))


def min_n_perm_for_alpha(alpha):
    """Smallest n_perm whose minimum achievable block-shuffle p-value, (0+1)/(n_perm+1),
    is <= alpha."""
    return max(1, math.ceil(1.0 / float(alpha)) - 1)


def resolve_bakeoff_n_perm(n_candidates, requested=None):
    """Pick (or validate) the permutation count so the Bonferroni-corrected alpha is achievable
    by the statistic thresholded against it (#1160). Default: at least
    DEFAULT_BAKEOFF_MIN_N_PERM and at least 2x resolution headroom below the corrected alpha
    (min achievable p <= alpha/2). An explicit request below the achievability floor raises —
    the harness must fail loudly instead of emitting a false honest-negative from a sweep whose
    eligibility arm no candidate can satisfy."""
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
    """How many additional as-or-more-extreme permutations the p-value could absorb before
    crossing alpha. 0 = knife-edge (one more extreme permutation under a different seed flips
    the verdict); negative = already above alpha."""
    scale = int(n_perm) + 1
    count = int(round(float(p_value) * scale))          # (ge+1) recovered from the reported p
    limit = int(math.floor(float(alpha) * scale + 1e-9))
    return limit - count


def verdict_knife_edge(steps):
    """True when the trustworthy/abstain verdict sits within one permutation step of alpha on
    EITHER side: steps 0 / -1 flip on a single as-or-more-extreme permutation (one added /
    one removed under a different seed), steps 1 is a single step from the boundary itself.
    A one-sided check would report an abstain-by-one incumbent as a comfortable abstain —
    understating exactly the fragility this flag exists to surface."""
    return abs(int(steps)) <= 1


def structurally_ineligible_reason(k, thresholds):
    """A k-latent candidate emits at most k distinct label names, so below the incumbent-derived
    min_active_labels floor it can NEVER pass non-degeneracy — it is still scored for evidence,
    but must not inflate the family-wise denominator (#1160). Returns None when the candidate is
    structurally eligible."""
    if int(k) < int(thresholds.min_active_labels):
        return (f"k={int(k)} can emit at most {int(k)} distinct labels < min_active_labels="
                f"{int(thresholds.min_active_labels)}: non-degeneracy is unsatisfiable")
    return None


def bonferroni_denominator(candidates):
    """Number of structurally ELIGIBLE candidates — the family-wise correction divides alpha
    only across candidates that could actually be selected. Structurally ineligible ones
    (k below the non-degeneracy floor) contribute zero false-selection probability, so counting
    them would shrink alpha without bounding anything (#1160)."""
    return sum(1 for c in candidates if not c.get("structurally_ineligible"))


def select_winner(candidates):
    """Eligible = structurally eligible AND gate ship AND non-degenerate on every eval window
    AND the model's forward-vol p-value clears the BONFERRONI-corrected threshold (alpha /
    number of structurally eligible candidates swept — see bonferroni_denominator). The gate's
    own significance arm uses the raw per-candidate alpha; across an 18+ candidate sweep
    ~1-in-20 clears it by chance, so the family-wise correction is what keeps the SELECTION's
    false-positive rate bounded. Winner = max held-out model_kruskal_h, stability_gain as
    tiebreak. Returns None when none are eligible."""
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
    # Denominator policy (#1160): every (family, k) cell is fit and scored for evidence, but the
    # family-wise correction divides alpha only across STRUCTURALLY ELIGIBLE candidates — a k
    # below the incumbent-derived min_active_labels floor can never pass non-degeneracy, so it
    # contributes zero false-selection probability. Never silent: ineligible cells are logged to
    # stderr and stamped into the report.
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
                              # Knife-edge visibility (#1160): additional as-or-more-extreme
                              # permutations the incumbent p could absorb before the
                              # trustworthy/abstain verdict flips (0 = next one flips it;
                              # negative = already abstained, -1 by a single permutation).
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
