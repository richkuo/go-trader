"""Tests for backtest/eval_windows.py (#977 M1 harness) — pure scoring layer.

Leg execution (run_leg) is exercised end-to-end against the Backtester with a
synthetic frame; everything else (bars, verdicts, sweeps, parsing) is tested
without data access.
"""
import math

import numpy as np
import pandas as pd
import pytest

import eval_windows as ew


def _leg(sharpe, ddadj=0.0, trades=5, return_pct=-1.0, max_dd_pct=-10.0):
    return {"sharpe": sharpe, "ddadj": ddadj, "trades": trades,
            "return_pct": return_pct, "max_dd_pct": max_dd_pct,
            "bh_return_pct": -30.0}


# ---------------------------------------------------------------------------
# dd_adjusted_return
# ---------------------------------------------------------------------------

def test_ddadj_definition():
    # #963: DDadj = return / |max DD|
    assert ew.dd_adjusted_return(-10.0, -50.0) == pytest.approx(-0.2)
    assert ew.dd_adjusted_return(5.0, -2.5) == pytest.approx(2.0)


def test_ddadj_zero_drawdown_is_zero():
    # No drawdown (typically zero-trade leg) must not inflate the mean.
    assert ew.dd_adjusted_return(0.0, 0.0) == 0.0
    assert ew.dd_adjusted_return(12.0, 0.0) == 0.0


# ---------------------------------------------------------------------------
# leg_from_results
# ---------------------------------------------------------------------------

def test_leg_from_results_collapses_backtester_dict():
    results = {"total_return_pct": -12.5, "max_drawdown_pct": -25.0,
               "sharpe_ratio": -0.9, "total_trades": 17}
    leg = ew.leg_from_results(results, bh_return_pct=-44.3)
    assert leg["sharpe"] == -0.9
    assert leg["return_pct"] == -12.5
    assert leg["max_dd_pct"] == -25.0
    assert leg["ddadj"] == pytest.approx(-0.5)
    assert leg["trades"] == 17
    assert leg["bh_return_pct"] == -44.3


# ---------------------------------------------------------------------------
# incumbent_bars
# ---------------------------------------------------------------------------

def test_incumbent_bars_per_dataset_median():
    legs = {
        "BTC/USDT 1h": {
            "a": _leg(-1.0, ddadj=-0.5),
            "b": _leg(-2.0, ddadj=-1.0),
            "c": _leg(-3.0, ddadj=-1.5),
        },
    }
    bars = ew.incumbent_bars(legs)
    assert bars["BTC/USDT 1h"]["sharpe"] == pytest.approx(-2.0)
    assert bars["BTC/USDT 1h"]["ddadj"] == pytest.approx(-1.0)
    assert bars["BTC/USDT 1h"]["n"] == 3


def test_incumbent_bars_skips_missing_legs_and_empty_datasets():
    legs = {
        "BTC/USDT 1h": {"a": _leg(-1.0), "b": None, "c": _leg(-3.0)},
        "SOL/USDT 4h": {"a": None, "b": None},
    }
    bars = ew.incumbent_bars(legs)
    # median over the two present legs only
    assert bars["BTC/USDT 1h"]["sharpe"] == pytest.approx(-2.0)
    assert bars["BTC/USDT 1h"]["n"] == 2
    # no incumbent ran → no bar for that dataset
    assert bars["SOL/USDT 4h"] is None


# ---------------------------------------------------------------------------
# score_candidate
# ---------------------------------------------------------------------------

def _bars(sharpe=-1.0, ddadj=-0.5, datasets=("d1", "d2", "d3", "d4")):
    return {ds: {"sharpe": sharpe, "ddadj": ddadj, "n": 8} for ds in datasets}


