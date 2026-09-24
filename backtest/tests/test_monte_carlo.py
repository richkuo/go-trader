
import itertools
import json

import pytest

import eval_windows as ew
import monte_carlo as mc


@pytest.mark.parametrize("trades,expected_dd,expected_final", [
    ([], 0.0, 0.0),
    ([10.0, 10.0], 0.0, 21.0),
    ([10.0, -20.0, 10.0], 20.0, -3.2),
    ([-100.0, 50.0], 100.0, -100.0),
    ([-10.0], 10.0, -10.0),
])
def test_equity_path_stats(trades, expected_dd, expected_final):
    dd, final = mc.equity_path_stats(trades)
    assert dd == pytest.approx(expected_dd)
    assert final == pytest.approx(expected_final)


def test_percentile_empty_none_and_interpolation():
    assert mc.percentile([], 50) is None
    assert mc.percentile([5.0], 95) == 5.0
    assert mc.percentile([0.0, 10.0], 50) == pytest.approx(5.0)
    assert mc.percentile([0.0, 10.0, 20.0], 25) == pytest.approx(5.0)


def test_add_one_smoothed_never_zero_or_one():
    assert mc.add_one_smoothed(0, 100) == pytest.approx(1 / 101)
    assert mc.add_one_smoothed(100, 100) == pytest.approx(101 / 101)


def test_auto_block_len():
    assert mc.auto_block_len(1) == 1
    assert mc.auto_block_len(27) == 3
    assert mc.auto_block_len(28) == 4


def test_permutation_dds_stay_in_enumerated_set():
    trades = [10.0, -20.0, 5.0]
    enumerated = {round(mc.equity_path_stats(list(p))[0], 4)
                  for p in itertools.permutations(trades)}
    stats = mc.resample_stats(trades, "permute", n_paths=500, seed=42)
    dd_vals = stats["max_dd_pct_percentiles"]
    assert min(enumerated) <= dd_vals["p5"] <= dd_vals["p50"] \
        <= dd_vals["p95"] <= max(enumerated)
    fr = stats["final_return_pct_percentiles"]
    expected_final = mc.equity_path_stats(trades)[1]
    assert fr["p5"] == pytest.approx(expected_final, abs=1e-4)
    assert fr["p95"] == pytest.approx(expected_final, abs=1e-4)


def test_permutation_per_path_dds_match_enumeration_exactly():
    trades = [10.0, -20.0, 5.0]
    enumerated = {round(mc.equity_path_stats(list(p))[0], 6)
                  for p in itertools.permutations(trades)}
    from random import Random
    rng = Random(7)
    seen = {round(mc.equity_path_stats(mc.permuted_path(trades, rng))[0], 6)
            for _ in range(300)}
    assert seen <= enumerated
    assert seen == enumerated


def test_block_path_preserves_circular_contiguity():
    values = [1.0, 2.0, 3.0, 4.0, 5.0]
    adjacent = {(values[i], values[(i + 1) % len(values)])
                for i in range(len(values))}
    from random import Random
    rng = Random(3)
    for _ in range(50):
        path = mc.block_bootstrap_path(values, 2, rng)
        assert len(path) == len(values)
        for i in range(0, len(path) - 1, 2):
            assert (path[i], path[i + 1]) in adjacent


def test_block_len_full_series_yields_rotations():
    values = [1.0, 2.0, 3.0, 4.0]
    rotations = {tuple(values[i:] + values[:i]) for i in range(len(values))}
    from random import Random
    rng = Random(11)
    for _ in range(30):
        assert tuple(mc.block_bootstrap_path(values, len(values), rng)) \
            in rotations


def test_block_scheme_auto_len_recorded():
    stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1)
    assert stats["block_len"] == 3
    stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1,
                              block_len=5)
    assert stats["block_len"] == 5


