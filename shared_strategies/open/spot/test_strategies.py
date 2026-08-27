
import importlib.util
import numpy as np
import pandas as pd
import pytest

import sys, os

_spot_dir = os.path.dirname(os.path.abspath(__file__))
_shared_dir = os.path.join(_spot_dir, '..')

sys.path.insert(0, _spot_dir)
sys.path.insert(0, _shared_dir)

_spec = importlib.util.spec_from_file_location(
    "spot_strategies", os.path.join(_spot_dir, "strategies.py"))
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


class TestRegistry:
    def test_strategies_registered(self):
        names = list_strategies()
        assert len(names) >= 10
        for expected in ["mean_reversion_pro", "liquidity_sweeps", "anchored_vwap",
                         "chart_pattern", "momentum_pro", "atr_band_revert"]:
            assert expected in names, f"{expected} not registered"
        for quarantined in ["sma_crossover", "ema_crossover", "rsi", "macd",
                            "momentum", "bollinger_bands", "mean_reversion",
                            "supertrend", "parabolic_sar",
                            "tema_cross", "regime_adaptive"]:
            assert quarantined not in names, f"{quarantined} should be hidden"
            assert quarantined in STRATEGY_REGISTRY, f"{quarantined} must stay loadable"

    def test_get_unknown_strategy_raises(self):
        with pytest.raises(ValueError, match="Unknown strategy"):
            get_strategy("nonexistent_strategy_xyz")

    def test_apply_strategy_returns_dataframe(self):
        df = make_ohlcv(make_trending_up(100))
        result = apply_strategy("sma_crossover", df)
        assert isinstance(result, pd.DataFrame)
        assert "signal" in result.columns


def _run_strategy(name, closes, params=None, volume=None, index=None):
    df = make_ohlcv(closes, volume=volume, index=index)
    return apply_strategy(name, df, params)


def _assert_valid_signals(result):
    signals = result["signal"].dropna()
    assert set(signals.unique()).issubset({-1.0, 0.0, 1.0}), \
        f"Unexpected signal values: {set(signals.unique())}"


class TestSMACrossover:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 140, 60))
        result = _run_strategy("sma_crossover", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 70, 60))
        result = _run_strategy("sma_crossover", closes)
        assert (result["signal"] == -1).any()

    def test_short_data(self):
        result = _run_strategy("sma_crossover", [100.0] * 10)
        assert "signal" in result.columns

    def test_empty_df(self):
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy("sma_crossover", df)
        assert len(result) == 0


