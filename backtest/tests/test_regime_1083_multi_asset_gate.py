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
    assert any("no #1080 gate-passing model" in r
               for r in report["summary"]["blocking_reasons"])
    # A no-winner cell ran the methodology on real data -> a genuine failure,
    # not an operationally-inconclusive (data-gap) cell.
    assert report["summary"]["failed_cells"] == 1
    assert report["summary"]["inconclusive_cells"] == 0


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
    assert any("SOL/USDT 4h: ValueError: no cached data" in r
               for r in report["summary"]["blocking_reasons"])
    # An exception (here a data gap) is inconclusive, not a methodology failure.
    assert report["summary"]["inconclusive_cells"] == 1
    assert report["summary"]["failed_cells"] == 0


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


def _summary_row(dataset, passed, reason="economic gate failed"):
    row = {"dataset": dataset, "pass": passed, "blocking_reasons": []}
    if not passed:
        row["blocking_reasons"] = [reason]
    return row


def test_min_pass_cells_tolerates_k_of_n_failures():
    # Must survive (1): exactly min_pass_cells pass, the rest fail -> pass.
    rows = [
        _summary_row("A", True),
        _summary_row("B", True),
        _summary_row("C", True),
        _summary_row("D", False),
    ]
    s = m.summarize(rows, min_pass_cells=3)
    assert s["pass"] is True
    assert s["passed_cells"] == 3
    assert s["total_cells"] == 4
    # The single failing cell does NOT block, but stays visible as a diagnostic.
    assert s["blocking_reasons"] == []
    assert any("D: economic gate failed" in d for d in s["cell_diagnostics"])


def test_min_pass_cells_minus_one_blocks_with_cell_reasons():
    # Must survive (2): min_pass_cells - 1 pass -> block, with the actionable
    # failing-cell reasons promoted into the blocking list.
    rows = [
        _summary_row("A", True),
        _summary_row("B", True),
        _summary_row("C", False),
        _summary_row("D", False, reason="no #1080 gate-passing model"),
    ]
    s = m.summarize(rows, min_pass_cells=3)
    assert s["pass"] is False
    assert s["passed_cells"] == 2
    assert any("passed cells 2 < required 3" == b for b in s["blocking_reasons"])
    assert any("C: economic gate failed" in b for b in s["blocking_reasons"])
    assert any("D: no #1080 gate-passing model" in b for b in s["blocking_reasons"])


def test_all_cells_pass_meets_floor():
    # Must survive (3): all cells pass with total >= min -> pass.
    rows = [_summary_row("A", True), _summary_row("B", True), _summary_row("C", True)]
    s = m.summarize(rows, min_pass_cells=2)
    assert s["pass"] is True
    assert s["blocking_reasons"] == []
    assert s["cell_diagnostics"] == []


def test_min_pass_cells_above_total_is_a_hard_floor():
    # Must survive (4): min_pass_cells > total datasets -> block as a floor,
    # even when every present cell passes.
    rows = [_summary_row("A", True), _summary_row("B", True)]
    s = m.summarize(rows, min_pass_cells=3)
    assert s["pass"] is False
    assert s["passed_cells"] == 2
    assert any("passed cells 2 < required 3" == b for b in s["blocking_reasons"])


def test_run_multi_asset_gate_promotes_on_breadth_with_a_failing_cell():
    # End-to-end: 2 of 3 cells clear at min_pass_cells=2. Before the fix the
    # failing SOL cell vetoed promotion regardless of the breadth threshold.
    def fake_bakeoff(symbol, timeframe, **kwargs):
        return _bakeoff()

    def fake_fit(*args, **kwargs):
        return {"model_id": "m", "states": ["a", "b"], "mapping": {}}

    def fake_economic(**kwargs):
        ok = kwargs["symbol"] != "SOL/USDT"
        return {
            "summary": {
                "pass": ok,
                "blocking_reasons": [] if ok else ["oos: candidate Sharpe does not beat"],
            },
            "rows": [],
        }

    report = m.run_multi_asset_gate(
        datasets=[("BTC/USDT", "1h"), ("ETH/USDT", "4h"), ("SOL/USDT", "4h")],
        min_pass_cells=2,
        bakeoff_fn=fake_bakeoff,
        fit_model_fn=fake_fit,
        economic_gate_fn=fake_economic,
    )

    s = report["summary"]
    assert s["pass"] is True
    assert s["passed_cells"] == 2
    assert s["total_cells"] == 3
    assert s["blocking_reasons"] == []
    assert any("SOL/USDT 4h" in d for d in s["cell_diagnostics"])


def test_iterable_params_are_materialized_for_multi_cell_runs():
    # A one-shot generator for families/k_range must survive every cell and the
    # report metadata, not empty after the first cell.
    seen_families = []

    def fake_bakeoff(symbol, timeframe, **kwargs):
        seen_families.append(tuple(kwargs["families"]))
        return _bakeoff()

    def fake_fit(*args, **kwargs):
        return {"model_id": "m", "states": [], "mapping": {}}

    def fake_economic(**kwargs):
        return {"summary": {"pass": True, "blocking_reasons": []}, "rows": []}

    report = m.run_multi_asset_gate(
        datasets=[("BTC/USDT", "1h"), ("ETH/USDT", "4h")],
        min_pass_cells=2,
        families=(f for f in ("kmeans", "gmm")),
        k_range=(k for k in (3, 4)),
        bakeoff_fn=fake_bakeoff,
        fit_model_fn=fake_fit,
        economic_gate_fn=fake_economic,
    )

    assert seen_families == [("kmeans", "gmm"), ("kmeans", "gmm")]
    assert report["families"] == ["kmeans", "gmm"]
    assert report["k_range"] == [3, 4]
    assert report["summary"]["pass"] is True


