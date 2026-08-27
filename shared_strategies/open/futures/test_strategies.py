
import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

_futures_dir = os.path.dirname(os.path.abspath(__file__))
_spot_dir = os.path.join(_futures_dir, '..', 'spot')
_shared_dir = os.path.join(_futures_dir, '..')

sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

_spec = importlib.util.spec_from_file_location(
    "futures_strategies", os.path.join(_futures_dir, "strategies.py"))
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

STRATEGY_REGISTRY = _mod.STRATEGY_REGISTRY
apply_strategy = _mod.apply_strategy
list_strategies = _mod.list_strategies
get_strategy = _mod.get_strategy

_conftest_spec = importlib.util.spec_from_file_location(
    "conftest_helpers", os.path.join(_shared_dir, "conftest.py"))
_conftest_mod = importlib.util.module_from_spec(_conftest_spec)
_conftest_spec.loader.exec_module(_conftest_mod)

make_ohlcv = _conftest_mod.make_ohlcv
make_trending_up = _conftest_mod.make_trending_up
make_trending_down = _conftest_mod.make_trending_down
make_flat = _conftest_mod.make_flat
make_volatile = _conftest_mod.make_volatile



class TestFuturesRegistry:
    def test_strategies_registered(self):
        names = list_strategies()
        assert len(names) >= 10
        for expected in ["breakout", "delta_neutral_funding",
                         "mean_reversion_pro", "momentum_pro",
                         "anchored_vwap", "chart_pattern"]:
            assert expected in names, f"{expected} not registered"
        for quarantined in ["sma_crossover", "ema_crossover", "bollinger_bands",
                            "volume_weighted", "triple_ema", "rsi_macd_combo",
                            "momentum", "mean_reversion", "rsi", "macd",
                            "stoch_rsi", "supertrend", "squeeze_momentum",
                            "ichimoku_cloud", "atr_breakout", "heikin_ashi_ema",
                            "order_blocks", "parabolic_sar",
                            "triple_ema_bidir", "tema_cross_bd", "funding_skew",
                            "consolidation_range", "regime_adaptive"]:
            assert quarantined not in names, f"{quarantined} should be hidden"
            assert quarantined in STRATEGY_REGISTRY, f"{quarantined} must stay loadable"

    def test_get_unknown_strategy_raises(self):
        with pytest.raises(ValueError, match="Unknown strategy"):
            get_strategy("nonexistent_xyz")

    def test_apply_returns_dataframe(self):
        df = make_ohlcv(make_trending_up(100))
        result = apply_strategy("momentum", df)
        assert isinstance(result, pd.DataFrame)
        assert "signal" in result.columns



def _run(name, closes, params=None, volume=None, index=None):
    df = make_ohlcv(closes, volume=volume, index=index)
    return apply_strategy(name, df, params)


def _valid_signals(result):
    signals = result["signal"].dropna()
    assert set(signals.unique()).issubset({-1.0, 0.0, 1.0})



class TestSMACrossover:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 140, 60))
        result = _run("sma_crossover", closes)
        _valid_signals(result)
        assert "sma_fast" in result.columns
        assert "sma_slow" in result.columns
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("sma_crossover", make_flat(80))
        assert (result["signal"].dropna() == 0).all()



class TestEMACrossover:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("ema_crossover", closes)
        _valid_signals(result)
        assert "ema_fast" in result.columns
        assert "ema_slow" in result.columns
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("ema_crossover", make_flat(80))
        assert (result["signal"].dropna() == 0).all()



class TestBollingerBands:
    def test_buy_signal(self):
        closes = list(np.full(30, 100.0)) + list(np.linspace(100, 80, 10)) + list(np.linspace(80, 105, 10))
        result = _run("bollinger_bands", closes)
        assert "bb_middle" in result.columns
        assert "bb_upper" in result.columns
        assert "bb_lower" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("bollinger_bands", make_flat(40))
        assert (result["signal"] == 0).all()



class TestVolumeWeighted:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 30)) + list(np.linspace(80, 130, 30))
        volume = [100.0] * 60
        volume[35] = 500.0
        volume[36] = 500.0
        result = _run("volume_weighted", closes, volume=volume)
        assert "price_sma" in result.columns
        assert "vol_sma" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("volume_weighted", make_flat(40))
        assert (result["signal"] == 0).all()



class TestTripleEMA:
    def test_buy_signal(self):
        closes = make_trending_up(80)
        result = _run("triple_ema", closes)
        assert "ema_short" in result.columns
        assert "ema_mid" in result.columns
        assert "ema_long" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_flat_no_signal(self):
        result = _run("triple_ema", make_flat(80))
        assert (result["signal"].dropna() == 0).all()



