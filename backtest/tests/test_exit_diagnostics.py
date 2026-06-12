"""Tests for the #997 M3 exit-quality diagnostics + backtester hold telemetry."""

import math

import pandas as pd
import pytest

from backtester import Backtester
import exit_diagnostics as ed


# --------------------------------------------------------------------------
# Pure aggregation helpers (no data access).
# --------------------------------------------------------------------------

def _t(**kw):
    base = dict(pnl_pct=0.0, shares=10.0, entry_price=100.0, exit_price=100.0,
               entry_fee=1.0, exit_fee=1.0, mfe_pct=0.0, mae_pct=0.0,
               entry_atr=2.0, bars_held=10, bars_to_mfe=5, bars_to_mae=2,
               exit_reason="signal")
    base.update(kw)
    return base


def test_trade_metrics_fee_and_atr_conversion():
    m = ed.trade_metrics(_t(pnl_pct=5.0, exit_price=105.0, mae_pct=-2.0))
    # entry_fee 1 / (10*100) = 0.1%; exit_fee 1 / (10*105) ~= 0.0952%.
    assert m["fee_pct"] == pytest.approx(0.1 + 1 / (10 * 105) * 100, abs=1e-6)
    assert m["net_pct"] == pytest.approx(5.0 - m["fee_pct"], abs=1e-6)
    # MAE 2% of entry 100 = 2.0 price units / entry_atr 2.0 = 1.0 ATR.
    assert m["mae_atr"] == pytest.approx(1.0, abs=1e-6)


def test_classify_bleed_modes_each_mode():
    trades = [
        _t(pnl_pct=5.0, exit_price=105.0, mfe_pct=6.0, mae_pct=-0.5),   # clean_win
        _t(pnl_pct=1.0, exit_price=101.0, mfe_pct=8.0, mae_pct=-1.0),   # late_giveback (cap<0.5)
        _t(pnl_pct=-4.0, exit_price=96.0, mfe_pct=0.2, mae_pct=-5.0),   # early_reversal
        _t(pnl_pct=0.1, exit_price=100.1, mfe_pct=0.3, mae_pct=-0.2),   # fee_churn (fees>gross)
        _t(pnl_pct=-3.0, exit_price=97.0, mfe_pct=0.7, mae_pct=-3.0),   # clean_loss (modest favourable in [0.5,1.0), then faded)
    ]
    modes = ed.classify_bleed_modes([ed.trade_metrics(t) for t in trades])
    assert modes["clean_win"]["count"] == 1
    assert modes["late_giveback"]["count"] == 1
    assert modes["early_reversal"]["count"] == 1
    assert modes["fee_churn"]["count"] == 1
    assert modes["clean_loss"]["count"] == 1


def test_fee_churn_summary_flags():
    trades = [
        ed.trade_metrics(_t(pnl_pct=0.1, exit_price=100.1, entry_fee=1.0, exit_fee=1.0)),  # flipped
        ed.trade_metrics(_t(pnl_pct=5.0, exit_price=105.0, entry_fee=1.0, exit_fee=1.0)),  # clean
    ]
    s = ed.fee_churn_summary(trades)
    assert s["trades"] == 2
    assert s["trades_flipped_to_loss_by_fees"] == 1


def test_holding_time_buckets_partition():
    trades = [ed.trade_metrics(_t(bars_held=b, pnl_pct=1.0, exit_price=101.0))
              for b in (1, 3, 10, 30, 100)]
    ht = ed.holding_time_summary(trades)
    labels = [b["bucket"] for b in ht["buckets"]]
    assert labels == ["1", "2-5", "6-20", "21-50", "51+"]
    assert ht["bars_held"]["max"] == 100


def test_percentile_interpolation():
    assert ed._pct([1, 2, 3, 4], 0) == 1.0
    assert ed._pct([1, 2, 3, 4], 100) == 4.0
    assert ed._pct([1, 2, 3, 4], 50) == pytest.approx(2.5)


def test_empty_inputs_are_safe():
    diag = ed.diagnose_trades([])
    assert diag["holding_time"]["trades"] == 0
    assert diag["bleed_modes"] == {}
    assert diag["fee_churn"] == {"trades": 0}


