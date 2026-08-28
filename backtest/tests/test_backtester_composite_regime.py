import sys
import pathlib

import numpy as np
import pandas as pd
import pytest

sys.path.insert(0, str(pathlib.Path(__file__).parent.parent.parent / "shared_tools"))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent))

from backtester import Backtester
from regime import VALID_LABELS_COMPOSITE


COMPOSITE_LABELS = sorted(VALID_LABELS_COMPOSITE)


def _gated_df(label: str, n: int = 100, buy_at: int = 50) -> pd.DataFrame:
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


@pytest.mark.parametrize("label", COMPOSITE_LABELS)
def test_composite_label_blocks_entry_when_gate_mismatches(label):
    covered_by = {"ranging_directional_up", "ranging_directional_down"}
    if label in covered_by:
        other = next(l for l in COMPOSITE_LABELS if l != label and l != "ranging_directional")
    else:
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


@pytest.mark.parametrize("label", COMPOSITE_LABELS)
def test_composite_label_allowed_within_multi_label_gate(label):
    allow = [label] + [l for l in COMPOSITE_LABELS if l != label][:2]
    df = _gated_df(label)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=allow,
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] >= 1


def test_clean_and_choppy_variants_are_distinct_gates():
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


def test_ranging_directional_blocked_by_trending_gate():
    df = _gated_df("ranging_directional")
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True,
        allowed_regimes=["trending_up_clean", "trending_down_clean"],
    )
    assert bt.run(df, save=False)["total_trades"] == 0


def test_composite_regime_flip_does_not_close_open_position():
    df = _gated_df("trending_up_clean", n=100)
    df["regime"] = "trending_up_clean"
    df.iloc[52:, df.columns.get_loc("regime")] = "ranging_volatile"
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["trending_up_clean"],
    )
    result = bt.run(df, save=False)
    assert result["total_trades"] == 1
    assert result["trades"][0]["exit_price"] > 0


@pytest.mark.parametrize("sub", ["ranging_directional_up", "ranging_directional_down"])
def test_bare_ranging_directional_covers_sub_label_entry(sub):
    df = _gated_df(sub)
    bt = Backtester(
        initial_capital=1000, commission_pct=0, slippage_pct=0,
        regime_enabled=True, allowed_regimes=["ranging_directional"],
    )
    assert bt.run(df, save=False)["total_trades"] >= 1


def test_explicit_sub_label_does_not_cover_bare_or_sibling_entry():
    for bar in ["ranging_directional", "ranging_directional_down"]:
        df = _gated_df(bar)
        bt = Backtester(
            initial_capital=1000, commission_pct=0, slippage_pct=0,
            regime_enabled=True, allowed_regimes=["ranging_directional_up"],
        )
        assert bt.run(df, save=False)["total_trades"] == 0, (
            f"explicit _up must NOT cover bar '{bar}'"
        )
