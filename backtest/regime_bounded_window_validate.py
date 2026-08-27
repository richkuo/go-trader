from __future__ import annotations

import inspect
import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_THIS_DIR, ".."))
for _p in (_THIS_DIR, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from regime import (
    COMPOSITE_ADX_PERIOD_CAP,
    _DEFAULT_COMPOSITE_THRESHOLDS,
    composite_feature_matrix,
    compute_regime,
    compute_regime_composite,
)
from regime_calibrate import gate_verdict
from regime_diagnostics import DEFAULT_PERIOD, score_labels
from regime_hmm import forward_filter_labels

DEFAULT_LOOKBACK = 200
DEFAULT_AGREEMENT_THRESHOLD = 0.95
DEFAULT_MIN_AGREEMENT_BARS = 30
ADX_COL = 3


def _gate_primary_horizon() -> int:
    primary = inspect.signature(gate_verdict).parameters["primary"].default
    return int(str(primary).lstrip("h"))


def _windows_overlap(fit_range, val_range) -> bool:
    fs, fe = fit_range
    vs, ve = val_range
    fe = "9999-12-31" if fe is None else str(fe)
    ve = "9999-12-31" if ve is None else str(ve)
    return str(fs) < ve and str(vs) < fe


def _provenance_status(model, symbol: str, timeframe: str, window: str,
                       windows: dict | None = None) -> dict:
    if windows is None:
        try:
            from eval_windows import WINDOWS as windows
        except Exception:
            windows = {}
    fitted_on = dict((model or {}).get("fitted_on") or {})
    validated_on = {"symbol": symbol, "timeframe": timeframe, "window": window}
    verified = bool(fitted_on)
    name_match = verified and all(fitted_on.get(k) == v for k, v in validated_on.items())
    same_instrument = (verified and fitted_on.get("symbol") == symbol
                       and fitted_on.get("timeframe") == timeframe)
    date_overlap = False
    overlap_resolvable = True
    overlap_detail = None
    if same_instrument and not name_match:
        fit_range = windows.get(fitted_on.get("window"))
        val_range = windows.get(window)
        if fit_range is None or val_range is None:
            overlap_resolvable = False
        elif _windows_overlap(fit_range, val_range):
            date_overlap = True
            overlap_detail = {"fit_window": fitted_on.get("window"),
                              "fit_range": list(fit_range),
                              "validation_window": window,
                              "validation_range": list(val_range)}
    in_sample = bool(name_match or date_overlap)
    return {"fitted_on": fitted_on, "validated_on": validated_on, "verified": verified,
            "name_match": bool(name_match), "date_overlap": date_overlap,
            "overlap_resolvable": overlap_resolvable, "overlap_detail": overlap_detail,
            "in_sample": in_sample}



def bounded_window_adx(df: pd.DataFrame, period: int, lookback: int,
                       adx_threshold: float, eval_start: int = 0) -> np.ndarray:
    n = len(df)
    out = np.full(n, np.nan)
    adx_period = min(int(period), COMPOSITE_ADX_PERIOD_CAP)
    high = df["high"].to_numpy(dtype=float)
    low = df["low"].to_numpy(dtype=float)
    close = df["close"].to_numpy(dtype=float)
    for i in range(max(0, eval_start), n):
        lo = max(0, i - lookback + 1)
        w = pd.DataFrame({"high": high[lo:i + 1], "low": low[lo:i + 1], "close": close[lo:i + 1]})
        adx = compute_regime(w, period=adx_period, adx_threshold=adx_threshold)["adx"]
        out[i] = float(adx.iloc[-1]) if len(adx) else np.nan
    return out


def full_window_views(df_window: pd.DataFrame, model, period: int, th: dict, *,
                      want_model: bool = True, want_handrule: bool = True):
    feats = composite_feature_matrix(df_window, period, th).to_numpy()
    hr_labels = None
    if want_handrule:
        hr_labels = compute_regime_composite(df_window, period=period, thresholds=th)["regime"].to_numpy()
    model_labels = None
    if want_model and model is not None:
        model_labels, _ = forward_filter_labels(feats, model)
    return feats, model_labels, hr_labels


def bounded_window_views(df: pd.DataFrame, model, period: int, th: dict,
                         lookback: int, eval_start: int, *,
                         want_model: bool = True, want_handrule: bool = True):
    feats_rows: list[np.ndarray] = []
    model_labs: list = []
    hr_labs: list = []
    n = len(df)
    want_model = want_model and model is not None
    for i in range(eval_start, n):
        lo = max(0, i - lookback + 1)
        w = df.iloc[lo:i + 1]
        feat_df = composite_feature_matrix(w, period, th)
        feats_rows.append(feat_df.iloc[-1].to_numpy() if len(feat_df) else np.full(4, np.nan))
        if want_handrule:
            hr = compute_regime_composite(w, period=period, thresholds=th)["regime"]
            hr_labs.append(hr.iloc[-1] if len(hr) else None)
        if want_model:
            seq, _ = forward_filter_labels(feat_df.to_numpy(), model)
            model_labs.append(seq[-1] if len(seq) else None)
    feats = np.vstack(feats_rows) if feats_rows else np.empty((0, 4))
    model_arr = np.array(model_labs, dtype=object) if want_model else None
    hr_arr = np.array(hr_labs, dtype=object) if want_handrule else None
    return feats, model_arr, hr_arr



def adx_drift_stats(full_adx: np.ndarray, bounded_adx: np.ndarray) -> dict:
    a = np.asarray(full_adx, dtype=float)
    b = np.asarray(bounded_adx, dtype=float)
    mask = ~np.isnan(a) & ~np.isnan(b)
    if not mask.any():
        return {"n": 0, "mean_abs": 0.0, "median_abs": 0.0, "p95_abs": 0.0,
                "max_abs": 0.0, "mean_rel": 0.0, "p95_rel": 0.0, "corr": 1.0}
    av, bv = a[mask], b[mask]
    d = np.abs(av - bv)
    denom = np.where(np.abs(av) > 1e-9, np.abs(av), np.nan)
    rel = d / denom
    corr = float(np.corrcoef(av, bv)[0, 1]) if mask.sum() > 1 and av.std() > 0 and bv.std() > 0 else 1.0
    return {
        "n": int(mask.sum()),
        "mean_abs": float(d.mean()),
        "median_abs": float(np.median(d)),
        "p95_abs": float(np.percentile(d, 95)),
        "max_abs": float(d.max()),
        "mean_rel": float(np.nanmean(rel)) if np.isfinite(rel).any() else 0.0,
        "p95_rel": float(np.nanpercentile(rel, 95)) if np.isfinite(rel).any() else 0.0,
        "corr": corr,
    }


def label_drift_stats(full_labels, bounded_labels, valid_mask) -> dict:
    f = np.asarray(full_labels, dtype=object)
    b = np.asarray(bounded_labels, dtype=object)
    m = np.asarray(valid_mask, dtype=bool)
    f, b = f[m], b[m]
    n = len(f)
    if n == 0:
        return {"n": 0, "agreement": 1.0, "disagreements": 0, "transitions": {}}
    eq = np.array([x == y for x, y in zip(f, b)])
    transitions: dict[str, int] = {}
    for x, y in zip(f[~eq], b[~eq]):
        key = f"{x}->{y}"
        transitions[key] = transitions.get(key, 0) + 1
    return {
        "n": n,
        "agreement": float(eq.mean()),
        "disagreements": int((~eq).sum()),
        "transitions": dict(sorted(transitions.items())),
    }


def _feature_valid_mask(full_feats: np.ndarray, bounded_feats: np.ndarray) -> np.ndarray:
    fv = ~np.isnan(np.asarray(full_feats, dtype=float)).any(axis=1)
    bv = ~np.isnan(np.asarray(bounded_feats, dtype=float)).any(axis=1)
    return fv & bv



def go_no_go(full_model_scored, full_hr_scored, bounded_model_scored, bounded_hr_scored,
             model_label_drift: dict, *,
             agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD,
             min_agreement_bars: int = DEFAULT_MIN_AGREEMENT_BARS,
             in_sample: bool = False, provenance_verified: bool = True,
             require_provenance: bool = False) -> dict:
    label_agreement = float(model_label_drift.get("agreement", 0.0))
    comparable_bars = int(model_label_drift.get("n", 0))
    full_verdict = gate_verdict(full_hr_scored, full_model_scored)
    bounded_verdict = gate_verdict(bounded_hr_scored, bounded_model_scored)
    reasons: list[str] = []
    warnings: list[str] = []
    if in_sample:
        reasons.append(
            "in-sample re-score: the validation window equals OR overlaps (by date range) "
            "the model's fit window -- separation/stability are optimistic; validate on a "
            "disjoint held-out window")
    if not provenance_verified:
        msg = ("model provenance unverifiable (no fitted_on stamp) -- cannot confirm the "
               "validation window is held out from the fit window")
        if require_provenance:
            reasons.append(msg + " and --require-provenance is set")
        else:
            warnings.append(msg)
    if not bounded_verdict["ship"]:
        reasons.append("model fails the calibrate gate under bounded-window ADX")
    if full_verdict["ship"] and not bounded_verdict["ship"]:
        reasons.append("verdict regressed: ships full-window but not bounded-window")
    enough_bars = comparable_bars >= min_agreement_bars
    if not enough_bars:
        reasons.append(
            f"insufficient comparable bars: {comparable_bars} < {min_agreement_bars} "
            "(agreement not measurable -> fail closed)")
    elif label_agreement < agreement_threshold:
        reasons.append(
            f"full-vs-bounded model label agreement {label_agreement:.4f} "
            f"< threshold {agreement_threshold:.4f}")
    promote = not reasons
    return {
        "promote": promote,
        "blocking_reasons": reasons,
        "warnings": warnings,
        "in_sample": bool(in_sample),
        "provenance_verified": bool(provenance_verified),
        "label_agreement": label_agreement,
        "agreement_threshold": float(agreement_threshold),
        "comparable_bars": comparable_bars,
        "min_agreement_bars": int(min_agreement_bars),
        "full_window_verdict": full_verdict,
        "bounded_window_verdict": bounded_verdict,
    }



def validate_frames(df_window: pd.DataFrame, df_ext: pd.DataFrame, eval_start: int, model, *,
                    period: int | None = None, incumbent_period: int = DEFAULT_PERIOD,
                    thresholds: dict | None = None,
                    lookback: int = DEFAULT_LOOKBACK, target: str = "volatility",
                    seed: int = 0, horizons=(1, 4, 12),
                    agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD,
                    min_agreement_bars: int = DEFAULT_MIN_AGREEMENT_BARS,
                    in_sample: bool = False, provenance_verified: bool = True,
                    require_provenance: bool = False) -> dict:
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS if thresholds is None else thresholds)
    close = df_window["close"].to_numpy(dtype=float)
    model_period = int(period) if period is not None else (
        int(model["period"]) if model and "period" in model else DEFAULT_PERIOD)
    if model is not None:
        gate_h = _gate_primary_horizon()
        if gate_h not in {int(h) for h in horizons}:
            raise ValueError(
                f"horizons {tuple(int(h) for h in horizons)} omit the calibrate gate's "
                f"primary horizon h{gate_h}; gate_verdict reads report['h{gate_h}'] and "
                f"would KeyError mid-run. Include {gate_h} in horizons.")

    hr_full_feats, _, hr_full_labels = full_window_views(
        df_window, None, incumbent_period, th, want_model=False, want_handrule=True)
    hr_bounded_feats, _, hr_bounded_labels = bounded_window_views(
        df_ext, None, incumbent_period, th, lookback, eval_start,
        want_model=False, want_handrule=True)
    hr_valid = _feature_valid_mask(hr_full_feats, hr_bounded_feats)
    hr_full_scored = score_labels(close, hr_full_labels, hr_full_feats, horizons=horizons,
                                  seed=seed, target=target)
    hr_bounded_scored = score_labels(close, hr_bounded_labels, hr_bounded_feats,
                                     horizons=horizons, seed=seed, target=target)

    report: dict = {
        "lookback": int(lookback),
        "model_period": model_period,
        "incumbent_period": int(incumbent_period),
        "target": target,
        "seed": int(seed),
        "n_eval_bars": int(len(close)),
        "handrule": {
            "period": int(incumbent_period),
            "n_scored_bars": int(hr_valid.sum()),
            "label_drift": label_drift_stats(hr_full_labels, hr_bounded_labels, hr_valid),
            "full": hr_full_scored,
            "bounded": hr_bounded_scored,
        },
    }

    if model is not None:
        m_full_feats, m_full_labels, _ = full_window_views(
            df_window, model, model_period, th, want_model=True, want_handrule=False)
        m_bounded_feats, m_bounded_labels, _ = bounded_window_views(
            df_ext, model, model_period, th, lookback, eval_start,
            want_model=True, want_handrule=False)
        m_valid = _feature_valid_mask(m_full_feats, m_bounded_feats)
        model_drift = label_drift_stats(m_full_labels, m_bounded_labels, m_valid)
        full_model_scored = score_labels(close, m_full_labels, m_full_feats, horizons=horizons,
                                         seed=seed, target=target)
        bounded_model_scored = score_labels(close, m_bounded_labels, m_bounded_feats,
                                            horizons=horizons, seed=seed, target=target)
        report["n_scored_bars"] = int(m_valid.sum())
        report["adx_drift"] = adx_drift_stats(m_full_feats[:, ADX_COL], m_bounded_feats[:, ADX_COL])
        report["model"] = {
            "period": model_period,
            "label_drift": model_drift,
            "full": full_model_scored,
            "bounded": bounded_model_scored,
        }
        report["go_no_go"] = go_no_go(
            full_model_scored, hr_full_scored, bounded_model_scored, hr_bounded_scored,
            model_drift, agreement_threshold=agreement_threshold,
            min_agreement_bars=min_agreement_bars, in_sample=in_sample,
            provenance_verified=provenance_verified, require_provenance=require_provenance)
    else:
        report["n_scored_bars"] = int(hr_valid.sum())
        report["adx_drift"] = adx_drift_stats(hr_full_feats[:, ADX_COL], hr_bounded_feats[:, ADX_COL])
    return report


