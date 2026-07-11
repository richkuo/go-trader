"""
Tests for post-TP stop-loss adjustment (`sl_after`) parity in the backtester (#709).

Mirrors the Go test coverage in scheduler/post_tp_sl_test.go: pure-helper unit
tests for parse/validate/compute, plus end-to-end backtester tests for each
mode (breakeven / atr_offset / trail_from_here) on long and short positions.
"""
from __future__ import annotations

import importlib.util
import os
import sys

import pandas as pd
import pytest


_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))


def _load_post_tp_sl():
    path = os.path.join(_REPO_ROOT, "shared_strategies", "close", "post_tp_sl.py")
    name = "_test_post_tp_sl"
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    # Register in sys.modules before exec so @dataclass can resolve cls.__module__.
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


sl = _load_post_tp_sl()

from backtester import Backtester


# ─── Pure-helper coverage ───────────────────────────────────────────────────


def test_parse_sl_after_rule_breakeven_string():
    rule = sl.parse_sl_after_rule("breakeven")
    assert rule.kind == "breakeven"
    assert rule.is_empty() is False


def test_parse_sl_after_rule_empty_inputs():
    assert sl.parse_sl_after_rule(None).is_empty()
    assert sl.parse_sl_after_rule("").is_empty()


def test_parse_sl_after_rule_implicit_atr_offset():
    rule = sl.parse_sl_after_rule({"atr_mult": 0.25})
    assert rule.kind == "atr_offset"
    assert rule.atr_mult == 0.25


def test_parse_sl_after_rule_negative_atr_mult():
    rule = sl.parse_sl_after_rule({"atr_mult": -0.5})
    assert rule.kind == "atr_offset"
    assert rule.atr_mult == -0.5


def test_parse_sl_after_rule_explicit_kind_atr_offset():
    rule = sl.parse_sl_after_rule({"kind": "atr_offset", "atr_mult": 0.25})
    assert rule.kind == "atr_offset"
    assert rule.atr_mult == 0.25


def test_parse_sl_after_rule_nested_trail_from_here():
    rule = sl.parse_sl_after_rule({"trail_from_here": {"atr_mult": 1.0}})
    assert rule.kind == "trail_from_here"
    assert rule.trail_atr_mult == 1.0


def test_parse_sl_after_rule_explicit_kind_trail_from_here():
    rule = sl.parse_sl_after_rule({"kind": "trail_from_here", "atr_mult": 1.5})
    assert rule.kind == "trail_from_here"
    assert rule.trail_atr_mult == 1.5


@pytest.mark.parametrize("raw", [
    "hold",
    {"kind": "weird"},
    {"trail_from_here": {"atr_mult": -1}},
    {"trail_from_here": {"atr_mult": 0}},
    {"trail_from_here": {}},
    {},
    42,
    {"kind": 1},
    {"trail_from_here": "1.0"},
])
def test_parse_sl_after_rule_errors(raw):
    with pytest.raises(ValueError):
        sl.parse_sl_after_rule(raw)


def test_validate_sl_after_rule_accepts_valid():
    for r in [
        sl.SLAfterRule(),
        sl.SLAfterRule(kind="breakeven"),
        sl.SLAfterRule(kind="atr_offset", atr_mult=0.25),
        sl.SLAfterRule(kind="atr_offset", atr_mult=0),
        sl.SLAfterRule(kind="atr_offset", atr_mult=-0.5),
        sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1.0),
    ]:
        sl.validate_sl_after_rule(r)  # must not raise


def test_validate_sl_after_rule_rejects_bad():
    for r in [
        sl.SLAfterRule(kind="trail_from_here"),
        sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=0),
        sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=-1),
        sl.SLAfterRule(kind="weird"),
    ]:
        with pytest.raises(ValueError):
            sl.validate_sl_after_rule(r)


def test_compute_breakeven_long_and_short():
    px, mode, ok = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="breakeven"), "long", 100, 5, 0,
    )
    assert ok and px == 100 and mode == "breakeven"
    px, mode, ok = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="breakeven"), "short", 200, 5, 0,
    )
    assert ok and px == 200 and mode == "breakeven"


