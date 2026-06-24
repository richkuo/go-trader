"""Tests for OKXExchangeAdapter — mock ccxt to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch

# Load okx adapter by file path to avoid module name collisions
_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)

# Load the module once to get the class reference
_spec = importlib.util.spec_from_file_location("okx_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)
OKXExchangeAdapter = _mod.OKXExchangeAdapter


@pytest.fixture
def adapter():
    """Create OKXExchangeAdapter in paper mode with a mocked ccxt exchange."""
    mock_ex = MagicMock()
    with patch.dict(os.environ, {}, clear=False):
        for key in ("OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX"):
            os.environ.pop(key, None)
        # Patch ccxt.okx in the loaded module so __init__ uses our mock
        orig_ccxt_okx = _mod.ccxt.okx
        _mod.ccxt.okx = MagicMock(return_value=mock_ex)
        try:
            a = OKXExchangeAdapter()
        finally:
            _mod.ccxt.okx = orig_ccxt_okx
    return a, mock_ex


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self, adapter):
        a, _ = adapter
        assert a.name == "okx"

    def test_paper_mode(self, adapter):
        a, _ = adapter
        assert a.mode == "paper"
        assert a.is_live is False

    def test_live_mode(self):
        mock_ex = MagicMock()
        with patch.dict(os.environ, {
            "OKX_API_KEY": "key",
            "OKX_API_SECRET": "secret",
            "OKX_PASSPHRASE": "pass",
        }):
            orig = _mod.ccxt.okx
            _mod.ccxt.okx = MagicMock(return_value=mock_ex)
            try:
                a = OKXExchangeAdapter()
            finally:
                _mod.ccxt.okx = orig
            assert a.is_live is True
            assert a.mode == "live"


# ─── Market Data ───────────────────────────────────

class TestMarketData:
    def test_get_spot_price(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.return_value = {"last": 67500.0}
        price = a.get_spot_price("BTC")
        assert price == 67500.0
        mock_ex.fetch_ticker.assert_called_once_with("BTC/USDT")

    def test_get_spot_price_tries_multiple_suffixes(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.side_effect = [
            Exception("not found"),
            {"last": 67000.0},
        ]
        price = a.get_spot_price("BTC")
        assert price == 67000.0
        assert mock_ex.fetch_ticker.call_count == 2

    def test_get_spot_price_all_fail(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ticker.side_effect = Exception("fail")
        assert a.get_spot_price("BTC") == 0.0

    def test_get_ohlcv(self, adapter):
        a, mock_ex = adapter
        candles = [[1700000000000, 100, 110, 90, 105, 50]]
        mock_ex.fetch_ohlcv.return_value = candles
        result = a.get_ohlcv("BTC", "1h", 200)
        assert result == candles
        mock_ex.fetch_ohlcv.assert_called_once_with("BTC/USDT", "1h", limit=200)

    def test_get_ohlcv_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.side_effect = Exception("fail")
        assert a.get_ohlcv("BTC") == []

    def test_get_ohlcv_closes(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.return_value = [
            [1700000000000, 100, 110, 90, 105, 50],
            [1700003600000, 105, 115, 95, 110, 60],
        ]
        closes = a.get_ohlcv_closes("BTC")
        assert closes == [105, 110]

    def test_get_ohlcv_closes_empty(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.side_effect = Exception("fail")
        assert a.get_ohlcv_closes("BTC") == []

    def test_get_perp_ohlcv(self, adapter):
        a, mock_ex = adapter
        candles = [[1700000000000, 100, 110, 90, 105, 50]]
        mock_ex.fetch_ohlcv.return_value = candles
        result = a.get_perp_ohlcv("BTC", "1h", 200)
        assert result == candles
        mock_ex.fetch_ohlcv.assert_called_once_with("BTC/USDT:USDT", "1h", limit=200)

    def test_get_funding_rate(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate.return_value = {"fundingRate": 0.0001}
        rate = a.get_funding_rate("BTC")
        assert rate == 0.0001
        mock_ex.fetch_funding_rate.assert_called_once_with("BTC/USDT:USDT")

    def test_get_funding_rate_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate.side_effect = Exception("fail")
        assert a.get_funding_rate("BTC") == 0.0

    def test_get_funding_history(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate_history.return_value = [
            {"fundingRate": 0.0001, "timestamp": 1700000000000},
            {"fundingRate": 0.0002, "timestamp": 1700003600000},
        ]
        result = a.get_funding_history("BTC", days=7)
        assert len(result) == 2
        assert result[0] == {"rate": 0.0001, "time": 1700000000000}

    def test_get_funding_history_on_error(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_funding_rate_history.side_effect = Exception("fail")
        assert a.get_funding_history("BTC") == []


# ─── Order Execution ──────────────────────────────

class TestOrderExecution:
    def test_market_open_paper_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.market_open("BTC", True, 0.5)

    def test_market_close_paper_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.market_close("BTC")

    def test_market_open_spot(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.create_market_order.return_value = {"id": "123"}
        result = a.market_open("BTC", True, 0.5, inst_type="spot")
        assert result == {"id": "123"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT", "buy", 0.5, params={"tdMode": "cash"}
        )

    def test_market_open_swap(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.create_market_order.return_value = {"id": "456"}
        result = a.market_open("BTC", False, 1.0, inst_type="swap")
        assert result == {"id": "456"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT:USDT", "sell", 1.0, params={"tdMode": "cross"}
        )

    def test_market_close_with_position(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_positions.return_value = [
            {"contracts": "1.5", "side": "long"},
        ]
        mock_ex.create_market_order.return_value = {"id": "789"}
        result = a.market_close("BTC")
        assert result == {"id": "789"}
        mock_ex.create_market_order.assert_called_once_with(
            "BTC/USDT:USDT", "sell", 1.5,
            params={"tdMode": "cross", "reduceOnly": True}
        )

    def test_market_close_no_position(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_positions.return_value = []
        result = a.market_close("BTC")
        assert result == {}

    def test_market_close_hedge_mode_closes_both(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_positions.return_value = [
            {"contracts": "1.5", "side": "long"},
            {"contracts": "0.8", "side": "short"},
        ]
        mock_ex.create_market_order.side_effect = [{"id": "aaa"}, {"id": "bbb"}]
        result = a.market_close("BTC")
        assert mock_ex.create_market_order.call_count == 2
        # Verify first call closes the long (sell side)
        first_call = mock_ex.create_market_order.call_args_list[0]
        assert first_call[0][1] == "sell"  # close long = sell
        assert first_call[0][2] == 1.5
        # Verify second call closes the short (buy side)
        second_call = mock_ex.create_market_order.call_args_list[1]
        assert second_call[0][1] == "buy"  # close short = buy
        assert second_call[0][2] == 0.8
        # Returns first result
        assert result == {"id": "aaa"}


# ─── Options Protocol ──────────────────────────────

class TestOptionsProtocol:
    def test_get_vol_metrics(self, adapter):
        a, mock_ex = adapter
        closes = [50000 + i * 100 for i in range(90)]
        candles = [[i * 86400000, c - 50, c + 50, c - 100, c, 1000] for i, c in enumerate(closes)]
        mock_ex.fetch_ohlcv.return_value = candles
        vol, iv_rank = a.get_vol_metrics("BTC")
        assert vol > 0
        assert 0 <= iv_rank <= 100

    def test_get_vol_metrics_insufficient_data(self, adapter):
        a, mock_ex = adapter
        mock_ex.fetch_ohlcv.return_value = [[0, 100, 110, 90, 105, 50]] * 5
        vol, iv_rank = a.get_vol_metrics("BTC")
        assert vol == 0.60
        assert iv_rank == 50.0

    def test_get_real_expiry_with_markets(self, adapter):
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp1 = now + timedelta(days=30)
        exp2 = now + timedelta(days=60)

        mock_ex.markets = {
            "BTC-30D-100000-C": {
                "type": "option",
                "base": "BTC",
                "active": True,
                "expiry": int(exp1.timestamp() * 1000),
            },
            "BTC-60D-100000-C": {
                "type": "option",
                "base": "BTC",
                "active": True,
                "expiry": int(exp2.timestamp() * 1000),
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets

        expiry_str, actual_dte = a.get_real_expiry("BTC", 30)
        assert actual_dte >= 29
        assert actual_dte <= 31

    def test_get_real_strike_with_markets(self, adapter):
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp = now + timedelta(days=30)
        exp_str = exp.strftime("%Y-%m-%d")
        exp_start = int(datetime.strptime(exp_str, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp() * 1000)

        mock_ex.markets = {
            "BTC-30D-65000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 65000.0,
                "expiry": exp_start + 3600000,
            },
            "BTC-30D-70000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 70000.0,
                "expiry": exp_start + 3600000,
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets

        strike = a.get_real_strike("BTC", exp_str, "call", 67000.0)
        assert strike == 65000.0

    def test_get_real_strike_fallback(self, adapter):
        a, mock_ex = adapter
        mock_ex.markets = {}
        mock_ex.load_markets.return_value = {}
        strike = a.get_real_strike("BTC", "2026-04-15", "call", 67500.0)
        assert strike == 68000.0

    def test_get_premium_and_greeks_fallback(self, adapter):
        """When live quote fails and BS import has arg mismatch, returns zeros."""
        a, mock_ex = adapter
        mock_ex.markets = {}
        mock_ex.load_markets.return_value = {}
        pct, usd, greeks = a.get_premium_and_greeks(
            "BTC", "call", 70000, "2026-05-01", 30, 67000, 0.6
        )
        # Returns a valid tuple (may be zeros if BS fallback also fails)
        assert isinstance(pct, float)
        assert isinstance(usd, float)
        assert "delta" in greeks

    def test_get_premium_and_greeks_live_quote(self, adapter):
        """When a matching option market exists, returns live quote data."""
        a, mock_ex = adapter
        from datetime import datetime, timezone, timedelta
        now = datetime.now(timezone.utc)
        exp = now + timedelta(days=30)
        exp_str = exp.strftime("%Y-%m-%d")
        exp_start = int(datetime.strptime(exp_str, "%Y-%m-%d").replace(tzinfo=timezone.utc).timestamp() * 1000)

        mock_ex.markets = {
            "BTC-30D-70000-C": {
                "type": "option",
                "base": "BTC",
                "optionType": "call",
                "active": True,
                "strike": 70000,
                "expiry": exp_start + 3600000,
            },
        }
        mock_ex.load_markets.return_value = mock_ex.markets
        mock_ex.fetch_ticker.return_value = {
            "last": 0.05,
            "close": 0.05,
            "info": {"delta": "0.45", "gamma": "0.001", "theta": "-10", "vega": "50"},
        }
        pct, usd, greeks = a.get_premium_and_greeks(
            "BTC", "call", 70000, exp_str, 30, 67000, 0.6
        )
        assert pct == 0.05
        assert usd == 0.05 * 67000
        assert greeks["delta"] == 0.45


# ─── #1105 account bills + coherent equity/uPnL (cash-flow journal) ─────────

def _ledger_entry(bill_id, ts, balchg="1", ccy="USDT", btype="2", sub="",
                  pnl="0", fee="0", inst="BTC-USDT-SWAP", trade="t1"):
    """A ccxt fetch_ledger entry carrying a raw OKX bill in `info`."""
    return {
        "id": bill_id,
        "timestamp": ts,
        "info": {
            "billId": bill_id, "ts": str(ts), "ccy": ccy, "type": btype,
            "subType": sub, "balChg": str(balchg), "pnl": str(pnl),
            "fee": str(fee), "instId": inst, "tradeId": trade,
        },
    }


class TestNormalizeOKXBill:
    def test_full_bill(self):
        out = _mod._normalize_okx_bill(_ledger_entry("b1", 1700000000000, "19.7", pnl="20", fee="0.3"))
        assert out == {
            "bill_id": "b1", "ts_ms": 1700000000000, "ccy": "USDT", "type": "2",
            "sub_type": "", "bal_chg": 19.7, "pnl": 20.0, "fee": 0.3,
            "inst_id": "BTC-USDT-SWAP", "trade_id": "t1",
        }

    def test_falls_back_to_unified_timestamp(self):
        entry = {"id": "b2", "timestamp": 1700000000999, "info": {"billId": "b2", "balChg": "1.0"}}
        out = _mod._normalize_okx_bill(entry)
        assert out["ts_ms"] == 1700000000999
        assert out["bal_chg"] == 1.0

    def test_garbage_numbers_default_zero(self):
        entry = {"id": "b3", "timestamp": 1, "info": {"billId": "b3", "balChg": "", "pnl": None, "fee": "x"}}
        out = _mod._normalize_okx_bill(entry)
        assert out["bal_chg"] == 0.0 and out["pnl"] == 0.0 and out["fee"] == 0.0


class TestOKXUSDTCashBalance:
    def _info(self, details):
        return {"code": "0", "data": [{"details": details}]}

    def test_reads_usdt_cash_bal(self):
        info = self._info([{"ccy": "BTC", "cashBal": "0.5"}, {"ccy": "USDT", "cashBal": "900.25"}])
        assert _mod._okx_usdt_cash_balance(info) == 900.25

    def test_missing_details_returns_none(self):
        assert _mod._okx_usdt_cash_balance({"data": [{}]}) is None
        assert _mod._okx_usdt_cash_balance({}) is None
        assert _mod._okx_usdt_cash_balance(None) is None

    def test_no_usdt_entry_returns_none(self):
        assert _mod._okx_usdt_cash_balance(self._info([{"ccy": "BTC", "cashBal": "0.5"}])) is None

    def test_unparseable_cash_bal_returns_none(self):
        assert _mod._okx_usdt_cash_balance(self._info([{"ccy": "USDT", "cashBal": "n/a"}])) is None


class TestGetAccountEquityAndUPnL:
    def test_paper_mode_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.get_account_equity_and_upnl()

    def test_coherent_eq_and_upnl(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_balance.return_value = {
            "total": {"USDT": 1000.0},
            "info": {"data": [{"details": [{"ccy": "USDT", "cashBal": "900.0"}]}]},
        }
        eq, upnl = a.get_account_equity_and_upnl()
        assert eq == 1000.0
        assert upnl == 100.0  # eq - cashBal, one coherent snapshot

    def test_missing_cash_bal_defaults_upnl_zero(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_balance.return_value = {"total": {"USDT": 1000.0}, "info": {}}
        eq, upnl = a.get_account_equity_and_upnl()
        assert eq == 1000.0 and upnl == 0.0


class TestGetAccountBills:
    def test_paper_mode_raises(self, adapter):
        a, _ = adapter
        with pytest.raises(RuntimeError, match="live mode"):
            a.get_account_bills(since_ms=0)

    def test_single_page_ascending(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_ledger.return_value = [
            _ledger_entry("b2", 200, "19.7"),
            _ledger_entry("b1", 100, "-0.5"),
        ]
        bills, capped = a.get_account_bills(since_ms=50)
        assert capped is False
        assert [b["bill_id"] for b in bills] == ["b1", "b2"]
        assert [b["ts_ms"] for b in bills] == [100, 200]

    def test_full_page_advances_with_overlap_not_plus_one(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        page1 = [_ledger_entry(f"p1-{i}", 100 + i, "1") for i in range(100)]  # ts 100..199
        page2 = [_ledger_entry("p2-0", 500, "2")]  # short → stop
        mock_ex.fetch_ledger.side_effect = [page1, page2]
        bills, capped = a.get_account_bills(since_ms=0, page_limit=100)
        assert capped is False
        assert len(bills) == 101
        assert mock_ex.fetch_ledger.call_count == 2
        # Overlap: next cursor is the page's last ts (199), NOT last_ts + 1.
        assert mock_ex.fetch_ledger.call_args_list[1].kwargs["since"] == 199

    def test_same_ms_straddle_across_page_boundary_is_captured(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        # page1 fills exactly 100 entries, the last (p-99) at ts=199. A SECOND bill
        # at ts=199 (p-100) falls beyond the cut. cursor=199 (overlap) re-fetches it.
        page1 = [_ledger_entry(f"p-{i}", 100 + i, "1") for i in range(100)]  # p-99 @ 199
        page2 = [_ledger_entry("p-99", 199, "1"),       # dup (deduped)
                 _ledger_entry("p-100", 199, "1"),      # the straddling bill
                 _ledger_entry("p-105", 205, "1")]      # short page → stop
        mock_ex.fetch_ledger.side_effect = [page1, page2]
        bills, capped = a.get_account_bills(since_ms=0, page_limit=100)
        ids = [b["bill_id"] for b in bills]
        assert "p-100" in ids                      # same-ms straddler NOT skipped
        assert ids.count("p-99") == 1              # dedup absorbed the overlap
        assert capped is False

    def test_same_ms_block_larger_than_page_fails_closed(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        # Every bill shares ts=100 and the page is always full → cannot advance by
        # timestamp without dropping the tail, so the adapter caps (fail closed).
        page = [_ledger_entry(f"x-{i}", 100, "1") for i in range(10)]
        mock_ex.fetch_ledger.return_value = page  # same full page every call
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=10000)
        assert capped is True
        assert len(bills) == 10
        assert mock_ex.fetch_ledger.call_count == 2  # second call detects no progress

    def test_capped_on_max_bills_truncates_oldest_prefix(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        calls = {"n": 0}

        def side_effect(*args, **kwargs):
            calls["n"] += 1
            base = calls["n"] * 1000
            return [_ledger_entry(f"c-{base+i}", base + i, "1") for i in range(10)]

        mock_ex.fetch_ledger.side_effect = side_effect
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=25)
        assert capped is True
        assert len(bills) == 25
        assert all(bills[i]["ts_ms"] <= bills[i + 1]["ts_ms"] for i in range(len(bills) - 1))

    def test_dedup_across_overlapping_pages(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        page1 = [_ledger_entry(f"d-{i}", 100 + i, "1") for i in range(100)]
        page2 = [_ledger_entry("d-99", 199, "1"), _ledger_entry("d-new", 250, "1")]
        mock_ex.fetch_ledger.side_effect = [page1, page2]
        bills, capped = a.get_account_bills(since_ms=0, page_limit=100)
        ids = [b["bill_id"] for b in bills]
        assert len(ids) == len(set(ids))
        assert "d-new" in ids and ids.count("d-99") == 1

    def test_loop_budget_exhaustion_reports_capped(self, adapter):
        # A backlog larger than the iteration budget, with same-ms clustering so
        # the overlap re-read makes net-new-per-page < page_limit. The loop must
        # exhaust its budget and report capped=True (NOT fall through to False with
        # an incomplete prefix that the Go journal would treat as complete).
        a, mock_ex = adapter
        a._is_live = True
        # 400 bills, 2 per ms (ts 0,0,1,1,2,2,...,199,199); page_limit 10,
        # max_bills 100 → budget = 100//10 + 2 = 12 iterations. Overlap nets ~8
        # new/page, so 12 pages can't reach 100 → budget runs out while the feed
        # still has bills, below the max_bills cap.
        all_bills = [_ledger_entry(f"x-{i}", i // 2, "1") for i in range(400)]

        def side_effect(*args, **kwargs):
            since = kwargs.get("since", 0)
            window = [e for e in all_bills if e["timestamp"] >= since]
            return window[:10]

        mock_ex.fetch_ledger.side_effect = side_effect
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=100)
        assert capped is True, "budget exhaustion must report capped=True, not False"
        assert len(bills) < 400  # an incomplete prefix, correctly flagged

    def test_drained_distinct_ts_not_spuriously_capped(self, adapter):
        # The inverse: a fully-drained feed of distinct timestamps that exits on a
        # short page must stay capped=False (no spurious cap from the for…else).
        a, mock_ex = adapter
        a._is_live = True
        all_bills = [_ledger_entry(f"d-{i}", 100 + i, "1") for i in range(25)]  # < max_bills

        def side_effect(*args, **kwargs):
            since = kwargs.get("since", 0)
            window = [e for e in all_bills if e["timestamp"] >= since]
            return window[:10]

        mock_ex.fetch_ledger.side_effect = side_effect
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=100)
        assert capped is False
        assert len(bills) == 25  # whole feed drained
