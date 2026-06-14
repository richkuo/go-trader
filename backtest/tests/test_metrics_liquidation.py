"""#1005: liquidation floor for negative equity curves.

``equity.pct_change()`` over a negative base inverts return signs (a deepening
blowup reads as a positive return, a recovery as negative), corrupting
Sharpe/Sortino/volatility — reachable for stop-less short legs losing >100%.
A real account is dead at zero: ``_calculate_metrics`` sticky-floors the curve
at 0 from the first bust bar and flags the run ``liquidated`` so harness
consumers (eval_windows, fee_audit) surface blown legs instead of silently
trusting their metrics.
"""
import os
import sys

import numpy as np
import pandas as pd
import pytest

_BT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BT_DIR not in sys.path:
    sys.path.insert(0, _BT_DIR)

import eval_windows as ew  # noqa: E402
import fee_audit as fa  # noqa: E402
from backtester import Backtester  # noqa: E402


# ---------------------------------------------------------------------------
# _calculate_metrics — unit level
# ---------------------------------------------------------------------------

def _metrics(equities, initial_capital=1000.0, timeframe="1d"):
    idx = pd.date_range("2024-01-01", periods=len(equities), freq="D")
    equity_df = pd.DataFrame({"equity": np.asarray(equities, dtype=float)},
                             index=idx)
    df = pd.DataFrame({"close": np.full(len(equities), 100.0)}, index=idx)
    bt = Backtester(initial_capital=initial_capital)
    return bt._calculate_metrics(equity_df, [], df, timeframe=timeframe)


def test_deepening_blowup_reads_negative_not_positive():
    # Pre-#1005: pct_change over the negative base read -500 → -1500 → -2500
    # as POSITIVE per-bar returns, inflating Sharpe. Floored, the leg carries
    # only the path to death (-50%, -100%) — Sharpe must be negative.
    m = _metrics([1000, 500, -500, -1500, -2500])
    assert m["liquidated"] is True
    assert m["sharpe_ratio"] < 0
    assert m["total_return_pct"] == pytest.approx(-100.0)
    assert m["max_drawdown_pct"] == pytest.approx(-100.0)


def test_deeper_blowup_never_ranks_above_shallower():
    # Identical paths to the bust bar; one then deepens 4x further. Floored,
    # both runs are dead at the same bar and must carry identical metrics —
    # a deeper blowup can tie, never win.
    deep = _metrics([1000, 500, -500, -2500])
    shallow = _metrics([1000, 500, -500, -600])
    for key in ("sharpe_ratio", "total_return_pct", "max_drawdown_pct",
                "volatility_pct", "sortino_ratio"):
        assert deep[key] == pytest.approx(shallow[key])
    assert deep["liquidated"] and shallow["liquidated"]


def test_floor_is_sticky_no_resurrection():
    # A recovery after the bust bar is fiction — a real account was already
    # liquidated. The floor must not let equity resurrect.
    m = _metrics([1000, -200, 800, 900])
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)


def test_recovery_before_zero_is_not_liquidation():
    # Deep drawdown that never touches 0 is a survivable path, not a bust.
    m = _metrics([1000, 50, 400, 600])
    assert m["liquidated"] is False
    assert m["total_return_pct"] == pytest.approx(-40.0)


def test_healthy_curve_metrics_unchanged():
    # Positive control: never-negative equity must be byte-identical to the
    # pre-#1005 computation (returns straight off pct_change).
    equities = [1000.0, 1100.0, 1050.0, 1200.0]
    m = _metrics(equities)
    assert m["liquidated"] is False
    assert m["total_return_pct"] == pytest.approx(20.0)
    rets = pd.Series(equities).pct_change().dropna()
    expected_sharpe = (rets.mean() / rets.std()) * np.sqrt(365)
    assert m["sharpe_ratio"] == pytest.approx(round(expected_sharpe, 3))


def test_zero_equity_bar_counts_as_bust():
    # Boundary: equity == 0 exactly is bust (<= 0), not a survivable bar.
    m = _metrics([1000, 0, 500])
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)


def test_early_bust_one_sample_reads_negative_not_neutral():
    # The #1005 regression at the degenerate boundary: a bust on the 2nd bar
    # leaves a single surviving return (the -100% bust bar); post-bust bars
    # drop out as NaN. The len>1 variance guard previously collapsed
    # Sharpe/Sortino/volatility to a NEUTRAL 0.0 — a dead account reading as
    # "fine" — which ranks a fast blowup ABOVE a slow one. A liquidated leg
    # must read clearly negative on the risk-adjusted axes and non-zero vol.
    m = _metrics([1000, -500, 800, 900])  # bust idx 1 -> 1 surviving sample
    assert m["liquidated"] is True
    assert m["sharpe_ratio"] < 0
    assert m["sortino_ratio"] < 0
    assert m["volatility_pct"] != 0


