import json
import os
import sys
import traceback
from datetime import datetime, timezone
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'okx'))

def main():
    try:
        from adapter import OKXExchangeAdapter
        adapter = OKXExchangeAdapter()
        if not adapter.is_live:
            _emit_error('OKX adapter not live — set OKX_API_KEY / OKX_API_SECRET / OKX_PASSPHRASE')
            return
        eq, upnl = adapter.get_account_equity_and_upnl()
        balance = float(eq or 0.0)
        unrealized_pnl = float(upnl or 0.0)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return
    print(json.dumps({'balance': balance, 'unrealized_pnl': unrealized_pnl, 'platform': 'okx', 'timestamp': datetime.now(timezone.utc).isoformat()}))

def _emit_error(message):
    print(json.dumps({'balance': 0.0, 'unrealized_pnl': 0.0, 'platform': 'okx', 'timestamp': datetime.now(timezone.utc).isoformat(), 'error': message}))
    sys.exit(1)
if __name__ == '__main__':
    main()
