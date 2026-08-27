import argparse
import json
import os
import sys
import time
import traceback
from datetime import datetime, timezone
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'hyperliquid'))
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
from hl_user_fills import apply_user_fills_lookup

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--symbol', required=True)
    parser.add_argument('--mode', default='live')
    parser.add_argument('--sz', type=float, default=None, help='partial close size in coin units (omit for full position)')
    parser.add_argument('--cancel-stop-loss-oid', type=int, action='append', default=[], help='cancel this trigger OID; repeat for shared-coin triggers (#421)')
    parser.add_argument('--cancel-protection-after-close', action='store_true', help='cancel trigger OIDs only after the close fill covers the requested size')
    args = parser.parse_args()
    if args.mode != 'live':
        print(json.dumps({'close': None, 'platform': 'hyperliquid', 'timestamp': datetime.now(timezone.utc).isoformat(), 'error': '--mode=live required for emergency close'}))
        sys.exit(1)
    cancel_err = ''
    cancel_succeeded = False
    cancel_succeeded_oids = []
    cancel_failed_oids = []
    try:
        from adapter import HyperliquidExchangeAdapter
        adapter = HyperliquidExchangeAdapter()
        if not args.cancel_protection_after_close:
            cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids = _cancel_trigger_orders(adapter, args.symbol, args.cancel_stop_loss_oid)
        fills_since_ms = int(time.time() * 1000) - 10000
        result = adapter.market_close(args.symbol, args.sz)
    except Exception as e:
        traceback.print_exc(file=sys.stderr)
        _emit_error(args.symbol, str(e), cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    if not isinstance(result, dict):
        _emit_error(args.symbol, f'unexpected SDK response type {type(result).__name__}: {result!r}', cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    outer_status = result.get('status')
    if outer_status not in (None, 'ok'):
        _emit_error(args.symbol, f'sdk status={outer_status!r}: {result}', cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    statuses = result.get('response', {}).get('data', {}).get('statuses', [])
    if not statuses:
        _emit_success(args.symbol, fill={}, already_flat=True, cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    first = statuses[0]
    if 'error' in first:
        _emit_error(args.symbol, f"per-status error: {first['error']}", cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    if 'filled' not in first:
        _emit_error(args.symbol, f'close not filled (status keys={list(first.keys())}): {first}', cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)
        return
    filled = first['filled']
    fill = {'avg_px': float(filled.get('avgPx', 0) or 0), 'total_sz': float(filled.get('totalSz', 0) or 0)}
    if args.cancel_protection_after_close and _fill_covers_requested_size(fill['total_sz'], args.sz):
        cancel_err, cancel_succeeded, cancel_succeeded_oids, cancel_failed_oids = _cancel_trigger_orders(adapter, args.symbol, args.cancel_stop_loss_oid)
    oid = filled.get('oid')
    if oid is not None:
        fill['oid'] = int(oid)
    fee = filled.get('fee')
    if fee is not None:
        fill['fee'] = float(fee)
    if fill.get('oid'):
        try:
            lookup = adapter.lookup_fill_fee_by_oid(fill['oid'], fills_since_ms)
            if not lookup:
                print(f"[WARN] userFills lookup returned no fills for oid={fill['oid']}", file=sys.stderr)
            elif not apply_user_fills_lookup(fill, lookup):
                print(f"[WARN] userFills lookup returned malformed fill data for oid={fill['oid']}", file=sys.stderr)
        except Exception as fe:
            print(f"[WARN] userFills lookup failed for oid={fill['oid']}: {fe}", file=sys.stderr)
    _emit_success(args.symbol, fill, cancel_err=cancel_err, cancel_succeeded=cancel_succeeded, cancel_succeeded_oids=cancel_succeeded_oids, cancel_failed_oids=cancel_failed_oids)

def _cancel_trigger_orders(adapter, symbol, cancel_oids):
    cancel_errors = []
    cancel_succeeded_oids = []
    cancel_failed_oids = []
    for oid in cancel_oids:
        if oid <= 0:
            continue
        try:
            adapter.cancel_trigger_order(symbol, oid)
            cancel_succeeded_oids.append(oid)
        except Exception as ce:
            cancel_failed_oids.append(oid)
            cancel_errors.append(f'{oid}: {ce}')
            print(f'[WARN] cancel_trigger_order({symbol}, {oid}) failed: {ce}', file=sys.stderr)
    return ('; '.join(cancel_errors), bool(cancel_succeeded_oids), cancel_succeeded_oids, cancel_failed_oids)

def _fill_covers_requested_size(total_sz, requested_sz):
    if total_sz <= 0:
        return False
    if requested_sz is None:
        return True
    return total_sz >= requested_sz * 0.99

def _emit_success(symbol, fill, already_flat=False, cancel_err='', cancel_succeeded=False, cancel_succeeded_oids=None, cancel_failed_oids=None):
    close = {'symbol': symbol, 'fill': fill}
    if already_flat:
        close['already_flat'] = True
    out = {'close': close, 'platform': 'hyperliquid', 'timestamp': datetime.now(timezone.utc).isoformat()}
    if cancel_err:
        out['cancel_stop_loss_error'] = cancel_err
    if cancel_succeeded:
        out['cancel_stop_loss_succeeded'] = True
    if cancel_succeeded_oids:
        out['cancel_stop_loss_succeeded_oids'] = cancel_succeeded_oids
    if cancel_failed_oids:
        out['cancel_stop_loss_failed_oids'] = cancel_failed_oids
    print(json.dumps(out))

def _emit_error(symbol, message, cancel_err='', cancel_succeeded=False, cancel_succeeded_oids=None, cancel_failed_oids=None):
    out = {'close': {'symbol': symbol, 'fill': {}}, 'platform': 'hyperliquid', 'timestamp': datetime.now(timezone.utc).isoformat(), 'error': message}
    if cancel_err:
        out['cancel_stop_loss_error'] = cancel_err
    if cancel_succeeded:
        out['cancel_stop_loss_succeeded'] = True
    if cancel_succeeded_oids:
        out['cancel_stop_loss_succeeded_oids'] = cancel_succeeded_oids
    if cancel_failed_oids:
        out['cancel_stop_loss_failed_oids'] = cancel_failed_oids
    print(json.dumps(out))
    sys.exit(1)
if __name__ == '__main__':
    main()
