"""Tests for shared_tools/regime.py."""

import math
import inspect
import importlib
import pathlib
import sys

import numpy as np
import pandas as pd

# Insert shared_tools into sys.path so that regime.py can import atr.py
sys.path.insert(0, str(pathlib.Path(__file__).parent))

import importlib.util

spec = importlib.util.spec_from_file_location(
    "regime", pathlib.Path(__file__).parent / "regime.py"
)
_regime_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(_regime_mod)
compute_regime = _regime_mod.compute_regime
latest_regime = _regime_mod.latest_regime
compute_multi_regime = _regime_mod.compute_multi_regime
regime_payload_for_config = _regime_mod.regime_payload_for_config
regime_label_from_payload = _regime_mod.regime_label_from_payload
required_ohlcv_limit = _regime_mod.required_ohlcv_limit
ensure_regime_columns = _regime_mod.ensure_regime_columns


# ─── Fixtures ────────────────────────────────────────────────────────────────


def _make_uptrend(n: int = 100, noise: float = 0.5) -> pd.DataFrame:
    """Monotonic uptrend: price rises ~1 per bar, triggering +DI >> -DI."""
    close = np.linspace(100.0, 200.0, n)
    high = close + noise
    low = close - noise
    return pd.DataFrame({
        "open": close - noise * 0.3,
        "high": high,
        "low": low,
        "close": close,
        "volume": np.ones(n) * 1000.0,
    })


def _make_downtrend(n: int = 100, noise: float = 0.5) -> pd.DataFrame:
    """Monotonic downtrend: price falls ~1 per bar, triggering -DI >> +DI."""
    close = np.linspace(200.0, 100.0, n)
    high = close + noise
    low = close - noise
    return pd.DataFrame({
        "open": close + noise * 0.3,
        "high": high,
        "low": low,
        "close": close,
        "volume": np.ones(n) * 1000.0,
    })


def _make_flat(n: int = 100, noise: float = 0.05) -> pd.DataFrame:
    """Flat price: TR is tiny, +DM and -DM cancel, ADX stays near 0."""
    close = np.full(n, 100.0)
    high = close + noise
    low = close - noise
    return pd.DataFrame({
        "open": close,
        "high": high,
        "low": low,
        "close": close,
        "volume": np.ones(n) * 1000.0,
    })


# ─── compute_regime tests ─────────────────────────────────────────────────────


def test_compute_regime_returns_dataframe():
    df = _make_uptrend()
    result = compute_regime(df)
    assert isinstance(result, pd.DataFrame)
    assert len(result) == len(df)


def test_compute_regime_adds_required_columns():
    df = _make_uptrend()
    result = compute_regime(df)
    for col in ("regime", "regime_score", "adx", "plus_di", "minus_di"):
        assert col in result.columns, f"Missing column: {col}"


def test_compute_regime_uptrend_labels_trending_up():
    """Monotonic uptrend should produce trending_up after ADX warmup."""
    df = _make_uptrend(n=100)
    result = compute_regime(df, period=14, adx_threshold=20.0)
    # Last bar (well past warmup) should be trending_up
    assert result["regime"].iloc[-1] == "trending_up"


def test_compute_regime_downtrend_labels_trending_down():
    """Monotonic downtrend should produce trending_down after ADX warmup."""
    df = _make_downtrend(n=100)
    result = compute_regime(df, period=14, adx_threshold=20.0)
    assert result["regime"].iloc[-1] == "trending_down"


def test_compute_regime_flat_labels_ranging():
    """Flat data keeps ADX near 0, so regime should be ranging throughout."""
    df = _make_flat(n=100)
    result = compute_regime(df, period=14, adx_threshold=20.0)
    # All bars (after warmup) should be ranging
    assert result["regime"].iloc[-1] == "ranging"


def test_compute_regime_warmup_bars_default_ranging():
    """Bars before ADX warmup completes (< 2*period) should be labeled ranging."""
    df = _make_uptrend(n=100)
    result = compute_regime(df, period=14, adx_threshold=20.0)
    # Warmup: first 2*period - 1 = 27 rows have no valid ADX → ranging
    warmup_end = 14 * 2 - 1  # index 27
    for i in range(warmup_end):
        assert result["regime"].iloc[i] == "ranging", (
            f"Row {i} should be ranging during warmup, got {result['regime'].iloc[i]}"
        )


