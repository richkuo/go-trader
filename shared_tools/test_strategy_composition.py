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
