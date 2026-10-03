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
        spec = importlib.util.spec_from_file_location("hl_adapter_cache_test", path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    mod._test_stub_client_error = _StubClientError
    return mod


@pytest.fixture
def adapter_mod():
    return _load_adapter_module()


@pytest.fixture
def cache_path(tmp_path):
    return str(tmp_path / "hl_meta.json")


def _sample_meta():
    return (
        {"universe": [{"index": 0, "name": "USDC/USDC", "tokens": [0, 0]}],
         "tokens": [{"name": "USDC", "szDecimals": 0}]},
        {"universe": [{"name": "BTC", "szDecimals": 5}, {"name": "ETH", "szDecimals": 4}]},
    )


@pytest.mark.parametrize("case", ["fresh", "expired", "coin_missing", "file_missing", "malformed_value"])
def test_sz_decimals_from_meta_cache_reads_lot_size_offline(adapter_mod, cache_path, case):
    spot_meta, meta = _sample_meta()
    if case == "malformed_value":
        meta = {"universe": [{"name": "ETH", "szDecimals": "four"}]}
    if case != "file_missing":
        adapter_mod._save_meta_cache(spot_meta, meta, path=cache_path)
    if case == "expired":
        with open(cache_path) as f:
            payload = json.load(f)
        payload["ts"] = time.time() - 10 * adapter_mod.META_CACHE_TTL_S
        with open(cache_path, "w") as f:
            json.dump(payload, f)
    symbol = "DOGE" if case == "coin_missing" else "ETH"

    got = adapter_mod.sz_decimals_from_meta_cache(symbol, path=cache_path)

    if case in ("fresh", "expired"):
        assert got == 4
    else:
        assert got is None
