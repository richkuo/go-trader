"""Tests for TopStepExchangeAdapter — mock requests/yfinance to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch
from datetime import datetime

# Load topstep adapter by file path to avoid module name collisions
_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_spec = importlib.util.spec_from_file_location("topstep_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)

TopStepExchangeAdapter = _mod.TopStepExchangeAdapter
CONTRACT_SPECS = _mod.CONTRACT_SPECS
YAHOO_SYMBOL_MAP = _mod.YAHOO_SYMBOL_MAP


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        assert adapter.name == "topstep"

    def test_paper_mode(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        assert adapter.mode == "paper"
        assert adapter.is_live is False

    def test_live_mode_no_creds_raises(self):
        with patch.dict(os.environ, {}, clear=False):
            for key in ("TOPSTEP_API_KEY", "TOPSTEP_API_SECRET", "TOPSTEP_ACCOUNT_ID"):
                os.environ.pop(key, None)
            with pytest.raises(RuntimeError, match="Live mode requires"):
                TopStepExchangeAdapter(mode="live")


# ─── Contract Specs ────────────────────────────────

class TestContractSpecs:
    def test_get_es_spec(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        spec = adapter.get_contract_spec("ES")
        assert spec["tick_size"] == 0.25
        assert spec["tick_value"] == 12.50
        assert spec["multiplier"] == 50
        assert spec["margin"] == 15400

    def test_get_nq_spec(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        spec = adapter.get_contract_spec("NQ")
        assert spec["multiplier"] == 20

    def test_unknown_symbol_raises(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        with pytest.raises(ValueError, match="Unknown symbol"):
            adapter.get_contract_spec("UNKNOWN")


# ─── Market Data (Paper Mode) ─────────────────────

class TestMarketDataPaper:
    def test_get_price_paper_yahoo(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        mock_hist = MagicMock()
        mock_hist.empty = False
        mock_close = MagicMock()
        mock_close.iloc.__getitem__ = MagicMock(return_value=5500.0)
        mock_hist.__getitem__ = MagicMock(return_value=mock_close)

        mock_yf = MagicMock()
        mock_ticker = MagicMock()
        mock_ticker.history.return_value = mock_hist
        mock_yf.Ticker.return_value = mock_ticker
        with patch.dict(sys.modules, {"yfinance": mock_yf}):
            price = adapter.get_price("ES")
            assert price == 5500.0

    def test_get_price_unknown_symbol(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        assert adapter.get_price("UNKNOWN") == 0.0

    def test_get_ohlcv_paper_yahoo(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        import pandas as pd
        dates = pd.date_range("2026-04-01", periods=5, freq="h")
        data = {
            "Open": [5490, 5495, 5500, 5505, 5510],
            "High": [5495, 5500, 5505, 5510, 5515],
            "Low": [5485, 5490, 5495, 5500, 5505],
            "Close": [5492, 5498, 5503, 5508, 5512],
            "Volume": [100, 200, 150, 180, 120],
        }
        hist = pd.DataFrame(data, index=dates)

        mock_yf = MagicMock()
        mock_ticker = MagicMock()
        mock_ticker.history.return_value = hist
        mock_yf.Ticker.return_value = mock_ticker
        with patch.dict(sys.modules, {"yfinance": mock_yf}):
            candles = adapter.get_ohlcv("ES", "1h", 5)
            assert len(candles) == 5
            assert candles[0][4] == 5492  # close

    def test_get_ohlcv_unknown_symbol(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        assert adapter.get_ohlcv("UNKNOWN") == []


# ─── Account Data ──────────────────────────────────

class TestAccountData:
    def test_get_open_positions_paper(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        assert adapter.get_open_positions() == []


# ─── Order Execution ──────────────────────────────

class TestOrderExecution:
    def test_market_open_paper_raises(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_open("ES", True, 1)

    def test_market_close_paper_raises(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_close("ES")


# ─── Market Hours ──────────────────────────────────

class TestMarketHours:
    def test_is_market_open_returns_bool(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        result = adapter.is_market_open()
        assert isinstance(result, bool)


# ─── Yahoo Symbol Map ─────────────────────────────

class TestYahooSymbolMap:
    def test_known_symbols(self):
        assert YAHOO_SYMBOL_MAP["ES"] == "ES=F"
        assert YAHOO_SYMBOL_MAP["NQ"] == "NQ=F"
        assert YAHOO_SYMBOL_MAP["GC"] == "GC=F"