def _align_eval_start(df_window: pd.DataFrame, df_ext: pd.DataFrame) -> int:
    eval_start = len(df_ext) - len(df_window)
    if eval_start < 0:
        raise ValueError("extended frame is shorter than the window frame")
    if len(df_window):
        a = float(df_window["close"].iloc[0])
        b = float(df_ext["close"].iloc[eval_start])
        if not (abs(a - b) <= 1e-6 * max(1.0, abs(a))):
            raise ValueError("window/extended frames are not bar-aligned at eval_start")
    return eval_start


def validate(symbol: str, timeframe: str, window: str, model, *,
             lookback: int = DEFAULT_LOOKBACK, incumbent_period: int = DEFAULT_PERIOD,
             target: str = "volatility", seed: int = 0, horizons=(1, 4, 12),
             agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD,
             min_agreement_bars: int = DEFAULT_MIN_AGREEMENT_BARS,
             require_provenance: bool = False) -> dict:
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    if window not in WINDOWS:
        raise SystemExit(f"unknown window {window!r}; known: {list(WINDOWS)}")
    start, end = WINDOWS[window]
    df_window = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                                 start_date=start, end_date=end)
    df_ext = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                              start_date=None, end_date=end)
    eval_start = _align_eval_start(df_window, df_ext)
    prov = _provenance_status(model, symbol, timeframe, window, WINDOWS) if model is not None else None
    provenance_verified = bool(prov and prov["verified"] and prov["overlap_resolvable"])
    report = validate_frames(df_window, df_ext, eval_start, model, period=None,
                             incumbent_period=incumbent_period, lookback=lookback,
                             target=target, seed=seed, horizons=horizons,
                             agreement_threshold=agreement_threshold,
                             min_agreement_bars=min_agreement_bars,
                             in_sample=bool(prov and prov["in_sample"]),
                             provenance_verified=provenance_verified,
                             require_provenance=require_provenance)
    report.update({"symbol": symbol, "timeframe": timeframe, "window": window})
    if prov is not None:
        report["provenance"] = prov
    return report