def test_compute_regime_score_in_range():
    """regime_score should be in [0.0, 1.0] for all bars."""
    df = _make_uptrend()
    result = compute_regime(df)
    scores = result["regime_score"].dropna()
    assert (scores >= 0.0).all()
    assert (scores <= 1.0).all()


def test_compute_regime_label_values_valid():
    """All regime labels must be one of the three valid values."""
    valid = {"trending_up", "trending_down", "ranging"}
    df = _make_uptrend()
    result = compute_regime(df)
    assert set(result["regime"].unique()).issubset(valid)


def test_compute_regime_adx_non_negative():
    """ADX column should be >= 0 everywhere."""
    df = _make_uptrend()
    result = compute_regime(df)
    assert (result["adx"] >= 0).all()


def test_compute_regime_does_not_mutate_input():
    """compute_regime should return a new DataFrame, not mutate the input."""
    df = _make_uptrend()
    original_cols = set(df.columns)
    _ = compute_regime(df)
    assert set(df.columns) == original_cols


def test_compute_regime_short_df_no_crash():
    """Very short df (fewer than 2*period bars) should not crash."""
    df = _make_uptrend(n=10)
    result = compute_regime(df, period=14)
    assert len(result) == 10
    assert "regime" in result.columns
    assert (result["regime"] == "ranging").all()


def test_compute_regime_empty_df_no_crash():
    """Empty df should return an empty DataFrame with the expected columns."""
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    result = compute_regime(df)
    assert isinstance(result, pd.DataFrame)
    for col in ("regime", "regime_score", "adx", "plus_di", "minus_di"):
        assert col in result.columns


def test_regime_module_importable_as_package():
    """shared_tools.regime should support package imports as well as check-script imports."""
    mod = importlib.import_module("shared_tools.regime")
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    assert mod.latest_regime(df)["regime"] == "ranging"


def test_compute_regime_reuses_extracted_adx_core():
    """Regime detection should call the ADX helper extracted from adx_trend.py."""
    source_file = pathlib.Path(inspect.getfile(_regime_mod._compute_adx_components))

    assert source_file.name == "adx_trend.py"
    assert source_file.parent.name == "open"


def test_compute_regime_tied_di_labels_ranging(monkeypatch):
    """A DI tie has no directional winner, even if prior ADX remains elevated."""
    df = _make_uptrend(n=40)
    n = len(df)
    components = {
        "plus_di": np.full(n, 20.0),
        "minus_di": np.full(n, 20.0),
        "adx": np.full(n, 50.0),
        "adx_start": 0,
    }
    monkeypatch.setattr(_regime_mod, "_compute_adx_components", lambda *_args: components)

    result = compute_regime(df, period=14, adx_threshold=20.0)

    assert result["regime"].iloc[-1] == "ranging"


# ─── latest_regime tests ──────────────────────────────────────────────────────


def test_latest_regime_returns_dict():
    df = _make_uptrend()
    result = latest_regime(df)
    assert isinstance(result, dict)


def test_latest_regime_has_required_keys():
    df = _make_uptrend()
    result = latest_regime(df)
    assert "regime" in result
    assert "score" in result
    assert "metrics" in result
    metrics = result["metrics"]
    for key in ("adx", "plus_di", "minus_di", "atr_pct"):
        assert key in metrics, f"Missing metrics key: {key}"


def test_latest_regime_uptrend_label():
    df = _make_uptrend(n=100)
    result = latest_regime(df, period=14, adx_threshold=20.0)
    assert result["regime"] == "trending_up"


def test_latest_regime_score_in_range():
    df = _make_uptrend()
    result = latest_regime(df)
    assert 0.0 <= result["score"] <= 1.0


def test_latest_regime_metrics_finite():
    df = _make_uptrend()
    result = latest_regime(df)
    for key, val in result["metrics"].items():
        assert math.isfinite(val), f"metrics[{key}] = {val} is not finite"


def test_latest_regime_warmup_incomplete_returns_ranging():
    """When df has too few bars for ADX warmup, default to ranging."""
    df = _make_uptrend(n=5)
    result = latest_regime(df, period=14)
    assert result["regime"] == "ranging"


