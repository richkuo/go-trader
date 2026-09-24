import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester


def _df(closes, signals, opens=None, atr=None):
    closes = np.asarray(closes, dtype=float)
    n = len(closes)
    opens = closes if opens is None else np.asarray(opens, dtype=float)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": opens,
            "high": np.maximum(opens, closes) + 0.5,
            "low": np.minimum(opens, closes) - 0.5,
            "close": closes,
            "volume": np.full(n, 1000.0),
            "signal": np.asarray(signals, dtype=float),
        },
        index=idx,
    )
    if atr is not None:
        df["atr"] = float(atr)
    return df


def _run(df, **kw):
    kw.setdefault("direction", "short")
    kw.setdefault("commission_pct", 0.0)
    kw.setdefault("slippage_pct", 0.0)
    bt = Backtester(initial_capital=10000.0, **kw)
    return bt.run(df.copy(), strategy_name="x", symbol="BTC/USDT",
                  timeframe="1d", save=False)


@pytest.mark.parametrize(
    "closes,expected_entry,expected_exit,pnl_sign,expected_final",
    [
        ([100, 100, 90, 80, 80, 80], 100.0, 80.0, 1, 12000.0),
        ([100, 100, 110, 120, 120, 120], None, None, -1, 8000.0),
    ],
)
def test_short_round_trip_pnl(closes, expected_entry, expected_exit, pnl_sign,
                              expected_final):
    signals = [-1, 0, 0, 1, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    if expected_entry is not None:
        assert t["entry_price"] == pytest.approx(expected_entry)
    if expected_exit is not None:
        assert t["exit_price"] == pytest.approx(expected_exit)
    assert (t["pnl"] > 0) if pnl_sign > 0 else (t["pnl"] < 0)
    assert res["final_capital"] == pytest.approx(expected_final)


def test_short_entry_fills_next_bar_open_not_signal_bar():
    closes = [100, 100, 100, 90, 90]
    opens = [100, 100, 95, 90, 90]
    signals = [0, -1, 0, 0, 1]
    res = _run(_df(closes, signals, opens=opens))
    assert res["total_trades"] == 1
    assert res["trades"][0]["entry_price"] == pytest.approx(95.0)


def test_commission_and_slippage_charged_on_both_legs():
    closes = [100, 100, 100, 100, 100]
    signals = [-1, 0, 1, 0, 0]
    free = _run(_df(closes, signals))
    costly = _run(_df(closes, signals), commission_pct=0.001,
                  slippage_pct=0.0005)
    assert free["final_capital"] == pytest.approx(10000.0)
    assert costly["final_capital"] < free["final_capital"] - 25.0


def test_long_signal_while_flat_opens_nothing_in_short_mode():
    closes = [100, 100, 100, 100, 100]
    signals = [1, 0, -1, -1, 0]
    res = _run(_df(closes, signals))
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"


def test_invert_signal_composes_with_short_mode():
    closes = [100, 100, 90, 80, 80]
    signals = [1, 0, -1, 0, 0]
    res = _run(_df(closes, signals), invert_signal=True)
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["side"] == "short"
    assert t["pnl"] > 0


def test_regime_gate_blocks_short_entries():
    closes = [100, 100, 90, 80, 80]
    signals = [-1, 0, 0, 0, 0]
    df = _df(closes, signals)
    df["regime"] = "bullish"
    res = _run(df, regime_enabled=True, allowed_regimes=["bearish"])
    assert res["total_trades"] == 0
    assert res["final_capital"] == pytest.approx(10000.0)


def test_end_of_data_flush_closes_open_short():
    closes = [100, 100, 90, 85, 80]
    signals = [-1, 0, 0, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    t = res["trades"][0]
    assert t["exit_reason"] == "end_of_data"
    assert t["pnl"] > 0


def test_fixed_atr_stop_buys_back_on_adverse_rally():
    closes = [100, 100, 103, 110, 120, 130, 130]
    signals = [-1, 0, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    stopped = _run(df, stop_loss_atr_mult=1.0)
    no_stop = _run(df)
    assert stopped["total_trades"] == 1
    assert stopped["trades"][0]["exit_reason"] == "signal_sl"
    assert stopped["final_capital"] > no_stop["final_capital"]


def test_fixed_atr_stop_fills_next_bar_open():
    closes = [100, 100, 103, 200, 200]
    opens = [100, 100, 103, 104, 200]
    signals = [-1, 0, 0, 0, 0]
    df = _df(closes, signals, opens=opens, atr=2.0)
    res = _run(df, stop_loss_atr_mult=1.0, intrabar_resolution="bar_close")
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_price"] == pytest.approx(104.0)


def test_trailing_atr_stop_tightens_down_and_exits_on_bounce():
    closes = [100, 100, 90, 80, 70, 80, 95, 100, 100]
    signals = [-1, 0, 0, 0, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    trailed = _run(df, trailing_stop_atr_mult=1.0)
    no_stop = _run(df)
    assert trailed["total_trades"] == 1
    assert trailed["trades"][0]["exit_reason"] == "signal_sl"
    assert trailed["final_capital"] > no_stop["final_capital"]


def test_trailing_stop_never_loosens_on_adverse_move():
    closes = [100, 100, 85, 70, 75, 75]
    signals = [-1, 0, 0, 0, 0, 0]
    df = _df(closes, signals, atr=2.0)
    res = _run(df, trailing_stop_atr_mult=1.0)
    assert res["total_trades"] == 1
    assert res["trades"][0]["exit_reason"] == "signal_sl"
    assert res["trades"][0]["pnl"] > 0


def test_fixed_pct_stop_mirrors_above_entry():
    closes = [100, 100, 106, 115, 130, 130]
    signals = [-1, 0, 0, 0, 0, 0]
    df = _df(closes, signals)
    stopped = _run(df, stop_loss_pct=0.05)
    no_stop = _run(df)
    assert stopped["total_trades"] == 1
    assert stopped["final_capital"] > no_stop["final_capital"]


def test_starting_long_rejected_on_short_path():
    closes = [100, 100, 100]
    signals = [0, 0, 0]
    bt = Backtester(initial_capital=10000.0, direction="short",
                    commission_pct=0.0, slippage_pct=0.0)
    with pytest.raises(ValueError, match="starting_long"):
        bt.run(_df(closes, signals), save=False,
               starting_long={"entry_price": 100.0})


def test_direction_both_rejected_on_plain_path():
    closes = [100, 100, 110, 120, 120]
    signals = [1, 0, 0, -1, 0]
    with pytest.raises(ValueError, match="direction='both'"):
        _run(_df(closes, signals), direction="both")


def test_direction_long_default_is_unchanged():
    closes = [100, 100, 110, 120, 120]
    signals = [1, 0, 0, -1, 0]
    base = _run(_df(closes, signals), direction=None)
    explicit = _run(_df(closes, signals), direction="long")
    assert base["final_capital"] == explicit["final_capital"]
    assert [t["side"] for t in base["trades"]] == ["long"]
    assert [t["side"] for t in explicit["trades"]] == ["long"]


def test_no_short_reentry_after_blowup_plain_path():
    closes = [100, 100, 250, 250, 100, 50, 50]
    signals = [-1, 0, 1, -1, 0, 0, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert res["trades"][0]["pnl"] < 0
    assert res["final_capital"] == pytest.approx(-5000.0)


def test_consecutive_reentry_attempts_after_blowup_do_not_compound():
    closes = [100, 100, 250, 250, 100, 80, 50]
    signals = [-1, 0, 1, -1, -1, -1, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert res["final_capital"] == pytest.approx(-5000.0)


def test_cash_zero_boundary_books_no_phantom_trade():
    closes = [100, 100, 200, 200, 200]
    signals = [-1, 0, 1, -1, 0]
    res = _run(_df(closes, signals))
    assert res["total_trades"] == 1
    assert len(res["trades"]) == 1
    assert res["final_capital"] == pytest.approx(0.0)


@pytest.mark.parametrize(
    "closes,signals,extra_kw",
    [
        ([100, 100, 200, 300, 300, 100, 100], [-1, 0, 0, 0, -1, 0, 0], {}),
        ([100, 100, 200, 300, 300, 300, 300], [-1, 0, 0, 0, 1, 0, 0],
         {"direction": "both"}),
    ],
)
def test_no_reopen_after_short_blowup_engine_path(closes, signals, extra_kw):
    df = _df(closes, signals, atr=2.0)
    res = _run(df, close_strategies=[
        {"name": "time_stop", "params": {"max_bars": 2}},
    ], **extra_kw)
    assert res["total_trades"] == 1
    assert res["trades"][0]["side"] == "short"
    assert res["final_capital"] == pytest.approx(-10000.0)


def test_short_mode_with_close_refs_uses_engine_path_not_plain():
    closes = [100, 100, 90, 80, 80]
    signals = [-1, 0, 1, 0, 0]
    df = _df(closes, signals, atr=2.0)
    never_fires = [{"name": "tiered_tp_pct", "params": {"tp_tiers": [
        {"profit_pct": 0.9, "close_fraction": 1.0},
    ]}}]
    res = _run(df, close_strategies=never_fires)
    assert [t["side"] for t in res["trades"]] == ["short"]
    assert res["trades"][0]["exit_reason"] == "end_of_data"