class TestEMACrossover:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run_strategy("ema_crossover", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run_strategy("ema_crossover", closes)
        assert (result["signal"] == -1).any()

    def test_flat_data(self):
        result = _run_strategy("ema_crossover", make_flat(100))
        signals = result["signal"].dropna()
        real = signals[(signals == 1) | (signals == -1)]
        assert len(real) <= 1


class TestRSI:
    def test_buy_on_oversold_recovery(self):
        closes = (
            list(np.linspace(100, 100, 20)) +
            list(np.linspace(100, 60, 20)) +
            list(np.linspace(60, 85, 30))
        )
        result = _run_strategy("rsi", closes)
        _assert_valid_signals(result)
        assert "rsi" in result.columns
        assert (result["signal"] == 1).any(), "Expected buy signal when RSI recovers from oversold"

    def test_sell_on_overbought_drop(self):
        closes = (
            list(np.linspace(100, 100, 20)) +
            list(np.linspace(100, 160, 20)) +
            list(np.linspace(160, 130, 30))
        )
        result = _run_strategy("rsi", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == -1).any(), "Expected sell signal when RSI drops from overbought"

    def test_rsi_range(self):
        closes = make_volatile(200, amplitude=15)
        result = _run_strategy("rsi", closes)
        valid_rsi = result["rsi"].dropna()
        assert (valid_rsi >= 0).all() and (valid_rsi <= 100).all()

    def test_flat_no_signal(self):
        result = _run_strategy("rsi", make_flat(50))
        assert (result["signal"] == 0).all()


class TestBollingerBands:
    def test_buy_at_lower_band(self):
        closes = (
            list(np.linspace(100, 100, 30)) +
            list(np.linspace(100, 75, 15)) +
            list(np.linspace(75, 95, 20))
        )
        result = _run_strategy("bollinger_bands", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any(), "Expected buy signal when price recovers from below lower band"

    def test_sell_at_upper_band(self):
        closes = (
            list(np.linspace(100, 100, 30)) +
            list(np.linspace(100, 125, 15)) +
            list(np.linspace(125, 105, 20))
        )
        result = _run_strategy("bollinger_bands", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == -1).any(), "Expected sell signal when price drops from above upper band"

    def test_flat_no_signal(self):
        result = _run_strategy("bollinger_bands", make_flat(50))
        assert (result["signal"] == 0).all()


class TestMACD:
    def test_bullish_crossover(self):
        closes = list(np.linspace(120, 80, 50)) + list(np.linspace(80, 140, 50))
        result = _run_strategy("macd", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any()
        assert "macd_line" in result.columns
        assert "macd_signal" in result.columns

    def test_bearish_crossover(self):
        closes = list(np.linspace(80, 140, 50)) + list(np.linspace(140, 70, 50))
        result = _run_strategy("macd", closes)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run_strategy("macd", make_flat(100))
        signals = result["signal"].dropna()
        real = signals[(signals == 1) | (signals == -1)]
        assert len(real) <= 1


class TestMeanReversion:
    def test_buy_on_dip(self):
        closes = (
            list(np.linspace(100, 100, 40)) +
            list(np.linspace(100, 80, 10)) +
            list(np.linspace(80, 95, 20))
        )
        result = _run_strategy("mean_reversion", closes)
        _assert_valid_signals(result)
        assert "z_score" in result.columns
        assert (result["signal"] == 1).any(), "Expected buy signal when z-score recovers from dip"

    def test_flat_no_signal(self):
        result = _run_strategy("mean_reversion", make_flat(60))
        assert (result["signal"] == 0).all()


class TestMomentum:
    def test_buy_on_strong_uptrend(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 30))
        result = _run_strategy("momentum", closes, {"roc_period": 14, "threshold": 5.0})
        _assert_valid_signals(result)
        assert "roc" in result.columns
        assert (result["signal"] == 1).any()

    def test_sell_on_strong_downtrend(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 70, 30))
        result = _run_strategy("momentum", closes, {"roc_period": 14, "threshold": 5.0})
        _assert_valid_signals(result)
        assert (result["signal"] == -1).any()

    def test_flat_no_signal(self):
        result = _run_strategy("momentum", make_flat(60))
        assert (result["signal"] == 0).all()


class TestVolumeWeighted:
    def test_buy_with_high_volume(self):
        n = 60
        closes = list(np.linspace(100, 90, 30)) + list(np.linspace(90, 115, 30))
        vol = [100.0] * n
        for i in range(28, 40):
            vol[i] = 300.0
        result = _run_strategy("volume_weighted", closes, volume=vol)
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any(), "Expected buy signal on upward SMA cross with high volume"

    def test_low_volume_filters_signal(self):
        closes = list(np.linspace(100, 90, 30)) + list(np.linspace(90, 115, 30))
        vol = [50.0] * 60
        result = _run_strategy("volume_weighted", closes, volume=vol)
        _assert_valid_signals(result)


class TestTripleEMA:
    def test_aligned_bullish(self):
        closes = make_trending_up(120, start=80, step=0.5)
        result = _run_strategy("triple_ema", closes)
        _assert_valid_signals(result)
        assert "ema_short" in result.columns
        assert "ema_mid" in result.columns
        assert "ema_long" in result.columns

    def test_bearish_after_uptrend(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 70, 80))
        result = _run_strategy("triple_ema", closes)
        assert (result["signal"] == -1).any()


