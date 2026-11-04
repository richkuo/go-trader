import json
import sys

import pandas as pd
import pytest

import run_backtest
from registry_loader import load_registry
from regime_label_columns import attach_regime_label_columns, column_name

N = 240
TRIGGER_BAR = 120
REGIME = {"enabled": True, "period": 14, "adx_threshold": 20,
          "windows": {"medium": {"classifier": "composite", "period": 20}}}
CLOSE = {"name": "tiered_tp_atr", "params": {"tp_tiers": [{"atr_multiple": 50.0, "close_fraction": 1.0}]}}


def _frame(drift, wiggle):
    idx = pd.date_range("2024-01-01", periods=N, freq="1h")
    close = pd.Series([300.0 + drift * i + (wiggle if i % 2 == 0 else -wiggle) for i in range(N)], index=idx)
    return pd.DataFrame({"open": close.shift(1).fillna(close.iloc[0]), "high": close + 0.5,
                         "low": close - 0.5, "close": close, "volume": 10.0})


DOWN_CHOPPY = (-0.6, 1.5)
UP_CLEAN = (0.6, 0.0)


def _strategy(**extra):
    sc = {"id": "hl-side", "type": "perps", "platform": "hyperliquid",
          "args": ["sma_crossover", "BTC", "1h", "--mode=paper"],
          "open_strategy": {"name": "sma_crossover"}, "close_strategy": CLOSE,
          "direction": "both", "leverage": 1, "regime_side": True}
    sc.update(extra)
    return sc


def _write(tmp_path, sc, name="config.json"):
    path = tmp_path / name
    path.write_text(json.dumps({"config_version": 15, "regime": REGIME, "strategies": [sc]}))
    return str(path)


class _ScriptedRegistry:
    def __init__(self, triggers):
        self._real = load_registry("futures")
        self._triggers = triggers

    def __getattr__(self, name):
        return getattr(self._real, name)

    def apply_strategy(self, name, df, params=None):
        out = self._real.apply_strategy(name, df, params)
        out["signal"] = [self._triggers.get(i, 0) for i in range(len(out))]
        return out


def _run(monkeypatch, path, frame, triggers, extra_argv=(), htf_frame=None):
    monkeypatch.setattr(run_backtest, "load_cached_data", lambda *a, **k: frame.copy())
    monkeypatch.setattr(run_backtest, "load_registry", lambda *_a, **_k: _ScriptedRegistry(triggers))
    monkeypatch.setattr("backtester.store_backtest_result", lambda *a, **k: None)
    if htf_frame is not None:
        monkeypatch.setattr(run_backtest, "_load_htf_cache", lambda *a, **k: htf_frame.copy())
    captured = {}
    real = run_backtest.run_single_backtest

    def wrap(*args, **kwargs):
        captured["result"] = real(*args, **kwargs)
        captured["kwargs"] = kwargs
        return captured["result"]

    monkeypatch.setattr(run_backtest, "run_single_backtest", wrap)
    monkeypatch.setattr(sys, "argv", [
        "run_backtest.py", "--config", path, "--strategy", "hl-side", "--mode", "single",
        "--symbol", "BTC", "--timeframe", "1h", "--funding", "off", *extra_argv])
    run_backtest.main()
    return captured["result"], captured["kwargs"]


def _entry(frame, bar=TRIGGER_BAR):
    return str(frame.index[bar + 1])


def _sides(result):
    return [t["side"] for t in result["trades"]]


def test_labels_used_by_the_fixtures(tmp_path):
    plan = run_backtest.load_strategy_config(_write(tmp_path, _strategy()), "hl-side")["regime_label_windows"]
    col = column_name(plan["windows"]["directional"])
    for (drift, wiggle), label in ((DOWN_CHOPPY, "trending_down_choppy"), (UP_CLEAN, "trending_up_clean")):
        labeled, _ = attach_regime_label_columns(_frame(drift, wiggle), plan)
        assert labeled[col].iloc[TRIGGER_BAR] == label
        assert bool(labeled[col + "_ready"].iloc[TRIGGER_BAR])


