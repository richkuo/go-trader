import json
import os
import sqlite3
import subprocess
import sys
import types

import pytest

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import (
    CLOSE_CAPABILITIES,
    Backtester,
    CapabilityContext,
    CloseCapabilityError,
    aggregate_close_validations,
    decode_close_validation,
    validate_close_capabilities,
)
from registry_loader import load_registry

REPO = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
STUDY = os.path.join(REPO, "backtest", "candidates", "awesome_oscillator_1660")
MANIFEST = os.path.join(STUDY, "study_manifest.json")
SEED = os.path.join(STUDY, "candidate_seed.json")
TIME_STOP = {"name": "time_stop", "params": {"max_bars": 20}}
ZSCORE = {"name": "zscore_target", "params": {"lookback": 20, "z_target": 2.0}}
TIERED = {"name": "tiered_tp_atr", "params": {"tp_tiers": [
    {"atr_multiple": 1.0, "close_fraction": 0.5}, {"atr_multiple": 2.0, "close_fraction": 1.0}]}}
PLATFORMS = ("hyperliquid", "binanceus", "okx", "okx-perps", "robinhood", "luno")


def _dynamic_ref():
    with open(os.path.join(REPO, "backtest", "testdata", "tp_tier_parity.json")) as fh:
        ladder = json.load(fh)["ladders"]["dynamic"]
    return {"name": ladder["name"], "params": ladder["params"]}


def _frozen_frame(rows=600):
    manifest = om.load_manifest(MANIFEST)
    frame, _, _ = om.window_frame(manifest, om.dataset_by_key(manifest, "BTC 4h"), "test")
    return frame.iloc[-rows:].copy()


def _run_backtest(*extra):
    cmd = [sys.executable, os.path.join(REPO, "backtest", "run_backtest.py"), "--mode", "single",
           "--registry", "futures", "--strategy", "awesome_oscillator", "--stop-loss-atr-mult", "1.0",
           "--direction", "both", "--manifest", MANIFEST, "--manifest-dataset", "BTC 4h",
           "--manifest-window", "test", *extra]
    return subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, timeout=300)


def _refusal_payload(proc):
    return json.loads(proc.stdout.strip().splitlines()[-1])


def test_valid_dynamic_ladder_is_refused_as_live_only_in_every_entry_path(tmp_path):
    from backtester import _ensure_close_strategies_path
    _ensure_close_strategies_path()
    from tiered_tp_atr_regime import resting_ladder_errors
    dyn = _dynamic_ref()
    assert resting_ladder_errors(dyn["name"], dyn["params"]) == []

    for mode in (None, "strict", "approximate"):
        with pytest.raises(CloseCapabilityError) as exc:
            Backtester(platform="hyperliquid", close_strategies=[dyn], comparison_mode=mode)
        assert exc.value.reason_code == "LIVE_ONLY_CLOSE"
        assert exc.value.to_dict()["close_validation"]["close_eligibility"] == "refused"

    import run_backtest
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"config_version": 19, "strategies": [{
        "id": "hl-dyn", "type": "perps", "platform": "hyperliquid",
        "open_strategy": {"name": "sma_crossover"}, "close_strategy": dyn}]}))
    with pytest.raises(CloseCapabilityError) as exc:
        run_backtest.load_strategy_config(str(cfg), "hl-dyn", comparison_mode="approximate")
    assert exc.value.reason_code == "LIVE_ONLY_CLOSE"
    assert "hl-dyn" in str(exc.value)

    proc = _run_backtest("--close-strategy", json.dumps(dyn), "--comparison-mode", "approximate")
    assert proc.returncode == 1
    assert _refusal_payload(proc)["reason_code"] == "LIVE_ONLY_CLOSE"
    assert "BACKTEST REPORT" not in proc.stdout

    import eval_windows
    cand = tmp_path / "cand.json"
    cand.write_text(json.dumps({"name": "awesome_oscillator", "direction": "both",
                                "close_strategies": [dyn], "comparison_mode": "approximate"}))
    with pytest.raises(SystemExit, match="LIVE_ONLY_CLOSE"):
        eval_windows.main(["--registry", "futures", "--candidate-json", str(cand),
                           "--manifest", MANIFEST, "--windows", "test"])

    from parity_diff import compute_parity_frame
    with pytest.raises(CloseCapabilityError) as exc:
        compute_parity_frame(_frozen_frame(260), "awesome_oscillator", registry="futures",
                             close_refs=[dyn], comparison_mode="approximate")
    assert exc.value.reason_code == "LIVE_ONLY_CLOSE"

    sys.path.insert(0, os.path.join(REPO, "backtest", "research"))
    import regime_1081_economic_gate as gate
    with pytest.raises(CloseCapabilityError) as exc:
        gate.validate_arm_config({"close_strategies": [dyn]})
    assert exc.value.reason_code == "LIVE_ONLY_CLOSE"

    from exit_policy_ab import replay_capability
    replay = replay_capability([dyn], "approximate")
    assert replay["replayable"] is False
    assert replay["refusal"]["reason_code"] == "LIVE_ONLY_CLOSE"