def test_resample_stats_deterministic_under_seed():
    trades = [3.0, -2.0, 1.5, -4.0, 2.2, 0.7, -1.1]
    for scheme in mc.SCHEMES:
        a = mc.resample_stats(trades, scheme, n_paths=200, seed=99)
        b = mc.resample_stats(trades, scheme, n_paths=200, seed=99)
        assert a == b


def test_resample_stats_empty_is_degenerate_not_crash():
    for scheme in mc.SCHEMES:
        s = mc.resample_stats([], scheme, n_paths=100, seed=1)
        assert s["n_trades"] == 0
        assert s["max_dd_pct_percentiles"] is None
        assert s["p_dd_ge_kill_switch"] is None


def test_resample_stats_single_trade():
    for scheme in mc.SCHEMES:
        s = mc.resample_stats([5.0], scheme, n_paths=50, seed=1)
        assert s["max_dd_pct_percentiles"]["p50"] == 0.0
        assert s["final_return_pct_percentiles"]["p50"] == pytest.approx(5.0)


def test_all_winner_series_reports_smoothed_floor():
    s = mc.resample_stats([1.0, 2.0, 3.0], "permute", n_paths=100, seed=1,
                          kill_switch_pct=25.0)
    assert s["p_dd_ge_kill_switch"] == pytest.approx(1 / 101)
    assert s["p_final_below_start"] == pytest.approx(1 / 101)


def test_certain_breach_smoothed_below_one():
    s = mc.resample_stats([-50.0], "permute", n_paths=100, seed=1,
                          kill_switch_pct=25.0)
    assert s["p_dd_ge_kill_switch"] == pytest.approx(101 / 101)
    assert s["p_final_below_start"] == pytest.approx(101 / 101)


def test_unknown_scheme_rejected():
    with pytest.raises(ValueError):
        mc.resample_stats([1.0], "bogus")


def _trade(pnl_pct, shares=2.0, entry_price=100.0, pnl=None):
    return {"pnl_pct": pnl_pct, "shares": shares, "entry_price": entry_price,
            "pnl": pnl}


@pytest.mark.parametrize("trades,kwargs,expected", [
    ([_trade(5.0, pnl=8.0)], {}, [4.0]),
    ([_trade(5.0, pnl=8.0)], {"returns": "gross"}, [5.0]),
    ([_trade(5.0, shares=0.0, pnl=8.0)], {}, [5.0]),
    ([1.5, -2.0], {}, [1.5, -2.0]),
])
def test_trade_returns(trades, kwargs, expected):
    assert mc.trade_returns(trades, **kwargs) == [pytest.approx(v)
                                                  for v in expected]


@pytest.mark.parametrize("trades,kwargs,match", [
    ([], {"returns": "fees"}, None),
    ([{"shares": 2.0, "entry_price": 100.0, "pnl": 8.0}],
     {"returns": "gross"}, "pnl_pct"),
    ([{"shares": 0.0, "entry_price": 0.0, "pnl": 8.0}],
     {"returns": "net"}, "pnl_pct"),
])
def test_trade_returns_rejects(trades, kwargs, match):
    with pytest.raises(ValueError, match=match):
        mc.trade_returns(trades, **kwargs)


def test_payload_dict_and_list_forms():
    assert mc.trades_from_json_payload({"trades": [1, 2]}) == [1, 2]
    assert mc.trades_from_json_payload([1, 2]) == [1, 2]
    with pytest.raises(ValueError):
        mc.trades_from_json_payload({"no_trades": []})
    with pytest.raises(ValueError):
        mc.trades_from_json_payload("nope")


def _cfg(strategies, platforms=None):
    return {"strategies": strategies, "platforms": platforms or {}}


