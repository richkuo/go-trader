
import numpy as np
import pandas as pd
import pytest

from shared_strategies.open.conftest import load_module, make_ohlcv as make_frame

_MOMENTUM_PRO = load_module("_momentum_pro_test", __file__.replace("test_momentum_pro.py", "momentum_pro.py"))
momentum_pro_core = _MOMENTUM_PRO.momentum_pro_core


def build_uptrend_with_pullback():
    n = 260
    closes = list(np.linspace(100, 200, n - 6))
    base = closes[-1]
    closes += [base - 4, base - 7, base - 9]
    closes += [base - 4, base + 6, base + 12]
    closes = np.array(closes, dtype=float)
    n = len(closes)
    highs = closes + 1.0
    lows = closes - 1.0
    opens = closes - 0.3
    vol = np.full(n, 100.0)
    vol[-1] = 100.0
    vol[-2] = 500.0
    return make_frame(closes, volume=vol, opens=opens, highs=highs, lows=lows)


def test_columns_present():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df)
    for col in ("signal", "ema_fast", "ema_mid", "ema_long", "adx", "vol_sma"):
        assert col in out.columns


def test_warmup_returns_no_signal():
    df = make_frame(
        [100] * 30, opens=[100] * 30, highs=[101] * 30,
        lows=[99] * 30, volume=[100] * 30,
    )
    out = momentum_pro_core(df)
    assert (out["signal"] == 0).all()


def test_empty_df_is_safe():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    out = momentum_pro_core(df)
    assert "signal" in out.columns
    assert len(out) == 0


def test_uptrend_pullback_fires_long():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df, vol_mult=1.2)
    assert (out["signal"] == 1).any(), "expected a long entry on the resumption bar"


def test_volume_gate_blocks_when_unmet():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df, vol_mult=1e6)
    assert (out["signal"] == 0).all()


def test_flat_market_no_signal():
    n = 260
    closes = np.full(n, 100.0) + np.random.RandomState(0).randn(n) * 0.05
    df = make_frame(
        closes, opens=closes - 0.3, highs=closes + 0.5,
        lows=closes - 0.5, volume=np.full(n, 100.0),
    )
    out = momentum_pro_core(df)
    assert (out["signal"] == 0).all()


def test_downtrend_pullback_fires_short():
    n = 260
    closes = list(np.linspace(200, 100, n - 6))
    base = closes[-1]
    closes += [base + 4, base + 7, base + 9]
    closes += [base + 4, base - 6, base - 12]
    closes = np.array(closes, dtype=float)
    n = len(closes)
    highs = closes + 1.0
    lows = closes - 1.0
    opens = closes + 0.3
    vol = np.full(n, 100.0)
    vol[-2] = 500.0
    df = make_frame(closes, volume=vol, opens=opens, highs=highs, lows=lows)
    out = momentum_pro_core(df, vol_mult=1.2)
    assert (out["signal"] == -1).any(), "expected a short entry on the breakdown bar"


def test_vol_target_off_by_default_no_entry_fraction_column():
    df = build_uptrend_with_pullback()
    out = momentum_pro_core(df)
    assert "entry_fraction" not in out.columns


def test_vol_target_zero_is_byte_identical_to_default():
    df = build_uptrend_with_pullback()
    base = momentum_pro_core(df)
    off = momentum_pro_core(df, vol_target_atr_pct=0.0)
    pd.testing.assert_frame_equal(base, off)


def test_vol_target_never_changes_signals():
    df = build_uptrend_with_pullback()
    base = momentum_pro_core(df)
    sized = momentum_pro_core(df, vol_target_atr_pct=0.01)
    pd.testing.assert_series_equal(base["signal"], sized["signal"])


def test_vol_target_emits_fraction_scaled_by_atr():
    n = 260
    closes = np.full(n, 100.0)
    df = make_frame(
        closes, volume=np.full(n, 100.0), opens=closes,
        highs=closes + 1.0, lows=closes - 1.0,
    )
    out = momentum_pro_core(df, vol_target_atr_pct=0.01)
    assert "entry_fraction" in out.columns
    warm = out["entry_fraction"].iloc[50:]
    assert warm.notna().all()
    assert warm.iloc[-1] == pytest.approx(0.5)
    assert ((warm > 0) & (warm <= 1)).all()


def test_vol_target_fraction_floors_at_min_fraction():
    n = 260
    closes = np.full(n, 100.0)
    df = make_frame(
        closes, volume=np.full(n, 100.0), opens=closes,
        highs=closes + 1.0, lows=closes - 1.0,
    )
    out = momentum_pro_core(df, vol_target_atr_pct=0.0001,
                            vol_target_min_fraction=0.10)
    warm = out["entry_fraction"].iloc[50:]
    assert np.allclose(warm, 0.10)


def test_vol_target_caps_fraction_at_one_in_quiet_markets():
    n = 260
    closes = np.full(n, 100.0)
    df = make_frame(
        closes, volume=np.full(n, 100.0), opens=closes,
        highs=closes + 1.0, lows=closes - 1.0,
    )
    out = momentum_pro_core(df, vol_target_atr_pct=0.50)
    warm = out["entry_fraction"].iloc[50:]
    assert np.allclose(warm, 1.0)
