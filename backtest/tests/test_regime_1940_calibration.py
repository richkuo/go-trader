"""Integration checks for the #1940 regime-classifier calibration."""
import hashlib
import importlib.util
from pathlib import Path

import numpy as np
import pandas as pd
import pytest


_PATH = Path(__file__).resolve().parents[1] / "research" / "regime_1940_classifier_calibration.py"
_SPEC = importlib.util.spec_from_file_location("regime_1940_classifier_calibration", _PATH)
study = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(study)

_CANDLES = Path(__file__).resolve().parents[1] / "testdata" / "ledger_export" / "regime_market" / "BTC_1h_candles.csv.gz"


def _canon(labels, feats) -> str:
    lines = []
    for row in feats:
        parts = ["nan" if np.isnan(v) else f"{v:.10g}" for v in row]
        lines.append(",".join(parts))
    blob = "labels\n" + "|".join(labels) + "\nfeats\n" + "\n".join(lines)
    return hashlib.sha256(blob.encode()).hexdigest()


def test_uncapped_need_uses_the_full_period_and_capped_need_uses_the_helper():
    from regime import CLASSIFIER_COMPOSITE, required_regime_bars
    assert study._composite_ready_need(60, 14) == required_regime_bars(60, CLASSIFIER_COMPOSITE)
    assert study._composite_ready_need(60, None) == 2 * 60 + 1
    frame = _price_frame(180, seed=3)
    capped = study.composite_feature_matrix(frame, 60, adx_period_cap=14).to_numpy()
    free = study.composite_feature_matrix(frame, 60, adx_period_cap=None).to_numpy()
    assert np.isfinite(capped).all(axis=1).argmax() == study._composite_ready_need(60, 14) - 1
    assert np.isfinite(free).all(axis=1).argmax() == 120


def test_short_permutation_total_is_refused():
    with pytest.raises(ValueError, match="n_perm"):
        study.resolve_n_perm(158400, 0.05, 10)


def test_raw_pass_with_corrected_failure_is_not_suggested():
    incumbent = _row("inc", h=10, p=0.2, transition=0.3, support="s", split="oos", horizon=4)
    candidate = _row("cand", h=12, p=0.01, transition=0.1, support="s", split="oos", horizon=4)
    candidate["corrected_pass"] = False
    candidate["p_adjusted"] = 0.4
    candidate["bh_reject"] = False
    decision = study.decide_suggestions(
        [{"dataset_id": "ds", "classifier": "adx", "setting_id": "cand", "incumbent_id": "inc"}],
        [incumbent, candidate], 0.05, ["oos"], [4],
    )
    assert decision[0]["suggested"] is False
    assert decision[0]["raw_pass_corrected_fail"] == 1
    text = study.render_report(_doc(decision, power=[{
        "dataset_id": "ds", "classifier": "adx", "horizon": 4, "status": "unresolved",
        "bound_multiplier": 2.0, "bound_power": 0.4, "bound_kruskal_h": 3.0,
    }]))
    assert "suggest-only" in text
    assert "raw gate passed and Benjamini-Hochberg correction failed" in text
    assert "multiplier is not Kruskal-Wallis H" in text
    assert "KW-H = 2" not in text


def test_exact_half_and_empty_held_out_are_refused_and_a_strict_majority_passes():
    rows = []
    for split, horizon, passes in (("oos", 4, True), ("oos", 12, True), ("2024", 4, False), ("2024", 12, False)):
        rows.append(_row("inc", h=10, p=0.2, transition=0.3, support="s", split=split, horizon=horizon))
        cand = _row("cand", h=12, p=0.01, transition=0.1 if passes else 0.3, support="s", split=split, horizon=horizon)
        cand["corrected_pass"] = True
        cand["p_adjusted"] = 0.01
        cand["bh_reject"] = True
        rows.append(cand)
    winner = [{"dataset_id": "ds", "classifier": "adx", "setting_id": "cand", "incumbent_id": "inc"}]
    half = study.decide_suggestions(winner, rows, 0.05, ["oos", "2024"], [4, 12])
    assert half[0]["held_out_passing"] == 2 and half[0]["held_out_available"] == 4
    assert half[0]["suggested"] is False
    rows[7]["transition_rate"] = 0.1
    more = study.decide_suggestions(winner, rows, 0.05, ["oos", "2024"], [4, 12])
    assert more[0]["held_out_passing"] == 3 and more[0]["held_out_available"] == 4
    assert more[0]["suggested"] is True
    empty = study.decide_suggestions(
        [{"dataset_id": "ds", "classifier": "adx", "setting_id": None, "incumbent_id": "inc"}],
        [], 0.05, ["oos"], [4],
    )
    assert empty[0]["suggested"] is False
    assert empty[0]["reason"] == "no training winner"


