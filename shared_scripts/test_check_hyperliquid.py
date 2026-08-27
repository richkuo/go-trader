import sys
import os
import json
import math
import importlib.util
from unittest.mock import MagicMock, patch
from io import StringIO
import pytest
_UNSET = object()

def _load_check_module():
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'check_hyperliquid.py')
    spec = importlib.util.spec_from_file_location('check_hyperliquid', script_path)
    mod = importlib.util.module_from_spec(spec)
    return (mod, spec)

class TestFillExtraction:

    def _run_execute_with_mock_response(self, sdk_response, lookup_result=_UNSET):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.market_open.return_value = sdk_response
        if lookup_result is not _UNSET:
            mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result
        captured = StringIO()
        with patch.dict(sys.modules, {}):
            with patch.object(mod, '__builtins__', mod.__builtins__):
                import builtins
                original_import = builtins.__import__

                def mock_import(name, *args, **kwargs):
                    if name == 'adapter':
                        fake_mod = MagicMock()
                        fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                        return fake_mod
                    return original_import(name, *args, **kwargs)
                with patch('builtins.__import__', side_effect=mock_import):
                    with patch('sys.stdout', captured):
                        mod.run_execute('BTC', 'buy', 0.01, 'live')
        return json.loads(captured.getvalue())

    def test_fill_with_oid_and_fee(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '55000.5', 'totalSz': '0.01', 'oid': 1234567890, 'fee': '0.35'}}]}}}
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result['execution']['fill']
        assert fill['avg_px'] == 55000.5
        assert fill['total_sz'] == 0.01
        assert fill['oid'] == 1234567890
        assert fill['fee'] == 0.35

    def test_fill_with_oid_no_fee(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '2100.0', 'totalSz': '0.5', 'oid': 9876543210}}]}}}
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result['execution']['fill']
        assert fill['oid'] == 9876543210
        assert 'fee' not in fill

    def test_fill_uses_numeric_lookup_result(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '2100.0', 'totalSz': '0.5', 'oid': 9876543210}}]}}}
        result = self._run_execute_with_mock_response(sdk_response, lookup_result={'fee': '0.42', 'closed_pnl': '3.14'})
        fill = result['execution']['fill']
        assert fill['fee'] == 0.42
        assert fill['closed_pnl'] == 3.14

    def test_fill_ignores_truthy_non_mapping_lookup_result(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '2100.0', 'totalSz': '0.5', 'oid': 9876543210}}]}}}
        result = self._run_execute_with_mock_response(sdk_response, lookup_result=MagicMock())
        fill = result['execution']['fill']
        assert fill['oid'] == 9876543210
        assert 'fee' not in fill
        assert 'closed_pnl' not in fill

    def test_fill_ignores_malformed_lookup_values(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '2100.0', 'totalSz': '0.5', 'oid': 9876543210}}]}}}
        result = self._run_execute_with_mock_response(sdk_response, lookup_result={'fee': MagicMock(), 'closed_pnl': MagicMock()})
        fill = result['execution']['fill']
        assert fill['oid'] == 9876543210
        assert 'fee' not in fill
        assert 'closed_pnl' not in fill

    def test_fill_without_oid(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '50000', 'totalSz': '0.1'}}]}}}
        result = self._run_execute_with_mock_response(sdk_response)
        fill = result['execution']['fill']
        assert fill['avg_px'] == 50000.0
        assert fill['total_sz'] == 0.1
        assert 'oid' not in fill
        assert 'fee' not in fill

    def test_fill_empty_statuses(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': []}}}
        result = self._run_execute_with_mock_response(sdk_response)
        assert result['execution']['fill'] == {}

class TestMarginMode:

    def _run_execute_with_margin(self, margin_mode, leverage, update_leverage_side_effect=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        if update_leverage_side_effect is not None:
            mock_adapter.update_leverage.side_effect = update_leverage_side_effect
        mock_adapter.market_open.return_value = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '50000', 'totalSz': '0.01'}}]}}}
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                exit_code = 0
                try:
                    mod.run_execute('BTC', 'buy', 0.01, 'live', margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return (json.loads(captured.getvalue()), mock_adapter, exit_code)

    def test_isolated_calls_update_leverage_with_is_cross_false(self):
        result, adapter, exit_code = self._run_execute_with_margin('isolated', 5)
        assert exit_code == 0
        adapter.update_leverage.assert_called_once_with(5, 'BTC', is_cross=False)
        adapter.market_open.assert_called_once()
        assert result['execution']['action'] == 'buy'

    def test_cross_calls_update_leverage_with_is_cross_true(self):
        result, adapter, exit_code = self._run_execute_with_margin('cross', 3)
        assert exit_code == 0
        adapter.update_leverage.assert_called_once_with(3, 'BTC', is_cross=True)
        adapter.market_open.assert_called_once()

    def test_no_margin_mode_skips_update_leverage(self):
        result, adapter, exit_code = self._run_execute_with_margin('', 0)
        assert exit_code == 0
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_called_once()

    def test_invalid_margin_mode_fails_closed(self):
        result, adapter, exit_code = self._run_execute_with_margin('portfolio', 5)
        assert exit_code == 1
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_not_called()
        assert 'invalid margin_mode' in result.get('error', '')

    def test_zero_leverage_with_mode_fails_closed(self):
        result, adapter, exit_code = self._run_execute_with_margin('isolated', 0)
        assert exit_code == 1
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_not_called()
        assert 'leverage' in result.get('error', '').lower()

    def test_update_leverage_failure_aborts_order(self):
        result, adapter, exit_code = self._run_execute_with_margin('isolated', 5, update_leverage_side_effect=RuntimeError('HL rejected: position open'))
        assert exit_code == 1
        adapter.update_leverage.assert_called_once()
        adapter.market_open.assert_not_called()
        assert 'update_leverage failed' in result.get('error', '')
        assert 'position open' in result.get('error', '')

class TestPeerLeverageSkip:

    def _run_execute_with_existing_pos(self, margin_mode, leverage, current_state):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.get_position_leverage.return_value = current_state
        mock_adapter.market_open.return_value = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '50000', 'totalSz': '0.01'}}]}}}
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                exit_code = 0
                try:
                    mod.run_execute('ETH', 'buy', 0.5, 'live', margin_mode=margin_mode, leverage=leverage)
                except SystemExit as e:
                    exit_code = e.code
        return (json.loads(captured.getvalue()), mock_adapter, exit_code)

    def test_skips_update_leverage_when_state_matches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos('isolated', 5, {'margin_mode': 'isolated', 'leverage': 5})
        assert exit_code == 0
        adapter.update_leverage.assert_not_called()
        adapter.market_open.assert_called_once()
        assert result['execution']['action'] == 'buy'

    def test_calls_update_leverage_when_mode_mismatches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos('isolated', 5, {'margin_mode': 'cross', 'leverage': 5})
        adapter.update_leverage.assert_called_once_with(5, 'ETH', is_cross=False)

    def test_calls_update_leverage_when_leverage_mismatches(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos('isolated', 5, {'margin_mode': 'isolated', 'leverage': 3})
        adapter.update_leverage.assert_called_once_with(5, 'ETH', is_cross=False)

    def test_calls_update_leverage_when_no_existing_position(self):
        result, adapter, exit_code = self._run_execute_with_existing_pos('isolated', 5, None)
        adapter.update_leverage.assert_called_once_with(5, 'ETH', is_cross=False)

    def test_state_fetch_failure_falls_back_to_calling_update_leverage(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.get_position_leverage.side_effect = RuntimeError('info endpoint timeout')
        mock_adapter.market_open.return_value = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '50000', 'totalSz': '0.01'}}]}}}
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                exit_code = 0
                try:
                    mod.run_execute('ETH', 'buy', 0.5, 'live', margin_mode='isolated', leverage=5)
                except SystemExit as e:
                    exit_code = e.code
        assert exit_code == 0
        mock_adapter.update_leverage.assert_called_once_with(5, 'ETH', is_cross=False)

