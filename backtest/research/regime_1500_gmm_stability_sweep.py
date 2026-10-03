from __future__ import annotations
import os, sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, ".."))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import importlib.util
import json
import time

import numpy as np

from regime import compute_regime_composite, _DEFAULT_COMPOSITE_THRESHOLDS
from regime_calibrate import gate_verdict, STABILITY_MIN_GAIN
from regime_diagnostics import forward_realized_vol, separation
from regime_hmm import DECODE_MIN_DWELL_KEY, DECODE_STICKINESS_KEY
import regime_vol_model as rvm
from regime_enriched_features import canonical_indices_for, decode_with_model

PRIMARY = ("volume", "gmm", 5)
CONTEXT = (("volume", "hmm", 7), ("volume", "gmm", 6), ("volume", "gmm", 7),
           ("htf", "hmm", 7), ("all_enriched", "kmeans", 5), ("all_enriched", "kmeans", 6),
           ("all_enriched", "kmeans", 7), ("all_enriched", "gmm", 7), ("all_enriched", "hmm", 6))
FILTER_WINDOWS = (8, 16, 32, 64, 128, 256, 512)
DWELLS = (2, 3, 4, 6, 8, 12, 24)
STICKINESS = (1.0, 2.0, 5.0, 10.0, 20.0, 50.0)
COMBOS = ((256, 0, 0.0), (256, 3, 0.0), (256, 6, 0.0), (64, 3, 5.0), (64, 3, 20.0),
          (64, 6, 5.0), (64, 6, 20.0), (256, 3, 20.0), (256, 6, 20.0),
          (64, 0, 100.0), (64, 0, 200.0), (64, 0, 500.0), (64, 0, 1000.0),
          (64, 2, 1.0), (64, 2, 2.0), (64, 2, 5.0), (64, 2, 10.0), (64, 2, 20.0), (64, 2, 50.0),
          (64, 2, 100.0), (64, 3, 1.0), (64, 3, 2.0), (64, 3, 3.0), (64, 3, 10.0))
CONTEXT_SETTINGS = ((16, 0, 0.0), (256, 0, 0.0), (64, 3, 0.0), (64, 6, 0.0),
                    (64, 0, 5.0), (64, 0, 20.0), (64, 6, 20.0))
PRIMARY_HORIZON = 4


def _load(name, filename):
    path = os.path.join(_THIS_DIR, filename)
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def primary_settings():
    seen, out = set(), []
    for fw in FILTER_WINDOWS:
        out.append((fw, 0, 0.0))
    for d in DWELLS:
        out.append((64, d, 0.0))
    for s in STICKINESS:
        out.append((64, 0, s))
    out.extend(COMBOS)
    uniq = []
    for st in out:
        if st not in seen:
            seen.add(st)
            uniq.append(st)
    return uniq


def apply_setting(model, setting):
    fw, dwell, stick = setting
    out = dict(model)
    out["filter_window"] = int(fw)
    out.pop(DECODE_MIN_DWELL_KEY, None)
    out.pop(DECODE_STICKINESS_KEY, None)
    if dwell:
        out[DECODE_MIN_DWELL_KEY] = int(dwell)
    if stick:
        out[DECODE_STICKINESS_KEY] = float(stick)
    return out


def setting_name(setting):
    fw, dwell, stick = setting
    parts = [f"fw={fw}"]
    if dwell:
        parts.append(f"dwell={dwell}")
    if stick:
        parts.append(f"stick={stick:g}")
    return " ".join(parts)


def held_out_h(df, labels, mat, horizon=PRIMARY_HORIZON):
    arr = mat.to_numpy(dtype=float)
    valid = ~np.isnan(arr).any(axis=1)
    fwd = forward_realized_vol(df["close"].to_numpy(), horizon)
    return separation(np.asarray(labels, dtype=object)[valid], fwd[valid])["kruskal_h"]