class TestTripleEMABidir:
    def test_uptrend_enters_long(self):
        result = _run("triple_ema_bidir", make_trending_up(120))
        assert "ema_short" in result.columns
        _valid_signals(result)
        assert (result["position"] == 1).any()
        assert (result["signal"] == 1).any()

    def test_downtrend_enters_short(self):
        result = _run("triple_ema_bidir", make_trending_down(120))
        _valid_signals(result)
        assert (result["position"] == -1).any(), "bearish stack must emit position=-1"
        assert (result["signal"] == -1).any(), "bearish stack must emit short-entry signal"

    def test_flat_no_signal(self):
        result = _run("triple_ema_bidir", make_flat(80))
        assert (result["position"].dropna() == 0).all()
        assert (result["signal"].dropna() == 0).all()

    def test_direct_flip_signal_clamped(self):
        closes = list(make_trending_up(80, start=100, step=1.0)) + list(
            make_trending_down(80, start=180, step=1.0)
        )
        result = _run("triple_ema_bidir", closes)
        _valid_signals(result)
        assert result["signal"].min() >= -1
        assert result["signal"].max() <= 1

    def test_custom_params_applied(self):
        closes = make_trending_up(60)
        default = _run("triple_ema_bidir", closes)
        custom = _run("triple_ema_bidir", closes,
                      params={"short_period": 3, "mid_period": 10, "long_period": 30})
        assert not default["ema_short"].equals(custom["ema_short"])



class TestRSIMACDCombo:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("rsi_macd_combo", closes)
        assert "rsi" in result.columns
        assert "macd_line" in result.columns
        assert "macd_signal_line" in result.columns
        _valid_signals(result)
        assert (result["signal"] == 1).any() or (result["signal"] == -1).any()

    def test_default_gate_preserves_legacy_behavior(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60))
        result = _run("rsi_macd_combo", closes)
        _valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_loosened_short_gate_catches_more_shorts(self):
        closes = (list(np.linspace(80, 160, 50)) +
                  list(np.linspace(160, 100, 20)) +
                  list(np.linspace(100, 115, 15)) +
                  list(np.linspace(115, 60, 40)))
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, params={"rsi_short_min": 0})
        _valid_signals(strict)
        _valid_signals(loose)
        assert (loose["signal"] == -1).sum() > (strict["signal"] == -1).sum(), \
            "loosening rsi_short_min must allow more short signals"

    def test_loosened_long_gate_catches_more_longs(self):
        closes = (list(np.linspace(140, 70, 50)) +
                  list(np.linspace(70, 130, 20)) +
                  list(np.linspace(130, 115, 15)) +
                  list(np.linspace(115, 180, 40)))
        strict = _run("rsi_macd_combo", closes)
        loose = _run("rsi_macd_combo", closes, params={"rsi_long_max": 100})
        _valid_signals(strict)
        _valid_signals(loose)
        assert (loose["signal"] == 1).sum() >= (strict["signal"] == 1).sum()

    def test_params_forwarded_via_apply_strategy(self):
        closes = make_trending_down(80)
        result = _run("rsi_macd_combo", closes,
                      params={"rsi_short_min": 30, "rsi_long_max": 70})
        assert "rsi" in result.columns
        _valid_signals(result)

    def test_flat_no_signal(self):
        result = _run("rsi_macd_combo", make_flat(80))
        assert (result["signal"] == 0).all()



class TestMomentum:
    def test_buy_signal(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 120, 20))
        result = _run("momentum", closes, {"roc_period": 14, "threshold": 3.0})
        _valid_signals(result)
        assert "roc" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_signal(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 80, 20))
        result = _run("momentum", closes, {"roc_period": 14, "threshold": 3.0})
        _valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run("momentum", make_flat(60))
        assert (result["signal"] == 0).all()



class TestMeanReversion:
    def test_buy_on_dip(self):
        closes = (
            list(np.linspace(100, 100, 40)) +
            list(np.linspace(100, 80, 10)) +
            list(np.linspace(80, 95, 20))
        )
        result = _run("mean_reversion", closes)
        _valid_signals(result)
        assert "z_score" in result.columns

    def test_flat_no_signal(self):
        result = _run("mean_reversion", make_flat(60))
        assert (result["signal"] == 0).all()



class TestRSI:
    def test_produces_rsi_column(self):
        closes = make_volatile(80, amplitude=10)
        result = _run("rsi", closes)
        assert "rsi" in result.columns
        valid = result["rsi"].dropna()
        assert (valid >= 0).all() and (valid <= 100).all()

    def test_flat_no_signal(self):
        result = _run("rsi", make_flat(50))
        assert (result["signal"] == 0).all()



