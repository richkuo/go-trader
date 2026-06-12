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
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
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
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
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
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
        ],
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
        close_strategies=[
            {"name": "tiered_tp_atr_live", "params": {
            "atr_source": "live",
            "tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.5},
                {"atr_multiple": 2.0, "close_fraction": 1.0},
            ],
        }},
        ],
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
        close_strategies=[
            {"name": "tp_at_pct", "params": {"pct": 0.02}},
            {"name": "tiered_tp_pct", "params": {"tp_tiers": [
                {"profit_pct": 0.05, "close_fraction": 1.0},
            ]}},
        ],
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
        close_strategies=[{"name": "tp_at_pct", "params": {"pct": 0.03}}],
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
        close_strategies=[
            {"name": "tiered_tp_atr", "params": {"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.5},
            {"atr_multiple": 2.0, "close_fraction": 1.0},
        ]}},
        ],
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
        close_strategies=[{"name": "tiered_tp_atr"}],
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0},
    )
    # No tier hits → forced close at the final bar's close ($120).
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 120.0


def test_trailing_tp_ratchet_trail_only_tier_exits_on_tightened_trail():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 110, 99, 120],
        "close": [100, 100, 110, 99, 120, 120],
        "atr": [10, 10, 10, 10, 10, 10],
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=3.0,
        close_strategies=[{"name": "trailing_tp_ratchet", "params": {
            "tp_tiers": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
        }}],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])
    assert result["trades"][0]["exit_price"] == 99.0


def test_trailing_tp_ratchet_regime_uses_open_time_regime():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 110, 99, 120],
        "close": [100, 100, 110, 99, 120, 120],
        "atr": [10, 10, 10, 10, 10, 10],
        "regime": ["ranging", "ranging", "trending_up", "trending_up", "trending_up", "trending_up"],
        "open_action": ["long", "none", "none", "none", "none", "none"],
    }, index=idx)
    close_ref = {
        "name": "trailing_tp_ratchet_regime",
        "params": {"tp_tiers": {
            "ranging": [
                {"atr_multiple": 1.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
            "trending_up": [
                {"atr_multiple": 99.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
            "trending_down": [
                {"atr_multiple": 99.0, "close_fraction": 0.0, "trailing_mult_after": 1.0},
            ],
        }},
    }
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        # #870: the regime variant's opening trail / SL owner is the per-regime
        # trailing_stop_atr_regime block (scalar trailing_stop_atr_mult rejected).
        # Open 3.0 preserves the prior initial trail distance.
        trailing_stop_atr_regime={"trend_regime": {
            "ranging": {"atr_multiple": 3.0},
            "trending_up": {"atr_multiple": 3.0},
            "trending_down": {"atr_multiple": 3.0},
        }},
        close_strategies=[close_ref],
    )
    result = bt.run(df, save=False)

    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])
    assert result["trades"][0]["exit_price"] == 99.0


def test_close_strategy_unknown_name_raises():
    try:
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            close_strategies=[{"name": "does_not_exist"}],
        )
    except ValueError as exc:
        assert "does_not_exist" in str(exc)
    else:
        raise AssertionError("expected ValueError for unknown close strategy")


# ---------------------------------------------------------------------------
# #996: bare fixed/trailing/pct stops paired with a close evaluator. Live arms
# these via runHyperliquidProtectionSync / armTrailingStopAtOpenNow regardless
# of sl_after; pre-#996 the open/close engine path silently dropped them
# (the SL trigger was only seeded when sl_after had usable tier thresholds).
# ---------------------------------------------------------------------------

_FAR_TP = [{"name": "tp_at_pct", "params": {"pct": 0.5}}]  # never fires


def test_scalar_atr_stop_fires_alongside_close_evaluator():
    # Entry bar 1 @100, ATR=2, mult=1 → trigger 98. Bar 2 close=96 breaches;
    # fill at bar 3's OPEN (95, distinct from the breach close — pins the
    # N→N+1 fill alignment, no look-ahead).
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 96, 95, 95],
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_scalar_atr_stop_inverse_no_breach_is_noop():
    # Same config, price never reaches the 98 trigger → identical to no-stop.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 99, 99],
        closes=[100, 100, 99, 99, 99],
        atrs=[2.0] * 5,
    )
    kw = dict(initial_capital=1000, commission_pct=0, slippage_pct=0,
              close_strategies=_FAR_TP)
    with_stop = Backtester(stop_loss_atr_mult=1.0, **kw).run(df.copy(), save=False)
    no_stop = Backtester(**kw).run(df.copy(), save=False)
    assert with_stop["final_capital"] == no_stop["final_capital"]
    assert with_stop["total_trades"] == no_stop["total_trades"]


