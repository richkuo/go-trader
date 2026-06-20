# backtest/tests/test_regime_bounded_window.py
"""Tests for the bounded-window ADX re-validation harness (#1082)."""
import os
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..", "..", "shared_tools")))

from regime import (
    _DEFAULT_COMPOSITE_THRESHOLDS,
    VALID_LABELS_COMPOSITE,
    composite_feature_matrix,
    compute_regime_composite,
)
from regime_hmm import fit_label_anchored_hmm, forward_filter_labels
from regime_bounded_window_validate import (
    DEFAULT_AGREEMENT_THRESHOLD,
    _align_eval_start,
    _gate_primary_horizon,
    _provenance_status,
    _sweep_blocked,
    adx_drift_stats,
    bounded_window_adx,
    bounded_window_views,
    full_window_views,
    go_no_go,
    label_drift_stats,
    validate_frames,
)

TH = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
PERIOD = 48


def _ohlcv(n, seed=0, regime_mix=True):
    """Synthetic OHLCV with a trend->range->downtrend mix so ADX actually moves."""
    rng = np.random.default_rng(seed)
    if regime_mix:
        a = max(1, n // 3)
        seg = np.concatenate([
            np.linspace(0, 60, a),
            np.full(n - 2 * a, 60.0),
            np.linspace(60, 10, a),
        ])[:n]
    else:
        seg = np.zeros(n)
    close = 100 + seg + np.cumsum(rng.normal(0, 1, n)) * 0.3
    high = close + np.abs(rng.normal(0, 0.6, n))
    low = close - np.abs(rng.normal(0, 0.6, n))
    return pd.DataFrame({"high": high, "low": low, "close": close})


def _full_adx(df):
    return composite_feature_matrix(df, PERIOD, TH)["adx"].to_numpy()


def _fit_model(df, filter_window=24):
    feats = composite_feature_matrix(df, PERIOD, TH).to_numpy()
    labels = compute_regime_composite(df, period=PERIOD, thresholds=TH)["regime"].to_numpy()
    model = fit_label_anchored_hmm(feats, labels, sorted(VALID_LABELS_COMPOSITE),
                                   filter_window=filter_window)
    model["period"] = PERIOD
    return model


def _mean_abs_drift(full, bounded):
    m = ~np.isnan(full) & ~np.isnan(bounded)
    return float(np.mean(np.abs(full[m] - bounded[m]))) if m.any() else 0.0


# --- ADX causality + bounded-window drift -----------------------------------------

def test_bounded_adx_equals_full_when_lookback_spans_window():
    # Wilder ADX is causal: ADX[i] over series[:i+1] == ADX[i] over the whole series.
    # So when the bounded window reaches back to index 0 for every bar, bounded == full
    # EXACTLY -- the only source of drift is a window that starts later than 0.
    df = _ohlcv(250, seed=3)
    full = _full_adx(df)
    bounded = bounded_window_adx(df, PERIOD, lookback=len(df), adx_threshold=TH["adx"], eval_start=0)
    m = ~np.isnan(full)
    assert m.any()
    np.testing.assert_allclose(bounded[m], full[m], rtol=0, atol=1e-9)


def test_bounded_adx_drift_decays_as_lookback_grows():
    df = _ohlcv(320, seed=4)
    full = _full_adx(df)
    d_short = _mean_abs_drift(full, bounded_window_adx(df, PERIOD, 40, TH["adx"], 0))
    d_long = _mean_abs_drift(full, bounded_window_adx(df, PERIOD, 160, TH["adx"], 0))
    d_span = _mean_abs_drift(full, bounded_window_adx(df, PERIOD, len(df), TH["adx"], 0))
    assert d_short > d_long > 0.0          # a short live lookback drifts more than a long one
    assert d_span == 0.0                   # spanning the window => no drift at all
    assert d_long < d_short                 # monotone improvement with more warm-up


def test_bounded_adx_respects_eval_start():
    df = _ohlcv(160, seed=5)
    out = bounded_window_adx(df, PERIOD, 60, TH["adx"], eval_start=100)
    assert np.isnan(out[:100]).all()       # bars before eval_start are not computed
    assert (~np.isnan(out[150:])).any()    # later bars are


# --- faithful per-bar reproduction ------------------------------------------------

def test_bounded_views_last_bar_matches_one_shot_live_call():
    # The harness must reproduce, for the final scored bar, exactly what one live cycle
    # computes over its trailing fetch: same feature row, same hand-rule and model labels.
    df = _ohlcv(300, seed=6)
    model = _fit_model(df)
    lookback, eval_start = 120, 297
    feats, model_labels, hr_labels = bounded_window_views(df, model, PERIOD, TH, lookback, eval_start)

    i = len(df) - 1
    w = df.iloc[i - lookback + 1: i + 1]
    F = composite_feature_matrix(w, PERIOD, TH)
    one_shot_feat = F.iloc[-1].to_numpy()
    one_shot_hr = compute_regime_composite(w, period=PERIOD, thresholds=TH)["regime"].iloc[-1]
    one_shot_model = forward_filter_labels(F.to_numpy(), model)[0][-1]

    np.testing.assert_allclose(feats[-1], one_shot_feat, rtol=0, atol=1e-12, equal_nan=True)
    assert hr_labels[-1] == one_shot_hr
    assert model_labels[-1] == one_shot_model


def test_full_window_views_seed_at_window_start():
    # The full view must be the fit's view: ADX seeded at the window start, model labels
    # from forward-filtering the full feature matrix.
    df = _ohlcv(160, seed=7)
    model = _fit_model(df)
    feats, model_labels, hr_labels = full_window_views(df, model, PERIOD, TH)
    np.testing.assert_allclose(feats[:, 3], _full_adx(df), rtol=0, atol=1e-12, equal_nan=True)
    assert len(model_labels) == len(df) and len(hr_labels) == len(df)


# --- drift statistics -------------------------------------------------------------

def test_adx_drift_stats_zero_when_identical():
    a = np.array([10.0, 20.0, np.nan, 30.0])
    s = adx_drift_stats(a, a.copy())
    assert s["n"] == 3 and s["mean_abs"] == 0.0 and s["max_abs"] == 0.0 and s["corr"] == 1.0


def test_label_drift_stats_counts_disagreements_on_valid_bars_only():
    full = np.array(["a", "a", "b", "c"], dtype=object)
    bounded = np.array(["a", "x", "b", "y"], dtype=object)
    valid = np.array([True, True, True, False])   # last bar excluded (warm-up/low-ATR)
    s = label_drift_stats(full, bounded, valid)
    assert s["n"] == 3
    assert s["disagreements"] == 1                 # only a->x counts; c->y is masked out
    assert abs(s["agreement"] - 2 / 3) < 1e-9
    assert s["transitions"] == {"a->x": 1}


# --- go / no-go gate --------------------------------------------------------------

def _scored(kw_h, transition_rate, p_value=0.005):
    return {"stability": {"transition_rate": transition_rate},
            "h4": {"separation": {"kruskal_h": kw_h}, "significance": {"p_value": p_value}}}


def _drift(agreement, n=500):
    return {"n": n, "agreement": agreement,
            "disagreements": int(round((1 - agreement) * n)), "transitions": {}}


def test_go_no_go_promotes_when_bounded_ships_and_agreement_high():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)          # within tolerance, whipsaw down, significant
    v = go_no_go(md, hr, md, hr, _drift(0.99))
    assert v["promote"] is True
    assert v["blocking_reasons"] == []
    assert v["bounded_window_verdict"]["ship"] is True