def test_config_runs_open_either_trigger_sign_on_the_label_side(monkeypatch, tmp_path, subtests):
    path = _write(tmp_path, _strategy())
    for shape, side in ((DOWN_CHOPPY, "short"), (UP_CLEAN, "long")):
        frame = _frame(*shape)
        for trigger in (1, -1):
            with subtests.test(side=side, trigger=trigger):
                res, kwargs = _run(monkeypatch, path, frame, {TRIGGER_BAR: trigger})
                assert kwargs["regime_side"] is True
                assert kwargs["regime_side_certification"]["context"] == "paper"
                assert _sides(res) == [side]
                assert res["trades"][0]["entry_date"] == _entry(frame)
                assert res["regime_side"]["context"] == "paper"


def test_warmup_bars_and_zero_triggers_hold(monkeypatch, tmp_path):
    path = _write(tmp_path, _strategy())
    frame = _frame(*DOWN_CHOPPY)
    res, _ = _run(monkeypatch, path, frame, {5: 1, 10: -1})
    assert res["trades"] == []
    assert res["regime_side"]["counts"]["held:window_unavailable"] == 2
    res, _ = _run(monkeypatch, path, frame, {})
    assert res["trades"] == []
    assert not any(k.startswith("opened") for k in res["regime_side"]["counts"])


def test_base_direction_from_the_config(monkeypatch, tmp_path, subtests):
    for direction, shape, expected in (
            ("long", DOWN_CHOPPY, []), ("long", UP_CLEAN, ["long"]),
            ("short", DOWN_CHOPPY, ["short"]), ("short", UP_CLEAN, []),
            (None, UP_CLEAN, ["long"]), (None, DOWN_CHOPPY, [])):
        with subtests.test(direction=direction, shape=shape):
            sc = _strategy(direction=direction) if direction else {
                k: v for k, v in _strategy().items() if k != "direction"}
            path = _write(tmp_path, sc, name=f"{direction}-{shape[0]}.json")
            res, _ = _run(monkeypatch, path, _frame(*shape), {TRIGGER_BAR: 1})
            assert _sides(res) == expected


def _htf(rising):
    idx = pd.date_range("2023-12-01", "2024-01-12", freq="4h")
    close = pd.Series([1000.0 + (5.0 if rising else -5.0) * i for i in range(len(idx))], index=idx)
    return pd.DataFrame({"open": close, "high": close + 1, "low": close - 1, "close": close, "volume": 1.0})


def test_htf_filter_checks_the_mapped_side(monkeypatch, tmp_path, subtests):
    path = _write(tmp_path, _strategy())
    frame = _frame(*DOWN_CHOPPY)
    for name, trigger, rising, expected in (
            ("raw_disagrees_mapped_agrees", 1, False, ["short"]),
            ("raw_agrees_mapped_disagrees", 1, True, [])):
        with subtests.test(case=name):
            res, _ = _run(monkeypatch, path, frame, {TRIGGER_BAR: trigger},
                          extra_argv=("--htf-filter",), htf_frame=_htf(rising))
            assert _sides(res) == expected
            assert res["regime_side"]["htf_filter"] is True
            if not expected:
                assert res["regime_side"]["counts"]["held:htf_filter"] == 1


def _artifact(tmp_path, states, generated_at="2023-12-01T00:00:00Z", expires_at=None,
              classifier="composite"):
    entry = {"asset": "BTC", "timeframe": "1h", "classifier": classifier,
             "generated_at": generated_at, "states": states}
    if expires_at:
        entry["expires_at"] = expires_at
    path = tmp_path / "certs.json"
    path.write_text(json.dumps({"schema_version": 1, "generated_at": generated_at, "certified": [entry]}))
    return str(path)


