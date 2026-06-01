"""Tests for /tmp/hl_meta.json caching + 429 short-circuit in lookup_fill_fee_by_oid (#768)."""

import importlib.util
import json
import os
import sys
import time
from unittest.mock import MagicMock

import pytest


def _load_adapter_module():
    """Load adapter.py with the same SDK mocks the existing suite uses.

    We deliberately don't use _load_hl_adapter from test_adapter.py — we need
    fresh module state per test to exercise module-level _load_meta_cache /
    _save_meta_cache / _fetch_raw_meta in isolation.
    """
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
        spec = importlib.util.spec_from_file_location("hl_adapter_cache_test", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    # Hand back the StubClientError too so 429 tests can raise it.
    mod._test_stub_client_error = _StubClientError
    return mod


@pytest.fixture
def adapter_mod():
    return _load_adapter_module()


@pytest.fixture
def cache_path(tmp_path):
    return str(tmp_path / "hl_meta.json")


# ─── Cache load/save round-trip ────────────────────────────────────

def _sample_meta():
    return (
        {"universe": [{"index": 0, "name": "USDC/USDC", "tokens": [0, 0]}],
         "tokens": [{"name": "USDC", "szDecimals": 0}]},
        {"universe": [{"name": "BTC", "szDecimals": 5}, {"name": "ETH", "szDecimals": 4}]},
    )


def test_save_then_load_returns_payload(adapter_mod, cache_path):
    spot_meta, meta = _sample_meta()
    adapter_mod._save_meta_cache(spot_meta, meta, path=cache_path)
    got = adapter_mod._load_meta_cache(path=cache_path)
    assert got is not None
    got_spot, got_meta = got
    assert got_spot == spot_meta
    assert got_meta == meta


def test_load_returns_none_when_file_missing(adapter_mod, cache_path):
    assert adapter_mod._load_meta_cache(path=cache_path) is None


def test_load_returns_none_when_ttl_expired(adapter_mod, cache_path):
    spot_meta, meta = _sample_meta()
    # Manually stamp an old timestamp so we don't have to wait.
    payload = {"ts": time.time() - 7200, "spot_meta": spot_meta, "meta": meta}
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    # TTL is 3600s; 7200s-old cache must be a miss.
    assert adapter_mod._load_meta_cache(path=cache_path) is None


def test_load_within_ttl_returns_payload(adapter_mod, cache_path):
    spot_meta, meta = _sample_meta()
    payload = {"ts": time.time() - 60, "spot_meta": spot_meta, "meta": meta}
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    got = adapter_mod._load_meta_cache(path=cache_path)
    assert got is not None


def test_load_rejects_empty_universe(adapter_mod, cache_path):
    # An empty universe would silently bypass the symbol-miss guardrail —
    # treat as cache miss so we re-fetch.
    payload = {
        "ts": time.time(),
        "spot_meta": {"universe": [], "tokens": []},
        "meta": {"universe": []},
    }
    with open(cache_path, "w") as f:
        json.dump(payload, f)
    assert adapter_mod._load_meta_cache(path=cache_path) is None


def test_load_rejects_garbage(adapter_mod, cache_path):
    with open(cache_path, "w") as f:
        f.write("not json")
    assert adapter_mod._load_meta_cache(path=cache_path) is None


def test_save_atomic_replace_does_not_leak_tmp(adapter_mod, cache_path, tmp_path):
    spot_meta, meta = _sample_meta()
    adapter_mod._save_meta_cache(spot_meta, meta, path=cache_path)
    # No `.hl_meta_*` leftover from the mkstemp side.
    leftovers = [p.name for p in tmp_path.iterdir() if p.name.startswith(".hl_meta_")]
    assert leftovers == []
    assert os.path.exists(cache_path)


def test_save_unserializable_payload_swallows_error(adapter_mod, cache_path):
    """A bogus payload (e.g. a MagicMock from a misconfigured SDK mock) must
    not raise — caching is best-effort.
    """
    # MagicMock is not JSON-serializable; the helper must log and return.
    adapter_mod._save_meta_cache(MagicMock(), MagicMock(), path=cache_path)
    # Either no file or no leftover .hl_meta_* — both are acceptable.
    if os.path.exists(cache_path):
        pytest.fail("cache file should not exist after a failed save")


# ─── 429 short-circuit ─────────────────────────────────────────────

def _make_live_adapter(adapter_mod, monkeypatch):
    """Build an adapter instance with a fake address and a controllable Info."""
    monkeypatch.setenv("HYPERLIQUID_ACCOUNT_ADDRESS", "0xdeadbeef")
    # Pre-seed the cache so __init__ doesn't try to network-fetch.
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()
    # Replace Info with a stub we can poke.
    a._info = MagicMock()
    return a


def test_lookup_fill_fee_returns_empty_on_429_and_no_retry(adapter_mod, monkeypatch):
    a = _make_live_adapter(adapter_mod, monkeypatch)
    err = adapter_mod._test_stub_client_error(status_code=429)
    a._info.user_fills_by_time = MagicMock(side_effect=err)

    sleeps = []
    monkeypatch.setattr(adapter_mod.time, "sleep", lambda s: sleeps.append(s))

    result = a.lookup_fill_fee_by_oid(oid=12345, since_ms=0)
    assert result == {}
    # Single attempt — no retry budget burned.
    assert a._info.user_fills_by_time.call_count == 1
    assert sleeps == []


def test_lookup_fill_fee_still_retries_non_429_errors(adapter_mod, monkeypatch):
    """Other ClientErrors (and generic Exceptions) must keep the original
    retry behavior — only 429 short-circuits.
    """
    a = _make_live_adapter(adapter_mod, monkeypatch)
    # Mix: 500-like error first, then a successful empty fills list.
    err = adapter_mod._test_stub_client_error(status_code=500)
    call_count = {"n": 0}

    def side(addr, since_ms):
        call_count["n"] += 1
        if call_count["n"] == 1:
            raise err
        return []
    a._info.user_fills_by_time = side

    sleeps = []
    monkeypatch.setattr(adapter_mod.time, "sleep", lambda s: sleeps.append(s))

    result = a.lookup_fill_fee_by_oid(oid=12345, since_ms=0, max_retries=4)
    assert result == {}
    assert call_count["n"] >= 2
    assert len(sleeps) >= 1


def test_lookup_fill_fee_returns_real_fill_on_match(adapter_mod, monkeypatch):
    """Sanity: matched OIDs still sum fees and closed_pnl correctly so we
    don't regress the happy path while adding the 429 branch.
    """
    a = _make_live_adapter(adapter_mod, monkeypatch)
    a._info.user_fills_by_time = MagicMock(return_value=[
        {"oid": 42, "fee": "0.10", "closedPnl": "1.50"},
        {"oid": 42, "fee": "0.05", "closedPnl": "0.75"},
        {"oid": 999, "fee": "0.99", "closedPnl": "9.99"},  # different OID
    ])
    monkeypatch.setattr(adapter_mod.time, "sleep", lambda s: None)
    result = a.lookup_fill_fee_by_oid(oid=42, since_ms=0)
    assert result["count"] == 2
    assert result["fee"] == pytest.approx(0.15)
    assert result["closed_pnl"] == pytest.approx(2.25)


# ─── _sz_decimals symbol-miss force-refresh ────────────────────────

def test_sz_decimals_refreshes_on_missing_symbol(adapter_mod, monkeypatch):
    """When a symbol is not in the cached universe (stale cache after HL adds
    a new coin), _sz_decimals must force a meta refresh once before falling
    back to 3.
    """
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()

    # Initial cached map: only BTC known.
    a._info = MagicMock()
    a._info.asset_to_sz_decimals = {"BTC": 5}

    # Refresh path: _build_info returns a new Info whose map includes NEWCOIN.
    refreshed = MagicMock()
    refreshed.asset_to_sz_decimals = {"BTC": 5, "NEWCOIN": 2}
    monkeypatch.setattr(a, "_build_info", lambda base_url, allow_cache: refreshed)

    assert a._sz_decimals("NEWCOIN") == 2
    # And the refreshed Info is retained on the adapter.
    assert a._info is refreshed


def test_sz_decimals_caches_misses_to_avoid_repeat_refresh(adapter_mod, monkeypatch):
    """A typo'd or genuinely-unlisted symbol must only trigger one meta
    refresh per subprocess. Without the miss cache, every subsequent
    order/round/floor call would fire 2 fresh /info calls — exactly the
    burst behavior #768 set out to eliminate. (PR #769 review point 2.)
    """
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()
    a._info = MagicMock()
    a._info.asset_to_sz_decimals = {"BTC": 5}

    refresh_calls = {"n": 0}

    def fake_build(base_url, allow_cache):
        refresh_calls["n"] += 1
        refreshed = MagicMock()
        # Refresh doesn't bring UNLISTED in — typo or delisted.
        refreshed.asset_to_sz_decimals = {"BTC": 5}
        return refreshed

    monkeypatch.setattr(a, "_build_info", fake_build)

    # First call: refresh fires, miss is recorded.
    assert a._sz_decimals("UNLISTED") == 3
    assert refresh_calls["n"] == 1
    # Subsequent calls: short-circuit on the recorded miss, no more refreshes.
    for _ in range(5):
        assert a._sz_decimals("UNLISTED") == 3
    assert refresh_calls["n"] == 1
    # And a different missing symbol still gets its one refresh.
    assert a._sz_decimals("ALSOUNLISTED") == 3
    assert refresh_calls["n"] == 2


def test_sz_decimals_returns_3_when_still_missing_after_refresh(adapter_mod, monkeypatch):
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()
    a._info = MagicMock()
    a._info.asset_to_sz_decimals = {"BTC": 5}

    refreshed = MagicMock()
    refreshed.asset_to_sz_decimals = {"BTC": 5}  # still missing UNLISTED
    monkeypatch.setattr(a, "_build_info", lambda base_url, allow_cache: refreshed)

    assert a._sz_decimals("UNLISTED") == 3


# ─── _normalize_spot_meta: sparse token index guard (#831) ─────────


def _sdk_universe_loop(spot_meta):
    """Re-implementation of the SDK's Info.__init__ spot loop (info.py:43-49).

    Used to prove that, after normalization, the exact positional lookup the
    SDK performs no longer raises IndexError. Returns {asset: szDecimals}.
    """
    asset_to_sz_decimals = {}
    for spot_info in spot_meta["universe"]:
        asset = spot_info["index"] + 10000
        base, quote = spot_info["tokens"]
        base_info = spot_meta["tokens"][base]  # positional — the crashy line
        spot_meta["tokens"][quote]
        asset_to_sz_decimals[asset] = base_info["szDecimals"]
    return asset_to_sz_decimals


def _sparse_spot_meta():
    """Mirror the real #831 shape: token index 479 ('WARS') lives at list
    position 459 (sparse), and spot pair '@367' references it as base.
    """
    tokens = [{"name": f"T{i}", "szDecimals": 0, "index": i} for i in range(458)]
    # Sparse tail: a gap (indices 458-477 missing), then high-index tokens
    # packed at positions 458+.
    tokens += [
        {"name": "WARS", "szDecimals": 2, "index": 479},
        {"name": "ZZZ", "szDecimals": 1, "index": 513},
    ]
    universe = [
        {"index": 0, "name": "PURR/USDC", "tokens": [1, 0]},
        {"index": 367, "name": "@367", "tokens": [479, 0]},  # base at sparse idx
    ]
    return {"universe": universe, "tokens": tokens}


def test_normalize_makes_sdk_positional_lookup_not_crash(adapter_mod):
    spot_meta = _sparse_spot_meta()
    # Sanity: the raw meta crashes the SDK's positional loop.
    with pytest.raises(IndexError):
        _sdk_universe_loop(spot_meta)

    normalized = adapter_mod._normalize_spot_meta(spot_meta)
    # After normalization the same loop resolves cleanly.
    result = _sdk_universe_loop(normalized)
    # @367's base token (index 479 = WARS, szDecimals 2) resolves correctly.
    assert result[367 + 10000] == 2
    # PURR/USDC base (index 1) still resolves.
    assert (0 + 10000) in result


def test_normalize_resolves_token_by_index_not_position(adapter_mod):
    spot_meta = _sparse_spot_meta()
    normalized = adapter_mod._normalize_spot_meta(spot_meta)
    # The dense list is index-aligned: position 479 IS the WARS token.
    assert normalized["tokens"][479]["name"] == "WARS"
    assert normalized["tokens"][513]["name"] == "ZZZ"
    # Original input is left untouched (shallow-copy semantics).
    assert len(spot_meta["tokens"]) == 460


def test_normalize_drops_unresolvable_pairs(adapter_mod):
    spot_meta = {
        "universe": [
            {"index": 0, "name": "GOOD/USDC", "tokens": [1, 0]},
            {"index": 1, "name": "BAD/USDC", "tokens": [999, 0]},  # 999 absent
            {"index": 2, "name": "MALFORMED"},                     # no tokens
        ],
        "tokens": [
            {"name": "USDC", "szDecimals": 8, "index": 0},
            {"name": "GOOD", "szDecimals": 2, "index": 1},
        ],
    }
    normalized = adapter_mod._normalize_spot_meta(spot_meta)
    names = [u["name"] for u in normalized["universe"]]
    assert names == ["GOOD/USDC"]
    # Surviving pair still resolves under the SDK loop.
    assert _sdk_universe_loop(normalized)[0 + 10000] == 2


def test_normalize_passes_through_aligned_meta_unchanged(adapter_mod):
    spot_meta = {
        "universe": [{"index": 0, "name": "P/USDC", "tokens": [1, 0]}],
        "tokens": [
            {"name": "USDC", "szDecimals": 8, "index": 0},
            {"name": "P", "szDecimals": 2, "index": 1},
        ],
    }
    normalized = adapter_mod._normalize_spot_meta(spot_meta)
    # Already dense + index-aligned with no drops → identity (same object).
    assert normalized is spot_meta


def test_normalize_passes_through_malformed_input(adapter_mod):
    assert adapter_mod._normalize_spot_meta(None) is None
    bad = {"universe": "nope", "tokens": []}
    assert adapter_mod._normalize_spot_meta(bad) is bad


def test_build_info_normalizes_before_sdk(adapter_mod, monkeypatch):
    """_build_info must hand the SDK a normalized spot_meta so the crashy
    positional loop never sees a sparse token list (cache-hit path)."""
    spot_meta = _sparse_spot_meta()
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (spot_meta, {"universe": [{"name": "BTC", "szDecimals": 5}]}))

    captured = {}

    def fake_info(base_url, skip_ws, meta=None, spot_meta=None):
        captured["spot_meta"] = spot_meta
        # Prove the SDK's own loop would survive on what it was handed.
        _sdk_universe_loop(spot_meta)
        return MagicMock()

    monkeypatch.setattr(adapter_mod, "_HLInfo", fake_info)
    adapter_mod.HyperliquidExchangeAdapter()
    assert captured["spot_meta"]["tokens"][479]["name"] == "WARS"


def test_sz_decimals_uses_cached_value_without_refresh(adapter_mod, monkeypatch):
    monkeypatch.setattr(adapter_mod, "_load_meta_cache",
                        lambda *a, **kw: (
                            {"universe": [], "tokens": []},
                            {"universe": [{"name": "BTC", "szDecimals": 5}]},
                        ))
    a = adapter_mod.HyperliquidExchangeAdapter()
    a._info = MagicMock()
    a._info.asset_to_sz_decimals = {"BTC": 5, "ETH": 4}

    rebuilt = {"called": False}

    def fake_build(base_url, allow_cache):
        rebuilt["called"] = True
        return MagicMock()

    monkeypatch.setattr(a, "_build_info", fake_build)

    assert a._sz_decimals("BTC") == 5
    assert a._sz_decimals("ETH") == 4
    assert rebuilt["called"] is False
