import os
import sys
import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module

_FUTURES_DIR = os.path.dirname(os.path.abspath(__file__))
_SPOT_DIR = os.path.join(_FUTURES_DIR, "..", "spot")
_SHARED_DIR = os.path.join(_FUTURES_DIR, "..")
sys.path.insert(0, _SPOT_DIR)
sys.path.insert(0, _SHARED_DIR)

_MOD = load_module("_futures_strategies_test", os.path.join(_FUTURES_DIR, "strategies.py"))
_HELPERS = load_module("_futures_strategy_helpers_test", os.path.join(_SHARED_DIR, "conftest.py"))

STRATEGY_REGISTRY = _MOD.STRATEGY_REGISTRY
apply_strategy = _MOD.apply_strategy
list_strategies = _MOD.list_strategies
get_strategy = _MOD.get_strategy
make_ohlcv = _HELPERS.make_ohlcv
make_trending_up = _HELPERS.make_trending_up
make_trending_down = _HELPERS.make_trending_down
make_flat = _HELPERS.make_flat
make_volatile = _HELPERS.make_volatile


def _run(name, closes, params=None, volume=None, index=None):
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
        "breakout", "delta_neutral_funding", "mean_reversion_pro",
        "momentum_pro", "anchored_vwap", "chart_pattern",
    }:
        assert expected in names
    for hidden in {
        "sma_crossover", "ema_crossover", "bollinger_bands", "volume_weighted",
        "triple_ema", "rsi_macd_combo", "momentum", "mean_reversion", "rsi",
        "macd", "stoch_rsi", "supertrend", "squeeze_momentum", "ichimoku_cloud",
        "atr_breakout", "heikin_ashi_ema", "order_blocks", "parabolic_sar",
        "triple_ema_bidir", "tema_cross_bd", "funding_skew",
        "consolidation_range", "regime_adaptive",
    }:
        assert hidden not in names
        assert hidden in STRATEGY_REGISTRY


def test_unknown_strategy_raises():
    with pytest.raises(ValueError, match="Unknown strategy"):
        get_strategy("nonexistent_xyz")


def test_apply_strategy_returns_signal_contract():
    df = make_ohlcv(make_trending_up(100))
    result = apply_strategy("momentum", df)
    _assert_signal_contract(result, df.index)


class TestTripleEmaBidir:
    @pytest.mark.parametrize("maker,expected", [
        (make_trending_up, 1),
        (make_trending_down, -1),
    ])
    def test_trend_direction_enters_matching_side(self, maker, expected):
        result = _run("triple_ema_bidir", maker(120))
        _assert_signal_contract(result)
        assert (result["position"] == expected).any()
        assert (result["signal"] == expected).any()

    def test_direct_flip_signal_is_clamped(self):
        closes = list(make_trending_up(80, start=100, step=1.0)) + list(
            make_trending_down(80, start=180, step=1.0)
        )
        result = _run("triple_ema_bidir", closes)
        _assert_signal_contract(result)

    def test_custom_periods_change_the_ema_series(self):
        closes = make_trending_up(60)
        default = _run("triple_ema_bidir", closes)
        custom = _run(
            "triple_ema_bidir", closes,
            {"short_period": 3, "mid_period": 10, "long_period": 30},
        )
        assert not default["ema_short"].equals(custom["ema_short"])


class TestRsiMacdCombo:
    def test_default_gate_keeps_short_signal(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60))
        result = _run("rsi_macd_combo", closes)
        _assert_signal_contract(result)
        assert (result["signal"] == -1).any()

    def test_loosened_short_gate_catches_more_shorts(self):
        closes = (
            list(np.linspace(80, 160, 50)) + list(np.linspace(160, 100, 20)) +
            list(np.linspace(100, 115, 15)) + list(np.linspace(115, 60, 40))
        )
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, {"rsi_short_min": 0})
        _assert_signal_contract(strict)
        _assert_signal_contract(loose)
        assert (loose["signal"] == -1).sum() > (strict["signal"] == -1).sum()

    def test_loosened_long_gate_does_not_reduce_longs(self):
        closes = (
            list(np.linspace(140, 70, 50)) + list(np.linspace(70, 130, 20)) +
            list(np.linspace(130, 115, 15)) + list(np.linspace(115, 180, 40))
        )
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, {"rsi_long_max": 100})
        _assert_signal_contract(strict)
        _assert_signal_contract(loose)
        assert (loose["signal"] == 1).sum() >= (strict["signal"] == 1).sum()

    def test_params_are_forwarded_through_shim(self):
        result = _run(
            "rsi_macd_combo", make_trending_down(80),
            {"rsi_short_min": 30, "rsi_long_max": 70},
        )
        _assert_signal_contract(result)


