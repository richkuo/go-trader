import importlib.util
from pathlib import Path

import pandas as pd

from strategy_composition import (
    compose_signal,
    evaluate_open_close,
    finalize_decision,
    max_close_fraction,
)


def test_compose_signal_close_before_open():
    assert compose_signal("short", 1.0, "long") == -1
    assert compose_signal("long", 1.0, "short") == 1
    assert compose_signal("long", 1.0, "") == 0
    assert compose_signal("short", 0.0, "long") == 0


def test_max_close_fraction_clamps_and_is_order_independent():
    fraction, strategy = max_close_fraction([
        type("E", (), {"strategy": "a", "close_fraction": 0.25})(),
        type("E", (), {"strategy": "b", "close_fraction": 1.2})(),
        type("E", (), {"strategy": "c", "close_fraction": -1})(),
    ])
    assert fraction == 1.0
    assert strategy == "b"


def test_evaluate_open_close_reuses_legacy_strategy_once():
    calls = []
    df = pd.DataFrame({"close": [100, 101]})

    def get_strategy(name):
        assert name == "legacy"

    def apply_strategy(name, data, params=None):
        calls.append(name)
        result = data.copy()
        result["signal"] = [0, -1]
        return result

    evaluation = evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="legacy",
        open_strategy=None,
        close_strategies=None,
        position_side="long",
        disable_implicit_close=False,
    )
    decision = finalize_decision(evaluation, position_side="long")

    assert calls == ["legacy"]
    assert decision["open_strategy"] == "legacy"
    assert decision["close_strategies"] == ["legacy"]
    assert decision["open_action"] == "short"
    assert decision["close_fraction"] == 1.0
    assert decision["signal"] == -1


def test_disable_implicit_close_ignores_reversal_without_explicit_close():
    df = pd.DataFrame({"close": [100, 101]})

    def get_strategy(name):
        assert name == "legacy"

    def apply_strategy(name, data, params=None):
        result = data.copy()
        result["signal"] = [0, -1]
        return result

    evaluation = evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="legacy",
        open_strategy=None,
        close_strategies=None,
        position_side="long",
        disable_implicit_close=True,
    )
    decision = finalize_decision(evaluation, position_side="long")

    assert decision["close_strategies"] == []
    assert decision["close_fraction"] == 0.0
    assert decision["signal"] == 0


def test_evaluate_open_close_passes_position_ctx_to_close_only():
    calls = []
    df = pd.DataFrame({"close": [100, 106]})

    def get_strategy(name):
        assert name in {"open", "close"}

    def apply_strategy(name, data, params=None):
        calls.append((name, dict(params or {})))
        result = data.copy()
        result["signal"] = 0
        if name == "close":
            result["close_fraction"] = 1.0 if params and params.get("avg_cost") == 100 else 0.0
        return result

    evaluation = evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="legacy",
        open_strategy="open",
        close_strategies=["close"],
        position_side="long",
        disable_implicit_close=False,
        params={"open_only": 1},
        position_ctx={
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 0.5,
            "initial_quantity": 1.0,
            "entry_atr": 12.5,
        },
    )
    decision = finalize_decision(evaluation, position_side="long")

    assert calls[0] == ("open", {"open_only": 1})
    assert calls[1] == (
        "close",
        {
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 0.5,
            "initial_quantity": 1.0,
            "entry_atr": 12.5,
        },
    )
    assert decision["close_fraction"] == 1.0
    assert decision["signal"] == -1


def test_evaluate_open_close_reruns_same_strategy_when_close_params_differ():
    calls = []
    df = pd.DataFrame({"close": [100, 106]})

    def get_strategy(name):
        assert name == "same"

    def apply_strategy(name, data, params=None):
        calls.append(dict(params or {}))
        result = data.copy()
        result["signal"] = [0, 0]
        result["close_fraction"] = 1.0 if params and params.get("avg_cost") == 100 else 0.0
        return result

    evaluation = evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="same",
        open_strategy=None,
        close_strategies=None,
        position_side="long",
        disable_implicit_close=False,
        params={"lookback": 5},
        position_ctx={"avg_cost": 100},
    )

    assert calls == [{"lookback": 5}, {"lookback": 5, "avg_cost": 100}]
    assert finalize_decision(evaluation, position_side="long")["close_fraction"] == 1.0


def test_tp_at_pct_position_aware_close_handles_missing_and_hit():
    strategy_path = Path(__file__).resolve().parents[1] / "shared_strategies" / "spot" / "strategies.py"
    spec = importlib.util.spec_from_file_location("_spot_strategies_for_tp_test", strategy_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    df = pd.DataFrame({"close": [100, 106]})
    missing = mod.apply_strategy("tp_at_pct", df)
    assert missing.iloc[-1]["close_fraction"] == 0.0
    assert missing.iloc[-1]["reason"] == "noop:missing_position"

    legacy = mod.apply_strategy(
        "sma_crossover",
        df,
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5},
    )
    assert "signal" in legacy.columns

    hit = mod.apply_strategy(
        "tp_at_pct",
        df,
        {
            "pct": 0.05,
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 0.5,
            "initial_quantity": 1.0,
            "entry_atr": 12.5,
        },
    )
    assert hit.iloc[-1]["close_fraction"] == 1.0
    assert hit.iloc[-1]["reason"] == "tp_at_pct:hit"
