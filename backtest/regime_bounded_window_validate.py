"""Bounded-window ADX re-validation harness (#1082).

WHY THIS EXISTS
---------------
The offline fit (regime_calibrate.fit_on_window -> composite_feature_matrix ->
compute_regime) computes Wilder ADX over the **full** eval window: the recursive
DI/ADX smoothing is seeded at the window start and warmed by every bar before the
scored bar. The **live** regime check (shared_scripts/check_regime.py) runs as a
fresh subprocess each cycle over a **bounded** fetch (`--ohlcv-limit`, default 200
bars), so its ADX recursion is seeded only `lookback` bars back. Wilder ADX is
recursive, so the same calendar bar can receive a different ADX live vs in the
fit, and a model fitted on full-window ADX may not reproduce live.

This harness re-validates a fitted model's forward-volatility separation under
bounded-window ADX (matching the live lookback) and quantifies the drift, so
promotion of a model into the live classifier (#1074) can be gated on a check
that reflects what the model will actually see live.

CAUSALITY NOTE (sets expectations for the drift magnitude)
----------------------------------------------------------
Wilder ADX is *causal*: ADX at bar i depends only on bars <= i, so
compute_regime(series[:i+1]) and compute_regime(series)[i] are identical. Bounded
vs full ADX therefore differ at bar i **only** when the bounded window starts
later than index 0 (i.e. it has dropped early warmup the full window kept). The
seed's influence decays geometrically at ((p-1)/p)^k per bar, and the composite
classifier caps the ADX sub-period at COMPOSITE_ADX_PERIOD_CAP (14) regardless of
the fit's `period`, so over a 200-bar live lookback the residual warmup drift is
expected to be small. The point of this harness is to *measure* it, not assume it.

Two boundary facts the comparison respects:
  * ADX arms at index 2*cap-1 and only emits once a later bar exists, so a fetch
    must carry > 2*cap bars to read a non-zero ADX. Live always does
    (check_regime.py enforces `--min-bars 30` > 2*14), and the harness lookback
    must exceed it too; below that the bounded ADX is unarmed (0), not "drifted".
  * The full-window (fit) view is *cold* at the window's first ~2*period bars
    (ADX seeded at the window start), whereas the bounded view is *warm* there
    because each bar carries a real `lookback`-bar prefix -- exactly the live
    asymmetry. Drift and label-agreement are therefore measured on the bars valid
    in BOTH views (the fit's cold warm-up bars, NaN-masked out of the fit, are
    excluded so they cannot masquerade as drift).

FAITHFUL LIVE REPRODUCTION
--------------------------
For each scored bar i the harness reproduces exactly what a live cycle does for
that bar: take the trailing `lookback` bars, run the real
`composite_feature_matrix` (bounded ADX inside) over that slice, and either
forward-filter the model over the slice or run `compute_regime_composite`
(hand-rule incumbent) over it -- taking the last bar's feature row / label. The
HMM forward-filter is itself windowed to `filter_window`, but it still consumes
the bounded ADX values, so model-label drift is real and mediated entirely by the
ADX feature. We call the same functions live calls; we do not re-implement them.

GO / NO-GO CHECK (gates #1074 promotion)
----------------------------------------
A model is cleared for promotion only when ALL hold:
  1. It still passes the regime_calibrate gate (`gate_verdict(...).ship`) when that
     gate is re-run on the **bounded-window** labels of both arms -- i.e. the
     forward-volatility separation and stability improvement survive bounded ADX.
  2. Per-bar model-label agreement between the full-window and bounded-window
     views is >= `agreement_threshold` (default 0.95): the labels the model was
     validated on are the labels it will emit live.
The report additionally surfaces the full-window verdict (to flag a regression:
passes full, fails bounded) and ADX/label drift statistics, including an optional
lookback sweep showing how drift decays as the live lookback lengthens.
"""
from __future__ import annotations

import os
import sys

