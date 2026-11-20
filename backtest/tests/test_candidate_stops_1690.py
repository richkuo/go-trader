import json
import math
import os
import subprocess
import sys

import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
AUTO_SUGGEST = os.path.join(REPO, "backtest", "auto_suggest.py")
DATASET = "BTC/USDT:4h"
WINDOW = "2023"

INCUMBENT = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [
    {"atr_multiple": 0.5, "close_fraction": 0.5},
    {"atr_multiple": 1.0, "close_fraction": 1.0}]}}]
WIDE_TP = [{"name": "tiered_tp_atr", "params": {"tp_tiers": [
    {"atr_multiple": 1.5, "close_fraction": 0.5},
    {"atr_multiple": 3.0, "close_fraction": 1.0}]}}]
STOP_ONLY_REASONS = {"sl", "end_of_data"}


def _frozen_candles() -> pd.DataFrame:
    start = pd.Timestamp("2022-12-01")
    rows, prev = [], None
    for i in range(2600):
        close = 100.0 * math.exp(0.18 * math.sin(2 * math.pi * i / 170.0)
                                 + 0.06 * math.sin(2 * math.pi * i / 41.0)
                                 + 0.02 * math.sin(2 * math.pi * i / 9.0))
        open_ = prev if prev is not None else close
        wick = 0.006 + 0.004 * abs(math.sin(i / 5.0))
        rows.append({"timestamp": int((start + pd.Timedelta(hours=4 * i)).timestamp() * 1000),
                     "open": open_, "high": max(open_, close) * (1 + wick),
                     "low": min(open_, close) * (1 - wick), "close": close,
                     "volume": 1000.0})
        prev = close
    return pd.DataFrame(rows)


@pytest.fixture(scope="module")
def candle_env(tmp_path_factory):
    import storage
    db = str(tmp_path_factory.mktemp("ohlcv_1690") / "ohlcv.db")
    candles = _frozen_candles()
    storage.store_ohlcv(candles, "binanceus", "BTC/USDT", "4h", db_path=db)
    step = 3600 * 1000
    first = int(candles["timestamp"].iloc[0]) - 24 * step
    last = int(candles["timestamp"].iloc[-1]) + 24 * step
    hours = list(range(first, last, step))
    storage.store_funding_fetch("hyperliquid", "BTC", [{"time": h, "rate": 1.25e-05} for h in hours], [],
                                db_path=db)
    storage.store_funding_coverage("hyperliquid", "BTC", hours[0], hours[-1] + step, db_path=db)
    env = dict(os.environ)
    env["GO_TRADER_OHLCV_CACHE_DB"] = db
    return env


def _auto_suggest(env, tmp_path, spec, *extra):
    spec_path = tmp_path / "suggest.json"
    spec_path.write_text(json.dumps(spec))
    out_dir = tmp_path / "runs"
    report = tmp_path / "report.json"
    cmd = [sys.executable, AUTO_SUGGEST, "--spec", str(spec_path), "--windows", WINDOW,
           "--datasets", DATASET, "--bootstrap-resamples", "200",
           "--out-dir", str(out_dir), "--json", str(report), *extra]
    proc = subprocess.run(cmd, cwd=REPO, env=env, capture_output=True, text=True,
                          timeout=900)
    payload = json.loads(report.read_text()) if report.exists() else None
    return proc, payload, out_dir


def _m6_payload(out_dir, key):
    with open(out_dir / f"{key}.m6.json") as fh:
        return json.load(fh)


def _dataset(payload):
    results = payload["results"][WINDOW]
    assert len(results) == 1
    return results[0]


def _spec(m6):
    return {"study": "stops_1690", "registry": "spot", "harnesses": ["m6"],
            "correction": {"method": "benjamini_hochberg", "alpha": 0.05},
            "m6": m6}