def test_go_no_go_blocks_when_bounded_fails_gate():
    hr = _scored(10.0, 0.40)
    md_full = _scored(9.7, 0.25)     # ships full-window
    md_bounded = _scored(4.0, 0.25)  # separation collapses under bounded ADX
    v = go_no_go(md_full, hr, md_bounded, hr, _drift(0.99))
    assert v["promote"] is False
    assert any("fails the calibrate gate" in r for r in v["blocking_reasons"])
    assert any("regressed" in r for r in v["blocking_reasons"])  # full ships, bounded doesn't


def test_go_no_go_blocks_when_label_agreement_below_threshold():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)          # gate itself ships under bounded
    v = go_no_go(md, hr, md, hr, _drift(0.80))
    assert v["promote"] is False
    assert any("label agreement" in r for r in v["blocking_reasons"])
    assert v["bounded_window_verdict"]["ship"] is True   # gate ok, agreement is the blocker


def test_go_no_go_default_threshold():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    just_under = go_no_go(md, hr, md, hr, _drift(DEFAULT_AGREEMENT_THRESHOLD - 0.001))
    at = go_no_go(md, hr, md, hr, _drift(DEFAULT_AGREEMENT_THRESHOLD))
    assert just_under["promote"] is False and at["promote"] is True


def test_go_no_go_blocks_on_insufficient_comparable_bars():
    # Finding 1: a short window shrinks the full∩bounded intersection toward empty;
    # label_drift_stats then reports a vacuous agreement=1.0 on ~0 bars. Even with the
    # gate shipping and agreement "perfect", the gate must FAIL CLOSED on too few bars.
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)                      # gate ships under bounded
    v = go_no_go(md, hr, md, hr, _drift(1.0, n=5), min_agreement_bars=30)
    assert v["promote"] is False
    assert any("insufficient comparable bars" in r for r in v["blocking_reasons"])
    assert v["bounded_window_verdict"]["ship"] is True   # gate ok; the bar floor is the blocker