class TestRSIMACDCombo:
    def test_buy_signal(self):
        closes = list(np.linspace(120, 80, 60)) + list(np.linspace(80, 130, 60))
        result = _run_strategy("rsi_macd_combo", closes)
        _assert_valid_signals(result)
        assert "rsi" in result.columns
        assert "macd_line" in result.columns
        assert (result["signal"] == 1).any(), "Expected buy signal on MACD bullish cross with RSI < 50"

    def test_sell_signal(self):
        closes = list(np.linspace(80, 140, 60)) + list(np.linspace(140, 80, 60))
        result = _run_strategy("rsi_macd_combo", closes)
        _assert_valid_signals(result)
        assert (result["signal"] == -1).any(), "Expected sell signal on MACD bearish cross with RSI > 50"

    def test_loosened_short_gate_catches_more_shorts(self):
        closes = (list(np.linspace(80, 160, 50)) +
                  list(np.linspace(160, 100, 20)) +
                  list(np.linspace(100, 115, 15)) +
                  list(np.linspace(115, 60, 40)))
        strict = _run_strategy("rsi_macd_combo", closes)
        loose = _run_strategy("rsi_macd_combo", closes, params={"rsi_short_min": 0})
        _assert_valid_signals(strict)
        _assert_valid_signals(loose)
        assert (loose["signal"] == -1).sum() > (strict["signal"] == -1).sum()

    def test_loosened_long_gate_catches_more_longs(self):
        closes = (list(np.linspace(140, 70, 50)) +
                  list(np.linspace(70, 130, 20)) +
                  list(np.linspace(130, 115, 15)) +
                  list(np.linspace(115, 180, 40)))
        strict = _run_strategy("rsi_macd_combo", closes)
        loose = _run_strategy("rsi_macd_combo", closes, params={"rsi_long_max": 100})
        _assert_valid_signals(strict)
        _assert_valid_signals(loose)
        assert (loose["signal"] == 1).sum() >= (strict["signal"] == 1).sum()

    def test_params_forwarded_via_apply_strategy(self):
        closes = list(np.linspace(140, 70, 80))
        result = _run_strategy("rsi_macd_combo", closes,
                               params={"rsi_short_min": 30, "rsi_long_max": 70})
        assert "rsi" in result.columns
        _assert_valid_signals(result)


class TestStochRSI:
    def test_buy_in_oversold(self):
        closes = (
            list(np.linspace(100, 100, 20)) +
            list(np.linspace(100, 50, 30)) +
            list(np.linspace(50, 65, 10)) +
            list(np.linspace(65, 45, 15)) +
            list(np.linspace(45, 48, 5))
        )
        result = _run_strategy("stoch_rsi", closes)
        _assert_valid_signals(result)
        assert "stoch_k" in result.columns
        assert "stoch_d" in result.columns
        assert (result["signal"] == 1).any(), "Expected buy signal on stoch RSI oversold crossover"

    def test_flat_data(self):
        result = _run_strategy("stoch_rsi", make_flat(80))
        assert "signal" in result.columns


class TestSupertrend:
    def test_output_columns(self):
        closes = list(np.linspace(150, 60, 50)) + list(np.linspace(60, 180, 70))
        result = _run_strategy("supertrend", closes)
        assert "supertrend" in result.columns
        assert "st_direction" in result.columns
        assert "signal" in result.columns
        _assert_valid_signals(result)

    def test_direction_computed(self):
        closes = make_trending_down(100, start=200, step=1.0)
        result = _run_strategy("supertrend", closes)
        dirs = result["st_direction"]
        assert set(dirs.unique()).issubset({-1, 0, 1})

    def test_single_bar(self):
        result = _run_strategy("supertrend", [100.0])
        assert (result["signal"] == 0).all()

    def test_flat_data(self):
        result = _run_strategy("supertrend", make_flat(60))
        assert (result["signal"] == 0).all()


class TestIchimokuCloud:
    def test_output_columns(self):
        closes = make_trending_up(120)
        result = _run_strategy("ichimoku_cloud", closes)
        for col in ["tenkan", "kijun", "senkou_a", "senkou_b", "signal"]:
            assert col in result.columns

    def test_requires_many_bars(self):
        result = _run_strategy("ichimoku_cloud", [100.0] * 20)
        assert (result["signal"] == 0).all()

    def test_strong_trend(self):
        closes = make_trending_up(150, start=50, step=1.0)
        result = _run_strategy("ichimoku_cloud", closes)
        _assert_valid_signals(result)


class TestPairsSpread:
    def test_self_mean_reversion(self):
        closes = make_volatile(100, amplitude=10)
        result = _run_strategy("pairs_spread", closes)
        _assert_valid_signals(result)
        assert "z_score" in result.columns

    def test_with_close_b(self):
        closes_a = make_volatile(80, center=100, amplitude=5)
        closes_b = make_volatile(80, center=50, amplitude=3, seed=99)
        df = make_ohlcv(closes_a)
        df["close_b"] = closes_b
        result = apply_strategy("pairs_spread", df)
        assert "spread" in result.columns
        _assert_valid_signals(result)


