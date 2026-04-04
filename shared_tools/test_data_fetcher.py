"""Tests for data_fetcher.py — OHLCV fetching with mocked CCXT."""

import time
from unittest.mock import MagicMock, patch, PropertyMock

import pandas as pd
import pytest
import ccxt


# We need to mock the `storage` module before importing data_fetcher,
# because data_fetcher does `from storage import store_ohlcv, load_ohlcv`
# and storage.py calls init_db() at import time (creates a real DB file).
import sys
from unittest.mock import MagicMock as _MagicMock

_real_storage = sys.modules.get("storage")
_mock_storage = _MagicMock()
_mock_storage.store_ohlcv = _MagicMock()
_mock_storage.load_ohlcv = _MagicMock(return_value=pd.DataFrame())
sys.modules["storage"] = _mock_storage

from data_fetcher import get_exchange, fetch_ohlcv, fetch_full_history, load_cached_data

# Restore so test_storage.py (imported later) gets the real module
if _real_storage is not None:
    sys.modules["storage"] = _real_storage
else:
    del sys.modules["storage"]


# ─── Fixtures ──────────────────────────────────

SAMPLE_CANDLES = [
    [1700000000000, 35000.0, 35500.0, 34800.0, 35200.0, 100.0],
    [1700003600000, 35200.0, 35800.0, 35100.0, 35600.0, 120.0],
    [1700007200000, 35600.0, 36000.0, 35400.0, 35900.0, 90.0],
    [1700010800000, 35900.0, 36200.0, 35700.0, 36100.0, 110.0],
    [1700014400000, 36100.0, 36300.0, 35900.0, 36000.0, 80.0],
]


def _make_mock_exchange(candles=None, exchange_id="binanceus"):
    """Create a mock exchange that returns given candles."""
    mock_ex = MagicMock()
    mock_ex.fetch_ohlcv.return_value = candles if candles is not None else SAMPLE_CANDLES
    mock_ex.parse8601.side_effect = lambda s: int(pd.Timestamp(s).timestamp() * 1000)
    mock_ex.milliseconds.return_value = int(time.time() * 1000)
    mock_ex.rateLimit = 100
    return mock_ex


# ─── get_exchange ──────────────────────────────

class TestGetExchange:
    def test_returns_exchange_instance(self):
        # binanceus is a real ccxt exchange
        ex = get_exchange("binanceus")
        assert isinstance(ex, ccxt.Exchange)

    def test_enables_rate_limit(self):
        ex = get_exchange("binanceus")
        assert ex.enableRateLimit is True

    def test_invalid_exchange_raises(self):
        with pytest.raises(AttributeError):
            get_exchange("nonexistent_exchange_xyz")


# ─── fetch_ohlcv ───────────────────────────────

class TestFetchOhlcv:
    @patch("data_fetcher.get_exchange")
    def test_returns_dataframe_with_expected_columns(self, mock_get_ex):
        mock_get_ex.return_value = _make_mock_exchange()
        df = fetch_ohlcv("BTC/USDT", "1h", limit=5, store=False)

        assert isinstance(df, pd.DataFrame)
        for col in ("timestamp", "open", "high", "low", "close", "volume"):
            assert col in df.columns
        assert len(df) == 5

    @patch("data_fetcher.get_exchange")
    def test_datetime_index(self, mock_get_ex):
        mock_get_ex.return_value = _make_mock_exchange()
        df = fetch_ohlcv("BTC/USDT", "1h", limit=5, store=False)
        assert df.index.name == "datetime"

    @patch("data_fetcher.get_exchange")
    def test_since_parameter_parsed(self, mock_get_ex):
        mock_ex = _make_mock_exchange()
        mock_get_ex.return_value = mock_ex
        fetch_ohlcv("BTC/USDT", "1d", since="2024-01-01", limit=5, store=False)

        # parse8601 should have been called with the ISO string
        mock_ex.parse8601.assert_called_once_with("2024-01-01T00:00:00Z")
        # fetch_ohlcv should pass the parsed timestamp
        call_args = mock_ex.fetch_ohlcv.call_args
        assert call_args[1]["since"] is not None

    @patch("data_fetcher.get_exchange")
    def test_empty_response_returns_empty_df(self, mock_get_ex):
        mock_ex = _make_mock_exchange(candles=[])
        mock_get_ex.return_value = mock_ex
        df = fetch_ohlcv("BTC/USDT", "1h", limit=5, store=False)
        assert len(df) == 0
        assert "timestamp" in df.columns

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    def test_store_called_when_enabled(self, mock_store, mock_get_ex):
        mock_get_ex.return_value = _make_mock_exchange()
        fetch_ohlcv("BTC/USDT", "1h", limit=5, store=True)
        mock_store.assert_called_once()

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    def test_store_not_called_when_disabled(self, mock_store, mock_get_ex):
        mock_get_ex.return_value = _make_mock_exchange()
        fetch_ohlcv("BTC/USDT", "1h", limit=5, store=False)
        mock_store.assert_not_called()


# ─── fetch_full_history ────────────────────────

