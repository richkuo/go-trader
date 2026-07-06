"""Parser tests for sl_after (#708, #736).

Mirrors the Go tests in scheduler/post_tp_sl_test.go covering the scalar +
trend_regime shapes. The backtester resolver is tested via the runtime
helpers; this file focuses on parse/validate.
"""

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


# --- Scalar shapes still parse identically ---------------------------------


def test_scalar_atr_offset_implicit():
    rule = parse_sl_after_rule({"atr_mult": 0.25})
    assert rule == SLAfterRule(kind="atr_offset", atr_mult=0.25)


def test_scalar_atr_offset_signed():
    rule = parse_sl_after_rule({"atr_mult": -0.5})
    assert rule == SLAfterRule(kind="atr_offset", atr_mult=-0.5)


def test_scalar_trail_from_here():
    rule = parse_sl_after_rule(
        {"trail_from_here": {"atr_mult": 1.0}}
    )
    assert rule == SLAfterRule(kind="trail_from_here", trail_atr_mult=1.0)


def test_scalar_trail_from_here_tp_atr_fraction():
    rule = parse_sl_after_rule(
        {"trail_from_here": {"tp_atr_fraction": 0.5}}
    )
    assert rule.kind == "trail_from_here"


def test_scalar_breakeven_string():
    assert parse_sl_after_rule("breakeven") == SLAfterRule(kind="breakeven")


# --- Regime shape (#736) ---------------------------------------------------


def test_regime_atr_offset_implicit():
    rule = parse_sl_after_rule(
        _regime({"trending_up": 0.0, "trending_down": 0.0, "ranging": -0.5})
    )
    assert rule.kind == "atr_offset"
    assert rule.atr_mult == 0.0
    assert rule.atr_regime is not None
    # Signed atr values must round-trip — sl_after.atr_offset is the one
    # surface where 0 and negative atrs are legal.
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


def test_regime_rejects_bare_label_keys():
    # Operator forgot the trend_regime wrapper.
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule({"trending_up": {"atr_multiple": 0.25}})
    assert "trend_regime" in str(exc.value) or "object must contain" in str(exc.value)


def test_regime_rejects_missing_labels():
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "trend_regime": {
                    "trending_up": {"atr_multiple": 0.25},
                    "ranging": {"atr_multiple": 0.0},
                }
            }
        )
    assert "missing required regime labels" in str(exc.value)


def test_regime_rejects_use_defaults_with_explicit():
    with pytest.raises(ValueError):
        parse_sl_after_rule(
            {
                "use_defaults": True,
                "trend_regime": {
                    "trending_up": {"atr_multiple": 0.25},
                    "trending_down": {"atr_multiple": 0.25},
                    "ranging": {"atr_multiple": 0.0},
                },
            }
        )


def test_regime_rejects_scalar_and_regime_mix():
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "atr_mult": 0.25,
                "trend_regime": {
                    "trending_up": {"atr_multiple": 0.25},
                    "trending_down": {"atr_multiple": 0.25},
                    "ranging": {"atr_multiple": 0.0},
                },
            }
        )
    assert "pick one shape" in str(exc.value)


def test_atr_offset_regime_rejects_stray_trail_atr_mult():
    # Misplaced trail_atr_mult on an atr_offset regime config — pre-review
    # the parser silently dropped it.
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "kind": "atr_offset",
                "trend_regime": {
                    "trending_up": {"atr_multiple": 0.25},
                    "trending_down": {"atr_multiple": 0.25},
                    "ranging": {"atr_multiple": 0.0},
                },
                "trail_atr_mult": 99.0,
            }
        )
    assert "pick one shape" in str(exc.value)


def test_trail_regime_rejects_stray_atr_offset():
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "trail_from_here": {
                    "trend_regime": {
                        "trending_up": {"atr_multiple": 1.0},
                        "trending_down": {"atr_multiple": 1.0},
                        "ranging": {"atr_multiple": 0.5},
                    },
                    "atr_offset": -3.0,
                }
            }
        )
    assert "pick one shape" in str(exc.value)


# --- Equality / resolve helpers --------------------------------------------


def test_resolve_for_regime_atr_offset():
    rule = parse_sl_after_rule(
        _regime({"trending_up": 0.0, "trending_down": 0.0, "ranging": -0.5})
    )
    resolved = rule.resolve_for_regime("ranging")
    assert resolved is not None
    assert resolved.kind == "atr_offset"
    assert resolved.atr_mult == -0.5
    # Resolved rule drops the regime block — purely scalar.
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
    assert resolved is rule  # scalar form unchanged


# --- Strategy-level parse round trip ---------------------------------------


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
    # #870: trending_up_clean → clean group (4 cumulative fractions).
    assert got == [0.25, 0.5, 0.75, 1.0]


# --- Manual rejection: trail_from_here regime variant ----------------------


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


# --- validate_sl_after_rule guards -----------------------------------------


def test_validate_breakeven_rejects_regime_block():
    rule = SLAfterRule(kind="breakeven", atr_regime=object())  # type: ignore[arg-type]
    with pytest.raises(ValueError):
        validate_sl_after_rule(rule)


def test_parse_strategy_tp_sl_after_rules_regime_close_per_tier_sl_after():
    # #1228 parity: a per-tier sl_after on a tiered_tp_atr_regime close loads
    # in Go but errored in the Python mirror (parse_regime_tp_tiers rejected
    # the sl_after sibling key), so the rule silently never armed at fire
    # time. The regime-resolved parse must succeed and align the rule.
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