def test_strict_research_exits_refuse_before_simulation_on_every_platform():
    for platform in PLATFORMS:
        for ref, needed in ((TIME_STOP, ["bars_held"]), (ZSCORE, ["zscore"])):
            for mode in (None, "strict"):
                with pytest.raises(CloseCapabilityError) as exc:
                    Backtester(platform=platform, close_strategies=[ref], comparison_mode=mode)
                err = exc.value
                assert err.reason_code == "UNSUPPORTED_LIVE_CONTEXT"
                assert err.reasons[0]["required_inputs"] == needed
                assert err.reasons[0]["feature"] == ref["name"]

    proc = _run_backtest("--close-strategy", json.dumps(TIME_STOP))
    assert proc.returncode == 1
    payload = _refusal_payload(proc)
    assert payload["reason_code"] == "UNSUPPORTED_LIVE_CONTEXT"
    assert payload["close_validation"]["mode"] == "strict"
    assert payload["close_validation"]["refusals"][0]["details"]["platform"] == "hyperliquid"
    assert "BACKTEST REPORT" not in proc.stdout


def test_invalid_mode_values_are_errors_and_never_select_approximation():
    for bad in ("", " strict", "Strict", "APPROXIMATE", "exact", True, 1, 0.0, ["approximate"]):
        for refs in ([TIME_STOP], None):
            with pytest.raises(CloseCapabilityError) as exc:
                validate_close_capabilities(close_refs=refs, comparison_mode=bad)
            assert exc.value.reason_code == "INVALID_COMPARISON_MODE"
            assert exc.value.to_dict()["close_validation"]["mode"] is None
    proc = _run_backtest("--close-strategy", json.dumps(TIME_STOP), "--comparison-mode", "Approximate")
    assert proc.returncode == 1
    assert _refusal_payload(proc)["reason_code"] == "INVALID_COMPARISON_MODE"


def test_explicit_approximation_runs_research_exits_and_reports_incomplete_parity(tmp_path):
    proc = _run_backtest("--close-strategy", json.dumps(TIME_STOP), "--comparison-mode", "approximate")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert ("close validation: mode=approximate eligibility=approximate parity=incomplete "
            "approximations=time_stop(bars_held)") in proc.stdout

    out = tmp_path / "m1.json"
    import eval_windows
    assert eval_windows.main(["--registry", "futures", "--candidate-json", SEED, "--manifest", MANIFEST,
                              "--windows", "test", "--json", str(out)]) == 0
    payload = json.loads(out.read_text())
    window = payload["window_scores"][0]["close_validation"]
    assert window["modes"] == ["approximate"]
    assert window["requested_set_complete"] is True
    assert window["incomplete_parity"] is True and window["parity_status"] == "incomplete"
    assert [(a["feature"], a["required_inputs"]) for a in window["approximations"]] == [
        ("time_stop", ["bars_held"])]
    legs = [r["leg"] for r in payload["window_scores"][0]["rows"] if r["leg"]]
    assert legs and all(l["close_validation"]["close_eligibility"] == "approximate" for l in legs)
    assert decode_close_validation(payload["close_validation"])["decode_status"] == "ok"

    with pytest.raises(SystemExit, match="conflicts"):
        eval_windows.main(["--registry", "futures", "--candidate-json", SEED, "--manifest", MANIFEST,
                           "--windows", "test", "--comparison-mode", "strict"])


