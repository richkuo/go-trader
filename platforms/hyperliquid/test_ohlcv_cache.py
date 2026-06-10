"""Tests for the per-cycle OHLCV file cache in get_ohlcv (#839).

Mirrors test_meta_cache.py's module-loading approach so the cache helpers and
get_ohlcv integration can be exercised with fresh module state and mocked SDK.
"""

import importlib.util
import json
import os
import sys
import time
from unittest.mock import MagicMock

import pytest


def _load_adapter_module():
    info_mod = MagicMock()
    exchange_mod = MagicMock()
    api_mod = MagicMock()
    utils_pkg = MagicMock()
    error_mod = MagicMock()
    hl_pkg = MagicMock()

    info_mod.Info = MagicMock()
    exchange_mod.Exchange = MagicMock()
    api_mod.API = MagicMock()

    class _StubClientError(Exception):
        def __init__(self, status_code=None, *a, **kw):
            super().__init__(*a, **kw)
            self.status_code = status_code

    error_mod.ClientError = _StubClientError

    mod_names = (
        "hyperliquid",
        "hyperliquid.info",
        "hyperliquid.exchange",
        "hyperliquid.api",
        "hyperliquid.utils",
        "hyperliquid.utils.error",
    )
    saved = {name: sys.modules.get(name) for name in mod_names}
    sys.modules["hyperliquid"] = hl_pkg
    sys.modules["hyperliquid.info"] = info_mod
    sys.modules["hyperliquid.exchange"] = exchange_mod
    sys.modules["hyperliquid.api"] = api_mod
    sys.modules["hyperliquid.utils"] = utils_pkg
    sys.modules["hyperliquid.utils.error"] = error_mod

    try:
        path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
        spec = importlib.util.spec_from_file_location("hl_adapter_ohlcv_test", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    return mod


@pytest.fixture
def adapter_mod():
    return _load_adapter_module()


@pytest.fixture
def cache_path(tmp_path):
    return str(tmp_path / "hl_ohlcv_BTC_1h_200.json")


def _sample_candles():
    return [
        [1700000000000, 100.0, 110.0, 90.0, 105.0, 50.0],
        [1700003600000, 105.0, 115.0, 95.0, 110.0, 60.0],
    ]


# ─── enable/disable toggle ─────────────────────────────────────────

def test_cache_enabled_by_default(adapter_mod, monkeypatch):
    monkeypatch.delenv("GO_TRADER_HL_OHLCV_CACHE", raising=False)
    assert adapter_mod._ohlcv_cache_enabled() is True


def test_cache_disabled_via_env(adapter_mod, monkeypatch):
    monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
    assert adapter_mod._ohlcv_cache_enabled() is False


# ─── path construction ─────────────────────────────────────────────

def test_cache_path_sanitizes_symbol(adapter_mod, tmp_path):
    path = adapter_mod._ohlcv_cache_path("PURR/USDC", "1h", 200, cache_dir=str(tmp_path))
    # No slash from the symbol leaks into the filename → stays in cache_dir.
    assert os.path.dirname(path) == str(tmp_path)
    assert os.path.basename(path) == "hl_ohlcv_PURR_USDC_1h_200.json"


def test_cache_path_distinct_per_limit_and_interval(adapter_mod, tmp_path):
    d = str(tmp_path)
    p1 = adapter_mod._ohlcv_cache_path("BTC", "1h", 200, cache_dir=d)
    p2 = adapter_mod._ohlcv_cache_path("BTC", "1h", 50, cache_dir=d)
    p3 = adapter_mod._ohlcv_cache_path("BTC", "4h", 200, cache_dir=d)
    assert len({p1, p2, p3}) == 3


def test_cache_path_sanitizes_interval(adapter_mod, tmp_path):
    # A stray slash / dotdot in interval must not escape the cache dir.
    path = adapter_mod._ohlcv_cache_path("BTC", "../1h", 200, cache_dir=str(tmp_path))
    assert os.path.dirname(path) == str(tmp_path)
    assert ".." not in os.path.basename(path)
    assert "/" not in os.path.basename(path)


# ─── interval-aware TTL ────────────────────────────────────────────

def test_ttl_caps_fast_intervals_to_half_bar(adapter_mod):
    # 1m bar = 60_000ms → half-bar 30s, below the 60s cap.
    assert adapter_mod._ohlcv_cache_ttl(60_000) == 30


def test_ttl_caps_slow_intervals_at_default(adapter_mod):
    # 1h bar → half-bar would be 1800s, capped at OHLCV_CACHE_TTL_S (60).
    assert adapter_mod._ohlcv_cache_ttl(3_600_000) == adapter_mod.OHLCV_CACHE_TTL_S


def test_ttl_never_zero(adapter_mod):
    assert adapter_mod._ohlcv_cache_ttl(1) >= 1


# ─── load/save round-trip ──────────────────────────────────────────

def test_save_then_load_returns_candles(adapter_mod, cache_path):
    candles = _sample_candles()
    adapter_mod._save_ohlcv_cache(candles, cache_path)
    got = adapter_mod._load_ohlcv_cache(cache_path)
    assert got == candles


def test_load_returns_none_when_missing(adapter_mod, cache_path):
    assert adapter_mod._load_ohlcv_cache(cache_path) is None


def test_load_returns_none_when_ttl_expired(adapter_mod, cache_path):
    payload = {"ts": time.time() - 3600, "candles": _sample_candles()}
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    # Default TTL is 60s; an hour-old entry is a miss.
    assert adapter_mod._load_ohlcv_cache(cache_path) is None


def test_load_within_ttl_returns_candles(adapter_mod, cache_path):
    payload = {"ts": time.time() - 10, "candles": _sample_candles()}
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    assert adapter_mod._load_ohlcv_cache(cache_path) == _sample_candles()


def test_load_rejects_empty_candle_list(adapter_mod, cache_path):
    # An empty list would pin every peer to the insufficient-data error path
    # for the TTL window — treat as a miss so they re-fetch live.
    payload = {"ts": time.time(), "candles": []}
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    assert adapter_mod._load_ohlcv_cache(cache_path) is None


def test_load_rejects_garbage(adapter_mod, cache_path):
    with open(cache_path, "w") as f:
        f.write("not json")
    assert adapter_mod._load_ohlcv_cache(cache_path) is None


def test_save_atomic_replace_leaves_no_tmp(adapter_mod, cache_path, tmp_path):
    adapter_mod._save_ohlcv_cache(_sample_candles(), cache_path)
    leftovers = [p.name for p in tmp_path.iterdir() if p.name.startswith(".hl_ohlcv_")]
    assert leftovers == []
    assert os.path.exists(cache_path)


def test_save_unserializable_swallows_error(adapter_mod, cache_path):
    adapter_mod._save_ohlcv_cache(MagicMock(), cache_path)
    if os.path.exists(cache_path):
        pytest.fail("cache file should not exist after a failed save")


# ─── get_ohlcv integration ─────────────────────────────────────────

def _make_adapter(adapter_mod, monkeypatch, candles_return):
    """Build an adapter with a stub Info returning the given raw candles."""
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()
    a._info = MagicMock()
    a._info.candles_snapshot = MagicMock(return_value=candles_return)
    return a


def _raw(ts):
    return {"T": ts, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"}


def test_second_call_hits_cache_and_skips_snapshot(adapter_mod, monkeypatch, tmp_path):
    monkeypatch.delenv("GO_TRADER_HL_OHLCV_CACHE", raising=False)  # default = enabled
    monkeypatch.setattr(adapter_mod, "OHLCV_CACHE_DIR", str(tmp_path))
    a = _make_adapter(adapter_mod, monkeypatch, [_raw(1700000000000)])

    first = a.get_ohlcv("BTC", "1h", 200)
    assert a._info.candles_snapshot.call_count == 1
    assert first == [[1700000000000, 100.0, 110.0, 90.0, 105.0, 50.0]]

    # A peer strategy (same symbol/interval/limit) reads from cache.
    second = a.get_ohlcv("BTC", "1h", 200)
    assert a._info.candles_snapshot.call_count == 1  # no extra /info call
    assert second == first


def test_disabled_cache_always_fetches(adapter_mod, monkeypatch, tmp_path):
    monkeypatch.setattr(adapter_mod, "OHLCV_CACHE_DIR", str(tmp_path))
    monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
    a = _make_adapter(adapter_mod, monkeypatch, [_raw(1700000000000)])

    a.get_ohlcv("BTC", "1h", 200)
    a.get_ohlcv("BTC", "1h", 200)
    assert a._info.candles_snapshot.call_count == 2  # no caching
    # And nothing was written to the cache dir.
    assert [p for p in tmp_path.iterdir()] == []


def test_distinct_limits_do_not_share_cache(adapter_mod, monkeypatch, tmp_path):
    monkeypatch.setattr(adapter_mod, "OHLCV_CACHE_DIR", str(tmp_path))
    a = _make_adapter(adapter_mod, monkeypatch, [_raw(1700000000000)])

    a.get_ohlcv("BTC", "1h", 200)
    a.get_ohlcv("BTC", "1h", 50)  # different limit → separate fetch
    assert a._info.candles_snapshot.call_count == 2


def test_empty_result_is_not_cached(adapter_mod, monkeypatch, tmp_path):
    monkeypatch.setattr(adapter_mod, "OHLCV_CACHE_DIR", str(tmp_path))
    a = _make_adapter(adapter_mod, monkeypatch, [])  # insufficient data

    assert a.get_ohlcv("BTC", "1h", 200) == []
    assert a.get_ohlcv("BTC", "1h", 200) == []
    # Both calls hit the network — an empty result must never be cached.
    assert a._info.candles_snapshot.call_count == 2
    assert [p for p in tmp_path.iterdir()] == []


def test_cache_stores_trimmed_rows_not_widened_fetch(adapter_mod, monkeypatch, tmp_path):
    """Peers must read exactly `limit` rows from cache (#937), not limit+margin."""
    monkeypatch.setattr(adapter_mod, "OHLCV_CACHE_DIR", str(tmp_path))
    extra = [
        _raw(1700000000000 + i * 3_600_000) for i in range(adapter_mod.OHLCV_GAP_MARGIN + 5)
    ]
    a = _make_adapter(adapter_mod, monkeypatch, extra)

    got = a.get_ohlcv("BTC", "1h", 5)
    assert len(got) == 5

    cache_files = list(tmp_path.iterdir())
    assert len(cache_files) == 1
    with open(cache_files[0], "r") as f:
        cached = json.load(f)["candles"]
    assert len(cached) == 5

    a._info.candles_snapshot.reset_mock()
    peer = a.get_ohlcv("BTC", "1h", 5)
    assert a._info.candles_snapshot.call_count == 0
    assert len(peer) == 5
    assert peer == got