def test_score_pass_when_means_beat_bar_on_both_metrics():
    legs = {ds: _leg(-0.3, ddadj=-0.2) for ds in ("d1", "d2", "d3", "d4")}
    score = ew.score_candidate(legs, _bars())
    assert score["verdict"] == "pass"
    assert score["mean_sharpe"] == pytest.approx(-0.3)
    assert score["mean_bar_sharpe"] == pytest.approx(-1.0)
    assert score["beats_sharpe_count"] == 4
    assert score["beats_ddadj_count"] == 4
    assert not score["degenerate"]


def test_score_fail_when_only_one_metric_beats_bar():
    # Sharpe beats the bar, DDadj does not → fail (the #955 bar is BOTH).
    legs = {ds: _leg(-0.3, ddadj=-0.9) for ds in ("d1", "d2", "d3", "d4")}
    score = ew.score_candidate(legs, _bars(ddadj=-0.5))
    assert score["verdict"] == "fail"


def test_score_degenerate_zero_trade_majority_rejected():
    # Means beat the bar, but 3/4 legs never traded — #976 rejects these
    # (htf_factor 5/8 cleared the bar while going zero-trade on 4/6 datasets).
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2, trades=4),
        "d2": _leg(0.0, ddadj=0.0, trades=0),
        "d3": _leg(0.0, ddadj=0.0, trades=0),
        "d4": _leg(0.0, ddadj=0.0, trades=0),
    }
    score = ew.score_candidate(legs, _bars())
    assert score["degenerate"]
    assert score["verdict"] == "degenerate"


def test_score_trading_exactly_half_is_not_degenerate():
    # boundary: ceil(4/2)=2 traded datasets suffice
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2, trades=4),
        "d2": _leg(-0.3, ddadj=-0.2, trades=1),
        "d3": _leg(0.0, ddadj=0.0, trades=0),
        "d4": _leg(0.0, ddadj=0.0, trades=0),
    }
    score = ew.score_candidate(legs, _bars())
    assert not score["degenerate"]
    assert score["verdict"] == "pass"


def test_score_unscored_datasets_excluded_from_means():
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2),
        "d2": None,                      # candidate had no data
        "d3": _leg(-0.5, ddadj=-0.3),
    }
    bars = _bars(datasets=("d1", "d3"))
    bars["d2"] = None                    # incumbents had no data either
    score = ew.score_candidate(legs, bars)
    assert score["scored_datasets"] == 2
    assert score["mean_sharpe"] == pytest.approx(-0.4)


def test_score_no_data_verdict():
    score = ew.score_candidate({"d1": None}, {"d1": None})
    assert score["verdict"] == "no data"


# ---------------------------------------------------------------------------
# sweep helpers
# ---------------------------------------------------------------------------

def test_parse_sweep_arg_coerces_numbers():
    assert ew.parse_sweep_arg("period=10,14,20") == ("period", [10, 14, 20])
    assert ew.parse_sweep_arg("z=1.75,2.0") == ("z", [1.75, 2.0])
    assert ew.parse_sweep_arg("mode=fade,breakout") == ("mode", ["fade", "breakout"])


def test_parse_sweep_arg_rejects_malformed():
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("period")
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("=1,2")
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("period=")


def test_expand_sweep_cartesian_preserves_base_params():
    combos = ew.expand_sweep({"keep": 1}, [("a", [1, 2]), ("b", ["x"])])
    assert len(combos) == 2
    labels = [c[0] for c in combos]
    assert labels == ["a=1 b=x", "a=2 b=x"]
    for _, params in combos:
        assert params["keep"] == 1
        assert "a" in params and params["b"] == "x"


# ---------------------------------------------------------------------------
# dataset / window definitions
# ---------------------------------------------------------------------------

def test_parse_dataset_arg():
    assert ew.parse_dataset_arg("BTC/USDT:1h") == ("BTC/USDT", "1h")
    with pytest.raises(ValueError):
        ew.parse_dataset_arg("BTC-USDT-1h")