@pytest.mark.parametrize("cfg,strategy_id,expected", [
    (_cfg([{"id": "s1", "type": "perps", "max_drawdown_pct": 12.5}]),
     "s1", 12.5),
    (_cfg([{"id": "s1", "type": "spot", "platform": "okx"}],
          {"okx": {"risk": {"max_drawdown_pct": 33.0}}}), "s1", 33.0),
    (_cfg([{"id": "hl-btc-x", "type": "perps"}],
          {"hyperliquid": {"risk": {"max_drawdown_pct": 18.0}}}),
     "hl-btc-x", 18.0),
    (_cfg([{"id": "s1", "type": "options", "platform": "binanceus"}]),
     "s1", 40.0),
    (_cfg([{"id": "s1", "type": "futures", "platform": "binanceus"}]),
     "s1", 45.0),
    (_cfg([{"id": "s1", "type": "perps", "platform": "binanceus"}]),
     "s1", 50.0),
    (_cfg([{"id": "s1", "type": "spot", "platform": "binanceus"}]),
     "s1", 60.0),
])
def test_resolve_kill_switch_pct(cfg, strategy_id, expected):
    assert mc.resolve_kill_switch_pct(cfg, strategy_id) == expected


def test_kill_switch_missing_strategy_raises():
    with pytest.raises(ValueError):
        mc.resolve_kill_switch_pct(_cfg([]), "ghost")


def test_trade_samples_carry_net_return():
    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 2.0, "entry_price": 100.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct"] == 5.0
    assert samples[0]["pnl_pct_net"] == pytest.approx(4.0)


def test_trade_samples_net_falls_back_to_gross():
    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 0.0, "entry_price": 0.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct_net"] == 5.0


def test_cli_deterministic_byte_identical_json(tmp_path):
    trades = [{"entry_date": "2025-01-01", "pnl_pct": 3.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": 2.5},
              {"entry_date": "2025-01-02", "pnl_pct": -2.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": -2.5},
              {"entry_date": "2025-01-03", "pnl_pct": 4.0, "shares": 1.0,
               "entry_price": 100.0, "pnl": 3.5}]
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"trades": trades}))
    outs = []
    for i in range(2):
        out = tmp_path / f"mc{i}.json"
        rc = mc.main(["--trades-json", str(src), "--seed", "42",
                      "--n-paths", "300", "--json", str(out)])
        assert rc == 0
        outs.append(out.read_bytes())
    assert outs[0] == outs[1]
    payload = json.loads(outs[0])
    assert {b["scheme"] for b in payload["schemes"]} == set(mc.SCHEMES)
    assert payload["kill_switch_pct"] == mc.DEFAULT_KILL_SWITCH_PCT


def test_cli_config_threshold_resolution(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0, 3.0]))
    cfg = tmp_path / "config.json"
    cfg.write_text(json.dumps({"strategies": [
        {"id": "hl-x", "type": "perps", "max_drawdown_pct": 15.0}]}))
    out = tmp_path / "mc.json"
    rc = mc.main(["--trades-json", str(src), "--config", str(cfg),
                  "--strategy-id", "hl-x", "--n-paths", "50",
                  "--json", str(out)])
    assert rc == 0
    assert json.loads(out.read_text())["kill_switch_pct"] == 15.0


def test_cli_empty_trades_no_crash(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps({"trades": []}))
    assert mc.main(["--trades-json", str(src), "--n-paths", "50"]) == 0


def _valid_trades_json(tmp_path):
    src = tmp_path / "results.json"
    src.write_text(json.dumps([1.0, -2.0, 3.0]))
    return src


def _write_json(tmp_path, name, payload):
    p = tmp_path / name
    p.write_text(json.dumps(payload))
    return str(p)


def _write_text(tmp_path, name, text):
    p = tmp_path / name
    p.write_text(text)
    return str(p)


_BAD_CFG = {"strategies": [{"id": "hl-x", "type": "perps",
                            "max_drawdown_pct": 15.0}]}


