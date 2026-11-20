"""Integration checks for the #1940 regime-classifier calibration."""
import hashlib
from pathlib import Path

import numpy as np
import pandas as pd
import pytest


_CANDLES = Path(__file__).resolve().parents[1] / "testdata" / "ledger_export" / "regime_market" / "BTC_1h_candles.csv.gz"


def _canon(labels, feats) -> str:
    lines = []
    for row in feats:
        parts = ["nan" if np.isnan(v) else f"{v:.10g}" for v in row]
        lines.append(",".join(parts))
    blob = "labels\n" + "|".join(labels) + "\nfeats\n" + "\n".join(lines)
    return hashlib.sha256(blob.encode()).hexdigest()


def test_explicit_diagnostics_reject_conflicts_and_keep_the_default_payload_shape(monkeypatch):
    import data_fetcher
    from regime_diagnostics import run_window, _OMITTED
    seen = {}

    def fake_load(symbol, timeframe, exchange_id="binanceus", start_date=None, end_date=None):
        seen["exchange"] = exchange_id
        return _price_frame(80, seed=1)

    monkeypatch.setattr(data_fetcher, "load_cached_data", fake_load)
    default = run_window("BTC/USDT", "1h", "is", horizons=(4,), n_perm=20)
    assert "h4" in default and "inputs" not in default
    explicit = run_window("BTC/USDT", "1h", "is", horizons=(4,), n_perm=20, classifier="adx",
                          period=10, adx_threshold=15, exchange="hyperliquid")
    assert seen["exchange"] == "hyperliquid"
    assert explicit["h4"]["separation"]["kruskal_h"] >= 0
    with pytest.raises(ValueError, match="conflicting model"):
        run_window("BTC/USDT", "1h", "is", model={"period": 48, "states": []}, classifier="adx")
    with pytest.raises(ValueError, match="period"):
        run_window("BTC/USDT", "1h", "is", period=1)
    with pytest.raises(ValueError, match="adx_period_cap"):
        run_window("BTC/USDT", "1h", "is", classifier="adx", adx_period_cap=14)
    assert _OMITTED is not None


def _price_frame(n, seed):
    rng = np.random.default_rng(seed)
    close = 100 * np.exp(np.cumsum(rng.normal(0, 0.01, n)))
    return pd.DataFrame({
        "open": close, "high": close * 1.01, "low": close * 0.99, "close": close, "volume": np.ones(n),
    })
