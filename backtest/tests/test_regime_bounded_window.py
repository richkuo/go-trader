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
from regime_bounded_window_validate import DEFAULT_AGREEMENT_THRESHOLD, _align_eval_start, _sweep_blocked, bounded_window_adx, bounded_window_views, go_no_go, label_drift_stats

TH = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
PERIOD = 48


def _ohlcv(n, seed=0, regime_mix=True):
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


def _fit_model(df, filter_window=24):
    feats = composite_feature_matrix(df, PERIOD, TH).to_numpy()
    labels = compute_regime_composite(df, period=PERIOD, thresholds=TH)["regime"].to_numpy()
    model = fit_label_anchored_hmm(feats, labels, sorted(VALID_LABELS_COMPOSITE),
                                   filter_window=filter_window)
    model["period"] = PERIOD
    return model


def test_bounded_adx_respects_eval_start():
    df = _ohlcv(160, seed=5)
    out = bounded_window_adx(df, PERIOD, 60, TH["adx"], eval_start=100)
    assert np.isnan(out[:100]).all()
    assert (~np.isnan(out[150:])).any()


def test_bounded_views_last_bar_matches_one_shot_live_call():
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


def _scored(kw_h, transition_rate, p_value=0.005):
    return {"stability": {"transition_rate": transition_rate},
            "h4": {"separation": {"kruskal_h": kw_h}, "significance": {"p_value": p_value}}}


def _drift(agreement, n=500):
    return {"n": n, "agreement": agreement,
            "disagreements": int(round((1 - agreement) * n)), "transitions": {}}


def test_go_no_go_promotion_and_every_blocking_reason():
    hr = _scored(10.0, 0.40)
    md = _scored(9.7, 0.25)

    case = "test_go_no_go_promotes_when_bounded_ships_and_agreement_high"
    v = go_no_go(md, hr, md, hr, _drift(0.99))
    assert v["promote"] is True, case
    assert v["blocking_reasons"] == [], case
    assert v["bounded_window_verdict"]["ship"] is True, case

    case = "test_go_no_go_blocks_when_bounded_fails_gate"
    md_bounded = _scored(4.0, 0.25)
    v = go_no_go(md, hr, md_bounded, hr, _drift(0.99))
    assert v["promote"] is False, case
    assert any("fails the calibrate gate" in r for r in v["blocking_reasons"]), case
    assert any("regressed" in r for r in v["blocking_reasons"]), case

    case = "test_go_no_go_blocks_when_label_agreement_below_threshold"
    v = go_no_go(md, hr, md, hr, _drift(0.80))
    assert v["promote"] is False, case
    assert any("label agreement" in r for r in v["blocking_reasons"]), case
    assert v["bounded_window_verdict"]["ship"] is True, case

    case = "test_go_no_go_default_threshold"
    just_under = go_no_go(md, hr, md, hr, _drift(DEFAULT_AGREEMENT_THRESHOLD - 0.001))
    at = go_no_go(md, hr, md, hr, _drift(DEFAULT_AGREEMENT_THRESHOLD))
    assert just_under["promote"] is False and at["promote"] is True, case

    case = "test_go_no_go_blocks_on_insufficient_comparable_bars"
    v = go_no_go(md, hr, md, hr, _drift(1.0, n=5), min_agreement_bars=30)
    assert v["promote"] is False, case
    assert any("insufficient comparable bars" in r for r in v["blocking_reasons"]), case
    assert v["bounded_window_verdict"]["ship"] is True, case

    case = "test_go_no_go_zero_comparable_bars_vacuous_agreement_blocks"
    empty = label_drift_stats(np.array([], dtype=object), np.array([], dtype=object),
                              np.array([], dtype=bool))
    assert empty["agreement"] == 1.0 and empty["n"] == 0, case
    v = go_no_go(md, hr, md, hr, empty)
    assert v["promote"] is False, case
    assert any("insufficient comparable bars" in r for r in v["blocking_reasons"]), case

    case = "test_go_no_go_blocks_in_sample_rescore"
    v = go_no_go(md, hr, md, hr, _drift(1.0), in_sample=True)
    assert v["promote"] is False, case
    assert v["in_sample"] is True, case
    assert any("in-sample re-score" in r for r in v["blocking_reasons"]), case
    assert v["bounded_window_verdict"]["ship"] is True, case

    case = "test_go_no_go_unverified_provenance_warns_but_does_not_block_by_default"
    v = go_no_go(md, hr, md, hr, _drift(0.99), provenance_verified=False)
    assert v["promote"] is True, case
    assert v["provenance_verified"] is False, case
    assert v["warnings"] and any("provenance" in w for w in v["warnings"]), case
    assert v["blocking_reasons"] == [], case

    case = "test_go_no_go_unverified_provenance_blocks_when_required"
    v = go_no_go(md, hr, md, hr, _drift(0.99), provenance_verified=False,
                 require_provenance=True)
    assert v["promote"] is False, case
    assert any("provenance unverifiable" in r for r in v["blocking_reasons"]), case


