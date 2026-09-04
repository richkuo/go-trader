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


@pytest.mark.parametrize("raw,kind,attr,value", [
    ("breakeven", "breakeven", None, None),
    ({"atr_mult": 0.25}, "atr_offset", "atr_mult", 0.25),
    ({"atr_mult": -0.5}, "atr_offset", "atr_mult", -0.5),
    ({"kind": "atr_offset", "atr_mult": 0.25}, "atr_offset", "atr_mult", 0.25),
    ({"trail_from_here": {"atr_mult": 1.0}}, "trail_from_here",
     "trail_atr_mult", 1.0),
    ({"kind": "trail_from_here", "atr_mult": 1.5}, "trail_from_here",
     "trail_atr_mult", 1.5),
])
def test_parse_sl_after_rule_accepts(raw, kind, attr, value):
    rule = sl.parse_sl_after_rule(raw)
    assert rule.kind == kind
    assert rule.is_empty() is False
    if attr is not None:
        assert getattr(rule, attr) == value


@pytest.mark.parametrize("raw", [None, ""])
def test_parse_sl_after_rule_empty_inputs(raw):
    assert sl.parse_sl_after_rule(raw).is_empty()


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


@pytest.mark.parametrize("rule,side,avg,atr,mark,want_ok,want_px,want_mode", [
    (sl.SLAfterRule(kind="breakeven"), "long", 100, 5, 0, True, 100, "breakeven"),
    (sl.SLAfterRule(kind="breakeven"), "short", 200, 5, 0, True, 200, "breakeven"),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=0.25), "long", 100, 5, 0,
     True, 100 + 0.25 * 5, "atr+0.25"),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=-0.5), "long", 100, 5, 0,
     True, 100 - 0.5 * 5, "atr-0.5"),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=0), "long", 100, 5, 0,
     True, 100, "atr+0"),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=1), "long", 100, 5, 0,
     True, 100 + 1 * 5, "atr+1"),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=0.25), "short", 100, 5, 0,
     True, 100 - 0.25 * 5, None),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=-0.5), "short", 100, 5, 0,
     True, 100 + 0.5 * 5, None),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1.0), "long", 100, 5, 110,
     True, 110 - 1.0 * 5, "trail 1×ATR"),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1.5), "short", 100, 5, 90,
     True, 90 + 1.5 * 5, None),
    (sl.SLAfterRule(), "long", 100, 5, 0, False, None, None),
    (sl.SLAfterRule(kind="breakeven"), "neutral", 100, 5, 0, False, None, None),
    (sl.SLAfterRule(kind="breakeven"), "long", 0, 5, 0, False, None, None),
    (sl.SLAfterRule(kind="atr_offset", atr_mult=0.25), "long", 100, 0, 0,
     False, None, None),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1), "long", 100, 0, 110,
     False, None, None),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=1), "long", 100, 5, 0,
     False, None, None),
    (sl.SLAfterRule(kind="trail_from_here", trail_atr_mult=0), "long", 100, 5, 110,
     False, None, None),
    (sl.SLAfterRule(kind="weird"), "long", 100, 5, 110, False, None, None),
])
def test_compute_post_tp_stop_loss_trigger(
        rule, side, avg, atr, mark, want_ok, want_px, want_mode):
    px, mode, ok = sl.compute_post_tp_stop_loss_trigger(rule, side, avg, atr, mark)
    assert bool(ok) is want_ok
    if not want_ok:
        return
    assert abs(px - want_px) < 1e-9
    if want_mode is not None:
        assert mode == want_mode


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


_TWO_TIERS = [
    {"atr_multiple": 2, "close_fraction": 0.5},
    {"atr_multiple": 3, "close_fraction": 1.0},
]