class TestClassifySLResponse:

    def _classify(self, response):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod._classify_sl_response(response)

    def test_resting(self):
        kind, oid = self._classify({'response': {'type': 'order', 'data': {'statuses': [{'resting': {'oid': 12345}}]}}})
        assert kind == 'resting'
        assert oid == 12345

    def test_resting_missing_oid_returns_zero(self):
        kind, oid = self._classify({'response': {'type': 'order', 'data': {'statuses': [{'resting': {}}]}}})
        assert kind == 'resting'
        assert oid == 0

    def test_filled_immediate_with_oid(self):
        kind, oid = self._classify({'response': {'type': 'order', 'data': {'statuses': [{'filled': {'oid': 67890, 'avgPx': '3000'}}]}}})
        assert kind == 'filled'
        assert oid == 67890

    def test_filled_immediate_without_oid(self):
        kind, oid = self._classify({'response': {'type': 'order', 'data': {'statuses': [{'filled': {}}]}}})
        assert kind == 'filled'
        assert oid == 0

    def test_per_status_error(self):
        kind, payload = self._classify({'response': {'type': 'order', 'data': {'statuses': [{'error': 'Too many open trigger orders'}]}}})
        assert kind == 'error'
        assert 'Too many' in payload

    def test_missing_when_no_statuses(self):
        kind, payload = self._classify({'response': {'type': 'order', 'data': {'statuses': []}}})
        assert kind == 'missing'
        assert payload is None

    def test_missing_when_completely_malformed(self):
        kind, payload = self._classify({})
        assert kind == 'missing'
        assert payload is None

    def test_missing_when_status_is_not_dict(self):
        kind, payload = self._classify({'response': {'type': 'order', 'data': {'statuses': ['not a dict']}}})
        assert kind == 'missing'
        assert payload is None
_CANCEL_OK_RESPONSE = {'status': 'ok', 'response': {'type': 'cancel', 'data': {'statuses': ['success']}}}
_CANCEL_REJECTED_RESPONSE = {'status': 'err', 'response': {'type': 'cancel', 'data': {'statuses': [{'error': 'order already filled'}]}}}

