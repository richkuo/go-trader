"""Integration checks for the #1940 regime-classifier calibration."""
import hashlib
import importlib.util
import socket
import sqlite3
from pathlib import Path

import numpy as np
import pandas as pd
import pytest

from regime_stats import benjamini_hochberg

_PATH = Path(__file__).resolve().parents[1] / "research" / "regime_1940_classifier_calibration.py"
_SPEC = importlib.util.spec_from_file_location("regime_1940_classifier_calibration", _PATH)
study = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(study)

_CANDLES = Path(__file__).resolve().parents[1] / "testdata" / "ledger_export" / "regime_market" / "BTC_1h_candles.csv.gz"
_PROTOCOL = Path(__file__).resolve().parents[2] / "docs" / "research" / "1940-artifacts" / "protocol.json"
_GOLDEN = "2d9c44a194e0c2df29e62291439772d2c0fd1ebdf35a5cf434c48bf8e49dda1e"


def _canon(labels, feats) -> str:
    lines = []
    for row in feats:
        parts = ["nan" if np.isnan(v) else f"{v:.10g}" for v in row]
        lines.append(",".join(parts))
    blob = "labels\n" + "|".join(labels) + "\nfeats\n" + "\n".join(lines)
    return hashlib.sha256(blob.encode()).hexdigest()


def test_default_cap_matches_recorded_classifier_output():
    from regime import compute_regime_composite, composite_feature_matrix
    frame = pd.read_csv(_CANDLES)
    labels = compute_regime_composite(frame, period=48)["regime"].tolist()
    explicit = compute_regime_composite(frame, period=48, adx_period_cap=14)["regime"].tolist()
    feats = composite_feature_matrix(frame, 48).to_numpy()
    feats_explicit = composite_feature_matrix(frame, 48, adx_period_cap=14).to_numpy()
    assert labels == explicit
    assert np.array_equal(np.isnan(feats), np.isnan(feats_explicit))
    np.testing.assert_allclose(np.nan_to_num(feats), np.nan_to_num(feats_explicit))
    assert _canon(labels, feats) == _GOLDEN


def test_uncapped_need_uses_the_full_period_and_capped_need_uses_the_helper():
    from regime import CLASSIFIER_COMPOSITE, required_regime_bars
    assert study._composite_ready_need(60, 14) == required_regime_bars(60, CLASSIFIER_COMPOSITE)
    assert study._composite_ready_need(60, None) == 2 * 60 + 1
    frame = _price_frame(180, seed=3)
    capped = study.composite_feature_matrix(frame, 60, adx_period_cap=14).to_numpy()
    free = study.composite_feature_matrix(frame, 60, adx_period_cap=None).to_numpy()
    assert np.isfinite(capped).all(axis=1).argmax() == study._composite_ready_need(60, 14) - 1
    assert np.isfinite(free).all(axis=1).argmax() == 120


def test_protocol_hash_is_ready_before_any_score():
    scored = {}

    def execute(begun, args):
        scored["sha"] = begun["sha256"]
        scored["settings"] = len(begun["expanded"]["setting_ids"])
        return 0

    code = study.run_study(["--protocol", str(_PROTOCOL), "--snapshot", "/tmp/unused.sqlite"], execute_fn=execute)
    assert code == 0
    assert scored["sha"] == hashlib.sha256(_PROTOCOL.read_bytes()).hexdigest()
    assert scored["settings"] == 1320
    assert study.begin_study(_PROTOCOL)["expanded"]["family_size"] == 158400


def test_short_permutation_total_is_refused():
    with pytest.raises(ValueError, match="n_perm"):
        study.resolve_n_perm(158400, 0.05, 10)


def test_loader_defects_and_read_only_cache(tmp_path):
    cache = tmp_path / "cache"
    cache.mkdir()
    marker = cache / "keep.txt"
    marker.write_text("same")
    before = sorted(p.name for p in cache.iterdir()), marker.read_bytes()
    db = tmp_path / "snap.sqlite"
    _write_ohlcv(db, _hourly(0, 10))
    conn = study.open_snapshot(db)
    try:
        start, end = 0, 10 * 3_600_000
        assert study.candle_defect(study.read_candles(conn, "ex", "SYM", "1h", start, end), start, end, 3_600_000) is None
        assert study.describe_input([], "ex", "SYM", "1h", start, end)["reason"] == "missing"
        truncated = study.read_candles(conn, "ex", "SYM", "1h", start, end + 5 * 3_600_000)
        assert study.candle_defect(truncated, start, end + 5 * 3_600_000, 3_600_000) == "truncated"
    finally:
        conn.close()
    dup = _hourly(0, 6)
    dup.append(dup[-1])
    assert study.candle_defect(dup, 0, 6 * 3_600_000, 3_600_000) == "duplicate"
    gapped = _hourly(0, 8)
    del gapped[3]
    assert study.candle_defect(gapped, 0, 8 * 3_600_000, 3_600_000) == "gapped"
    original = socket.socket.connect

    def blocked(*_a, **_k):
        raise AssertionError("network")

    socket.socket.connect = blocked
    try:
        again = study.open_snapshot(db)
        again.close()
    finally:
        socket.socket.connect = original
    assert (sorted(p.name for p in cache.iterdir()), marker.read_bytes()) == before