@pytest.mark.parametrize("refs,kwargs,want_errs", [
    (
        [{"name": "tiered_tp_atr_live",
          "params": {"sl_after": "breakeven", "tp_tiers": _TWO_TIERS}}],
        {"stop_loss_atr_mult": 1.0, "trailing_stop_atr_mult": 1.5},
        [("trailing_stop",)],
    ),
    (
        [{"name": "tiered_tp_atr_live",
          "params": {"sl_after": "breakeven", "tp_tiers": _TWO_TIERS}}],
        {},
        [("fixed stop-loss",)],
    ),
    (
        [{"name": "tiered_tp_atr_live",
          "params": {
              "sl_after": "breakeven",
              "tp_tiers": [
                  {"atr_multiple": 2, "close_fraction": 0.5},
                  {"atr_multiple": 3, "close_fraction": 1.0,
                   "sl_after": {"atr_mult": 0.5}},
              ],
          }}],
        {"stop_loss_atr_mult": 1.0},
        [],
    ),
    (
        [{"name": "tiered_tp_atr_live",
          "params": {"sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                     "tp_tiers": _TWO_TIERS}}],
        {"stop_loss_atr_mult": 1.5, "strategy_type": "manual"},
        [("trail_from_here is not supported on manual",)],
    ),
    (
        [{"name": "tp_at_pct", "params": {"pct": 0.05, "sl_after": "breakeven"}}],
        {"stop_loss_atr_mult": 1.0},
        [("only honored on tiered_tp_atr",)],
    ),
    (
        [{"name": "tiered_tp_pct",
          "params": {"tp_tiers": [
              {"pct": 0.05, "close_fraction": 0.5, "sl_after": "breakeven"},
          ]}}],
        {"stop_loss_atr_mult": 1.0},
        [("no effect", "tiered_tp_pct")],
    ),
    (
        [{"name": "tiered_tp_atr_live", "params": {"tp_tiers": _TWO_TIERS}}],
        {"stop_loss_atr_mult": 1.0},
        [],
    ),
])
def test_validate_post_tp_stop_loss_rules(refs, kwargs, want_errs):
    errs = sl.validate_post_tp_stop_loss_rules(refs, **kwargs)
    if not want_errs:
        assert errs == []
        return
    for parts in want_errs:
        assert any(all(p in e for p in parts) for e in errs), errs


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


_STANDARD_TIERS = [
    {"atr_multiple": 1.0, "close_fraction": 0.5},
    {"atr_multiple": 2.0, "close_fraction": 1.0},
]

_SCENARIOS = {
    "breakeven_after_tp1_long": dict(
        opens=[100, 100, 100, 110, 100, 95],
        closes=[100, 100, 110, 110, 95, 95],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": "breakeven", "tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 95.0)],
    ),
    "breakeven_after_tp1_short": dict(
        opens=[100, 100, 100, 90, 100, 110],
        closes=[100, 100, 90, 90, 110, 110],
        side="short",
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": "breakeven", "tp_tiers": _STANDARD_TIERS},
        expect=[("short", 90.0), ("short", 110.0)],
    ),
    "atr_offset_after_tp1_long": dict(
        opens=[100, 100, 100, 110, 105, 104],
        closes=[100, 100, 110, 110, 104, 104],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": {"atr_mult": 0.5}, "tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 104.0)],
    ),
    "trail_from_here_long_walks_up": dict(
        opens=[100, 100, 100, 110, 115, 118, 107],
        closes=[100, 100, 110, 115, 118, 107, 107],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                "tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 107.0)],
    ),
    "trail_from_here_short_walks_down": dict(
        opens=[100, 100, 100, 90, 85, 82, 93],
        closes=[100, 100, 90, 85, 82, 93, 93],
        side="short",
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                "tp_tiers": _STANDARD_TIERS},
        expect=[("short", 90.0), ("short", 93.0)],
    ),
    "tp_atr_fraction_uses_firing_tier_multiple": dict(
        opens=[100, 100, 100, 120, 125, 128, 117],
        closes=[100, 100, 120, 125, 128, 117, 117],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": {"trail_from_here": {"tp_atr_fraction": 0.5}},
                "tp_tiers": [
                    {"atr_multiple": 2.0, "close_fraction": 0.5},
                    {"atr_multiple": 4.0, "close_fraction": 1.0},
                ]},
        expect=[("long", 120.0), ("long", 117.0)],
    ),
    "tp_atr_fraction_uses_default_tier_multiple": dict(
        opens=[100, 100, 100, 110, 115, 118, 112],
        closes=[100, 100, 110, 115, 118, 112, 112],
        stop_loss_atr_mult=1.0,
        params={"sl_after": {"trail_from_here": {"tp_atr_fraction": 0.5}}},
        expect=[("long", 115.0), ("long", 112.0)],
    ),
    "no_sl_after_unchanged_behavior": dict(
        opens=[100, 100, 100, 110, 90],
        closes=[100, 100, 110, 90, 90],
        params={"tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 90.0)],
    ),
    "multi_tier_cleared_same_bar_highest_wins": dict(
        opens=[100, 100, 100, 120, 110, 105],
        closes=[100, 100, 120, 120, 105, 105],
        intrabar="bar_close",
        stop_loss_atr_mult=2.0,
        params={"tp_tiers": [
            {"atr_multiple": 1.0, "close_fraction": 0.3, "sl_after": "breakeven"},
            {"atr_multiple": 2.0, "close_fraction": 0.6,
             "sl_after": {"atr_mult": 1.0}},
            {"atr_multiple": 3.0, "close_fraction": 1.0,
             "sl_after": {"atr_mult": 2.0}},
        ]},
        expect=[("long", 120.0), ("long", 105.0)],
    ),
    "no_same_bar_fire_after_bump_long": dict(
        opens=[100, 100, 100, 110, 99, 99],
        closes=[100, 100, 110, 99, 99, 99],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": "breakeven", "tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 99.0)],
        single_sl=(99.0, "2024-01-06 00:00:00"),
    ),
    "no_same_bar_fire_after_bump_short": dict(
        opens=[100, 100, 100, 90, 101, 101],
        closes=[100, 100, 90, 101, 101, 101],
        side="short",
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": "breakeven", "tp_tiers": _STANDARD_TIERS},
        expect=[("short", 90.0), ("short", 101.0)],
        single_sl=(101.0, "2024-01-06 00:00:00"),
    ),
    "flag_clears_for_next_bar_long": dict(
        opens=[100, 100, 100, 110, 105, 95],
        closes=[100, 100, 110, 105, 95, 95],
        intrabar="bar_close",
        stop_loss_atr_mult=1.0,
        params={"sl_after": "breakeven", "tp_tiers": _STANDARD_TIERS},
        expect=[("long", 110.0), ("long", 95.0)],
        single_sl=(95.0, "2024-01-06 00:00:00"),
    ),
}