@pytest.mark.parametrize("side,mult,want", [
    ("long", 0.25, 100 + 0.25 * 5),
    ("long", -0.5, 100 - 0.5 * 5),
    ("long", 0, 100),
    ("short", 0.25, 100 - 0.25 * 5),
    ("short", -0.5, 100 + 0.5 * 5),
])
def test_compute_atr_offset(side, mult, want):
    px, _, ok = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="atr_offset", atr_mult=mult), side, 100, 5, 0,
    )
    assert ok
    assert abs(px - want) < 1e-9


@pytest.mark.parametrize("mult,want_mode", [
    (0, "atr+0"),
    (0.25, "atr+0.25"),
    (-0.5, "atr-0.5"),
    (1, "atr+1"),
])
def test_compute_atr_offset_mode_label(mult, want_mode):
    _, mode, _ = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="atr_offset", atr_mult=mult), "long", 100, 5, 0,
    )
    assert mode == want_mode


def test_compute_trail_from_here_long_and_short():
    px, mode, ok = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1.0),
        "long", 100, 5, 110,
    )
    assert ok and abs(px - (110 - 1.0 * 5)) < 1e-9 and "trail" in mode
    px, _, ok = sl.compute_post_tp_stop_loss_trigger(
        sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1.5),
        "short", 100, 5, 90,
    )
    assert ok and abs(px - (90 + 1.5 * 5)) < 1e-9


@pytest.mark.parametrize("rule,side,avg,atr,mark", [
    (sl.SLAfterRule(), "long", 100, 5, 0),
    (sl.SLAfterRule(kind="breakeven"), "neutral", 100, 5, 0),
    (sl.SLAfterRule(kind="breakeven"), "long", 0, 5, 0),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=0.25), "long", 100, 0, 0),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1), "long", 100, 0, 110),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1), "long", 100, 5, 0),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=0), "long", 100, 5, 110),
    (sl.SLAfterRule(kind="weird"), "long", 100, 5, 110),
])
def test_compute_rejects_bad_inputs(rule, side, avg, atr, mark):
    _, _, ok = sl.compute_post_tp_stop_loss_trigger(rule, side, avg, atr, mark)
    assert not ok


def test_parse_strategy_tp_sl_after_rules_default_and_per_tier_override():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {
            "sl_after": "breakeven",
            "tp_tiers": [
                # out of order — should sort ascending by atr_multiple
                {"atr_multiple": 3, "close_fraction": 1.0,
                 "sl_after": {"atr_mult": 0.25}},
                {"atr_multiple": 2, "close_fraction": 0.5},
            ],
        },
    }]
    rules, errs = sl.parse_strategy_tp_sl_after_rules(refs)
    assert errs == []
    assert rules.default.kind == "breakeven"
    assert len(rules.per_tier) == 2
    assert rules.per_tier[0].is_empty()  # tier mult=2 → inherits default
    assert rules.per_tier[1].kind == "atr_offset"
    assert rules.per_tier[1].atr_mult == 0.25
    assert rules.has_any()
    assert rules.for_tier(0).kind == "breakeven"
    assert rules.for_tier(1).kind == "atr_offset"
    assert rules.for_tier(99).kind == "breakeven"  # out of range → default


def test_parse_strategy_tp_sl_after_rules_no_tiered_tp():
    rules, errs = sl.parse_strategy_tp_sl_after_rules([
        {"name": "tp_at_pct", "params": {"pct": 0.05}},
    ])
    assert errs == [] and not rules.has_any()


def test_parse_strategy_tp_sl_after_rules_reports_malformed():
    refs = [{
        "name": "tiered_tp_atr",
        "params": {
            "sl_after": "unknown-string",
            "tp_tiers": [
                {"atr_multiple": 2, "close_fraction": 0.5,
                 "sl_after": {"kind": "weird"}},
                {"atr_multiple": 3, "close_fraction": 1.0},
            ],
        },
    }]
    _, errs = sl.parse_strategy_tp_sl_after_rules(refs)
    assert len(errs) >= 2


def test_validate_rejects_combination_with_trailing():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {
            "sl_after": "breakeven",
            "tp_tiers": [{"atr_multiple": 2, "close_fraction": 0.5},
                      {"atr_multiple": 3, "close_fraction": 1.0}],
        },
    }]
    errs = sl.validate_post_tp_stop_loss_rules(
        refs, stop_loss_atr_mult=1.0, trailing_stop_atr_mult=1.5,
    )
    assert any("trailing_stop" in e for e in errs)