class TestMACD:
    def test_bullish_cross(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run("macd", closes)
        _valid_signals(result)
        assert (result["signal"] == 1).any()
        assert "macd_line" in result.columns

    def test_bearish_cross(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run("macd", closes)
        assert (result["signal"] == -1).any()



class TestBreakout:
    def test_upside_breakout(self):
        closes = list(np.linspace(100, 105, 30)) + [120, 122, 125, 128, 130]
        result = _run("breakout", closes)
        _valid_signals(result)
        assert "high_roll" in result.columns
        assert "atr" in result.columns

    def test_flat_no_breakout(self):
        result = _run("breakout", make_flat(40))
        assert (result["signal"] == 0).all()



class TestStochRSI:
    def test_columns(self):
        closes = make_volatile(80, amplitude=10)
        result = _run("stoch_rsi", closes)
        assert "stoch_k" in result.columns
        assert "stoch_d" in result.columns
        _valid_signals(result)

    def test_flat_data(self):
        result = _run("stoch_rsi", make_flat(80))
        assert "signal" in result.columns



class TestSupertrend:
    def test_output_columns(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 150, 60))
        result = _run("supertrend", closes)
        _valid_signals(result)
        assert "supertrend" in result.columns
        assert "st_direction" in result.columns

    def test_direction_computed(self):
        closes = make_trending_down(100, start=200, step=1.0)
        result = _run("supertrend", closes)
        dirs = result["st_direction"]
        assert set(dirs.unique()).issubset({-1, 0, 1})



class TestSqueezeMomentum:
    def test_columns(self):
        closes = make_volatile(100, amplitude=10)
        result = _run("squeeze_momentum", closes)
        assert "squeeze_on" in result.columns
        assert "squeeze_mom" in result.columns
        _valid_signals(result)

    def test_flat_no_signal(self):
        result = _run("squeeze_momentum", make_flat(60))
        assert (result["signal"] == 0).all()



class TestIchimokuCloud:
    def test_columns(self):
        closes = make_trending_up(120)
        result = _run("ichimoku_cloud", closes)
        for col in ["tenkan", "kijun", "senkou_a", "senkou_b"]:
            assert col in result.columns

    def test_short_data(self):
        result = _run("ichimoku_cloud", [100.0] * 20)
        assert (result["signal"] == 0).all()



class TestATRBreakout:
    def test_upside_breakout(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 10))
        result = _run("atr_breakout", closes, {"atr_period": 14, "multiplier": 1.0})
        _valid_signals(result)

    def test_flat_no_breakout(self):
        result = _run("atr_breakout", make_flat(50))
        assert (result["signal"] == 0).all()



class TestHeikinAshiEMA:
    def test_columns(self):
        closes = make_trending_up(80)
        result = _run("heikin_ashi_ema", closes)
        for col in ["ha_open", "ha_close", "ha_high", "ha_low", "ha_ema"]:
            assert col in result.columns
        _valid_signals(result)



class TestOrderBlocks:
    def test_flat_no_signal(self):
        result = _run("order_blocks", make_flat(80))
        assert (result["signal"] == 0).all()

    def test_no_crash_volatile(self):
        closes = make_volatile(100, amplitude=15)
        result = _run("order_blocks", closes)
        _valid_signals(result)



class TestVWAPReversion:
    def test_with_datetime_index(self):
        n = 100
        closes = make_volatile(n, center=100, amplitude=8)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run("vwap_reversion", closes, index=idx)
        assert "vwap" in result.columns
        _valid_signals(result)



class TestParabolicSAR:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 140, 60))
        result = _run("parabolic_sar", closes)
        _valid_signals(result)
        assert "sar" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_signal(self):
        closes = list(np.linspace(80, 140, 40)) + list(np.linspace(140, 70, 60))
        result = _run("parabolic_sar", closes)
        assert (result["signal"] == -1).any()

    def test_single_bar(self):
        result = _run("parabolic_sar", [100.0])
        assert result["sar"].isna().all()
        assert (result["signal"] == 0).all()