def test_explicit_diagnostics_reject_conflicts_and_keep_the_default_payload_shape(monkeypatch):
    import data_fetcher
    from regime_diagnostics import run_window, _OMITTED
    seen = {}

    def fake_load(symbol, timeframe, exchange_id="binanceus", start_date=None, end_date=None):
        seen["exchange"] = exchange_id
        return _price_frame(80, seed=1)

    monkeypatch.setattr(data_fetcher, "load_cached_data", fake_load)
    default = run_window("BTC/USDT", "1h", "is", horizons=(4,), n_perm=20)
    assert "h4" in default and "inputs" not in default
    explicit = run_window("BTC/USDT", "1h", "is", horizons=(4,), n_perm=20, classifier="adx",
                          period=10, adx_threshold=15, exchange="hyperliquid")
    assert seen["exchange"] == "hyperliquid"
    assert explicit["h4"]["separation"]["kruskal_h"] >= 0
    with pytest.raises(ValueError, match="conflicting model"):
        run_window("BTC/USDT", "1h", "is", model={"period": 48, "states": []}, classifier="adx")
    with pytest.raises(ValueError, match="period"):
        run_window("BTC/USDT", "1h", "is", period=1)
    with pytest.raises(ValueError, match="adx_period_cap"):
        run_window("BTC/USDT", "1h", "is", classifier="adx", adx_period_cap=14)
    assert _OMITTED is not None


def test_resumed_cells_restore_support_for_confirmation(monkeypatch):
    import multiprocessing as mp
    cell = {"cell_id": "ds|oos|4", "dataset_id": "ds", "split": "oos", "horizon": 4}
    saved = _row("inc", h=2, p=0.01, transition=0.1, support="s", split="oos")
    monkeypatch.setattr(study, "_load_checkpoint", lambda *_: {"scores": [saved], "population": {"status": "available"}})
    monkeypatch.setattr(study, "frame_from_rows", lambda _: pd.DataFrame({"close": np.arange(20.0)}))
    monkeypatch.setattr(study, "build_label_bank", lambda *_: {"base": np.ones(20, dtype=bool), "close": np.arange(20.0)})
    monkeypatch.setattr(study, "forward_realized_vol", lambda close, h: np.ones(len(close)))
    monkeypatch.setattr(study, "forward_window_inside", lambda *_: True)
    captured = {}
    monkeypatch.setattr(study, "_refine_confirmation", lambda *args: captured.update(args[-1]))

    class Pool:
        def close(self): pass
        def join(self): pass

    class Context:
        def Pool(self, *args, **kwargs): return Pool()

    monkeypatch.setattr(mp, "get_context", lambda _: Context())
    from types import SimpleNamespace
    args = SimpleNamespace(workers=1)
    loaded = {"ds|oos": {"meta": {"status": "available"}, "rows": _hourly(0, 20)}}
    protocol = {"splits": {"oos": []}, "horizons": [4], "min_usable_bars": 1}
    expanded = {"datasets": [{"dataset_id": "ds"}], "cells": [cell], "settings": [], "incumbent_ids": {"adx": "inc"}}
    study._score_all(loaded, protocol, expanded, args, Path("unused"))
    assert np.array_equal(captured[cell["cell_id"]], np.arange(20))