def test_live_equivalence_reads_the_artifact_exact_label_and_expiry(monkeypatch, tmp_path, subtests):
    frame = _frame(*DOWN_CHOPPY)
    live_args = ["sma_crossover", "BTC", "1h", "--mode=live"]
    cases = (
        ("certified_exact_label", {"trending_down_choppy": "short"}, {}, False, ["short"]),
        ("sibling_label", {"trending_down_clean": "short"}, {}, False, []),
        ("opposite_side", {"trending_down_choppy": "long"}, {}, False, []),
        ("expired_before_the_data", {"trending_down_choppy": "short"},
         {"expires_at": "2023-12-15T00:00:00Z"}, False, []),
        ("certificate_newer_than_the_interval", {"trending_down_choppy": "short"},
         {"generated_at": "2024-06-01T00:00:00Z"}, False, []),
        ("wrong_classifier_cell", {"trending_down_choppy": "short"}, {"classifier": "adx"}, False, []),
        ("acknowledged", {}, {}, True, ["short"]),
    )
    for name, states, art_kw, ack, expected in cases:
        with subtests.test(case=name):
            monkeypatch.setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", _artifact(tmp_path, states, **art_kw))
            sc = _strategy(args=live_args)
            if ack:
                sc["allow_uncertified_regime_side"] = True
            res, kwargs = _run(monkeypatch, _write(tmp_path, sc, name=f"{name}.json"), frame,
                               {TRIGGER_BAR: 1})
            assert kwargs["regime_side_certification"]["context"] == "live"
            assert _sides(res) == expected
            if not expected:
                assert res["regime_side"]["counts"]["held:uncertified"] == 1
    with subtests.test(case="missing_artifact_holds"):
        monkeypatch.setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", str(tmp_path / "absent.json"))
        res, kwargs = _run(monkeypatch, _write(tmp_path, _strategy(args=live_args), name="absent.json"),
                           frame, {TRIGGER_BAR: 1})
        assert kwargs["regime_side_certification"]["states"] == {}
        assert res["trades"] == []
    with subtests.test(case="malformed_artifact_holds"):
        bad = tmp_path / "bad.json"
        bad.write_text(json.dumps({"schema_version": 1, "certified": [
            {"asset": "BTC", "timeframe": "1h", "classifier": "composite",
             "generated_at": "2023-12-01T00:00:00Z",
             "states": {"trending_down_choppy": "short", "trending_up_clean": "sideways"}}]}))
        monkeypatch.setenv("GO_TRADER_DIRECTIONAL_CERT_PATH", str(bad))
        res, _ = _run(monkeypatch, _write(tmp_path, _strategy(args=live_args), name="bad-cfg.json"),
                      frame, {TRIGGER_BAR: 1})
        assert res["trades"] == []


def test_loader_refuses_structural_combinations(tmp_path, subtests):
    cases = (
        ("spot", {"type": "spot", "platform": "binanceus"}),
        ("okx", {"platform": "okx"}),
        ("directional_policy", {"regime_directional_policy": {"trend_regime": {
            "trending_up_clean": {"direction": "long"}}}}),
        ("window_divergence", {"regime_window_divergence": {"enabled": True}}),
        ("profile_allocation", {"regime_profile_allocation": {"window": "medium"}}),
        ("invert_signal", {"invert_signal": True}),
        ("closed_bar_decisions", {"closed_bar_decisions": True}),
        ("no_close", {"close_strategy": None}),
        ("open_as_close", {"close_strategy": {"name": "sma_crossover"}}),
        ("non_boolean", {"regime_side": "yes"}),
        ("ack_without_mode", {"regime_side": False, "allow_uncertified_regime_side": True}),
    )
    for name, extra in cases:
        with subtests.test(case=name):
            sc = _strategy(**extra)
            if extra.get("close_strategy", "keep") is None:
                sc.pop("close_strategy")
            with pytest.raises(ValueError):
                run_backtest.load_strategy_config(_write(tmp_path, sc, name=f"{name}.json"), "hl-side")
    with subtests.test(case="regime_disabled"):
        path = tmp_path / "off.json"
        path.write_text(json.dumps({"config_version": 15, "regime": {"enabled": False},
                                    "strategies": [_strategy()]}))
        with pytest.raises(ValueError):
            run_backtest.load_strategy_config(str(path), "hl-side")
    with subtests.test(case="feature_off_keeps_shape"):
        loaded = run_backtest.load_strategy_config(
            _write(tmp_path, {k: v for k, v in _strategy().items() if k != "regime_side"},
                   name="off-shape.json"), "hl-side")
        assert loaded["regime_side"] is False
        assert loaded["regime_side_certification"] is None