_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_ROOT = os.path.abspath(os.path.join(_THIS_DIR, ".."))
for _p in (_THIS_DIR, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import numpy as np
import pandas as pd

from regime import (  # noqa: E402
    COMPOSITE_ADX_PERIOD_CAP,
    _DEFAULT_COMPOSITE_THRESHOLDS,
    composite_feature_matrix,
    compute_regime,
    compute_regime_composite,
)
from regime_calibrate import gate_verdict  # noqa: E402
from regime_diagnostics import score_labels  # noqa: E402
from regime_hmm import forward_filter_labels  # noqa: E402

# Mirrors shared_scripts/check_regime.py's `--ohlcv-limit` default and the probe
# argv in scheduler/version_probe.go. The live regime check fetches this many
# bars per cycle; the harness reproduces ADX over exactly this trailing window.
DEFAULT_LOOKBACK = 200
DEFAULT_PERIOD = 48  # matches regime_calibrate.fit_on_window when a model omits "period"
# A model whose live labels disagree with the labels it was validated on more than
# this fraction of bars has not been validated for what it will actually emit.
DEFAULT_AGREEMENT_THRESHOLD = 0.95
ADX_COL = 3  # composite_feature_matrix column order: return_eff, range_eff, efficiency, adx


# ---------------------------------------------------------------------------
# Core views (pure; operate on DataFrames so they are unit-testable without the
# data loader). `df` carries OHLCV with high/low/close columns.
# ---------------------------------------------------------------------------

def bounded_window_adx(df: pd.DataFrame, period: int, lookback: int,
                       adx_threshold: float, eval_start: int = 0) -> np.ndarray:
    """Per-bar ADX as the *live* bounded fetch computes it.

    For each bar i in [eval_start, n) the ADX is computed over only the trailing
    `lookback` bars (seeded at the start of that slice), at the composite ADX
    sub-period cap -- exactly what shared_scripts/check_regime.py sees. Bars
    before `eval_start` are NaN. Returns an array aligned to df (length n).
    """
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


def full_window_views(df_window: pd.DataFrame, model, period: int, th: dict):
    """Full-window features + (model, hand-rule) labels -- what the FIT consumed.

    ADX is seeded at the window start (recursive warm-up over the whole window).
    Returns (features[n x 4], model_labels|None, handrule_labels).
    """
    feats = composite_feature_matrix(df_window, period, th).to_numpy()
    hr_labels = compute_regime_composite(df_window, period=period, thresholds=th)["regime"].to_numpy()
    model_labels = None
    if model is not None:
        model_labels, _ = forward_filter_labels(feats, model)
    return feats, model_labels, hr_labels


def bounded_window_views(df: pd.DataFrame, model, period: int, th: dict,
                         lookback: int, eval_start: int):
    """Faithful per-bar live reproduction over `df` for bars [eval_start, n).

    Each scored bar is computed from its own trailing `lookback`-bar slice using
    the same functions the live cycle calls. Returns
    (features[m x 4], model_labels[m]|None, handrule_labels[m]) where m = n - eval_start.
    """
    feats_rows: list[np.ndarray] = []
    model_labs: list = []
    hr_labs: list = []
    n = len(df)
    want_model = model is not None
    for i in range(eval_start, n):
        lo = max(0, i - lookback + 1)
        w = df.iloc[lo:i + 1]
        feat_df = composite_feature_matrix(w, period, th)
        feats_rows.append(feat_df.iloc[-1].to_numpy() if len(feat_df) else np.full(4, np.nan))
        hr = compute_regime_composite(w, period=period, thresholds=th)["regime"]
        hr_labs.append(hr.iloc[-1] if len(hr) else None)
        if want_model:
            seq, _ = forward_filter_labels(feat_df.to_numpy(), model)
            model_labs.append(seq[-1] if len(seq) else None)
    feats = np.vstack(feats_rows) if feats_rows else np.empty((0, 4))
    model_arr = np.array(model_labs, dtype=object) if want_model else None
    return feats, model_arr, np.array(hr_labs, dtype=object)


# ---------------------------------------------------------------------------
# Drift statistics
# ---------------------------------------------------------------------------

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
    """Bars usable in BOTH views: neither feature row is NaN (warm-up / low-ATR)."""
    fv = ~np.isnan(np.asarray(full_feats, dtype=float)).any(axis=1)
    bv = ~np.isnan(np.asarray(bounded_feats, dtype=float)).any(axis=1)
    return fv & bv


# ---------------------------------------------------------------------------
# Go / no-go check
# ---------------------------------------------------------------------------

def go_no_go(full_model_scored, full_hr_scored, bounded_model_scored, bounded_hr_scored,
             label_agreement: float, *,
             agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD) -> dict:
    """Promotion gate for #1074. Promote iff the calibrate gate still ships under
    bounded-window labels AND model label agreement clears the threshold."""
    full_verdict = gate_verdict(full_hr_scored, full_model_scored)
    bounded_verdict = gate_verdict(bounded_hr_scored, bounded_model_scored)
    reasons: list[str] = []
    if not bounded_verdict["ship"]:
        reasons.append("model fails the calibrate gate under bounded-window ADX")
    if full_verdict["ship"] and not bounded_verdict["ship"]:
        reasons.append("verdict regressed: ships full-window but not bounded-window")
    if label_agreement < agreement_threshold:
        reasons.append(
            f"full-vs-bounded model label agreement {label_agreement:.4f} "
            f"< threshold {agreement_threshold:.4f}")
    promote = bool(bounded_verdict["ship"] and label_agreement >= agreement_threshold)
    return {
        "promote": promote,
        "blocking_reasons": reasons,
        "label_agreement": float(label_agreement),
        "agreement_threshold": float(agreement_threshold),
        "full_window_verdict": full_verdict,
        "bounded_window_verdict": bounded_verdict,
    }


# ---------------------------------------------------------------------------
# Orchestration (pure core + data-loading wrapper)
# ---------------------------------------------------------------------------

def validate_frames(df_window: pd.DataFrame, df_ext: pd.DataFrame, eval_start: int, model, *,
                    period: int = DEFAULT_PERIOD, thresholds: dict | None = None,
                    lookback: int = DEFAULT_LOOKBACK, target: str = "volatility",
                    seed: int = 0, horizons=(1, 4, 12),
                    agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD) -> dict:
    """Pure validation core. `df_window` is the exact eval window (full-window/fit
    view); `df_ext` is the same window prefixed with >= `lookback` warm-up bars,
    with `eval_start` the index in df_ext where the eval window begins. The eval
    bars of both frames are the same calendar bars."""
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS if thresholds is None else thresholds)
    close = df_window["close"].to_numpy(dtype=float)

    full_feats, full_model_labels, full_hr_labels = full_window_views(df_window, model, period, th)
    bounded_feats, bounded_model_labels, bounded_hr_labels = bounded_window_views(
        df_ext, model, period, th, lookback, eval_start)

    valid = _feature_valid_mask(full_feats, bounded_feats)

    report: dict = {
        "lookback": int(lookback),
        "period": int(period),
        "target": target,
        "seed": int(seed),
        "n_eval_bars": int(len(close)),
        "n_scored_bars": int(valid.sum()),
        "adx_drift": adx_drift_stats(full_feats[:, ADX_COL], bounded_feats[:, ADX_COL]),
        "handrule": {
            "label_drift": label_drift_stats(full_hr_labels, bounded_hr_labels, valid),
            "full": score_labels(close, full_hr_labels, full_feats, horizons=horizons,
                                 seed=seed, target=target),
            "bounded": score_labels(close, bounded_hr_labels, bounded_feats, horizons=horizons,
                                    seed=seed, target=target),
        },
    }
    if model is not None:
        model_drift = label_drift_stats(full_model_labels, bounded_model_labels, valid)
        full_model_scored = score_labels(close, full_model_labels, full_feats, horizons=horizons,
                                         seed=seed, target=target)
        bounded_model_scored = score_labels(close, bounded_model_labels, bounded_feats,
                                            horizons=horizons, seed=seed, target=target)
        report["model"] = {
            "label_drift": model_drift,
            "full": full_model_scored,
            "bounded": bounded_model_scored,
        }
        report["go_no_go"] = go_no_go(
            full_model_scored, report["handrule"]["full"],
            bounded_model_scored, report["handrule"]["bounded"],
            model_drift["agreement"], agreement_threshold=agreement_threshold)
    return report