def test_versioned_definitions_match_protocol():
    # #963/#976 incumbent eight — breakout excluded (futures-only on the
    # long-leg harness), sma_crossover in its slot.
    assert len(ew.INCUMBENTS) == 8
    assert "breakout" not in ew.INCUMBENTS
    assert "sma_crossover" in ew.INCUMBENTS
    # six audit datasets, five windows, protocol + held-out partition exact
    assert len(ew.DATASETS) == 6
    assert set(ew.PROTOCOL_WINDOWS) | set(ew.HELD_OUT_WINDOWS) == set(ew.WINDOWS)
    assert ew.WINDOWS["is"] == ("2025-06-10", "2026-01-01")
    assert ew.WINDOWS["oos"] == ("2026-01-01", None)
    # held-out windows are bounded (never run to "latest" — they must stay
    # frozen as data accrues)
    for w in ew.HELD_OUT_WINDOWS:
        assert ew.WINDOWS[w][1] is not None


# ---------------------------------------------------------------------------
# run_leg end-to-end on a synthetic frame (no network/cache access)
# ---------------------------------------------------------------------------

class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {"period": 2},
                                        "description": "test"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1   # buy
        sig[20::20] = -1  # sell
        out["signal"] = sig
        return out


def _synthetic_df(n=120):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_run_leg_returns_leg_metrics(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None))
    assert leg is not None
    assert leg["trades"] > 0
    for key in ("sharpe", "return_pct", "max_dd_pct", "ddadj",
                "trades", "bh_return_pct"):
        assert key in leg
    # B&H over the synthetic frame: close[-1] vs close[0]
    expected_bh = (df["close"].iloc[-1] - df["close"].iloc[0]) / df["close"].iloc[0] * 100
    assert leg["bh_return_pct"] == pytest.approx(expected_bh, abs=0.01)


def test_run_leg_empty_data_returns_none(monkeypatch):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: pd.DataFrame(), raising=True)
    assert ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                      ("2023-01-01", "2024-01-01")) is None


# ---------------------------------------------------------------------------
# validate_candidate — entry transforms must be modeled faithfully or rejected
# (mirrors run_backtest.py --config guards; review finding on PR #994)
# ---------------------------------------------------------------------------

def test_validate_candidate_rejects_short_without_close_refs():
    with pytest.raises(ValueError, match="silently dropped"):
        ew.validate_candidate({"name": "x", "direction": "short"})


def test_validate_candidate_rejects_both_without_close_refs():
    # "both" is the sneakier case: _apply_direction_invert never masks it,
    # so without this guard it runs long/flat with zero indication.
    with pytest.raises(ValueError, match="silently dropped"):
        ew.validate_candidate({"name": "x", "direction": "both"})


def test_validate_candidate_allows_short_with_close_refs():
    c = {"name": "x", "direction": "short",
         "close_strategies": [{"name": "tp_at_pct", "params": {}}]}
    assert ew.validate_candidate(c) is c


def test_validate_candidate_invert_signal_gated_by_type():
    # default type is perps → allowed; declared non-perps type → rejected
    assert ew.validate_candidate({"name": "x", "invert_signal": True})
    assert ew.validate_candidate(
        {"name": "x", "invert_signal": True, "type": "manual"})
    with pytest.raises(ValueError, match="invert_signal"):
        ew.validate_candidate(
            {"name": "x", "invert_signal": True, "type": "spot"})


def test_validate_candidate_rejects_bogus_direction_and_missing_name():
    with pytest.raises(ValueError, match="direction"):
        ew.validate_candidate({"name": "x", "direction": "sideways"})
    with pytest.raises(ValueError, match="name"):
        ew.validate_candidate({})


def test_evaluate_window_validates_before_any_work():
    # Bad candidate must fail at the gate — reg=None proves nothing ran.
    with pytest.raises(ValueError, match="silently dropped"):
        ew.evaluate_window(None, {"name": "x", "direction": "both"},
                           [("BTC/USDT", "1h")], "oos", 1000.0, {})
