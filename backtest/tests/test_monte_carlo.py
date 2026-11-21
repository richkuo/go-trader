
import json

import pytest

import eval_windows as ew
import monte_carlo as mc


def test_block_path_preserves_circular_contiguity(subtests):
    with subtests.test(case="test_block_path_preserves_circular_contiguity"):
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

    with subtests.test(case="test_block_len_full_series_yields_rotations"):
        values = [1.0, 2.0, 3.0, 4.0]
        rotations = {tuple(values[i:] + values[:i]) for i in range(len(values))}
        from random import Random
        rng = Random(11)
        for _ in range(30):
            assert tuple(mc.block_bootstrap_path(values, len(values), rng)) \
                in rotations

    with subtests.test(case="test_block_scheme_auto_len_recorded"):
        stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1)
        assert stats["block_len"] == 3
        stats = mc.resample_stats([1.0] * 27, "block", n_paths=10, seed=1,
                                  block_len=5)
        assert stats["block_len"] == 5


def _trade(pnl_pct, shares=2.0, entry_price=100.0, pnl=None):
    return {"pnl_pct": pnl_pct, "shares": shares, "entry_price": entry_price,
            "pnl": pnl}


def test_trade_returns():
    cases = [
        ("net_from_pnl", [_trade(5.0, pnl=8.0)], {}, [4.0]),
        ("gross", [_trade(5.0, pnl=8.0)], {"returns": "gross"}, [5.0]),
        ("zero_shares_falls_back", [_trade(5.0, shares=0.0, pnl=8.0)], {}, [5.0]),
        ("bare_floats", [1.5, -2.0], {}, [1.5, -2.0]),
    ]
    for case_id, trades, kwargs, expected in cases:
        assert mc.trade_returns(trades, **kwargs) == [pytest.approx(v)
                                                      for v in expected], (
            f"test_trade_returns[{case_id}]")


def _cfg(strategies, platforms=None):
    return {"strategies": strategies, "platforms": platforms or {}}


def test_resolve_kill_switch_pct():
    cases = [
        (_cfg([{"id": "s1", "type": "perps", "max_drawdown_pct": 12.5}]),
         "s1", 12.5),
        (_cfg([{"id": "s1", "type": "spot", "platform": "robinhood"}],
              {"robinhood": {"risk": {"max_drawdown_pct": 33.0}}}), "s1", 33.0),
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
    ]
    for cfg, strategy_id, expected in cases:
        assert mc.resolve_kill_switch_pct(cfg, strategy_id) == expected, (
            f"test_resolve_kill_switch_pct[{cfg['strategies'][0]}-{cfg['platforms']}]")


def test_trade_samples_carry_net_return_and_fall_back_to_gross():
    case = "test_trade_samples_carry_net_return"
    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 2.0, "entry_price": 100.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct"] == 5.0, case
    assert samples[0]["pnl_pct_net"] == pytest.approx(4.0), case

    results = {"trades": [{"entry_date": "2025-01-01", "pnl_pct": 5.0,
                           "shares": 0.0, "entry_price": 0.0, "pnl": 8.0}]}
    samples = ew.trade_samples_from_results(results)
    assert samples[0]["pnl_pct_net"] == 5.0, "test_trade_samples_net_falls_back_to_gross"


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


_BAD_INVOCATIONS = [
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
]


def test_cli_rejects_bad_invocation(tmp_path, subtests):
    for i, (build_argv, match) in enumerate(_BAD_INVOCATIONS):
        with subtests.test(case=i, match=match):
            case_dir = tmp_path / f"case{i}"
            case_dir.mkdir()
            with pytest.raises(SystemExit, match=match):
                mc.main(build_argv(case_dir))


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


def _patch_leg(monkeypatch, leg):
    import registry_loader
    monkeypatch.setattr(registry_loader, "load_registry", lambda r: _FakeReg)
    monkeypatch.setattr(ew, "run_candidate_leg", lambda *a, **kw: leg)


def test_run_candidate_leg_trades_delegates_and_returns_net_series(monkeypatch, subtests):
    with subtests.test(case="test_run_candidate_leg_trades_delegates_and_returns_net_series"):
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

    with subtests.test(case="test_run_candidate_leg_trades_returns_none_on_missing_data"):
        _patch_leg(monkeypatch, None)
        assert mc.run_candidate_leg_trades({"name": "squeeze_momentum"}, "spot",
                                           "BTC/USDT:1h", "is", 1000.0, "net") is None
