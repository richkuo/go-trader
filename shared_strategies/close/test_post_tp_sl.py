
from __future__ import annotations

import pytest

from .post_tp_sl import (
    SLAfterRule,
    parse_tp_tier_close_fractions,
    parse_sl_after_rule,
    parse_strategy_tp_sl_after_rules,
    validate_post_tp_stop_loss_rules,
    validate_sl_after_rule,
)


def _regime(entries):
    return {
        "trend_regime": {
            label: {"atr_multiple": atr} for label, atr in entries.items()
        }
    }


@pytest.mark.parametrize("raw,expected", [
    ({"atr_mult": 0.25}, SLAfterRule(kind="atr_offset", atr_mult=0.25)),
    ({"atr_mult": -0.5}, SLAfterRule(kind="atr_offset", atr_mult=-0.5)),
    ({"trail_from_here": {"atr_mult": 1.0}},
     SLAfterRule(kind="trail_from_here", trail_atr_mult=1.0)),
    ("breakeven", SLAfterRule(kind="breakeven")),
])
def test_scalar_shapes_parse(raw, expected):
    assert parse_sl_after_rule(raw) == expected


def test_scalar_trail_from_here_tp_atr_fraction():
    rule = parse_sl_after_rule(
        {"trail_from_here": {"tp_atr_fraction": 0.5}}
    )
    assert rule.kind == "trail_from_here"


def test_regime_atr_offset_implicit():
    rule = parse_sl_after_rule(
        _regime({"trending_up": 0.0, "trending_down": 0.0, "ranging": -0.5})
    )
    assert rule.kind == "atr_offset"
    assert rule.atr_mult == 0.0
    assert rule.atr_regime is not None
    entry = rule.atr_regime.resolve("ranging")
    assert entry is not None and entry.atr == -0.5
    entry_up = rule.atr_regime.resolve("trending_up")
    assert entry_up is not None and entry_up.atr == 0.0


def test_regime_atr_offset_explicit_kind():
    rule = parse_sl_after_rule(
        {
            "kind": "atr_offset",
            **_regime(
                {"trending_up": 0.25, "trending_down": 0.25, "ranging": 0.0}
            ),
        }
    )
    assert rule.kind == "atr_offset"
    assert rule.atr_regime is not None


def test_regime_trail_from_here():
    rule = parse_sl_after_rule(
        {
            "trail_from_here": _regime(
                {"trending_up": 1.0, "trending_down": 1.0, "ranging": 0.5}
            )
        }
    )
    assert rule.kind == "trail_from_here"
    assert rule.trail_atr_regime is not None
    assert rule.trail_atr_mult == 0.0


@pytest.mark.parametrize("atr", [0.0, -1.0])
def test_regime_trail_rejects_non_positive(atr):
    with pytest.raises(ValueError):
        parse_sl_after_rule(
            {
                "trail_from_here": _regime(
                    {
                        "trending_up": 1.0,
                        "trending_down": 1.0,
                        "ranging": atr,
                    }
                )
            }
        )


_FULL_TREND_REGIME = {
    "trending_up": {"atr_multiple": 0.25},
    "trending_down": {"atr_multiple": 0.25},
    "ranging": {"atr_multiple": 0.0},
}

_FULL_TRAIL_REGIME = {
    "trending_up": {"atr_multiple": 1.0},
    "trending_down": {"atr_multiple": 1.0},
    "ranging": {"atr_multiple": 0.5},
}


@pytest.mark.parametrize("raw,substrings", [
    ({"trending_up": {"atr_multiple": 0.25}}, ("trend_regime", "object must contain")),
    ({"trend_regime": {"trending_up": {"atr_multiple": 0.25},
                       "ranging": {"atr_multiple": 0.0}}},
     ("missing required regime labels",)),
    ({"use_defaults": True, "trend_regime": dict(_FULL_TREND_REGIME)}, ()),
    ({"atr_mult": 0.25, "trend_regime": dict(_FULL_TREND_REGIME)}, ("pick one shape",)),
    ({"kind": "atr_offset", "trend_regime": dict(_FULL_TREND_REGIME),
      "trail_atr_mult": 99.0}, ("pick one shape",)),
    ({"trail_from_here": {"trend_regime": dict(_FULL_TRAIL_REGIME),
                          "atr_offset": -3.0}}, ("pick one shape",)),
])
def test_regime_shape_rejections(raw, substrings):
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(raw)
    if substrings:
        assert any(sub in str(exc.value) for sub in substrings), exc.value


def test_resolve_for_regime_atr_offset():
    rule = parse_sl_after_rule(
        _regime({"trending_up": 0.0, "trending_down": 0.0, "ranging": -0.5})
    )
    resolved = rule.resolve_for_regime("ranging")
    assert resolved is not None
    assert resolved.kind == "atr_offset"
    assert resolved.atr_mult == -0.5
    assert resolved.atr_regime is None


def test_resolve_for_regime_trail_from_here():
    rule = parse_sl_after_rule(
        {
            "trail_from_here": _regime(
                {"trending_up": 1.0, "trending_down": 1.0, "ranging": 0.5}
            )
        }
    )
    resolved = rule.resolve_for_regime("ranging")
    assert resolved is not None
    assert resolved.kind == "trail_from_here"
    assert resolved.trail_atr_mult == 0.5


