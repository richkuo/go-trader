"""Tests for shared_tools/regime.py."""

import math
import inspect
import importlib
import pathlib
import sys

import numpy as np
import pandas as pd
import pytest

# Insert shared_tools into sys.path so that regime.py can import atr.py
sys.path.insert(0, str(pathlib.Path(__file__).parent))

import importlib.util

spec = importlib.util.spec_from_file_location(
    "regime", pathlib.Path(__file__).parent / "regime.py"
)
_regime_mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(_regime_mod)
compute_regime = _regime_mod.compute_regime
compute_regime_bundle = _regime_mod.compute_regime_bundle
map_adx_label = _regime_mod.map_adx_label
latest_regime = _regime_mod.latest_regime
compute_multi_regime = _regime_mod.compute_multi_regime
regime_payload_for_config = _regime_mod.regime_payload_for_config
regime_label_from_payload = _regime_mod.regime_label_from_payload
required_ohlcv_limit = _regime_mod.required_ohlcv_limit
parse_regime_windows_json = _regime_mod.parse_regime_windows_json
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


def test_parse_regime_windows_json_rejects_reserved_name():
    with pytest.raises(ValueError, match="reserved"):
        parse_regime_windows_json('{"regime": 168}')


def test_map_composite_label_states():
    th = {"return_pct": 0.05, "range_pct": 0.03, "adx": 25, "efficiency": 0.5}
    m = _regime_mod.map_composite_label
    # (return_eff, adx, range_eff, efficiency, thresholds)
    # Clean trend: big net move + high efficiency + high ADX.
    assert m(0.10, 30, 0.10, 0.7, th) == "trending_up_clean"
    # Choppy trend: big net move but ADX too low to confirm clean.
    assert m(0.10, 10, 0.10, 0.7, th) == "trending_up_choppy"
    # Choppy trend: big net move, high ADX, but low efficiency (lots of churn).
    assert m(0.10, 30, 0.10, 0.2, th) == "trending_up_choppy"
    assert m(-0.10, 30, 0.10, 0.7, th) == "trending_down_clean"
    assert m(-0.10, 10, 0.10, 0.7, th) == "trending_down_choppy"
    # Ranging family: no decisive net move.
    assert m(0.01, 10, 0.01, 0.0, th) == "ranging_quiet"
    assert m(0.01, 10, 0.10, 0.0, th) == "ranging_volatile"
    assert m(0.01, 30, 0.10, 0.0, th) == "ranging_directional"


def test_latest_regime_composite_ranging_not_trending():
    """Regression: a mean-reverting market must not be labeled trending.

    Pre-fix the metric divided whole-window numerators by a single-bar ATR, so
    `big_move`/`wide` were always true and ranging labels were unreachable.
    """
    import numpy as np

    n = 200
    period = 50
    idx = pd.date_range("2024-01-01", periods=n, freq="h")
    rng = np.random.default_rng(0)
    prices = 100 + 2 * np.sin(np.linspace(0, 8 * np.pi, n)) + rng.normal(0, 0.3, n)
    df = pd.DataFrame(
        {
            "open": prices,
            "high": prices * 1.003,
            "low": prices * 0.997,
            "close": prices,
        },
        index=idx,
    )
    snap = _regime_mod.latest_regime_composite(df, period=period)
    assert snap["regime"].startswith("ranging"), snap


def test_parse_regime_windows_spec_json_composite():
    spec = _regime_mod.parse_regime_windows_spec_json(
        '{"macro":{"classifier":"composite","period":100,"thresholds":{"return_pct":0.05,"range_pct":0.03,"adx":25}}}'
    )
    assert spec["macro"]["classifier"] == "composite"
    assert spec["macro"]["period"] == 100


def test_compute_regime_bundle_raw_projects_like_latest_regime_adx3():
    df = _make_uptrend(n=120)
    bundle = compute_regime_bundle(df, period=14)
    assert bundle is not None
    snap = latest_regime(df, period=14, adx_threshold=20.0)
    raw = bundle["raw"]
    projected = map_adx_label(raw["adx"], raw["plus_di"], raw["minus_di"], 20.0)
    assert projected == snap["regime"]
    assert raw["adx"] == pytest.approx(snap["metrics"]["adx"])


def test_compute_regime_bundle_period_28_matches_full_period_adx():
    df = _make_uptrend(n=200)
    bundle = compute_regime_bundle(df, period=28)
    assert bundle is not None
    snap = latest_regime(df, period=28, adx_threshold=20.0)
    raw = bundle["raw"]
    assert map_adx_label(raw["adx"], raw["plus_di"], raw["minus_di"], 20.0) == snap["regime"]


def test_compute_regime_bundle_raw_projects_like_latest_composite():
    df = _make_uptrend(n=120)
    bundle = compute_regime_bundle(df, period=14)
    assert bundle is not None
    snap = _regime_mod.latest_regime_composite(df, period=14)
    raw = bundle["raw"]
    projected = _regime_mod.map_composite_label(
        raw["return_eff"],
        raw["composite_adx"],
        raw["range_eff"],
        raw["efficiency"],
        _regime_mod._DEFAULT_COMPOSITE_THRESHOLDS,
    )
    assert projected == snap["regime"]


def test_ensure_regime_columns_last_bar_matches_latest_regime():
    df = _make_uptrend(n=120)
    out = ensure_regime_columns(df.copy(), period=14, adx_threshold=20.0)
    snap = latest_regime(df, period=14, adx_threshold=20.0)
    assert str(out["regime"].iloc[-1]) == snap["regime"]


def test_map_adx_label_trending_down():
    assert map_adx_label(30, 5, 20, 20) == "trending_down"


def test_resolve_check_regime_uses_injected_legacy():
    df = _make_uptrend(n=120)
    stdout, live, strategy = _regime_mod.resolve_check_regime(
        df,
        regime_enabled=True,
        injected_payload="trending_up",
    )
    assert stdout == "trending_up"
    assert live == "trending_up"
    assert strategy["regime"] == "trending_up"


def test_resolve_check_regime_falls_back_to_inline():
    df = _make_uptrend(n=120)
    out_inj = _regime_mod.resolve_check_regime(df, regime_enabled=True, injected_payload="ranging")
    out_inline = _regime_mod.resolve_check_regime(df, regime_enabled=True, injected_payload=None)
    assert out_inj[0] == "ranging"
    assert out_inline[0] in _regime_mod.VALID_LABELS_ADX


def test_latest_regime_composite_downtrend():
    df = _make_downtrend(n=120)
    snap = _regime_mod.latest_regime_composite(df, period=50, thresholds={"return_pct": 0.02, "range_pct": 0.02, "adx": 15})
    assert snap["regime"] in _regime_mod.VALID_LABELS_COMPOSITE
    assert "trending_down" in snap["regime"]
