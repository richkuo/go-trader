"""Tests for HyperliquidExchangeAdapter — mock SDK to avoid live API calls."""

import sys
import os
import importlib.util
import pytest
from unittest.mock import MagicMock, patch


def _load_hl_adapter(mock_info_cls=None, mock_exchange_cls=None, mock_api_cls=None):
    """Load the hyperliquid adapter with mocked SDK modules.

    Mocks hyperliquid.info.Info, hyperliquid.exchange.Exchange,
    hyperliquid.api.API, and hyperliquid.utils.error.ClientError before
    loading adapter.py so it picks up _SDK_AVAILABLE = True. ClientError
    must be a real Exception subclass so the adapter's except clause is
    a valid exception type.
    """
    info_mod = MagicMock()
    exchange_mod = MagicMock()
    api_mod = MagicMock()
    utils_pkg = MagicMock()
    error_mod = MagicMock()
    hl_pkg = MagicMock()

    info_mod.Info = mock_info_cls or MagicMock()
    exchange_mod.Exchange = mock_exchange_cls or MagicMock()
    api_mod.API = mock_api_cls or MagicMock()

    class _StubClientError(Exception):
        def __init__(self, status_code=None, *a, **kw):
            super().__init__(*a, **kw)
            self.status_code = status_code
    error_mod.ClientError = _StubClientError

    saved = {}
    mod_names = (
        "hyperliquid",
        "hyperliquid.info",
        "hyperliquid.exchange",
        "hyperliquid.api",
        "hyperliquid.utils",
        "hyperliquid.utils.error",
    )
    for name in mod_names:
        saved[name] = sys.modules.get(name)

    sys.modules["hyperliquid"] = hl_pkg
    sys.modules["hyperliquid.info"] = info_mod
    sys.modules["hyperliquid.exchange"] = exchange_mod
    sys.modules["hyperliquid.api"] = api_mod
    sys.modules["hyperliquid.utils"] = utils_pkg
    sys.modules["hyperliquid.utils.error"] = error_mod

    try:
        adapter_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "adapter.py")
        spec = importlib.util.spec_from_file_location("hl_adapter", adapter_path)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
    finally:
        # Restore original modules even if loading fails
        for name, orig in saved.items():
            if orig is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = orig

    return mod


# ─── Properties ────────────────────────────────────

