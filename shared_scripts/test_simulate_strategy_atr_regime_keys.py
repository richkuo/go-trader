import os
import sys
import time

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from shared_tools.conftest import load_module

_SIMULATE_STRATEGY = load_module("_simulate_strategy_atr_keys_test", os.path.join(ROOT, "shared_scripts", "simulate_strategy.py"))
_resolve_atr_regime_stop = _SIMULATE_STRATEGY._resolve_atr_regime_stop
_run_payload = _SIMULATE_STRATEGY._run_payload

SL_CANON = "stop_loss_atr_mult_regime"
SL_LEGACY = "stop_loss_atr_regime"
TRAIL_CANON = "trailing_stop_atr_mult_regime"
TRAIL_V18 = "trail_stop_atr_regime"
TRAIL_V17 = "trailing_stop_atr_regime"


def _block(mult: float) -> dict:
    return {"trend_regime": {
        "trending_up": {"atr_multiple": mult},
        "trending_down": {"atr_multiple": mult},
        "ranging": {"atr_multiple": mult},
    }}


A = _block(2.0)
B = _block(4.0)


def test_atr_regime_stop_spellings_resolve_merge_or_raise(subtests):
    with subtests.test(case="missing_field_resolves_to_none"):
        assert _resolve_atr_regime_stop({}, SL_CANON) is None
        assert _resolve_atr_regime_stop({}, TRAIL_CANON) is None

    resolved = [
        (SL_CANON, {SL_CANON: A}), (SL_CANON, {SL_LEGACY: A}),
        (TRAIL_CANON, {TRAIL_CANON: A}), (TRAIL_CANON, {TRAIL_V18: A}), (TRAIL_CANON, {TRAIL_V17: A}),
        (SL_CANON, {SL_LEGACY: A, SL_CANON: A}),
        (SL_CANON, {SL_CANON: A, SL_LEGACY: A}),
        (TRAIL_CANON, {TRAIL_CANON: A, TRAIL_V18: A, TRAIL_V17: A}),
        (TRAIL_CANON, {TRAIL_V17: A, TRAIL_V18: A}),
    ]
    for canon, cfg in resolved:
        with subtests.test(case="resolves", canon=canon, keys=list(cfg)):
            assert _resolve_atr_regime_stop(cfg, canon) == A, (canon, list(cfg))

    conflicting = [
        (SL_CANON, {SL_LEGACY: B, SL_CANON: A}),
        (SL_CANON, {SL_CANON: A, SL_LEGACY: B}),
        (TRAIL_CANON, {TRAIL_CANON: A, TRAIL_V18: B}),
        (TRAIL_CANON, {TRAIL_V17: A, TRAIL_V18: B}),
        (TRAIL_CANON, {TRAIL_V18: B, TRAIL_V17: A}),
    ]
    for canon, cfg in conflicting:
        with subtests.test(case="conflict_raises", canon=canon, keys=list(cfg)), pytest.raises(ValueError):
            _resolve_atr_regime_stop(cfg, canon)


def _payload(cfg: dict) -> dict:
    base = int(time.time()) - 3600 * 60
    candles = [
        {"time": base + i * 3600, "open": 100 + i, "high": 101 + i,
         "low": 99 + i, "close": 100 + i, "volume": 10}
        for i in range(60)
    ]
    full = {
        "type": "perps", "platform": "hyperliquid", "symbol": "BTC/USDC",
        "timeframe": "1h", "open_strategy": {"name": "hold", "params": {}},
        "initial_capital": 1000, "stop_units": "live_percent",
        "leverage_source": "strategy_config",
    }
    full.update(cfg)
    return _run_payload({"candles": candles, "configs": [{"label": "x", "config": full}]})


def test_simulation_reports_conflicts_as_json_and_runs_agreeing_spellings(subtests):
    with subtests.test(case="conflict_surfaces_as_a_json_error_not_a_crash"):
        out = _payload({SL_LEGACY: A, SL_CANON: B})
        assert "conflicting ATR-regime stop keys" in out["error"]
        assert out["markers"] == {}

    with subtests.test(case="agreeing_spellings_simulate_normally"):
        out = _payload({SL_LEGACY: A, SL_CANON: A,
                        "regime": {"enabled": True, "period": 14, "adx_threshold": 20}})
        assert not out.get("error")
        assert "x" in out["markers"]
