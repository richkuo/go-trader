"""Bar-open partial closes versus an armed isolated-liquidation stop.

A close_fraction that does not move the stop still lets the walk fill the
armed stop. A close that moves the stop skips that fill, and the range
liquidation must still book.
"""

import pandas as pd
import pytest

from backtester import Backtester, isolated_liquidation_price

CRASH = "2024-01-05 00:00:00"
_LIQ_TIERS = [{"lower_bound": 0, "max_leverage": 20}]


def _frame(side, close_fraction_on_prior):
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    if side == "long":
        opens = [100, 100, 100, 100, 97, 96]
        highs = [100, 100, 101, 101, 97, 96]
        lows = [100, 100, 99, 98, 90, 96]
        closes = [100, 100, 100, 100, 96, 96]
        actions = ["none", "long", "none", "none", "none", "none"]
    else:
        opens = [100, 100, 100, 100, 103, 104]
        highs = [100, 100, 101, 102, 110, 104]
        lows = [100, 100, 99, 99, 103, 104]
        closes = [100, 100, 100, 100, 104, 104]
        actions = ["none", "short", "none", "none", "none", "none"]
    return pd.DataFrame({
        "open": opens,
        "high": highs,
        "low": lows,
        "close": closes,
        "open_action": actions,
        "close_fraction": [0, 0, 0, close_fraction_on_prior, 0, 0],
    }, index=idx)


def _run(side, close_fraction, sl_after=False):
    kwargs = dict(
        initial_capital=10000,
        commission_pct=0,
        slippage_pct=0,
        platform="hyperliquid",
        strategy_type="perps",
        stop_loss_pct=0.07,
        leverage=10,
        intrabar_resolution="ohlc_walk",
        liquidation_model="venue_isolated",
        perps_sizing={
            "exchange_leverage": 10,
            "sizing_leverage": 1.0,
            "margin_mode": "isolated",
            "budget_source": "strategy_cash",
        },
        venue_margin={"max_leverage": 20, "tiers": _LIQ_TIERS},
    )
    if sl_after:
        kwargs["close_strategies"] = [{
            "name": "tiered_tp_atr",
            "params": {"tp_tiers": [
                {"atr_multiple": 100.0, "close_fraction": 0.5, "sl_after": "breakeven"},
                {"atr_multiple": 200.0, "close_fraction": 1.0},
            ]},
        }]
    return Backtester(**kwargs).run(_frame(side, close_fraction), save=False)


def _on_crash(result, reason):
    return [
        t for t in result["trades"]
        if t["exit_date"] == CRASH and t["exit_reason"] == reason
    ]


def test_partial_close_without_stop_move_fills_the_armed_stop():
    """10x long at 100, stop 93, close_fraction 0.5, bar open 97 low 90 close 96."""
    result = _run("long", 0.5)
    partial = _on_crash(result, "column_close_fraction")
    stopped = _on_crash(result, "sl")
    assert len(partial) == 1 and partial[0]["shares"] == 50.0
    assert len(stopped) == 1
    assert stopped[0]["shares"] == 50.0
    assert stopped[0]["exit_price"] == 93.0
    assert stopped[0]["entry_price"] == 100.0
    assert result["margin"]["liquidation_count"] == 0


def test_same_bar_without_partial_close_fills_stop_and_skips_liquidation():
    result = _run("long", 0.0)
    stopped = _on_crash(result, "sl")
    assert len(stopped) == 1
    assert stopped[0]["exit_price"] == 93.0
    assert stopped[0]["shares"] == 100.0
    assert _on_crash(result, "venue_liquidation") == []
    assert result["margin"]["liquidation_count"] == 0


def test_short_partial_close_without_stop_move_fills_the_armed_stop():
    """Short mirror: high 110 is above the ~107.32 liquidation, stop armed at 107."""
    result = _run("short", 0.5)
    partial = _on_crash(result, "column_close_fraction")
    stopped = _on_crash(result, "sl")
    assert len(partial) == 1 and partial[0]["shares"] == 50.0
    assert len(stopped) == 1
    assert stopped[0]["shares"] == 50.0
    assert stopped[0]["exit_price"] == 107.0
    assert result["margin"]["liquidation_count"] == 0


@pytest.mark.parametrize("side", ["long", "short"])
def test_stop_move_on_partial_close_still_books_range_liquidation(side):
    result = _run(side, 0.5, sl_after=True)
    liq_px = isolated_liquidation_price(side, 100.0, 1.0, 10.0, 20.0)
    liquidated = _on_crash(result, "venue_liquidation")
    partial = _on_crash(result, "column_close_fraction")
    assert len(partial) == 1 and partial[0]["shares"] == 50.0
    assert len(liquidated) == 1
    assert liquidated[0]["shares"] == 50.0
    assert liquidated[0]["exit_price"] == liq_px
    assert _on_crash(result, "sl") == []
    assert result["margin"]["liquidation_count"] == 1
    later = [t for t in result["trades"] if t["exit_date"] > CRASH]
    assert later == []
