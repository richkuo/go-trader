import os
import sys
from unittest.mock import patch

import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_SPOT_DIR = os.path.dirname(os.path.abspath(__file__))
_SHARED_DIR = os.path.dirname(_SPOT_DIR)
sys.path.insert(0, _SPOT_DIR)
sys.path.insert(0, _SHARED_DIR)

_MOD = load_module("_spot_strategies_test", os.path.join(_SPOT_DIR, "strategies.py"))
_HELPERS = load_module("_spot_strategy_helpers_test", os.path.join(_SHARED_DIR, "conftest.py"))

STRATEGY_REGISTRY = _MOD.STRATEGY_REGISTRY
apply_strategy = _MOD.apply_strategy
list_strategies = _MOD.list_strategies
get_strategy = _MOD.get_strategy
make_ohlcv = _HELPERS.make_ohlcv
make_trending_up = _HELPERS.make_trending_up
make_flat = _HELPERS.make_flat
make_volatile = _HELPERS.make_volatile


def _run_strategy(name, closes, params=None, volume=None, index=None):
    return apply_strategy(name, make_ohlcv(closes, volume=volume, index=index), params)


def _assert_signal_contract(result, expected_index=None):
    assert isinstance(result, pd.DataFrame)
    if expected_index is not None:
        assert result.index.equals(expected_index)
    assert "signal" in result.columns
    signals = result["signal"].dropna()
    assert set(signals.unique()).issubset({-1, 0, 1})


def test_registry_exposes_visible_and_loadable_hidden_strategies():
    names = set(list_strategies())
    for expected in {
        "mean_reversion_pro", "liquidity_sweeps", "anchored_vwap",
        "chart_pattern", "momentum_pro", "atr_band_revert",
    }:
        assert expected in names
    for hidden in {
        "sma_crossover", "ema_crossover", "rsi", "macd", "momentum",
        "bollinger_bands", "mean_reversion", "supertrend", "parabolic_sar",
        "tema_cross", "regime_adaptive",
    }:
        assert hidden not in names
        assert hidden in STRATEGY_REGISTRY


def test_unknown_strategy_raises():
    with pytest.raises(ValueError, match="Unknown strategy"):
        get_strategy("nonexistent_strategy_xyz")


def test_apply_strategy_returns_signal_contract():
    df = make_ohlcv(make_trending_up(100))
    result = apply_strategy("sma_crossover", df)
    _assert_signal_contract(result, df.index)


class TestRsiMacdCombo:
    def test_buy_and_sell_signals(self):
        cases = [
            (list(np.linspace(120, 80, 60)) + list(np.linspace(80, 130, 60)), 1),
            (list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60)), -1),
        ]
        for closes, expected in cases:
            result = _run_strategy("rsi_macd_combo", closes)
            _assert_signal_contract(result)
            assert (result["signal"] == expected).any()

    def test_loosened_short_gate_catches_more_shorts(self):
        closes = (
            list(np.linspace(80, 160, 50)) + list(np.linspace(160, 100, 20)) +
            list(np.linspace(100, 115, 15)) + list(np.linspace(115, 60, 40))
        )
        strict = _run_strategy("rsi_macd_combo", closes)
        loose = _run_strategy("rsi_macd_combo", closes, {"rsi_short_min": 0})
        _assert_signal_contract(strict)
        _assert_signal_contract(loose)
        assert (loose["signal"] == -1).sum() > (strict["signal"] == -1).sum()

    def test_loosened_long_gate_does_not_reduce_longs(self):
        closes = (
            list(np.linspace(140, 70, 50)) + list(np.linspace(70, 130, 20)) +
            list(np.linspace(130, 115, 15)) + list(np.linspace(115, 180, 40))
        )
        strict = _run_strategy("rsi_macd_combo", closes)
        loose = _run_strategy("rsi_macd_combo", closes, {"rsi_long_max": 100})
        _assert_signal_contract(strict)
        _assert_signal_contract(loose)
        assert (loose["signal"] == 1).sum() >= (strict["signal"] == 1).sum()

    def test_params_are_forwarded_through_shim(self):
        result = _run_strategy(
            "rsi_macd_combo", list(np.linspace(140, 70, 80)),
            {"rsi_short_min": 30, "rsi_long_max": 70},
        )
        _assert_signal_contract(result)


class TestPairsSpread:
    def test_self_series_returns_signal_contract(self):
        result = _run_strategy("pairs_spread", make_volatile(100, amplitude=10))
        _assert_signal_contract(result)

    def test_close_b_is_used_for_spread(self):
        closes = make_volatile(80, center=100, amplitude=5)
        df = make_ohlcv(closes)
        df["close_b"] = make_volatile(80, center=50, amplitude=3, seed=99)
        result = apply_strategy("pairs_spread", df)
        _assert_signal_contract(result, df.index)
        assert "spread" in result.columns


