import numpy as np
import pandas as pd
import pytest

import eval_windows as ew


def _leg(sharpe, ddadj=0.0, trades=5, return_pct=-1.0, max_dd_pct=-10.0):
    return {"sharpe": sharpe, "ddadj": ddadj, "trades": trades,
            "return_pct": return_pct, "max_dd_pct": max_dd_pct,
            "bh_return_pct": -30.0}


@pytest.mark.parametrize("return_pct,max_dd_pct,expected", [
    (-10.0, -50.0, -0.2),
    (5.0, -2.5, 2.0),
    (0.0, 0.0, 0.0),
    (12.0, 0.0, 0.0),
])
def test_dd_adjusted_return(return_pct, max_dd_pct, expected):
    assert ew.dd_adjusted_return(return_pct, max_dd_pct) == pytest.approx(expected)


def test_leg_from_results_collapses_backtester_dict():
    results = {"total_return_pct": -12.5, "max_drawdown_pct": -25.0,
               "sharpe_ratio": -0.9, "total_trades": 17}
    leg = ew.leg_from_results(results, bh_return_pct=-44.3)
    assert leg["sharpe"] == -0.9
    assert leg["return_pct"] == -12.5
    assert leg["max_dd_pct"] == -25.0
    assert leg["ddadj"] == pytest.approx(-0.5)
    assert leg["trades"] == 17
    assert leg["bh_return_pct"] == -44.3


def test_incumbent_bars_per_dataset_median():
    legs = {
        "BTC/USDT 1h": {
            "a": _leg(-1.0, ddadj=-0.5),
            "b": _leg(-2.0, ddadj=-1.0),
            "c": _leg(-3.0, ddadj=-1.5),
        },
    }
    bars = ew.incumbent_bars(legs)
    assert bars["BTC/USDT 1h"]["sharpe"] == pytest.approx(-2.0)
    assert bars["BTC/USDT 1h"]["ddadj"] == pytest.approx(-1.0)
    assert bars["BTC/USDT 1h"]["n"] == 3


def test_incumbent_bars_skips_missing_legs_and_empty_datasets():
    legs = {
        "BTC/USDT 1h": {"a": _leg(-1.0), "b": None, "c": _leg(-3.0)},
        "SOL/USDT 4h": {"a": None, "b": None},
    }
    bars = ew.incumbent_bars(legs)
    assert bars["BTC/USDT 1h"]["sharpe"] == pytest.approx(-2.0)
    assert bars["BTC/USDT 1h"]["n"] == 2
    assert bars["SOL/USDT 4h"] is None


def _bars(sharpe=-1.0, ddadj=-0.5, datasets=("d1", "d2", "d3", "d4")):
    return {ds: {"sharpe": sharpe, "ddadj": ddadj, "n": 8} for ds in datasets}


def test_score_pass_when_means_beat_bar_on_both_metrics():
    legs = {ds: _leg(-0.3, ddadj=-0.2) for ds in ("d1", "d2", "d3", "d4")}
    score = ew.score_candidate(legs, _bars())
    assert score["verdict"] == "pass"
    assert score["mean_sharpe"] == pytest.approx(-0.3)
    assert score["mean_bar_sharpe"] == pytest.approx(-1.0)
    assert score["beats_sharpe_count"] == 4
    assert score["beats_ddadj_count"] == 4
    assert not score["degenerate"]


def test_score_fail_when_only_one_metric_beats_bar():
    legs = {ds: _leg(-0.3, ddadj=-0.9) for ds in ("d1", "d2", "d3", "d4")}
    score = ew.score_candidate(legs, _bars(ddadj=-0.5))
    assert score["verdict"] == "fail"


def test_score_degenerate_zero_trade_majority_rejected():
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2, trades=4),
        "d2": _leg(0.0, ddadj=0.0, trades=0),
        "d3": _leg(0.0, ddadj=0.0, trades=0),
        "d4": _leg(0.0, ddadj=0.0, trades=0),
    }
    score = ew.score_candidate(legs, _bars())
    assert score["degenerate"]
    assert score["verdict"] == "degenerate"