# --------------------------------------------------------------------------
# End-to-end: the backtester stamps exact hold telemetry.
# --------------------------------------------------------------------------

def test_backtester_stamps_exact_excursions():
    # Open long at bar 1's open (=100). Held bars 1,2,3; time_stop(3) fires at
    # bar 3 close, fills at bar 4 open (=103). Excursions span bars 1-3 only.
    idx = pd.date_range("2024-01-01", periods=6, freq="h")
    df = pd.DataFrame({
        "open":  [100, 100, 101, 102, 103, 103],
        "high":  [100, 101, 103, 104, 105, 105],
        "low":   [100,  99, 100, 101, 102, 102],
        "close": [100, 100, 101, 102, 103, 103],
        "open_action": ["long", "none", "none", "none", "none", "none"],
        "atr":   [2.0] * 6,
    }, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.001, slippage_pct=0.0,
                    open_strategy={"name": "x"},
                    close_strategies=[{"name": "time_stop", "params": {"max_bars": 3}}],
                    direction="long")
    r = bt.run(df, strategy_name="x", save=False)
    assert r["total_trades"] == 1
    t = r["trades"][0]
    assert t["bars_held"] == 3
    assert t["exit_reason"] == "time_stop:3"
    # MFE: max high through bar 3 = 104 -> +4%, at bar 3.
    assert t["mfe_pct"] == pytest.approx(4.0)
    assert t["bars_to_mfe"] == 3
    # MAE: min low through bar 3 = 99 (bar 1) -> -1%, at bar 1.
    assert t["mae_pct"] == pytest.approx(-1.0)
    assert t["bars_to_mae"] == 1
    assert t["entry_atr"] == pytest.approx(2.0)
    assert t["entry_fee"] == pytest.approx(1.0)  # 1000 * 0.001


def test_atr_stop_engine_fires_at_next_open():
    # entry at bar 1 open=100, entry_atr=2, atr_stop mult=2 -> stop at 96.
    # bar 2 close=95 breaches -> fill at bar 3 open=95.
    idx = pd.date_range("2024-01-01", periods=5, freq="h")
    df = pd.DataFrame({
        "open":  [100, 100, 97, 95, 95],
        "high":  [100, 101, 98, 96, 96],
        "low":   [100,  99, 95, 94, 94],
        "close": [100, 100, 95, 95, 95],
        "open_action": ["long", "none", "none", "none", "none"],
        "atr":   [2.0] * 5,
    }, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    open_strategy={"name": "x"},
                    close_strategies=[{"name": "atr_stop", "params": {"atr_mult": 2.0}}],
                    direction="long")
    r = bt.run(df, strategy_name="x", save=False)
    assert r["total_trades"] == 1
    assert r["trades"][0]["exit_reason"] == "atr_stop:2"


def test_zscore_target_engine_uses_closed_bar_z():
    # A long that stretches up; zscore_target(lookback=3, z_target=1.0) closes
    # on the first bar whose closed-bar z >= 1.0. Just assert it exits via the
    # zscore evaluator (deterministic series, fill at next open).
    idx = pd.date_range("2024-01-01", periods=10, freq="h")
    closes = [100, 100, 100, 100, 101, 103, 108, 108, 108, 108]
    df = pd.DataFrame({
        "open": closes, "high": closes, "low": closes, "close": closes,
        "open_action": ["long"] + ["none"] * 9,
    }, index=idx)
    bt = Backtester(initial_capital=1000.0, commission_pct=0.0, slippage_pct=0.0,
                    open_strategy={"name": "x"},
                    close_strategies=[{"name": "zscore_target",
                                       "params": {"lookback": 3, "z_target": 1.0}}],
                    direction="long")
    r = bt.run(df, strategy_name="x", save=False)
    assert r["total_trades"] == 1
    assert r["trades"][0]["exit_reason"].startswith("zscore_target:")


def test_duplicate_zscore_target_rejected():
    with pytest.raises(ValueError, match="duplicate zscore_target"):
        Backtester(
            initial_capital=1000.0,
            open_strategy={"name": "x"},
            close_strategies=[
                {"name": "zscore_target", "params": {"lookback": 10, "z_target": 2.0}},
                {"name": "zscore_target", "params": {"lookback": 20, "z_target": 2.0}},
            ],
        )
