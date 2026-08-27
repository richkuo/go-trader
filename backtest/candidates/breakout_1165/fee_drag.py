import argparse
import importlib.util
import json
import os
import sys
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, _HERE)
sys.path.insert(0, os.path.join(_HERE, '..', '..'))
sys.path.insert(0, os.path.join(_HERE, '..', '..', '..', 'shared_tools'))
from driver_common import candidate_leg_kwargs
DEFAULT_CANDIDATES = 'baseline.json'

def _load_984_fee_drag():
    path = os.path.join(_HERE, '..', 'breakout_984', 'fee_drag.py')
    spec = importlib.util.spec_from_file_location('breakout_984_fee_drag', path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod
summarize_fee_drag = _load_984_fee_drag().summarize_fee_drag

def main(argv=None):
    p = argparse.ArgumentParser()
    p.add_argument('--start', default='2025-06-10', help='window start (default: the #956 audit start)')
    p.add_argument('--end', default=None, help='window end (default: latest cache)')
    p.add_argument('--candidates', default=DEFAULT_CANDIDATES, help='comma list of candidate JSON files in this dir')
    p.add_argument('--json', default=None, dest='json_out')
    args = p.parse_args(argv)
    from eval_windows import DATASETS, dataset_key, run_leg, validate_candidate
    from registry_loader import load_registry
    reg = load_registry('futures')
    window = (args.start, args.end)
    out = {'window': {'start': args.start, 'end': args.end}, 'candidates': {}}
    for fn in [c.strip() for c in args.candidates.split(',') if c.strip()]:
        with open(os.path.join(_HERE, fn)) as fh:
            candidate = validate_candidate(json.load(fh))
        label = fn[:-5] if fn.endswith('.json') else fn
        common = candidate_leg_kwargs(candidate)
        gross_legs, net_legs, per_dataset = ([], [], {})
        for symbol, tf in DATASETS:
            net = run_leg(reg, candidate['name'], candidate.get('params'), symbol, tf, window, **common)
            gross = run_leg(reg, candidate['name'], candidate.get('params'), symbol, tf, window, **common, commission_pct=0.0, slippage_pct=0.0)
            net_legs.append(net)
            gross_legs.append(gross)
            per_dataset[dataset_key(symbol, tf)] = {'gross_return_pct': None if gross is None else gross['return_pct'], 'net_return_pct': None if net is None else net['return_pct'], 'trades': None if net is None else net['trades']}
        summary = summarize_fee_drag(gross_legs, net_legs)
        out['candidates'][label] = {'summary': summary, 'per_dataset': per_dataset}
        s = summary or {}
        print(f"{label:<22} gross {s.get('mean_gross_return_pct')!s:>8}%  net {s.get('mean_net_return_pct')!s:>8}%  drag {s.get('drag_pp')!s:>6}pp  #T {s.get('trades')!s:>5}  T/yr {s.get('trades_per_year')!s:>6}")
    if args.json_out:
        with open(args.json_out, 'w') as fh:
            json.dump(out, fh, indent=2, default=str)
        print(f'\nwrote {args.json_out}')
    return 0
if __name__ == '__main__':
    sys.exit(main())
