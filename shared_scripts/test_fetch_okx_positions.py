import builtins
import importlib.util
import json
import os
import sys
from io import StringIO
from unittest.mock import MagicMock, patch
import pytest

def _run_script(positions_or_exc, is_live=True):
    script_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'fetch_okx_positions.py')
    spec = importlib.util.spec_from_file_location('fetch_okx_positions', script_path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mock_adapter_cls = MagicMock()
    mock_adapter = MagicMock()
    mock_adapter.is_live = is_live
    mock_adapter_cls.return_value = mock_adapter
    if isinstance(positions_or_exc, Exception):
        mock_adapter.fetch_open_positions.side_effect = positions_or_exc
    else:
        mock_adapter.fetch_open_positions.return_value = positions_or_exc
    captured = StringIO()
    exit_code = {'value': 0}
    original_import = builtins.__import__

    def mock_import(name, *args, **kwargs):
        if name == 'adapter':
            fake_mod = MagicMock()
            fake_mod.OKXExchangeAdapter = mock_adapter_cls
            return fake_mod
        return original_import(name, *args, **kwargs)

    def mock_exit(code=0):
        exit_code['value'] = code
        raise SystemExit(code)
    with patch('builtins.__import__', side_effect=mock_import), patch('sys.stdout', captured), patch('sys.argv', ['fetch_okx_positions.py']), patch.object(mod.sys, 'exit', side_effect=mock_exit):
        try:
            mod.main()
        except SystemExit:
            pass
    raw = captured.getvalue().strip()
    parsed = json.loads(raw) if raw else {}
    return (parsed, exit_code['value'])

class TestSuccess:

    def test_long_position(self):
        out, code = _run_script([{'symbol': 'BTC/USDT:USDT', 'contracts': '0.01', 'side': 'long', 'entryPrice': '42000.5'}])
        assert code == 0
        assert len(out['positions']) == 1
        p = out['positions'][0]
        assert p['coin'] == 'BTC'
        assert p['size'] == 0.01
        assert p['side'] == 'long'
        assert p['entry_price'] == 42000.5

    def test_short_position_size_is_negative(self):
        out, code = _run_script([{'symbol': 'ETH/USDT:USDT', 'contracts': '0.5', 'side': 'short', 'entryPrice': '3000'}])
        assert code == 0
        assert out['positions'][0]['size'] == -0.5
        assert out['positions'][0]['side'] == 'short'

    def test_zero_size_filtered(self):
        out, code = _run_script([{'symbol': 'BTC/USDT:USDT', 'contracts': '0', 'side': 'long'}])
        assert code == 0
        assert out['positions'] == []

    def test_empty_positions(self):
        out, code = _run_script([])
        assert code == 0
        assert out['positions'] == []
        assert 'error' not in out

class TestFailurePaths:

    def test_non_live_adapter(self):
        out, code = _run_script([], is_live=False)
        assert code == 1
        assert 'OKX_API_KEY' in out['error']

    def test_exchange_raises(self):
        out, code = _run_script(RuntimeError('OKX 503 Service Unavailable'))
        assert code == 1
        assert 'OKX 503' in out['error']
        assert out['positions'] == []
if __name__ == '__main__':
    sys.exit(pytest.main([__file__, '-v']))
