"""
Tests for regime detection integration in Backtester (issue #482/#543).

Verifies:
- Vectorized regime column computed before Backtester.run() via ensure_regime_columns.
- Entry gating: signals blocked when bar's regime not in allowed_regimes.
- Live↔backtest parity: latest_regime(df) and compute_regime(df).iloc[-1]
  produce identical labels for the same OHLCV window.
"""
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester
from regime import compute_regime, latest_regime, ensure_regime_columns


# ─── Fixtures ────────────────────────────────────────────────────────────────


def _uptrend_df(n: int = 100) -> pd.DataFrame:
    """Monotonic uptrend — ADX will be high, +DI >> -DI → trending_up."""
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.5, "low": close - 0.5,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def _ranging_df(n: int = 100) -> pd.DataFrame:
    """Flat prices — ADX stays near 0 → ranging."""
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    return pd.DataFrame(
        {"open": close, "high": close + 0.05, "low": close - 0.05,
         "close": close, "volume": 1000.0},
        index=idx,
    )


def _signal_df(df: pd.DataFrame, *, buy_at: int = 30) -> pd.DataFrame:
    """Attach a signal column: buy at bar buy_at, hold rest."""
    out = df.copy()
    out["signal"] = 0
    out.iloc[buy_at, out.columns.get_loc("signal")] = 1
    return out


# ─── ensure_regime_columns integration ───────────────────────────────────────


def test_ensure_regime_columns_adds_regime_column():
    df = _uptrend_df()
    out = ensure_regime_columns(df)
    assert "regime" in out.columns
    assert "regime_score" in out.columns


def test_ensure_regime_columns_uptrend_labels_trending_up():
    df = _uptrend_df(n=100)
    out = ensure_regime_columns(df, period=14, adx_threshold=20.0)
    assert out["regime"].iloc[-1] == "trending_up"


def test_ensure_regime_columns_ranging_labels_ranging():
    df = _ranging_df(n=100)
    out = ensure_regime_columns(df, period=14, adx_threshold=20.0)
    assert out["regime"].iloc[-1] == "ranging"


# ─── Backtester regime constructor params ─────────────────────────────────────


def test_backtester_accepts_regime_params():
    bt = Backtester(
        initial_capital=1000,
        commission_pct=0,
        slippage_pct=0,
        regime_enabled=True,
        regime_period=14,
        regime_adx_threshold=20.0,
        allowed_regimes=["trending_up"],
    )
    assert bt.regime_enabled is True
    assert bt.regime_period == 14
    assert bt.regime_adx_threshold == 20.0
    assert bt.allowed_regimes == ["trending_up"]


def test_backtester_regime_defaults():
    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    assert bt.regime_enabled is False
    assert bt.allowed_regimes == []


# ─── Regime gating: uptrend df + trending_up allowed → entries pass ───────────


def test_regime_gate_allows_entry_when_regime_matches():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up"],
    )
    result = bt.run(df_sig, save=False)
    # Bar 50 should be trending_up (well past warmup on an uptrend) — entry allowed
    assert result["total_trades"] >= 1, "Expected at least one trade when regime matches"


# ─── Regime gating: uptrend df + ranging-only gate → entries blocked ──────────


def test_regime_gate_blocks_entry_when_regime_mismatches():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["ranging"],
    )
    result = bt.run(df_sig, save=False)
    # Bar 50 is trending_up, not ranging → entry blocked
    assert result["total_trades"] == 0, "Expected no trades when regime gate blocks entry"


# ─── Regime gate doesn't affect close paths ───────────────────────────────────


def test_regime_gate_does_not_close_open_position():
    """Once a position is open, a regime mismatch must not force-close it.

    The backtester shifts signals by 1 bar (signal on bar N executes at bar N+1).
    So the entry fires at bar 51. We set regime to trending_up through bar 51
    (entry allowed) then ranging at bar 52+ (position already open — not force-closed).
    """
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = df.copy()
    df_sig["signal"] = 0
    # Signal at bar 50 → executes at bar 51's open (next-bar execution model)
    df_sig.iloc[50, df_sig.columns.get_loc("signal")] = 1
    # Keep trending_up through bar 51 so the shifted entry passes the gate;
    # regime becomes ranging at bar 52+, but the position should be held.
    df_sig["regime"] = "trending_up"
    df_sig.iloc[52:, df_sig.columns.get_loc("regime")] = "ranging"

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up"],
    )
    result = bt.run(df_sig, save=False)
    # Position opened at bar 51, held to end (no sell signal) → 1 trade
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] > 0


# ─── Empty allowed_regimes = allow all ────────────────────────────────────────


def test_regime_enabled_empty_allowed_allows_all():
    df = _uptrend_df(n=100)
    ensure_regime_columns(df, period=14, adx_threshold=20.0)
    df_sig = _signal_df(df, buy_at=50)

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=[],  # empty = allow all
    )
    result = bt.run(df_sig, save=False)
    assert result["total_trades"] >= 1


# ─── Live↔backtest parity ─────────────────────────────────────────────────────


def test_parity_latest_regime_matches_compute_regime_last_bar():
    """
    Parity test: latest_regime(df) and compute_regime(df).iloc[-1] must
    produce identical labels for the same OHLCV window.
    This is guaranteed by design (both call the same _adx_components logic),
    but this test makes the contract explicit and regression-guards it.
    """
    df = _uptrend_df(n=100)
    live_result = latest_regime(df, period=14, adx_threshold=20.0)
    backtest_series = compute_regime(df, period=14, adx_threshold=20.0)
    backtest_last = backtest_series.iloc[-1]

    assert live_result["regime"] == backtest_last["regime"], (
        f"Parity violation: live={live_result['regime']}, "
        f"backtest last bar={backtest_last['regime']}"
    )
    assert abs(live_result["score"] - float(backtest_last["regime_score"])) < 1e-9, (
        "Score mismatch between live and backtest"
    )


def test_parity_ranging_df():
    df = _ranging_df(n=100)
    live = latest_regime(df, period=14, adx_threshold=20.0)
    bt_last = compute_regime(df, period=14, adx_threshold=20.0).iloc[-1]
    assert live["regime"] == bt_last["regime"]


# ─── Regime disabled = no behavior change ─────────────────────────────────────


def test_regime_disabled_does_not_block_entries():
    df = _ranging_df(n=100)  # ranging would block if regime gating were on
    df_sig = _signal_df(df, buy_at=50)

    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=False, allowed_regimes=["trending_up"],
    )
    result = bt.run(df_sig, save=False)
    assert result["total_trades"] >= 1, "Disabled regime gate must not block entries"