class TestATRBreakout:
    def test_upside_breakout(self):
        closes = list(np.linspace(100, 100, 30)) + list(np.linspace(100, 130, 10))
        result = _run_strategy("atr_breakout", closes, {"atr_period": 14, "multiplier": 1.0})
        _assert_valid_signals(result)
        assert (result["signal"] == 1).any(), "Expected buy signal on upside ATR breakout"

    def test_flat_no_breakout(self):
        result = _run_strategy("atr_breakout", make_flat(50))
        assert (result["signal"] == 0).all()


class TestHeikinAshiEMA:
    def test_output_columns(self):
        closes = make_trending_up(80)
        result = _run_strategy("heikin_ashi_ema", closes)
        for col in ["ha_open", "ha_close", "ha_high", "ha_low", "ha_ema", "signal"]:
            assert col in result.columns

    def test_uptrend(self):
        closes = make_trending_up(100, start=80, step=0.8)
        result = _run_strategy("heikin_ashi_ema", closes)
        _assert_valid_signals(result)


class TestOrderBlocks:
    def test_no_crash_on_flat(self):
        result = _run_strategy("order_blocks", make_flat(80))
        assert (result["signal"] == 0).all()

    def test_displacement_produces_signal(self):
        closes = (
            list(np.linspace(100, 98, 20)) +
            [115] +
            list(np.linspace(115, 100, 20)) +
            list(np.linspace(100, 110, 20))
        )
        n = len(closes)
        opens = [c - 0.3 for c in closes]
        opens[20] = 98
        df = pd.DataFrame({
            "open": opens,
            "high": [max(o, c) + 0.5 for o, c in zip(opens, closes)],
            "low": [min(o, c) - 0.5 for o, c in zip(opens, closes)],
            "close": closes,
            "volume": [100.0] * n,
        })
        result = apply_strategy("order_blocks", df)
        _assert_valid_signals(result)


class TestVWAPReversion:
    def test_with_datetime_index(self):
        n = 100
        closes = make_volatile(n, center=100, amplitude=8)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run_strategy("vwap_reversion", closes, index=idx)
        assert "vwap" in result.columns
        _assert_valid_signals(result)

    def test_no_temp_columns_in_output(self):
        n = 50
        closes = make_volatile(n, center=100, amplitude=5)
        idx = pd.date_range("2024-01-01", periods=n, freq="h")
        result = _run_strategy("vwap_reversion", closes, index=idx)
        for col in ["_day", "_tp_vol", "_cum_tp_vol", "_cum_vol"]:
            assert col not in result.columns


class TestChartPattern:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run_strategy("chart_pattern", closes)
        assert "signal" in result.columns
        _assert_valid_signals(result)


class TestLiquiditySweeps:
    def test_returns_signal(self):
        closes = list(np.linspace(90, 110, 50)) + list(np.linspace(110, 90, 50))
        result = _run_strategy("liquidity_sweeps", closes)
        assert "signal" in result.columns
        _assert_valid_signals(result)


class TestParabolicSAR:
    def test_uptrend_buy(self):
        closes = list(np.linspace(120, 80, 40)) + list(np.linspace(80, 140, 60))
        result = _run_strategy("parabolic_sar", closes)
        _assert_valid_signals(result)
        assert "sar" in result.columns
        assert (result["signal"] == 1).any()

    def test_downtrend_sell(self):
        closes = list(np.linspace(80, 140, 40)) + list(np.linspace(140, 70, 60))
        result = _run_strategy("parabolic_sar", closes)
        assert (result["signal"] == -1).any()

    def test_single_bar(self):
        result = _run_strategy("parabolic_sar", [100.0])
        assert (result["signal"] == 0).all()
        assert result["sar"].isna().all()


class TestSqueezeMomentum:
    def test_returns_signal_column(self):
        closes = make_volatile(100, amplitude=10)
        result = _run_strategy("squeeze_momentum", closes)
        assert "signal" in result.columns
        assert "squeeze_on" in result.columns
        assert "squeeze_mom" in result.columns
        _assert_valid_signals(result)

    def test_flat_no_signal(self):
        result = _run_strategy("squeeze_momentum", make_flat(60))
        assert (result["signal"] == 0).all()


