"""Pure/orchestration tests for the #1083 multi-asset regime gate."""

import os
import sys

_THIS = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS, ".."))
_RESEARCH = os.path.join(_BACKTEST, "research")
_ROOT = os.path.abspath(os.path.join(_BACKTEST, ".."))
for _p in (_RESEARCH, _BACKTEST, _ROOT, os.path.join(_ROOT, "shared_tools")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

import pytest

import regime_1083_multi_asset_gate as m  # noqa: E402


def _bakeoff(winner=None):
    return {
        "winner": winner or {"family": "kmeans", "k": 3},
        "candidate_count": 2,
        "target": "volatility",
        "handrule_held_out": {"p_value": 0.01, "abstained": False},
        "significance_alpha": 0.05,
        "bonferroni_alpha": 0.025,
    }


def test_parse_datasets_defaults_to_eval_windows_datasets():
    assert m.parse_datasets("") == list(m.DATASETS)
    assert m.parse_datasets("BTC/USDT:1h,ETH/USDT:4h") == [
        ("BTC/USDT", "1h"),
        ("ETH/USDT", "4h"),
    ]
    with pytest.raises(ValueError, match="SYMBOL:TIMEFRAME"):
        m.parse_datasets("BTC/USDT")


def test_multi_asset_gate_threads_selected_model_to_economic_gate():
    seen = []

    def fake_bakeoff(symbol, timeframe, **kwargs):
        seen.append(("bakeoff", symbol, timeframe, kwargs["families"], kwargs["k_range"]))
        return _bakeoff()

    def fake_fit(symbol, timeframe, winner, **kwargs):
        seen.append(("fit", symbol, timeframe, winner, kwargs["in_sample"]))
        return {"model_id": f"{symbol}-{timeframe}", "states": ["a", "b"], "mapping": {}}

    def fake_economic(**kwargs):
        seen.append(("economic", kwargs["symbol"], kwargs["timeframe"], kwargs["model"]["model_id"]))
        return {"summary": {"pass": True, "blocking_reasons": []}, "rows": []}

    report = m.run_multi_asset_gate(
        datasets=[("BTC/USDT", "1h"), ("ETH/USDT", "4h")],
        min_pass_cells=2,
        families=("kmeans",),
        k_range=(3,),
        bakeoff_fn=fake_bakeoff,
        fit_model_fn=fake_fit,
        economic_gate_fn=fake_economic,
    )

    assert report["summary"]["pass"] is True
    assert report["summary"]["passed_cells"] == 2
    assert seen == [
        ("bakeoff", "BTC/USDT", "1h", ("kmeans",), (3,)),
        ("fit", "BTC/USDT", "1h", {"family": "kmeans", "k": 3}, "is"),
        ("economic", "BTC/USDT", "1h", "BTC/USDT-1h"),
        ("bakeoff", "ETH/USDT", "4h", ("kmeans",), (3,)),
        ("fit", "ETH/USDT", "4h", {"family": "kmeans", "k": 3}, "is"),
        ("economic", "ETH/USDT", "4h", "ETH/USDT-4h"),
    ]


def test_no_bakeoff_winner_fails_closed_and_skips_economic_gate():
    economic_calls = []

    def fake_bakeoff(*args, **kwargs):
        return _bakeoff(winner=None) | {"winner": None}

    def fake_economic(**kwargs):
        economic_calls.append(kwargs)
        return {"summary": {"pass": True, "blocking_reasons": []}}

    report = m.run_multi_asset_gate(
        datasets=[("BTC/USDT", "1h")],
        min_pass_cells=1,
        bakeoff_fn=fake_bakeoff,
        economic_gate_fn=fake_economic,
    )

    assert report["summary"]["pass"] is False
    assert economic_calls == []
    assert report["rows"][0]["blocking_reasons"] == ["no #1080 gate-passing model"]
    assert "no #1080 gate-passing model" in report["summary"]["blocking_reasons"][1]


def test_cell_exception_is_reported_not_silently_skipped():
    def fake_bakeoff(*args, **kwargs):
        raise ValueError("no cached data")

    report = m.run_multi_asset_gate(
        datasets=[("SOL/USDT", "4h")],
        min_pass_cells=1,
        bakeoff_fn=fake_bakeoff,
    )

    assert report["summary"]["pass"] is False
    assert report["rows"][0]["error"] == "ValueError: no cached data"
    assert any("SOL/USDT 4h: ValueError: no cached data" == r
               for r in report["summary"]["blocking_reasons"])


def test_economic_failure_blocks_aggregate_even_with_model_winner():
    def fake_bakeoff(*args, **kwargs):
        return _bakeoff()

    def fake_fit(*args, **kwargs):
        return {"model_id": "m"}

    def fake_economic(**kwargs):
        return {
            "summary": {
                "pass": False,
                "blocking_reasons": ["model/tiered_tp/oos: candidate Sharpe does not beat"],
            },
            "rows": [],
        }

    report = m.run_multi_asset_gate(
        datasets=[("BTC/USDT", "1h")],
        min_pass_cells=1,
        bakeoff_fn=fake_bakeoff,
        fit_model_fn=fake_fit,
        economic_gate_fn=fake_economic,
    )

    assert report["summary"]["pass"] is False
    assert report["rows"][0]["pass"] is False
    assert any("candidate Sharpe" in r for r in report["summary"]["blocking_reasons"])
    text = m.format_report(report)
    assert "BTC/USDT 1h" in text
    assert "kmeans:3" in text
