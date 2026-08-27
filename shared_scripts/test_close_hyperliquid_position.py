import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, call, patch
import pytest
_UNSET = object()

def _run_script(sdk_response_or_exc, argv, lookup_result=_UNSET):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'close_hyperliquid_position.py')
    spec = importlib.util.spec_from_file_location('close_hyperliquid_position', script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(sdk_response_or_exc, Exception):
        mock_adapter.market_close.side_effect = sdk_response_or_exc
    else:
        mock_adapter.market_close.return_value = sdk_response_or_exc
    if lookup_result is not _UNSET:
        mock_adapter.lookup_fill_fee_by_oid.return_value = lookup_result
    captured = StringIO()
    exit_code = {'value': 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == 'adapter':
            fake_mod = MagicMock()
            fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code['value'] = code
        raise SystemExit(code)
    with patch('builtins.__import__', side_effect=mock_import), patch('sys.stdout', captured), patch('sys.argv', ['close_hyperliquid_position.py'] + argv), patch.object(mod.sys, 'exit', side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass
    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return (parsed, exit_code['value'])

class TestPaperModeRejected:

    def test_paper_mode_exits_nonzero(self):
        out, code = _run_script({}, ['--symbol=ETH', '--mode=paper'])
        assert code == 1
        assert '--mode=live required' in out['error']
        assert out['close'] is None

    def test_default_mode_is_live(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000', 'totalSz': '0.5'}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH'])
        assert code == 0, out
        assert 'error' not in out

class TestSuccessFill:

    def test_filled_with_all_fields(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000.5', 'totalSz': '0.517', 'oid': 9999999, 'fee': '1.25'}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 0
        assert out['close']['symbol'] == 'ETH'
        fill = out['close']['fill']
        assert fill['avg_px'] == 3000.5
        assert fill['total_sz'] == 0.517
        assert fill['oid'] == 9999999
        assert fill['fee'] == 1.25

    def test_filled_missing_optional_fields(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '50000', 'totalSz': '0.01'}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=BTC', '--mode=live'])
        assert code == 0
        fill = out['close']['fill']
        assert fill['avg_px'] == 50000
        assert fill['total_sz'] == 0.01
        assert 'oid' not in fill
        assert 'fee' not in fill

    def test_success_reports_exact_cancelled_oids(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000', 'totalSz': '0.5'}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=123', '--cancel-stop-loss-oid=456'])
        assert code == 0
        assert out['cancel_stop_loss_succeeded'] is True
        assert out['cancel_stop_loss_succeeded_oids'] == [123, 456]

    def test_filled_uses_numeric_lookup_result(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000.5', 'totalSz': '0.517', 'oid': 9999999}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'], lookup_result={'fee': '0.91', 'closed_pnl': '7.5'})
        assert code == 0
        fill = out['close']['fill']
        assert fill['fee'] == 0.91
        assert fill['closed_pnl'] == 7.5

    def test_filled_ignores_truthy_non_mapping_lookup_result(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000.5', 'totalSz': '0.517', 'oid': 9999999}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'], lookup_result=MagicMock())
        assert code == 0
        fill = out['close']['fill']
        assert fill['oid'] == 9999999
        assert 'fee' not in fill
        assert 'closed_pnl' not in fill

    def test_filled_ignores_malformed_lookup_values(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000.5', 'totalSz': '0.517', 'oid': 9999999}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'], lookup_result={'fee': MagicMock(), 'closed_pnl': MagicMock()})
        assert code == 0
        fill = out['close']['fill']
        assert fill['oid'] == 9999999
        assert 'fee' not in fill
        assert 'closed_pnl' not in fill

class TestAlreadyFlat:

    def test_empty_statuses_is_success(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': []}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 0, out
        assert out['close']['symbol'] == 'ETH'
        assert out['close']['fill'] == {}
        assert 'error' not in out
        assert out['close']['already_flat'] is True

    def test_no_response_field(self):
        sdk_response = {'status': 'ok'}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 0
        assert out['close']['already_flat'] is True

class TestFailurePaths:

    def test_outer_status_not_ok(self):
        sdk_response = {'status': 'err', 'response': {'msg': 'nonce too low'}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 1
        assert "sdk status='err'" in out['error']

    def test_per_status_error(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'error': 'Order reduce-only would not reduce position'}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 1
        assert 'per-status error' in out['error']
        assert 'reduce-only would not reduce' in out['error']

    def test_per_status_resting(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'resting': {'oid': 12345}}]}}}
        out, code = _run_script(sdk_response, ['--symbol=ETH', '--mode=live'])
        assert code == 1
        assert 'close not filled' in out['error']
        assert 'resting' in out['error']

    def test_adapter_raises(self):
        out, code = _run_script(RuntimeError('HYPERLIQUID_SECRET_KEY not set'), ['--symbol=ETH', '--mode=live'])
        assert code == 1
        assert 'HYPERLIQUID_SECRET_KEY' in out['error']
        assert out['close']['fill'] == {}

    def test_non_dict_response(self):
        out, code = _run_script('unexpected string response', ['--symbol=ETH', '--mode=live'])
        assert code == 1
        assert 'unexpected SDK response type' in out['error']