def test_score_trading_exactly_half_is_not_degenerate():
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2, trades=4),
        "d2": _leg(-0.3, ddadj=-0.2, trades=1),
        "d3": _leg(0.0, ddadj=0.0, trades=0),
        "d4": _leg(0.0, ddadj=0.0, trades=0),
    }
    score = ew.score_candidate(legs, _bars())
    assert not score["degenerate"]
    assert score["verdict"] == "pass"


def test_score_unscored_datasets_excluded_from_means():
    legs = {
        "d1": _leg(-0.3, ddadj=-0.2),
        "d2": None,
        "d3": _leg(-0.5, ddadj=-0.3),
    }
    bars = _bars(datasets=("d1", "d3"))
    bars["d2"] = None
    score = ew.score_candidate(legs, bars)
    assert score["scored_datasets"] == 2
    assert score["mean_sharpe"] == pytest.approx(-0.4)


def test_score_no_data_verdict():
    score = ew.score_candidate({"d1": None}, {"d1": None})
    assert score["verdict"] == "no data"


def test_parse_sweep_arg_coerces_numbers():
    assert ew.parse_sweep_arg("period=10,14,20") == ("period", [10, 14, 20])
    assert ew.parse_sweep_arg("z=1.75,2.0") == ("z", [1.75, 2.0])
    assert ew.parse_sweep_arg("mode=fade,breakout") == ("mode", ["fade", "breakout"])


def test_parse_sweep_arg_rejects_malformed():
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("period")
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("=1,2")
    with pytest.raises(ValueError):
        ew.parse_sweep_arg("period=")


def test_expand_sweep_cartesian_preserves_base_params():
    combos = ew.expand_sweep({"keep": 1}, [("a", [1, 2]), ("b", ["x"])])
    assert len(combos) == 2
    labels = [c[0] for c in combos]
    assert labels == ["a=1 b=x", "a=2 b=x"]
    for _, params in combos:
        assert params["keep"] == 1
        assert "a" in params and params["b"] == "x"


def test_parse_dataset_arg():
    assert ew.parse_dataset_arg("BTC/USDT:1h") == ("BTC/USDT", "1h")
    with pytest.raises(ValueError):
        ew.parse_dataset_arg("BTC-USDT-1h")


def test_versioned_definitions_match_protocol():
    assert len(ew.INCUMBENTS) == 8
    assert "breakout" not in ew.INCUMBENTS
    assert "sma_crossover" in ew.INCUMBENTS
    assert len(ew.DATASETS) == 6
    assert set(ew.PROTOCOL_WINDOWS) | set(ew.HELD_OUT_WINDOWS) == set(ew.WINDOWS)
    assert ew.WINDOWS["is"] == ("2025-06-10", "2026-01-01")
    assert ew.WINDOWS["oos"] == ("2026-01-01", None)
    for w in ew.HELD_OUT_WINDOWS:
        assert ew.WINDOWS[w][1] is not None


class _FakeRegistry:
    STRATEGY_REGISTRY = {"alternator": {"default_params": {"period": 2},
                                        "description": "test"}}

    @staticmethod
    def list_strategies():
        return ["alternator"]

    @staticmethod
    def apply_strategy(name, df, params):
        out = df.copy()
        sig = np.zeros(len(out), dtype=int)
        sig[10::20] = 1
        sig[20::20] = -1
        out["signal"] = sig
        return out


def _synthetic_df(n=120):
    idx = pd.date_range("2026-01-01", periods=n, freq="1h")
    base = 100 + np.cumsum(np.sin(np.arange(n) / 5.0))
    return pd.DataFrame({
        "open": base, "high": base * 1.01, "low": base * 0.99,
        "close": base, "volume": np.full(n, 1000.0),
    }, index=idx)