def evaluate_setting(model, setting, wins, eval_windows, held_out, thresholds):
    m = apply_setting(model, setting)
    per_window = {}
    for w in eval_windows:
        wdf, wmat = wins[w]
        labels, _ = decode_with_model(wmat, m)
        valid = ~np.isnan(wmat.to_numpy(dtype=float)).any(axis=1)
        nd = rvm.non_degeneracy(np.asarray(labels, dtype=object)[valid], thresholds)
        per_window[w] = nd
        if w == held_out:
            per_window[w]["kruskal_h"] = float(held_out_h(wdf, labels, wmat))
    return {
        "setting": {"filter_window": setting[0], DECODE_MIN_DWELL_KEY: setting[1],
                    DECODE_STICKINESS_KEY: setting[2]},
        "label": setting_name(setting),
        "windows": per_window,
        "non_degenerate_all": all(per_window[w]["ok"] for w in eval_windows),
        "held_out_transition_rate": per_window[held_out]["transition_rate"],
        "held_out_kruskal_h": per_window[held_out]["kruskal_h"],
        "min_active_labels": min(per_window[w]["active_labels"] for w in eval_windows),
        "min_transition_rate": min(per_window[w]["transition_rate"] for w in eval_windows),
    }


def reaches_band(row, hr_rate):
    return row["non_degenerate_all"] and (hr_rate - row["held_out_transition_rate"]) >= STABILITY_MIN_GAIN


def best_reachable(rows, hr_rate):
    in_band = [r for r in rows if reaches_band(r, hr_rate)]
    if in_band:
        return max(in_band, key=lambda r: r["held_out_kruskal_h"]), True
    nd = [r for r in rows if r["non_degenerate_all"]]
    pool = nd if nd else rows
    return min(pool, key=lambda r: r["held_out_transition_rate"]), False