def _run_script_with_cancel(sdk_response, cancel_response, argv):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'close_hyperliquid_position.py')
    spec = importlib.util.spec_from_file_location('close_hyperliquid_position', script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(sdk_response, Exception):
        mock_adapter.market_close.side_effect = sdk_response
    else:
        mock_adapter.market_close.return_value = sdk_response
    if isinstance(cancel_response, Exception):
        mock_adapter.cancel_trigger_order.side_effect = cancel_response
    else:
        mock_adapter.cancel_trigger_order.return_value = cancel_response
    captured = StringIO()
    exit_code = {'value': 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == 'adapter':
            fake_mod = MagicMock()
            fake_mod.HyperliquidExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code['value'] = code
        raise SystemExit(code)
    with patch('builtins.__import__', side_effect=mock_import), patch('sys.stdout', captured), patch('sys.argv', ['close_hyperliquid_position.py'] + argv), patch.object(mod.sys, 'exit', side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass
    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return (parsed, exit_code['value'], mock_adapter)

class TestCancelStopLossOID:

    def _filled_response(self, sym='ETH'):
        return {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000', 'totalSz': '1.0', 'oid': 999}}]}}}

    def test_no_cancel_when_oid_zero(self):
        out, code, adapter = _run_script_with_cancel(self._filled_response(), {'status': 'ok'}, ['--symbol=ETH', '--mode=live'])
        assert code == 0
        adapter.cancel_trigger_order.assert_not_called()
        assert 'cancel_stop_loss_succeeded' not in out
        assert 'cancel_stop_loss_error' not in out

    def test_cancel_succeeded_surfaces_in_envelope(self):
        out, code, adapter = _run_script_with_cancel(self._filled_response(), {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345'])
        assert code == 0
        adapter.cancel_trigger_order.assert_called_once_with('ETH', 12345)
        assert out.get('cancel_stop_loss_succeeded') is True

    def test_multiple_cancel_oids_are_all_attempted(self):
        out, code, adapter = _run_script_with_cancel(self._filled_response(), {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345', '--cancel-stop-loss-oid=67890'])
        assert code == 0
        adapter.cancel_trigger_order.assert_any_call('ETH', 12345)
        adapter.cancel_trigger_order.assert_any_call('ETH', 67890)
        assert adapter.cancel_trigger_order.call_count == 2
        assert out.get('cancel_stop_loss_succeeded') is True

    def test_cancel_failure_is_non_fatal(self):
        out, code, adapter = _run_script_with_cancel(self._filled_response(), RuntimeError('order not found'), ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=999'])
        assert code == 0
        assert 'order not found' in out.get('cancel_stop_loss_error', '')
        assert 'cancel_stop_loss_succeeded' not in out

    def test_cancel_state_propagates_through_close_failure(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'error': 'no position to close'}]}}}
        out, code, adapter = _run_script_with_cancel(sdk_response, {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345'])
        assert code == 1
        assert 'per-status error' in out['error']
        assert out.get('cancel_stop_loss_succeeded') is True

    def test_post_cancel_waits_until_close_fill(self):
        out, code, adapter = _run_script_with_cancel(self._filled_response(), {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345', '--cancel-protection-after-close'])
        assert code == 0
        assert adapter.method_calls[:2] == [call.market_close('ETH', None), call.cancel_trigger_order('ETH', 12345)]
        assert out.get('cancel_stop_loss_succeeded') is True
        assert out.get('cancel_stop_loss_succeeded_oids') == [12345]

    def test_post_cancel_skips_cancel_when_close_errors(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'error': 'no position to close'}]}}}
        out, code, adapter = _run_script_with_cancel(sdk_response, {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345', '--cancel-protection-after-close'])
        assert code == 1
        assert 'per-status error' in out['error']
        adapter.cancel_trigger_order.assert_not_called()
        assert 'cancel_stop_loss_succeeded' not in out

    def test_post_cancel_skips_cancel_when_adapter_raises(self):
        out, code, adapter = _run_script_with_cancel(RuntimeError('network down'), {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--cancel-stop-loss-oid=12345', '--cancel-protection-after-close'])
        assert code == 1
        assert 'network down' in out['error']
        adapter.cancel_trigger_order.assert_not_called()

    def test_post_cancel_skips_cancel_when_sized_close_underfills(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000', 'totalSz': '0.5', 'oid': 999}}]}}}
        out, code, adapter = _run_script_with_cancel(sdk_response, {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--sz=1.0', '--cancel-stop-loss-oid=12345', '--cancel-protection-after-close'])
        assert code == 0
        assert out['close']['fill']['total_sz'] == 0.5
        adapter.cancel_trigger_order.assert_not_called()
        assert 'cancel_stop_loss_succeeded' not in out

    def test_post_cancel_cancels_when_sized_close_fills_request(self):
        sdk_response = {'status': 'ok', 'response': {'type': 'order', 'data': {'statuses': [{'filled': {'avgPx': '3000', 'totalSz': '1.0', 'oid': 999}}]}}}
        out, code, adapter = _run_script_with_cancel(sdk_response, {'status': 'ok'}, ['--symbol=ETH', '--mode=live', '--sz=1.0', '--cancel-stop-loss-oid=12345', '--cancel-protection-after-close'])
        assert code == 0
        adapter.cancel_trigger_order.assert_called_once_with('ETH', 12345)
        assert out.get('cancel_stop_loss_succeeded_oids') == [12345]
if __name__ == '__main__':
    sys.exit(pytest.main([__file__, '-v']))