class TestBreakout:
    def test_upside_breakout_emits_long_signal(self):
        closes = list(np.linspace(100, 105, 30)) + [120, 122, 125, 128, 130]
        result = _run("breakout", closes)
        _assert_signal_contract(result)
        assert (result["signal"] == 1).any()

    def test_flat_market_stays_silent(self):
        result = _run("breakout", make_flat(40))
        _assert_signal_contract(result)
        assert (result["signal"] == 0).all()


class TestVwapReversion:
    def test_signal_contract_uses_datetime_index(self):
        n = 100
        index = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run(
            "vwap_reversion", make_volatile(n, center=100, amplitude=8), index=index
        )
        _assert_signal_contract(result, index)


class TestDeltaNeutralFunding:
    def test_scalar_funding_controls_entry_and_exit(self):
        cases = [
            (0.0005, 0.0001, -1),
            (0.00003, 0.00005, 1),
            (0.0, 0.00005, 0),
        ]
        for funding, threshold, expected in cases:
            result = _run(
                "delta_neutral_funding", make_flat(20),
                {"avg_funding_rate_7d": funding, "entry_threshold": 0.0001,
                 "exit_threshold": threshold},
            )
            _assert_signal_contract(result)
            assert result["signal"].iloc[-1] == expected

    @staticmethod
    def _series_df(funding_values, freq="4h"):
        n = len(funding_values)
        index = pd.date_range("2026-01-01", periods=n, freq=freq, tz="UTC")
        df = make_ohlcv(make_flat(n), index=index)
        df["funding_rate"] = np.asarray(funding_values, dtype=float)
        return df

    def test_series_requires_full_window_before_entry(self):
        df = self._series_df([np.nan] * 10 + [0.0005] * 50)
        out = apply_strategy(
            "delta_neutral_funding", df,
            {"entry_threshold": 0.0001, "exit_threshold": 0.00005},
        )
        _assert_signal_contract(out, df.index)
        assert (out["signal"].iloc[:51] == 0).all()
        assert out["signal"].iloc[51] == -1

    def test_series_column_overrides_scalar_and_supports_hysteresis(self):
        rich = self._series_df([0.0005] * 60)
        rich_out = apply_strategy("delta_neutral_funding", rich, {"avg_funding_rate_7d": 0.0})
        _assert_signal_contract(rich_out, rich.index)
        assert rich_out["signal"].iloc[-1] == -1

        neutral = self._series_df([0.00007] * 60)
        neutral_out = apply_strategy(
            "delta_neutral_funding", neutral,
            {"entry_threshold": 0.0001, "exit_threshold": 0.00005},
        )
        _assert_signal_contract(neutral_out, neutral.index)
        assert (neutral_out["signal"] == 0).all()


class TestAmdIfvg:
    def test_signal_contract_and_session_output(self):
        n = 96
        index = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run(
            "amd_ifvg", make_volatile(n, center=100, amplitude=5), index=index
        )
        _assert_signal_contract(result, index)
        assert {"asian_high", "asian_low"}.issubset(result.columns)

    def test_short_data_is_silent(self):
        index = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run("amd_ifvg", [100.0, 101.0], index=index)
        _assert_signal_contract(result, index)
        assert (result["signal"] == 0).all()


_EMPTY_STRATEGIES = (
    "sma_crossover", "ema_crossover", "bollinger_bands", "volume_weighted",
    "triple_ema", "triple_ema_bidir", "rsi_macd_combo", "momentum",
    "mean_reversion", "rsi", "macd", "breakout", "stoch_rsi", "supertrend",
    "squeeze_momentum", "atr_breakout", "heikin_ashi_ema", "parabolic_sar",
    "ichimoku_cloud", "order_blocks", "delta_neutral_funding", "vwap_reversion",
    "chart_pattern", "liquidity_sweeps", "amd_ifvg",
)

_SINGLE_ROW_STRATEGIES = tuple(name for name in _EMPTY_STRATEGIES if name != "delta_neutral_funding")


@pytest.mark.parametrize(
    "name,rows",
    [(name, 0) for name in _EMPTY_STRATEGIES] +
    [(name, 1) for name in _SINGLE_ROW_STRATEGIES],
)
def test_empty_and_single_row_keep_signal_contract(name, rows):
    if rows == 0:
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    else:
        df = make_ohlcv([100.0])
    result = apply_strategy(name, df)
    _assert_signal_contract(result, df.index)
    assert len(result) == rows