@pytest.mark.parametrize("build_argv,match", [
    (lambda tp: [], None),
    (lambda tp: ["--trades-json", "a.json", "--strategy", "breakout"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--config", "cfg.json"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--n-paths", "-5"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--n-paths", "0"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--percentiles", "5,50,150"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--percentiles", "-1"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--schemes", ","], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--percentiles", ","], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--percentiles", "5,abc,95"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--config", _write_json(tp, "config.json", _BAD_CFG),
                 "--strategy-id", "typo-d-id"], None),
    (lambda tp: ["--trades-json", _write_json(tp, "results.json",
                                              {"foo": []})], None),
    (lambda tp: ["--trades-json", _write_json(tp, "results.json",
                                              "nope")], None),
    (lambda tp: ["--trades-json", _write_json(tp, "results.json", 42)], None),
    (lambda tp: ["--trades-json", _write_json(
        tp, "results.json", {"trades": [{"shares": 0.0, "entry_price": 0.0,
                                         "pnl": 8.0}]})], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--config", str(tp / "does_not_exist.json"),
                 "--strategy-id", "hl-x"], None),
    (lambda tp: ["--trades-json", str(_valid_trades_json(tp)),
                 "--config", _write_text(tp, "config.json", "{not valid json"),
                 "--strategy-id", "hl-x"], None),
    (lambda tp: ["--trades-json", str(tp / "does_not_exist.json")], None),
    (lambda tp: ["--trades-json", _write_text(tp, "results.json",
                                              "{not valid json")], None),
    (lambda tp: ["--strategy", "squeeze_momentum",
                 "--params", "{bad json"], None),
    (lambda tp: ["--strategy", "x", "--candidate-json", "c.json"],
     "exactly one trade source"),
    (lambda tp: ["--trades-json", "a.json", "--candidate-json", "c.json"],
     "exactly one trade source"),
    (lambda tp: ["--trades-json", "a.json", "--windows", "is"],
     "do not apply to --trades-json"),
    (lambda tp: ["--strategy", "x", "--windows", "is", "--window", "oos"],
     "mutually exclusive"),
    (lambda tp: ["--strategy", "x", "--datasets", "BTC/USDT:1h",
                 "--dataset", "ETH/USDT:1h"], "mutually exclusive"),
    (lambda tp: ["--candidate-json", "c.json", "--params", "{}"],
     "candidate JSON carries its own"),
    (lambda tp: ["--candidate-json", "c.json", "--direction", "short"],
     "candidate JSON carries its own"),
    (lambda tp: ["--candidate-json", _write_json(
        tp, "bad.json", {"name": "squeeze_momentum", "direction": "both"}),
        "--window", "is"], "not a valid candidate"),
])
def test_cli_rejects_bad_invocation(tmp_path, build_argv, match):
    with pytest.raises(SystemExit, match=match):
        mc.main(build_argv(tmp_path))


def test_run_leg_trades_rejects_bad_dataset(tmp_path):
    with pytest.raises(SystemExit, match="--dataset"):
        mc.run_leg_trades("squeeze_momentum", "spot", None,
                          "not-a-valid-dataset", "is", 1000.0, None, "net")


class _FakeReg:
    STRATEGY_REGISTRY = {"squeeze_momentum": {"default_params": {}}}

    @staticmethod
    def list_strategies():
        return ["squeeze_momentum"]


_SAMPLES = [{"entry_date": "2025-01-01", "pnl_pct": 2.0, "pnl_pct_net": 1.5},
            {"entry_date": "2025-01-02", "pnl_pct": -1.0, "pnl_pct_net": -1.4}]


def _rich_candidate():
    return {"name": "squeeze_momentum", "direction": "long",
            "params": {"kc_mult": 1.5},
            "close_strategies": [{"name": "atr_stop", "params": {"atr_mult": 2.0}}],
            "allowed_regimes": ["trending_up_clean"],
            "regime_windows_spec": {"medium": {"classifier": "composite",
                                               "period": 14}},
            "trailing_stop_atr_mult": 2.5}