class TestClassifyCancelResponse:

    def _load(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod

    def test_confirmed_string_status_is_ok(self):
        assert self._load()._classify_cancel_response(_CANCEL_OK_RESPONSE) == ('ok', '')

    def test_confirmed_dict_status_without_error_is_ok(self):
        resp = {'status': 'ok', 'response': {'data': {'statuses': [{}]}}}
        assert self._load()._classify_cancel_response(resp) == ('ok', '')

    def test_top_level_err_rejects(self):
        kind, payload = self._load()._classify_cancel_response({'status': 'err'})
        assert kind == 'error'
        assert 'err' in payload

    def test_per_order_error_rejects(self):
        resp = {'status': 'ok', 'response': {'data': {'statuses': [{'error': 'no such order'}]}}}
        assert self._load()._classify_cancel_response(resp)[0] == 'error'

    def test_missing_statuses_fails_closed(self):
        assert self._load()._classify_cancel_response({})[0] == 'error'

    def test_non_dict_fails_closed(self):
        assert self._load()._classify_cancel_response(None)[0] == 'error'

class TestUpdateStopLoss:

    def _run_update(self, side='long', place_response=None, cancel_side_effect=None, cancel_response=_UNSET, place_side_effect=None, open_oids=None, open_oids_side_effect=None, lookup_result=_UNSET, cancel_oid=11111, post_place_oids=_UNSET):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.round_perps_trigger_px.side_effect = lambda _symbol, px: round(px, 2)
        mock_adapter.cancel_trigger_order.return_value = _CANCEL_OK_RESPONSE if cancel_response is _UNSET else cancel_response
        base_oids = {11111} if open_oids is None else open_oids
        if open_oids_side_effect is not None:
            mock_adapter.open_order_oids.side_effect = open_oids_side_effect
        elif post_place_oids is not _UNSET:
            reads = {'n': 0}

            def _oids(_symbol):
                reads['n'] += 1
                if reads['n'] == 1:
                    return base_oids
                if isinstance(post_place_oids, Exception):
                    raise post_place_oids
                return post_place_oids
            mock_adapter.open_order_oids.side_effect = _oids
        else:
            mock_adapter.open_order_oids.return_value = base_oids
        if cancel_side_effect is not None:
            mock_adapter.cancel_trigger_order.side_effect = cancel_side_effect
        if lookup_result is not _UNSET:
            mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result
        mock_adapter.place_stop_loss.return_value = place_response or {'response': {'type': 'order', 'data': {'statuses': [{'resting': {'oid': 22222}}]}}}
        if place_side_effect is not None:
            mock_adapter.place_stop_loss.side_effect = place_side_effect
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_update_stop_loss('ETH', side, 0.5, 3104.123, 'live', cancel_oid=cancel_oid)
        return (json.loads(captured.getvalue()), mock_adapter)

    def test_cancel_then_place_long_stop(self):
        out, adapter = self._run_update(side='long')
        adapter.cancel_trigger_order.assert_called_once_with('ETH', 11111)
        adapter.place_stop_loss.assert_called_once_with('ETH', 0.5, 3104.12, False)
        method_names = [call[0] for call in adapter.method_calls]
        assert method_names.index('cancel_trigger_order') < method_names.index('place_stop_loss')
        assert out['cancel_stop_loss_succeeded'] is True
        assert out['stop_loss_oid'] == 22222
        assert out['stop_loss_trigger_px'] == 3104.12

    def test_short_stop_places_buy_trigger(self):
        out, adapter = self._run_update(side='short')
        adapter.place_stop_loss.assert_called_once_with('ETH', 0.5, 3104.12, True)
        assert out['stop_loss_oid'] == 22222

    def test_unreadable_placement_resolves_to_the_resting_oid(self):
        out, _ = self._run_update(place_response={'status': 'weird'}, post_place_oids={22222})
        assert out['stop_loss_oid'] == 22222
        assert 'stop_loss_outcome_unknown' not in out

    def test_placement_exception_resolves_to_the_resting_oid(self):

        def _boom(*_a, **_k):
            raise RuntimeError('connection reset after submit')
        out, _ = self._run_update(place_side_effect=_boom, post_place_oids={22222})
        assert out['stop_loss_oid'] == 22222
        assert 'stop_loss_outcome_unknown' not in out

    def test_unreadable_placement_with_nothing_resting_is_a_genuine_failure(self):
        out, _ = self._run_update(place_response={'status': 'weird'}, post_place_oids=set())
        assert 'stop_loss_outcome_unknown' not in out
        assert 'stop_loss_oid' not in out
        assert 'no usable status' in out['stop_loss_error']

    def test_unresolvable_diff_marks_outcome_unknown(self):
        out, _ = self._run_update(place_response={'status': 'weird'}, post_place_oids=RuntimeError('indexer down'))
        assert out.get('stop_loss_outcome_unknown') is True
        assert 'stop_loss_oid' not in out

    def test_ambiguous_diff_marks_outcome_unknown(self):
        out, _ = self._run_update(place_response={'status': 'weird'}, post_place_oids={22222, 33333})
        assert out.get('stop_loss_outcome_unknown') is True
        assert 'stop_loss_oid' not in out

    def test_rejected_placement_does_not_mark_outcome_unknown(self):
        out, _ = self._run_update(place_response={'status': 'err', 'response': 'open order limit'})
        assert 'stop_loss_outcome_unknown' not in out

    def test_cancel_failure_defers_replacement(self):
        out, adapter = self._run_update(cancel_side_effect=RuntimeError('cancel down'))
        adapter.cancel_trigger_order.assert_called_once_with('ETH', 11111)
        adapter.place_stop_loss.assert_not_called()
        assert 'cancel down' in out['cancel_stop_loss_error']
        assert 'stop_loss_oid' not in out

    def test_cancel_rejected_defers_replacement(self):
        out, adapter = self._run_update(cancel_response=_CANCEL_REJECTED_RESPONSE)
        adapter.place_stop_loss.assert_not_called()
        assert 'cancel_stop_loss_succeeded' not in out
        assert 'already filled' in out['cancel_stop_loss_error']
        assert 'stop_loss_oid' not in out

    def test_open_order_lookup_failure_defers_replacement(self):
        out, adapter = self._run_update(open_oids_side_effect=RuntimeError('indexer down'))
        adapter.cancel_trigger_order.assert_not_called()
        adapter.place_stop_loss.assert_not_called()
        assert out['open_order_check_error'] == 'indexer down'
        assert 'stop_loss_oid' not in out

    def test_missing_old_oid_with_fill_does_not_replace(self):
        out, adapter = self._run_update(open_oids=set(), lookup_result={'fee': 0.01, 'count': 1})
        adapter.cancel_trigger_order.assert_not_called()
        adapter.place_stop_loss.assert_not_called()
        assert out['stop_loss_filled_externally'] is True
        assert 'stop_loss_oid' not in out

    def test_missing_old_oid_without_fill_places_replacement(self):
        out, adapter = self._run_update(open_oids=set(), lookup_result=None)
        adapter.cancel_trigger_order.assert_not_called()
        adapter.lookup_fill_fee_by_oid.assert_called_once()
        adapter.place_stop_loss.assert_called_once_with('ETH', 0.5, 3104.12, False)
        assert out['stop_loss_oid'] == 22222

    def test_initial_placement_without_cancel_oid(self):
        out, adapter = self._run_update(cancel_oid=0)
        adapter.open_order_oids.assert_called_once_with('ETH')
        adapter.cancel_trigger_order.assert_not_called()
        adapter.place_stop_loss.assert_called_once_with('ETH', 0.5, 3104.12, False)
        assert out['stop_loss_oid'] == 22222

class TestCloseFullPosition:

    def _run_close_full(self, market_close_response=None):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.market_close.return_value = market_close_response or {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000.5', 'totalSz': '0.211', 'oid': 888}}]}}}
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_execute('ETH', 'sell', 0.0, 'live', close_full_position=True)
        return (json.loads(captured.getvalue()), mock_adapter)

    def test_uses_market_close_not_market_open(self):
        _, adapter = self._run_close_full()
        adapter.market_close.assert_called_once_with('ETH', sz=None)
        adapter.market_open.assert_not_called()

    def test_output_shape_matches_sized_close(self):
        out, _ = self._run_close_full()
        assert 'execution' in out
        fill = out['execution']['fill']
        assert fill['avg_px'] == 3000.5
        assert fill['total_sz'] == 0.211
        assert fill['oid'] == 888

    def test_dust_scenario_closes_full_residual(self):
        _, adapter = self._run_close_full(market_close_response={'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3100.0', 'totalSz': '0.211', 'oid': 999}}]}}})
        adapter.market_close.assert_called_once_with('ETH', sz=None)

