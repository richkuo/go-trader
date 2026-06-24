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
        import pandas as pd
        adapter = TopStepExchangeAdapter(mode="paper")
        hist = pd.DataFrame({"Close": [5400.0, 5450.0, 5500.0]})

        mock_yf = MagicMock()
        mock_ticker = MagicMock()
        mock_ticker.history.return_value = hist
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


# ─── Cash-flow journal feeds (#1106) ──────────────

def _live_adapter_with_session(session):
    """Build a live adapter without real creds by injecting a mock session.

    Bypasses __init__'s live-mode credential check / requests import so the
    #1106 balance + fills methods can be unit-tested against a mocked HTTP layer.
    """
    adapter = TopStepExchangeAdapter(mode="paper")
    adapter._mode = "live"
    adapter._account_id = "acct-1"
    adapter._session = session
    assert adapter.is_live is True
    return adapter


class TestCashflowJournalFeeds:
    def test_equity_and_upnl_derives_upnl_from_same_read(self):
        session = MagicMock()
        resp = MagicMock()
        resp.json.return_value = {"equity": 50120.0, "cashBalance": 50000.0}
        session.get.return_value = resp
        adapter = _live_adapter_with_session(session)
        equity, upnl = adapter.get_account_equity_and_upnl()
        assert equity == 50120.0
        # uPnL = equity − cashBalance from the SAME response (coherent snapshot).
        assert upnl == pytest.approx(120.0)

    def test_equity_and_upnl_missing_cash_degrades_to_zero_upnl(self):
        session = MagicMock()
        resp = MagicMock()
        resp.json.return_value = {"equity": 1000.0}  # no cashBalance/balance field
        session.get.return_value = resp
        adapter = _live_adapter_with_session(session)
        equity, upnl = adapter.get_account_equity_and_upnl()
        assert equity == 1000.0
        assert upnl == pytest.approx(0.0)

    def test_equity_and_upnl_missing_equity_raises(self):
        # #1106 review finding 1: a 200 with a shape-mismatched body (no "equity"
        # field) must RAISE — never silently coerce to a $0 equity that the
        # portfolio kill switch would consume.
        session = MagicMock()
        resp = MagicMock()
        resp.json.return_value = {"cashBalance": 50000.0}  # no equity field
        session.get.return_value = resp
        adapter = _live_adapter_with_session(session)
        with pytest.raises(ValueError, match="missing 'equity'"):
            adapter.get_account_equity_and_upnl()

    def test_equity_and_upnl_paper_raises(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.get_account_equity_and_upnl()

    def test_get_account_fills_shapes_and_sorts(self):
        session = MagicMock()
        resp = MagicMock()
        resp.json.return_value = {"fills": [
            {"id": "f2", "timestamp": 200, "symbol": "es", "kind": "TRADE", "realizedPnl": 20, "fee": 0.3},
            {"id": "f1", "timestamp": 100, "symbol": "ES", "kind": "", "realizedPnl": 0, "commission": 0.5},
        ]}
        session.get.return_value = resp
        adapter = _live_adapter_with_session(session)
        # Two fills < page_limit → short page → feed drained, single GET, not capped.
        fills, capped = adapter.get_account_fills(since_ms=0, page_limit=1000)
        assert capped is False
        assert session.get.call_count == 1
        # Oldest-first.
        assert [f["ts_ms"] for f in fills] == [100, 200]
        assert fills[0] == {"fill_id": "f1", "ts_ms": 100, "symbol": "ES", "kind": "", "realized_pnl": 0.0, "fee": 0.5}
        assert fills[1]["symbol"] == "ES" and fills[1]["kind"] == "trade" and fills[1]["realized_pnl"] == 20.0

    def test_get_account_fills_drains_across_pages(self):
        # Full first page (== page_limit) must NOT be reported capped: the loop
        # advances the cursor and pages forward until a short page drains the feed.
        page1 = {"fills": [
            {"id": "a", "timestamp": 100, "symbol": "ES", "kind": "trade", "realizedPnl": 1, "fee": 0},
            {"id": "b", "timestamp": 200, "symbol": "ES", "kind": "trade", "realizedPnl": 2, "fee": 0},
        ]}
        page2 = {"fills": [  # boundary overlap (ts 200 re-read) + one new fill
            {"id": "b", "timestamp": 200, "symbol": "ES", "kind": "trade", "realizedPnl": 2, "fee": 0},
            {"id": "c", "timestamp": 300, "symbol": "ES", "kind": "trade", "realizedPnl": 3, "fee": 0},
        ]}
        page3 = {"fills": []}  # drained
        session = MagicMock()
        r1, r2, r3 = MagicMock(), MagicMock(), MagicMock()
        r1.json.return_value, r2.json.return_value, r3.json.return_value = page1, page2, page3
        session.get.side_effect = [r1, r2, r3]
        adapter = _live_adapter_with_session(session)
        fills, capped = adapter.get_account_fills(since_ms=0, page_limit=2)
        assert capped is False  # feed drained, not page-capped
        # Overlap fill b deduped → three distinct fills, oldest-first.
        assert [f["fill_id"] for f in fills] == ["a", "b", "c"]

    def test_get_account_fills_single_ms_overflow_caps(self):
        # A FULL page whose fills all share one millisecond (timestamp paging can't
        # step past it) must fail closed (capped) rather than loop or drop fills.
        same_ms = {"fills": [
            {"id": "x", "timestamp": 500, "symbol": "ES", "kind": "trade", "realizedPnl": 1, "fee": 0},
            {"id": "y", "timestamp": 500, "symbol": "ES", "kind": "trade", "realizedPnl": 1, "fee": 0},
        ]}
        session = MagicMock()
        resp = MagicMock()
        resp.json.return_value = same_ms
        session.get.return_value = resp
        adapter = _live_adapter_with_session(session)
        _, capped = adapter.get_account_fills(since_ms=0, page_limit=2)
        assert capped is True

    def test_get_account_fills_paper_raises(self):
        adapter = TopStepExchangeAdapter(mode="paper")
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.get_account_fills()


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
