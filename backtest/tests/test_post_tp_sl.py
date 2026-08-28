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
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    return mod


sl = _load_post_tp_sl()

from backtester import Backtester


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
        sl.validate_sl_after_rule(r)


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
    assert rules.per_tier[0].is_empty()
    assert rules.per_tier[1].kind == "atr_offset"
    assert rules.per_tier[1].atr_mult == 0.25
    assert rules.has_any()
    assert rules.for_tier(0).kind == "breakeven"
    assert rules.for_tier(1).kind == "atr_offset"
    assert rules.for_tier(99).kind == "breakeven"


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
            {"atr_multiple": 3, "close_fraction": 0.9},
            {"atr_multiple": 1, "close_fraction": 0.25},
            {"atr_multiple": 2, "close_fraction": 0.5},
        ]},
    }]
    fractions = sl.parse_tp_tier_close_fractions(refs)
    assert fractions == [0.25, 0.5, 1.0]


def test_find_highest_cleared_tier_basic():
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.0) == -1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.5) == 0
    assert sl.find_highest_cleared_tier([0.5, 1.0], 1.0) == 1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 0.5, from_idx=1) == -1
    assert sl.find_highest_cleared_tier([0.5, 1.0], 1.0, from_idx=1) == 1


def _df_open_then_hold(opens, closes, atrs=None, open_actions=None):
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    if open_actions is None:
        open_actions = ["long"] + ["none"] * (n - 1)
    data = {"open": opens, "close": closes, "open_action": open_actions}
    if atrs is not None:
        data["atr"] = atrs
    return pd.DataFrame(data, index=idx)


def test_backtester_breakeven_after_tp1_long():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 100, 95],
        closes=[100, 100, 110, 110, 95, 95],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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


def test_backtester_breakeven_after_tp1_short():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 100, 110],
        closes=[100, 100, 90, 90, 110, 110],
        atrs=[10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 105, 104],
        closes=[100, 100, 110, 110, 104, 104],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert ("long", 104.0) in sides_prices, sides_prices


def test_backtester_trail_from_here_long_walks_up():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 115, 118, 107],
        closes=[100, 100, 110, 115, 118, 107, 107],
        atrs=[10, 10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert ("long", 110.0) in sides_prices, sides_prices
    assert ("long", 107.0) in sides_prices, sides_prices


def test_backtester_tp_atr_fraction_uses_firing_tier_multiple():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 120, 125, 128, 117],
        closes=[100, 100, 120, 125, 128, 117, 117],
        atrs=[10, 10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 85, 82, 93],
        closes=[100, 100, 90, 85, 82, 93, 93],
        atrs=[10, 10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 6,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    assert ("long", 110.0) in sides_prices
    assert ("long", 90.0) in sides_prices


def test_backtester_multi_tier_cleared_same_bar_highest_wins():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 120, 110, 105],
        closes=[100, 100, 120, 120, 105, 105],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert ("long", 120.0) in sides_prices, sides_prices
    assert ("long", 105.0) in sides_prices, sides_prices


def test_backtester_sl_after_idempotent_across_bars():
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
    kwargs = dict(
        side="long",
        avg_cost=100.0,
        entry_atr=10.0,
        position_qty=0.5,
        initial_qty=1.0,
        mark_price=110.0,
        fill_price=110.0,
        sl_trigger_px=90.0,
        sl_tiers_processed=0,
        post_tp_trail_mult=None,
        sl_high_water_px=0.0,
    )
    trig1, processed1, trail1, hwm1 = bt._maybe_apply_sl_after(**kwargs)
    assert trig1 == 105.0
    assert processed1 == 1
    assert trail1 is None

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
    trig, processed, trail, hwm = bt._maybe_apply_sl_after(
        side="long",
        avg_cost=100.0,
        entry_atr=10.0,
        position_qty=0.5,
        initial_qty=1.0,
        mark_price=110.0,
        fill_price=110.0,
        sl_trigger_px=0.0,
        sl_tiers_processed=0,
        post_tp_trail_mult=None,
        sl_high_water_px=0.0,
    )
    assert trig == 0.0
    assert processed == 0
    assert trail is None
    assert hwm == 0.0


def test_backtester_sl_after_no_same_bar_fire_after_bump_long():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 99, 99],
        closes=[100, 100, 110, 99, 99, 99],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert ("long", 99.0) in sides_prices, sides_prices

    sl_closes = [t for t in result["trades"] if t["exit_price"] == 99.0]
    assert len(sl_closes) == 1, sl_closes
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_no_same_bar_fire_after_bump_short():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 90, 101, 101],
        closes=[100, 100, 90, 101, 101, 101],
        atrs=[10, 10, 10, 10, 10, 10],
        open_actions=["short"] + ["none"] * 5,
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_flag_clears_for_next_bar_long():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 110, 105, 95],
        closes=[100, 100, 110, 105, 95, 95],
        atrs=[10, 10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
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
    assert sl_closes[0]["exit_date"] == "2024-01-06 00:00:00", sl_closes[0]


def test_backtester_sl_after_does_not_seed_when_no_tier_thresholds():
    df = _df_open_then_hold(
        opens=[100, 100, 100, 80, 80],
        closes=[100, 100, 80, 80, 80],
        atrs=[10, 10, 10, 10, 10],
    )
    bt = Backtester(
        intrabar_resolution="bar_close",
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": [
                    {"atr_multiple": 1.0, "close_fraction": 0.0},
                    {"atr_multiple": 2.0, "close_fraction": 0.0},
                ],
            },
        }],
    )
    assert bt._tp_tier_thresholds_static == []
    result = bt.run(df, save=False)
    sl_fires = [t for t in result["trades"] if t.get("exit_price") in (90.0, 89.0, 91.0)]
    assert not sl_fires, f"phantom SL fired at {[t['exit_price'] for t in sl_fires]}"