def test_baseline_config_short_leg_replaces_inherited_percent_stop(candle_env, tmp_path):
    cfg = {"config_version": 16, "strategies": [{
        "id": "hl-sma-btc", "type": "perps", "platform": "hyperliquid",
        "open_strategy": {"name": "sma_crossover"},
        "close_strategy": INCUMBENT[0],
        "direction": "short", "stop_loss_pct": 4.0, "leverage": 2}]}
    cfg_path = tmp_path / "config.json"
    cfg_path.write_text(json.dumps(cfg))
    spec = _spec({
        "strategy_id": "hl-sma-btc",
        "baseline_config": str(cfg_path),
        "candidate_close_variants": [
            {"key": "atr_trail", "candidate_close": [],
             "candidate_stops": {"trailing_stop_atr_mult": 2.0}},
            {"key": "tp_trail", "candidate_close": WIDE_TP,
             "candidate_stops": {"trailing_stop_atr_mult": 2.0}},
            {"key": "tp_inherit", "candidate_close": WIDE_TP},
        ],
    })
    spec["registry"] = "futures"
    proc, report, out_dir = _auto_suggest(candle_env, tmp_path, spec)
    assert proc.returncode == 0, proc.stderr[-3000:]
    assert report["close_validation"]["requested_set_complete"] is True

    controls = []
    for key, stop_only in (("m6.atr_trail", True), ("m6.tp_trail", False)):
        payload = _m6_payload(out_dir, key)
        assert payload["open"]["direction"] == "short"
        assert payload["control_stop_owner"] == "fixed_pct"
        assert payload["candidate_stop_owner"] == "trailing_atr"
        assert payload["candidate_stop_only"] is stop_only
        assert payload["control_stops"]["stop_loss_pct"] == pytest.approx(0.04)
        stops = payload["candidate_stops"]
        assert stops["trailing_stop_atr_mult"] == 2.0
        assert "stop_loss_pct" not in stops
        assert stops["leverage"] == payload["control_stops"]["leverage"]
        assert stops["max_drawdown_pct"] == payload["control_stops"]["max_drawdown_pct"]
        assert stops["stop_platform"] == "hyperliquid"
        ctx = stops["capability_context"]
        assert ctx["resolved_stop_owner"] is None
        assert ctx["raw_fields"]["stop_loss_pct"] == {"present": False, "value": None}
        assert ctx["raw_fields"]["trailing_stop_atr_mult"] == {"present": True, "value": 2.0}
        evidence = ctx["input_evidence"]
        control_evidence = payload["control_stops"]["capability_context"]["input_evidence"]
        assert evidence["leverage"] == control_evidence["leverage"]
        assert evidence["leverage"]["status"] == "verified"
        assert evidence["stop_units"] == control_evidence["stop_units"]
        assert evidence["candidate_stop_override"] == {
            "status": "verified", "source": "candidate_stops",
            "value": {"trailing_stop_atr_mult": 2.0}}
        assert evidence["resolved_live_units"]["value"]["stop_loss_pct"] is None
        assert evidence["resolved_live_units"]["value"]["trailing_stop_atr_mult"] == 2.0
        res = _dataset(payload)
        controls.append(res["control_arm"])
        assert res["paired_diag"]["paired"] > 0
        assert res["candidate_arm"]["exit_reasons"].get("sl", 0) > 0
        if stop_only:
            assert set(res["candidate_arm"]["exit_reasons"]) <= STOP_ONLY_REASONS
            assert set(res["paired_diag"]["candidate_exit_reasons"]) <= STOP_ONLY_REASONS
    inherit = _m6_payload(out_dir, "m6.tp_inherit")
    assert inherit["candidate_stops_mode"] == "inherit"
    assert inherit["candidate_stop_owner"] == "fixed_pct"
    controls.append(_dataset(inherit)["control_arm"])
    assert all(c == controls[0] for c in controls)


def test_stop_only_variant_without_candidate_close_runs_like_empty_close(candle_env, tmp_path):
    stops = {"trailing_stop_atr_mult": 3.0}
    spec = _spec({
        "strategy_id": "sma_crossover",
        "incumbent_close": INCUMBENT,
        "candidate_close_variants": [
            {"key": "trail_no_close", "candidate_stops": stops},
            {"key": "trail_empty_close", "candidate_close": [], "candidate_stops": stops},
            {"key": "inherit_no_close"},
        ],
    })
    proc, report, out_dir = _auto_suggest(candle_env, tmp_path, spec)
    entries = {e["key"]: e for e in report["ranked"]}
    results = {}
    for key in ("m6.trail_no_close", "m6.trail_empty_close"):
        entry = entries[key]
        assert entry["precondition_errors"] == []
        assert entry["verdict"] not in ("excluded_close_capability",
                                        "excluded_not_replayable", "run_failed")
        assert entry["candidate"]["candidate_close"] == []
        assert entry["candidate"]["candidate_stop_only"] is True
        assert entry["evidence"]["m6"]["data"][WINDOW]["paired_n"] > 0
        assert "--candidate-close '[]'" in entry["reproduce"][0]
        res = _dataset(_m6_payload(out_dir, key))
        assert res["paired_diag"]["paired"] > 0
        assert set(res["candidate_arm"]["exit_reasons"]) <= STOP_ONLY_REASONS
        results[key] = res
    assert results["m6.trail_no_close"] == results["m6.trail_empty_close"]
    inherit = entries["m6.inherit_no_close"]
    assert inherit["verdict"] == "excluded_not_replayable"
    assert inherit["precondition_errors"] == ["excluded_not_replayable"]
    assert inherit["candidate"]["candidate_stop_only"] is False
    assert inherit["evidence"] == {}
    assert not (out_dir / "m6.inherit_no_close.m6.json").exists()