def _sweep_summary(report: dict) -> dict:
    row = {"lookback": report["lookback"], "adx_mean_abs": report["adx_drift"]["mean_abs"],
           "adx_p95_abs": report["adx_drift"]["p95_abs"], "adx_corr": report["adx_drift"]["corr"]}
    if "model" in report:
        row["model_label_agreement"] = report["model"]["label_drift"]["agreement"]
        row["comparable_bars"] = report["go_no_go"]["comparable_bars"]
        row["promote"] = report["go_no_go"]["promote"]
        row["bounded_ship"] = report["go_no_go"]["bounded_window_verdict"]["ship"]
    return row


def _sweep_blocked(sweep: list[dict]) -> bool:
    model_rows = [r for r in sweep if "promote" in r]
    return bool(model_rows) and not all(bool(r["promote"]) for r in model_rows)



def build_parser():
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(
        description="Bounded-window ADX re-validation + go/no-go gate for #1074 (#1082)")
    p.add_argument("--symbol", default="BTC/USDT")
    p.add_argument("--timeframe", default="1h")
    p.add_argument("--window", default="oos", help=f"known: {', '.join(WINDOWS)}")
    p.add_argument("--model-json", default=None,
                   help="fitted model JSON (regime_calibrate --out). Omit to report hand-rule drift only.")
    p.add_argument("--lookback", type=int, default=DEFAULT_LOOKBACK,
                   help=f"live bounded fetch size (default {DEFAULT_LOOKBACK}, mirrors --ohlcv-limit)")
    p.add_argument("--lookback-sweep", default=None,
                   help="comma list of lookbacks to sweep, e.g. 100,150,200,300,400. "
                        "Exit code is worst-case (non-zero if ANY swept lookback blocks promotion).")
    p.add_argument("--incumbent-period", type=int, default=DEFAULT_PERIOD,
                   help=f"composite period for the hand-rule incumbent arm (default "
                        f"{DEFAULT_PERIOD}, matches regime_calibrate's gate)")
    p.add_argument("--target", default="volatility", choices=("returns", "volatility"),
                   help="forward variable the separation is scored on (default volatility, #1078)")
    p.add_argument("--horizons", default="1,4,12")
    p.add_argument("--agreement-threshold", type=float, default=DEFAULT_AGREEMENT_THRESHOLD)
    p.add_argument("--min-agreement-bars", type=int, default=DEFAULT_MIN_AGREEMENT_BARS,
                   help=f"fail closed if fewer than this many bars are scored by both arms "
                        f"(default {DEFAULT_MIN_AGREEMENT_BARS})")
    p.add_argument("--require-provenance", action="store_true",
                   help="block promotion when the model carries no fitted_on stamp "
                        "(cannot confirm the validation window is held out from the fit). "
                        "An in-sample re-score is always refused regardless of this flag.")
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write the full report JSON to this path")
    return p


