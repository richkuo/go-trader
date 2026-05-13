"""Parser tests for sl_after (#708, #736).

Mirrors the Go tests in scheduler/post_tp_sl_test.go covering the scalar +
trend_regime shapes. The backtester resolver is tested via the runtime
helpers; this file focuses on parse/validate.
"""

from __future__ import annotations

import pytest

from .post_tp_sl import (
    SLAfterRule,
    parse_sl_after_rule,
    parse_strategy_tp_sl_after_rules,
    validate_post_tp_stop_loss_rules,
    validate_sl_after_rule,
)


def _regime(entries):
    return {
        "trend_regime": {
            label: {"atr": atr} for label, atr in entries.items()
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
        parse_sl_after_rule({"trending_up": {"atr": 0.25}})
    assert "trend_regime" in str(exc.value) or "object must contain" in str(exc.value)


def test_regime_rejects_missing_labels():
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "trend_regime": {
                    "trending_up": {"atr": 0.25},
                    "ranging": {"atr": 0.0},
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
                    "trending_up": {"atr": 0.25},
                    "trending_down": {"atr": 0.25},
                    "ranging": {"atr": 0.0},
                },
            }
        )


def test_regime_rejects_scalar_and_regime_mix():
    with pytest.raises(ValueError) as exc:
        parse_sl_after_rule(
            {
                "atr_mult": 0.25,
                "trend_regime": {
                    "trending_up": {"atr": 0.25},
                    "trending_down": {"atr": 0.25},
                    "ranging": {"atr": 0.0},
                },
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
                "tiers": [
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
                "tiers": [
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
