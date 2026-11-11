"""Live-config regime windows through the real --config single path."""

import json
import sys

import pandas as pd
import pytest

import run_backtest
from backtester import Backtester, CloseCapabilityError
from regime import bounded_window_labels, prepare_check_regime, valid_labels_for_classifier
from regime_label_columns import (
    attach_regime_label_columns,
    column_name,
    feature_label_kwargs,
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


def test_prepared_atr_window_is_admitted(tmp_path):
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


def test_default_directional_selector_keeps_the_primary_stamp():
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
    assert opens[0]["directional"] == df[column_name("medium")].iloc[220]
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


_PARITY_REGIME = {"enabled": True, "period": 14, "adx_threshold": 20, "windows": {
    "medium": {"classifier": "adx", "period": 14},
    "fast": {"classifier": "adx", "period": 5},
    "slow": {"classifier": "adx", "period": 40},
}}
_PARITY_POLICY = {"trend_regime": {
    "trending_down": {"direction": "short", "invert_signal": True}}}


def _parity_frame_df(n=330):
    import numpy as np
    i = np.arange(n)
    close = 1000 + 80 * np.sin(i / 14.0) + 30 * np.sin(i / 3.1)
    return pd.DataFrame(
        {"open": close, "high": close + 2, "low": close - 2, "close": close,
         "volume": 1000.0},
        index=pd.date_range("2024-01-01", periods=n, freq="1h"))


def _parity_run(selectors, regime=None):
    import numpy as np
    from parity_diff import ParityConfig, compute_parity_frame, summarize
    from registry_loader import load_registry

    reg = load_registry("futures")
    name = "_parity_label_windows_alternating"

    def alternating(frame):
        hours = frame.index.as_unit("ns").view("int64") // 3_600_000_000_000
        return frame.assign(signal=np.where((hours // 7) % 2 == 0, 1, -1))

    reg.STRATEGY_REGISTRY[name] = {"fn": alternating, "description": "test-only",
                                   "default_params": {}}
    try:
        plan = resolve_feature_windows(selectors, regime or _PARITY_REGIME)
        cfg = ParityConfig(
            strategy_name=name, registry="futures", regime_enabled=True,
            direction="long", regime_directional_policy=_PARITY_POLICY,
            regime_windows_spec=plan["windows_spec"], regime_label_plan=plan)
        frame = compute_parity_frame(_parity_frame_df(), cfg=cfg,
                                     window=plan["limit"] + 5)
        return plan, frame, summarize(frame)
    finally:
        del reg.STRATEGY_REGISTRY[name]


def _labels(plan, *windows):
    labeled, _ = attach_regime_label_columns(_parity_frame_df(), plan)
    return [labeled[column_name(w)] for w in windows]


def test_parity_named_gate_keeps_the_primary_directional_label():
    plan, frame, summary = _parity_run({"regime_gate_window": "fast"})
    assert plan["windows"]["directional"] == "medium"
    medium, fast = _labels(plan, "medium", "fast")
    scored = pd.DatetimeIndex(frame["ts"])
    differs = ((medium != fast)
               & ((medium == "trending_down") | (fast == "trending_down"))).loc[scored]
    assert differs.any()
    assert summary["clean"], frame[~frame["match"]].head()
    assert list(frame["live_directional_regime"]) == list(medium.shift(1).loc[scored])
    assert (frame["live_open_action"] == "short").any()


def test_parity_named_directional_and_atr_windows_against_the_default_run():
    _, default, _ = _parity_run({})

    case = "test_parity_named_directional_window_reads_that_window_on_both_sides"
    plan, named, summary = _parity_run({"regime_directional_window": "slow"})
    assert summary["clean"], f"{case}: {named[~named['match']].head()}"
    slow, = _labels(plan, "slow")
    scored = pd.DatetimeIndex(named["ts"])
    assert list(named["live_directional_regime"]) == list(slow.shift(1).loc[scored]), case
    assert list(named["live_directional_regime"]) != list(default["live_directional_regime"]), case
    assert list(named["live_signal"]) != list(default["live_signal"]), case

    case = "test_parity_named_atr_window_keeps_the_primary_directional_label"
    plan, named_atr, summary = _parity_run({"regime_atr_window": "fast"})
    assert plan["windows"]["directional"] == "medium", case
    assert summary["clean"], f"{case}: {named_atr[~named_atr['match']].head()}"
    assert list(named_atr["bt_signal"]) == list(default["bt_signal"]), case
    assert list(named_atr["live_signal"]) == list(default["live_signal"]), case




def _composite_trail_backtester(primary_classifier, atr_classifier):
    composite = {"adx": 20, "range_pct": 0.03, "return_pct": 0.02}
    windows = {
        "medium": {"classifier": primary_classifier, "period": 350},
        "trail": {"classifier": atr_classifier, "period": 350},
    }
    for w in windows.values():
        if w["classifier"] == "composite":
            w["thresholds"] = dict(composite)
    plan = resolve_feature_windows(
        {"regime_atr_window": "trail"}, {"enabled": True, "windows": windows})
    labels = sorted(valid_labels_for_classifier("composite"))
    return Backtester(
        initial_capital=1000, platform="hyperliquid", strategy_type="perps",
        regime_enabled=True, regime_windows_spec=plan["windows_spec"],
        trailing_stop_atr_mult_regime={
            "trend_regime": {label: {"atr_multiple": 2.5} for label in labels}},
        **feature_label_kwargs(plan),
    )


def test_regime_stop_block_uses_the_atr_window_vocabulary():
    bt = _composite_trail_backtester("adx", "composite")
    owner = bt._stop_context.resolved_stop_owner
    assert owner["name"] == "trailing_atr_regime"
    composite = tuple(sorted(valid_labels_for_classifier("composite")))
    assert tuple(owner["parameters"]["regime_labels"]) == composite
    assert tuple(bt._regime_atr_labels) == composite
    with pytest.raises(CloseCapabilityError) as refused:
        _composite_trail_backtester("composite", "adx")
    assert refused.value.reason_code == "MISSING_STOP_INPUT"
