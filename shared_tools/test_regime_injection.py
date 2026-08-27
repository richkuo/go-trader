
import importlib.util
import json
import pathlib
import sys

import numpy as np
import pandas as pd

sys.path.insert(0, str(pathlib.Path(__file__).parent))

_spec = importlib.util.spec_from_file_location(
    "regime", pathlib.Path(__file__).parent / "regime.py"
)
_regime_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_regime_mod)

prepare_check_regime = _regime_mod.prepare_check_regime
regime_from_injected_payload = _regime_mod.regime_from_injected_payload
compute_multi_regime = _regime_mod.compute_multi_regime
latest_regime = _regime_mod.latest_regime
latest_regime_composite = _regime_mod.latest_regime_composite

_CHECK_REGIME_PATH = (
    pathlib.Path(__file__).parent.parent / "shared_scripts" / "check_regime.py"
)
_cr_spec = importlib.util.spec_from_file_location("check_regime", _CHECK_REGIME_PATH)
_check_regime_mod = importlib.util.module_from_spec(_cr_spec)
_cr_spec.loader.exec_module(_check_regime_mod)
compute_regime_bundle = _check_regime_mod.compute_regime_bundle


def _make_uptrend(n: int = 120, noise: float = 0.5) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    return pd.DataFrame({
        "open": close - noise * 0.3,
        "high": close + noise,
        "low": close - noise,
        "close": close,
        "volume": np.ones(n) * 1000.0,
    })


_SPEC_MULTI = {
    "short": {"classifier": "adx", "period": 10, "adx_threshold": 20.0},
    "medium": {"classifier": "adx", "period": 14, "adx_threshold": 20.0},
    "macro": {
        "classifier": "composite",
        "period": 30,
        "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 25.0, "efficiency": 0.5},
    },
}




def test_injected_payload_matches_inline_triple():
    df = _make_uptrend()
    inline = prepare_check_regime(
        df, regime_enabled=True, windows_spec=_SPEC_MULTI, atr_window="macro"
    )
    payload = compute_multi_regime(df, _SPEC_MULTI)
    injected = regime_from_injected_payload(json.dumps(payload), atr_window="macro")
    assert injected == inline


def test_injected_payload_primary_window_selection():
    df = _make_uptrend()
    payload = compute_multi_regime(df, _SPEC_MULTI)
    _, _, strategy_regime = regime_from_injected_payload(json.dumps(payload))
    assert strategy_regime == payload["medium"]

    no_medium = {k: v for k, v in payload.items() if k != "medium"}
    _, _, strategy_regime = regime_from_injected_payload(json.dumps(no_medium))
    assert strategy_regime == no_medium["macro"]


def test_injected_payload_atr_window_label():
    df = _make_uptrend()
    payload = compute_multi_regime(df, _SPEC_MULTI)
    _, live, _ = regime_from_injected_payload(json.dumps(payload), atr_window="short")
    assert live == payload["short"]["regime"]
    _, live, _ = regime_from_injected_payload(json.dumps(payload), atr_window="nope")
    assert live == payload["medium"]["regime"]


def test_injected_payload_empty_fails_open():
    for raw in ("", "   ", "{}", "null", "not-json", "[1,2]"):
        stdout_regime, live, snap = regime_from_injected_payload(raw)
        assert stdout_regime == ""
        assert live == ""
        assert snap["regime"] == ""


def test_prepare_check_regime_skips_compute_when_injected():
    payload = {"default": {"regime": "trending_up", "score": 0.4, "metrics": {}}}
    stdout_regime, live, snap = prepare_check_regime(
        None,
        regime_enabled=True,
        windows_spec={"default": {"classifier": "adx", "period": 14}},
        injected_payload_json=json.dumps(payload),
    )
    assert stdout_regime == payload
    assert live == "trending_up"
    assert snap["regime"] == "trending_up"
    stdout_regime, live, snap = prepare_check_regime(
        None, regime_enabled=True, windows_spec=_SPEC_MULTI, injected_payload_json=""
    )
    assert (stdout_regime, live, snap["regime"]) == ("", "", "")


def test_prepare_check_regime_disabled_ignores_injection():
    out = prepare_check_regime(
        None, regime_enabled=False, injected_payload_json='{"default":{"regime":"x"}}'
    )
    assert out[0] == "" and out[1] == ""




def test_bundle_snapshots_match_compute_multi_regime():
    df = _make_uptrend()
    bundle = compute_regime_bundle(df, _SPEC_MULTI)
    assert bundle["regime"] == compute_multi_regime(df, _SPEC_MULTI)


def test_bundle_adx3_view_is_full_period_classifier():
    df = _make_uptrend()
    spec = {
        "macro": {
            "classifier": "composite",
            "period": 30,
            "thresholds": {"return_eff": 0.05, "range_eff": 0.03, "adx": 25.0, "efficiency": 0.5},
        }
    }
    bundle = compute_regime_bundle(df, spec)
    expected = latest_regime(df, period=30, adx_threshold=25.0)["regime"]
    assert bundle["views"]["macro"]["adx3"] == expected
    assert bundle["views"]["macro"]["composite7"] == bundle["regime"]["macro"]["regime"]


def test_bundle_composite7_view_for_adx_window():
    df = _make_uptrend()
    spec = {"short": {"classifier": "adx", "period": 10, "adx_threshold": 20.0}}
    bundle = compute_regime_bundle(df, spec)
    assert bundle["views"]["short"]["adx3"] == bundle["regime"]["short"]["regime"]
    expected = latest_regime_composite(df, 10, None)["regime"]
    assert bundle["views"]["short"]["composite7"] == expected


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