def test_scalar_trailing_stop_walks_alongside_close_evaluator():
    # Entry bar 1 @100, ATR=2, trail mult=1. Bar 1 close=106 ratchets the
    # trigger to 104; bar 3 close=103 breaches the WALKED trigger (the
    # entry-anchored level would be 98, never touched) → fill bar 4 open.
    df = _df_open_then_hold(
        opens=[100, 100, 106, 106, 103, 103],
        closes=[100, 106, 106, 103, 103, 103],
        atrs=[2.0] * 6,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, trailing_stop_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 1030.0


def test_scalar_atr_stop_protects_short_side():
    # Short entry bar 1 @100, ATR=2, mult=1 → trigger 102. Bar 2 close=103
    # breaches (price moved against the short) → fill bar 3 open=103.
    n = 5
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 100, 100, 103, 103],
        "close": [100, 100, 103, 103, 103],
        "atr": [2.0] * n,
        "open_action": ["short"] + ["none"] * (n - 1),
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["side"] == "short"
    assert result["trades"][0]["exit_price"] == 103.0
    assert result["final_capital"] == 970.0


def test_pct_stop_fires_alongside_close_evaluator():
    # stop_loss_pct=0.02 → trigger 98; same shape as the ATR variant.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 95, 95],
        closes=[100, 100, 96, 95, 95],
        atrs=[2.0] * 5,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_pct=0.02,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["final_capital"] == 950.0


def test_tp_tier_partial_then_scalar_stop_closes_remainder():
    # Compound: a 1-ATR TP tier banks half at 102, then the crash through the
    # 98 stop closes the remainder — both exits must book.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 102, 102, 96, 96],
        closes=[100, 100, 102, 102, 96, 96, 96],
        atrs=[2.0] * 7,
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=[{"name": "tiered_tp_atr", "params": {
            "tp_tiers": [{"atr_multiple": 1.0, "close_fraction": 0.5}],
        }}],
        stop_loss_atr_mult=1.0,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 2
    exits = sorted(t["exit_price"] for t in result["trades"])
    assert exits == [96.0, 102.0]
    # 5 shares banked at 102 + 5 shares stopped at 96
    assert result["final_capital"] == 5 * 102.0 + 5 * 96.0


# ---------------------------------------------------------------------------
# PR #1000 review: a position carried across a walk-forward fold boundary
# (starting_long seed) must be managed by the same close stack as a position
# opened mid-window — the seed block arms the fixed/trailing SL trigger from
# the seeded entry_atr/high_water instead of leaving it at 0 for the carried
# position's lifetime. Seed stamping itself is covered in
# test_walk_forward_warmup.py.
# ---------------------------------------------------------------------------

def test_seeded_position_fixed_atr_stop_fires_plain_path():
    # Plain signal path (no close refs). Seed long @100 with EntryATR=2,
    # stop mult=2 → trigger 96. Bar 0 close=95 breaches → fill bar 1 open.
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "high": [100, 95, 95],
        "low": [95, 95, 95],
        "close": [95, 95, 95],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_seeded_position_trailing_stop_anchors_at_seed_high_water():
    # Trail mult=2, EntryATR=2, seeded high_water=110 → trigger 106. Bar 0
    # close=105 breaches the warmup-walked trigger (entry-anchored would be
    # 96, never touched) → fill bar 1 open=105, not the 120 ride.
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [105, 105, 120],
        "high": [105, 120, 120],
        "low": [105, 105, 120],
        "close": [105, 120, 120],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        trailing_stop_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0,
                       "high_water": 110.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 105.0


def test_seeded_position_fixed_atr_stop_fires_engine_path():
    # Same carried-position stop, open/close engine path (a far TP close ref
    # alongside the bare stop — the joint-sweep stack shape). Trigger 96,
    # bar 0 close=95 breaches → fill bar 1 open=95.
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "close": [95, 95, 95],
        "atr": [2.0] * n,
        "open_action": ["none"] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        close_strategies=_FAR_TP, stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0, "entry_atr": 2.0},
    )
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] == 95.0
    assert result["final_capital"] == 950.0


def test_seeded_position_without_entry_atr_stop_stays_unarmed():
    # Boundary: no entry_atr in the seed → the ATR stop cannot price a
    # trigger and must stay unarmed (no spurious exits), matching the
    # mid-window open behavior when ATR is unavailable.
    n = 3
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame({
        "open": [100, 95, 95],
        "high": [100, 95, 95],
        "low": [95, 95, 95],
        "close": [95, 95, 95],
        "signal": [0] * n,
    }, index=idx)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        stop_loss_atr_mult=2.0,
    )
    result = bt.run(
        df, save=False,
        starting_long={"entry_price": 100.0},
    )
    # Rides to the forced end-of-run close at 95 — exactly one forced close.
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[-1])