def _align_eval_start(df_window: pd.DataFrame, df_ext: pd.DataFrame) -> int:
    """Index in df_ext where the eval window begins. Both frames end at the same
    bar (same end_date), so the window is df_ext's tail of len(df_window)."""
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
             lookback: int = DEFAULT_LOOKBACK, target: str = "volatility", seed: int = 0,
             horizons=(1, 4, 12), agreement_threshold: float = DEFAULT_AGREEMENT_THRESHOLD) -> dict:
    """Data-loading wrapper: load the eval window plus a >= lookback warm-up
    prefix, then run validate_frames."""
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
    period = int(model["period"]) if model and "period" in model else DEFAULT_PERIOD
    report = validate_frames(df_window, df_ext, eval_start, model, period=period,
                             lookback=lookback, target=target, seed=seed, horizons=horizons,
                             agreement_threshold=agreement_threshold)
    report.update({"symbol": symbol, "timeframe": timeframe, "window": window})
    return report


def _sweep_summary(report: dict) -> dict:
    """Compact per-lookback row for the sensitivity sweep."""
    row = {"lookback": report["lookback"], "adx_mean_abs": report["adx_drift"]["mean_abs"],
           "adx_p95_abs": report["adx_drift"]["p95_abs"], "adx_corr": report["adx_drift"]["corr"]}
    if "model" in report:
        row["model_label_agreement"] = report["model"]["label_drift"]["agreement"]
        row["promote"] = report["go_no_go"]["promote"]
        row["bounded_ship"] = report["go_no_go"]["bounded_window_verdict"]["ship"]
    return row


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

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
                   help="comma list of lookbacks to sweep, e.g. 100,150,200,300,400")
    p.add_argument("--target", default="volatility", choices=("returns", "volatility"),
                   help="forward variable the separation is scored on (default volatility, #1078)")
    p.add_argument("--horizons", default="1,4,12")
    p.add_argument("--agreement-threshold", type=float, default=DEFAULT_AGREEMENT_THRESHOLD)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--json", default=None, help="write the full report JSON to this path")
    return p


def main(argv=None) -> int:
    import json
    args = build_parser().parse_args(argv)
    model = None
    if args.model_json:
        with open(args.model_json) as fh:
            loaded = json.load(fh)
        model = loaded.get("model", loaded) if isinstance(loaded, dict) else loaded
    horizons = tuple(int(x) for x in args.horizons.split(","))

    if args.lookback_sweep:
        lookbacks = [int(x) for x in args.lookback_sweep.split(",")]
        sweep = []
        for lb in lookbacks:
            rep = validate(args.symbol, args.timeframe, args.window, model, lookback=lb,
                           target=args.target, seed=args.seed, horizons=horizons,
                           agreement_threshold=args.agreement_threshold)
            sweep.append(_sweep_summary(rep))
        payload = {"symbol": args.symbol, "timeframe": args.timeframe, "window": args.window,
                   "target": args.target, "sweep": sweep}
    else:
        payload = validate(args.symbol, args.timeframe, args.window, model,
                           lookback=args.lookback, target=args.target, seed=args.seed,
                           horizons=horizons, agreement_threshold=args.agreement_threshold)

    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, "w") as fh:
            fh.write(text)
    print(text)
    if "go_no_go" in payload:
        return 0 if payload["go_no_go"]["promote"] else 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
