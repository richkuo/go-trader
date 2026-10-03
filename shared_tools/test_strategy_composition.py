from pathlib import Path

import pandas as pd
import pytest

from shared_tools.conftest import load_module

_STRATEGY_COMPOSITION = load_module("_strategy_composition_test", Path(__file__).with_name("strategy_composition.py"))
evaluate_open_close = _STRATEGY_COMPOSITION.evaluate_open_close
finalize_decision = _STRATEGY_COMPOSITION.finalize_decision
strip_unsupported_position_context = _STRATEGY_COMPOSITION.strip_unsupported_position_context


@pytest.mark.parametrize("close_results,want_fraction,want_fill", [
    ({"tier": {"close_fraction": 0.5, "tier_fill_price": 104.0}}, 0.5, 104.0),
    ({"tier": {"close_fraction": 0.5, "tier_fill_price": 104.0},
      "stop": {"close_fraction": 1.0}}, 1.0, None),
    ({"tier": {"close_fraction": 0.0, "tier_fill_price": 104.0}}, 0.0, None),
])
def test_close_tier_fill_price_follows_the_winning_close(close_results, want_fraction, want_fill):
    df = pd.DataFrame({"close": [100, 106]})

    def apply_strategy(name, data, params=None):
        result = data.copy()
        result["signal"] = 0
        return result

    evaluation = evaluate_open_close(
        apply_strategy,
        lambda name: None,
        df,
        positional_strategy="open",
        open_strategy="open",
        close_strategies=list(close_results),
        position_side="long",
        position_ctx={"side": "long", "avg_cost": 100, "risk_anchor_price": 99, "tp_model": "resting_limit"},
        close_evaluate=lambda name, position, market, params: close_results[name],
    )
    decision = finalize_decision(evaluation, position_side="long")

    def legacy_close(df, avg_cost=None):
        return df

    assert decision["close_fraction"] == want_fraction
    assert decision.get("close_tier_fill_price") == want_fill
    assert strip_unsupported_position_context(
        legacy_close, {"avg_cost": 100, "risk_anchor_price": 99, "tp_model": "resting_limit", "lookback": 5},
    ) == {"avg_cost": 100, "lookback": 5}


def _signal_frame(signal):
    df = pd.DataFrame({"close": [100.0, 101.0], "signal": [0, signal]})
    return df


def _decide(raw_signal, position_side, invert, close_fraction=None):
    def apply_strategy(name, data, params=None):
        out = data.copy()
        out["signal"] = raw_signal
        return out

    def close_evaluate(name, position, market, params):
        if close_fraction is None:
            raise ValueError(f"Unknown close strategy: {name}")
        return {"close_fraction": close_fraction}

    evaluation = evaluate_open_close(
        apply_strategy,
        lambda name: None,
        _signal_frame(raw_signal),
        positional_strategy="open",
        open_strategy="open",
        close_strategies=None if close_fraction is None else ["registry_close"],
        position_side=position_side,
        close_evaluate=close_evaluate,
        invert_open_signal=invert,
    )
    return finalize_decision(evaluation, position_side, invert_open_signal=invert)


@pytest.mark.parametrize("raw,side,invert,registry_fraction,want_signal,want_fraction", [
    (1, "long", True, 0.4, -1, 0.4),
    (-1, "short", True, 1.0, 1, 1.0),
    (1, "", True, None, -1, 0.0),
    (-1, "short", True, None, 1, 1.0),
    (1, "short", True, None, 0, 0.0),
    (1, "long", False, 0.4, -1, 0.4),
    (-1, "short", False, 1.0, 1, 1.0),
    (1, "", False, None, 1, 0.0),
    (-1, "short", False, None, 0, 0.0),
    (1, "short", False, None, 1, 1.0),
])
def test_invert_open_signal_table(raw, side, invert, registry_fraction, want_signal, want_fraction):
    decision = _decide(raw, side, invert, registry_fraction)
    assert decision["signal"] == want_signal
    assert decision["close_fraction"] == want_fraction
    assert decision.get("open_signal_inverted", False) is invert