def test_run_candidate_leg_threads_the_whole_candidate_into_run_leg(monkeypatch):
    seen = {}

    def fake_run_leg(reg, name, params, symbol, timeframe, window, **kw):
        seen.update(name=name, params=params, kw=kw)
        return {"trade_samples": list(_SAMPLES)}

    monkeypatch.setattr(ew, "run_leg", fake_run_leg)
    cand = _rich_candidate()
    ew.run_candidate_leg(_FakeReg, cand, "BTC/USDT", "1h",
                         ("2025-06-10", "2026-01-01"), keep_trades=True)

    assert seen["name"] == "squeeze_momentum"
    assert seen["params"] == {"kc_mult": 1.5}
    kw = seen["kw"]
    assert kw["close_strategies"] == cand["close_strategies"]
    assert kw["allowed_regimes"] == ["trending_up_clean"]
    assert kw["regime_windows_spec"] == cand["regime_windows_spec"]
    assert kw["trailing_stop_atr_mult"] == 2.5
    assert kw["keep_trades"] is True


def test_run_candidate_leg_applies_the_validated_long_direction_default(monkeypatch):
    seen = {}
    monkeypatch.setattr(ew, "run_leg",
                        lambda *a, **kw: seen.update(kw) or {"trade_samples": []})
    ew.run_candidate_leg(_FakeReg, {"name": "squeeze_momentum"}, "BTC/USDT",
                         "1h", ("2025-06-10", None))
    assert seen["direction"] == "long"


def _patch_leg(monkeypatch, leg):
    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    monkeypatch.setattr(ew, "run_candidate_leg", lambda *a, **kw: leg)


def test_run_candidate_leg_trades_delegates_and_returns_net_series(monkeypatch):
    captured = {}

    def fake(reg, cand, symbol, timeframe, window, **kw):
        captured.update(cand=cand, symbol=symbol, timeframe=timeframe, kw=kw)
        return {"trade_samples": list(_SAMPLES)}

    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    monkeypatch.setattr(ew, "run_candidate_leg", fake)

    vals = mc.run_candidate_leg_trades(_rich_candidate(), "spot",
                                       "BTC/USDT:1h", "is", 1000.0, "net")
    assert vals == [1.5, -1.4]
    assert captured["cand"]["allowed_regimes"] == ["trending_up_clean"]
    assert (captured["symbol"], captured["timeframe"]) == ("BTC/USDT", "1h")
    assert captured["kw"]["keep_trades"] is True

    monkeypatch.setattr(ew, "run_candidate_leg", fake)
    assert mc.run_candidate_leg_trades(_rich_candidate(), "spot",
                                       "BTC/USDT:1h", "is", 1000.0,
                                       "gross") == [2.0, -1.0]


def test_run_candidate_leg_trades_returns_none_on_missing_data(monkeypatch):
    _patch_leg(monkeypatch, None)
    assert mc.run_candidate_leg_trades({"name": "squeeze_momentum"}, "spot",
                                       "BTC/USDT:1h", "is", 1000.0, "net") is None


def test_default_dataset_args_matches_the_eval_windows_audit_six():
    assert mc.default_dataset_args() == [f"{s}:{t}" for s, t in ew.DATASETS]
    assert len(mc.default_dataset_args()) == 6


def _write_candidate(tmp_path, **over):
    cand = {"name": "squeeze_momentum", "direction": "long"}
    cand.update(over)
    p = tmp_path / "cand.json"
    p.write_text(json.dumps(cand))
    return p


def test_multileg_payload_has_one_block_per_window_dataset_pair(monkeypatch, tmp_path):
    _patch_leg(monkeypatch, {"trade_samples": list(_SAMPLES)})
    cand = _write_candidate(tmp_path)
    out = tmp_path / "mc.json"
    rc = mc.main(["--candidate-json", str(cand), "--windows", "is,oos",
                  "--datasets", "BTC/USDT:1h,ETH/USDT:4h", "--n-paths", "50",
                  "--json", str(out)])
    assert rc == 0
    payload = json.loads(out.read_text())
    assert "legs" in payload and "observed" not in payload
    assert len(payload["legs"]) == 4
    assert {(l["window"], l["dataset"]) for l in payload["legs"]} == {
        ("is", "BTC/USDT 1h"), ("is", "ETH/USDT 4h"),
        ("oos", "BTC/USDT 1h"), ("oos", "ETH/USDT 4h")}
    leg = payload["legs"][0]
    assert leg["status"] == "ok" and leg["n_trades"] == 2
    assert {b["scheme"] for b in leg["schemes"]} == set(mc.SCHEMES)
    assert payload["candidate"]["name"] == "squeeze_momentum"