class TestProperties:
    def test_name(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        assert adapter.name == "hyperliquid"

    def test_paper_mode_when_no_secret(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("HYPERLIQUID_SECRET_KEY", None)
            mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
            adapter = mod.HyperliquidExchangeAdapter()
            assert adapter.mode == "paper"
            assert adapter.is_live is False

    def test_sdk_not_available_raises(self):
        mock_info_cls = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        mod._SDK_AVAILABLE = False
        with pytest.raises(ImportError, match="hyperliquid-python-sdk"):
            mod.HyperliquidExchangeAdapter()


# ─── Market Data ───────────────────────────────────

class TestMarketData:
    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        return adapter, mock_info

    def test_get_spot_price_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {"BTC": "67500.50"}
        assert adapter.get_spot_price("BTC") == 67500.50

    def test_get_spot_price_fallback_perp_suffix(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {"BTC-PERP": "67000.00"}
        assert adapter.get_spot_price("BTC") == 67000.00

    def test_get_spot_price_not_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.all_mids.return_value = {}
        assert adapter.get_spot_price("XYZ") == 0.0

    def test_get_ohlcv(self, monkeypatch):
        # Disable the #839 OHLCV cache so this exercises the live fetch path
        # deterministically (no /tmp cross-run state).
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
            {"T": 1700003600000, "o": "105", "h": "115", "l": "95", "c": "110", "v": "60"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 2)
        assert len(result) == 2
        assert result[0] == [1700000000000, 100.0, 110.0, 90.0, 105.0, 50.0]

    def test_get_ohlcv_uses_t_key_fallback(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"t": 1700000000000, "o": "100", "h": "110", "l": "90", "c": "105", "v": "50"},
        ]
        result = adapter.get_ohlcv("BTC", "1h", 1)
        assert result[0][0] == 1700000000000

    def test_get_ohlcv_widens_window_for_gap_margin(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = []
        adapter.get_ohlcv("BTC", "1h", 200)
        _name, interval, start_ms, end_ms = mock_info.candles_snapshot.call_args[0]
        assert interval == "1h"
        assert end_ms - start_ms == 3_600_000 * (200 + 50)

    def test_get_ohlcv_trims_to_limit_when_api_returns_extra(self, monkeypatch):
        monkeypatch.setenv("GO_TRADER_HL_OHLCV_CACHE", "0")
        adapter, mock_info = self._make_adapter()
        mock_info.candles_snapshot.return_value = [
            {"T": 1700000000000 + i * 3_600_000, "o": "100", "h": "110", "l": "90",
             "c": str(100 + i), "v": "50"}
            for i in range(5)
        ]
        result = adapter.get_ohlcv("BTC", "1h", 3)
        assert len(result) == 3
        assert result[0][4] == 102.0
        assert result[-1][4] == 104.0

    def test_get_funding_rate_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.return_value = [
            {"universe": [{"name": "BTC"}, {"name": "ETH"}]},
            [{"funding": "0.0001"}, {"funding": "0.0002"}],
        ]
        rate = adapter.get_funding_rate("BTC")
        assert rate == 0.0001

    def test_get_funding_rate_not_found(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.return_value = [
            {"universe": [{"name": "ETH"}]},
            [{"funding": "0.0002"}],
        ]
        assert adapter.get_funding_rate("BTC") == 0.0

    def test_get_funding_rate_on_error(self):
        adapter, mock_info = self._make_adapter()
        mock_info.meta_and_asset_ctxs.side_effect = Exception("API down")
        assert adapter.get_funding_rate("BTC") == 0.0

    def test_get_funding_history(self):
        adapter, mock_info = self._make_adapter()
        mock_info.funding_history.return_value = [
            {"fundingRate": "0.0001", "time": 1700000000000},
            {"fundingRate": "0.0002", "time": 1700003600000},
        ]
        result = adapter.get_funding_history("BTC", days=7)
        assert len(result) == 2
        assert result[0] == {"rate": 0.0001, "time": 1700000000000}

    def test_get_funding_history_on_error(self):
        adapter, mock_info = self._make_adapter()
        mock_info.funding_history.side_effect = Exception("fail")
        assert adapter.get_funding_history("BTC") == []


# ─── Account Data ──────────────────────────────────

class TestAccountData:
    def _make_adapter_with_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_get_open_positions(self):
        adapter, mock_info = self._make_adapter_with_address()
        mock_info.user_state.return_value = {
            "assetPositions": [
                {"position": {"coin": "BTC", "szi": "0.5", "entryPx": "67000", "unrealizedPnl": "100.50"}},
                {"position": {"coin": "ETH", "szi": "0", "entryPx": "3500", "unrealizedPnl": "0"}},
            ]
        }
        positions = adapter.get_open_positions()
        assert len(positions) == 1
        assert positions[0]["coin"] == "BTC"
        assert positions[0]["size"] == 0.5
        assert positions[0]["entry_price"] == 67000.0

    def test_get_open_positions_no_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.get_open_positions() == []

    def test_get_open_positions_on_error(self):
        adapter, mock_info = self._make_adapter_with_address()
        mock_info.user_state.side_effect = Exception("fail")
        assert adapter.get_open_positions() == []


# ─── Order Execution ──────────────────────────────

class TestOrderExecution:
    def test_market_open_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        assert adapter._exchange is None
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_open("BTC", True, 0.5)

    def test_market_close_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.market_close("BTC")

    def test_market_open_live_mode(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_open.return_value = {"status": "ok"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        # Simulate live mode
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        result = adapter.market_open("BTC", True, 0.5)
        assert result == {"status": "ok"}
        mock_exchange.market_open.assert_called_once_with("BTC", True, 0.5, None, 0.01)

    def test_market_open_size_rounded_to_zero_raises(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 0}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.market_open("BTC", True, 0.4)

    def test_market_close_live_mode(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange

        result = adapter.market_close("BTC")
        assert result == {"status": "closed"}
        mock_exchange.market_close.assert_called_once_with("BTC", None)

    def test_market_close_partial_size_rounds_to_sz_decimals(self):
        # Issue #425: unrounded sz (e.g. 0.250965 from per-strategy CB sizing
        # of a shared-wallet position) must be rounded to the asset's
        # sz_decimals before reaching the SDK or HL rejects with
        # `float_to_wire causes rounding`. Mirrors market_open / place_stop_loss.
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_close("ETH", 0.2509645272613055)
        mock_exchange.market_close.assert_called_once_with("ETH", 0.251)

    def test_market_close_full_close_passes_none_unchanged(self):
        # sz=None must bypass rounding (SDK closes the full on-chain position
        # internally). A naive round(None, ...) would raise TypeError.
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 5}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.market_close.return_value = {"status": "closed"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        adapter.market_close("BTC")
        mock_exchange.market_close.assert_called_once_with("BTC", None)

    def test_market_close_partial_size_rounded_to_zero_raises(self):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = {"BTC": 0}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info

        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.market_close("BTC", 0.4)
        mock_exchange.market_close.assert_not_called()


# ─── Stop Loss / Trigger Orders (#412 / #421) ──────

class TestStopLossPlacement:
    """Coverage for place_stop_loss + cancel_trigger_order added in #412 and
    refined for tick-size rules in #421 review point 5."""

    def _live_adapter(self, sz_decimals=None):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = sz_decimals or {"BTC": 5, "ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        mock_exchange = MagicMock()
        adapter._exchange = mock_exchange
        adapter._info = mock_info
        return adapter, mock_exchange, mod

    def test_place_stop_loss_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.place_stop_loss("BTC", 0.01, 60000, False)

    def test_place_stop_loss_long_uses_sell_with_lower_limit(self):
        adapter, ex, _ = self._live_adapter()
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.5, 3000.0, is_buy=False, limit_slippage_pct=5.0)
        args, kwargs = ex.order.call_args
        sym, is_buy, sz, limit_px, order_type = args
        assert sym == "ETH"
        assert is_buy is False
        # limit_px is below trigger_px for a sell-stop
        assert limit_px < 3000.0
        assert kwargs == {"reduce_only": True}
        assert order_type["trigger"]["tpsl"] == "sl"
        assert order_type["trigger"]["isMarket"] is True

    def test_place_stop_loss_short_uses_buy_with_higher_limit(self):
        adapter, ex, _ = self._live_adapter()
        ex.order.return_value = {"status": "ok"}
        adapter.place_stop_loss("ETH", 0.5, 3000.0, is_buy=True, limit_slippage_pct=5.0)
        _, _, _, limit_px, _ = ex.order.call_args.args
        # limit_px is above trigger_px for a buy-stop
        assert limit_px > 3000.0

    def test_place_stop_loss_size_rounds_to_zero_raises(self):
        adapter, _, _ = self._live_adapter(sz_decimals={"BTC": 0})
        with pytest.raises(ValueError, match="rounded to zero"):
            adapter.place_stop_loss("BTC", 0.4, 60000, False)

    def test_place_stop_loss_invalid_trigger_px_raises(self):
        adapter, _, _ = self._live_adapter()
        with pytest.raises(ValueError, match="trigger_px must be > 0"):
            adapter.place_stop_loss("BTC", 0.01, 0, False)

    def test_place_stop_loss_high_priced_asset_uses_per_asset_px_decimals(self):
        # BTC has sz_decimals=5 → px_decimals = 6 - 5 = 1. Fixed-6-decimal
        # rounding (the pre-#421 behavior) would produce e.g. 60000.000000;
        # the new logic must round to ≤1 decimal, capped at 5 sig figs.
        adapter, ex, mod = self._live_adapter(sz_decimals={"BTC": 5})
        ex.order.return_value = {"status": "ok"}
        # Use a trigger_px that would produce extra decimals after the
        # slip-band multiplication. trigger_px = 63123.456 → after rounding
        # to px_decimals=1 we expect 63123.5 (but capped at 5 sig figs to 63120).
        adapter.place_stop_loss("BTC", 0.001, 63123.456, is_buy=False)
        _, _, _, limit_px, order_type = ex.order.call_args.args
        # 5-sig-fig rounding for ~63000 → tens place (63120 or 63130).
        assert limit_px == round(limit_px, 0) or limit_px == round(limit_px, -1)
        assert order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], 0) or \
               order_type["trigger"]["triggerPx"] == round(order_type["trigger"]["triggerPx"], -1)

    def test_round_perps_px_high_price(self):
        # Direct unit test of the helper. BTC at $63500 with sz_decimals=5
        # → px_decimals=1, but 5 sig fig cap takes priority and rounds to
        # tens place.
        from importlib import reload  # noqa: F401
        mod = _load_hl_adapter(mock_info_cls=MagicMock(return_value=MagicMock()))
        # 63123.456 with 5 sig figs → 63123 (decimals=0)
        assert mod._round_perps_px(63123.456, sz_decimals=5) == 63123
        # 0.123456 with 5 sig figs and sz_decimals=2 → px_decimals=4, sig_decimals=5
        # → use min(4, 5) = 4 decimals → 0.1235
        assert mod._round_perps_px(0.123456, sz_decimals=2) == 0.1235
        # Edge case: zero / negative passes through unchanged.
        assert mod._round_perps_px(0, sz_decimals=5) == 0
        assert mod._round_perps_px(-1.5, sz_decimals=5) == -1.5

    def test_round_perps_trigger_px_matches_internal_helper(self):
        # Public wrapper must produce the same value place_stop_loss would
        # submit, so callers can record the post-rounding price for PnL.
        adapter, _, mod = self._live_adapter(sz_decimals={"BTC": 5})
        assert adapter.round_perps_trigger_px("BTC", 63123.456) == mod._round_perps_px(63123.456, 5)
        # Idempotent — rounding a rounded value returns the same value.
        rounded = adapter.round_perps_trigger_px("BTC", 63123.456)
        assert adapter.round_perps_trigger_px("BTC", rounded) == rounded

    def test_cancel_trigger_order_paper_mode_raises(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        with pytest.raises(RuntimeError, match="live mode"):
            adapter.cancel_trigger_order("BTC", 12345)

    def test_cancel_trigger_order_passes_int_oid(self):
        adapter, ex, _ = self._live_adapter()
        ex.cancel.return_value = {"status": "ok"}
        adapter.cancel_trigger_order("BTC", "12345")
        ex.cancel.assert_called_once_with("BTC", 12345)

    def test_place_take_profit_limit_uses_reduce_only_limit(self):
        adapter, ex, _ = self._live_adapter(sz_decimals={"ETH": 4})
        ex.order.return_value = {"status": "ok"}
        adapter.place_take_profit_limit("ETH", 0.123456, 3100.0, is_buy=False)
        sym, is_buy, sz, limit_px, order_type = ex.order.call_args.args
        assert sym == "ETH"
        assert is_buy is False
        assert sz == 0.1234
        assert limit_px == 3100.0
        assert order_type == {"limit": {"tif": "Gtc"}}
        assert ex.order.call_args.kwargs == {"reduce_only": True}

    def test_open_order_oids_filters_by_symbol(self):
        adapter, _, _ = self._live_adapter()
        adapter._account_address = "0xabc"
        adapter._info.open_orders.return_value = [
            {"coin": "ETH", "oid": 111},
            {"coin": "BTC", "oid": 222},
            {"coin": "ETH", "oid": "333"},
        ]
        assert adapter.open_order_oids("ETH") == {111, 333}


# ─── userFills Lookup (#585) ──────────────────────────

class TestLookupFillFeeByOID:
    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_returns_empty_when_no_address(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.lookup_fill_fee_by_oid(123, since_ms=0) == {}
        mock_info.user_fills_by_time.assert_not_called()

    def test_aggregates_fee_and_pnl_across_partial_fills(self):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [
            {"oid": 100, "fee": "0.50", "closedPnl": "1.25"},
            {"oid": 100, "fee": "0.30", "closedPnl": "0.75"},
            {"oid": 999, "fee": "5.00", "closedPnl": "10.00"},  # different OID — ignored
        ]
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000)
        assert result["fee"] == pytest.approx(0.80)
        assert result["closed_pnl"] == pytest.approx(2.00)
        assert result["count"] == 2

    def test_handles_string_oid_in_response(self):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [
            {"oid": "100", "fee": "0.42", "closedPnl": "0"},
        ]
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000)
        assert result["fee"] == pytest.approx(0.42)
        assert result["count"] == 1

    def test_retries_until_indexer_catches_up(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        # First two attempts: fill not yet indexed. Third: appears.
        mock_info.user_fills_by_time.side_effect = [
            [],
            [{"oid": 999, "fee": "1", "closedPnl": "0"}],  # different OID
            [{"oid": 100, "fee": "0.65", "closedPnl": "0"}],
        ]
        sleeps = []
        monkeypatch.setattr("time.sleep", lambda s: sleeps.append(s))
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=4, retry_delay_s=0.1)
        assert result["fee"] == pytest.approx(0.65)
        assert mock_info.user_fills_by_time.call_count == 3
        assert sleeps == [0.1, 0.1]  # slept between attempts 1→2 and 2→3, not after the success

    def test_returns_empty_after_max_retries_exhausted(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = []
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=3, retry_delay_s=0.0)
        assert result == {}
        assert mock_info.user_fills_by_time.call_count == 3

    def test_swallows_sdk_exceptions_and_retries(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.side_effect = [
            Exception("network blip"),
            [{"oid": 100, "fee": "0.10", "closedPnl": "0"}],
        ]
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=4, retry_delay_s=0.0)
        assert result["fee"] == pytest.approx(0.10)

    def test_treats_non_list_response_as_no_match(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = {"unexpected": "shape"}
        monkeypatch.setattr("time.sleep", lambda s: None)
        result = adapter.lookup_fill_fee_by_oid(100, since_ms=1000, max_retries=2, retry_delay_s=0.0)
        assert result == {}


class TestLimitOpen:
    """Coverage for limit_open — the net-new non-reduce-only entry limit order (#883)."""

    def _make_adapter(self, sz_decimals=None):
        mock_info = MagicMock()
        mock_info.asset_to_sz_decimals = sz_decimals or {"BTC": 4, "ETH": 4}
        mock_info_cls = MagicMock(return_value=mock_info)
        mock_exchange = MagicMock()
        mock_exchange.order.return_value = {"status": "ok"}
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = mock_exchange
        adapter._info = mock_info
        return adapter, mock_exchange

    def test_paper_mode_raises(self):
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._exchange = None
        with pytest.raises(RuntimeError, match="limit_open requires live mode"):
            adapter.limit_open("BTC", True, 0.01, 58000)

    def test_alo_post_only_non_reduce_only(self):
        adapter, mock_exchange = self._make_adapter()
        adapter.limit_open("ETH", True, 0.5, 3000.0)
        # Default tif is Alo (post-only), and the entry order is NOT reduce-only —
        # the distinguishing property vs every other order this adapter places.
        args, kwargs = mock_exchange.order.call_args
        assert args[0] == "ETH"
        assert args[1] is True  # is_buy
        assert args[2] == 0.5   # size
        assert args[4] == {"limit": {"tif": "Alo"}}
        assert kwargs.get("reduce_only") is False

    def test_gtc_tif_passthrough(self):
        adapter, mock_exchange = self._make_adapter()
        adapter.limit_open("ETH", False, 0.5, 3000.0, tif="Gtc")
        _, _, _, _, order_type = mock_exchange.order.call_args[0]
        assert order_type == {"limit": {"tif": "Gtc"}}

    def test_invalid_limit_px_raises(self):
        adapter, _ = self._make_adapter()
        with pytest.raises(ValueError, match="limit_px must be > 0"):
            adapter.limit_open("ETH", True, 0.5, 0)

    def test_bad_tif_raises(self):
        adapter, _ = self._make_adapter()
        with pytest.raises(ValueError, match="unsupported tif"):
            adapter.limit_open("ETH", True, 0.5, 3000.0, tif="Fok")

    def test_size_rounded_to_zero_raises(self):
        adapter, _ = self._make_adapter(sz_decimals={"BTC": 0})
        with pytest.raises(ValueError, match="Size rounded to zero"):
            adapter.limit_open("BTC", True, 0.4, 58000)


class TestFillsSummaryByOID:
    """Coverage for fills_summary_by_oid — cumulative size + VWAP for resting
    limit-order fill tracking (#883)."""

    def _make_adapter(self):
        mock_info = MagicMock()
        mock_info_cls = MagicMock(return_value=mock_info)
        mod = _load_hl_adapter(mock_info_cls=mock_info_cls)
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = "0xABC123"
        return adapter, mock_info

    def test_no_address_returns_empty(self):
        mod = _load_hl_adapter()
        adapter = mod.HyperliquidExchangeAdapter()
        adapter._account_address = ""
        assert adapter.fills_summary_by_oid(100, since_ms=0) == {}

    def test_sums_size_and_size_weighted_vwap(self):
        adapter, mock_info = self._make_adapter()
        # Two partial legs of OID 100: 0.4@2000 and 0.6@2010, plus an unrelated OID.
        mock_info.user_fills_by_time.return_value = [
            {"oid": 100, "sz": "0.4", "px": "2000", "fee": "0.20"},
            {"oid": 100, "sz": "0.6", "px": "2010", "fee": "0.30"},
            {"oid": 999, "sz": "5", "px": "1", "fee": "9"},
        ]
        out = adapter.fills_summary_by_oid(100, since_ms=1000)
        assert out["filled_size"] == pytest.approx(1.0)
        assert out["fee"] == pytest.approx(0.50)
        assert out["count"] == 2
        # VWAP = (0.4*2000 + 0.6*2010) / 1.0 = 2006.0
        assert out["avg_px"] == pytest.approx(2006.0)

    def test_no_match_returns_empty(self, monkeypatch):
        adapter, mock_info = self._make_adapter()
        mock_info.user_fills_by_time.return_value = [{"oid": 7, "sz": "1", "px": "1", "fee": "0"}]
        monkeypatch.setattr("time.sleep", lambda s: None)
        assert adapter.fills_summary_by_oid(100, since_ms=1000, max_retries=2, retry_delay_s=0.0) == {}

    def test_zero_oid_returns_empty(self):
        adapter, mock_info = self._make_adapter()
        assert adapter.fills_summary_by_oid(0, since_ms=1000) == {}
        mock_info.user_fills_by_time.assert_not_called()