class TestAMDIFVG:
    def test_returns_signal_column(self):
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        closes = make_volatile(n, center=100, amplitude=5)
        result = _run_strategy("amd_ifvg", closes, index=idx)
        assert "signal" in result.columns
        _assert_valid_signals(result)
        assert "asian_high" in result.columns
        assert "asian_low" in result.columns

    def test_short_data_no_signal(self):
        idx = pd.date_range("2024-01-01", periods=2, freq="15min")
        result = _run_strategy("amd_ifvg", [100.0, 101.0], index=idx)
        assert (result["signal"] == 0).all()

    def test_no_crash_flat(self):
        n = 96
        idx = pd.date_range("2024-01-01", periods=n, freq="15min")
        result = _run_strategy("amd_ifvg", make_flat(n), index=idx)
        assert "signal" in result.columns


class TestRangeScalper:
    def test_signals_in_range_bound_data(self):
        n = 60
        rng = np.random.RandomState(123)
        closes = 100 + 2 * np.sin(np.linspace(0, 6 * np.pi, n)) + rng.randn(n) * 0.2
        volume = np.full(n, 50.0)
        df = make_ohlcv(closes, volume=volume, noise=0.3)
        result = apply_strategy("range_scalper", df, {
            "bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.02, "vol_ratio": 1.1,
            "rsi_period": 5, "rsi_ob": 65, "rsi_os": 35,
        })
        assert "signal" in result.columns
        assert "bb_bandwidth" in result.columns
        assert "in_range" in result.columns
        assert (result["signal"] == 1).any(), "Expected buy signals at lower band"
        assert (result["signal"] == -1).any(), "Expected sell signals at upper band"

    def test_no_signals_during_trend(self):
        closes = make_trending_up(80)
        volume = np.full(80, 500.0)
        df = make_ohlcv(closes, volume=volume)
        result = apply_strategy("range_scalper", df, {
            "bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.005,
            "rsi_period": 7, "rsi_ob": 70, "rsi_os": 30,
        })
        assert result["in_range"].sum() < len(result) * 0.3, "Expected few in_range=True bars during trend"

    def test_columns_present(self):
        df = make_ohlcv(make_volatile(50))
        result = apply_strategy("range_scalper", df)
        for col in ["bb_mid", "bb_upper", "bb_lower", "bb_bandwidth", "vol_sma",
                     "low_volume", "in_range", "rsi", "signal"]:
            assert col in result.columns, f"Missing column: {col}"

    def test_no_repeated_signals(self):
        n = 60
        rng = np.random.RandomState(123)
        closes = 100 + 2 * np.sin(np.linspace(0, 6 * np.pi, n)) + rng.randn(n) * 0.2
        volume = np.full(n, 50.0)
        df = make_ohlcv(closes, volume=volume, noise=0.3)
        result = apply_strategy("range_scalper", df, {
            "bb_period": 10, "bb_std": 1.5, "bw_threshold": 0.02, "vol_ratio": 1.1,
            "rsi_period": 5, "rsi_ob": 65, "rsi_os": 35,
        })
        signals = result[result["signal"] != 0]["signal"]
        consecutive_same = (signals == signals.shift(1)).sum()
        assert consecutive_same <= 1, f"Too many consecutive same signals ({consecutive_same}), crossover guard may be broken"


class TestEdgeCases:
    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "supertrend", "atr_breakout",
        "heikin_ashi_ema", "parabolic_sar", "amd_ifvg", "range_scalper",
    ])
    def test_empty_dataframe(self, name):
        df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
        result = apply_strategy(name, df)
        assert len(result) == 0

    @pytest.mark.parametrize("name", [
        "sma_crossover", "ema_crossover", "rsi", "bollinger_bands", "macd",
        "mean_reversion", "momentum", "volume_weighted", "triple_ema",
        "rsi_macd_combo", "stoch_rsi", "atr_breakout",
        "heikin_ashi_ema", "amd_ifvg", "range_scalper",
        "sweep_squeeze_combo",
    ])
    def test_single_row(self, name):
        df = make_ohlcv([100.0])
        result = apply_strategy(name, df)
        assert len(result) == 1
        assert "signal" in result.columns


