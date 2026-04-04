"""Tests for deribit/utils.py — mock HTTP requests to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime, timezone, timedelta

# Load the deribit utils by file path to avoid module name collisions
_utils_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "utils.py")
_spec = importlib.util.spec_from_file_location("deribit_utils", _utils_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

_format_instrument = _mod._format_instrument
fetch_available_expiries = _mod.fetch_available_expiries
find_closest_expiry = _mod.find_closest_expiry
fetch_available_strikes = _mod.fetch_available_strikes
find_closest_strike = _mod.find_closest_strike
get_live_quote = _mod.get_live_quote
get_live_premium = _mod.get_live_premium


# ─── Instrument Formatting ─────────────────────────

class TestFormatInstrument:
    def test_btc_call(self):
        result = _format_instrument("BTC", "call", 75000, "2026-03-13")
        assert result == "BTC-13MAR26-75000-C"

    def test_eth_put(self):
        result = _format_instrument("ETH", "put", 3500, "2026-12-25")
        assert result == "ETH-25DEC26-3500-P"

    def test_lowercase_underlying(self):
        result = _format_instrument("btc", "call", 100000, "2026-01-15")
        assert result.startswith("BTC-")

    def test_lowercase_option_type(self):
        result = _format_instrument("BTC", "Call", 80000, "2026-06-20")
        assert result.endswith("-C")

    def test_put_suffix(self):
        result = _format_instrument("BTC", "put", 80000, "2026-06-20")
        assert result.endswith("-P")


# ─── Fetch Available Expiries ──────────────────────

class TestFetchAvailableExpiries:
    def _mock_instruments(self, days_list):
        """Build a mock API response with expiries at given days offsets."""
        now = datetime.now(timezone.utc)
        instruments = []
        for d in days_list:
            exp_time = now + timedelta(days=d)
            instruments.append({
                "expiration_timestamp": int(exp_time.timestamp() * 1000),
                "instrument_name": f"BTC-TEST-C",
            })
        return instruments

    def test_filters_by_dte_range(self):
        instruments = self._mock_instruments([5, 15, 30, 45, 90])
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            result = fetch_available_expiries("BTC", min_dte=7, max_dte=60)
            dtes = [dte for _, dte in result]
            assert all(7 <= d <= 60 for d in dtes)
            # 5-day and 90-day should be excluded
            assert 5 not in dtes
            assert 90 not in dtes

    def test_returns_sorted(self):
        instruments = self._mock_instruments([45, 15, 30])
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            result = fetch_available_expiries("BTC", min_dte=7, max_dte=60)
            dtes = [dte for _, dte in result]
            assert dtes == sorted(dtes)

    def test_returns_empty_on_error(self):
        with patch("requests.get", side_effect=Exception("network")):
            assert fetch_available_expiries("BTC") == []


# ─── Find Closest Expiry ──────────────────────────

class TestFindClosestExpiry:
    def test_finds_closest(self):
        now = datetime.now(timezone.utc)
        instruments = []
        for d in [14, 21, 28, 35]:
            exp_time = now + timedelta(days=d)
            instruments.append({
                "expiration_timestamp": int(exp_time.timestamp() * 1000),
                "instrument_name": f"BTC-TEST-C",
            })
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            result = find_closest_expiry("BTC", target_dte=20)
            assert result is not None
            _, actual_dte = result
            assert abs(actual_dte - 21) <= 1  # closest to 20 is 21

    def test_returns_none_outside_tolerance(self):
        now = datetime.now(timezone.utc)
        instruments = [{
            "expiration_timestamp": int((now + timedelta(days=60)).timestamp() * 1000),
            "instrument_name": "BTC-TEST-C",
        }]
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            result = find_closest_expiry("BTC", target_dte=14, max_tolerance_days=7)
            assert result is None

    def test_returns_none_on_empty(self):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": []}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            assert find_closest_expiry("BTC", target_dte=30) is None


# ─── Fetch Available Strikes ──────────────────────

class TestFetchAvailableStrikes:
    def test_fetches_strikes_for_expiry(self):
        now = datetime.now(timezone.utc)
        target_date = now + timedelta(days=30)
        # Deribit expiry is at 8:00 UTC
        exp_time = target_date.replace(hour=8, minute=0, second=0, microsecond=0)
        exp_ts = int(exp_time.timestamp() * 1000)
        expiry_str = target_date.strftime("%Y-%m-%d")

        instruments = [
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-65000-C", "strike": 65000},
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-70000-C", "strike": 70000},
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-70000-P", "strike": 70000},
            # Different expiry
            {"expiration_timestamp": exp_ts + 86400 * 7 * 1000, "instrument_name": "BTC-80000-C", "strike": 80000},
        ]
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            strikes = fetch_available_strikes("BTC", expiry_str, "call")
            assert 65000 in strikes
            assert 70000 in strikes
            assert 80000 not in strikes  # different expiry

    def test_returns_empty_on_error(self):
        with patch("requests.get", side_effect=Exception("fail")):
            assert fetch_available_strikes("BTC", "2026-05-01", "call") == []


# ─── Find Closest Strike ──────────────────────────

class TestFindClosestStrike:
    def test_finds_closest(self):
        now = datetime.now(timezone.utc)
        target_date = now + timedelta(days=30)
        exp_time = target_date.replace(hour=8, minute=0, second=0, microsecond=0)
        exp_ts = int(exp_time.timestamp() * 1000)
        expiry_str = target_date.strftime("%Y-%m-%d")

        instruments = [
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-65000-C", "strike": 65000},
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-70000-C", "strike": 70000},
            {"expiration_timestamp": exp_ts, "instrument_name": "BTC-75000-C", "strike": 75000},
        ]
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": instruments}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            strike = find_closest_strike("BTC", expiry_str, "call", 67000)
            assert strike == 65000

    def test_returns_none_when_empty(self):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": []}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            assert find_closest_strike("BTC", "2026-05-01", "call", 67000) is None


# ─── Live Quote ────────────────────────────────────

class TestGetLiveQuote:
    def test_returns_quote(self):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {
            "result": {
                "mark_price": 0.045,
                "underlying_price": 67000,
                "greeks": {
                    "delta": 0.55,
                    "gamma": 0.0001,
                    "theta": -20.0,
                    "vega": 50.0,
                },
            }
        }
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            quote = get_live_quote("BTC", "call", 70000, "2026-05-01")
            assert quote is not None
            assert quote["mark_price"] == 0.045
            assert quote["greeks"]["delta"] == 0.55

    def test_returns_none_on_zero_price(self):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {"result": {"mark_price": 0}}
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            assert get_live_quote("BTC", "call", 70000, "2026-05-01") is None

    def test_returns_none_on_error(self):
        with patch("requests.get", side_effect=Exception("timeout")):
            assert get_live_quote("BTC", "call", 70000, "2026-05-01") is None


class TestGetLivePremium:
    def test_returns_mark_price(self):
        mock_resp = MagicMock()
        mock_resp.json.return_value = {
            "result": {
                "mark_price": 0.055,
                "underlying_price": 67000,
                "greeks": {"delta": 0.5, "gamma": 0, "theta": 0, "vega": 0},
            }
        }
        mock_resp.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_resp):
            premium = get_live_premium("BTC", "call", 70000, "2026-05-01")
            assert premium == 0.055

    def test_returns_none_on_failure(self):
        with patch("requests.get", side_effect=Exception("fail")):
            assert get_live_premium("BTC", "call", 70000, "2026-05-01") is None
