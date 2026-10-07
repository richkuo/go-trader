"""Live-config regime windows through the real --config single path."""

import json
import sys

import pandas as pd
import pytest

import run_backtest
from backtester import Backtester
from regime import bounded_window_labels, prepare_check_regime
from regime_label_columns import (
    attach_regime_label_columns,
    column_name,
    resolve_feature_windows,
)


def _frame(n=260, freq="1h", start="2024-01-01", step=None):
    idx = pd.date_range(start, periods=n, freq=freq)
    close = []
    px = 100.0
    for i in range(n):
        if step is None:
            px += 1.2 if i < n // 2 else -0.4
        else:
            px += step(i)
        close.append(px)
    s = pd.Series(close, index=idx)
    return pd.DataFrame({
        "open": s.shift(1).fillna(s.iloc[0]),
        "high": s + 1,
        "low": s - 1,
        "close": s,
        "volume": 10.0,
    })


def _config(strategy):
    return {
        "config_version": 15,
        "regime": {
            "enabled": True,
            "period": 14,
            "adx_threshold": 20,
            "windows": {
                "medium": {"classifier": "adx", "period": 14},
                "fast": {"classifier": "adx", "period": 8},
            },
        },
        "strategies": [strategy],
    }


def _strategy(**extra):
    sc = {
        "id": "spot-sma",
        "type": "spot",
        "platform": "binanceus",
        "args": ["sma_crossover", "BTC/USDT", "1h"],
        "open_strategy": {"name": "sma_crossover",
                          "params": {"fast_period": 5, "slow_period": 20}},
        "allowed_regimes": ["trending_up"],
    }
    sc.update(extra)
    return sc


def _write(tmp_path, doc):
    path = tmp_path / "config.json"
    path.write_text(json.dumps(doc))
    return str(path)


def _run_config(monkeypatch, path, strategy_id="spot-sma", frame=None):
    frame = _frame() if frame is None else frame
    monkeypatch.setattr(run_backtest, "load_cached_data", lambda *a, **k: frame.copy())
    monkeypatch.setattr("backtester.store_backtest_result", lambda *a, **k: None)
    captured = {}
    real = run_backtest.run_single_backtest

    def wrap(*args, **kwargs):
        captured["result"] = real(*args, **kwargs)
        captured["kwargs"] = kwargs
        return captured["result"]

    monkeypatch.setattr(run_backtest, "run_single_backtest", wrap)
    monkeypatch.setattr(sys, "argv", [
        "run_backtest.py", "--config", path, "--strategy", strategy_id,
        "--mode", "single", "--symbol", "BTC/USDT", "--timeframe", "1h",
    ])
    run_backtest.main()
    return captured["result"], captured["kwargs"]


def _entries(result):
    return [t["entry_date"] for t in (result or {}).get("trades") or []]