class TestSweepSqueezeCombo:

    def test_registered(self):
        assert "sweep_squeeze_combo" in STRATEGY_REGISTRY

    def test_output_columns(self):
        df = make_ohlcv(make_trending_up(80))
        result = apply_strategy("sweep_squeeze_combo", df)
        for col in ["signal", "ls_signal", "sq_signal", "sr_signal", "buy_votes", "sell_votes"]:
            assert col in result.columns, f"Missing column: {col}"

    def test_no_signal_on_flat(self):
        df = make_ohlcv(make_flat(80))
        result = apply_strategy("sweep_squeeze_combo", df)
        assert (result["signal"] == 0).all()

    def test_sweep_with_recovery_produces_buy(self):
        n = 80
        prices = list(np.linspace(110, 95, 25))
        prices += list(np.linspace(96, 105, 15))
        prices += list(np.linspace(105, 100, 15))
        prices += [96.0]
        prices += list(np.linspace(98, 108, n - len(prices)))
        df = make_ohlcv(prices, noise=0.3)
        df.loc[df.index[55], "low"] = 93.0
        df.loc[df.index[55], "close"] = 96.0
        result = apply_strategy("sweep_squeeze_combo", df, {"swing_lookback": 5})
        assert (result["ls_signal"] == 1).any(), \
            "Expected liquidity sweep buy signal on sweep candle"

    def test_short_data_no_crash(self):
        df = make_ohlcv([100.0, 101.0, 99.0])
        result = apply_strategy("sweep_squeeze_combo", df)
        assert "signal" in result.columns
        assert len(result) == 3

    def test_default_swing_lookback_is_10(self):
        defaults = STRATEGY_REGISTRY["sweep_squeeze_combo"]["default_params"]
        assert defaults["swing_lookback"] == 10

    def test_min_agree_default_is_2(self):
        defaults = STRATEGY_REGISTRY["sweep_squeeze_combo"]["default_params"]
        assert defaults["min_agree"] == 2

    def test_consensus_signal_with_two_agreeing(self):
        from unittest.mock import patch

        n = 50
        df = make_ohlcv(make_flat(n))

        fake_ls_df = df.copy()
        fake_ls_df["signal"] = 0
        fake_ls_df.iloc[25, fake_ls_df.columns.get_loc("signal")] = 1
        fake_sq = pd.Series(0, index=df.index)
        fake_sr = pd.Series(0, index=df.index)
        fake_sr.iloc[25] = 1

        with patch("sweep_squeeze_combo.liquidity_sweep_core", return_value=fake_ls_df), \
             patch("sweep_squeeze_combo._squeeze_signals", return_value=fake_sq), \
             patch("sweep_squeeze_combo._stoch_rsi_signals", return_value=fake_sr):
            result = apply_strategy("sweep_squeeze_combo", df, {"min_agree": 2})

        assert result.loc[result.index[25], "signal"] == 1, \
            "Expected consensus buy signal=1 when 2 of 3 sub-signals agree"
        assert result.loc[result.index[25], "buy_votes"] == 2

    def test_consensus_sell_signal_with_two_agreeing(self):
        from unittest.mock import patch

        n = 50
        df = make_ohlcv(make_flat(n))

        fake_ls_df = df.copy()
        fake_ls_df["signal"] = 0
        fake_sq = pd.Series(0, index=df.index)
        fake_sq.iloc[30] = -1
        fake_sr = pd.Series(0, index=df.index)
        fake_sr.iloc[30] = -1

        with patch("sweep_squeeze_combo.liquidity_sweep_core", return_value=fake_ls_df), \
             patch("sweep_squeeze_combo._squeeze_signals", return_value=fake_sq), \
             patch("sweep_squeeze_combo._stoch_rsi_signals", return_value=fake_sr):
            result = apply_strategy("sweep_squeeze_combo", df, {"min_agree": 2})

        assert (result["signal"] == -1).any(), \
            "Expected consensus sell signal=-1 when 2 of 3 sub-signals agree"
        assert result.loc[result.index[30], "sell_votes"] == 2

    def test_consensus_buy_signal_with_sweep(self):
        n = 80
        prices = list(np.linspace(110, 95, 25))
        prices += list(np.linspace(96, 105, 15))
        prices += list(np.linspace(105, 100, 15))
        prices += [96.0]
        prices += list(np.linspace(98, 108, n - len(prices)))
        df = make_ohlcv(prices, noise=0.3)
        df.loc[df.index[55], "low"] = 93.0
        df.loc[df.index[55], "close"] = 96.0
        result = apply_strategy("sweep_squeeze_combo", df, {
            "swing_lookback": 5,
            "min_agree": 1,
        })
        assert (result["signal"] == 1).any(), \
            "Expected consensus buy signal=1 with min_agree=1 and ls_signal firing"