def test_sweep_blocked_worst_case_over_lookbacks():
    promote_row = lambda lb, ok: {"lookback": lb, "promote": ok}
    assert _sweep_blocked([promote_row(100, True), promote_row(200, True)]) is False
    assert _sweep_blocked([promote_row(100, False), promote_row(200, True)]) is True
    assert _sweep_blocked([promote_row(200, True), promote_row(400, False)]) is True
    assert _sweep_blocked([{"lookback": 200, "adx_mean_abs": 0.1}]) is False


def test_align_eval_start_is_window_tail_and_rejects_misaligned_frames():
    import pytest
    df_ext = _ohlcv(500, seed=8)
    df_window = df_ext.iloc[200:].reset_index(drop=True)
    assert _align_eval_start(df_window, df_ext) == 200, "test_align_eval_start_is_window_tail"

    df_ext = _ohlcv(500, seed=9)
    df_window = _ohlcv(300, seed=99)
    with pytest.raises(ValueError):
        _align_eval_start(df_window, df_ext)


import json

import regime_bounded_window_validate as rbwv


def _wire_loader(monkeypatch, n_ext=200, n_window=140, seed=40):
    df_ext = _ohlcv(n_ext, seed=seed)
    df_window = df_ext.iloc[n_ext - n_window:].reset_index(drop=True)

    def loader(symbol, timeframe, exchange_id=None, start_date=None, end_date=None):
        return df_ext if start_date is None else df_window

    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data", loader)
    return df_ext, df_window


def _stub_gate_ship(monkeypatch):
    monkeypatch.setattr(rbwv, "gate_verdict", lambda hr, md, primary="h4": {"ship": True})


def _write_model(tmp_path, df, fitted_on=None):
    model = _fit_model(df, filter_window=24)
    if fitted_on is not None:
        model["fitted_on"] = fitted_on
    path = tmp_path / "model.json"
    path.write_text(json.dumps({"model": model}))
    return str(path)


def _argv(model_json, window, lookback=250, extra=None):
    a = ["--symbol", "BTC/USDT", "--timeframe", "1h", "--window", window,
         "--model-json", model_json, "--lookback", str(lookback), "--horizons", "4",
         "--agreement-threshold", "0", "--min-agreement-bars", "1", "--seed", "0"]
    return a + (extra or [])


def test_main_and_validate_block_in_sample_and_promote_disjoint_oos(monkeypatch, tmp_path, capsys):
    df_ext, _ = _wire_loader(monkeypatch)
    _stub_gate_ship(monkeypatch)
    fit = {"symbol": "BTC/USDT", "timeframe": "1h", "window": "is"}
    mj = _write_model(tmp_path, df_ext, fitted_on=fit)
    capsys.readouterr()

    assert rbwv.main(_argv(mj, "is")) == 1
    rep = json.loads(capsys.readouterr().out)
    assert rep["provenance"]["in_sample"] is True
    assert rep["go_no_go"]["promote"] is False
    assert any("in-sample re-score" in r for r in rep["go_no_go"]["blocking_reasons"])

    assert rbwv.main(_argv(mj, "oos")) == 0
    rep = json.loads(capsys.readouterr().out)
    assert rep["provenance"]["in_sample"] is False
    assert rep["go_no_go"]["provenance_verified"] is True
    assert rep["go_no_go"]["promote"] is True
