from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
for _p in (_THIS_DIR, os.path.abspath(os.path.join(_THIS_DIR, '..')), os.path.abspath(os.path.join(_THIS_DIR, '..', 'shared_tools'))):
    if _p not in sys.path:
        sys.path.insert(0, _p)
SEPARATION_TOLERANCE = 0.05
STABILITY_MIN_GAIN = 0.02
SIGNIFICANCE_ALPHA = 0.05

def gate_verdict(handrule_report: dict, model_report: dict, primary: str='h4') -> dict:
    hr_h = handrule_report[primary]['separation']['kruskal_h']
    md_h = model_report[primary]['separation']['kruskal_h']
    hr_p = handrule_report[primary]['significance']['p_value']
    md_p = model_report[primary]['significance']['p_value']
    hr_tr = handrule_report['stability']['transition_rate']
    md_tr = model_report['stability']['transition_rate']
    target = model_report.get('target') or handrule_report.get('target')
    model_separation_real = md_p <= SIGNIFICANCE_ALPHA
    incumbent_trustworthy = hr_p <= SIGNIFICANCE_ALPHA
    separation_ok = md_h >= hr_h * (1.0 - SEPARATION_TOLERANCE) and model_separation_real
    stability_ok = hr_tr - md_tr >= STABILITY_MIN_GAIN
    ship = separation_ok and stability_ok
    return {'target': target, 'gate_semantics': 'candidate-self-v2 (#1211)', 'separation_ok': bool(separation_ok), 'stability_ok': bool(stability_ok), 'model_separation_real': bool(model_separation_real), 'incumbent_trustworthy': bool(incumbent_trustworthy), 'ship': bool(ship), 'detail': {'handrule_kruskal_h': hr_h, 'model_kruskal_h': md_h, 'handrule_p_value': hr_p, 'model_p_value': md_p, 'handrule_transition_rate': hr_tr, 'model_transition_rate': md_tr}}

def fit_on_window(symbol, timeframe, window, *, period=48, filter_window=64):
    from regime import compute_regime_composite, composite_feature_matrix, VALID_LABELS_COMPOSITE, _DEFAULT_COMPOSITE_THRESHOLDS
    from regime_hmm import fit_label_anchored_hmm
    from data_fetcher import load_cached_data
    from eval_windows import WINDOWS, PLATFORM
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    feats = composite_feature_matrix(df, period, th).to_numpy()
    labels = compute_regime_composite(df, period=period, thresholds=th)['regime'].to_numpy()
    states = sorted(VALID_LABELS_COMPOSITE)
    model = fit_label_anchored_hmm(feats, labels, states, filter_window=filter_window, fitted_on={'symbol': symbol, 'timeframe': timeframe, 'window': window})
    model['period'] = period
    return model

def build_parser():
    import argparse
    from eval_windows import WINDOWS
    p = argparse.ArgumentParser(description='Fit + validate the regime HMM (#1065)')
    p.add_argument('--symbol', default='BTC/USDT')
    p.add_argument('--timeframe', default='1h')
    p.add_argument('--in-sample', default='is', help=f"known: {', '.join(WINDOWS)}")
    p.add_argument('--held-out', default='oos')
    p.add_argument('--period', type=int, default=48)
    p.add_argument('--filter-window', type=int, default=64)
    p.add_argument('--target', default='volatility', choices=('returns', 'volatility'), help='forward variable the gate scores separation on (default: volatility, #1078)')
    p.add_argument('--seed', type=int, default=0)
    p.add_argument('--out', default=None, help='write fitted model JSON here')
    p.add_argument('--json', default=None, help='write validation report JSON here')
    return p

def main(argv=None) -> int:
    import json
    from regime_diagnostics import run_window
    args = build_parser().parse_args(argv)
    model = fit_on_window(args.symbol, args.timeframe, args.in_sample, period=args.period, filter_window=args.filter_window)
    if args.out:
        with open(args.out, 'w') as fh:
            json.dump({'model': model}, fh, indent=2, default=float)
    hr = run_window(args.symbol, args.timeframe, args.held_out, model=None, seed=args.seed, target=args.target)
    md = run_window(args.symbol, args.timeframe, args.held_out, model=model, seed=args.seed, target=args.target)
    verdict = gate_verdict(hr, md)
    payload = {'symbol': args.symbol, 'timeframe': args.timeframe, 'target': args.target, 'in_sample': args.in_sample, 'held_out': args.held_out, 'verdict': verdict, 'handrule': hr, 'model': md}
    text = json.dumps(payload, indent=2, default=float)
    if args.json:
        with open(args.json, 'w') as fh:
            fh.write(text)
    print(text)
    return 0
if __name__ == '__main__':
    raise SystemExit(main())