class TestFetchFullHistory:
    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")  # skip rate-limit sleeps
    def test_paginates_through_data(self, mock_sleep, mock_store, mock_get_ex):
        # Simulate two pages: first returns 3 candles, second returns empty
        page1 = [
            [1700000000000, 35000, 35500, 34800, 35200, 100],
            [1700003600000, 35200, 35800, 35100, 35600, 120],
            [1700007200000, 35600, 36000, 35400, 35900, 90],
        ]

        mock_ex = _make_mock_exchange()
        # First call returns page1, second call returns empty
        mock_ex.fetch_ohlcv.side_effect = [page1, []]
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000  # future of the data
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 3
        assert mock_ex.fetch_ohlcv.call_count == 2

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_deduplicates_candles(self, mock_sleep, mock_store, mock_get_ex):
        # Two pages with overlapping timestamps
        page1 = [
            [1700000000000, 35000, 35500, 34800, 35200, 100],
            [1700003600000, 35200, 35800, 35100, 35600, 120],
        ]
        page2 = [
            [1700003600000, 35200, 35800, 35100, 35600, 120],  # duplicate
            [1700007200000, 35600, 36000, 35400, 35900, 90],
        ]

        mock_ex = _make_mock_exchange()
        mock_ex.fetch_ohlcv.side_effect = [page1, page2, []]
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 3  # 3 unique timestamps

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_empty_history(self, mock_sleep, mock_store, mock_get_ex):
        mock_ex = _make_mock_exchange(candles=[])
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 0

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_rate_limit_retry(self, mock_sleep, mock_store, mock_get_ex):
        mock_ex = _make_mock_exchange()
        # First call raises RateLimitExceeded, second succeeds, third returns empty
        mock_ex.fetch_ohlcv.side_effect = [
            ccxt.RateLimitExceeded("rate limited"),
            SAMPLE_CANDLES[:2],
            [],
        ]
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 2

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_network_error_retry(self, mock_sleep, mock_store, mock_get_ex):
        mock_ex = _make_mock_exchange()
        mock_ex.fetch_ohlcv.side_effect = [
            ccxt.NetworkError("timeout"),
            SAMPLE_CANDLES[:2],
            [],
        ]
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 2

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_rate_limit_abort_after_5(self, mock_sleep, mock_store, mock_get_ex):
        mock_ex = _make_mock_exchange()
        mock_ex.fetch_ohlcv.side_effect = ccxt.RateLimitExceeded("rate limited")
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        assert len(df) == 0
        assert mock_ex.fetch_ohlcv.call_count == 5

    @patch("data_fetcher.get_exchange")
    @patch("data_fetcher.store_ohlcv")
    @patch("time.sleep")
    def test_no_progress_breaks_loop(self, mock_sleep, mock_store, mock_get_ex):
        # Same candle returned every time → last_ts == current_since → break
        candle = [[1700000000000, 35000, 35500, 34800, 35200, 100]]
        mock_ex = MagicMock()
        mock_ex.fetch_ohlcv.return_value = candle
        mock_ex.parse8601.return_value = 1700000000000
        mock_ex.milliseconds.return_value = 1700100000000
        mock_ex.rateLimit = 100
        mock_get_ex.return_value = mock_ex

        df = fetch_full_history("BTC/USDT", "1h", since="2023-11-14", store=False)
        # Should break after first call since last_ts == current_since
        assert mock_ex.fetch_ohlcv.call_count == 1


# ─── load_cached_data ──────────────────────────

class TestLoadCachedData:
    @patch("data_fetcher.load_ohlcv")
    @patch("data_fetcher.fetch_full_history")
    def test_returns_cached_when_available(self, mock_fetch, mock_load):
        cached_df = pd.DataFrame({
            "timestamp": [1700000000000],
            "open": [35000],
            "high": [35500],
            "low": [34800],
            "close": [35200],
            "volume": [100],
        })
        cached_df["datetime"] = pd.to_datetime(cached_df["timestamp"], unit="ms")
        cached_df.set_index("datetime", inplace=True)
        mock_load.return_value = cached_df

        df = load_cached_data("BTC/USDT", "1d")
        assert len(df) == 1
        mock_fetch.assert_not_called()

    @patch("data_fetcher.load_ohlcv")
    @patch("data_fetcher.fetch_full_history")
    def test_fetches_when_cache_empty(self, mock_fetch, mock_load):
        mock_load.return_value = pd.DataFrame()
        mock_fetch.return_value = pd.DataFrame({
            "timestamp": [1700000000000],
            "open": [35000],
            "high": [35500],
            "low": [34800],
            "close": [35200],
            "volume": [100],
        })

        df = load_cached_data("BTC/USDT", "1d")
        mock_fetch.assert_called_once()

    @patch("data_fetcher.load_ohlcv")
    def test_date_range_filtering(self, mock_load):
        mock_load.return_value = pd.DataFrame({
            "timestamp": [1700000000000],
            "open": [35000],
            "high": [35500],
            "low": [34800],
            "close": [35200],
            "volume": [100],
            "datetime": [pd.Timestamp("2023-11-14")],
        }).set_index("datetime")

        load_cached_data("BTC/USDT", "1d", start_date="2023-01-01", end_date="2024-01-01")

        call_args = mock_load.call_args
        # Should pass start_ts and end_ts
        assert call_args[0][3] is not None  # start_ts
        assert call_args[0][4] is not None  # end_ts