def test_validate_rejects_no_fixed_sl():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {
            "sl_after": "breakeven",
            "tp_tiers": [{"atr_multiple": 2, "close_fraction": 0.5},
                      {"atr_multiple": 3, "close_fraction": 1.0}],
        },
    }]
    errs = sl.validate_post_tp_stop_loss_rules(refs)
    assert any("fixed stop-loss" in e for e in errs)


def test_validate_accepts_valid():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {
            "sl_after": "breakeven",
            "tp_tiers": [
                {"atr_multiple": 2, "close_fraction": 0.5},
                {"atr_multiple": 3, "close_fraction": 1.0,
                 "sl_after": {"atr_mult": 0.5}},
            ],
        },
    }]
    assert sl.validate_post_tp_stop_loss_rules(refs, stop_loss_atr_mult=1.0) == []


def test_validate_rejects_trail_from_here_on_manual():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {
            "sl_after": {"trail_from_here": {"atr_mult": 1.0}},
            "tp_tiers": [{"atr_multiple": 2, "close_fraction": 0.5},
                      {"atr_multiple": 3, "close_fraction": 1.0}],
        },
    }]
    errs = sl.validate_post_tp_stop_loss_rules(
        refs, stop_loss_atr_mult=1.5, strategy_type="manual",
    )
    assert any("trail_from_here is not supported on manual" in e for e in errs)


def test_validate_rejects_sl_after_on_non_tiered_close_ref():
    refs = [{
        "name": "tp_at_pct",
        "params": {"pct": 0.05, "sl_after": "breakeven"},
    }]
    errs = sl.validate_post_tp_stop_loss_rules(refs, stop_loss_atr_mult=1.0)
    assert any("only honored on tiered_tp_atr" in e for e in errs)


def test_validate_rejects_per_tier_sl_after_on_non_tiered():
    refs = [{
        "name": "tiered_tp_pct",
        "params": {
            "tp_tiers": [
                {"pct": 0.05, "close_fraction": 0.5, "sl_after": "breakeven"},
            ],
        },
    }]
    errs = sl.validate_post_tp_stop_loss_rules(refs, stop_loss_atr_mult=1.0)
    assert any("no effect" in e and "tiered_tp_pct" in e for e in errs)


def test_validate_no_op_when_sl_after_absent():
    refs = [{
        "name": "tiered_tp_atr_live",
        "params": {"tp_tiers": [
            {"atr_multiple": 2, "close_fraction": 0.5},
            {"atr_multiple": 3, "close_fraction": 1.0},
        ]},
    }]
    assert sl.validate_post_tp_stop_loss_rules(refs, stop_loss_atr_mult=1.0) == []


def test_parse_tp_tier_close_fractions_sorts_and_coerces_final():
    refs = [{
        "name": "tiered_tp_atr",
        "params": {"tp_tiers": [
            {"atr_multiple": 3, "close_fraction": 0.9},  # final → coerced 1.0
            {"atr_multiple": 1, "close_fraction": 0.25},
            {"atr_multiple": 2, "close_fraction": 0.5},
        ]},
    }]
    fractions = sl.parse_tp_tier_close_fractions(refs)
    assert fractions == [0.25, 0.5, 1.0]


def test_find_highest_cleared_tier_basic():
    # Cumulative thresholds [0.5, 1.0]
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.0) == -1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.5) == 0
    assert sl.find_highest_cleared_tier([0.5, 1.0], 1.0) == 1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.5, from_idx=1) == -1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 1.0, from_idx=1) == 1


# ─── Backtester integration ────────────────────────────────────────────────


def _df_open_then_hold(opens, closes, atrs=None, open_actions=None):
    """Build a df with an open_action sequence; default is open at bar 0
    then 'none' on the rest."""
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    if open_actions is None:
        open_actions = ["long"] + ["none"] * (n - 1)
    data = {"open": opens, "close": closes, "open_action": open_actions}
    if atrs is not None:
        data["atr"] = atrs
    return pd.DataFrame(data, index=idx)


