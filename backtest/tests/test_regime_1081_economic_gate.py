"""Pure/core tests for the #1081 regime ATR economic gate."""

import os
import sys

import numpy as np
import pandas as pd
import pytest

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_RESEARCH = os.path.join(_BACKTEST, "research")
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_RESEARCH, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import regime_1081_economic_gate as m  # noqa: E402
from backtester import Backtester  # noqa: E402


def test_surface_arms_names_and_supported_configs():
    for surface in m.SURFACES:
        control, candidate = m.surface_arms(surface)
        assert control["label"]
        assert candidate["label"]
        m.validate_arm_config(control)
        m.validate_arm_config(candidate)


def test_rejects_live_only_dynamic_close():
    arm = {
        "close_strategies": [
            {"name": "tiered_tp_atr_live_regime_dynamic", "params": {}}
        ],
    }
    with pytest.raises(ValueError, match="HL-live-only"):
        m.validate_arm_config(arm)


def test_rejects_regime_aware_sl_after_surface():
    arm = {
        "close_strategies": [
            {
                "name": "tiered_tp_atr_regime",
                "params": {
                    "tp_tiers": [
                        {
                            "trend_regime": {
                                "ranging": {
                                    "atr_multiple": 1.0,
                                    "close_fraction": 1.0,
                                    "sl_after": {
                                        "trend_regime": {
                                            "ranging": {"atr_offset": 0.5}
                                        }
                                    },
                                }
                            }
                        }
                    ]
                },
            }
        ],
    }
    with pytest.raises(ValueError, match="sl_after"):
        m.validate_arm_config(arm)


def test_validate_label_stream_rejects_degenerate_and_unknown_labels():
    with pytest.raises(ValueError, match="degenerate"):
        m.validate_label_stream(
            ["ranging_quiet", "ranging_quiet", "ranging_quiet"],
            [True, True, True],
            source="probe",
        )
    with pytest.raises(ValueError, match="unknown"):
        m.validate_label_stream(
            ["ranging_quiet", "bogus", "trending_up_clean"],
            [True, True, True],
            source="probe",
        )


def test_validate_label_stream_counts_valid_rows_only():
    stats = m.validate_label_stream(
        ["ranging_quiet", "bogus_warmup", "trending_up_clean", "ranging_quiet"],
        [True, False, True, True],
        source="probe",
    )
    assert stats["n_valid"] == 3
    assert stats["active_labels"] == 2
    assert stats["counts"] == {"ranging_quiet": 2, "trending_up_clean": 1}


def _trade(entry_date, reason, mae, mfe=2.0, pnl=1.0):
    return {
        "entry_date": entry_date,
        "exit_reason": reason,
        "mae_pct": mae,
        "mfe_pct": mfe,
        "pnl_pct": pnl,
        "shares": 1.0,
        "entry_price": 100.0,
        "exit_price": 101.0,
        "entry_fee": 0.0,
        "exit_fee": 0.0,
    }


def test_summarize_results_stop_out_rate_and_mae_by_entry():
    results = {
        "total_return_pct": 10.0,
        "max_drawdown_pct": -5.0,
        "sharpe_ratio": 1.25,
        "total_trades": 3,
        "trades": [
            _trade("d1", "tiered_tp_atr:1.5", -0.5),
            _trade("d1", "sl", -1.5),  # same entry; any stop leg marks the entry stopped
            _trade("d2", "tiered_tp_atr:3", -0.25),
        ],
    }
    got = m.summarize_results(results)
    assert got["entries"] == 2
    assert got["stop_outs"] == 1
    assert got["stop_out_rate"] == pytest.approx(0.5)
    # Per-entry MAE is min leg MAE: d1=-1.5, d2=-0.25 -> median -0.875.
    assert got["median_mae_pct"] == pytest.approx(-0.875)
    assert got["ddadj"] == pytest.approx(2.0)


def test_compare_summaries_requires_all_gate_axes():
    control = {
        "entries": 3,
        "sharpe": 1.0,
        "ddadj": 0.5,
        "stop_out_rate": 0.25,
        "median_mae_pct": -2.0,
        "total_return_pct": 5.0,
        "max_drawdown_pct": -10.0,
    }
    candidate = {
        "entries": 3,
        "sharpe": 1.2,
        "ddadj": 0.75,
        "stop_out_rate": 0.10,
        "median_mae_pct": -1.0,
        "total_return_pct": 6.0,
        "max_drawdown_pct": -8.0,
    }
    assert m.compare_summaries(control, candidate)["pass"] is True
    worse = dict(candidate, stop_out_rate=0.5)
    verdict = m.compare_summaries(control, worse)
    assert verdict["pass"] is False
    assert "stop-out" in " ".join(verdict["blocking_reasons"])


def test_compare_summaries_equal_risk_adjusted_metrics_do_not_pass():
    control = {
        "entries": 2,
        "sharpe": 1.0,
        "ddadj": 0.5,
        "stop_out_rate": 0.0,
        "median_mae_pct": -1.0,
        "total_return_pct": 5.0,
        "max_drawdown_pct": -10.0,
    }
    candidate = dict(control)
    verdict = m.compare_summaries(control, candidate)
    assert verdict["pass"] is False
    joined = " ".join(verdict["blocking_reasons"])
    assert "Sharpe" in joined and "DD-adjusted" in joined


def test_injected_raw_regime_labels_are_shifted_by_backtester_no_lookahead():
    idx = pd.date_range("2024-01-01", periods=6, freq="D")
    df = pd.DataFrame(
        {
            "open": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "close": [100.0, 100.0, 100.0, 100.26, 100.26, 100.26],
            "atr": [1.0] * 6,
            # Raw open action at idx1 fills at idx2 open.
            "open_action": ["none", "long", "none", "none", "none", "none"],
        },
        index=idx,
    )
    labels = [
        "ranging",
        "ranging",
        "trending_up",
        "trending_up",
        "trending_up",
        "trending_up",
    ]
    frame = m.inject_regime_labels(df, labels)
    close_ref = {
        "name": "tiered_tp_atr_regime",
        "params": {
            "tp_tiers": [
                {
                    "trend_regime": {
                        "ranging": {"atr_multiple": 0.25, "close_fraction": 1.0},
                        "trending_up": {"atr_multiple": 10.0, "close_fraction": 1.0},
                        "trending_down": {"atr_multiple": 10.0, "close_fraction": 1.0},
                    }
                }
            ]
        },
    }
    bt = Backtester(
        initial_capital=10_000.0,
        commission_pct=0.0,
        slippage_pct=0.0,
        close_strategies=[close_ref],
        regime_enabled=True,
    )
    result = bt.run(frame, save=False)
    # Correct path: idx2 entry is stamped with idx1's raw "ranging" label,
    # so the 0.25 ATR tier fires. If injection pre-shifted labels or the
    # backtester read idx2's "trending_up" label, this would hold to the end.
    assert result["trades"][0]["exit_date"] == str(idx[4])


def test_inject_regime_labels_rejects_length_mismatch():
    df = pd.DataFrame({"close": [1.0, 2.0]})
    with pytest.raises(ValueError, match="label count"):
        m.inject_regime_labels(df, ["ranging_quiet"])
