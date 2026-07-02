"""Tests for the #1196 avwap_stop loss-of-line close evaluator."""

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


# --------------------------------------------------------------------------
# Long / short hit and boundary (buffer = buffer_atr_mult * live ATR)
# --------------------------------------------------------------------------

def test_long_hit_and_boundary(reg):
    params = {"buffer_atr_mult": 0.5, "atr_source": "live"}
    mkt = {"avwap": 100.0, "atr": 2.0}
    # buffer = 1.0 → hit at mark <= 99.0
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.0}, params)
    assert out["close_fraction"] == 1.0
    assert out["reason"].startswith("avwap_stop:")
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.5}, params)
    assert out["close_fraction"] == 0.0


def test_short_mirrors(reg):
    params = {"buffer_atr_mult": 0.5, "atr_source": "live"}
    mkt = {"avwap": 100.0, "atr": 2.0}
    out = reg.evaluate("avwap_stop", _short_pos(), {**mkt, "mark_price": 101.0}, params)
    assert out["close_fraction"] == 1.0
    out = reg.evaluate("avwap_stop", _short_pos(), {**mkt, "mark_price": 100.5}, params)
    assert out["close_fraction"] == 0.0


def test_zero_buffer_exits_at_line_touch_without_atr(reg):
    # buffer_atr_mult == 0 needs no ATR at all: exit exactly at the line.
    params = {"buffer_atr_mult": 0.0}
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 100.0, "avwap": 100.0}, params)
    assert out["close_fraction"] == 1.0
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 100.01, "avwap": 100.0}, params)
    assert out["close_fraction"] == 0.0


# --------------------------------------------------------------------------
# atr_source: live (market["atr"]) vs entry (position["entry_atr"])
# --------------------------------------------------------------------------

def test_atr_source_entry_vs_live(reg):
    # live ATR 4.0 (buffer 2.0 → hit at <= 98), entry ATR 1.0 (buffer 0.5 → hit at <= 99.5)
    pos = _long_pos(entry_atr=1.0)
    mkt = {"mark_price": 99.0, "avwap": 100.0, "atr": 4.0}
    assert reg.evaluate("avwap_stop", pos, mkt,
                        {"buffer_atr_mult": 0.5, "atr_source": "entry"})["close_fraction"] == 1.0
    assert reg.evaluate("avwap_stop", pos, mkt,
                        {"buffer_atr_mult": 0.5, "atr_source": "live"})["close_fraction"] == 0.0


def test_missing_atr_fails_safe_when_buffer_positive(reg):
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 50.0, "avwap": 100.0},
                       {"buffer_atr_mult": 0.5, "atr_source": "live"})
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_live_atr"
    out = reg.evaluate("avwap_stop", _long_pos(entry_atr=0.0),
                       {"mark_price": 50.0, "avwap": 100.0},
                       {"buffer_atr_mult": 0.5, "atr_source": "entry"})
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_entry_atr"


# --------------------------------------------------------------------------
# Fail-safe on missing context
# --------------------------------------------------------------------------

def test_missing_avwap_fails_safe(reg):
    out = reg.evaluate("avwap_stop", _long_pos(), {"mark_price": 50.0, "atr": 2.0}, None)
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_avwap"


def test_missing_position_or_mark_fails_safe(reg):
    out = reg.evaluate("avwap_stop", {}, {"mark_price": 50.0, "avwap": 100.0, "atr": 2.0}, None)
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_position"
    out = reg.evaluate("avwap_stop", _long_pos(), {"avwap": 100.0, "atr": 2.0}, None)
    assert out["close_fraction"] == 0.0
    assert out["reason"] == "noop:missing_mark_price"


# --------------------------------------------------------------------------
# Registry defaults: buffer 0.25 x live ATR
# --------------------------------------------------------------------------

def test_registry_defaults_use_live_atr_quarter_buffer(reg):
    mkt = {"avwap": 100.0, "atr": 2.0}  # buffer = 0.5
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.5}, None)
    assert out["close_fraction"] == 1.0
    out = reg.evaluate("avwap_stop", _long_pos(), {**mkt, "mark_price": 99.6}, None)
    assert out["close_fraction"] == 0.0