def test_supported_close_still_succeeds_strict_with_unverified_parity():
    proc = _run_backtest("--close-strategy", json.dumps(TIERED))
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "close validation: mode=strict eligibility=eligible parity=unverified" in proc.stdout
    res = Backtester(platform="hyperliquid", close_strategies=[TIERED]).run(
        ensure_atr_indicator(load_registry("futures").apply_strategy("awesome_oscillator", _frozen_frame(), None)),
        strategy_name="awesome_oscillator", save=False)
    cv = res["close_validation"]
    assert cv == {"schema_version": 1, "mode": "strict", "close_eligibility": "eligible",
                  "approximations": [], "incomplete_parity": False, "parity_status": "unverified",
                  "refusals": []}


def test_optimizer_mixed_candidate_set_refuses_whole_grid_or_keeps_approximation():
    from optimizer import walk_forward_optimize
    df = _frozen_frame(600)
    grid = [
        {"close_strategies": [TIERED], "stop_loss_atr_mult": None, "trailing_stop_atr_mult": None,
         "label": "tiered"},
        {"close_strategies": [TIME_STOP], "stop_loss_atr_mult": None, "trailing_stop_atr_mult": None,
         "label": "time_stop"},
    ]
    ranges = {"fast_period": [5], "slow_period": [34]}
    with pytest.raises(CloseCapabilityError) as exc:
        walk_forward_optimize(df, "awesome_oscillator", ranges, n_splits=2, registry="futures",
                              platform="hyperliquid", close_stack_grid=grid, verbose=False)
    assert exc.value.reason_code == "UNSUPPORTED_LIVE_CONTEXT"

    summary = walk_forward_optimize(df, "awesome_oscillator", ranges, n_splits=2, registry="futures",
                                    platform="hyperliquid", close_stack_grid=grid, verbose=False,
                                    comparison_mode="approximate")
    grid_cv = summary["close_validation"]
    assert grid_cv["children"] == 2 and grid_cv["requested_set_complete"] is True
    assert grid_cv["incomplete_parity"] is True
    assert [a["feature"] for a in grid_cv["approximations"]] == ["time_stop"]
    assert [v["label"] for v in summary["close_stack_validations"]] == ["tiered", "time_stop"]
    for fold in summary.get("window_results") or []:
        assert fold["best_close_stack_validation"]["mode"] == "approximate"
        assert fold["test_result"]["close_validation"]["incomplete_parity"] is True


def test_window_report_counts_only_legs_that_ran(monkeypatch, tmp_path):
    import data_fetcher
    import eval_windows
    frame = _frozen_frame(600)

    def cached(symbol, timeframe, start_date=None, end_date=None, **kw):
        if start_date == eval_windows.WINDOWS["2023"][0] or symbol != "BTC/USDT":
            return frame.iloc[0:0].copy()
        return frame.copy()

    monkeypatch.setattr(data_fetcher, "load_cached_data", cached)
    cand = tmp_path / "candidate.json"
    cand.write_text(json.dumps({"name": "awesome_oscillator", "direction": "both",
                                "stop_loss_atr_mult": 1.0, "close_strategies": [TIERED]}))
    out = tmp_path / "windows.json"
    eval_windows.main(["--candidate-json", str(cand), "--registry", "futures",
                       "--windows", "oos,2023", "--datasets", "BTC/USDT:4h,ETH/USDT:4h",
                       "--json", str(out)])
    payload = json.loads(out.read_text())
    ran, empty = payload["window_scores"]
    assert ran["scored_datasets"] == 1 and empty["verdict"] == "no data"
    assert ran["close_validation"]["children"] == 1
    for cv in (ran["close_validation"], empty["close_validation"], payload["close_validation"]):
        assert cv["close_eligibility"] == "eligible" and cv["parity_status"] == "unverified"
        assert cv["requested_set_complete"] is True and cv["unknown_children"] == 0

    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: frame.iloc[0:0].copy())
    eval_windows.main(["--candidate-json", str(cand), "--registry", "futures",
                       "--windows", "oos,2023", "--datasets", "BTC/USDT:4h",
                       "--json", str(out)])
    none_ran = json.loads(out.read_text())
    assert all(s["verdict"] == "no data" for s in none_ran["window_scores"])
    cv = none_ran["close_validation"]
    assert cv["close_eligibility"] == "eligible" and cv["requested_set_complete"] is True


