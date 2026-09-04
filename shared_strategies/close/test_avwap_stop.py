
import importlib.util
from pathlib import Path

import pytest


def _load_close_registry():
    path = Path(__file__).resolve().parent / "registry.py"
    spec = importlib.util.spec_from_file_location("_avwap_close_registry", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def reg():
    return _load_close_registry()


def _long_pos(**over):
    pos = {"side": "long", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 2.0}
    pos.update(over)
    return pos


def _short_pos(**over):
    pos = {"side": "short", "current_quantity": 1.0, "avg_cost": 100.0, "entry_atr": 2.0}
    pos.update(over)
    return pos


@pytest.mark.parametrize("side,mark,expected", [
    ("long", 99.0, 1.0),
    ("long", 99.5, 0.0),
    ("short", 101.0, 1.0),
    ("short", 100.5, 0.0),
])
def test_buffered_line_break_both_sides(reg, side, mark, expected):
    params = {"buffer_atr_mult": 0.5, "atr_source": "live"}
    pos = _long_pos() if side == "long" else _short_pos()
    out = reg.evaluate("avwap_stop", pos,
                       {"avwap": 100.0, "atr": 2.0, "mark_price": mark}, params)
    assert out["close_fraction"] == expected
    if expected == 1.0:
        assert out["reason"].startswith("avwap_stop:")


def test_zero_buffer_exits_at_line_touch_without_atr(reg):
    params = {"buffer_atr_mult": 0.0}
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 100.0, "avwap": 100.0}, params)
    assert out["close_fraction"] == 1.0
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 100.01, "avwap": 100.0}, params)
    assert out["close_fraction"] == 0.0


def test_atr_source_entry_vs_live(reg):
    pos = _long_pos(entry_atr=1.0)
    mkt = {"mark_price": 99.0, "avwap": 100.0, "atr": 4.0}
    assert reg.evaluate("avwap_stop", pos, mkt,
                        {"buffer_atr_mult": 0.5, "atr_source": "entry"})["close_fraction"] == 1.0
    assert reg.evaluate("avwap_stop", pos, mkt,
                        {"buffer_atr_mult": 0.5, "atr_source": "live"})["close_fraction"] == 0.0


@pytest.mark.parametrize("pos,mkt,params,reason", [
    (_long_pos(entry_atr=0.0), {"mark_price": 50.0, "avwap": 100.0},
     {"buffer_atr_mult": 0.5, "atr_source": "live"}, "noop:missing_live_atr"),
    (_long_pos(entry_atr=0.0), {"mark_price": 50.0, "avwap": 100.0},
     {"buffer_atr_mult": 0.5, "atr_source": "entry"}, "noop:missing_entry_atr"),
    (_long_pos(), {"mark_price": 50.0, "atr": 2.0}, None, "noop:missing_avwap"),
    ({}, {"mark_price": 50.0, "avwap": 100.0, "atr": 2.0}, None, "noop:missing_position"),
    (_long_pos(), {"avwap": 100.0, "atr": 2.0}, None, "noop:missing_mark_price"),
])
def test_missing_inputs_fail_safe(reg, pos, mkt, params, reason):
    out = reg.evaluate("avwap_stop", pos, mkt, params)
    assert out["close_fraction"] == 0.0
    assert out["reason"] == reason


def test_registry_defaults_use_live_atr_quarter_buffer(reg):
    mkt = {"avwap": 100.0, "atr": 2.0}
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.5}, None)
    assert out["close_fraction"] == 1.0
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.6}, None)
    assert out["close_fraction"] == 0.0
