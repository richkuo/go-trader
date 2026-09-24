import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch

_adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
_shared_tools = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '..', 'shared_tools'))
if _shared_tools not in sys.path:
    sys.path.insert(0, _shared_tools)

_spec = importlib.util.spec_from_file_location("okx_adapter", _adapter_path)
_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_mod)
OKXExchangeAdapter = _mod.OKXExchangeAdapter


@pytest.fixture
def adapter():
    mock_ex = MagicMock()
    with patch.dict(os.environ, {}, clear=False):
        for key in ("OKX_API_KEY", "OKX_API_SECRET", "OKX_PASSPHRASE", "OKX_SANDBOX"):
            os.environ.pop(key, None)
        orig_ccxt_okx = _mod.ccxt.okx
        _mod.ccxt.okx = MagicMock(return_value=mock_ex)
        try:
            a = OKXExchangeAdapter()
        finally:
            _mod.ccxt.okx = orig_ccxt_okx
    return a, mock_ex


class TestOrderExecution:

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
        first_call = mock_ex.create_market_order.call_args_list[0]
        assert first_call[0][1] == "sell"
        assert first_call[0][2] == 1.5
        second_call = mock_ex.create_market_order.call_args_list[1]
        assert second_call[0][1] == "buy"
        assert second_call[0][2] == 0.8
        assert result == {"id": "aaa"}


def _ledger_entry(bill_id, ts, balchg="1", ccy="USDT", btype="2", sub="",
                  pnl="0", fee="0", inst="BTC-USDT-SWAP", trade="t1"):
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


class TestOKXUSDTCashBalance:

    @pytest.mark.parametrize("info", [
        {"data": [{}]},
        {},
        None,
        {"code": "0", "data": [{"details": [{"ccy": "BTC", "cashBal": "0.5"}]}]},
        {"code": "0", "data": [{"details": [{"ccy": "USDT", "cashBal": "n/a"}]}]},
    ])
    def test_unreadable_cash_bal_returns_none(self, info):
        assert _mod._okx_usdt_cash_balance(info) is None


class TestGetAccountEquityAndUPnL:

    def test_coherent_eq_and_upnl(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        mock_ex.fetch_balance.return_value = {
            "total": {"USDT": 1000.0},
            "info": {"data": [{"details": [{"ccy": "USDT", "cashBal": "900.0"}]}]},
        }
        eq, upnl = a.get_account_equity_and_upnl()
        assert eq == 1000.0
        assert upnl == 100.0


class TestGetAccountBills:

    def test_same_ms_straddle_across_page_boundary_is_captured(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        page1 = [_ledger_entry(f"p-{i}", 100 + i, "1") for i in range(100)]
        page2 = [_ledger_entry("p-99", 199, "1"),
                 _ledger_entry("p-100", 199, "1"),
                 _ledger_entry("p-105", 205, "1")]
        mock_ex.fetch_ledger.side_effect = [page1, page2]
        bills, capped = a.get_account_bills(since_ms=0, page_limit=100)
        ids = [b["bill_id"] for b in bills]
        assert "p-100" in ids
        assert ids.count("p-99") == 1
        assert capped is False

    def test_same_ms_block_larger_than_page_fails_closed(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        page = [_ledger_entry(f"x-{i}", 100, "1") for i in range(10)]
        mock_ex.fetch_ledger.return_value = page
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=10000)
        assert capped is True
        assert len(bills) == 10
        assert mock_ex.fetch_ledger.call_count == 2

    def test_loop_budget_exhaustion_reports_capped(self, adapter):
        a, mock_ex = adapter
        a._is_live = True
        all_bills = [_ledger_entry(f"x-{i}", i // 2, "1") for i in range(400)]

        def side_effect(*args, **kwargs):
            since = kwargs.get("since", 0)
            window = [e for e in all_bills if e["timestamp"] >= since]
            return window[:10]

        mock_ex.fetch_ledger.side_effect = side_effect
        bills, capped = a.get_account_bills(since_ms=0, page_limit=10, max_bills=100)
        assert capped is True, "budget exhaustion must report capped=True, not False"
        assert len(bills) < 400
