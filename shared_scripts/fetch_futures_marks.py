import json
import os
import sys
import traceback

def main():
    symbols = sys.argv[1:]
    if not symbols:
        print(json.dumps({}))
        return
    try:
        sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'platforms', 'topstep'))
        from adapter import TopStepExchangeAdapter
    except Exception as e:
        print(f'[WARN][fetch_futures_marks] adapter import failed: {e}', file=sys.stderr)
        traceback.print_exc(file=sys.stderr)
        print(json.dumps({}))
        sys.exit(1)
    if os.environ.get('TOPSTEP_API_KEY') and os.environ.get('TOPSTEP_API_SECRET') and os.environ.get('TOPSTEP_ACCOUNT_ID'):
        mode = 'live'
    else:
        mode = 'paper'
    effective_mode = mode
    try:
        adapter = TopStepExchangeAdapter(mode=mode)
    except Exception as e:
        print(f'[WARN][fetch_futures_marks] {mode} mode init failed ({e}); falling back to paper', file=sys.stderr)
        try:
            adapter = TopStepExchangeAdapter(mode='paper')
            effective_mode = 'paper_fallback'
        except Exception as e2:
            print(f'[WARN][fetch_futures_marks] paper fallback failed: {e2}', file=sys.stderr)
            print(json.dumps({}))
            sys.exit(1)
    marks: 'dict[str, float | str]' = {}
    for symbol in symbols:
        try:
            price = adapter.get_price(symbol)
            if price and price > 0:
                marks[symbol] = float(price)
            else:
                print(f'[WARN][fetch_futures_marks] no price for {symbol}', file=sys.stderr)
        except Exception as e:
            print(f'[WARN][fetch_futures_marks] get_price({symbol}) failed: {e}', file=sys.stderr)
    marks['_mode'] = effective_mode
    print(json.dumps(marks))
if __name__ == '__main__':
    main()