def test_resolve_for_regime_unknown_label_defers():
    rule = parse_sl_after_rule(
        _regime({"trending_up": 0.0, "trending_down": 0.0, "ranging": -0.5})
    )
    assert rule.resolve_for_regime("never") is None
    assert rule.resolve_for_regime("") is None


def test_resolve_scalar_pass_through():
    rule = SLAfterRule(kind="atr_offset", atr_mult=0.25)
    resolved = rule.resolve_for_regime("trending_up")
    assert resolved is rule


def test_parse_strategy_tp_sl_after_rules_regime():
    close_refs = [
        {
            "name": "tiered_tp_atr_live",
            "params": {
                "sl_after": _regime(
                    {
                        "trending_up": 0.0,
                        "trending_down": 0.0,
                        "ranging": -0.5,
                    }
                ),
                "tp_tiers": [
                    {
                        "atr_multiple": 2,
                        "close_fraction": 0.5,
                        "sl_after": {
                            "trail_from_here": _regime(
                                {
                                    "trending_up": 1.0,
                                    "trending_down": 1.0,
                                    "ranging": 0.5,
                                }
                            )
                        },
                    },
                    {"atr_multiple": 3, "close_fraction": 1.0},
                ],
            },
        }
    ]
    rules, errs = parse_strategy_tp_sl_after_rules(close_refs)
    assert errs == []
    assert rules.default.kind == "atr_offset"
    assert rules.default.atr_regime is not None
    assert len(rules.per_tier) == 2
    assert rules.per_tier[0].kind == "trail_from_here"
    assert rules.per_tier[0].trail_atr_regime is not None


def test_parse_strategy_tp_sl_after_rules_regime_composite_labels():
    labels = (
        "ranging_directional",
        "ranging_quiet",
        "ranging_volatile",
        "trending_down_choppy",
        "trending_down_clean",
        "trending_up_choppy",
        "trending_up_clean",
    )
    close_refs = [{
        "name": "tiered_tp_atr_regime",
        "params": {
            "tp_tiers": [
                {
                    "trend_regime": {label: {"atr_multiple": 2.0} for label in labels},
                    "close_fraction": 0.5,
                },
                {
                    "trend_regime": {label: {"atr_multiple": 4.0} for label in labels},
                    "close_fraction": 1.0,
                },
            ],
        },
    }]
    rules, errs = parse_strategy_tp_sl_after_rules(
        close_refs,
        regime="trending_up_clean",
        labels=labels,
    )
    assert errs == []
    assert rules.multiples == [2.0, 4.0]


def test_parse_tp_tier_close_fractions_use_defaults_composite_label():
    close_refs = [{
        "name": "tiered_tp_atr_regime",
        "params": {"use_defaults": True},
    }]
    got = parse_tp_tier_close_fractions(
        close_refs,
        regime="trending_up_clean",
    )
    assert got == [0.25, 0.5, 0.75, 1.0]


def test_validate_rejects_trail_regime_on_manual():
    close_refs = [
        {
            "name": "tiered_tp_atr_live",
            "params": {
                "sl_after": {
                    "trail_from_here": _regime(
                        {
                            "trending_up": 1.0,
                            "trending_down": 1.0,
                            "ranging": 0.5,
                        }
                    )
                },
                "tp_tiers": [
                    {"atr_multiple": 2, "close_fraction": 0.5},
                    {"atr_multiple": 3, "close_fraction": 1.0},
                ],
            },
        }
    ]
    errs = validate_post_tp_stop_loss_rules(
        close_refs,
        stop_loss_atr_mult=1.5,
        strategy_type="manual",
    )
    assert any(
        "trail_from_here is not supported on manual" in e for e in errs
    ), errs


def test_validate_breakeven_rejects_regime_block():
    rule = SLAfterRule(kind="breakeven", atr_regime=object())
    with pytest.raises(ValueError):
        validate_sl_after_rule(rule)


def test_parse_strategy_tp_sl_after_rules_regime_close_per_tier_sl_after():
    close_refs = [
        {
            "name": "tiered_tp_atr_regime",
            "params": {
                "tp_tiers": [
                    {
                        "trend_regime": {
                            "trending_up": {"atr_multiple": 2.0, "close_fraction": 0.5},
                            "trending_down": {"atr_multiple": 2.0, "close_fraction": 0.5},
                            "ranging": {"atr_multiple": 1.5, "close_fraction": 0.5},
                        },
                        "sl_after": "breakeven",
                    },
                    {
                        "trend_regime": {
                            "trending_up": {"atr_multiple": 4.0, "close_fraction": 1.0},
                            "trending_down": {"atr_multiple": 4.0, "close_fraction": 1.0},
                            "ranging": {"atr_multiple": 3.0, "close_fraction": 1.0},
                        },
                    },
                ],
            },
        }
    ]
    rules, errs = parse_strategy_tp_sl_after_rules(close_refs, regime="ranging")
    assert errs == [], errs
    assert len(rules.per_tier) == 2
    assert rules.per_tier[0].kind == "breakeven"
    assert rules.per_tier[1].kind == ""
