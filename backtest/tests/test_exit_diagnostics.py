
import math

import pandas as pd
import pytest

from backtester import Backtester
import exit_diagnostics as ed


def _t(**kw):
    base = dict(pnl_pct=0.0, shares=10.0, entry_price=100.0, exit_price=100.0,
               entry_fee=1.0, exit_fee=1.0, mfe_pct=0.0, mae_pct=0.0,
               entry_atr=2.0, bars_held=10, bars_to_mfe=5, bars_to_mae=2,
               exit_reason="signal")
    base.update(kw)
    return base


def test_trade_metrics_fee_and_atr_conversion():
    m = ed.trade_metrics(_t(pnl_pct=5.0, exit_price=105.0, mae_pct=-2.0))
    assert m["fee_pct"] == pytest.approx(0.1 + 1 / (10 * 105) * 100, abs=1e-6)
    assert m["net_pct"] == pytest.approx(5.0 - m["fee_pct"], abs=1e-6)
    assert m["mae_atr"] == pytest.approx(1.0, abs=1e-6)


def test_fee_churn_summary_flags():
    trades = [
        ed.trade_metrics(_t(pnl_pct=0.1, exit_price=100.1, entry_fee=1.0, exit_fee=1.0)),
        ed.trade_metrics(_t(pnl_pct=5.0, exit_price=105.0, entry_fee=1.0, exit_fee=1.0)),
    ]
    s = ed.fee_churn_summary(trades)
    assert s["trades"] == 2
    assert s["trades_flipped_to_loss_by_fees"] == 1


def test_backtester_stamps_exact_excursions():
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
                    direction="long", comparison_mode="approximate")
    r = bt.run(df, strategy_name="x", save=False)
    assert r["total_trades"] == 1
    t = r["trades"][0]
    assert t["bars_held"] == 3
    assert t["exit_reason"] == "time_stop:3"
    assert t["mfe_pct"] == pytest.approx(4.0)
    assert t["bars_to_mfe"] == 3
    assert t["mae_pct"] == pytest.approx(-1.0)
    assert t["bars_to_mae"] == 1
    assert t["entry_atr"] == pytest.approx(2.0)
    assert t["entry_fee"] == pytest.approx(1.0)


def test_atr_stop_engine_fires_at_next_open():
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


def test_duplicate_zscore_target_rejected():
    with pytest.raises(ValueError, match="duplicate zscore_target"):
        Backtester(
            initial_capital=1000.0,
            open_strategy={"name": "x"},
            close_strategies=[
                {"name": "zscore_target", "params": {"lookback": 10, "z_target": 2.0}},
                {"name": "zscore_target", "params": {"lookback": 20, "z_target": 2.0}},
            ],
            comparison_mode="approximate",
        )
