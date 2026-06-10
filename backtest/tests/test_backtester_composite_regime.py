"""Per-label entry-gate coverage for the composite (7-state) regime vocabulary.

Issue #944 D3.2: ``backtest/tests/`` had ZERO references to the composite
labels (``ranging_quiet``, ``ranging_volatile``, ``ranging_directional``,
``trending_{up,down}_{clean,choppy}``). The ADX (3-state) gate is covered in
test_backtester_regime.py, but the composite labels — the only ones a strategy
can name in ``allowed_regimes`` when ``classifier="composite"`` — were never
exercised through ``Backtester(regime_enabled=True)``.

These tests drive the gate directly off a pre-supplied ``regime`` column (the
same mechanism live uses: ``ensure_regime_columns`` writes the label, the
backtester only gates on it). That keeps the per-label assertions deterministic
instead of hoping synthetic OHLCV lands on a particular composite bucket.

Gate contract (backtester.py): with ``regime_enabled=True`` and a non-empty
``allowed_regimes``, an entry signal is blocked when the bar's (shifted) regime
label is not in ``allowed_regimes``; closes are never gated. Signals shift +1
bar, and the regime column shifts +1 in lockstep, so a column set uniformly to
one label gates every entry on that label.
"""
import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester
from regime import VALID_LABELS_COMPOSITE


# Stable ordering for parametrization / picking a distinct "other" label.
COMPOSITE_LABELS = sorted(VALID_LABELS_COMPOSITE)


def _gated_df(label: str, n: int = 100, buy_at: int = 50) -> pd.DataFrame:
    """Uptrend OHLCV with a single buy signal at ``buy_at`` and the ``regime``
    column pinned uniformly to ``label`` (so the shifted gate reads ``label``
    at the entry bar)."""
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {"open": close, "high": close + 0.5, "low": close - 0.5,
         "close": close, "volume": 1000.0},
        index=idx,
    )
    df["signal"] = 0
    df.iloc[buy_at, df.columns.get_loc("signal")] = 1
    df["regime"] = label
    return df


# ─── Each composite label: matching gate ALLOWS the entry ────────────────────


@pytest.mark.parametrize("label", COMPOSITE_LABELS)
def test_composite_label_allows_entry_when_gate_matches(label):
    df = _gated_df(label)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=[label],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] >= 1, (
        f"Composite label '{label}' in allowed_regimes should permit the entry"
    )


# ─── Each composite label: a DIFFERENT gate BLOCKS the entry ─────────────────


@pytest.mark.parametrize("label", COMPOSITE_LABELS)
def test_composite_label_blocks_entry_when_gate_mismatches(label):
    # Pick any distinct label to gate on — the bar is ``label`` so it must block.
    other = next(l for l in COMPOSITE_LABELS if l != label)
    df = _gated_df(label)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=[other],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 0, (
        f"Bar regime '{label}' must be blocked when only '{other}' is allowed"
    )


# ─── Multi-label allow-list: any member permits the entry ────────────────────


@pytest.mark.parametrize("label", COMPOSITE_LABELS)
def test_composite_label_allowed_within_multi_label_gate(label):
    """A realistic ``allowed_regimes`` names several composite buckets; the bar's
    label being any one of them permits the entry."""
    allow = [label] + [l for l in COMPOSITE_LABELS if l != label][:2]
    df = _gated_df(label)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=allow,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] >= 1


# ─── Trend-family separation: trending_up_clean ≠ trending_up_choppy ──────────


def test_clean_and_choppy_variants_are_distinct_gates():
    """``trending_up_clean`` and ``trending_up_choppy`` are SEPARATE labels —
    a gate naming only the clean variant must block a choppy bar (and vice
    versa). Guards against any prefix/substring matching creeping into the gate.
    """
    df_choppy = _gated_df("trending_up_choppy")
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up_clean"],
    )
    assert bt.run(df_choppy, save=False)["total_trades"] == 0

    df_clean = _gated_df("trending_up_clean")
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up_choppy"],
    )
    assert bt.run(df_clean, save=False)["total_trades"] == 0


# ─── ranging_directional is gated independently of the trending labels ───────


def test_ranging_directional_blocked_by_trending_gate():
    """``ranging_directional`` carries direction in its name but is a RANGING
    bucket — a trending-only gate must still block it."""
    df = _gated_df("ranging_directional")
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True,
        allowed_regimes=["trending_up_clean", "trending_down_clean"],
    )
    assert bt.run(df, save=False)["total_trades"] == 0


# ─── Open position is held across a composite-regime flip (closes not gated) ──


def test_composite_regime_flip_does_not_close_open_position():
    """Once long, a flip from an allowed composite label to a disallowed one
    must NOT force a close — the gate is entry-only."""
    df = _gated_df("trending_up_clean", n=100)
    # Signal at bar 50 → fills at bar 51; keep the allowed label through bar 51,
    # then flip to a disallowed ranging label from bar 52 onward.
    df["regime"] = "trending_up_clean"
    df.iloc[52:, df.columns.get_loc("regime")] = "ranging_volatile"
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up_clean"],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] > 0