def test_backtester_breakeven_after_tp1_long():
    """TP1 at +1×ATR closes 50%, then SL bumps to breakeven (avg cost). A
    subsequent retrace below avg cost triggers the SL → full close at
    breakeven price on the next bar."""
    # ATR=10, entry @ $100. Bar 1 opens long. Bar 2 close=$110 → tier 1 fires,
    # 50% closes at bar 3 open=$110. SL bumps to $100 (breakeven). Bar 4
    # close=$95 < SL → triggers. Bar 5 opens at $95, full close.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 100, 95],
        closes=[100, 100, 110, 110, 95, 95],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    # Two trades: TP1 partial @ $110, SL flat @ $95.
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices, sides_prices
    assert ("long", 95.0) in sides_prices, sides_prices


def test_backtester_breakeven_after_tp1_short():
    """Mirror image for shorts. ATR=10, entry @ $100 short. Bar 2 close=$90
    → tier 1 fires, half closes at bar 3 open=$90. SL bumps to $100. Bar 4
    close=$110 > SL → full close at bar 5 open=$110."""
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 100, 110],
        closes=[100, 100, 90, 90, 110, 110],
        atrs=[10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("short", 90.0) in sides_prices, sides_prices
    assert ("short", 110.0) in sides_prices, sides_prices


def test_backtester_atr_offset_after_tp1_long():
    """sl_after = atr_offset 0.5: SL bumps to avg + 0.5×ATR = $105 after
    TP1. Subsequent bar dipping to $104 triggers."""
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 105, 104],
        closes=[100, 100, 110, 110, 104, 104],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"atr_mult": 0.5},
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices
    # Final close fires when bar 4 closes at $104 (≤ $105 SL); fills at bar 5
    # open = $104. Last bar would otherwise force-close at end of run.
    assert ("long", 104.0) in sides_prices, sides_prices