def test_multileg_is_deterministic_under_seed(monkeypatch, tmp_path):
    _patch_leg(monkeypatch, {"trade_samples": list(_SAMPLES)})
    cand = _write_candidate(tmp_path)
    outs = []
    for i in range(2):
        out = tmp_path / f"mc{i}.json"
        assert mc.main(["--candidate-json", str(cand), "--windows", "is",
                        "--datasets", "BTC/USDT:1h", "--seed", "42",
                        "--n-paths", "200", "--json", str(out)]) == 0
        outs.append(out.read_bytes())
    assert outs[0] == outs[1]


def test_multileg_defaults_to_the_audit_six_datasets(monkeypatch, tmp_path):
    _patch_leg(monkeypatch, {"trade_samples": list(_SAMPLES)})
    out = tmp_path / "mc.json"
    assert mc.main(["--candidate-json", str(_write_candidate(tmp_path)),
                    "--windows", "oos", "--n-paths", "20",
                    "--json", str(out)]) == 0
    assert len(json.loads(out.read_text())["legs"]) == 6


def test_multileg_records_a_no_data_leg_without_aborting_the_fan(monkeypatch, tmp_path):
    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    calls = {"n": 0}

    def fake(*a, **kw):
        calls["n"] += 1
        return None if calls["n"] == 1 else {"trade_samples": list(_SAMPLES)}

    monkeypatch.setattr(ew, "run_candidate_leg", fake)
    out = tmp_path / "mc.json"
    rc = mc.main(["--candidate-json", str(_write_candidate(tmp_path)),
                  "--windows", "is", "--datasets", "BTC/USDT:1h,ETH/USDT:4h",
                  "--n-paths", "20", "--json", str(out)])
    assert rc == 0
    legs = json.loads(out.read_text())["legs"]
    assert [l["status"] for l in legs] == ["no_data", "ok"]
    assert legs[0]["schemes"] == [] and legs[0]["observed"] is None


def test_multileg_fails_when_no_leg_has_data(monkeypatch, tmp_path):
    _patch_leg(monkeypatch, None)
    assert mc.main(["--candidate-json", str(_write_candidate(tmp_path)),
                    "--windows", "is", "--datasets", "BTC/USDT:1h"]) == 1


def test_multileg_bare_strategy_source_also_fans(monkeypatch, tmp_path):
    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    monkeypatch.setattr(ew, "run_leg",
                        lambda *a, **kw: {"trade_samples": list(_SAMPLES)})
    out = tmp_path / "mc.json"
    assert mc.main(["--strategy", "squeeze_momentum", "--windows", "is",
                    "--datasets", "BTC/USDT:1h", "--n-paths", "20",
                    "--json", str(out)]) == 0
    assert len(json.loads(out.read_text())["legs"]) == 1


def test_single_leg_payload_shape_is_unchanged_by_1295(monkeypatch, tmp_path):
    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    seen = {}

    def fake_run_leg(reg, name, params, symbol, timeframe, window, **kw):
        seen.update(direction=kw.get("direction"), symbol=symbol)
        return {"trade_samples": list(_SAMPLES)}

    monkeypatch.setattr(ew, "run_leg", fake_run_leg)
    out = tmp_path / "mc.json"
    assert mc.main(["--strategy", "squeeze_momentum", "--n-paths", "20",
                    "--json", str(out)]) == 0
    payload = json.loads(out.read_text())
    assert "legs" not in payload
    assert set(payload["observed"]) == {"max_dd_pct", "final_return_pct"}
    assert payload["n_trades"] == 2
    assert seen["symbol"] == "BTC/USDT"
    assert seen["direction"] is None