def run(symbol="BTC/USDT", timeframe="1h", *, in_sample="is", held_out="oos",
        eval_windows=("is", "oos", "2023", "2024", "2025H1"), period=48, base_filter_window=64,
        htf_multiple=4, vol_window=None, seed=0, n_perm=None, context=True):
    m1095 = _load("regime_1095_for_1500", "regime_1095_enriched_vol_model.py")
    m1080 = m1095._load_1080()
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    coin = symbol.split("/")[0]

    hr_streams = m1080._handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))
    all_windows = list(dict.fromkeys((in_sample, held_out, *eval_windows)))
    funding_by_window = {w: m1095._funding_for_window(coin, w) for w in all_windows}

    plan, ineligible, denominator = m1095.combined_family_plan(
        list(m1095.SUBSETS), ("hmm", "gmm", "kmeans"), range(2, 8), thresholds,
        ineligible_reason_fn=m1080.structurally_ineligible_reason)
    alpha = m1080.bonferroni_alpha(denominator)
    n_perm = m1080.resolve_bakeoff_n_perm(denominator, requested=n_perm)
    print(f"NOTE: n_perm={n_perm} alpha={alpha:.6f} denominator={denominator} "
          f"thresholds={vars(thresholds)}", file=sys.stderr)

    targets = [PRIMARY] + (list(CONTEXT) if context else [])
    needed = sorted({t[0] for t in targets})
    built = {}
    for sub_name in needed:
        columns = m1095.SUBSETS[sub_name]
        built[sub_name] = {w: m1095._enriched(symbol, timeframe, w, period, th, columns,
                                              funding_by_window[w], htf_multiple=htf_multiple,
                                              vol_window=vol_window) for w in all_windows}

    handrule = {}
    for sub_name in needed:
        held_df, held_mat = built[sub_name][held_out]
        hr_labels = compute_regime_composite(held_df, period=period, thresholds=th)["regime"].to_numpy()
        t0 = time.time()
        handrule[sub_name] = m1095._score(held_df, hr_labels, held_mat, n_perm=n_perm, seed=seed)
        print(f"NOTE: hand-rule {sub_name} scored in {time.time() - t0:.0f}s "
              f"(H={handrule[sub_name]['h4']['separation']['kruskal_h']:.2f}, "
              f"rate={handrule[sub_name]['stability']['transition_rate']:.4f})", file=sys.stderr)

    report = {"issue": 1500, "symbol": symbol, "timeframe": timeframe, "in_sample": in_sample,
              "held_out": held_out, "eval_windows": list(eval_windows),
              "base_filter_window": base_filter_window, "n_perm": int(n_perm),
              "bonferroni_alpha": alpha, "bonferroni_denominator": denominator,
              "non_degeneracy_thresholds": vars(thresholds),
              "handrule_held_out": {s: {"kruskal_h": h["h4"]["separation"]["kruskal_h"],
                                        "p_value": h["h4"]["significance"]["p_value"],
                                        "transition_rate": h["stability"]["transition_rate"]}
                                    for s, h in handrule.items()},
              "candidates": []}

    for sub_name, family, k in targets:
        wins = built[sub_name]
        fit_df, fit_mat = wins[in_sample]
        held_df, held_mat = wins[held_out]
        columns = list(fit_mat.columns)
        model = rvm.fit_unsupervised(fit_mat.to_numpy(dtype=float), family=family, k=k,
                                     filter_window=base_filter_window, period=period,
                                     thresholds=th, seed=seed, feature_names=columns,
                                     canonical_indices=canonical_indices_for(columns),
                                     fitted_on={"symbol": symbol, "timeframe": timeframe,
                                                "window": in_sample, "subset": sub_name})
        hr_rate = handrule[sub_name]["stability"]["transition_rate"]
        is_primary = (sub_name, family, k) == PRIMARY
        settings = primary_settings() if is_primary else list(CONTEXT_SETTINGS)
        settings = list(dict.fromkeys([(base_filter_window, 0, 0.0)] + settings))
        rows = []
        for st in settings:
            t0 = time.time()
            row = evaluate_setting(model, st, wins, eval_windows, held_out, thresholds)
            row["reaches_band"] = reaches_band(row, hr_rate)
            rows.append(row)
            print(f"{sub_name}:{family}:k={k} {row['label']:<28} oos_rate={row['held_out_transition_rate']:.4f} "
                  f"min_active={row['min_active_labels']} min_rate={row['min_transition_rate']:.4f} "
                  f"H={row['held_out_kruskal_h']:.2f} nd_all={row['non_degenerate_all']} "
                  f"band={row['reaches_band']} ({time.time() - t0:.0f}s)", file=sys.stderr)
        best, in_band = best_reachable(rows, hr_rate)
        entry = {"subset": sub_name, "family": family, "k": k, "primary": is_primary,
                 "states": model["states"], "naming": model["naming"],
                 "transition_diagonal": [float(model["transition"][i][i]) for i in range(k)],
                 "handrule_transition_rate": hr_rate, "sweep": rows,
                 "best_setting": best["setting"], "best_label": best["label"],
                 "best_reaches_band": in_band}
        if is_primary or in_band:
            best_model = apply_setting(model, tuple(best["setting"][key] for key in
                                                    ("filter_window", DECODE_MIN_DWELL_KEY,
                                                     DECODE_STICKINESS_KEY)))
            labels, _ = decode_with_model(held_mat, best_model)
            t0 = time.time()
            md = m1095._score(held_df, labels, held_mat, n_perm=n_perm, seed=seed)
            verdict = gate_verdict(handrule[sub_name], md)
            p = md["h4"]["significance"]["p_value"]
            entry["rescored"] = {
                "verdict": verdict, "model_kruskal_h": md["h4"]["separation"]["kruskal_h"],
                "model_p_value": p, "passes_bonferroni": bool(p <= alpha),
                "stability_gain": float(hr_rate - md["stability"]["transition_rate"]),
                "non_degenerate_all": best["non_degenerate_all"],
                "coverage": md["coverage"],
                "full_bar": bool(verdict["ship"] and p <= alpha and best["non_degenerate_all"]),
            }
            print(f"{sub_name}:{family}:k={k} rescored at {best['label']}: ship={verdict['ship']} "
                  f"p={p:.5f} H={entry['rescored']['model_kruskal_h']:.2f} "
                  f"gain={entry['rescored']['stability_gain']:.4f} ({time.time() - t0:.0f}s)",
                  file=sys.stderr)
        report["candidates"].append(entry)
    return report


def build_parser():
    import argparse
    p = argparse.ArgumentParser(description="#1500 decoder-smoothing sweep for the volume GMM k=5 candidate")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--period", type=int, default=48)
    p.add_argument("--filter-window", type=int, default=64)
    p.add_argument("--htf-multiple", type=int, default=4)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--n-perm", type=int, default=None)
    p.add_argument("--no-context", action="store_true", help="sweep only the primary candidate")
    p.add_argument("--json", default=None, help="write the sweep report JSON here")
    return p


def main(argv=None):
    args = build_parser().parse_args(argv)
    report = run(args.symbol, args.timeframe, period=args.period,
                 base_filter_window=args.filter_window, htf_multiple=args.htf_multiple,
                 seed=args.seed, n_perm=args.n_perm, context=not args.no_context)
    text = json.dumps(report, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