class TestDeltaNeutralFunding:
    def test_entry_on_high_funding(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.0005, "entry_threshold": 0.0001})
        assert result["signal"].iloc[-1] == -1

    def test_exit_on_low_funding(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.00003, "exit_threshold": 0.00005})
        assert result["signal"].iloc[-1] == 1

    def test_zero_funding_no_signal(self):
        result = _run("delta_neutral_funding", make_flat(20),
                       {"avg_funding_rate_7d": 0.0})
        assert (result["signal"] == 0).all()


    @staticmethod
    def _series_df(funding_values, freq="4h"):
        n = len(funding_values)
        idx = pd.date_range("2026-01-01", periods=n, freq=freq, tz="UTC")
        df = make_ohlcv(make_flat(n), index=idx)
        df["funding_rate"] = np.asarray(funding_values, dtype=float)
        return df

    def test_series_shorts_after_full_window_when_funding_rich(self):
        df = self._series_df([0.0005] * 60)
        out = apply_strategy("delta_neutral_funding", df,
                             {"entry_threshold": 0.0001, "exit_threshold": 0.00005})
        assert (out["signal"].iloc[:41] == 0).all()
        assert out["signal"].iloc[41] == -1
        assert out["signal"].iloc[-1] == -1

    def test_series_exits_when_funding_cheap(self):
        df = self._series_df([0.00003] * 60)
        out = apply_strategy("delta_neutral_funding", df,
                             {"entry_threshold": 0.0001, "exit_threshold": 0.00005})
        assert out["signal"].iloc[-1] == 1

    def test_series_holds_in_hysteresis_band(self):
        df = self._series_df([0.00007] * 60)
        out = apply_strategy("delta_neutral_funding", df,
                             {"entry_threshold": 0.0001, "exit_threshold": 0.00005})
        assert (out["signal"] == 0).all()

    def test_series_warmup_waits_for_full_window_of_real_data(self):
        df = self._series_df([np.nan] * 10 + [0.0005] * 50)
        out = apply_strategy("delta_neutral_funding", df,
                             {"entry_threshold": 0.0001, "exit_threshold": 0.00005})
        assert (out["signal"].iloc[:51] == 0).all()
        assert out["signal"].iloc[51] == -1

    def test_series_column_overrides_live_scalar(self):
        df = self._series_df([0.0005] * 60)
        out = apply_strategy("delta_neutral_funding", df,
                             {"avg_funding_rate_7d": 0.0})
        assert out["signal"].iloc[-1] == -1

    def test_series_signals_are_valid(self):
        df = self._series_df([0.0005] * 30 + [0.00001] * 30)
        out = apply_strategy("delta_neutral_funding", df)
        assert set(out["signal"].unique()).issubset({-1, 0, 1})



class TestChartPattern:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run("chart_pattern", closes)
        assert "signal" in result.columns
        _valid_signals(result)



class TestLiquiditySweeps:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run("liquidity_sweeps", closes)
        assert "signal" in result.columns
        _valid_signals(result)



class TestAMDIFVG:
    def test_returns_signal_column(self):
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        closes = make_volatile(n, center=100, amplitude=5)
        result = _run("amd_ifvg", closes, index=idx)
        assert "signal" in result.columns
        _valid_signals(result)
        assert "asian_high" in result.columns
        assert "asian_low" in result.columns

    def test_short_data_no_signal(self):
        idx = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run("amd_ifvg", [100.0, 101.0], index=idx)
        assert (result["signal"] == 0).all()

    def test_no_crash_flat(self):
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run("amd_ifvg", make_flat(n), index=idx)
        assert "signal" in result.columns



class TestEdgeCases:
    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "bollinger_bands",
        "volume_weighted", "triple_ema", "triple_ema_bidir", "rsi_macd_combo",
        "momentum", "mean_reversion", "rsi", "macd", "breakout",
        "stoch_rsi", "supertrend", "squeeze_momentum",
        "atr_breakout", "heikin_ashi_ema", "parabolic_sar",
        "ichimoku_cloud", "order_blocks", "delta_neutral_funding",
        "vwap_reversion", "chart_pattern", "liquidity_sweeps", "amd_ifvg",
    ])
    def test_empty_dataframe(self, name):
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy(name, df)
        assert len(result) == 0

    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "bollinger_bands",
        "volume_weighted", "triple_ema", "triple_ema_bidir", "rsi_macd_combo",
        "momentum", "mean_reversion", "rsi", "macd", "breakout",
        "stoch_rsi", "atr_breakout", "heikin_ashi_ema",
        "supertrend", "squeeze_momentum", "ichimoku_cloud",
        "order_blocks", "delta_neutral_funding",
        "chart_pattern", "liquidity_sweeps", "parabolic_sar", "amd_ifvg",
    ])
    def test_single_row(self, name):
        df = make_ohlcv([100.0])
        result = apply_strategy(name, df)
        assert len(result) == 1
        assert "signal" in result.columns
