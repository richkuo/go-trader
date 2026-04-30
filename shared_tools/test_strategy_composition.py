import importlib.util
from pathlib import Path

import pandas as pd

from strategy_composition import (
    compose_signal,
    evaluate_open_close,
    finalize_decision,
    max_close_fraction,
    validate_close_strategy_names,
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
    )
    decision = finalize_decision(evaluation, position_side="long")

    assert calls == ["legacy"]
    assert decision["open_strategy"] == "legacy"
    assert decision["close_strategies"] == ["legacy"]
    assert decision["open_action"] == "short"
    assert decision["close_fraction"] == 1.0
    assert decision["signal"] == -1


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


def test_evaluate_open_close_uses_close_registry_before_open_fallback():
    calls = []
    df = pd.DataFrame({"close": [100, 106]})

    def get_strategy(name):
        assert name == "open"

    def apply_strategy(name, data, params=None):
        calls.append(("open", name, dict(params or {})))
        result = data.copy()
        result["signal"] = [0, 0]
        return result

    def close_evaluate(name, position, market, params=None):
        calls.append(("close", name, dict(position), dict(market), dict(params or {})))
        return {"close_fraction": 1.0, "reason": "close:hit"}

    evaluation = evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="legacy",
        open_strategy="open",
        close_strategies=["tiered_tp_pct"],
        position_side="long",
        params={"lookback": 5},
        position_ctx={"side": "long", "avg_cost": 100, "current_quantity": 1.0},
        close_evaluate=close_evaluate,
        market_ctx={"mark_price": 106},
    )
    decision = finalize_decision(evaluation, position_side="long")

    assert calls == [
        ("open", "open", {"lookback": 5}),
        (
            "close",
            "tiered_tp_pct",
            {"side": "long", "avg_cost": 100, "current_quantity": 1.0},
            {"mark_price": 106},
            {},
        ),
    ]
    assert decision["close_strategy"] == "tiered_tp_pct"
    assert decision["signal"] == -1


def test_validate_close_strategy_names_reports_both_registries():
    def get_open_strategy(name):
        if name == "legacy_open_close":
            return {}
        raise ValueError("open missing")

    def get_close_strategy(name):
        if name == "tp_at_pct":
            return {}
        raise ValueError("close missing")

    validate_close_strategy_names(
        ["tp_at_pct", "legacy_open_close"],
        get_open_strategy,
        get_close_strategy,
        lambda: ["legacy_open_close"],
        lambda: ["tp_at_pct"],
    )

    import pytest
    with pytest.raises(ValueError, match="Available close strategies.*fallback open strategies"):
        validate_close_strategy_names(
            ["missing"],
            get_open_strategy,
            get_close_strategy,
            lambda: ["legacy_open_close"],
            lambda: ["tp_at_pct"],
        )


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
        params={"lookback": 5},
        position_ctx={"avg_cost": 100},
    )

    assert calls == [{"lookback": 5}, {"lookback": 5, "avg_cost": 100}]
    assert finalize_decision(evaluation, position_side="long")["close_fraction"] == 1.0


def test_tp_at_pct_position_aware_close_handles_missing_and_hit():
    strategy_path = Path(__file__).resolve().parents[1] / "shared_strategies" / "close" / "registry.py"
    spec = importlib.util.spec_from_file_location("_close_registry_for_tp_test", strategy_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    missing = mod.evaluate("tp_at_pct", {}, {"mark_price": 106}, {})
    assert missing["close_fraction"] == 0.0
    assert missing["reason"] == "noop:missing_position"

    strategy_path = Path(__file__).resolve().parents[1] / "shared_strategies" / "open" / "spot" / "strategies.py"
    spec = importlib.util.spec_from_file_location("_spot_strategies_for_tp_test", strategy_path)
    open_mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(open_mod)
    df = pd.DataFrame({"close": [100, 106]})
    legacy = open_mod.apply_strategy(
        "sma_crossover",
        df,
        {"side": "long", "avg_cost": 100, "current_quantity": 0.5},
    )
    assert "signal" in legacy.columns

    hit = mod.evaluate(
        "tp_at_pct",
        {
            "side": "long",
            "avg_cost": 100,
            "current_quantity": 0.5,
            "initial_quantity": 1.0,
            "entry_atr": 12.5,
        },
        {"mark_price": 106},
        {
            "pct": 0.05,
        },
    )
    assert hit["close_fraction"] == 1.0
    assert hit["reason"] == "tp_at_pct:hit"
