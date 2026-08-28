
import importlib.util
from pathlib import Path

import pytest


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_exitq_close_registry", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def reg():
    return _load_close_registry()


def test_defaults_are_noops(reg):
    pos = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0,
           "entry_atr": 2.0, "bars_held": 50}
    mkt = {"mark_price": 80.0, "atr": 2.0, "zscore": 5.0}
    for name in ("time_stop", "atr_stop", "zscore_target"):
        out = reg.evaluate(name, pos, mkt, None)
        assert out["close_fraction"] == 0.0, name
        assert out["reason"] == "noop:disabled", name


def test_time_stop_fires_at_threshold(reg):
    pos = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "bars_held": 3}
    out = reg.evaluate("time_stop", pos, {"mark_price": 100.0}, {"max_bars": 3})
    assert out["close_fraction"] == 1.0
    assert out["reason"] == "time_stop:3"


def test_time_stop_within_window_holds(reg):
    pos = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "bars_held": 2}
    out = reg.evaluate("time_stop", pos, {"mark_price": 100.0}, {"max_bars": 3})
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:within_window"


def test_time_stop_missing_context_fails_safe(reg):
    pos = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0}
    out = reg.evaluate("time_stop", pos, {"mark_price": 100.0}, {"max_bars": 3})
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_bars_held"


def test_atr_stop_long_hit_and_boundary(reg):
    params = {"atr_mult": 2.0}
    base = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 2.0}
    assert reg.evaluate("atr_stop", base, {"mark_price": 96.0}, params)["close_fraction"] == 1.0
    assert reg.evaluate("atr_stop", base, {"mark_price": 96.5}, params)["close_fraction"] == 0.0


def test_atr_stop_short_mirrors(reg):
    params = {"atr_mult": 2.0}
    base = {"side": "short", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 2.0}
    assert reg.evaluate("atr_stop", base, {"mark_price": 104.0}, params)["close_fraction"] == 1.0
    assert reg.evaluate("atr_stop", base, {"mark_price": 103.0}, params)["close_fraction"] == 0.0


def test_atr_stop_source_entry_vs_live(reg):
    base = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 2.0}
    mkt = {"mark_price": 95.0, "atr": 3.0}
    assert reg.evaluate("atr_stop", base, mkt, {"atr_mult": 2.0, "atr_source": "entry"})["close_fraction"] == 1.0
    assert reg.evaluate("atr_stop", base, mkt, {"atr_mult": 2.0, "atr_source": "live"})["close_fraction"] == 0.0


def test_atr_stop_missing_atr_fails_safe(reg):
    base = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 0.0}
    out = reg.evaluate("atr_stop", base, {"mark_price": 50.0}, {"atr_mult": 2.0})
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_entry_atr"
    out_live = reg.evaluate("atr_stop", base, {"mark_price": 50.0}, {"atr_mult": 2.0, "atr_source": "live"})
    assert out_live["reason"] == "noop:missing_live_atr"


def test_zscore_target_long_and_short(reg):
    params = {"lookback": 20, "z_target": 2.0}
    long_pos = {"side": "long", "current_quantity": 1.0}
    short_pos = {"side": "short", "current_quantity": 1.0}
    assert reg.evaluate("zscore_target", long_pos, {"mark_price": 100, "zscore": 2.0}, params)["close_fraction"] == 1.0
    assert reg.evaluate("zscore_target", long_pos, {"mark_price": 100, "zscore": 1.9}, params)["close_fraction"] == 0.0
    assert reg.evaluate("zscore_target", short_pos, {"mark_price": 100, "zscore": -2.0}, params)["close_fraction"] == 1.0
    assert reg.evaluate("zscore_target", short_pos, {"mark_price": 100, "zscore": -1.0}, params)["close_fraction"] == 0.0


def test_zscore_target_missing_or_nan_fails_safe(reg):
    params = {"lookback": 20, "z_target": 2.0}
    pos = {"side": "long", "current_quantity": 1.0}
    assert reg.evaluate("zscore_target", pos, {"mark_price": 100}, params)["reason"] == "noop:missing_zscore"
    assert reg.evaluate("zscore_target", pos, {"mark_price": 100, "zscore": float("nan")}, params)["reason"] == "noop:missing_zscore"
