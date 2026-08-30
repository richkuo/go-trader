import os
import sys
import time

import pytest

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
sys.path.insert(0, ROOT)
sys.path.insert(0, os.path.join(ROOT, "shared_tools"))
sys.path.insert(0, os.path.join(ROOT, "backtest"))

from simulate_strategy import _resolve_atr_regime_stop, _run_payload

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


def test_missing_field_resolves_to_none():
    assert _resolve_atr_regime_stop({}, SL_CANON) is None
    assert _resolve_atr_regime_stop({}, TRAIL_CANON) is None


@pytest.mark.parametrize("canon,key", [
    (SL_CANON, SL_CANON), (SL_CANON, SL_LEGACY),
    (TRAIL_CANON, TRAIL_CANON), (TRAIL_CANON, TRAIL_V18), (TRAIL_CANON, TRAIL_V17),
])
def test_each_spelling_resolves_on_its_own(canon, key):
    assert _resolve_atr_regime_stop({key: A}, canon) == A


@pytest.mark.parametrize("canon,cfg", [
    (SL_CANON, {SL_LEGACY: A, SL_CANON: A}),
    (SL_CANON, {SL_CANON: A, SL_LEGACY: A}),
    (TRAIL_CANON, {TRAIL_CANON: A, TRAIL_V18: A, TRAIL_V17: A}),
    (TRAIL_CANON, {TRAIL_V17: A, TRAIL_V18: A}),
])
def test_equivalent_spellings_merge_regardless_of_order(canon, cfg):
    assert _resolve_atr_regime_stop(cfg, canon) == A


@pytest.mark.parametrize("canon,cfg", [
    (SL_CANON, {SL_LEGACY: B, SL_CANON: A}),
    (SL_CANON, {SL_CANON: A, SL_LEGACY: B}),
    (TRAIL_CANON, {TRAIL_CANON: A, TRAIL_V18: B}),
    (TRAIL_CANON, {TRAIL_V17: A, TRAIL_V18: B}),
    (TRAIL_CANON, {TRAIL_V18: B, TRAIL_V17: A}),
])
def test_conflicting_spellings_raise(canon, cfg):
    with pytest.raises(ValueError):
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
        "initial_capital": 1000,
    }
    full.update(cfg)
    return _run_payload({"candles": candles, "configs": [{"label": "x", "config": full}]})


def test_conflict_surfaces_as_a_json_error_not_a_crash():
    out = _payload({SL_LEGACY: A, SL_CANON: B})
    assert "conflicting ATR-regime stop keys" in out["error"]
    assert out["markers"] == {}


def test_agreeing_spellings_simulate_normally():
    out = _payload({SL_LEGACY: A, SL_CANON: A})
    assert not out.get("error")
    assert "x" in out["markers"]