@pytest.mark.parametrize("name", list(_SCENARIOS))
def test_backtester_sl_after_scenarios(name):
    spec = _SCENARIOS[name]
    n = len(spec["closes"])
    side = spec.get("side", "long")
    df = _df_open_then_hold(
        opens=spec["opens"],
        closes=spec["closes"],
        atrs=[10] * n,
        open_actions=[side] + ["none"] * (n - 1),
    )
    kwargs = dict(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        close_strategies=[{"name": "tiered_tp_atr", "params": spec["params"]}],
    )
    if "intrabar" in spec:
        kwargs["intrabar_resolution"] = spec["intrabar"]
    if "stop_loss_atr_mult" in spec:
        kwargs["stop_loss_atr_mult"] = spec["stop_loss_atr_mult"]
    result = Backtester(**kwargs).run(df, save=False)
    sides_prices = [(t["side"], t["exit_price"]) for t in result["trades"]]
    for pair in spec["expect"]:
        assert pair in sides_prices, sides_prices
    if "single_sl" in spec:
        price, exit_date = spec["single_sl"]
        sl_closes = [t for t in result["trades"] if t["exit_price"] == price]
        assert len(sl_closes) == 1, sl_closes
        assert sl_closes[0]["exit_date"] == exit_date, sl_closes[0]


_REGIME_STOP_BLOCK = {
    "trend_regime": {"trending_up": {"atr_multiple": 0.25},
                     "trending_down": {"atr_multiple": 0.25},
                     "ranging": {"atr_multiple": 0.0}},
}

_REGIME_TP_ATR_FRACTION = {
    "trend_regime": {"trending_up": 0.75, "trending_down": 0.75, "ranging": 0.5},
}


