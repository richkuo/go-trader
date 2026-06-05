"""#879: check_hyperliquid.py injected-regime wiring parity."""
import importlib.util
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parents[1]


def _load(name, rel):
    sys.path.insert(0, str(ROOT / "shared_tools"))
    sys.path.insert(0, str(ROOT / "shared_scripts"))
    spec = importlib.util.spec_from_file_location(name, ROOT / rel)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_primary_window_key_matches_prepare():
    chl = _load("check_hl", "shared_scripts/check_hyperliquid.py")
    assert chl._primary_window_key(None) == ""
    assert chl._primary_window_key({"medium": {}, "macro": {}}) == "medium"
    assert chl._primary_window_key({"alpha": {}, "beta": {}}) == "alpha"


def test_injected_regime_block_matches_inline():
    chl = _load("check_hl2", "shared_scripts/check_hyperliquid.py")
    regime = _load("regime_hl", "shared_tools/regime.py")
    n = 120
    close = pd.Series(range(100, 100 + n)).astype(float)
    df = pd.DataFrame({"timestamp": range(n), "open": close, "high": close + 1,
                       "low": close - 1, "close": close, "volume": [1.0] * n})
    spec = {"medium": {"classifier": "adx", "period": 14, "adx_threshold": 20},
            "macro": {"classifier": "composite", "period": 30}}
    # inline path
    s_payload, s_live, s_strat = regime.prepare_check_regime(
        df, regime_enabled=True, windows_spec=spec, atr_window="macro")
    # injected path as wired in the check
    i_payload, i_live, i_strat = regime.resolve_injected_regime(
        s_payload, primary_key=chl._primary_window_key(spec), atr_window="macro")
    assert i_payload == s_payload
    assert i_live == s_live
    assert i_strat == s_strat