def test_first_bar_bust_zero_samples_reads_negative():
    # Boundary: equity <= 0 on the very first bar leaves ZERO surviving
    # returns (empty series). Must still read negative, never neutral 0.0.
    m = _metrics([-100, 50, 75])  # bust idx 0 -> 0 surviving samples
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)
    assert m["sharpe_ratio"] < 0
    assert m["sortino_ratio"] < 0


def test_liquidation_floor_is_timeframe_independent():
    # #1005 follow-up: the blown-leg risk-adjusted floor must be uniform across
    # timeframes. The earlier `-ann_factor` floor scaled with the timeframe
    # (1h ≈ -93.6, 4h ≈ -46.8), so the SAME total loss carried a ~2x different
    # Sharpe by timeframe and perturbed mean-Sharpe rankings of liquidated
    # strategies by which timeframe they busted on. Two equally-dead legs must
    # now tie on every risk-adjusted axis regardless of timeframe.
    bust = [1000, 500, -500, -1500]
    m_1h = _metrics(bust, timeframe="1h")
    m_4h = _metrics(bust, timeframe="4h")
    m_1d = _metrics(bust, timeframe="1d")
    assert m_1h["liquidated"] and m_4h["liquidated"] and m_1d["liquidated"]
    for key in ("sharpe_ratio", "sortino_ratio", "volatility_pct"):
        assert m_1h[key] == m_4h[key] == m_1d[key]
    # And still clearly negative / non-zero, not collapsed to neutral.
    assert m_1h["sharpe_ratio"] < 0
    assert m_1h["volatility_pct"] != 0


def test_faster_bust_never_outranks_slower_on_sharpe():
    # Invariant: a leg busting earlier (less of the death path measured) must
    # never report a HIGHER Sharpe than one busting later. Pre-fix the 1-sample
    # fast bust read 0.0 while the 3-sample slow bust read negative, inverting
    # the ranking on the very axis #1005 set out to fix.
    fast = _metrics([1000, -500, 0, 0])      # bust idx 1 -> 1 sample
    slow = _metrics([1000, 900, 800, -200])  # bust idx 3 -> 3 samples
    assert fast["liquidated"] and slow["liquidated"]
    assert fast["sharpe_ratio"] <= slow["sharpe_ratio"]
    assert fast["sortino_ratio"] <= slow["sortino_ratio"]


# ---------------------------------------------------------------------------
# End-to-end — stop-less short leg blowing past -100% (#989 harness shape)
# ---------------------------------------------------------------------------

def test_short_leg_blowup_end_to_end():
    closes = np.array([100, 100, 150, 250, 300, 300], dtype=float)
    n = len(closes)
    idx = pd.date_range("2024-01-01", periods=n, freq="D")
    df = pd.DataFrame(
        {
            "open": closes,
            "high": closes + 0.5,
            "low": closes - 0.5,
            "close": closes,
            "volume": np.full(n, 1000.0),
            # Short opens at bar 1 open (100) and is never closed; by close
            # 250 the buy-back cost exceeds the 2x notional held → equity < 0.
            "signal": np.array([-1, 0, 0, 0, 0, 0], dtype=float),
        },
        index=idx,
    )
    bt = Backtester(initial_capital=10000.0, direction="short",
                    commission_pct=0.0, slippage_pct=0.0)
    results = bt.run(df, strategy_name="x", symbol="BTC/USDT",
                     timeframe="1d", save=False)
    assert results["liquidated"] is True
    assert results["total_return_pct"] == pytest.approx(-100.0)
    assert results["max_drawdown_pct"] == pytest.approx(-100.0)


# ---------------------------------------------------------------------------
# eval_windows — flag propagation + non-silent reporting
# ---------------------------------------------------------------------------

def _results(ret=-100.0, dd=-100.0, sharpe=-2.0, trades=3, liquidated=True):
    return {"total_return_pct": ret, "max_drawdown_pct": dd,
            "sharpe_ratio": sharpe, "total_trades": trades,
            "liquidated": liquidated}


def test_leg_from_results_propagates_liquidated():
    assert ew.leg_from_results(_results())["liquidated"] is True
    assert ew.leg_from_results(_results(liquidated=False))["liquidated"] is False
    # Results dicts predating #1005 lack the key — must default False.
    legacy = _results()
    del legacy["liquidated"]
    assert ew.leg_from_results(legacy)["liquidated"] is False