def test_consumers_that_cannot_reproduce_the_mode_refuse(tmp_path, subtests):
    path = _write(tmp_path, _strategy())
    with subtests.test(consumer="exit_policy_ab"), pytest.raises(SystemExit):
        import exit_policy_ab
        exit_policy_ab.resolve_from_baseline(path, "hl-side")
    with subtests.test(consumer="tune_live"):
        import tune_live
        resolution = run_backtest.load_strategy_config(path, "hl-side")
        assert tune_live.unsupported_reason(resolution) == "unsupported_regime_side"
    with subtests.test(consumer="run_backtest_by_name"), pytest.raises(SystemExit):
        run_backtest.run_single_backtest("sma_crossover", "BTC", "1h", regime_side=True,
                                         regime_side_certification={"context": "paper"},
                                         direction="both", close_strategies=[CLOSE])


def test_eval_windows_runs_the_mode_in_paper_research(monkeypatch, tmp_path, subtests):
    import eval_windows
    loaded = run_backtest.load_strategy_config(_write(tmp_path, _strategy()), "hl-side")
    base = {"name": "sma_crossover", "type": "perps", "direction": "both",
            "close_strategies": [CLOSE], "regime_side": True,
            "regime_label_windows": loaded["regime_label_windows"]}
    for name, extra in (("invert", {"invert_signal": True}),
                        ("policy", {"regime_directional_policy": {"trend_regime": {
                            "trending_up_clean": {"direction": "long"}}}}),
                        ("no_close", {"close_strategies": None}),
                        ("no_plan", {"regime_label_windows": None}),
                        ("spot", {"type": "spot"}),
                        ("ack_without_mode", {"regime_side": False,
                                              "allow_uncertified_regime_side": True})):
        with subtests.test(case=name), pytest.raises(ValueError):
            eval_windows.validate_candidate({**base, **extra})
    frame = _frame(*DOWN_CHOPPY)
    monkeypatch.setattr("data_fetcher.load_cached_data", lambda *a, **k: frame.copy())
    reg = _ScriptedRegistry({TRIGGER_BAR: 1})
    candidate = eval_windows.validate_candidate(dict(base))
    leg = eval_windows.run_candidate_leg(
        reg, candidate, "BTC", "1h", (str(frame.index[0]), str(frame.index[-1] + pd.Timedelta(hours=1))),
        keep_trades=True, funding_mode="off")
    assert leg is not None
    assert [t["entry_date"] for t in leg["trade_samples"]] == [_entry(frame)]
    assert leg["trade_samples"][0]["pnl_pct"] > 0


def test_optimizer_handoff_treats_the_mode_as_short_capable():
    from backtester import Backtester
    from optimizer import scored_engine_can_open_short
    cols = {"gate": "regime_w_m", "directional": "regime_w_m", "atr": "regime_w_m"}
    frame = pd.DataFrame({"signal": [0]}, index=pd.date_range("2024-01-01", periods=1))
    for direction, expected in (("both", True), ("short", True), ("long", False)):
        engine = Backtester(regime_enabled=True, regime_label_columns=cols, direction=direction,
                            close_strategies=[CLOSE], regime_side=True,
                            regime_side_certification={"context": "paper"})
        assert scored_engine_can_open_short(engine, frame) is expected