def _warn_provenance(prov: dict | None, require_provenance: bool) -> None:
    if not prov:
        return
    if prov.get("in_sample"):
        if prov.get("date_overlap"):
            d = prov.get("overlap_detail") or {}
            print(f"ERROR: in-sample re-score -- validation window {d.get('validation_window')!r} "
                  f"{d.get('validation_range')} overlaps the model's fit window "
                  f"{d.get('fit_window')!r} {d.get('fit_range')}; promotion refused.",
                  file=sys.stderr)
        else:
            print(f"ERROR: in-sample re-score -- validation window {prov['validated_on']} equals "
                  f"the model's fit window; promotion refused.", file=sys.stderr)
    elif not prov.get("verified"):
        print("WARNING: model has no fitted_on provenance stamp; cannot confirm "
              f"{prov['validated_on']} is held out from the fit window."
              f"{' Blocked by --require-provenance.' if require_provenance else ''}",
              file=sys.stderr)
    elif not prov.get("overlap_resolvable"):
        print(f"WARNING: the model's fit window {prov['fitted_on'].get('window')!r} is not in "
              "WINDOWS; cannot confirm the validation window is disjoint from it."
              f"{' Blocked by --require-provenance.' if require_provenance else ''}",
              file=sys.stderr)


def main(argv=None) -> int:
    import json
    parser = build_parser()
    args = parser.parse_args(argv)
    model = None
    if args.model_json:
        with open(args.model_json) as fh:
            loaded = json.load(fh)
        model = loaded.get("model", loaded) if isinstance(loaded, dict) else loaded
    horizons = tuple(int(x) for x in args.horizons.split(","))

    if model is not None:
        gate_h = _gate_primary_horizon()
        if gate_h not in set(horizons):
            parser.error(
                f"--horizons {','.join(str(h) for h in horizons)} omits the calibrate gate's "
                f"primary horizon {gate_h}; the gate scores h{gate_h}. Include {gate_h}.")

    if args.lookback_sweep:
        lookbacks = [int(x) for x in args.lookback_sweep.split(",")]
        sweep = []
        prov = None
        for lb in lookbacks:
            rep = validate(args.symbol, args.timeframe, args.window, model, lookback=lb,
                           incumbent_period=args.incumbent_period, target=args.target,
                           seed=args.seed, horizons=horizons,
                           agreement_threshold=args.agreement_threshold,
                           min_agreement_bars=args.min_agreement_bars,
                           require_provenance=args.require_provenance)
            sweep.append(_sweep_summary(rep))
            prov = rep.get("provenance", prov)
        blocked = _sweep_blocked(sweep)
        payload = {"symbol": args.symbol, "timeframe": args.timeframe, "window": args.window,
                   "target": args.target, "sweep": sweep,
                   "promotion_blocked": blocked,
                   "blocked_lookbacks": [r["lookback"] for r in sweep
                                         if "promote" in r and not r["promote"]]}
        if prov is not None:
            payload["provenance"] = prov
    else:
        payload = validate(args.symbol, args.timeframe, args.window, model,
                           lookback=args.lookback, incumbent_period=args.incumbent_period,
                           target=args.target, seed=args.seed, horizons=horizons,
                           agreement_threshold=args.agreement_threshold,
                           min_agreement_bars=args.min_agreement_bars,
                           require_provenance=args.require_provenance)

    _warn_provenance(payload.get("provenance"), args.require_provenance)
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    if "sweep" in payload:
        return 1 if payload["promotion_blocked"] else 0
    if "go_no_go" in payload:
        return 0 if payload["go_no_go"]["promote"] else 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
