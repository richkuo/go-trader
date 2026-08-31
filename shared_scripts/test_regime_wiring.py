
import json
import pathlib
import sys

import numpy as np
import pandas as pd

_SHARED_TOOLS = pathlib.Path(__file__).parent.parent / "shared_tools"
_REPO_ROOT = _SHARED_TOOLS.parent
if str(_REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(_REPO_ROOT))
if str(_SHARED_TOOLS) not in sys.path:
    sys.path.insert(0, str(_SHARED_TOOLS))

from shared_tools.conftest import load_module
from shared_tools.conftest import make_ohlcv

_REGIME = load_module("_regime_wiring_regime_test", _SHARED_TOOLS / "regime.py")
latest_regime = _REGIME.latest_regime
latest_regime_composite = _REGIME.latest_regime_composite
_STRATEGY_COMPOSITION = load_module("_regime_wiring_composition_test", _SHARED_TOOLS / "strategy_composition.py")


def _make_uptrend_df(n: int = 100) -> pd.DataFrame:
    close = np.linspace(100.0, 200.0, n)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h", tz="UTC")
    return make_ohlcv(close, volume=1000.0, noise=0.5, index=idx)


def _make_flat_df(n: int = 100) -> pd.DataFrame:
    close = np.full(n, 100.0)
    idx = pd.date_range("2024-01-01", periods=n, freq="1h", tz="UTC")
    return make_ohlcv(close, volume=1000.0, noise=0.05, index=idx)


def test_latest_regime_output_json_serializable_uptrend():
    df = _make_uptrend_df()
    payload = latest_regime(df)
    serialized = json.dumps(payload)
    parsed = json.loads(serialized)
    assert parsed["regime"] in ("trending_up", "trending_down", "ranging")
    assert isinstance(parsed["score"], float)
    assert isinstance(parsed["metrics"], dict)


def test_latest_regime_output_json_serializable_flat():
    df = _make_flat_df()
    payload = latest_regime(df)
    serialized = json.dumps(payload)
    parsed = json.loads(serialized)
    assert parsed["regime"] == "ranging"


def test_composite_json_never_contains_nan_token_when_hurst_omitted():
    df = _make_flat_df(n=60)
    payload = latest_regime_composite(df, period=20)
    assert "hurst" not in payload["metrics"]
    serialized = json.dumps(payload)
    assert "NaN" not in serialized
    json.loads(serialized)


def test_composite_json_includes_finite_hurst_when_data_sufficient():
    df = _make_uptrend_df(n=300)
    payload = latest_regime_composite(df, period=20)
    assert "hurst" in payload["metrics"]
    serialized = json.dumps(payload)
    assert "NaN" not in serialized
    parsed = json.loads(serialized)
    assert isinstance(parsed["metrics"]["hurst"], float)
    assert np.isfinite(parsed["metrics"]["hurst"])


def test_adx_classifier_never_emits_hurst_metric():
    df = _make_uptrend_df(n=300)
    payload = latest_regime(df, period=20)
    assert "hurst" not in (payload.get("metrics") or {})
    assert "hurst" in latest_regime_composite(df, period=20)["metrics"]


def test_composite_hurst_is_a_finite_float_but_is_NOT_bounded_to_zero_one():
    saw_above_one = False
    for df in (_make_uptrend_df(n=300), _make_flat_df(n=300)):
        metrics = latest_regime_composite(df, period=20)["metrics"]
        if "hurst" not in metrics:
            continue
        h = metrics["hurst"]
        assert isinstance(h, float)
        assert np.isfinite(h), h
        assert h > 0.0, h
        assert round(h, 4) == h
        saw_above_one = saw_above_one or h > 1.0
    assert saw_above_one


def test_hurst_survives_the_go_metrics_map_contract():
    payload = latest_regime_composite(_make_uptrend_df(n=300), period=20)
    parsed = json.loads(json.dumps(payload))
    for key, value in parsed["metrics"].items():
        assert isinstance(value, (int, float)), (key, type(value))
        assert not isinstance(value, bool), key
        assert np.isfinite(value), key


def test_regime_label_string_is_safe_for_output_field():
    df = _make_uptrend_df()
    payload = latest_regime(df)
    label = payload["regime"]
    assert isinstance(label, str)
    assert label in ("trending_up", "trending_down", "ranging")
    assert json.dumps({"regime": label})