def test_main_rejects_unknown_family_up_front():
    # An unknown family is rejected once, before any cell runs (no data load).
    with pytest.raises(SystemExit, match="unknown families"):
        m.main(["--datasets", "BTC/USDT:1h", "--families", "hmm,kmean"])


def test_main_accepts_valid_families_and_threads_them(monkeypatch):
    captured = {}

    def fake_run(**kwargs):
        captured.update(kwargs)
        return {
            "strategy": kwargs.get("strategy", "s"),
            "registry": kwargs.get("registry", "spot"),
            "rows": [],
            "summary": {
                "pass": True,
                "blocking_reasons": [],
                "passed_cells": 0,
                "total_cells": 0,
                "min_pass_cells": 0,
            },
        }

    monkeypatch.setattr(m, "run_multi_asset_gate", fake_run)
    rc = m.main(["--datasets", "BTC/USDT:1h", "--families", "hmm,gmm"])
    assert rc == 0
    assert captured["families"] == ["hmm", "gmm"]


def _cell(symbol, timeframe, *, passed, error=None, reason="economic gate failed"):
    row = {
        "symbol": symbol,
        "timeframe": timeframe,
        "dataset": f"{symbol} {timeframe}",
        "pass": passed,
        "blocking_reasons": [],
    }
    if error is not None:
        row["error"] = error
        row["blocking_reasons"] = [error]
    elif not passed:
        row["blocking_reasons"] = [reason]
    return row


def test_cross_asset_breadth_blocks_single_symbol_even_with_enough_cells():
    # Three BTC timeframes pass but the result is BTC-specific: a regime model
    # promoted to run on every asset must demonstrate cross-asset breadth.
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("BTC/USDT", "4h", passed=True),
        _cell("BTC/USDT", "15m", passed=True),
        _cell("ETH/USDT", "1h", passed=False),
    ]
    s = m.summarize(rows, min_pass_cells=3)
    assert s["pass"] is False  # cell floor met (3), but only 1 symbol passes
    assert s["passed_cells"] == 3
    assert s["passing_symbols"] == ["BTC/USDT"]
    assert s["required_pass_symbols"] == 2  # min(2, 2 panel symbols)
    assert any("passing symbols 1" in b for b in s["blocking_reasons"])


def test_cross_asset_breadth_passes_when_passes_span_symbols():
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("BTC/USDT", "4h", passed=True),
        _cell("ETH/USDT", "1h", passed=True),
        _cell("SOL/USDT", "4h", passed=False),
    ]
    s = m.summarize(rows, min_pass_cells=3)
    assert s["pass"] is True
    assert set(s["passing_symbols"]) == {"BTC/USDT", "ETH/USDT"}
    assert s["blocking_reasons"] == []


def test_inconclusive_cells_do_not_erode_breadth_but_cannot_substitute_for_passes():
    # Two assets pass; a third cell can't be evaluated (data gap). The gap is an
    # unknown, so it neither blocks nor counts as a pass.
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("ETH/USDT", "1h", passed=True),
        _cell("SOL/USDT", "1h", passed=False, error="ValueError: no cached data"),
    ]
    s = m.summarize(rows, min_pass_cells=2)
    assert s["pass"] is True
    assert s["passed_cells"] == 2
    assert s["failed_cells"] == 0
    assert s["inconclusive_cells"] == 1
    # ...but an inconclusive cell can never make up the difference toward the floor.
    s2 = m.summarize(rows, min_pass_cells=3)
    assert s2["pass"] is False
    assert any("passed cells 2 < required 3" == b for b in s2["blocking_reasons"])


def test_inconclusive_only_panel_fails_closed():
    # A panel where nothing could be evaluated must never promote.
    rows = [
        _cell("BTC/USDT", "1h", passed=False, error="ValueError: no cached data"),
        _cell("ETH/USDT", "1h", passed=False, error="ValueError: no cached data"),
    ]
    s = m.summarize(rows, min_pass_cells=1)
    assert s["pass"] is False
    assert s["passed_cells"] == 0
    assert s["inconclusive_cells"] == 2
    assert s["failed_cells"] == 0


def test_min_pass_symbols_explicit_floor_above_panel_blocks():
    # An explicit symbol floor exceeding the panel's symbol count is
    # unsatisfiable -> hard block, mirroring min_pass_cells > total.
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("ETH/USDT", "1h", passed=True),
    ]
    s = m.summarize(rows, min_pass_cells=2, min_pass_symbols=3)
    assert s["pass"] is False
    assert s["required_pass_symbols"] == 3
    assert any("< required 3" in b for b in s["blocking_reasons"])


def test_min_pass_symbols_override_relaxes_for_narrow_panel():
    # A deliberately single-symbol run can opt out of cross-asset breadth.
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("BTC/USDT", "4h", passed=True),
    ]
    s = m.summarize(rows, min_pass_cells=2, min_pass_symbols=1)
    assert s["pass"] is True
    assert s["passing_symbols"] == ["BTC/USDT"]


def test_format_report_surfaces_breadth_and_symbol_lines():
    rows = [
        _cell("BTC/USDT", "1h", passed=True),
        _cell("ETH/USDT", "1h", passed=True),
        _cell("SOL/USDT", "1h", passed=False, error="ValueError: no cached data"),
    ]
    report = {
        "strategy": "s",
        "registry": "spot",
        "rows": rows,
        "summary": m.summarize(rows, min_pass_cells=2),
    }
    text = m.format_report(report)
    assert "2/3 cells passed" in text
    assert "inconclusive" in text
    assert "symbols:" in text