def test_go_no_go_zero_comparable_bars_vacuous_agreement_blocks():
    # The exact degenerate case: label_drift_stats on an all-NaN/empty mask returns
    # agreement=1.0, n=0. That 1.0 must never clear the gate.
    empty = label_drift_stats(np.array([], dtype=object), np.array([], dtype=object),
                              np.array([], dtype=bool))
    assert empty["agreement"] == 1.0 and empty["n"] == 0
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    v = go_no_go(md, hr, md, hr, empty)
    assert v["promote"] is False
    assert any("insufficient comparable bars" in r for r in v["blocking_reasons"])


# --- lookback sweep exit-code (finding 3) -----------------------------------------

def test_sweep_blocked_worst_case_over_lookbacks():
    promote_row = lambda lb, ok: {"lookback": lb, "promote": ok}
    assert _sweep_blocked([promote_row(100, True), promote_row(200, True)]) is False
    assert _sweep_blocked([promote_row(100, False), promote_row(200, True)]) is True   # any fail
    assert _sweep_blocked([promote_row(200, True), promote_row(400, False)]) is True   # live ok, longer fails
    assert _sweep_blocked([{"lookback": 200, "adx_mean_abs": 0.1}]) is False           # no model -> no gate


# --- alignment helper -------------------------------------------------------------

def test_align_eval_start_is_window_tail():
    df_ext = _ohlcv(500, seed=8)
    df_window = df_ext.iloc[200:].reset_index(drop=True)
    assert _align_eval_start(df_window, df_ext) == 200


def test_align_eval_start_rejects_misaligned_frames():
    df_ext = _ohlcv(500, seed=9)
    df_window = _ohlcv(300, seed=99)        # unrelated prices -> not a tail of df_ext
    import pytest
    with pytest.raises(ValueError):
        _align_eval_start(df_window, df_ext)


# --- end-to-end (small, fast) -----------------------------------------------------

def test_validate_frames_end_to_end_small():
    df_ext = _ohlcv(260, seed=11)
    eval_start = 180
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    model = _fit_model(df_ext, filter_window=24)
    rep = validate_frames(df_window, df_ext, eval_start, model, period=PERIOD,
                          lookback=120, target="volatility", seed=0, horizons=(4,))
    assert rep["n_eval_bars"] == len(df_window)
    assert rep["adx_drift"]["n"] > 0
    assert 0.0 <= rep["model"]["label_drift"]["agreement"] <= 1.0
    assert isinstance(rep["go_no_go"]["promote"], bool)
    # bounded ADX at this lookback never beats the full-window warm-up: corr is high but
    # drift is non-negative by construction.
    assert rep["adx_drift"]["mean_abs"] >= 0.0


def test_validate_frames_handrule_only_without_model():
    df_ext = _ohlcv(220, seed=12)
    eval_start = 140
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    rep = validate_frames(df_window, df_ext, eval_start, None, period=PERIOD,
                          lookback=90, target="volatility", seed=0, horizons=(4,))
    assert "model" not in rep and "go_no_go" not in rep
    assert "handrule" in rep and rep["adx_drift"]["n"] > 0


# --- gate primary horizon guard (PR review finding 2) -----------------------------

def test_gate_primary_horizon_tracks_gate_default():
    # The guard must read the gate's own `primary` default, not a hardcoded 4, so a future
    # change to gate_verdict's primary keeps this in lockstep.
    from regime_calibrate import gate_verdict
    import inspect
    expected = int(str(inspect.signature(gate_verdict).parameters["primary"].default).lstrip("h"))
    assert _gate_primary_horizon() == expected == 4


def test_validate_frames_rejects_horizons_missing_gate_primary():
    # With a model present, the gate runs; omitting the gate's primary horizon from
    # `horizons` would make gate_verdict raise a deep KeyError mid-run. Reject up front.
    import pytest
    df_ext = _ohlcv(220, seed=31)
    eval_start = 140
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    model = _fit_model(df_ext, filter_window=24)
    with pytest.raises(ValueError, match=r"h4"):
        validate_frames(df_window, df_ext, eval_start, model, period=PERIOD,
                        lookback=90, horizons=(1, 12))