def test_report_aggregates_never_upgrade_refused_or_legacy_children(tmp_path, capsys):
    import auto_suggest
    import monte_carlo
    spec = tmp_path / "suggest.json"
    spec.write_text(json.dumps({
        "study": "refusal", "registry": "futures", "harnesses": ["m1"], "windows": ["oos"],
        "correction": {"method": "benjamini_hochberg", "alpha": 0.05}, "candidates": [],
        "m6": {"strategy_id": "awesome_oscillator", "incumbent_close": [TIERED],
               "candidate_close_variants": [{"key": "ts", "candidate_close": [TIME_STOP]}]}}))
    report_path = tmp_path / "report.json"
    assert auto_suggest.main(["--spec", str(spec), "--out-dir", str(tmp_path / "runs"),
                              "--json", str(report_path)]) == 1
    refused = json.loads(report_path.read_text())["close_validation"]
    assert refused["close_eligibility"] == "refused" and refused["requested_set_complete"] is False
    assert [r["reason_code"] for r in refused["refusals"]] == ["UNSUPPORTED_LIVE_CONTEXT"]

    trades = tmp_path / "legacy.json"
    trades.write_text(json.dumps({"trades": [{"pnl_pct": 1.0, "pnl_pct_net": 0.9},
                                             {"pnl_pct": -0.5, "pnl_pct_net": -0.6}]}))
    mc_path = tmp_path / "mc.json"
    assert monte_carlo.main(["--trades-json", str(trades), "--n-paths", "50",
                             "--kill-switch-pct", "50", "--json", str(mc_path)]) == 0
    legacy = json.loads(mc_path.read_text())["close_validation"]
    assert legacy["close_eligibility"] == "unknown" and legacy["unknown_children"] == 1
    assert legacy["requested_set_complete"] is False and legacy["incomplete_parity"] is True


def test_parity_cli_refuses_strict_and_reports_incomplete_agreement(monkeypatch):
    import data_fetcher
    import parity_diff
    frame = _frozen_frame(420)
    monkeypatch.setattr(data_fetcher, "load_cached_data", lambda *a, **k: frame)
    base = ["--strategy", "awesome_oscillator", "--registry", "futures", "--platform", "hyperliquid",
            "--symbol", "BTC/USDT", "--timeframe", "4h", "--close", 'time_stop:{"max_bars":20}',
            "--window", "200"]
    assert parity_diff.main(base) == 2
    assert parity_diff.main(base + ["--comparison-mode", "approximate"]) == 3
    strict_frame = parity_diff.compute_parity_frame(frame, "awesome_oscillator", registry="futures",
                                                    close_refs=[TIERED], window=200)
    strict_summary = parity_diff.summarize(strict_frame)
    assert strict_summary["close_parity"] == "unverified"
    assert strict_summary["clean"] is strict_summary["decision_agreement"]


def test_saved_results_column_migration_is_idempotent_and_round_trips(tmp_path):
    import storage
    db = str(tmp_path / "research.db")
    conn = sqlite3.connect(db)
    conn.executescript("""
        CREATE TABLE backtest_results (
            id INTEGER PRIMARY KEY AUTOINCREMENT, strategy_name TEXT NOT NULL, symbol TEXT NOT NULL,
            timeframe TEXT NOT NULL, start_date TEXT NOT NULL, end_date TEXT NOT NULL,
            initial_capital REAL NOT NULL, final_capital REAL NOT NULL, total_return_pct REAL,
            annual_return_pct REAL, sharpe_ratio REAL, sortino_ratio REAL, max_drawdown_pct REAL,
            win_rate REAL, profit_factor REAL, total_trades INTEGER, params TEXT,
            created_at TEXT DEFAULT (datetime('now')), trades_json TEXT);
        INSERT INTO backtest_results (strategy_name, symbol, timeframe, start_date, end_date,
            initial_capital, final_capital) VALUES ('legacy', 'BTC', '4h', 'a', 'b', 1000, 1000);
    """)
    conn.commit()
    conn.close()
    storage.init_db(db)
    storage.init_db(db)
    cols = [r[1] for r in sqlite3.connect(db).execute("PRAGMA table_info(backtest_results)")]
    assert cols.count("close_validation_json") == 1

    signals = ensure_atr_indicator(load_registry("futures").apply_strategy(
        "awesome_oscillator", _frozen_frame(), None))
    res = Backtester(platform="hyperliquid", close_strategies=[TIME_STOP],
                     comparison_mode="approximate").run(signals, strategy_name="awesome_oscillator",
                                                        save=False)
    storage.store_backtest_result(res, db_path=db)
    rows = storage.get_backtest_results(db_path=db).set_index("strategy_name")
    stored = json.loads(rows.loc["awesome_oscillator", "close_validation_json"])
    assert stored == res["close_validation"]
    assert decode_close_validation(stored)["parity_status"] == "incomplete"
    legacy = rows.loc["legacy", "close_validation_json"]
    assert legacy is None or legacy != legacy
    assert decode_close_validation(None)["close_eligibility"] == "unknown"


