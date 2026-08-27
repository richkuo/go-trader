
import numpy as np
import pandas as pd

from liquidity_sweeps import _find_swing_lows, liquidity_sweep_core


def _make_ohlcv(highs, lows, closes, opens=None):
    n = len(closes)
    if opens is None:
        opens = closes
    return pd.DataFrame({
        "open":   opens,
        "high":   highs,
        "low":    lows,
        "close":  closes,
        "volume": [100.0] * n,
    })


def _monotone_uptrend(n: int, start: float = 100.0, slope: float = 0.5) -> tuple:
    closes = start + slope * np.arange(n)
    highs = closes + 0.3
    lows = closes - 0.3
    return highs.astype(float), lows.astype(float), closes.astype(float)


class TestLookahead:

    def test_sweep_never_fires_inside_confirmation_window(self):
        lookback = 5
        n = 80

        highs, lows, closes = _monotone_uptrend(n)
        p = 15
        lows[p] = 50.0
        closes[p] = 80.0
        q = p + lookback + 1
        lows[q] = 49.0
        closes[q] = 81.0

        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)

        sl = _find_swing_lows(df["low"], lookback)
        swing_positions = np.where(sl.notna())[0].tolist()
        assert p in swing_positions, "test setup failed to plant swing at p"

        for sp in swing_positions:
            window_end = min(sp + lookback + 1, n)
            inside = out["signal"].iloc[sp + 1: window_end]
            assert (inside == 0).all(), (
                f"signal fired inside confirmation window of swing at {sp}: "
                f"non-zero at {[sp + 1 + i for i in np.where(inside != 0)[0]]}"
            )

    def test_signal_at_k_independent_of_bars_after_k(self):
        lookback = 5
        n = 80
        highs, lows, closes = _monotone_uptrend(n)
        lows[20] = 60.0
        closes[20] = 70.0
        lows[40] = 59.0
        closes[40] = 80.0

        df = _make_ohlcv(highs, lows, closes)
        full = liquidity_sweep_core(df, swing_lookback=lookback)

        signal_bars = list(np.where(full["signal"].values != 0)[0])
        for k in signal_bars:
            partial_df = df.iloc[: k + 1]
            partial = liquidity_sweep_core(partial_df, swing_lookback=lookback)
            assert partial["signal"].iloc[k] == full["signal"].iloc[k], (
                f"signal at bar {k} differs after truncation: "
                f"full={full['signal'].iloc[k]} truncated={partial['signal'].iloc[k]}"
            )


class TestBasic:
    def test_bullish_sweep_after_confirmation(self):
        lookback = 3
        n = 40
        highs, lows, closes = _monotone_uptrend(n)
        lows[10] = 50.0
        closes[10] = 80.0
        lows[25] = 49.0
        closes[25] = 81.0

        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)
        assert (out["signal"].iloc[25:] == 1).any()

    def test_no_signal_when_close_doesnt_recover(self):
        lookback = 3
        n = 40
        highs, lows, closes = _monotone_uptrend(n)
        lows[10] = 50.0; closes[10] = 80.0
        lows[25] = 49.0; closes[25] = 48.0
        df = _make_ohlcv(highs, lows, closes)
        out = liquidity_sweep_core(df, swing_lookback=lookback)
        assert (out["signal"] == 1).sum() == 0

    def test_short_data_returns_zeros(self):
        df = _make_ohlcv([100.5] * 5, [99.5] * 5, [100] * 5)
        out = liquidity_sweep_core(df, swing_lookback=5)
        assert (out["signal"] == 0).all()