def test_run_leg_returns_leg_metrics(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None))
    assert leg is not None
    assert leg["trades"] > 0
    for key in ("sharpe", "return_pct", "max_dd_pct", "ddadj",
                "trades", "bh_return_pct"):
        assert key in leg
    expected_bh = (df["close"].iloc[-1] - df["close"].iloc[0]) / df["close"].iloc[0] * 100
    assert leg["bh_return_pct"] == pytest.approx(expected_bh, abs=0.01)


def test_run_leg_empty_data_returns_none(monkeypatch):
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: pd.DataFrame(), raising=True)
    assert ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                      ("2023-01-01", "2024-01-01")) is None


def test_run_leg_threads_stop_loss_atr_mult(monkeypatch):
    df = _synthetic_df(n=240)
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    stopped = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                         ("2026-01-01", None), stop_loss_atr_mult=0.1)
    assert plain is not None and stopped is not None
    assert (stopped["return_pct"], stopped["max_dd_pct"]) != \
        (plain["return_pct"], plain["max_dd_pct"]), (
        "stop_loss_atr_mult=0.1 produced an identical leg — the stop never "
        "reached the Backtester")


def test_run_leg_threads_allowed_regimes_and_blocks_entries(monkeypatch):
    df = _synthetic_df(n=240)
    df = df.copy()
    df["regime"] = "trending_up"
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    gated = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None), allowed_regimes=["trending_down"])
    assert plain is not None and plain.get("trades", 0) > 0
    assert gated is not None
    assert gated.get("trades", -1) == 0, (
        "allowed_regimes gate did not suppress entries when bar regime "
        "was never in the allowed set")


@pytest.mark.parametrize("candidate", [
    {"name": "x", "direction": "short"},
    {"name": "x", "direction": "short",
     "close_strategies": [{"name": "tp_at_pct", "params": {}}]},
    {"name": "x", "allowed_regimes": ["trending_down"]},
    {"name": "x", "direction": "short",
     "close_strategies": [{"name": "atr_stop", "params": {"atr_mult": 1.5}}],
     "allowed_regimes": ["ranging", "trending_down"]},
    {"name": "x", "allowed_regimes": ["trending_up"],
     "regime_period": 21, "regime_adx_threshold": 25.0},
    {"name": "x", "invert_signal": True},
    {"name": "x", "invert_signal": True, "type": "manual"},
    {"name": "x", "stop_loss_atr_mult": 2.0},
    {"name": "x", "trailing_stop_atr_mult": 2.5},
])
def test_validate_candidate_accepts(candidate):
    assert ew.validate_candidate(candidate) is candidate


@pytest.mark.parametrize("candidate,match", [
    ({"name": "x", "direction": "both"}, "silently dropped"),
    ({"name": "x", "direction": "sideways"}, "direction"),
    ({}, "name"),
    ({"name": "x", "invert_signal": True, "type": "spot"}, "invert_signal"),
    ({"name": "x", "stop_loss_atr_mult": 2.0,
      "trailing_stop_atr_mult": 2.5}, "mutually exclusive"),
    ({"name": "x", "allowed_regimes": "trending_down"}, "list of strings"),
    ({"name": "x", "allowed_regimes": ["trending_down", 123]}, "strings"),
    ({"name": "x", "regime_windows_spec": "medium"}, "regime_windows_spec"),
    ({"name": "x", "regime_windows_spec": {"regime": 14}}, "reserved"),
    ({"name": "x", "regime_windows_spec": {"medium": "fast"}},
     "regime_windows_spec"),
    ({"name": "x", "regime_directional_policy": "trending_up"},
     "regime_directional_policy"),
    ({"name": "x", "regime_directional_policy": {
        "trending_up": {"direction": "long"}}}, "trend_regime"),
    ({"name": "x", "regime_directional_policy": {"trend_regime": {
        "trending_up": {"direction": "flat"}}}}, "regime_directional_policy"),
    ({"name": "x", "regime_directional_policy": {"trend_regime": {
        "trending_down": {"direction": "both"}}}}, "close_strategies"),
    ({"name": "x", "regime_period": 21}, "gate consumer"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_windows_spec": {"medium": {"classifier": "adx", "period": 14}},
      "regime_period": 21}, "windows spec owns"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": True}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": 1}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": "14"}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_period": 14.0}, "regime_period"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": True}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": 0}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": -5}, "regime_adx_threshold"),
    ({"name": "x", "allowed_regimes": ["trending_up"],
      "regime_adx_threshold": "20"}, "regime_adx_threshold"),
])
def test_validate_candidate_rejects(candidate, match):
    with pytest.raises(ValueError, match=match):
        ew.validate_candidate(candidate)


