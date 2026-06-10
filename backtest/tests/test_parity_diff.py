"""Tests for backtest/parity_diff.py (#906 D7.4)."""
import json
import os
import sys

import numpy as np
import pandas as pd

_BACKTEST_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BACKTEST_DIR not in sys.path:
    sys.path.insert(0, _BACKTEST_DIR)

from parity_diff import (  # noqa: E402
    ParityConfig,
    backtest_effective_frame,
    compare_parity,
    format_summary,
    live_bar_decision,
    vector_frame,
)


def _flat_df(n: int = 60) -> pd.DataFrame:
    idx = pd.date_range("2024-01-01", periods=n, freq="D", tz="UTC")
    close = np.full(n, 100.0)
    return pd.DataFrame(
        {"open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 1000.0},
        index=idx,
    )


def _registry_stub():
  class Reg:
      def get_strategy(self, name):
          return {"default_params": {}}

      def apply_strategy(self, name, df, params=None):
          out = df.copy()
          # Causal signal: bar i is 1 once i >= 34 in *this* window (matches
          # both full-history vector reads and sliding-window live replay).
          out["signal"] = [1 if i >= 34 else 0 for i in range(len(df))]
          return out

  return Reg()


def test_backtest_effective_shift_aligns_decision_to_next_bar():
    vec = pd.DataFrame(
        {
            "signal": [0, 1, 0, -1, 0],
            "open_action": ["none", "long", "none", "short", "none"],
            "close_fraction": [0.0, 0.0, 0.5, 0.0, 0.0],
            "regime": ["", "ranging", "ranging", "trending_up", ""],
        },
        index=pd.date_range("2024-01-01", periods=5, freq="D"),
    )
    eff = backtest_effective_frame(vec)
    assert eff.iloc[2]["signal"] == 1
    assert eff.iloc[2]["open_action"] == "long"
    assert eff.iloc[3]["close_fraction"] == 0.5
    assert eff.iloc[3]["regime"] == "ranging"


def test_vector_and_live_replay_agree_on_constant_strategy():
    df = _flat_df(60)
    cfg = ParityConfig(
        strategy_name="stub",
        symbol="BTC/USDT",
        timeframe="1d",
        params={},
        min_warmup=30,
    )
    reg = _registry_stub()

    from regime import ensure_regime_columns

    result = compare_parity(
        df,
        cfg,
        apply_strategy=reg.apply_strategy,
        get_strategy=reg.get_strategy,
        ensure_regime_columns=ensure_regime_columns,
        include_fills=False,
    )
    assert result.bars_compared == 30
    assert result.mismatches == 0


def test_live_bar_decision_matches_vector_last_row():
    df = _flat_df(40)
    cfg = ParityConfig(strategy_name="stub", symbol="X", timeframe="1d", params={}, min_warmup=30)
    reg = _registry_stub()
    vec = vector_frame(
        df, cfg, apply_strategy=reg.apply_strategy, ensure_regime_columns=lambda d, **_: d,
    )
    live = live_bar_decision(
        df, cfg, apply_strategy=reg.apply_strategy, get_strategy=reg.get_strategy,
    )
    idx = df.index[-1]
    assert int(vec.loc[idx, "signal"]) == live["signal"]


def test_format_summary_reports_mismatch():
    result = type("R", (), {
        "bars_compared": 2,
        "mismatches": 1,
        "fills": [],
        "rows": [{
            "decision_match": False,
            "bar": "2024-01-02T00:00:00+00:00",
            "vector_signal": 1,
            "live_signal": 0,
            "vector_regime": "a",
            "live_regime": "b",
            "vector_open_action": "long",
            "live_open_action": "none",
            "vector_close_fraction": 0.0,
            "live_close_fraction": 0.0,
        }],
    })()
    text = format_summary(result)
    assert "Mismatches:    1" in text
    assert "signal 1≠0" in text


def test_regression_issue_index_documents_closed_bug_tests():
    """D8.4 — every closed backtest bug issue has a named regression test file."""
    tests_dir = os.path.join(_BACKTEST_DIR, "tests")
    index = {
        "#302": ["test_backtester_end_to_end.py", "test_backtester_fills.py",
                 "test_options_vol_math.py", "test_options_iv_rank.py"],
        "#304": ["test_backtest_reporting.py"],
        "#715": ["test_post_tp_sl.py"],  # cited on regression test functions, not module doc
        "#730": ["test_backtester_lookahead.py"],
    }
    for issue, files in index.items():
        for fname in files:
            path = os.path.join(tests_dir, fname)
            assert os.path.isfile(path), f"{issue} regression file missing: {fname}"
            with open(path) as fh:
                body = fh.read()
            assert issue.replace("#", "") in body or issue in body, (
                f"{fname} should cite {issue} somewhere in the file"
            )