def test_strip_unsupported_drops_regime_for_non_aware_function():
    def dummy_strategy(df, rsi_period=14):
        return df

    df = _make_uptrend_df()
    params = {"rsi_period": 14, "regime": latest_regime(df)}
    stripped = _STRATEGY_COMPOSITION.strip_unsupported_position_context(dummy_strategy, params)
    assert "regime" not in stripped
    assert stripped["rsi_period"] == 14


def test_strip_unsupported_keeps_regime_for_aware_function():
    def regime_aware_strategy(df, regime=None, rsi_period=14):
        return df

    df = _make_uptrend_df()
    params = {"rsi_period": 14, "regime": latest_regime(df)}
    stripped = _STRATEGY_COMPOSITION.strip_unsupported_position_context(regime_aware_strategy, params)
    assert "regime" in stripped
    assert stripped["rsi_period"] == 14


def test_strip_unsupported_drops_regime_for_var_keyword_wrapper():
    def dummy_strategy_wrapper(df, **params):
        return df

    df = _make_uptrend_df()
    params = {
        "adx_period": 14,
        "adx_threshold": 25,
        "regime": latest_regime(df),
        "side": "long",
        "avg_cost": 100.0,
    }
    stripped = _STRATEGY_COMPOSITION.strip_unsupported_position_context(dummy_strategy_wrapper, params)
    assert "regime" not in stripped
    assert "side" not in stripped
    assert "avg_cost" not in stripped
    assert stripped["adx_period"] == 14
    assert stripped["adx_threshold"] == 25


def test_apply_strategy_does_not_crash_on_regime_injection():
    futures_path = (
        pathlib.Path(__file__).parent.parent
        / "shared_strategies" / "open" / "futures" / "strategies.py"
    )
    mod = load_module("_futures_shim_720", futures_path)

    df = _make_uptrend_df(n=200)
    params = {"regime": latest_regime(df)}
    for name in ("adx_trend", "sweep_squeeze_combo", "amd_ifvg", "range_scalper"):
        if name not in mod.STRATEGY_REGISTRY:
            continue
        result = mod.apply_strategy(name, df, params)
        assert "signal" in result.columns, f"{name} returned no signal column"


def _load_check_options_module():
    src_path = pathlib.Path(__file__).parent / "check_options.py"
    return load_module("_check_options_under_test", src_path)


def test_check_options_regime_label_from_uptrend_df():
    module = _load_check_options_module()
    df = _make_uptrend_df(100)
    label = module._regime_label_from_df(df)
    assert label in ("trending_up", "trending_down", "ranging")


def test_check_options_regime_label_from_short_df_is_none():
    module = _load_check_options_module()
    df = _make_uptrend_df(10)
    assert module._regime_label_from_df(df) is None


def test_check_options_regime_label_from_none_df_is_none():
    module = _load_check_options_module()
    assert module._regime_label_from_df(None) is None


def test_check_options_fetch_ohlcv_df_uses_adapter_when_available():
    module = _load_check_options_module()

    class StubAdapter:
        def __init__(self, rows):
            self._rows = rows
            self.calls = []

        def get_ohlcv(self, symbol, timeframe, limit):
            self.calls.append((symbol, timeframe, limit))
            return self._rows

    rows = [
        [i * 1000, 100.0 + i, 101.0 + i, 99.0 + i, 100.5 + i, 1000.0]
        for i in range(50)
    ]
    adapter = StubAdapter(rows)
    df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=adapter)
    assert df is not None
    assert len(df) == 50
    assert {"high", "low", "close"}.issubset(df.columns)
    assert adapter.calls == [("BTC", "4h", 100)]


def test_check_options_fetch_ohlcv_df_short_returns_none():
    module = _load_check_options_module()

    class StubAdapter:
        def get_ohlcv(self, symbol, timeframe, limit):
            return [[i * 1000, 100.0, 101.0, 99.0, 100.5, 1000.0] for i in range(5)]

    df = module._fetch_ohlcv_df("BTC", "4h", 100, 30, adapter=StubAdapter())
    assert df is None


def test_latest_regime_honors_custom_period():
    df = _make_uptrend_df(50)
    warmup = latest_regime(df, period=200)
    real = latest_regime(df, period=14)
    assert warmup["metrics"]["adx"] == 0.0
    assert real["metrics"]["adx"] > 0.0


def test_latest_regime_honors_custom_adx_threshold():
    df = _make_uptrend_df(100)
    assert latest_regime(df, adx_threshold=101.0)["regime"] == "ranging"
    assert latest_regime(df, adx_threshold=50.0)["regime"] == "trending_up"