def test_mixed_resumed_and_fresh_cells_both_restore_support(monkeypatch):
    import multiprocessing as mp
    from types import SimpleNamespace
    cells = [{"cell_id": f"ds|oos|{h}", "dataset_id": "ds", "split": "oos", "horizon": h} for h in (4, 12)]
    saved = _row("inc", h=2, p=0.01, transition=0.1, support="s", split="oos")
    monkeypatch.setattr(study, "_load_checkpoint", lambda _, cell_id: {"scores": [saved], "population": {"status": "available"}} if cell_id.endswith("|4") else None)
    monkeypatch.setattr(study, "_save_checkpoint", lambda *_: None)
    monkeypatch.setattr(study, "frame_from_rows", lambda _: pd.DataFrame({"close": np.arange(20.0)}))
    bank_entry = {"labels": np.where(np.arange(20) % 2, "high", "low"), "known": ("low", "high")}
    monkeypatch.setattr(study, "build_label_bank", lambda *_: {"base": np.ones(20, dtype=bool), "close": np.arange(20.0), "bank": {"inc": bank_entry}})
    monkeypatch.setattr(study, "forward_realized_vol", lambda close, h: np.arange(len(close), dtype=float))
    monkeypatch.setattr(study, "forward_window_inside", lambda *_: True)
    monkeypatch.setattr(study, "score_cell_stats", lambda *_: {"block_len": 4, "kruskal_h": 2.0, "k": 2, "degenerate": False,
        "transition_rate": 0.1, "flips": 1, "mean_dwell": 2.0, "coverage": 1.0, "mean_vol": 1.0,
        "median_vol": 1.0, "order_by_mean": True, "order_by_median": True})
    captured = {}
    monkeypatch.setattr(study, "_refine_confirmation", lambda *args: captured.update(args[-1]))

    class Pool:
        def map(self, fn, tasks):
            return [[{"ge": 0, "executed": 2, "h_obs": 2.0, "kind": "screen", "p_value": 0.5} for _ in task["items"]] for task in tasks]
        def close(self): pass
        def join(self): pass

    class Context:
        def Pool(self, *args, **kwargs): return Pool()

    monkeypatch.setattr(mp, "get_context", lambda _: Context())
    loaded = {"ds|oos": {"meta": {"status": "available"}, "rows": _hourly(0, 20)}}
    protocol = {"splits": {"oos": []}, "horizons": [4, 12], "min_usable_bars": 1, "block_dwell_multiplier": 1,
                "seed": 1, "n_perm": 2, "screen_n": 2, "screen_continue_if_p_at_most": 1.0}
    expanded = {"datasets": [{"dataset_id": "ds"}], "cells": cells, "settings": [{"setting_id": "inc", "classifier": "adx"}], "incumbent_ids": {"adx": "inc"}}
    study._score_all(loaded, protocol, expanded, SimpleNamespace(workers=1), Path("unused"))
    assert all(np.array_equal(captured[cell["cell_id"]], np.arange(20)) for cell in cells)


def _price_frame(n, seed):
    rng = np.random.default_rng(seed)
    close = 100 * np.exp(np.cumsum(rng.normal(0, 0.01, n)))
    return pd.DataFrame({
        "open": close, "high": close * 1.01, "low": close * 0.99, "close": close, "volume": np.ones(n),
    })


def _hourly(start_hour, count):
    rows = []
    for i in range(count):
        ts = (start_hour + i) * 3_600_000
        px = 100 + i
        rows.append((ts, px, px + 1, px - 1, px, 1.0))
    return rows


def _row(setting, h, p, transition, support, split="is", horizon=4):
    return {
        "status": "available", "setting_id": setting, "classifier": "adx", "dataset_id": "ds",
        "split": split, "horizon": horizon, "kruskal_h": h, "p_value": p,
        "transition_rate": transition, "support_sha256": support,
    }


def _doc(suggestions, power):
    return {
        "protocol_sha256": "abc", "family_size": 10, "n_perm": 100, "screen_n": 10,
        "screen_continue_if_p_at_most": 0.08, "alpha": 0.05, "suggestions": suggestions,
        "inputs": [], "adx_highlights": [], "comparisons": [], "confusion": [],
        "sensitivity": [], "cap_agreement": [], "asset_notes": [], "dwell_highlights": [],
        "order_highlights": [], "power": power, "nulls": [],
    }