def test_validate_frames_handrule_only_allows_horizons_without_gate_primary():
    # No model => no gate => no h4 requirement. Hand-rule drift on horizons={1,12} is fine.
    df_ext = _ohlcv(220, seed=32)
    eval_start = 140
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    rep = validate_frames(df_window, df_ext, eval_start, None, period=PERIOD,
                          lookback=90, horizons=(1, 12))
    assert "go_no_go" not in rep and rep["adx_drift"]["n"] > 0


# --- in-sample / provenance guard (PR review finding 1) ---------------------------

def test_provenance_status_flags_in_sample_and_out_of_sample():
    fit = {"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"}
    model = {"fitted_on": fit}
    same = _provenance_status(model, "BTC/USDT", "1h", "is")
    assert same["in_sample"] is True and same["verified"] is True
    diff_window = _provenance_status(model, "BTC/USDT", "1h", "oos")
    assert diff_window["in_sample"] is False and diff_window["verified"] is True
    diff_symbol = _provenance_status(model, "ETH/USDT", "1h", "is")
    assert diff_symbol["in_sample"] is False     # different instrument is not in-sample
    missing = _provenance_status({"states": []}, "BTC/USDT", "1h", "is")
    assert missing["verified"] is False and missing["in_sample"] is False


def test_go_no_go_blocks_in_sample_rescore():
    # An in-sample re-score scores the gate optimistically; it must never promote even when
    # the bounded gate ships and agreement is perfect.
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    v = go_no_go(md, hr, md, hr, _drift(1.0), in_sample=True)
    assert v["promote"] is False
    assert v["in_sample"] is True
    assert any("in-sample re-score" in r for r in v["blocking_reasons"])
    assert v["bounded_window_verdict"]["ship"] is True   # gate ok; in-sample is the blocker


def test_go_no_go_unverified_provenance_warns_but_does_not_block_by_default():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    v = go_no_go(md, hr, md, hr, _drift(0.99), provenance_verified=False)
    assert v["promote"] is True                          # flagged, not blocked, by default
    assert v["provenance_verified"] is False
    assert v["warnings"] and any("provenance" in w for w in v["warnings"])
    assert v["blocking_reasons"] == []


def test_go_no_go_unverified_provenance_blocks_when_required():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    v = go_no_go(md, hr, md, hr, _drift(0.99), provenance_verified=False,
                 require_provenance=True)
    assert v["promote"] is False
    assert any("provenance unverifiable" in r for r in v["blocking_reasons"])


def test_go_no_go_promote_iff_no_blocking_reason():
    # Invariant: promote is the single negation of blocking_reasons -- a new blocking
    # condition can never be added while promote stays True.
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)
    ok = go_no_go(md, hr, md, hr, _drift(0.99))
    assert ok["promote"] is (ok["blocking_reasons"] == [])
    blocked = go_no_go(md, hr, md, hr, _drift(0.99), in_sample=True)
    assert blocked["promote"] is (blocked["blocking_reasons"] == [])


def test_validate_frames_in_sample_blocks_end_to_end():
    df_ext = _ohlcv(260, seed=33)
    eval_start = 180
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    model = _fit_model(df_ext, filter_window=24)
    rep = validate_frames(df_window, df_ext, eval_start, model, period=PERIOD,
                          lookback=120, target="volatility", seed=0, horizons=(4,),
                          in_sample=True)
    assert rep["go_no_go"]["promote"] is False
    assert any("in-sample re-score" in r for r in rep["go_no_go"]["blocking_reasons"])


def test_validate_frames_scores_incumbent_at_its_own_period():
    # Finding 2: the hand-rule incumbent arm is scored at `incumbent_period` (matching
    # regime_calibrate's gate), independent of the model's fit period — so the full-window
    # verdict here equals the calibrate verdict for the same model+window even when != 48.
    df_ext = _ohlcv(260, seed=21)
    eval_start = 150
    df_window = df_ext.iloc[eval_start:].reset_index(drop=True)
    feats = composite_feature_matrix(df_ext, 30, TH).to_numpy()
    labels = compute_regime_composite(df_ext, period=30, thresholds=TH)["regime"].to_numpy()
    model = fit_label_anchored_hmm(feats, labels, sorted(VALID_LABELS_COMPOSITE), filter_window=24)
    model["period"] = 30
    rep = validate_frames(df_window, df_ext, eval_start, model, incumbent_period=48,
                          lookback=110, target="volatility", seed=0, horizons=(4,))
    assert rep["model_period"] == 30
    assert rep["incumbent_period"] == 48
    assert rep["model"]["period"] == 30
    assert rep["handrule"]["period"] == 48