class TestSyncProtection:

    def _run_sync(self, *, size=1.0, avg_cost=2000.0, entry_atr=20.0, side='long', sl_oid=0, tp1_oid=0, tp2_oid=0, tp_tiers=None, tp_oids=None, tp_armed_tiers=None, open_oids=None, fill_lookup_by_oid=None, place_responses=None, reconcile_fill_hints_json=None, cancel_tp_oids=None, force_sl_replace=False):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.open_order_oids.return_value = set() if open_oids is None else set(open_oids)
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)
        mock_adapter.round_size.side_effect = lambda _sym, sz: round(sz, 3)
        mock_adapter.floor_size.side_effect = lambda _sym, sz: math.floor(sz * 1000) / 1000
        fills = fill_lookup_by_oid or {}

        def lookup_side_effect(oid, *args, **kwargs):
            return fills.get(int(oid), {})
        mock_adapter.lookup_fill_fee_by_oid.side_effect = lookup_side_effect
        responses = place_responses or {}

        def stop_loss_side_effect(*args, **kwargs):
            return responses.get('sl', {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'resting': {'oid': 9000}}]}}})

        def tp_side_effect(symbol, sz, px, is_buy):
            count = mock_adapter.place_take_profit_limit.call_count
            key = 'tp1' if count == 1 else 'tp2'
            return responses.get(key, {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'resting': {'oid': 9100 + count}}]}}})
        mock_adapter.place_stop_loss.side_effect = stop_loss_side_effect
        mock_adapter.place_take_profit_limit.side_effect = tp_side_effect
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', side, size, avg_cost, entry_atr, 'live', stop_loss_atr_mult=1.0, tp1_atr_mult=1.0, tp1_fraction=0.5, tp2_atr_mult=2.0, stop_loss_oid=sl_oid, tp1_oid=tp1_oid, tp2_oid=tp2_oid, tp_tiers=tp_tiers, tp_oids=tp_oids, tp_armed_tiers=tp_armed_tiers, reconcile_fill_hints_json=reconcile_fill_hints_json or '', cancel_tp_oids=cancel_tp_oids, force_sl_replace=force_sl_replace)
        return (json.loads(captured.getvalue()), mock_adapter)

    def test_sl_skips_force_replace_when_size_zero(self):
        out, adapter = self._run_sync(size=0, cancel_tp_oids=[303], open_oids={100}, sl_oid=100, force_sl_replace=True)
        assert out.get('stop_loss_oid') == 100
        adapter.place_stop_loss.assert_not_called()

    def test_surplus_cancel_failed_reported(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.open_order_oids.return_value = {303}
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)
        mock_adapter.round_size.side_effect = lambda _sym, sz: round(sz, 3)
        mock_adapter.floor_size.side_effect = lambda _sym, sz: math.floor(sz * 1000) / 1000
        mock_adapter.lookup_fill_fee_by_oid.return_value = {}
        mock_adapter.cancel_order_by_oid.side_effect = Exception('rpc down')
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', 'long', 0, 2000.0, 20.0, 'live', stop_loss_atr_mult=0, cancel_tp_oids=[303])
        out = json.loads(captured.getvalue())
        assert out.get('tp_cancel_failed_oids') == [303]
        mock_adapter.cancel_order_by_oid.assert_called_once_with('ETH', 303)

    def test_surplus_cancel_filled_skips_cancel(self):
        out, adapter = self._run_sync(cancel_tp_oids=[303], open_oids=set(), fill_lookup_by_oid={303: {'fee': 0.05, 'closed_pnl': 25.0, 'count': 1}})
        assert out.get('tp_cancel_filled_oids') == [303]
        assert not out.get('tp_cancel_failed_oids')
        adapter.cancel_order_by_oid.assert_not_called()

    def test_surplus_cancel_runs_when_size_zero(self):
        out, adapter = self._run_sync(size=0, tp_tiers=[(1.0, 0.5), (2.0, 1.0)], cancel_tp_oids=[303], open_oids={303})
        adapter.cancel_order_by_oid.assert_called_once_with('ETH', 303)

    def test_sl_filled_at_submit_skips_tp_placement(self):
        out, adapter = self._run_sync(tp_tiers=[(1.0, 0.5), (2.0, 1.0)], place_responses={'sl': {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'oid': 67890, 'avgPx': '1980'}}]}}}})
        assert out.get('stop_loss_filled_immediately') is True
        adapter.place_take_profit_limit.assert_not_called()
        assert not out.get('tp_oids')

    def test_resting_still_places_and_records_tps(self):
        out, adapter = self._run_sync(tp_tiers=[(1.0, 0.5), (2.0, 1.0)])
        assert adapter.place_take_profit_limit.call_count == 2
        assert len(out.get('tp_oids') or []) == 2

    def test_existing_oid_still_open_returns_same_oid(self):
        out, adapter = self._run_sync(tp1_oid=200, tp2_oid=300, sl_oid=100, open_oids={100, 200, 300})
        assert out['tp1_oid'] == 200
        assert out['tp2_oid'] == 300
        assert out['stop_loss_oid'] == 100
        adapter.place_take_profit_limit.assert_not_called()
        adapter.place_stop_loss.assert_not_called()
        adapter.lookup_fill_fee_by_oid.assert_not_called()

    def test_missing_oid_with_no_fill_places_replacement(self):
        out, adapter = self._run_sync(tp1_oid=200, tp2_oid=300, open_oids=set(), fill_lookup_by_oid={})
        assert 'tp1_oid' in out
        assert 'tp2_oid' in out
        assert adapter.lookup_fill_fee_by_oid.called
        assert adapter.place_take_profit_limit.call_count == 2
        assert not out.get('tp1_filled_externally')
        assert not out.get('tp2_filled_externally')

    def test_missing_oid_with_fill_marks_externally_filled(self):
        out, adapter = self._run_sync(tp1_oid=200, tp2_oid=300, open_oids=set(), fill_lookup_by_oid={200: {'fee': 0.05, 'closed_pnl': 25.0, 'count': 1}})
        assert out.get('tp1_filled_externally') is True
        assert 'tp1_fill' in out
        assert out['tp1_fill']['fee'] == 0.05
        assert not out.get('tp2_filled_externally')
        assert adapter.place_take_profit_limit.call_count == 1

    def test_zero_oid_armed_true_does_not_re_place_consumed_tp1(self):
        out, adapter = self._run_sync(size=0.22, tp_oids=[0, 300], tp_armed_tiers=[True, True], open_oids={300, 9000}, sl_oid=9000)
        assert out['tp_oids'] == [0, 300]
        assert out['tp2_oid'] == 300
        assert 'tp1_oid' not in out
        adapter.place_take_profit_limit.assert_not_called()

    def test_three_tiers_places_incremental_sizes(self):
        out, adapter = self._run_sync(size=10.0, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 0.8}, {'atr_multiple': 3.0, 'close_fraction': 1.0}], tp_oids=[0, 0, 0], open_oids=set())
        assert len(out['tp_oids']) == 3
        assert out['tp_pxs'] == [2020.0, 2040.0, 2060.0]
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sizes == pytest.approx([5.0, 3.0, 2.0])
        assert adapter.place_take_profit_limit.call_count == 3

    def test_final_tier_fraction_is_coerced_to_remaining_size(self):
        out, adapter = self._run_sync(size=10.0, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 0.7}], tp_oids=[0, 0], open_oids=set())
        assert out['tp_oids']
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sizes == pytest.approx([5.0, 5.0])

    def test_non_increasing_sorted_tiers_are_rejected(self):
        out, adapter = self._run_sync(tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 0.5, 'close_fraction': 0.7}], tp_oids=[0, 0], open_oids=set())
        assert 'tp_oids' not in out
        adapter.place_take_profit_limit.assert_not_called()

    def test_single_tier_config_is_rejected(self):
        out, adapter = self._run_sync(tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 1.0}], tp_oids=[0], open_oids=set())
        assert 'tp_oids' not in out
        adapter.place_take_profit_limit.assert_not_called()

    def test_three_tiers_detects_middle_oid_filled_externally(self):
        out, adapter = self._run_sync(tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 0.8}, {'atr_multiple': 3.0, 'close_fraction': 1.0}], tp_oids=[100, 200, 300], open_oids={100, 300}, fill_lookup_by_oid={200: {'fee': 0.01, 'closed_pnl': 7.0, 'count': 1}})
        assert out['tp_oids'] == [100, 0, 300]
        assert out['tp_filled_externally'] == [False, True, False]
        assert out['tp_fills'][1]['closed_pnl'] == 7.0
        adapter.place_take_profit_limit.assert_not_called()

    def test_reconcile_fill_hints_skips_lookup_fill_fee_by_oid(self):
        hints = json.dumps([{'oid': 9101, 'filled': True, 'fee': 0.02, 'closed_pnl': 1.5, 'count': 2}])
        out, adapter = self._run_sync(tp1_oid=9100, tp2_oid=9101, sl_oid=0, open_oids={9100}, fill_lookup_by_oid={}, reconcile_fill_hints_json=hints)
        assert out['tp_oids'][1] == 0
        assert out['tp_filled_externally'][1] is True
        assert out['tp_fills'][1]['fee'] == 0.02
        assert out['tp_fills'][1]['closed_pnl'] == 1.5
        adapter.lookup_fill_fee_by_oid.assert_not_called()

    def test_reconcile_fill_hints_filled_false_still_queries_userfills(self):
        hints = json.dumps([{'oid': 9101, 'filled': False}])
        out, adapter = self._run_sync(tp1_oid=9100, tp2_oid=9101, sl_oid=0, open_oids={9100}, fill_lookup_by_oid={9101: {'fee': 0.03, 'closed_pnl': 2.0, 'count': 1}}, reconcile_fill_hints_json=hints)
        assert out['tp_filled_externally'][1] is True
        assert out['tp_fills'][1]['fee'] == 0.03
        adapter.lookup_fill_fee_by_oid.assert_called()

    def test_reconcile_fill_hints_malformed_json_queries_userfills(self):
        out, adapter = self._run_sync(tp1_oid=9100, tp2_oid=9101, sl_oid=0, open_oids={9100}, fill_lookup_by_oid={9101: {'fee': 0.04, 'closed_pnl': 3.0, 'count': 1}}, reconcile_fill_hints_json='{not-json')
        assert out['tp_filled_externally'][1] is True
        adapter.lookup_fill_fee_by_oid.assert_called()

    def test_reconcile_fill_hints_extra_oid_does_not_skip_other_oid_lookup(self):
        hints = json.dumps([{'oid': 1, 'filled': True, 'fee': 0.0, 'count': 0}])
        out, adapter = self._run_sync(tp1_oid=9100, tp2_oid=9101, sl_oid=0, open_oids={9100}, fill_lookup_by_oid={9101: {'fee': 0.05, 'closed_pnl': 4.0, 'count': 1}}, reconcile_fill_hints_json=hints)
        assert out['tp_filled_externally'][1] is True
        called_oids = [c.args[0] for c in adapter.lookup_fill_fee_by_oid.call_args_list]
        assert 9101 in called_oids

    def test_open_orders_fetch_failure_defers_replacement(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        mock_adapter = MagicMock()
        mock_adapter_cls.return_value = mock_adapter
        mock_adapter.open_order_oids.side_effect = RuntimeError('indexer down')
        mock_adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 4)
        mock_adapter.round_size.side_effect = lambda _sym, sz: round(sz, 3)
        mock_adapter.floor_size.side_effect = lambda _sym, sz: math.floor(sz * 1000) / 1000
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', 'long', 1.0, 2000.0, 20.0, 'live', stop_loss_atr_mult=1.0, tp1_atr_mult=1.0, tp1_fraction=0.5, tp2_atr_mult=2.0, stop_loss_oid=100, tp1_oid=200, tp2_oid=300, reconcile_fill_hints_json='')
        out = json.loads(captured.getvalue())
        assert out['open_order_check_error'] == 'indexer down'
        mock_adapter.place_take_profit_limit.assert_not_called()
        mock_adapter.place_stop_loss.assert_not_called()

    def test_floor_residual_absorbed_by_final_tier(self):
        out, adapter = self._run_sync(size=0.003, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}], tp_oids=[0, 0], open_oids=set())
        assert out['tp_oids']
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sizes == pytest.approx([0.001, 0.002])
        assert sum(sizes) == pytest.approx(0.003)

    def test_float_drift_below_lot_boundary_normalizes(self):
        drifted = 0.011 - 0.01
        assert drifted < 0.001
        out, adapter = self._run_sync(size=drifted, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}], tp_oids=[0, 0], open_oids=set())
        assert out['tp_oids']
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sum(sizes) == pytest.approx(0.001)

    def test_size_rounds_to_zero_skips_tier_block(self):
        out, adapter = self._run_sync(size=0.0004, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.5}, {'atr_multiple': 2.0, 'close_fraction': 1.0}], tp_oids=[0, 0], open_oids=set())
        adapter.place_take_profit_limit.assert_not_called()
        assert 'tp_oids' not in out
        assert 'tp_pxs' not in out

    def test_three_tier_non_uniform_flooring_zero_residual(self):
        out, adapter = self._run_sync(size=0.007, tp_tiers=[{'atr_multiple': 1.0, 'close_fraction': 0.3}, {'atr_multiple': 2.0, 'close_fraction': 0.6}, {'atr_multiple': 3.0, 'close_fraction': 1.0}], tp_oids=[0, 0, 0], open_oids=set())
        assert out['tp_oids']
        sizes = [call.args[1] for call in adapter.place_take_profit_limit.call_args_list]
        assert sum(sizes) == pytest.approx(0.007)