class TestVwapReversion:
    def test_signal_contract_uses_datetime_index(self):
        n = 100
        index = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run_strategy(
            "vwap_reversion", make_volatile(n, center=100, amplitude=8), index=index
        )
        _assert_signal_contract(result, index)

    def test_internal_temporary_columns_do_not_leak(self):
        index = pd.date_range("2024-01-01", periods=50, freq="h")
        result = _run_strategy(
            "vwap_reversion", make_volatile(50, center=100, amplitude=5), index=index
        )
        _assert_signal_contract(result, index)
        assert not {"_day", "_tp_vol", "_cum_tp_vol", "_cum_vol"} & set(result.columns)


class TestRangeScalper:
    def test_range_bound_data_emits_both_directions(self):
        n = 60
        rng = np.random.RandomState(123)
        closes = 100 + 2 * np.sin(np.linspace(0, 6 * np.pi, n)) + rng.randn(n) * 0.2
        result = _run_strategy(
            "range_scalper", closes,
            {
                "bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.02,
                "vol_ratio": 1.1, "rsi_period": 5, "rsi_ob": 65, "rsi_os": 35,
            },
            volume=np.full(n, 50.0),
        )
        _assert_signal_contract(result)
        assert (result["signal"] == 1).any()
        assert (result["signal"] == -1).any()

    def test_trend_does_not_emit_range_entries(self):
        result = _run_strategy(
            "range_scalper", make_trending_up(80),
            {"bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.005,
             "rsi_period": 7, "rsi_ob": 70, "rsi_os": 30},
            volume=np.full(80, 500.0),
        )
        _assert_signal_contract(result)
        assert (result["signal"] == 0).all()

    def test_signal_does_not_repeat_without_a_new_cross(self):
        n = 60
        rng = np.random.RandomState(123)
        closes = 100 + 2 * np.sin(np.linspace(0, 6 * np.pi, n)) + rng.randn(n) * 0.2
        result = _run_strategy(
            "range_scalper", closes,
            {"bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.02,
             "vol_ratio": 1.1, "rsi_period": 5, "rsi_ob": 65, "rsi_os": 35},
            volume=np.full(n, 50.0),
        )
        _assert_signal_contract(result)
        signals = result.loc[result["signal"] != 0, "signal"]
        assert (signals == signals.shift(1)).sum() <= 1


class TestAmdIfvg:
    def test_signal_contract_and_session_output(self):
        n = 96
        index = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run_strategy(
            "amd_ifvg", make_volatile(n, center=100, amplitude=5), index=index
        )
        _assert_signal_contract(result, index)
        assert {"asian_high", "asian_low"}.issubset(result.columns)

    def test_short_data_is_silent(self):
        index = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run_strategy("amd_ifvg", [100.0, 101.0], index=index)
        _assert_signal_contract(result, index)
        assert (result["signal"] == 0).all()


_EDGE_STRATEGIES = (
    "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
    "mean_reversion", "momentum", "volume_weighted", "triple_ema",
    "rsi_macd_combo", "stoch_rsi", "supertrend", "atr_breakout",
    "heikin_ashi_ema", "parabolic_sar", "amd_ifvg", "range_scalper",
)


@pytest.mark.parametrize(
    "name,rows",
    [(name, 0) for name in _EDGE_STRATEGIES] + [(name, 1) for name in _EDGE_STRATEGIES],
)
def test_empty_and_single_row_keep_signal_contract(name, rows):
    if rows == 0:
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    else:
        df = make_ohlcv([100.0])
    result = apply_strategy(name, df)
    _assert_signal_contract(result, df.index)
    assert len(result) == rows


class TestSweepSqueezeCombo:
    def test_signal_contract_on_trend(self):
        df = make_ohlcv(make_trending_up(80))
        result = apply_strategy("sweep_squeeze_combo", df)
        _assert_signal_contract(result, df.index)

    def test_two_agreeing_components_create_consensus_buy(self):
        df = make_ohlcv(make_flat(50))
        fake_ls = df.copy()
        fake_ls["signal"] = 0
        fake_ls.iloc[25, fake_ls.columns.get_loc("signal")] = 1
        fake_sq = pd.Series(0, index=df.index)
        fake_sr = pd.Series(0, index=df.index)
        fake_sr.iloc[25] = 1
        with patch("sweep_squeeze_combo.liquidity_sweep_core", return_value=fake_ls), \
                patch("sweep_squeeze_combo._squeeze_signals", return_value=fake_sq), \
                patch("sweep_squeeze_combo._stoch_rsi_signals", return_value=fake_sr):
            result = apply_strategy("sweep_squeeze_combo", df, {"min_agree": 2})
        _assert_signal_contract(result, df.index)
        assert result.loc[result.index[25], "signal"] == 1
        assert result.loc[result.index[25], "buy_votes"] == 2

    def test_sweep_component_can_create_consensus_buy(self):
        prices = list(np.linspace(110, 95, 25)) + list(np.linspace(96, 105, 15))
        prices += list(np.linspace(105, 100, 15)) + [96.0]
        prices += list(np.linspace(98, 108, 25))
        df = make_ohlcv(prices, noise=0.3)
        df.loc[df.index[55], "low"] = 93.0
        df.loc[df.index[55], "close"] = 96.0
        result = apply_strategy(
            "sweep_squeeze_combo", df, {"swing_lookback": 5, "min_agree": 1}
        )
        _assert_signal_contract(result, df.index)
        assert (result["signal"] == 1).any()