def test_backtester_trail_from_here_long_walks_up():
    """trail_from_here at 1×ATR after TP1: SL trigger trails the high-water
    mark by 1×ATR. Price rises to $118 (hwm), pulls back to $107 < trigger
    ($108) → SL fires. Prices stay below TP2's 2×ATR ($120) so this isolates
    the trail behavior from the second tier firing."""
    # Entry at bar 1 ($100). Bar 2 close=$110 → TP1 fires.
    # Bar 3 open=$110 (TP1 partial, seeds sl_trigger=$100).
    # Bar 3 close=$115 → hwm=$115, trigger=$105.
    # Bar 4 close=$118 → hwm=$118, trigger=$108.
    # Bar 5 close=$107 → 107 < trigger ($108) → SL fires.
    # Bar 6 open=$107 → trail SL fill.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 115, 118, 107],
        closes=[100, 100, 110, 115, 118, 107, 107],
        atrs=[10, 10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    # TP1 partial close at $110.
    assert ("long", 110.0) in sides_prices, sides_prices
    # Trail SL fires at bar 6 open = $107.
    assert ("long", 107.0) in sides_prices, sides_prices


def test_backtester_tp_atr_fraction_uses_firing_tier_multiple():
    """tp_atr_fraction=0.5 on a 2×ATR TP1 resolves to a 1×ATR trail."""
    df = _df_open_then_hold(
        opens=[100, 100, 100, 120, 125, 128, 117],
        closes=[100, 100, 120, 125, 128, 117, 117],
        atrs=[10, 10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"trail_from_here": {"tp_atr_fraction": 0.5}},
                "tp_tiers": [
                    {"atr_multiple": 2.0, "close_fraction": 0.5},
                    {"atr_multiple": 4.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 120.0) in sides_prices, sides_prices
    assert ("long", 117.0) in sides_prices, sides_prices


def test_backtester_tp_atr_fraction_uses_default_tier_multiple():
    """#870: default TP1 is 1.5×ATR, so tp_atr_fraction=0.5 resolves to 0.75×ATR.

    With ATR=10 the first tier fires at +15 (mark 115), scaling out 40%; the
    remainder rides to the forced close at 112.
    """
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 115, 118, 112],
        closes=[100, 100, 110, 115, 118, 112, 112],
        atrs=[10, 10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"trail_from_here": {"tp_atr_fraction": 0.5}},
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 115.0) in sides_prices, sides_prices
    assert ("long", 112.0) in sides_prices, sides_prices


def test_backtester_trail_from_here_short_walks_down():
    """Mirror image for shorts: hwm tracks lowest mark, trigger sits above
    it by trail_atr_mult × ATR. Prices stay above TP2's 2×ATR ($80)
    threshold so this isolates the trail behavior."""
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 85, 82, 93],
        closes=[100, 100, 90, 85, 82, 93, 93],
        atrs=[10, 10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 6,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("short", 90.0) in sides_prices, sides_prices
    # Bar 3 partial fill at $90, sl_trigger seeds at $90 + 10 = $100.
    # End-of-bar 3 walk: hwm=$85, trigger=$95.
    # End-of-bar 4 close=$82: hwm=$82, trigger=$92.
    # End-of-bar 5 close=$93 → 93 > trigger ($92) → SL fires at bar 6 open=$93.
    assert ("short", 93.0) in sides_prices, sides_prices


def test_backtester_validation_rejects_no_fixed_sl():
    with pytest.raises(ValueError, match="fixed stop-loss"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "sl_after": "breakeven",
                    "tp_tiers": [
                        {"atr_multiple": 2, "close_fraction": 0.5},
                        {"atr_multiple": 3, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_combo_with_trailing():
    with pytest.raises(ValueError, match="trailing_stop"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0, trailing_stop_atr_mult=1.5,
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "sl_after": "breakeven",
                    "tp_tiers": [
                        {"atr_multiple": 2, "close_fraction": 0.5},
                        {"atr_multiple": 3, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_trail_from_here_on_manual():
    with pytest.raises(ValueError, match="trail_from_here is not supported on manual"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="manual",
            stop_loss_atr_mult=1.5,
            close_strategies=[{
                "name": "tiered_tp_atr_live",
                "params": {
                    "sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                    "tp_tiers": [
                        {"atr_multiple": 2, "close_fraction": 0.5},
                        {"atr_multiple": 3, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_sl_after_on_non_tiered_ref():
    with pytest.raises(ValueError, match="only honored on tiered_tp_atr"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0,
            close_strategies=[{
                "name": "tp_at_pct",
                "params": {"pct": 0.05, "sl_after": "breakeven"},
            }],
        )


# #736 — regime-aware sl_after parses cleanly (live HL uses it) but the
# backtester defers per-regime resolution to a follow-up parity issue. Verify
# the loader fails loud rather than silently producing scalar=0 results.
def test_backtester_validation_rejects_regime_sl_after_strategy_default():
    with pytest.raises(ValueError, match="HL-live-only"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0,
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "sl_after": {
                        "trend_regime": {"trending_up": {"atr_multiple": 0.25},
                            "trending_down": {"atr_multiple": 0.25},
                            "ranging": {"atr_multiple": 0.0},
                        },
                    },
                    "tp_tiers": [
                        {"atr_multiple": 1.0, "close_fraction": 0.5},
                        {"atr_multiple": 2.0, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_regime_sl_after_per_tier():
    with pytest.raises(ValueError, match="HL-live-only"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0,
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "tp_tiers": [
                        {
                            "atr_multiple": 1.0,
                            "close_fraction": 0.5,
                            "sl_after": {
                                "trail_from_here": {
                                    "trend_regime": {"trending_up": {"atr_multiple": 1.0},
                                        "trending_down": {"atr_multiple": 1.0},
                                        "ranging": {"atr_multiple": 0.5},
                                    },
                                },
                            },
                        },
                        {"atr_multiple": 2.0, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_regime_tp_atr_fraction():
    with pytest.raises(ValueError, match="HL-live-only"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0,
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "sl_after": {
                        "trail_from_here": {
                            "tp_atr_fraction": {
                                "trend_regime": {
                                    "trending_up": 0.75,
                                    "trending_down": 0.75,
                                    "ranging": 0.5,
                                },
                            },
                        },
                    },
                    "tp_tiers": [
                        {"atr_multiple": 2.0, "close_fraction": 0.5},
                        {"atr_multiple": 4.0, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_validation_rejects_regime_tp_atr_fraction_on_regime_tier():
    with pytest.raises(ValueError, match="HL-live-only"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_atr_mult=1.0,
            close_strategies=[{
                "name": "tiered_tp_atr_regime",
                "params": {
                    "tp_tiers": [
                        {
                            "trend_regime": {"trending_up": {"atr_multiple": 2.0},
                                "trending_down": {"atr_multiple": 2.0},
                                "ranging": {"atr_multiple": 1.5},
                            },
                            "close_fraction": 0.5,
                            "sl_after": {
                                "trail_from_here": {
                                    "tp_atr_fraction": {
                                        "trend_regime": {
                                            "trending_up": 0.75,
                                            "trending_down": 0.75,
                                            "ranging": 0.5,
                                        },
                                    },
                                },
                            },
                        },
                        {
                            "trend_regime": {"trending_up": {"atr_multiple": 4.0},
                                "trending_down": {"atr_multiple": 4.0},
                                "ranging": {"atr_multiple": 3.0},
                            },
                            "close_fraction": 1.0,
                        },
                    ],
                },
            }],
        )


def test_backtester_no_sl_after_unchanged_behavior():
    """Sanity check: a strategy with tiered_tp_atr but no sl_after still
    behaves as before — no extra SL hits, partial close at tier price."""
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 90],
        closes=[100, 100, 110, 90, 90],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    # TP1 partial @ $110, then end-of-run forced close at $90 (no SL active).
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices
    assert ("long", 90.0) in sides_prices


def test_backtester_multi_tier_cleared_same_bar_highest_wins():
    """Price jumps past tier 0 directly to tier 1 in a single bar. The
    highest cleared tier's rule must win — proves find_highest_cleared_tier
    is wired through and a lower tier's rule doesn't shadow the higher
    tier's intent."""
    # 3 tiers at 1×/2×/3× ATR (cumulative close_fractions 0.3/0.6/1.0).
    # Tier 0 rule = breakeven ($100); tier 1 rule = atr_offset +1.0×ATR
    # ($110); tier 2 rule = atr_offset +2.0×ATR ($120). Bar 2 close jumps
    # straight to $120 = 2×ATR → tier 0 AND tier 1 both clear in the same
    # bar; final tier untouched (close_ratio=0.6 < 1.0).
    #
    # If tier 1's rule wins (correct): SL=$110. Bar 4 close=$105 < $110 →
    # SL fires at bar 5 open=$105.
    # If tier 0 incorrectly won: SL=$100, $105 wouldn't trigger.
    df = _df_open_then_hold(
        opens=[100, 100, 100, 120, 110, 105],
        closes=[100, 100, 120, 120, 105, 105],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=2.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.3,
                     "sl_after": "breakeven"},
                    {"atr_multiple": 2.0, "close_fraction": 0.6,
                     "sl_after": {"atr_mult": 1.0}},
                    {"atr_multiple": 3.0, "close_fraction": 1.0,
                     "sl_after": {"atr_mult": 2.0}},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    # Cumulative TP fill at $120 (the cumulative-target close evaluator may
    # emit two separate fills at the same bar's open price; that's fine —
    # just assert the SL trigger fired post-bump).
    assert ("long", 120.0) in sides_prices, sides_prices
    # SL fires at $105 (only possible if tier 1's rule won — tier 0's
    # $100 breakeven wouldn't have triggered on a $105 close).
    assert ("long", 105.0) in sides_prices, sides_prices


def test_backtester_sl_after_idempotent_across_bars():
    """A tier's sl_after rule must apply exactly once. Direct unit-level
    check on _maybe_apply_sl_after: calling it twice with the same simulated
    state after a tier cleared must produce a no-op the second time (the
    watermark short-circuits re-evaluation), mirroring the live
    SLAdjustedTiersProcessed guard."""
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"atr_mult": 0.5},
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    # Simulate state right after TP1 fires: 50% of initial qty closed.
    kwargs = dict(
        side="long",
        avg_cost=100.0,
        entry_atr=10.0,
        position_qty=0.5,
        initial_qty=1.0,
        mark_price=110.0,
        fill_price=110.0,
        sl_trigger_px=90.0,  # initial SL @ avg - 1×ATR
        sl_tiers_processed=0,
        post_tp_trail_mult=None,
        sl_high_water_px=0.0,
    )
    trig1, processed1, trail1, hwm1 = bt._maybe_apply_sl_after(**kwargs)
    # First call applies tier 0's rule: SL bumps to $100 + 0.5×$10 = $105,
    # watermark advances past tier 0.
    assert trig1 == 105.0
    assert processed1 == 1
    assert trail1 is None

    # Second call with the now-current state (same closed_ratio, watermark
    # already past tier 0) must be a no-op — no further bump, watermark
    # stays put.
    kwargs2 = dict(kwargs)
    kwargs2.update(
        sl_trigger_px=trig1,
        sl_tiers_processed=processed1,
        post_tp_trail_mult=trail1,
        sl_high_water_px=hwm1,
    )
    trig2, processed2, trail2, hwm2 = bt._maybe_apply_sl_after(**kwargs2)
    assert trig2 == trig1
    assert processed2 == processed1
    assert trail2 == trail1
    assert hwm2 == hwm1


def test_backtester_validation_rejects_margin_pct_only():
    """stop_loss_margin_pct cannot be the sole fixed SL in backtests —
    the backtester does not model leverage, so the pre-TP SL would never
    fire and post-TP bumps would silently diverge from live. Reject loudly."""
    with pytest.raises(ValueError, match="stop_loss_margin_pct"):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid", strategy_type="perps",
            stop_loss_margin_pct=0.5,
            close_strategies=[{
                "name": "tiered_tp_atr",
                "params": {
                    "sl_after": "breakeven",
                    "tp_tiers": [
                        {"atr_multiple": 1.0, "close_fraction": 0.5},
                        {"atr_multiple": 2.0, "close_fraction": 1.0},
                    ],
                },
            }],
        )


def test_backtester_sl_after_defers_when_sl_unarmed():
    """Mirrors live `currentOID == 0` short-circuit: when the fixed SL
    couldn't be seeded (e.g., ATR-mult SL but entry ATR=0 at open),
    breakeven must defer instead of installing a fresh trigger — otherwise
    the backtester would diverge from live by arming an SL where live
    would not."""
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    # Simulate state right after a TP partial fired but SL was never armed
    # (sl_trigger_px = 0). breakeven would naturally install SL=$100, but
    # the gate must defer.
    trig, processed, trail, hwm = bt._maybe_apply_sl_after(
        side="long",
        avg_cost=100.0,
        entry_atr=10.0,
        position_qty=0.5,
        initial_qty=1.0,
        mark_price=110.0,
        fill_price=110.0,
        sl_trigger_px=0.0,  # unarmed
        sl_tiers_processed=0,
        post_tp_trail_mult=None,
        sl_high_water_px=0.0,
    )
    # Defer: trigger unchanged, watermark NOT advanced (so a later bar
    # could retry if conditions improve — same as live).
    assert trig == 0.0
    assert processed == 0
    assert trail is None
    assert hwm == 0.0


def test_backtester_sl_after_no_same_bar_fire_after_bump_long():
    """Regression for #715: when a TP tier fills on bar N and `sl_after` bumps
    the SL trigger, the end-of-bar SL hit check on bar N must NOT fire — the
    new SL OID lands mid-cycle in live (after the TP fill), so collapsing both
    events into one bar would over-trigger.

    Setup (long, ATR=10, entry @ $100):
      Bar 1 (01-02): open long @ $100, close $100
      Bar 2 (01-03): close $110 → TP1 tier detected (queues 50% close for bar 3)
      Bar 3 (01-04): open $110 → TP1 partial fills, SL bumps to $100 (breakeven),
                     close $99 → WITHOUT the fix, SL hit check fires same bar
      Bar 4 (01-05): close $99 → SL hit check fires (queues full close for bar 5)
      Bar 5 (01-06): open $99 → full close fills here

    With the fix, the SL close fills on 2024-01-06. Without it, the suppressed
    same-bar SL fire on bar 3 would queue the full close one bar earlier
    (exit_date 2024-01-05).
    """
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 99, 99],
        closes=[100, 100, 110, 99, 99, 99],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)

    # Two trades expected: TP1 partial @ $110 on bar 3, SL full @ $99 on bar 5.
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices, sides_prices
    assert ("long", 99.0) in sides_prices, sides_prices

    # The SL close must NOT exit on bar 4 (same-bar fire after bump). It must
    # exit on bar 5 (one full bar later, after the flag clears and the next
    # bar's end-of-bar check fires).
    sl_closes = [t for t in result["trades"] if t["exit_price"] == 99.0]
    assert len(sl_closes) == 1, sl_closes
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_no_same_bar_fire_after_bump_short():
    """Short-side mirror of the #715 regression. The gate is side-agnostic;
    this locks in the symmetry against future refactors.

    Setup (short, ATR=10, entry @ $100):
      Bar 1 (01-02): open short @ $100
      Bar 2 (01-03): close $90  → TP1 detected (price dropped 1×ATR)
      Bar 3 (01-04): open $90   → TP1 partial fills, SL bumps to $100,
                                  close $101 → WITHOUT the fix, SL fires
                                  same bar (short: mark ≥ trigger)
      Bar 4 (01-05): close $101 → SL hit check fires, queues full close
      Bar 5 (01-06): open $101  → full close fills here
    """
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 101, 101],
        closes=[100, 100, 90, 101, 101, 101],
        atrs=[10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("short", 90.0) in sides_prices, sides_prices
    assert ("short", 101.0) in sides_prices, sides_prices
    sl_closes = [t for t in result["trades"] if t["exit_price"] == 101.0]
    assert len(sl_closes) == 1, sl_closes
    # Without the fix, the same-bar fire on bar 3 would queue the close at
    # bar 4 open (exit_date 2024-01-05). With the fix, the next bar's hit
    # check fires and the close fills on bar 5 open (2024-01-06).
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_flag_clears_for_next_bar_long():
    """Companion to #715: the suppression flag must reset on the bar AFTER
    the bump. If the bump bar's close is above the new trigger (no same-bar
    fire) and the NEXT bar's close drops below the trigger, the SL must fire
    on that next bar — not be silently skipped.

    Setup (long, ATR=10, entry @ $100):
      Bar 1 (01-02): open long @ $100
      Bar 2 (01-03): close $110 → TP1 detected
      Bar 3 (01-04): open $110 → TP1 partial fills, SL bumps to $100, close
                     $105 → suppression flag set but SL not hit anyway
      Bar 4 (01-05): close $95 → SL hit check runs (flag cleared), queues
                     full close for bar 5
      Bar 5 (01-06): open $95 → full close fills here
    """
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 105, 95],
        closes=[100, 100, 110, 105, 95, 95],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.5},
                    {"atr_multiple": 2.0, "close_fraction": 1.0},
                ],
            },
        }],
    )
    result = bt.run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices, sides_prices
    assert ("long", 95.0) in sides_prices, sides_prices
    sl_closes = [t for t in result["trades"] if t["exit_price"] == 95.0]
    assert len(sl_closes) == 1, sl_closes
    # SL queued at bar 4 close, fills at bar 5 open — confirming the flag
    # cleared after bar 3 (the bump bar).
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_does_not_seed_when_no_tier_thresholds():
    """#716 item 3 regression: a degenerate sl_after config where
    `parse_tp_tier_close_fractions` returns [] (e.g., all tiers have
    close_fraction=0, which the parser drops) must NOT seed a phantom
    fixed-SL trigger at open. Without tier thresholds the post-TP
    adjustment machinery never fires; a seeded-then-never-adjusted SL
    would represent a fixed-SL behavior the rest of the engine doesn't
    actually simulate.

    Construct a strategy with tiers whose close_fractions are all zero
    so the parser drops them, then verify the SL never installs.
    """
    df = _df_open_then_hold(
        opens=[100, 100, 100, 80, 80],
        closes=[100, 100, 80, 80, 80],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",  # pin pre-#1271 legacy fill semantics this scenario encodes
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                # All zero close_fractions are dropped by the parser →
                # _tp_tier_thresholds == [].
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.0},
                    {"atr_multiple": 2.0, "close_fraction": 0.0},
                ],
            },
        }],
    )
    # Pre-condition: parser returned no usable thresholds.
    assert bt._tp_tier_thresholds_static == []
    result = bt.run(df, save=False)
    # No SL fire — price dropped to $80 (well below the would-be SL at $90)
    # but the gate must skip the seed entirely. Only the end-of-run forced
    # close should appear.
    sl_fires = [t for t in result["trades"] if t.get("exit_price") in (90.0, 89.0, 91.0)]
    assert not sl_fires, f"phantom SL fired at {[t['exit_price'] for t in sl_fires]}"
