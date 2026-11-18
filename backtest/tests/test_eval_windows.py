import numpy as np
import pandas as pd
import pytest

import eval_windows as ew


def _leg(sharpe, ddadj=0.0, trades=5, return_pct=-1.0, max_dd_pct=-10.0):
    return {"sharpe": sharpe, "ddadj": ddadj, "trades": trades,
            "return_pct": return_pct, "max_dd_pct": max_dd_pct,
            "bh_return_pct": -30.0}


def _bars(sharpe=-1.0, ddadj=-0.5, datasets=("d1", "d2", "d3", "d4")):
    return {ds: {"sharpe": sharpe, "ddadj": ddadj, "n": 8} for ds in datasets}


class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {"period": 2},
                                        "description": "test"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1
        sig[20::20] = -1
        out["signal"] = sig
        return out


def _synthetic_df(n=120):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_run_leg_threads_stop_loss_atr_mult(monkeypatch):
    df = _synthetic_df(n=240)
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    stopped = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                         ("2026-01-01", None), stop_loss_atr_mult=0.1)
    assert plain is not None and stopped is not None
    assert (stopped["return_pct"], stopped["max_dd_pct"]) != \
        (plain["return_pct"], plain["max_dd_pct"]), (
        "stop_loss_atr_mult=0.1 produced an identical leg — the stop never "
        "reached the Backtester")


def test_run_leg_threads_allowed_regimes_and_blocks_entries(monkeypatch):
    df = _synthetic_df(n=240)
    df = df.copy()
    df["regime"] = "trending_up"
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    gated = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None), allowed_regimes=["trending_down"])
    assert plain is not None and plain.get("trades", 0) > 0
    assert gated is not None
    assert gated.get("trades", -1) == 0, (
        "allowed_regimes gate did not suppress entries when bar regime "
        "was never in the allowed set")


_VALIDATE_CANDIDATE_ACCEPTS_CASES = [
    {"name": "x", "direction": "short"},
    {"name": "x", "direction": "short",
     "close_strategies": [{"name": "tp_at_pct", "params": {}}]},
    {"name": "x", "allowed_regimes": ["trending_down"]},
    {"name": "x", "direction": "short",
     "close_strategies": [{"name": "atr_stop", "params": {"atr_mult": 1.5}}],
     "allowed_regimes": ["ranging", "trending_down"]},
    {"name": "x", "allowed_regimes": ["trending_up"],
     "regime_period": 21, "regime_adx_threshold": 25.0},
    {"name": "x", "invert_signal": True},
    {"name": "x", "invert_signal": True, "type": "manual"},
    {"name": "x", "stop_loss_atr_mult": 2.0},
    {"name": "x", "trailing_stop_atr_mult": 2.5},
]


def test_validate_candidate_accepts(subtests):
    for i, candidate in enumerate(_VALIDATE_CANDIDATE_ACCEPTS_CASES):
        with subtests.test(case=i, candidate=candidate):
            assert ew.validate_candidate(candidate) is candidate


_VALIDATE_CANDIDATE_REJECTS_CASES = [
    ({"name": "x", "direction": "both"}, "silently dropped"),
    ({"name": "x", "direction": "sideways"}, "direction"),
    ({}, "name"),
    ({"name": "x", "invert_signal": True, "type": "spot"}, "invert_signal"),
    ({"name": "x", "stop_loss_atr_mult": 2.0,
      "trailing_stop_atr_mult": 2.5}, "mutually exclusive"),
    ({"name": "x", "allowed_regimes": "trending_down"}, "list of strings"),
    ({"name": "x", "allowed_regimes": ["trending_down", 123]}, "strings"),
    ({"name": "x", "regime_windows_spec": "medium"}, "regime_windows_spec"),
    ({"name": "x", "regime_windows_spec": {"regime": 14}}, "reserved"),
    ({"name": "x", "regime_windows_spec": {"medium": "fast"}},
     "regime_windows_spec"),
    ({"name": "x", "regime_directional_policy": "trending_up"},
     "regime_directional_policy"),
    ({"name": "x", "regime_directional_policy": {
        "trending_up": {"direction": "long"}}}, "trend_regime"),
    ({"name": "x", "regime_directional_policy": {"trend_regime": {
        "trending_up": {"direction": "flat"}}}}, "regime_directional_policy"),
    ({"name": "x", "regime_directional_policy": {"trend_regime": {
        "trending_down": {"direction": "both"}}}}, "close_strategies"),
    ({"name": "x", "regime_period": 21}, "gate consumer"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_windows_spec": {"medium": {"classifier": "adx", "period": 14}},
      "regime_period": 21}, "windows spec owns"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": True}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": 1}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": "14"}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": 14.0}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": True}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": 0}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": -5}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": "20"}, "regime_adx_threshold"),
]