def test_latest_regime_empty_df_returns_ranging():
    df = pd.DataFrame(columns=["open", "high", "low", "close", "volume"])
    result = latest_regime(df)
    assert result["regime"] == "ranging"
    assert result["score"] == 0.0


# ─── ensure_regime_columns tests ─────────────────────────────────────────────


def test_ensure_regime_columns_injects_when_missing():
    df = _make_uptrend()
    assert "regime" not in df.columns
    out = ensure_regime_columns(df)
    assert "regime" in out.columns
    assert out is df  # mutates in-place


def test_ensure_regime_columns_noop_when_present():
    df = _make_uptrend()
    ensure_regime_columns(df)
    sentinel = df["regime"].copy()
    ensure_regime_columns(df)
    pd.testing.assert_series_equal(df["regime"], sentinel)


def test_ensure_regime_columns_fills_partial_existing_columns():
    """A pre-existing regime label should not block missing metric columns."""
    df = _make_uptrend()
    df["regime"] = "external"
    out = ensure_regime_columns(df, period=14, adx_threshold=20.0)
    assert out is df
    for col in ("regime", "regime_score", "adx", "plus_di", "minus_di"):
        assert col in df.columns
    assert df["regime"].iloc[-1] == "trending_up"


def test_ensure_regime_columns_idempotent():
    df = _make_uptrend()
    ensure_regime_columns(df)
    first = df["regime"].copy()
    ensure_regime_columns(df)
    pd.testing.assert_series_equal(df["regime"], first)


# ─── compute_multi_regime tests ───────────────────────────────────────────────


def test_compute_multi_regime_returns_per_window_snapshots():
    df = _make_uptrend(n=100)
    result = compute_multi_regime(df, {"short": 14, "long": 28}, adx_threshold=20.0)
    assert set(result.keys()) == {"long", "short"}
    for snap in result.values():
        assert "regime" in snap
        assert "score" in snap
        assert "metrics" in snap


def test_compute_multi_regime_uptrend_labels():
    df = _make_uptrend(n=100)
    result = compute_multi_regime(df, {"short": 14, "long": 28})
    assert result["short"]["regime"] == "trending_up"
    assert result["long"]["regime"] == "trending_up"


def test_compute_multi_regime_empty_windows_raises():
    df = _make_uptrend()
    try:
        compute_multi_regime(df, {})
        assert False, "expected ValueError"
    except ValueError as exc:
        assert "non-empty" in str(exc)


def test_compute_multi_regime_invalid_period_raises():
    df = _make_uptrend()
    try:
        compute_multi_regime(df, {"short": 1})
        assert False, "expected ValueError"
    except ValueError as exc:
        assert "period" in str(exc)


def test_compute_multi_regime_short_df_warmup_ranging():
    df = _make_uptrend(n=10)
    result = compute_multi_regime(df, {"short": 14, "long": 28})
    assert result["short"]["regime"] == "ranging"
    assert result["long"]["regime"] == "ranging"


def test_regime_payload_for_config_legacy():
    df = _make_uptrend(n=100)
    payload = regime_payload_for_config(df, period=14, adx_threshold=20.0)
    assert isinstance(payload, dict)
    assert payload["regime"] == "trending_up"


def test_regime_payload_for_config_multi():
    df = _make_uptrend(n=100)
    payload = regime_payload_for_config(
        df, period=14, windows={"short": 14, "long": 28}
    )
    assert "short" in payload
    assert payload["short"]["regime"] == "trending_up"


def test_regime_label_from_payload_legacy_and_multi():
    legacy = {"regime": "trending_up", "score": 0.5, "metrics": {}}
    assert regime_label_from_payload(legacy) == "trending_up"
    multi = {"short": {"regime": "ranging", "score": 0.1, "metrics": {}},
             "long": {"regime": "trending_up", "score": 0.8, "metrics": {}}}
    assert regime_label_from_payload(multi, "short") == "ranging"
    assert regime_label_from_payload(multi, "long") == "trending_up"


def test_required_ohlcv_limit_scales_with_windows():
    assert required_ohlcv_limit(period=14) == 200
    assert required_ohlcv_limit(period=14, windows={"long": 2160}) >= 4320
