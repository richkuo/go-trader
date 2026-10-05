import json
import math
import os
import shlex
import subprocess
import sys

import pandas as pd
import pytest

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
if os.path.join(REPO, "shared_tools") not in sys.path:
    sys.path.insert(0, os.path.join(REPO, "shared_tools"))
AUTO_SUGGEST = os.path.join(REPO, "backtest", "auto_suggest.py")
M6 = os.path.join(REPO, "backtest", "exit_policy_ab.py")
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
    storage.store_ohlcv(_frozen_candles(), "binanceus", "BTC/USDT", "4h", db_path=db)
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


def test_stop_overrides_and_stop_only_variants_run_paired_through_m6(candle_env, tmp_path):
    spec = _spec({
        "strategy_id": "sma_crossover",
        "incumbent_close": INCUMBENT,
        "candidate_close_variants": [
            {"key": "fixed_sl", "candidate_close": WIDE_TP,
             "candidate_stops": {"stop_loss_atr_mult": 1.0}},
            {"key": "trail_tight", "candidate_close": WIDE_TP,
             "candidate_stops": {"trailing_stop_atr_mult": 1.5}},
            {"key": "trail_wide", "candidate_close": WIDE_TP,
             "candidate_stops": {"trailing_stop_atr_mult": 3.0}},
            {"key": "atr_trail", "candidate_close": [],
             "candidate_stops": {"trailing_stop_atr_mult": 3.0}},
        ],
        "close_stack_specs": [
            {"trailing_stop_atr_mult": [2.0]},
            {"close": {"name": "tiered_tp_atr", "params": {"tp_tiers": [WIDE_TP[0]["params"]["tp_tiers"]]}},
             "stop_loss_atr_mult": [1.25]},
        ],
    })
    proc, report, out_dir = _auto_suggest(candle_env, tmp_path, spec)
    assert proc.returncode == 0, proc.stderr[-3000:]
    assert report["close_validation"]["requested_set_complete"] is True
    entries = {e["key"]: e for e in report["ranked"]}
    expected = {
        "m6.fixed_sl": ({"stop_loss_atr_mult": 1.0}, False, "fixed_atr"),
        "m6.trail_tight": ({"trailing_stop_atr_mult": 1.5}, False, "trailing_atr"),
        "m6.trail_wide": ({"trailing_stop_atr_mult": 3.0}, False, "trailing_atr"),
        "m6.atr_trail": ({"trailing_stop_atr_mult": 3.0}, True, "trailing_atr"),
        "m6.close_stack_0": ({"trailing_stop_atr_mult": 2.0}, True, "trailing_atr"),
        "m6.close_stack_1": ({"stop_loss_atr_mult": 1.25}, False, "fixed_atr"),
    }
    assert set(entries) == set(expected)
    family_keys = {t["candidate_key"] for t in report["family_tests"] if t["harness"] == "m6"}
    assert family_keys == set(expected)

    controls = []
    for key, (selection, stop_only, owner) in expected.items():
        entry = entries[key]
        assert entry["precondition_errors"] == []
        assert entry["verdict"] not in ("excluded_close_capability",
                                        "excluded_not_replayable", "run_failed")
        cand = entry["candidate"]
        assert not any(r["name"] in ("stop_loss_atr_mult", "trailing_stop_atr_mult")
                       for r in cand["candidate_close"])
        assert cand["candidate_stops"] == selection
        assert cand["candidate_stop_only"] is stop_only
        assert entry["evidence"]["m6"]["data"][WINDOW]["paired_n"] > 0
        policy = entry["evidence"]["m6"]["stop_policy"]
        assert policy["candidate_stops_mode"] == "replace"
        assert policy["candidate_stops_selection"] == selection
        assert policy["candidate_stops"] == selection
        assert policy["candidate_stop_only"] is stop_only
        assert policy["candidate_stop_owner"] == owner
        assert policy["control_stop_owner"] == "none"
        assert policy["stop_units"] == "engine_fraction"
        assert "--candidate-stops" in entry["reproduce"][0]
        assert shlex.quote(json.dumps(selection, sort_keys=True)) in entry["reproduce"][0]

        res = _dataset(_m6_payload(out_dir, key))
        controls.append(res["control_arm"])
        assert res["paired_diag"]["paired"] > 0
        assert res["paired_diag"]["paired"] == res["paired_diag"]["schedule_entries"]
        cand_reasons = res["candidate_arm"]["exit_reasons"]
        replay_reasons = res["paired_diag"]["candidate_exit_reasons"]
        assert cand_reasons.get("sl", 0) > 0 and replay_reasons.get("sl", 0) > 0
        if stop_only:
            assert set(cand_reasons) <= STOP_ONLY_REASONS
            assert set(replay_reasons) <= STOP_ONLY_REASONS
        else:
            assert any(r.startswith("tiered_tp_atr") for r in cand_reasons)
    assert all(c == controls[0] for c in controls)
    assert set(controls[0]["exit_reasons"]) <= {"tiered_tp_atr:0.5", "tiered_tp_atr:1",
                                                "end_of_data"}

    tight = _dataset(_m6_payload(out_dir, "m6.trail_tight"))
    wide = _dataset(_m6_payload(out_dir, "m6.trail_wide"))
    assert tight["control_arm"] == wide["control_arm"]
    assert tight["candidate_arm"] != wide["candidate_arm"]
    tight_all = tight["per_regime"]["all"]
    wide_all = wide["per_regime"]["all"]
    assert tight_all["control_mean_net_pct"] == wide_all["control_mean_net_pct"]
    assert tight_all["candidate_mean_net_pct"] != wide_all["candidate_mean_net_pct"]

    dry = subprocess.run(
        [sys.executable, AUTO_SUGGEST, "--spec", str(tmp_path / "suggest.json"),
         "--windows", WINDOW, "--datasets", DATASET, "--bootstrap-resamples", "200",
         "--out-dir", str(out_dir), "--dry-run"],
        cwd=REPO, env=candle_env, capture_output=True, text=True, timeout=300)
    assert dry.returncode == 0, dry.stderr[-3000:]
    dry_lines = set(dry.stdout.splitlines())
    for key in expected:
        assert entries[key]["reproduce"][0] in dry_lines


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