@pytest.mark.parametrize("candidate,key", [
    ({"name": "x", "allowed_regimes": []}, "allowed_regimes"),
    ({"name": "x", "regime_windows_spec": {}}, "regime_windows_spec"),
    ({"name": "x", "regime_directional_policy": {}},
     "regime_directional_policy"),
])
def test_validate_candidate_strips_empty_optional_block(candidate, key):
    assert ew.validate_candidate(candidate) is candidate
    assert key not in candidate


def test_run_candidate_leg_threads_regime_lookback(monkeypatch):
    seen = {}

    def fake_run_leg(reg, name, params, symbol, timeframe, window, **kw):
        seen.update(kw)
        return {"ok": True}

    monkeypatch.setattr(ew, "run_leg", fake_run_leg, raising=True)
    cand = {"name": "x", "allowed_regimes": ["trending_up"],
            "regime_period": 21, "regime_adx_threshold": 25.0}
    ew.run_candidate_leg(_FakeRegistry(), cand, "BTC/USDT", "1h",
                         ("2026-01-01", None))
    assert seen["regime_period"] == 21
    assert seen["regime_adx_threshold"] == 25.0
    seen.clear()
    ew.run_candidate_leg(_FakeRegistry(), {"name": "x"}, "BTC/USDT", "1h",
                         ("2026-01-01", None))
    assert seen["regime_period"] == 14
    assert seen["regime_adx_threshold"] == 20.0


def test_run_leg_threads_regime_windows_spec_composite_gate(monkeypatch):
    df = _synthetic_df(n=240)
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    spec = {"medium": {"classifier": "composite", "period": 14}}
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    gated = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None),
                       allowed_regimes=["trending_up"],
                       regime_windows_spec=spec)
    assert plain is not None and plain.get("trades", 0) > 0
    assert gated is not None
    assert gated.get("trades", -1) == 0, (
        "composite windows spec did not reach the Backtester — the gate "
        "classified with the legacy ADX vocabulary and let entries through")


def test_validate_candidate_normalizes_regime_windows_spec():
    c = {"name": "x", "regime_windows_spec": {"medium": 14}}
    assert ew.validate_candidate(c) is c
    assert c["regime_windows_spec"]["medium"]["classifier"] == "adx"
    assert c["regime_windows_spec"]["medium"]["period"] == 14

    c2 = {"name": "x",
          "regime_windows_spec": {"medium": {"classifier": "composite",
                                             "period": 14}},
          "allowed_regimes": ["trending_up_clean"]}
    assert ew.validate_candidate(c2) is c2
    assert c2["regime_windows_spec"]["medium"]["classifier"] == "composite"


def test_evaluate_window_validates_before_any_work():
    with pytest.raises(ValueError, match="silently dropped"):
        ew.evaluate_window(None, {"name": "x", "direction": "both"},
                           [("BTC/USDT", "1h")], "oos", 1000.0, {})