def test_support_stays_inside_the_split_and_2025h1_drops_the_is_overlap():
    begun = study.begin_study(_PROTOCOL)
    assert begun["protocol"]["splits"]["2025H1"][1] == begun["protocol"]["splits"]["is"][0]
    frame = _price_frame(220, seed=4)
    bank = study.build_label_bank(frame, [
        {"setting_id": "a", "classifier": "adx", "period": 10, "adx_threshold": 20},
        {"setting_id": "b", "classifier": "composite", "period": 60, "return_eff": 0.05,
         "range_eff": 0.03, "efficiency": 0.5, "adx_threshold": 25, "cap": None},
    ])
    fwd = study.forward_realized_vol(bank["close"], 12)
    mask = bank["base"] & np.isfinite(fwd)
    assert study.forward_window_inside(mask, 12)
    assert int(np.flatnonzero(mask)[0]) >= 120
    changed = bank["close"].copy()
    changed[-30:] += 50
    fwd_changed = study.forward_realized_vol(changed, 12)
    assert np.array_equal(np.isfinite(fwd[:190]), np.isfinite(fwd_changed[:190]))


def test_held_out_rows_do_not_change_the_training_winner():
    incumbent = _row("inc", h=10, p=0.01, transition=0.2, support="s")
    candidate = _row("cand", h=12, p=0.01, transition=0.1, support="s")
    oos = _row("cand", h=1, p=0.5, transition=0.5, support="s", split="oos", horizon=4)
    first = study.select_winners([incumbent, candidate, oos], {"adx": "inc"})
    oos["kruskal_h"] = 999
    second = study.select_winners([incumbent, candidate, oos], {"adx": "inc"})
    assert first == second
    assert first[0]["setting_id"] == "cand"


def test_tie_breaks_on_the_setting_identifier():
    incumbent = _row("inc", h=10, p=0.01, transition=0.4, support="s")
    low = _row("b", h=20, p=0.01, transition=0.1, support="s")
    high = _row("a", h=20, p=0.01, transition=0.1, support="s")
    winners = study.select_winners([incumbent, low, high], {"adx": "inc"})
    assert winners[0]["setting_id"] == "a"


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


def test_degenerate_label_stream_is_not_selected():
    incumbent = _row("inc", h=10, p=0.01, transition=0.2, support="s")
    degenerate = _row("one", h=0, p=1, transition=0.0, support="s")
    winners = study.select_winners([incumbent, degenerate], {"adx": "inc"})
    assert winners[0]["setting_id"] is None


def test_adjusted_p_values_match_the_helper_and_an_unresolved_limit_keeps_units_apart():
    rng = np.random.default_rng(0)
    pvals = rng.random(12).tolist()
    flags = benjamini_hochberg(pvals, alpha=0.05, family_size=40)
    adj = study.adjusted_p_values(pvals, 40)
    assert flags == [bool(v <= 0.05) for v in adj]
    text = study.render_report(_doc([], power=[{
        "dataset_id": "ds", "classifier": "composite", "horizon": 12, "status": "unresolved",
        "bound_multiplier": 2.0, "bound_power": 0.1, "bound_kruskal_h": 4.5,
    }]))
    assert "detection limit unresolved" in text
    assert "largest tested multiplier is 2.0" in text
    assert "Kruskal-Wallis H at that multiplier is 4.5" in text
    again = study.render_report(_doc([], power=[{
        "dataset_id": "ds", "classifier": "composite", "horizon": 12, "status": "unresolved",
        "bound_multiplier": 2.0, "bound_power": 0.1, "bound_kruskal_h": 4.5,
    }]))
    assert again == text


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


def _write_ohlcv(path, rows):
    conn = sqlite3.connect(path)
    conn.execute("CREATE TABLE ohlcv (timestamp INTEGER, open REAL, high REAL, low REAL, close REAL, volume REAL, exchange TEXT, symbol TEXT, timeframe TEXT)")
    conn.executemany("INSERT INTO ohlcv VALUES (?,?,?,?,?,?,?,?,?)",
                     [(ts, o, h, l, c, v, "ex", "SYM", "1h") for ts, o, h, l, c, v in rows])
    conn.commit()
    conn.close()


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
