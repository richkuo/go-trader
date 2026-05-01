"""
Tests for close-strategy registry integration in Backtester (issue #534).

The backtester evaluates the close registry per-bar against the simulated
open position. Result is the max close_fraction across all evaluators,
applied at the next bar's open (same fill alignment as the column-based
close_fraction path).
"""
import pandas as pd

from backtester import Backtester


def _df_open_then_hold(opens, closes, atrs=None):
    """Build a df where bar 0 emits open_action=long; remaining bars hold."""
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    open_actions = ["long"] + ["none"] * (n - 1)
    data = {"open": opens, "close": closes, "open_action": open_actions}
    if atrs is not None:
        data["atr"] = atrs
    return pd.DataFrame(data, index=idx)


def test_tp_at_pct_closes_full_position_when_threshold_hit():
    # Bar 0 emits open_action=long → opens at bar 1's open ($100), 10 shares.
    # Bar 2's close hits +3% → close evaluator fires at end of bar 2,
    # applied at bar 3's open ($103).
    df = _df_open_then_hold(
        opens=[100, 100, 100, 103, 103],
        closes=[100, 100, 103, 103, 103],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tp_at_pct"],
        close_params={"tp_at_pct": {"pct": 0.03}},
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "long"
    assert result["trades"][0]["entry_price"] == 100.0
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 1030.0


def test_tp_at_pct_does_not_fire_when_threshold_not_hit():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 101, 101],
        closes=[100, 100, 101, 101, 101],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tp_at_pct"],
        close_params={"tp_at_pct": {"pct": 0.03}},
    )
    result = bt.run(df, save=False)
    # Position closes at the end of run at the final close ($101).
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 101.0


def test_tiered_tp_atr_partial_then_full_close():
    # ATR=10 throughout. Two tiers: 1×ATR closes 50%, 2×ATR closes 100%.
    # Entry at $100 (bar 1 open). Bar 2 close=$110 → tier 1 fires
    # (close 5 shares at bar 3 open=$110). Bar 3 close=$120 → tier 2 fires
    # (close remaining 5 at bar 4 open=$120).
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 120],
        closes=[100, 100, 110, 120, 120],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tiered_tp_atr"],
        close_params={"tiered_tp_atr": {"tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    assert result["trades"][0]["shares"] == 5.0
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][1]["shares"] == 5.0
    assert result["trades"][1]["exit_price"] == 120.0
    # 5 × ($110 - $100) + 5 × ($120 - $100) = $50 + $100 = $150 PnL.
    assert result["final_capital"] == 1150.0


def test_tiered_tp_atr_live_uses_live_atr_from_market():
    # Same scenario as the snapshot variant but using the live ATR evaluator
    # (atr_source="live") which reads market["atr"] each bar. With constant
    # ATR=10 the result is identical.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 120],
        closes=[100, 100, 110, 120, 120],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tiered_tp_atr_live"],
        close_params={"tiered_tp_atr_live": {
            "atr_source": "live",
            "tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ],
        }},
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 2
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][1]["exit_price"] == 120.0
    assert result["final_capital"] == 1150.0


def test_max_close_fraction_wins_between_two_evaluators():
    # tp_at_pct(2%) fires at +2%; tiered_tp_pct(5%) does not. Larger fraction
    # (1.0 from tp_at_pct) wins → full close.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 102, 102],
        closes=[100, 100, 102, 102, 102],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tp_at_pct", "tiered_tp_pct"],
        close_params={
            "tp_at_pct": {"pct": 0.02},
            "tiered_tp_pct": {"tiers": [
                {"profit_pct": 0.05, "close_fraction": 1.0},
            ]},
        },
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 102.0
    assert result["final_capital"] == 1020.0


def test_close_strategies_unset_preserves_legacy_close_fraction_behavior():
    # Without close_strategies the column-based close_fraction path is the
    # only mechanism — identical to test_open_close_backtester.py expectations.
    idx = pd.date_range("2024-01-01", periods=4, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 110, 110],
        "close": [100, 110, 110, 110],
        "open_action": ["long", "none", "none", "none"],
        "close_fraction": [0, 0, 1.0, 0],
    }, index=idx)

    bt = Backtester(initial_capital=1000, commission_pct=0, slippage_pct=0)
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["final_capital"] == 1100.0


def test_close_strategy_short_position_long_take_profit():
    # Short open at $100; price drops to $97 → tp_at_pct(3%) fires on short.
    n = 5
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 97, 97],
        "close": [100, 100, 97, 97, 97],
        "open_action": ["short", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tp_at_pct"],
        close_params={"tp_at_pct": {"pct": 0.03}},
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "short"
    assert result["trades"][0]["entry_price"] == 100.0
    assert result["trades"][0]["exit_price"] == 97.0
    # Short 10 @ $100 → cash 2000; close 10 @ $97 → cash 2000 - 970 = 1030.
    assert result["final_capital"] == 1030.0


def test_starting_long_seed_with_entry_atr_lets_tiered_tp_atr_fire():
    # Seed a long position at $100 with EntryATR=10. Eval is end-of-bar t,
    # fill at bar t+1's open:
    # - Bar 0 close=$110 → tier 1 fires (1×ATR, 50%) → fills at bar 1 open=$110
    # - Bar 1 close=$120 → tier 2 fires (2×ATR, 100%) → fills at bar 2 open=$120
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open":  [100, 110, 120],
        "close": [110, 120, 120],
        "atr":   [10,  10,  10],
        "open_action": ["none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tiered_tp_atr"],
        close_params={"tiered_tp_atr": {"tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 10.0},
    )
    # Two close legs: tier 1 at $110 (5 shares), tier 2 at $120 (5 shares).
    assert result["total_trades"] == 2
    assert result["trades"][0]["exit_price"] == 110.0
    assert result["trades"][0]["shares"] == 5.0
    assert result["trades"][1]["exit_price"] == 120.0
    assert result["trades"][1]["shares"] == 5.0
    # 5 × $10 + 5 × $20 = $150 PnL.
    assert result["final_capital"] == 1150.0


def test_starting_long_seed_without_entry_atr_atr_evaluator_noops():
    # Same scenario as above but no entry_atr passed — tiered_tp_atr should
    # silently no-op (mirrors live: stampEntryATRIfOpened rejects 0 → noop).
    # Position rides to forced end-of-run close.
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open":  [100, 110, 120],
        "close": [110, 120, 120],
        "atr":   [10,  10,  10],
        "open_action": ["none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=["tiered_tp_atr"],
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0},
    )
    # No tier hits → forced close at the final bar's close ($120).
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 120.0


def test_close_strategy_unknown_name_raises():
    try:
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=["does_not_exist"],
        )
    except ValueError as exc:
        assert "does_not_exist" in str(exc)
    else:
        raise AssertionError("expected ValueError for unknown close strategy")