def test_central_policy_refusal_and_reversal_limitation_are_kept(candle_env, tmp_path):
    spec = _spec({
        "strategy_id": "sma_crossover",
        "incumbent_close": INCUMBENT,
        "candidate_close_variants": [
            {"key": "ratchet_fixed", "candidate_close": [
                {"name": "trailing_tp_ratchet", "params": {}}],
             "candidate_stops": {"stop_loss_atr_mult": 1.0}},
            {"key": "reversal", "candidate_close": []},
            {"key": "reversal_drop", "candidate_close": [], "candidate_stops": "drop"},
            {"key": "tp_ok", "candidate_close": WIDE_TP},
        ],
    })
    proc, report, _ = _auto_suggest(candle_env, tmp_path, spec)
    assert proc.returncode == 1
    entries = {e["key"]: e for e in report["ranked"]}
    refused = entries["m6.ratchet_fixed"]
    assert refused["verdict"] == "excluded_close_capability"
    assert refused["precondition_errors"] == [
        "close_capability_refused:UNSUPPORTED_STOP_OWNER"]
    assert refused["close_capability_refusal"]["reason_code"] == "UNSUPPORTED_STOP_OWNER"
    assert refused["evidence"] == {}
    for key in ("m6.reversal", "m6.reversal_drop"):
        assert entries[key]["verdict"] == "excluded_not_replayable"
        assert entries[key]["candidate"]["candidate_stop_only"] is False
    assert entries["m6.tp_ok"]["evidence"]["m6"]["status"] == "ok"
    cv = report["close_validation"]
    assert cv["requested_set_complete"] is False
    assert [r["reason_code"] for r in cv["refusals"]] == ["UNSUPPORTED_STOP_OWNER"]
    assert "INCOMPLETE" in proc.stdout


@pytest.mark.parametrize("variant,needle", [
    ({"key": "v", "candidate_close": [{"name": "trailing_stop_atr_mult",
                                       "params": {"atr_mult": 3.0}}]}, "candidate_stops"),
    ({"key": "v", "candidate_close": [{"name": "stop_loss_atr_mult",
                                       "params": {"atr_mult": 1.0}}]}, "candidate_stops"),
    ({"key": "v", "candidate_close": [], "candidate_stops": True}, "candidate_stops"),
    ({"key": "v", "candidate_close": [], "candidate_stops": "keep"}, "candidate_stops"),
    ({"key": "v", "candidate_close": [], "candidate_stops": None}, "candidate_stops"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {}}, "exactly one"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {"stop_loss_pct": 2.0}},
     "unknown key"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {
        "stop_loss_atr_mult": 1.0, "trailing_stop_atr_mult": 2.0}}, "exactly one"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {"trailing_stop_atr_mult": 0}},
     "finite positive"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {"trailing_stop_atr_mult": -1}},
     "finite positive"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {"trailing_stop_atr_mult": "3"}},
     "finite positive"),
    ({"key": "v", "candidate_close": [], "candidate_stops": {"trailing_stop_atr_mult": True}},
     "finite positive"),
    ({"key": "v", "candidate_close": [],
      "candidate_stops": {"trailing_stop_atr_mult": float("nan")}}, "finite positive"),
], ids=["trail_close_ref", "sl_close_ref", "bool", "unknown_mode", "null", "empty",
        "unknown_key", "both_keys", "zero", "negative", "string", "bool_value", "nan"])
def test_spec_validation_rejects_invalid_stop_inputs(candle_env, tmp_path, variant, needle):
    spec = _spec({"strategy_id": "sma_crossover", "incumbent_close": INCUMBENT,
                  "candidate_close_variants": [variant]})
    proc, report, _ = _auto_suggest(candle_env, tmp_path, spec, "--dry-run")
    assert proc.returncode != 0
    assert report is None
    assert needle in proc.stderr
    assert "candidate_stops" in proc.stderr


def test_stack_close_named_as_stop_field_is_rejected(candle_env, tmp_path):
    spec = _spec({"strategy_id": "sma_crossover", "incumbent_close": INCUMBENT,
                  "close_stack_specs": [{"close": {"name": "trailing_stop_atr_mult",
                                                   "params": {"atr_mult": [3.0]}}}]})
    proc, _, _ = _auto_suggest(candle_env, tmp_path, spec, "--dry-run")
    assert proc.returncode != 0
    assert "close_stack_specs[0].close" in proc.stderr
    assert "candidate_stops" in proc.stderr


@pytest.mark.parametrize("raw", ['{"trailing_stop_atr_mult": 0}', "keep",
                                 '{"trailing_stop_pct": 1.0}'])
def test_m6_child_rejects_invalid_candidate_stops(candle_env, raw):
    proc = subprocess.run(
        [sys.executable, M6, "--strategy", "sma_crossover", "--registry", "spot",
         "--incumbent-close", json.dumps(INCUMBENT), "--candidate-close", "[]",
         "--candidate-stops", raw, "--windows", WINDOW, "--datasets", DATASET],
        cwd=REPO, env=candle_env, capture_output=True, text=True, timeout=300)
    assert proc.returncode != 0
    assert "--candidate-stops" in proc.stderr
