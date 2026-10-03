import importlib.util
import json
import pathlib

import numpy as np
import pandas as pd

from shared_tools.conftest import make_ohlcv

_spec = importlib.util.spec_from_file_location(
    "regime", pathlib.Path(__file__).parent / "regime.py"
)
_regime_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_regime_mod)

prepare_check_regime = _regime_mod.prepare_check_regime

_CHECK_REGIME_PATH = (
    pathlib.Path(__file__).parent.parent / "shared_scripts" / "check_regime.py"
)
_cr_spec = importlib.util.spec_from_file_location("check_regime", _CHECK_REGIME_PATH)
_check_regime_mod = importlib.util.module_from_spec(_cr_spec)
_cr_spec.loader.exec_module(_check_regime_mod)
compute_regime_bundle = _check_regime_mod.compute_regime_bundle


def _make_uptrend(n: int = 120, noise: float = 0.5) -> pd.DataFrame:
    return make_ohlcv(
        np.linspace(100.0, 200.0, n),
        volume=np.full(n, 1000.0),
        noise=noise,
    )


_SPEC_MULTI = {
    "short": {"classifier": "adx", "period": 10, "adx_threshold": 20.0},
    "medium": {"classifier": "adx", "period": 14, "adx_threshold": 20.0},
    "macro": {
        "classifier": "composite",
        "period": 30,
        "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 25.0, "efficiency": 0.5},
    },
}


def test_bundle_roundtrip_through_injection():
    df = _make_uptrend()
    bundle = compute_regime_bundle(df, _SPEC_MULTI)
    raw = json.dumps(bundle["regime"])
    injected = prepare_check_regime(
        df, regime_enabled=True, windows_spec=_SPEC_MULTI,
        atr_window="short", injected_payload_json=raw,
    )
    inline = prepare_check_regime(
        df, regime_enabled=True, windows_spec=_SPEC_MULTI, atr_window="short"
    )
    assert injected == inline