def test_validate_candidate_accepts_regime_directional_policy():
    c = {"name": "x", "direction": "both",
         "close_strategies": [{"name": "atr_stop",
                               "params": {"atr_mult": 2.0}}],
         "regime_directional_policy": {"trend_regime": {
             "trending_up": {"direction": "long"},
             "trending_down": {"direction": "both"}}}}
    assert ew.validate_candidate(c) is c
    assert c["regime_directional_policy"]["trend_regime"]["trending_up"] == {
        "direction": "long", "invert_signal": False}


def test_run_leg_omits_policy_kwargs_without_policy(monkeypatch):
    df = _synthetic_df()
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    import backtester as bt_mod
    real = bt_mod.Backtester
    calls = []

    def _spy(**kwargs):
        calls.append(kwargs)
        return real(**kwargs)

    monkeypatch.setattr(bt_mod, "Backtester", _spy, raising=True)
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None))
    assert leg is not None and calls
    assert "regime_directional_policy" not in calls[0]
    assert "regime_directional_certified" not in calls[0]


def test_run_leg_threads_regime_directional_policy_research_certified(monkeypatch):
    df = _synthetic_df()
    df = df.copy()
    df["regime"] = "trending_up"
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    import backtester as bt_mod
    real = bt_mod.Backtester
    calls = []

    def _spy(**kwargs):
        calls.append(kwargs)
        return real(**kwargs)

    monkeypatch.setattr(bt_mod, "Backtester", _spy, raising=True)
    policy = {"trend_regime": {"trending_up": {"direction": "short"}}}
    leg = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                     ("2026-01-01", None),
                     regime_directional_policy=policy)
    assert leg is not None and calls
    kw = calls[0]
    assert kw["regime_directional_policy"] == policy
    assert kw["regime_directional_certified"] is True
    assert kw["regime_enabled"] is True


def test_run_leg_policy_switches_plain_path_side(monkeypatch):
    df = _synthetic_df(n=240)
    df = df.copy()
    df["regime"] = "trending_up"
    import data_fetcher
    monkeypatch.setattr(data_fetcher, "load_cached_data",
                        lambda *a, **k: df, raising=True)
    plain = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None))
    gated = ew.run_leg(_FakeRegistry(), "alternator", None, "BTC/USDT", "1h",
                       ("2026-01-01", None),
                       regime_directional_policy={"trend_regime": {
                           "trending_up": {"direction": "short"}}})
    assert plain is not None and plain.get("trades", 0) > 0
    assert gated is not None and gated.get("trades", 0) > 0
    assert (gated["return_pct"], gated["max_dd_pct"]) != \
        (plain["return_pct"], plain["max_dd_pct"]), (
        "trending_up→short policy produced a leg identical to the ungated "
        "long run — the policy never reached the Backtester")


def test_evaluate_window_threads_regime_directional_policy(monkeypatch):
    seen = {}

    def _fake_run_leg(reg, name, params, symbol, timeframe, window, **kwargs):
        seen.update(kwargs)
        return {"sharpe": 1.0, "return_pct": 1.0, "max_dd_pct": 1.0,
                "ddadj": 1.0, "trades": 1, "bh_return_pct": 0.0,
                "span_days": 30.0}

    monkeypatch.setattr(ew, "run_leg", _fake_run_leg, raising=True)
    monkeypatch.setattr(ew, "incumbent_bars", lambda legs: {}, raising=True)
    monkeypatch.setattr(ew, "compute_incumbent_legs",
                        lambda *a, **k: {}, raising=True)
    policy = {"trend_regime": {"trending_up": {"direction": "long"}}}
    cand = {"name": "x", "direction": "both",
            "close_strategies": [{"name": "atr_stop",
                                  "params": {"atr_mult": 2.0}}],
            "regime_directional_policy": policy}
    ew.evaluate_window(None, cand, [("BTC/USDT", "1h")], "oos", 1000.0, {})
    assert seen.get("regime_directional_policy") == {"trend_regime": {
        "trending_up": {"direction": "long", "invert_signal": False}}}
