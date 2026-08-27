import argparse
import collections
import json
import os
import sys
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, '..', '..'))
sys.path.insert(0, os.path.join(_HERE, '..', '..', '..', 'shared_tools'))
from optimizer import DEFAULT_CLOSE_STACK_SPECS, generate_close_stack_grid, walk_forward_optimize
STRATEGY = 'squeeze_momentum'
DATASETS = [('BTC/USDT', '1h'), ('ETH/USDT', '1h'), ('SOL/USDT', '1h')]
START, END = ('2023-01-01', '2026-01-01')
N_SPLITS = 5

def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument('--json', default=None, dest='json_out')
    args = p.parse_args(argv)
    from data_fetcher import load_cached_data
    from registry_loader import load_registry
    reg = load_registry('spot')
    defaults = reg.STRATEGY_REGISTRY[STRATEGY]['default_params']
    frozen = {k: [v] for k, v in defaults.items()}
    grid = generate_close_stack_grid(DEFAULT_CLOSE_STACK_SPECS)
    out = {}
    for symbol, tf in DATASETS:
        df = load_cached_data(symbol, tf, start_date=START, end_date=END)
        res = walk_forward_optimize(df, STRATEGY, frozen, n_splits=N_SPLITS, optimize_metric='dd_adjusted_return', symbol=symbol, timeframe=tf, registry='spot', close_stack_grid=grid, verbose=False)
        folds = res.get('window_results') or []
        picks = [w['best_close_stack'] for w in folds]
        out[f'{symbol} {tf}'] = {'folds': folds, 'picks': picks}
        print(f'\n{symbol} {tf}: fold winners (train-selected by DDadj):')
        for w in folds:
            t = w.get('test_result') or {}
            print(f"  fold {w['fold']}: {w['best_close_stack']:<48} test ret {t.get('total_return_pct')}%  dd {t.get('max_drawdown_pct')}%")
        print('  most common:', collections.Counter(picks).most_common(3))
    if args.json_out:
        with open(args.json_out, 'w') as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f'\nwrote {args.json_out}')
    return 0
if __name__ == '__main__':
    sys.exit(main())
