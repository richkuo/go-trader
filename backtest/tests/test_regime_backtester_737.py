"""Backtester parity for regime-aware ATR surfaces (#737)."""

import pytest
import pandas as pd

from backtester import Backtester
from shared_strategies.close import regime_atr


def _tier_spec_widely_separated():
    """Single tier whose ATR multiple differs sharply by regime label."""
    return [
        {
            "trend_regime": {
                "trending_up": {"atr": 10.0, "close_fraction": 1.0},
                "trending_down": {"atr": 10.0, "close_fraction": 1.0},
                "ranging": {"atr": 0.25, "close_fraction": 1.0},
            }
        }
    ]


def test_tiered_tp_atr_regime_frozen_multiplier_ignores_mid_trade_regime_shift():
    """Tier distances stay anchored to the open-time label across regime flips."""
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame(
        {
            "open": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "close": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "atr": [1.0, 1.0, 1.0, 1.0, 1.0, 1.0],
            # Raw labels (``regime_enabled`` is off — stamp + live paths read
            # from ``_regime_bar_close`` captured before any shift).
            "regime": [
                "ranging",
                "ranging",
                "ranging",
                "trending_up",
                "trending_up",
                "trending_up",
            ],
            # Shifted pipeline: raw ``long`` at idx1 fills at idx2 open.
            "open_action": ["none", "long", "none", "none", "none", "none"],
        },
        index=idx,
    )

    close_ref = {
        "name": "tiered_tp_atr_regime",
        "params": {"tiers": _tier_spec_widely_separated()},
    }
    bt = Backtester(
        initial_capital=10_000.0,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=[close_ref],
    )
    result = bt.run(df, save=False)

    # End-of-bar idx3 close hits ranging multiple 0.25×ATR → pending full close
    # at idx4 open (shifted pipeline).
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[4])


def test_tiered_tp_atr_live_regime_re_resolves_multiplier_mid_trade():
    """Live variant moves tier distances with each bar's regime label."""
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame(
        {
            "open": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "close": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "atr": [1.0, 1.0, 1.0, 1.0, 1.0, 1.0],
            "regime": [
                "ranging",
                "ranging",
                "ranging",
                "trending_up",
                "trending_up",
                "trending_up",
            ],
            "open_action": ["none", "long", "none", "none", "none", "none"],
        },
        index=idx,
    )

    close_ref = {
        "name": "tiered_tp_atr_live_regime",
        "params": {"tiers": _tier_spec_widely_separated()},
    }
    bt = Backtester(
        initial_capital=10_000.0,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=[close_ref],
    )
    result = bt.run(df, save=False)

    # No TP partial: the engine flattens at the last bar (still one round-trip).
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_date"] == str(idx[-1])


def test_stop_loss_atr_regime_use_defaults_matches_baseline_table():
    """``use_defaults: true`` expands to the shared REGIME_ATR_DEFAULTS_STOP_LOSS."""
    mult = regime_atr.resolve_regime_atr(
        regime_atr.parse_regime_atr_block(
            {"use_defaults": True}, "probe", regime_atr.SURFACE_STOP_LOSS,
        )[0],
        "ranging",
    )
    assert mult == regime_atr.REGIME_ATR_DEFAULTS_STOP_LOSS["ranging"].atr


def test_stop_loss_atr_regime_seeds_initial_sl_trigger_from_open_stamp():
    """Hand-check: ranging default SL is 1.5×ATR below entry for a long."""
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame(
        {
            "open": [100.0, 100.0, 100.0, 96.0, 96.0, 96.0],
            "close": [100.0, 100.0, 100.0, 96.0, 96.0, 96.0],
            "atr": [1.0, 1.0, 1.0, 1.0, 1.0, 1.0],
            "regime": ["ranging"] * 6,
            "open_action": ["none", "long", "none", "none", "none", "none"],
        },
        index=idx,
    )

    close_ref = {
        "name": "tiered_tp_atr",
        "params": {
            "tiers": [{"atr_multiple": 100.0, "close_fraction": 1.0}],
            # ``sl_after`` arms the bar-level SL machinery (``_initial_sl_trigger``),
            # which is otherwise unused in the open/close engine (#737 test).
            "sl_after": "breakeven",
        },
    }
    bt = Backtester(
        initial_capital=10_000.0,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=[close_ref],
        stop_loss_atr_regime={"use_defaults": True},
    )
    result = bt.run(df, save=False)

    # SL trigger at 100 - 1.5 = 98.5; idx3 close 96 trips SL next open at 96.
    assert result["total_trades"] == 1
    tr = result["trades"][0]
    assert tr["entry_price"] == 100.0
    assert tr["exit_price"] == 96.0


def test_trailing_stop_atr_regime_use_defaults_parses():
    blk, errs = regime_atr.parse_regime_atr_block(
        {"use_defaults": True},
        "trailing_stop_atr_regime",
        regime_atr.SURFACE_TRAILING,
    )
    assert not errs
    mult = regime_atr.resolve_regime_atr(blk, "trending_up")
    assert mult == regime_atr.REGIME_ATR_DEFAULTS_TRAILING["trending_up"].atr


def test_backtester_rejects_mutex_stop_loss_scalar_with_regime_block():
    with pytest.raises(ValueError, match="mutually exclusive"):
        Backtester(
            stop_loss_atr_mult=1.0,
            stop_loss_atr_regime={"use_defaults": True},
        )


def test_backtester_rejects_mutex_trailing_scalar_with_regime_block():
    with pytest.raises(ValueError, match="mutually exclusive"):
        Backtester(
            trailing_stop_atr_mult=1.0,
            trailing_stop_atr_regime={"use_defaults": True},
        )


def test_parse_tp_tier_close_fractions_use_defaults_requires_regime_label():
    from shared_strategies.close import post_tp_sl as sl

    refs = [{"name": "tiered_tp_atr_regime", "params": {"use_defaults": True}}]
    assert sl.parse_tp_tier_close_fractions(refs, regime=None) == []
    fracs = sl.parse_tp_tier_close_fractions(refs, regime="ranging")
    assert fracs == [0.5, 1.0]
