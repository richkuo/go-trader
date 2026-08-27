import json
import os
import sys
import traceback
from datetime import datetime, timezone
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'topstep'))

def main():
    try:
        from adapter import TopStepExchangeAdapter
        adapter = TopStepExchangeAdapter(mode='live')
        if not adapter.is_live:
            _emit_error('TopStep adapter not live — set TOPSTEP_API_KEY / TOPSTEP_API_SECRET / TOPSTEP_ACCOUNT_ID')
            return
        raw = adapter.get_open_positions_raise()
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(str(e))
        return
    positions = []
    for p in raw or []:
        symbol = (p.get('symbol') or '').upper()
        if not symbol:
            continue
        try:
            qty = int(p.get('quantity') or 0)
        except (TypeError, ValueError):
            continue
        if qty == 0:
            continue
        try:
            avg_price = float(p.get('avg_price') or 0)
        except (TypeError, ValueError):
            avg_price = 0.0
        side = (p.get('side') or ('long' if qty > 0 else 'short')).lower()
        positions.append({'coin': symbol, 'size': qty, 'avg_price': avg_price, 'side': side})
    print(json.dumps({'positions': positions, 'platform': 'topstep', 'timestamp': datetime.now(timezone.utc).isoformat()}))

def _emit_error(message):
    print(json.dumps({'positions': [], 'platform': 'topstep', 'timestamp': datetime.now(timezone.utc).isoformat(), 'error': message}))
    sys.exit(1)
if __name__ == '__main__':
    main()