def test_score_candidate_counts_liquidated_legs_without_verdict_change():
    blown = ew.leg_from_results(_results())
    healthy = ew.leg_from_results(_results(ret=20.0, dd=-10.0, sharpe=1.5,
                                           liquidated=False))
    bar = {"sharpe": 0.5, "ddadj": 0.5, "n": 8}
    score = ew.score_candidate(
        {"A 1h": blown, "B 1h": healthy}, {"A 1h": bar, "B 1h": bar})
    assert score["liquidated_legs"] == 1
    # The floored metrics already rank the blown leg honestly; liquidation is
    # surfaced, not a verdict input.
    assert score["verdict"] in ("pass", "fail")
    # Legs without the key (hand-built or legacy) must not crash the count.
    no_key = {k: v for k, v in healthy.items() if k != "liquidated"}
    score2 = ew.score_candidate({"A 1h": no_key}, {"A 1h": bar})
    assert score2["liquidated_legs"] == 0


def test_liquidated_legs_counts_every_blowup_including_unscored():
    # Design (#1005): liquidated_legs counts EVERY blown leg, including one
    # with no incumbent bar (absent from `scored`), so operators see all
    # deaths even when a dataset has no comparison baseline. The count is
    # therefore over `rows`, not `scored`, and may exceed scored_datasets.
    blown_no_bar = ew.leg_from_results(_results())  # liquidated, no bar below
    healthy = ew.leg_from_results(_results(ret=20.0, dd=-10.0, sharpe=1.5,
                                           liquidated=False))
    score = ew.score_candidate(
        {"A 1h": blown_no_bar, "B 1h": healthy},
        {"B 1h": {"sharpe": 0.5, "ddadj": 0.5, "n": 8}})  # only B has a bar
    assert score["scored_datasets"] == 1            # only B is scored
    assert score["liquidated_legs"] == 1            # A counted despite no bar


def test_format_window_report_marks_liquidated_rows():
    blown = ew.leg_from_results(_results())
    bar = {"sharpe": 0.5, "ddadj": 0.5, "n": 8}
    score = ew.score_candidate({"SOL/USDT 4h": blown}, {"SOL/USDT 4h": bar})
    score["window"] = "2023"
    score["window_range"] = ["2023-01-01", "2024-01-01"]
    report = ew.format_window_report(score)
    assert "LIQ" in report
    assert "1 liquidated leg(s)" in report


def test_format_window_report_silent_when_no_liquidation():
    healthy = ew.leg_from_results(_results(ret=20.0, dd=-10.0, sharpe=1.5,
                                           liquidated=False))
    bar = {"sharpe": 0.5, "ddadj": 0.5, "n": 8}
    score = ew.score_candidate({"BTC/USDT 1h": healthy}, {"BTC/USDT 1h": bar})
    score["window"] = "oos"
    score["window_range"] = ["2026-01-01", None]
    report = ew.format_window_report(score)
    assert "LIQ" not in report
    assert "liquidated" not in report


# ---------------------------------------------------------------------------
# fee_audit — aggregation count + markdown section
# ---------------------------------------------------------------------------

def _fa_leg(liquidated=False):
    return {"error": None, "trades": 10, "span_days": 365.0,
            "net_ret": -100.0 if liquidated else 5.0,
            "gross_ret": -100.0 if liquidated else 8.0,
            "net_sharpe": -2.0 if liquidated else 1.0,
            "liquidated": liquidated}


def test_aggregate_counts_liquidated_legs():
    row = fa.aggregate_strategy(
        "blower", "futures", [_fa_leg(True), _fa_leg(False)])
    assert row["n_liquidated"] == 1
    # Legacy leg dicts without the key must not crash.
    legacy = {k: v for k, v in _fa_leg().items() if k != "liquidated"}
    row2 = fa.aggregate_strategy("ok", "spot", [legacy])
    assert row2["n_liquidated"] == 0


def test_render_markdown_liquidated_section_only_when_present():
    meta = {"command": "uv run ...", "registry": "futures",
            "windows_desc": "2023", "datasets_desc": "SOL/USDT 4h",
            "capital": 1000.0, "date": "2026-06-12"}
    blown = fa.aggregate_strategy("blower", "futures", [_fa_leg(True)])
    md = fa.render_markdown(fa.rank_rows([blown]), meta)
    assert "## Liquidated legs" in md
    assert "blower" in md

    clean = fa.aggregate_strategy("ok", "spot", [_fa_leg(False)])
    md_clean = fa.render_markdown(fa.rank_rows([clean]), meta)
    assert "## Liquidated legs" not in md_clean
