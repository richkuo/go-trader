"""Backtester parity for trailing_tp_ratchet (#844).

The close evaluator drives the per-tier partial closes; the backtester's ratchet
machinery tightens the trailing ATR mult as entry-ATR profit clears tiers and
exits the remainder via the trailing-stop walker.
"""
import pandas as pd

from backtester import Backtester


def _df(opens, closes, atrs):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    open_actions = ["long"] + ["none"] * (n - 1)
    return pd.DataFrame(
        {"open": opens, "close": closes, "open_action": open_actions, "atr": atrs},
        index=idx,
    )


def test_pure_trailing_ratchet_exits_on_tightened_trail():
    # ATR=10, entry $100, initial loose trail 3.0x (=30 wide). Two trail-only
    # rungs: +1xATR tightens to 2.0x, +2xATR tightens to 1.0x. Price runs to
    # $125 (clears both rungs -> trail 1.0x -> trigger $115), dips to $114
    # (tight trail fires), then recovers to $130. Exit at $114 PROVES the trail
    # tightened: a still-loose 3.0x trail (trigger $95) would not fire at $114
    # and would ride up to the $130 close.
    df = _df(
        opens=[100, 100, 115, 125, 116, 114, 130],
        closes=[100, 115, 125, 116, 114, 130, 130],
        atrs=[10] * 7,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=3.0,
        close_strategies=[{
            "name": "trailing_tp_ratchet",
            "params": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0},
                {"atr_multiple": 2.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ]},
        }],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1, result
    assert result["trades"][0]["exit_price"] == 114.0, result["trades"]
    assert result["final_capital"] == 1140.0, result["final_capital"]


def test_scale_out_ratchet_partial_then_trail_exit():
    # One rung: +1xATR closes 50% AND tightens the trail to 1.0x. Entry $100,
    # ATR 10. Bar reaches $110 (rung clears) -> 50% closed at $110, trail
    # tightens to 1.0x (trigger $100). Price reverses to $98 -> trail exits the
    # remainder at $98.
    df = _df(
        opens=[100, 100, 110, 110, 98, 98],
        closes=[100, 110, 110, 98, 98, 98],
        atrs=[10] * 6,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=3.0,
        close_strategies=[{
            "name": "trailing_tp_ratchet",
            "params": {"tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5, "trailing_mult_after": 1.0},
            ]},
        }],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2, result
    exits = [t["exit_price"] for t in result["trades"]]
    assert exits == [110.0, 98.0], exits
    # 10 sh @100; +5 sh @110 = 550 cash; +5 sh @98 = 490 -> 1040.
    assert result["final_capital"] == 1040.0, result["final_capital"]


def test_regime_keyed_ratchet_freezes_table_at_open():
    # trailing_tp_ratchet_regime: the 'ranging' table tightens to 0.5x at +1xATR.
    # Position opens in 'ranging' (regime frozen at open), so the ranging rung
    # drives the trail. Entry $100, ATR 10; $110 clears the rung -> trail 0.5x
    # (trigger $105); dip to $104 fires; recovery to $130 is not reached.
    n = 7
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": [100, 100, 110, 110, 104, 104, 130],
            "close": [100, 110, 110, 104, 104, 130, 130],
            "atr": [10] * n,
            "open_action": ["long"] + ["none"] * (n - 1),
            # regime stamped at open (shifted N-1 internally when regime_enabled).
            "regime": ["ranging"] * n,
        },
        index=idx,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True,
        trailing_stop_atr_mult=3.0,
        close_strategies=[{
            "name": "trailing_tp_ratchet_regime",
            "params": {"tp_tiers": {
                "trending_up": [{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0}],
                "trending_down": [{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 2.0}],
                "ranging": [{"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 0.5}],
            }},
        }],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1, result
    # 0.5x trail off the $110 high -> trigger $105 -> exits at the $104 bar's
    # next open ($104), not riding up to $130.
    assert result["trades"][0]["exit_price"] == 104.0, result["trades"]
