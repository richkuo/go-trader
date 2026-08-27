import os
import sys

import numpy as np
import pandas as pd
import pytest

_BT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _BT_DIR not in sys.path:
    sys.path.insert(0, _BT_DIR)

import eval_windows as ew
import fee_audit as fa
from backtester import Backtester



def _metrics(equities, initial_capital=1000.0, timeframe="1d"):
    idx = pd.date_range("2024-01-01", periods=len(equities), freq="D")
    equity_df = pd.DataFrame({"equity": np.asarray(equities, dtype=float)},
                             index=idx)
    df = pd.DataFrame({"close": np.full(len(equities), 100.0)}, index=idx)
    bt = Backtester(initial_capital=initial_capital)
    return bt._calculate_metrics(equity_df, [], df, timeframe=timeframe)


def test_deepening_blowup_reads_negative_not_positive():
    m = _metrics([1000, 500, -500, -1500, -2500])
    assert m["liquidated"] is True
    assert m["sharpe_ratio"] < 0
    assert m["total_return_pct"] == pytest.approx(-100.0)
    assert m["max_drawdown_pct"] == pytest.approx(-100.0)


def test_deeper_blowup_never_ranks_above_shallower():
    deep = _metrics([1000, 500, -500, -2500])
    shallow = _metrics([1000, 500, -500, -600])
    for key in ("sharpe_ratio", "total_return_pct", "max_drawdown_pct",
                "volatility_pct", "sortino_ratio"):
        assert deep[key] == pytest.approx(shallow[key])
    assert deep["liquidated"] and shallow["liquidated"]


def test_floor_is_sticky_no_resurrection():
    m = _metrics([1000, -200, 800, 900])
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)


def test_recovery_before_zero_is_not_liquidation():
    m = _metrics([1000, 50, 400, 600])
    assert m["liquidated"] is False
    assert m["total_return_pct"] == pytest.approx(-40.0)


def test_healthy_curve_metrics_unchanged():
    equities = [1000.0, 1100.0, 1050.0, 1200.0]
    m = _metrics(equities)
    assert m["liquidated"] is False
    assert m["total_return_pct"] == pytest.approx(20.0)
    rets = pd.Series(equities).pct_change().dropna()
    expected_sharpe = (rets.mean() / rets.std()) * np.sqrt(365)
    assert m["sharpe_ratio"] == pytest.approx(round(expected_sharpe, 3))


def test_zero_equity_bar_counts_as_bust():
    m = _metrics([1000, 0, 500])
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)


def test_early_bust_one_sample_reads_negative_not_neutral():
    m = _metrics([1000, -500, 800, 900])
    assert m["liquidated"] is True
    assert m["sharpe_ratio"] < 0
    assert m["sortino_ratio"] < 0
    assert m["volatility_pct"] != 0


def test_first_bar_bust_zero_samples_reads_negative():
    m = _metrics([-100, 50, 75])
    assert m["liquidated"] is True
    assert m["total_return_pct"] == pytest.approx(-100.0)
    assert m["sharpe_ratio"] < 0
    assert m["sortino_ratio"] < 0


def test_liquidation_floor_is_timeframe_independent():
    bust = [1000, 500, -500, -1500]
    m_1h = _metrics(bust, timeframe="1h")
    m_4h = _metrics(bust, timeframe="4h")
    m_1d = _metrics(bust, timeframe="1d")
    assert m_1h["liquidated"] and m_4h["liquidated"] and m_1d["liquidated"]
    for key in ("sharpe_ratio", "sortino_ratio", "volatility_pct"):
        assert m_1h[key] == m_4h[key] == m_1d[key]
    assert m_1h["sharpe_ratio"] < 0
    assert m_1h["volatility_pct"] != 0


def test_faster_bust_never_outranks_slower_on_sharpe():
    fast = _metrics([1000, -500, 0, 0])
    slow = _metrics([1000, 900, 800, -200])
    assert fast["liquidated"] and slow["liquidated"]
    assert fast["sharpe_ratio"] <= slow["sharpe_ratio"]
    assert fast["sortino_ratio"] <= slow["sortino_ratio"]



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



def _results(ret=-100.0, dd=-100.0, sharpe=-2.0, trades=3, liquidated=True):
    return {"total_return_pct": ret, "max_drawdown_pct": dd,
            "sharpe_ratio": sharpe, "total_trades": trades,
            "liquidated": liquidated}


def test_leg_from_results_propagates_liquidated():
    assert ew.leg_from_results(_results())["liquidated"] is True
    assert ew.leg_from_results(_results(liquidated=False))["liquidated"] is False
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
    assert score["verdict"] in ("pass", "fail")
    no_key = {k: v for k, v in healthy.items() if k != "liquidated"}
    score2 = ew.score_candidate({"A 1h": no_key}, {"A 1h": bar})
    assert score2["liquidated_legs"] == 0


def test_liquidated_legs_counts_every_blowup_including_unscored():
    blown_no_bar = ew.leg_from_results(_results())
    healthy = ew.leg_from_results(_results(ret=20.0, dd=-10.0, sharpe=1.5,
                                           liquidated=False))
    score = ew.score_candidate(
        {"A 1h": blown_no_bar, "B 1h": healthy},
        {"B 1h": {"sharpe": 0.5, "ddadj": 0.5, "n": 8}})
    assert score["scored_datasets"] == 1
    assert score["liquidated_legs"] == 1


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



def _screen_leg_with(monkeypatch, net_liquidated, gross_liquidated):
    def fake_run_leg(reg, name, params, sym, tf, window, capital=0.0,
                     direction=None, commission_pct=None, slippage_pct=None,
                     **kw):
        is_gross = commission_pct == 0.0
        blown = gross_liquidated if is_gross else net_liquidated
        return {"trades": 3, "span_days": 30.0,
                "return_pct": -100.0 if blown else 1.0,
                "sharpe": -100.0 if blown else 0.5,
                "liquidated": blown}

    monkeypatch.setattr(fa, "run_leg", fake_run_leg, raising=True)
    leg = fa.screen_leg(object(), "x", "BTC/USDT", "1h",
                        ("2026-01-01", None), capital=1000.0)
    assert leg is not None and leg["error"] is None
    return leg


def test_screen_leg_liquidated_when_net_run_busts(monkeypatch):
    assert _screen_leg_with(monkeypatch, True, False)["liquidated"] is True


def test_screen_leg_liquidated_when_only_gross_run_busts(monkeypatch):
    assert _screen_leg_with(monkeypatch, False, True)["liquidated"] is True


def test_screen_leg_not_liquidated_when_neither_run_busts(monkeypatch):
    assert _screen_leg_with(monkeypatch, False, False)["liquidated"] is False



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



def test_liquidated_ddadj_floor_constant_mirrors_backtester():
    from backtester import LIQUIDATED_METRIC_FLOOR
    assert ew.LIQUIDATED_DDADJ_FLOOR == -LIQUIDATED_METRIC_FLOOR


def test_leg_from_results_floors_ddadj_when_liquidated():
    blown = ew.leg_from_results(_results())
    assert blown["ddadj"] == ew.LIQUIDATED_DDADJ_FLOOR
    survivor = ew.leg_from_results(
        _results(ret=-50.0, dd=-25.0, sharpe=-1.0, liquidated=False))
    assert survivor["ddadj"] == pytest.approx(-2.0)
    assert survivor["ddadj"] > blown["ddadj"]


def test_leg_from_results_ddadj_unfloored_when_not_liquidated():
    leg = ew.leg_from_results(_results(ret=30.0, dd=-15.0, liquidated=False))
    assert leg["ddadj"] == pytest.approx(2.0)
