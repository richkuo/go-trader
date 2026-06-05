"""#879: fetch_regime.py parity with prepare_check_regime + output shape."""
import importlib.util
import json
import subprocess
import sys
from pathlib import Path

import pandas as pd

ROOT = Path(__file__).resolve().parents[1]


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.path.insert(0, str(ROOT / "shared_tools"))
    sys.path.insert(0, str(ROOT))
    spec.loader.exec_module(mod)
    return mod


def _uptrend_df(n=120):
    close = pd.Series(range(100, 100 + n)).astype(float)
    return pd.DataFrame({
        "timestamp": range(n), "open": close, "high": close + 1,
        "low": close - 1, "close": close, "volume": [1.0] * n,
    })


def test_compute_payload_matches_prepare_check_regime_adx():
    fr = _load("fetch_regime", ROOT / "shared_scripts" / "fetch_regime.py")
    regime = _load("regime_mod", ROOT / "shared_tools" / "regime.py")
    spec = {"default": {"classifier": "adx", "period": 14, "adx_threshold": 20}}
    df = _uptrend_df()
    payload = fr.compute_payload(df, spec)
    expected, _, _ = regime.prepare_check_regime(df, regime_enabled=True, windows_spec=spec)
    assert payload == expected


def test_compute_payload_matches_prepare_check_regime_multi_window():
    fr = _load("fetch_regime2", ROOT / "shared_scripts" / "fetch_regime.py")
    regime = _load("regime_mod2", ROOT / "shared_tools" / "regime.py")
    spec = {
        "medium": {"classifier": "adx", "period": 14, "adx_threshold": 20},
        "macro": {"classifier": "composite", "period": 30},
    }
    df = _uptrend_df()
    payload = fr.compute_payload(df, spec)
    expected, _, _ = regime.prepare_check_regime(df, regime_enabled=True, windows_spec=spec)
    assert payload == expected


def test_full_period_adx_not_capped():
    """period>14 ADX window must use the full period (not COMPOSITE cap)."""
    fr = _load("fetch_regime3", ROOT / "shared_scripts" / "fetch_regime.py")
    regime = _load("regime_mod3", ROOT / "shared_tools" / "regime.py")
    spec = {"slow": {"classifier": "adx", "period": 30, "adx_threshold": 20}}
    df = _uptrend_df(150)
    payload = fr.compute_payload(df, spec)
    expected, _, _ = regime.prepare_check_regime(df, regime_enabled=True, windows_spec=spec)
    assert payload == expected


def test_probe_only_short_circuits():
    out = subprocess.run(
        [sys.executable, str(ROOT / "shared_scripts" / "fetch_regime.py"), "--probe-only"],
        capture_output=True, text=True,
    )
    assert out.returncode == 0
    parsed = json.loads(out.stdout)
    assert parsed["regime"] == ""


def test_empty_spec_yields_empty_regime():
    out = subprocess.run(
        [sys.executable, str(ROOT / "shared_scripts" / "fetch_regime.py"),
         "--platform=hyperliquid", "--symbol=BTC", "--interval=1h",
         "--regime-windows-spec-json=", "--ohlcv-limit=200"],
        capture_output=True, text=True,
    )
    assert out.returncode == 0
    assert json.loads(out.stdout)["regime"] == ""
