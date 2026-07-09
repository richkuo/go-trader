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
        if name in ("tp_at_pct", "tiered_tp_pct"):
            return {}
        raise ValueError("close missing")

    validate_close_strategy_names(
        ["tp_at_pct", "legacy_open_close"],
        get_open_strategy,
        get_close_strategy,
        lambda: ["legacy_open_close"],
        lambda: ["tiered_tp_pct"],
    )

    import pytest
    with pytest.raises(ValueError, match="Available close strategies.*fallback open strategies"):
        validate_close_strategy_names(
            ["missing"],
            get_open_strategy,
            get_close_strategy,
            lambda: ["legacy_open_close"],
            lambda: ["tiered_tp_pct"],
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
    assert hit["reason"] == "tiered_tp_pct:0.05"


def test_evaluate_open_close_injects_avwap_from_open_result():
    # #1196: the open strategy's `avwap` column (last bar) is exposed to close
    # evaluators as market["avwap"] so avwap_stop exits against the same line
    # the entry was built on.
    captured = []
    df = pd.DataFrame({"close": [100, 106]})

    def get_strategy(name):
        pass

    def apply_strategy(name, data, params=None):
        result = data.copy()
        result["signal"] = [0, 0]
        result["avwap"] = [float("nan"), 101.5]
        return result

    def close_evaluate(name, position, market, params=None):
        captured.append(dict(market))
        return {"close_fraction": 0.0, "reason": "noop"}

    caller_market = {"mark_price": 106}
    evaluate_open_close(
        apply_strategy,
        get_strategy,
        df,
        positional_strategy="legacy",
        open_strategy="open",
        close_strategies=["avwap_stop"],
        position_side="long",
        position_ctx={"side": "long", "avg_cost": 100, "current_quantity": 1.0},
        close_evaluate=close_evaluate,
        market_ctx=caller_market,
    )
    assert captured == [{"mark_price": 106, "avwap": 101.5}]
    # The caller's market_ctx dict is never mutated.
    assert caller_market == {"mark_price": 106}


def test_evaluate_open_close_skips_avwap_when_nan_or_absent():
    captured = []
    df = pd.DataFrame({"close": [100, 106]})

    def get_strategy(name):
        pass

    def close_evaluate(name, position, market, params=None):
        captured.append(dict(market))
        return {"close_fraction": 0.0, "reason": "noop"}

    def apply_nan_avwap(name, data, params=None):
        result = data.copy()
        result["signal"] = [0, 0]
        result["avwap"] = [float("nan"), float("nan")]
        return result

    evaluate_open_close(
        apply_nan_avwap, get_strategy, df,
        positional_strategy="legacy", open_strategy="open",
        close_strategies=["avwap_stop"], position_side="long",
        close_evaluate=close_evaluate, market_ctx={"mark_price": 106},
    )

    def apply_no_avwap(name, data, params=None):
        result = data.copy()
        result["signal"] = [0, 0]
        return result

    evaluate_open_close(
        apply_no_avwap, get_strategy, df,
        positional_strategy="legacy", open_strategy="open",
        close_strategies=["avwap_stop"], position_side="long",
        close_evaluate=close_evaluate, market_ctx={"mark_price": 106},
    )
    assert captured == [{"mark_price": 106}, {"mark_price": 106}]


# --------------------------------------------------------------------------
# #1196 review: warn once when avwap_stop is configured but no usable avwap
# context is ever produced by the open strategy (the exit can never fire).
# --------------------------------------------------------------------------

_AVWAP_WARN_MARK = "avwap_stop"


def _noop_close_evaluate(name, position, market, params=None):
    return {"close_fraction": 0.0, "reason": "noop"}


def _run_avwap_open_close(apply_strategy, close_strategies):
    df = pd.DataFrame({"close": [100, 106]})
    evaluate_open_close(
        apply_strategy,
        lambda name: None,
        df,
        positional_strategy="legacy",
        open_strategy="open",
        close_strategies=close_strategies,
        position_side="long",
        close_evaluate=_noop_close_evaluate,
        market_ctx={"mark_price": 106},
    )


def _apply_with_avwap(values):
    def _apply(name, data, params=None):
        result = data.copy()
        result["signal"] = [0] * len(result)
        result["avwap"] = values
        return result
    return _apply


def _apply_without_avwap(name, data, params=None):
    result = data.copy()
    result["signal"] = [0] * len(result)
    return result


def test_avwap_stop_warns_once_when_column_absent(capsys):
    _run_avwap_open_close(_apply_without_avwap, ["avwap_stop"])
    err = capsys.readouterr().err
    assert err.count(_AVWAP_WARN_MARK) == 1


def test_avwap_stop_warns_once_when_column_all_nan(capsys):
    _run_avwap_open_close(_apply_with_avwap([float("nan"), float("nan")]), ["avwap_stop"])
    err = capsys.readouterr().err
    assert err.count(_AVWAP_WARN_MARK) == 1


def test_avwap_stop_does_not_warn_when_avwap_present(capsys):
    _run_avwap_open_close(_apply_with_avwap([float("nan"), 101.5]), ["avwap_stop"])
    err = capsys.readouterr().err
    assert _AVWAP_WARN_MARK not in err


def test_avwap_stop_does_not_warn_when_not_configured(capsys):
    _run_avwap_open_close(_apply_without_avwap, ["tiered_tp_atr_live"])
    err = capsys.readouterr().err
    assert _AVWAP_WARN_MARK not in err


def test_reject_backtest_only_strategies_validates_and_refuses():
    import pytest
    from strategy_composition import reject_backtest_only_strategies

    def get_strategy(name):
        if name == "normal":
            return {"backtest_only": False}
        if name == "research":
            return {"backtest_only": True}
        raise ValueError(f"Unknown strategy: {name}")

    # Existence validation is preserved for normal entries…
    reject_backtest_only_strategies(["normal"], get_strategy)
    with pytest.raises(ValueError, match="Unknown strategy: missing"):
        reject_backtest_only_strategies(["missing"], get_strategy)
    # …and a backtest_only entry fails closed on the live path (#1138).
    with pytest.raises(ValueError, match="backtest_only"):
        reject_backtest_only_strategies(["normal", "research"], get_strategy)


def test_warn_deprecated_edge_strategies_warns_but_never_rejects(capsys):
    from strategy_composition import warn_deprecated_edge_strategies

    def get_strategy(name):
        if name == "clean":
            return {"edge_status": None}
        if name == "quarantined":
            return {"edge_status": "deprecated_m5"}
        raise ValueError(f"Unknown strategy: {name}")

    # Clean entries produce no warnings and no output.
    assert warn_deprecated_edge_strategies(["clean"], get_strategy) == []
    # A quarantined entry warns on stderr (stdout stays JSON-clean) — but the
    # call never raises: the strategy keeps loading and trading (#1275).
    warnings = warn_deprecated_edge_strategies(["clean", "quarantined"], get_strategy)
    assert len(warnings) == 1
    assert "quarantined" in warnings[0]
    assert "deprecate" in warnings[0]
    captured = capsys.readouterr()
    assert captured.out == ""
    assert "quarantined" in captured.err
    # Unknown names are skipped (existence is reject_backtest_only's job).
    assert warn_deprecated_edge_strategies(["missing"], get_strategy) == []


def test_warn_deprecated_edge_leaves_backtest_only_rejection_untouched():
    import pytest
    from strategy_composition import (
        reject_backtest_only_strategies,
        warn_deprecated_edge_strategies,
    )

    def get_strategy(name):
        if name == "research":
            return {"backtest_only": True, "edge_status": None}
        raise ValueError(f"Unknown strategy: {name}")

    # The #1275 warning path is advisory-only and must not soften the #1138
    # hard reject: backtest_only still fails closed on the live path.
    assert warn_deprecated_edge_strategies(["research"], get_strategy) == []
    with pytest.raises(ValueError, match="backtest_only"):
        reject_backtest_only_strategies(["research"], get_strategy)


def test_validate_close_strategy_names_rejects_backtest_only_open_fallback():
    import pytest

    def get_open_strategy(name):
        if name == "research_open":
            return {"backtest_only": True}
        raise ValueError("open missing")

    def get_close_strategy(name):
        raise ValueError("close missing")

    # The open-as-close fallback is a live path — backtest_only entries are
    # refused there too (#1138), with the same loud ValueError.
    with pytest.raises(ValueError, match="backtest_only"):
        validate_close_strategy_names(
            ["research_open"], get_open_strategy, get_close_strategy
        )