class TestComputeTPTierSizes:

    @staticmethod
    def _floor3(sz):
        return math.floor(sz * 1000) / 1000

    def _load(self):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        return mod

    def test_zero_size_returns_zero_sizes(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(0.0, [(1.0, 0.5), (2.0, 1.0)], self._floor3)
        assert sizes == [0.0, 0.0]

    def test_negative_size_returns_zero_sizes(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(-0.5, [(1.0, 0.5), (2.0, 1.0)], self._floor3)
        assert sizes == [0.0, 0.0]

    def test_empty_tiers_returns_empty(self):
        mod = self._load()
        assert mod.compute_tp_tier_sizes(1.0, [], self._floor3) == []

    def test_two_tier_5050_split_zero_residual(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(0.003, [(1.0, 0.5), (2.0, 1.0)], self._floor3)
        assert sizes == pytest.approx([0.001, 0.002])
        assert sum(sizes) == pytest.approx(0.003)

    def test_final_tier_absorbs_subdivided_floor_loss(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(0.007, [(1.0, 0.3), (2.0, 0.6), (3.0, 1.0)], self._floor3)
        assert sizes == pytest.approx([0.002, 0.002, 0.003])
        assert sum(sizes) == pytest.approx(0.007)

    def test_lot_aligned_size_preserves_exact_split(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(10.0, [(1.0, 0.5), (2.0, 1.0)], self._floor3)
        assert sizes == pytest.approx([5.0, 5.0])

    def test_final_tier_below_one_uses_floored_remainder(self):
        mod = self._load()
        sizes = mod.compute_tp_tier_sizes(0.5, [(1.0, 0.5), (2.0, 1.0)], self._floor3)
        assert sum(sizes) == pytest.approx(0.5)
        assert sizes[0] == pytest.approx(0.25)
        assert sizes[1] == pytest.approx(0.25)

class TestProtectionSyncStopLossTriggerContract:

    def _run_sync(self, *, open_oids, stop_loss_oid, force_sl_replace, place_response):
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        adapter = MagicMock()
        mock_adapter_cls.return_value = adapter
        adapter.open_order_oids.return_value = set(open_oids)
        adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 2)
        adapter.round_size.side_effect = lambda _sym, sz: sz
        adapter.place_stop_loss.return_value = place_response
        adapter.cancel_order_by_oid.return_value = _CANCEL_OK_RESPONSE
        captured = StringIO()
        import builtins
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', 'long', 1.0, 2400.0, 30.0, 'live', stop_loss_atr_mult=2.5, stop_loss_oid=stop_loss_oid, force_sl_replace=force_sl_replace)
        return (json.loads(captured.getvalue()), adapter)

    @staticmethod
    def _resting(oid):
        return {'status': 'ok', 'response': {'data': {'statuses': [{'resting': {'oid': oid}}]}}}

    def test_echoed_oid_reports_no_trigger_price(self):
        out, adapter = self._run_sync(open_oids=[4242], stop_loss_oid=4242, force_sl_replace=False, place_response=self._resting(9001))
        assert out.get('stop_loss_oid') == 4242
        assert 'stop_loss_trigger_px' not in out
        adapter.place_stop_loss.assert_not_called()

    def test_force_replace_that_rests_reports_the_placed_trigger(self):
        out, adapter = self._run_sync(open_oids=[4242], stop_loss_oid=4242, force_sl_replace=True, place_response=self._resting(9002))
        assert out.get('stop_loss_oid') == 9002
        assert out.get('stop_loss_trigger_px') == pytest.approx(2325.0)
        adapter.place_stop_loss.assert_called_once()

    def test_force_replace_whose_placement_fails_reports_no_trigger_price(self):
        out, _ = self._run_sync(open_oids=[4242], stop_loss_oid=4242, force_sl_replace=True, place_response={'status': 'err', 'response': 'open order limit'})
        assert 'stop_loss_trigger_px' not in out
        assert out.get('stop_loss_error')

    def test_force_replace_cancel_rejected_defers_replacement(self):
        out, adapter = self._run_sync_cancel_response(_CANCEL_REJECTED_RESPONSE)
        adapter.place_stop_loss.assert_not_called()
        assert out.get('cancel_stop_loss_succeeded') is False
        assert 'already filled' in out['stop_loss_error']
        assert 'stop_loss_oid' not in out

    def _run_sync_cancel_response(self, cancel_response):
        import builtins
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        adapter = MagicMock()
        mock_adapter_cls.return_value = adapter
        adapter.open_order_oids.return_value = {4242}
        adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 2)
        adapter.round_size.side_effect = lambda _sym, sz: sz
        adapter.place_stop_loss.return_value = self._resting(9002)
        adapter.cancel_order_by_oid.return_value = cancel_response
        captured = StringIO()
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', 'long', 1.0, 2400.0, 30.0, 'live', stop_loss_atr_mult=2.5, stop_loss_oid=4242, force_sl_replace=True)
        return (json.loads(captured.getvalue()), adapter)

    def test_fresh_placement_reports_the_placed_trigger(self):
        out, adapter = self._run_sync(open_oids=[], stop_loss_oid=0, force_sl_replace=False, place_response=self._resting(9003))
        assert out.get('stop_loss_oid') == 9003
        assert out.get('stop_loss_trigger_px') == pytest.approx(2325.0)
        adapter.place_stop_loss.assert_called_once()

    def _run_sync_dynamic_oids(self, *, oid_reads, place_response):
        import builtins
        mod, spec = _load_check_module()
        spec.loader.exec_module(mod)
        mock_adapter_cls = MagicMock()
        adapter = MagicMock()
        mock_adapter_cls.return_value = adapter
        adapter.open_order_oids.side_effect = [set(s) for s in oid_reads]
        adapter.round_perps_trigger_px.side_effect = lambda _sym, px: round(px, 2)
        adapter.round_size.side_effect = lambda _sym, sz: sz
        adapter.place_stop_loss.return_value = place_response
        adapter.cancel_order_by_oid.return_value = _CANCEL_OK_RESPONSE
        captured = StringIO()
        original_import = builtins.__import__

        def mock_import(name, *args, **kwargs):
            if name == 'adapter':
                fake_mod = MagicMock()
                fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
                return fake_mod
            return original_import(name, *args, **kwargs)
        with patch('builtins.__import__', side_effect=mock_import):
            with patch('sys.stdout', captured):
                mod.run_sync_protection('ETH', 'long', 1.0, 2400.0, 30.0, 'live', stop_loss_atr_mult=2.5, stop_loss_oid=4242, force_sl_replace=True)
        return (json.loads(captured.getvalue()), adapter)

    def test_unreadable_placement_resolved_to_resting_reports_no_error(self):
        out, _ = self._run_sync_dynamic_oids(oid_reads=[{4242}, {9004}], place_response={'status': 'weird'})
        assert out.get('stop_loss_oid') == 9004
        assert 'stop_loss_error' not in out

    def test_unreadable_placement_ambiguous_diff_reports_no_warning_error(self):
        out, _ = self._run_sync_dynamic_oids(oid_reads=[{4242}, {9004, 9005}], place_response={'status': 'weird'})
        assert out.get('stop_loss_outcome_unknown') is True
        assert 'stop_loss_error' not in out

    def test_unreadable_placement_nothing_resting_keeps_the_error(self):
        out, _ = self._run_sync_dynamic_oids(oid_reads=[{4242}, set()], place_response={'status': 'weird'})
        assert 'stop_loss_oid' not in out
        assert 'stop_loss_outcome_unknown' not in out
        assert 'no usable status' in out.get('stop_loss_error', '')