def test_named_gate_changes_the_trade_list(monkeypatch, tmp_path):
    named_path = tmp_path / "named.json"
    primary_path = tmp_path / "primary.json"
    named_path.write_text(json.dumps(_config(_strategy(
        regime_gate_window="fast", regime_gate_on_failure="closed"))))
    primary_path.write_text(json.dumps(_config(_strategy(
        regime_gate_on_failure="closed"))))
    chop = _frame(n=320, step=lambda i: 2.5 if (i // 12) % 2 == 0 else -2.5)
    named_result, _ = _run_config(monkeypatch, str(named_path), frame=chop)
    primary_result, _ = _run_config(monkeypatch, str(primary_path), frame=chop)
    assert named_result["regime_label_windows"]["windows"]["gate"]["window"] == "fast"
    assert named_result["regime_label_windows"]["source"] == "bounded_window_labels"
    assert _entries(named_result) != _entries(primary_result)


def test_unknown_window_and_gate_vocabulary(tmp_path):
    bad = _write(tmp_path, _config(_strategy(regime_gate_window="missing")))
    with pytest.raises(ValueError, match="regime_gate_window='missing'"):
        run_backtest.load_strategy_config(bad, "spot-sma")
    path = tmp_path / "vocab.json"
    path.write_text(json.dumps(_config(_strategy(
        regime_gate_window="fast", allowed_regimes=["trending_up_clean"]))))
    with pytest.raises(ValueError, match="gate window 'fast'"):
        run_backtest.load_strategy_config(str(path), "spot-sma")


def test_prepared_atr_window_is_admitted_and_a_missing_column_is_refused(tmp_path):
    doc = _config(_strategy(
        regime_atr_window="fast",
        trailing_stop_atr_mult_regime={"use_defaults": True},
        allowed_regimes=None,
    ))
    doc["strategies"][0].pop("allowed_regimes")
    path = tmp_path / "atr.json"
    path.write_text(json.dumps(doc))
    loaded = run_backtest.load_strategy_config(str(path), "spot-sma")
    evidence = loaded["capability_context"].input_evidence["atr_regime_window"]
    assert evidence["status"] == "verified"
    assert evidence["value"] == "fast"
    raw = run_backtest.resolve_raw_config_stops(doc, "spot-sma", "ledger")
    assert raw["stop_context"].input_evidence["atr_regime_window"]["status"] == "missing"

    plan = loaded["regime_label_windows"]
    df = _frame()
    df["signal"] = 0
    df.loc[df.index[220], "signal"] = 1
    attach_regime_label_columns(df, plan)
    dropped = df.drop(columns=[column_name("fast")])
    bt = Backtester(
        initial_capital=1000, platform="binanceus", regime_enabled=True,
        regime_label_columns={"atr": column_name("fast"), "atr_named": True,
                              "gate": column_name("medium"),
                              "directional": column_name("medium"),
                              "directional_named": False,
                              "payload_row": "decision_bar", "gate_row": "decision_bar"},
        trailing_stop_atr_mult_regime={"use_defaults": True},
        capability_context=loaded["capability_context"],
        strategy_type="spot",
    )
    with pytest.raises(ValueError, match="refusing missing regime label column"):
        bt.run(dropped, strategy_name="sma_crossover", symbol="BTC/USDT",
               timeframe="1h", save=False)


def test_default_directional_selector_keeps_the_gate_stamp():
    regime = {"enabled": True, "period": 14, "windows": {
        "medium": {"classifier": "adx", "period": 14},
        "fast": {"classifier": "adx", "period": 8},
    }}
    plan = resolve_feature_windows(
        {"regime_gate_window": "fast"}, regime)
    assert plan["named"]["directional"] is False
    df = _frame()
    df["signal"] = 0
    df.loc[df.index[220], "signal"] = 1
    attach_regime_label_columns(df, plan)
    cols = {
        "gate": column_name("fast"),
        "directional": column_name("medium"),
        "directional_named": False,
        "atr_named": False,
        "payload_row": "decision_bar",
        "gate_row": "decision_bar",
    }
    bt = Backtester(
        initial_capital=1000, platform="binanceus", regime_enabled=True,
        allowed_regimes=["trending_up", "trending_down", "ranging"],
        regime_label_columns=cols,
        regime_feature_labels={"atr": None, "directional": tuple(plan["vocabularies"]["directional"])},
        strategy_type="spot",
    )
    bt.run(df, strategy_name="hold", symbol="BTC/USDT", timeframe="1h", save=False)
    opens = [row for row in bt._regime_stamp_trace if row["event"] == "stamp"]
    assert opens
    assert opens[0]["directional"] == opens[0]["gate"]
    assert opens[0]["gate"] != ""


def test_labels_match_prepare_and_the_helper_does_not_shift():
    regime = {"enabled": True, "period": 14, "windows": {
        "medium": {"classifier": "adx", "period": 14},
        "fast": {"classifier": "adx", "period": 8},
    }}
    plan = resolve_feature_windows({"regime_gate_window": "fast"}, regime)
    df = _frame()
    raw = df.copy()
    attach_regime_label_columns(df, plan)
    col = column_name("fast")
    limit = plan["limit"]
    for i in range(limit, len(df), 20):
        payload, _, _ = prepare_check_regime(
            raw.iloc[i - limit + 1:i + 1], regime_enabled=True,
            windows_spec=plan["windows_spec"])
        live = str((payload.get("fast") or {}).get("regime") or "")
        assert df[col].iloc[i] == live
        decision = df[col].shift(1).iloc[i]
        prior, _, _ = prepare_check_regime(
            raw.iloc[i - limit:i], regime_enabled=True,
            windows_spec=plan["windows_spec"])
        assert decision == str((prior.get("fast") or {}).get("regime") or "")
    labeled = raw.copy()
    bounded_window_labels(
        labeled, period=14, adx_threshold=20, windows_spec=plan["windows_spec"],
        limit=limit, columns={col: "fast"})
    assert list(labeled[col]) == list(df[col])


def test_higher_timeframe_shifts_once_before_the_fill():
    regime = {"enabled": True, "period": 14, "timeframe": "4h", "windows": {
        "medium": {"classifier": "adx", "period": 14},
    }}
    plan = resolve_feature_windows({}, regime)
    trade = _frame(n=1200, freq="1h")
    regime_frame = _frame(n=300, freq="4h")
    labeled = regime_frame.copy()
    col = column_name(plan["windows"]["gate"])
    bounded_window_labels(
        labeled, period=14, adx_threshold=20, windows_spec=plan["windows_spec"],
        limit=plan["limit"], columns={col: plan["windows"]["gate"]})
    out = trade.copy()
    attach_regime_label_columns(out, plan, regime_frame=regime_frame)
    aligned = labeled[[col]].shift(1).reindex(trade.index, method="ffill")[col].fillna("").map(
        lambda v: str(v or "").strip())
    assert list(out[col]) == list(aligned)
    filled = [i for i, value in enumerate(labeled[col]) if value]
    assert filled
    assert out[col].loc[labeled.index[filled[0]]] != labeled[col].iloc[filled[0]]


def test_callers_pass_the_plan_or_refuse(tmp_path, monkeypatch):
    from eval_windows import validate_candidate
    from optimizer import walk_forward_optimize

    with pytest.raises(ValueError, match="regime_gate_window='fast'"):
        validate_candidate({"name": "sma_crossover", "regime_gate_window": "fast"})
    plan = resolve_feature_windows({"regime_gate_window": "fast"}, {
        "enabled": True, "windows": {"fast": {"classifier": "adx", "period": 8},
                                     "medium": {"classifier": "adx", "period": 14}},
    })
    with pytest.raises(ValueError, match="refusing missing regime label column"):
        walk_forward_optimize(
            _frame(n=80, freq="1D"), "sma_crossover", {"fast_period": [5]},
            n_splits=1, regime_enabled=True, regime_label_plan=plan)

    import os
    root = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
    sys.path.insert(0, os.path.join(root, "shared_scripts"))
    import simulate_strategy as sim
    payload = {
        "candles": [
            {"time": 1700000000 + i * 3600, "open": 1, "high": 2, "low": 0.5,
             "close": 1.2, "volume": 1} for i in range(30)
        ],
        "configs": [{
            "label": "live",
            "config": {
                "type": "spot", "platform": "binanceus", "symbol": "BTC/USDT",
                "timeframe": "1h", "strategy": "sma_crossover",
                "open_strategy": {"name": "sma_crossover", "params": {}},
                "allowed_regimes": ["trending_up"],
                "regime": {"enabled": True, "timeframe": "4h", "period": 14},
            },
        }],
    }
    refused = sim._run_payload(payload)
    assert "regime.timeframe" in refused["live_refusal"]
