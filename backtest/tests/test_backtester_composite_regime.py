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
    df["regime_ready"] = True
    df["regime_bars"] = np.arange(n) + 29
    df["regime_need"] = 29
    return df


def _gate_cases():
    for label in COMPOSITE_LABELS:
        for multi_label_gate in (False, True):
            allow = [label]
            if multi_label_gate:
                allow += [l for l in COMPOSITE_LABELS if l != label][:2]
            yield f"allows-{label}-multi={multi_label_gate}", label, allow, True
    covered_by = {"ranging_directional_up", "ranging_directional_down"}
    for label in COMPOSITE_LABELS:
        if label in covered_by:
            other = next(l for l in COMPOSITE_LABELS if l != label and l != "ranging_directional")
        else:
            other = next(l for l in COMPOSITE_LABELS if l != label)
        yield f"blocks-{label}-allowed={other}", label, [other], False
    yield "clean_and_choppy_distinct-choppy_bar", "trending_up_choppy", ["trending_up_clean"], False
    yield "clean_and_choppy_distinct-clean_bar", "trending_up_clean", ["trending_up_choppy"], False
    yield ("ranging_directional_blocked_by_trending_gate", "ranging_directional",
           ["trending_up_clean", "trending_down_clean"], False)
    for sub in ["ranging_directional_up", "ranging_directional_down"]:
        yield f"bare_ranging_directional_covers-{sub}", sub, ["ranging_directional"], True
    for bar in ["ranging_directional", "ranging_directional_down"]:
        yield f"explicit_up_does_not_cover-{bar}", bar, ["ranging_directional_up"], False


def test_composite_gate_allows_matching_and_blocks_mismatching_labels(subtests):
    cases = list(_gate_cases())
    assert len(cases) == 2 * len(COMPOSITE_LABELS) + len(COMPOSITE_LABELS) + 7
    for name, bar_label, allow, admits in cases:
        with subtests.test(case=name):
            df = _gated_df(bar_label)
            bt = Backtester(
                initial_capital=1000, commission_pct=0, slippage_pct=0,
                regime_enabled=True, allowed_regimes=allow,
            )
            result = bt.run(df, save=False)
            if admits:
                assert result["total_trades"] >= 1, (
                    f"Composite label '{bar_label}' in allowed_regimes {allow} should permit the entry"
                )
            else:
                assert result["total_trades"] == 0, (
                    f"Bar regime '{bar_label}' must be blocked when only {allow} is allowed"
                )


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