@pytest.mark.parametrize("kwargs,match", [
    (
        dict(close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {"sl_after": "breakeven", "tp_tiers": _TWO_TIERS}}]),
        "fixed stop-loss",
    ),
    (
        dict(stop_loss_atr_mult=1.0, trailing_stop_atr_mult=1.5,
             close_strategies=[{
                 "name": "tiered_tp_atr",
                 "params": {"sl_after": "breakeven", "tp_tiers": _TWO_TIERS}}]),
        "trailing_stop",
    ),
    (
        dict(strategy_type="manual", stop_loss_atr_mult=1.5,
             close_strategies=[{
                 "name": "tiered_tp_atr_live",
                 "params": {"sl_after": {"trail_from_here": {"atr_mult": 1.0}},
                            "tp_tiers": _TWO_TIERS}}]),
        "trail_from_here is not supported on manual",
    ),
    (
        dict(stop_loss_atr_mult=1.0,
             close_strategies=[{
                 "name": "tp_at_pct",
                 "params": {"pct": 0.05, "sl_after": "breakeven"}}]),
        "only honored on tiered_tp_atr",
    ),
    (
        dict(stop_loss_atr_mult=1.0,
             close_strategies=[{
                 "name": "tiered_tp_atr",
                 "params": {"sl_after": _REGIME_STOP_BLOCK,
                            "tp_tiers": _STANDARD_TIERS}}]),
        "HL-live-only",
    ),
    (
        dict(stop_loss_atr_mult=1.0,
             close_strategies=[{
                 "name": "tiered_tp_atr",
                 "params": {"tp_tiers": [
                     {"atr_multiple": 1.0, "close_fraction": 0.5,
                      "sl_after": {"trail_from_here": {
                          "trend_regime": {
                              "trending_up": {"atr_multiple": 1.0},
                              "trending_down": {"atr_multiple": 1.0},
                              "ranging": {"atr_multiple": 0.5}}}}},
                     {"atr_multiple": 2.0, "close_fraction": 1.0},
                 ]}}]),
        "HL-live-only",
    ),
    (
        dict(stop_loss_atr_mult=1.0,
             close_strategies=[{
                 "name": "tiered_tp_atr",
                 "params": {
                     "sl_after": {"trail_from_here": {
                         "tp_atr_fraction": _REGIME_TP_ATR_FRACTION}},
                     "tp_tiers": [
                         {"atr_multiple": 2.0, "close_fraction": 0.5},
                         {"atr_multiple": 4.0, "close_fraction": 1.0},
                     ]}}]),
        "HL-live-only",
    ),
    (
        dict(stop_loss_atr_mult=1.0,
             close_strategies=[{
                 "name": "tiered_tp_atr_regime",
                 "params": {"tp_tiers": [
                     {
                         "trend_regime": {"trending_up": {"atr_multiple": 2.0},
                                          "trending_down": {"atr_multiple": 2.0},
                                          "ranging": {"atr_multiple": 1.5}},
                         "close_fraction": 0.5,
                         "sl_after": {"trail_from_here": {
                             "tp_atr_fraction": _REGIME_TP_ATR_FRACTION}},
                     },
                     {
                         "trend_regime": {"trending_up": {"atr_multiple": 4.0},
                                          "trending_down": {"atr_multiple": 4.0},
                                          "ranging": {"atr_multiple": 3.0}},
                         "close_fraction": 1.0,
                     },
                 ]}}]),
        "HL-live-only",
    ),
    (
        dict(stop_loss_margin_pct=0.5,
             close_strategies=[{
                 "name": "tiered_tp_atr",
                 "params": {"sl_after": "breakeven",
                            "tp_tiers": _STANDARD_TIERS}}]),
        "stop_loss_margin_pct",
    ),
])
def test_backtester_validation_rejects(kwargs, match):
    with pytest.raises(ValueError, match=match):
        Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            platform="hyperliquid",
            **dict({"strategy_type": "perps"}, **kwargs),
        )


def test_backtester_sl_after_idempotent_across_bars():
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": {"atr_mult": 0.5},
                "tp_tiers": _STANDARD_TIERS,
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


def test_backtester_sl_after_defers_when_sl_unarmed():
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        platform="hyperliquid", strategy_type="perps",
        stop_loss_atr_mult=1.0,
        close_strategies=[{
            "name": "tiered_tp_atr",
            "params": {
                "sl_after": "breakeven",
                "tp_tiers": _STANDARD_TIERS,
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