def _sim(payload, *args):
    return subprocess.run([sys.executable, os.path.join(REPO, "shared_scripts", "simulate_strategy.py"),
                           *args], input=json.dumps(payload) if payload is not None else "",
                          cwd=REPO, capture_output=True, text=True, timeout=300)


def test_check_simulation_returns_named_per_label_refusal_and_keeps_probe():
    frame = _frozen_frame(300)
    candles = [{"time": int(ts.timestamp()), "open": float(r.open), "high": float(r.high),
                "low": float(r.low), "close": float(r.close), "volume": float(r.volume)}
               for ts, r in frame.iterrows()]
    base = {"type": "perps", "platform": "hyperliquid", "symbol": "BTC", "timeframe": "4h",
            "open_strategy": {"name": "sma_crossover"}, "stop_loss_atr_mult": 1.0,
            "stop_units": "live_percent", "leverage_source": "loaded_config"}
    for refused, code in ((TIME_STOP, "UNSUPPORTED_LIVE_CONTEXT"), (_dynamic_ref(), "LIVE_ONLY_CLOSE")):
        proc = _sim({"candles": candles, "configs": [
            {"label": "a_supported", "config": dict(base, close_strategy=TIERED)},
            {"label": "b_refused", "config": dict(base, close_strategy=refused)}]})
        assert proc.returncode == 1
        out = json.loads(proc.stdout)
        assert out["error"].startswith("b_refused: ") and code in out["error"]
        assert out["markers"] == {}
        assert out["label"] == "b_refused" and out["close_capability"]["reason_code"] == code
    ok = _sim({"candles": candles, "configs": [{"label": "a", "config": dict(base, close_strategy=TIERED)}]})
    assert ok.returncode == 0 and "a" in json.loads(ok.stdout)["markers"]
    probe = _sim(None, "--probe-only")
    assert probe.returncode == 0 and json.loads(probe.stdout) == {"ok": True}


def test_tuner_and_replay_keep_the_refusal(tmp_path):
    import tune_live
    from exit_policy_ab import replay_capability
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"config_version": 19, "strategies": [{
        "id": "hl-ts", "type": "perps", "platform": "hyperliquid", "args": ["sma_crossover", "BTC", "4h"],
        "open_strategy": {"name": "sma_crossover"}, "close_strategy": TIME_STOP}]}))
    res = tune_live.tune_strategy(str(cfg), "hl-ts", "BTC", "4h", "futures", load_registry("futures"),
                                  types.SimpleNamespace(), {}, str(tmp_path))
    assert res["status"] == "close_capability_refused"
    assert res["close_capability"]["reason_code"] == "UNSUPPORTED_LIVE_CONTEXT"

    assert replay_capability([TIME_STOP])["refusal"]["reason_code"] == "UNSUPPORTED_LIVE_CONTEXT"
    assert replay_capability([TIME_STOP], "approximate")["replayable"] is True
    pct = replay_capability([{"name": "tiered_tp_pct", "params": {}}])
    assert pct["replayable"] is False and pct["refusal"] is None


def test_every_registered_close_has_one_central_declaration():
    from close_registry_loader import list_strategies
    assert set(list_strategies()) == set(CLOSE_CAPABILITIES)
    for name, cap in CLOSE_CAPABILITIES.items():
        ref = {"name": name, "params": {}}
        if cap.live == "supported":
            assert validate_close_capabilities(close_refs=[ref]).close_eligibility == "eligible"
        elif cap.live == "research_context":
            assert validate_close_capabilities(close_refs=[ref], comparison_mode="approximate"
                                               ).close_eligibility == "approximate"
        else:
            with pytest.raises(CloseCapabilityError):
                validate_close_capabilities(close_refs=[ref], comparison_mode="approximate")