def test_validate_candidate_rejects(subtests):
    for i, (candidate, match) in enumerate(_VALIDATE_CANDIDATE_REJECTS_CASES):
        with subtests.test(case=i, candidate=candidate, match=match):
            with pytest.raises(ValueError, match=match):
                ew.validate_candidate(candidate)


_VALIDATE_CANDIDATE_STRIPS_EMPTY_OPTIONAL_BLOCK_CASES = [
    ({"name": "x", "allowed_regimes": []}, "allowed_regimes"),
    ({"name": "x", "regime_windows_spec": {}}, "regime_windows_spec"),
    ({"name": "x", "regime_directional_policy": {}},
     "regime_directional_policy"),
]


def test_validate_candidate_strips_empty_optional_block(subtests):
    for i, (candidate, key) in enumerate(_VALIDATE_CANDIDATE_STRIPS_EMPTY_OPTIONAL_BLOCK_CASES):
        with subtests.test(case=i, candidate=candidate, key=key):
            assert ew.validate_candidate(candidate) is candidate
            assert key not in candidate


def test_validate_candidate_normalizes_regime_windows_spec(subtests):
    with subtests.test(case="test_validate_candidate_normalizes_regime_windows_spec"):
        c = {"name": "x", "regime_windows_spec": {"medium": 14}}
        assert ew.validate_candidate(c) is c
        assert c["regime_windows_spec"]["medium"]["classifier"] == "adx"
        assert c["regime_windows_spec"]["medium"]["period"] == 14

        c2 = {"name": "x",
              "regime_windows_spec": {"medium": {"classifier": "composite",
                                                 "period": 14}},
              "allowed_regimes": ["trending_up_clean"]}
        assert ew.validate_candidate(c2) is c2
        assert c2["regime_windows_spec"]["medium"]["classifier"] == "composite"

    with subtests.test(case="test_validate_candidate_accepts_regime_directional_policy"):
        c = {"name": "x", "direction": "both",
             "close_strategies": [{"name": "atr_stop",
                                   "params": {"atr_mult": 2.0}}],
             "regime_directional_policy": {"trend_regime": {
                 "trending_up": {"direction": "long"},
                 "trending_down": {"direction": "both"}}}}
        assert ew.validate_candidate(c) is c
        assert c["regime_directional_policy"]["trend_regime"]["trending_up"] == {
            "direction": "long", "invert_signal": False}


def test_evaluate_window_threads_regime_directional_policy(monkeypatch):
    seen = {}

    def _fake_run_leg(reg, name, params, symbol, timeframe, window, **kwargs):
        seen.update(kwargs)
        return {"sharpe": 1.0, "return_pct": 1.0, "max_dd_pct": 1.0,
                "ddadj": 1.0, "trades": 1, "bh_return_pct": 0.0,
                "span_days": 30.0}

    monkeypatch.setattr(ew, "run_leg", _fake_run_leg, raising=True)
    monkeypatch.setattr(ew, "incumbent_bars", lambda legs: {}, raising=True)
    monkeypatch.setattr(ew, "compute_incumbent_legs",
                        lambda *a, **k: {}, raising=True)
    policy = {"trend_regime": {"trending_up": {"direction": "long"}}}
    cand = {"name": "x", "direction": "both",
            "close_strategies": [{"name": "atr_stop",
                                  "params": {"atr_mult": 2.0}}],
            "regime_directional_policy": policy}
    ew.evaluate_window(None, cand, [("BTC/USDT", "1h")], "oos", 1000.0, {})
    assert seen.get("regime_directional_policy") == {"trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False}}}
