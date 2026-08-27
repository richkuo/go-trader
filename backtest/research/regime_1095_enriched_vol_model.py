from __future__ import annotations
import os, sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, '..'))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, '..'))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, 'shared_tools')):
    if _p not in sys.path:
        sys.path.insert(0, _p)
import importlib.util
import numpy as np
from regime import compute_regime_composite, _DEFAULT_COMPOSITE_THRESHOLDS
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import score_labels
from regime_calibrate import gate_verdict, SIGNIFICANCE_ALPHA
import regime_vol_model as rvm
from regime_enriched_features import CANONICAL_COLUMNS, ENRICHED_COLUMNS, LIVE_WIRING_DELTA, enriched_feature_matrix, canonical_indices_for, decode_with_model
DEFAULT_WINDOWS = ('is', 'oos', '2023', '2024', '2025H1')
MIN_VALID_BARS = 50
SUBSETS = {'canonical': list(CANONICAL_COLUMNS), 'funding': CANONICAL_COLUMNS + ['funding_rate'], 'volume': CANONICAL_COLUMNS + ['volume_z'], 'htf': CANONICAL_COLUMNS + ['htf_range_eff'], 'all_enriched': list(ENRICHED_COLUMNS)}

def _load_1080():
    path = os.path.join(_THIS_DIR, 'regime_1080_unsupervised_vol_model.py')
    spec = importlib.util.spec_from_file_location('regime_1080_for_1095', path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod

def combined_family_plan(subset_names, families, k_range, thresholds, *, ineligible_reason_fn):
    plan = [(sub, family, k) for sub in subset_names for family in families for k in k_range]
    reasons = {cell: ineligible_reason_fn(cell[2], thresholds) for cell in plan}
    denominator = sum((1 for cell in plan if not reasons[cell]))
    return (plan, reasons, denominator)

def _funding_for_window(coin, window):
    try:
        from funding_fetcher import load_cached_funding
        start, end = WINDOWS[window]
        return load_cached_funding(coin, start, end)
    except Exception:
        return None

def _enriched(symbol, timeframe, window, period, th, columns, funding, *, htf_multiple, vol_window):
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=WINDOWS[window][0], end_date=WINDOWS[window][1])
    mat = enriched_feature_matrix(df, period, th, funding=funding, vol_window=vol_window, htf_multiple=htf_multiple, columns=columns)
    return (df, mat)

def _score(df, labels, mat, *, n_perm, seed, target='volatility'):
    return score_labels(df['close'].to_numpy(), labels, mat.to_numpy(dtype=float), target=target, n_perm=n_perm, seed=seed)

def _valid_count(mat):
    arr = mat.to_numpy(dtype=float)
    return int((~np.isnan(arr).any(axis=1)).sum())

def run_bakeoff(symbol='BTC/USDT', timeframe='1h', *, in_sample='is', held_out='oos', eval_windows=DEFAULT_WINDOWS, families=('hmm', 'gmm', 'kmeans'), k_range=range(2, 8), subsets=None, period=48, filter_window=64, htf_multiple=4, vol_window=None, seed=0, n_perm=None):
    subsets = dict(subsets) if subsets is not None else dict(SUBSETS)
    m1080 = _load_1080()
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    coin = symbol.split('/')[0]
    hr_streams = m1080._handrule_streams(symbol, timeframe, eval_windows, period, th)
    thresholds = rvm.derive_thresholds(list(hr_streams.values()))
    all_windows = list(dict.fromkeys((in_sample, held_out, *eval_windows)))
    funding_by_window = {w: _funding_for_window(coin, w) for w in all_windows}
    built = {}
    subset_status = {}
    for sub_name, columns in subsets.items():
        needs_funding = 'funding_rate' in columns
        wins = {}
        for w in all_windows:
            wins[w] = _enriched(symbol, timeframe, w, period, th, columns, funding_by_window[w], htf_multiple=htf_multiple, vol_window=vol_window)
        if _valid_count(wins[in_sample][1]) < MIN_VALID_BARS or _valid_count(wins[held_out][1]) < MIN_VALID_BARS:
            subset_status[sub_name] = 'unavailable: too few valid bars after warmup' + (' (funding cache/network missing?)' if needs_funding else '')
            continue
        subset_status[sub_name] = 'ok'
        built[sub_name] = wins
    plan, ineligible_reasons, denominator = combined_family_plan(list(built), families, k_range, thresholds, ineligible_reason_fn=m1080.structurally_ineligible_reason)
    alpha = m1080.bonferroni_alpha(denominator)
    n_perm = m1080.resolve_bakeoff_n_perm(denominator, requested=n_perm)
    ineligible_report = [{'subset': s, 'family': f, 'k': k, 'reason': ineligible_reasons[s, f, k]} for s, f, k in plan if ineligible_reasons[s, f, k]]
    for entry in ineligible_report:
        print(f"NOTE: candidate {entry['subset']}:{entry['family']}:k={entry['k']} is structurally ineligible and excluded from the Bonferroni denominator — {entry['reason']}", file=sys.stderr)
    print(f'NOTE: n_perm={n_perm} (min achievable p {1.0 / (n_perm + 1):.6f}) vs Bonferroni-corrected alpha {alpha:.6f} over {denominator} structurally eligible of {len(plan)} swept candidates (ONE family across all {len(built)} available feature subsets)', file=sys.stderr)
    handrule = {}
    for sub_name, wins in built.items():
        held_df, held_mat = wins[held_out]
        hr_labels = compute_regime_composite(held_df, period=period, thresholds=th)['regime'].to_numpy()
        hr_scored = _score(held_df, hr_labels, held_mat, n_perm=n_perm, seed=seed)
        hr_p = hr_scored['h4']['significance']['p_value']
        steps = m1080.permutation_steps_to_alpha(hr_p, n_perm)
        handrule[sub_name] = {'scored': hr_scored, 'report': {'kruskal_h': hr_scored['h4']['separation']['kruskal_h'], 'p_value': hr_p, 'transition_rate': hr_scored['stability']['transition_rate'], 'abstained': bool(hr_p > SIGNIFICANCE_ALPHA), 'permutation_steps_to_alpha': int(steps), 'knife_edge': bool(m1080.verdict_knife_edge(steps))}}
    candidates = []
    for sub_name, family, k in plan:
        wins = built[sub_name]
        fit_df, fit_mat = wins[in_sample]
        held_df, held_mat = wins[held_out]
        columns = list(fit_mat.columns)
        cidx = canonical_indices_for(columns)
        model = rvm.fit_unsupervised(fit_mat.to_numpy(dtype=float), family=family, k=k, filter_window=filter_window, period=period, thresholds=th, seed=seed, feature_names=columns, canonical_indices=cidx, fitted_on={'symbol': symbol, 'timeframe': timeframe, 'window': in_sample, 'subset': sub_name})
        labels, _ = decode_with_model(held_mat, model)
        md = _score(held_df, labels, held_mat, n_perm=n_perm, seed=seed)
        verdict = gate_verdict(handrule[sub_name]['scored'], md)
        nd = {}
        for w in eval_windows:
            wdf, wmat = wins[w]
            wlabels, _ = decode_with_model(wmat, model)
            valid = ~np.isnan(wmat.to_numpy(dtype=float)).any(axis=1)
            nd[w] = rvm.non_degeneracy(np.asarray(wlabels, dtype=object)[valid], thresholds)
        candidates.append({'subset': sub_name, 'columns': columns, 'family': family, 'k': k, 'verdict': verdict, 'model_kruskal_h': md['h4']['separation']['kruskal_h'], 'model_p_value': md['h4']['significance']['p_value'], 'handrule_kruskal_h': handrule[sub_name]['report']['kruskal_h'], 'stability_gain': float(handrule[sub_name]['report']['transition_rate'] - md['stability']['transition_rate']), 'coverage': md['coverage'], 'non_degeneracy': {w: nd[w] for w in eval_windows}, 'non_degenerate_all': all((nd[w]['ok'] for w in eval_windows)), 'structurally_ineligible': bool(ineligible_reasons[sub_name, family, k]), 'structural_ineligibility_reason': ineligible_reasons[sub_name, family, k], 'states': model['states'], 'mapping': model['mapping']})
    for c in candidates:
        c['passes_bonferroni'] = not c['structurally_ineligible'] and c['model_p_value'] is not None and (c['model_p_value'] <= alpha)
    winner = m1080.select_winner(candidates)
    ablation = {}
    for sub_name in subsets:
        elig = [c for c in candidates if c['subset'] == sub_name and c['non_degenerate_all']]
        best = max(elig, key=lambda c: c['model_kruskal_h'], default=None)
        ablation[sub_name] = {'status': subset_status.get(sub_name, 'unavailable'), 'best_kruskal_h': best['model_kruskal_h'] if best else None, 'best_candidate': {'family': best['family'], 'k': best['k']} if best else None, 'any_ships': any((c['subset'] == sub_name and c['verdict'].get('ship') and c['non_degenerate_all'] and c['passes_bonferroni'] for c in candidates))}
    canon_best = ablation.get('canonical', {}).get('best_kruskal_h')
    for sub_name, info in ablation.items():
        info['separation_gain_vs_canonical'] = info['best_kruskal_h'] - canon_best if info['best_kruskal_h'] is not None and canon_best is not None else None
    return {'issue': 1095, 'symbol': symbol, 'timeframe': timeframe, 'in_sample': in_sample, 'held_out': held_out, 'target': 'volatility', 'htf_multiple': htf_multiple, 'candidate_count': len(candidates), 'significance_alpha': SIGNIFICANCE_ALPHA, 'bonferroni_alpha': alpha, 'bonferroni_denominator': denominator, 'bonferroni_denominator_policy': 'ONE family across all feature-subset ablations (#1095 4b): alpha is divided by the COMBINED structurally-eligible candidate count of the whole subsets x families x K grid; structurally ineligible candidates (k < incumbent-derived min_active_labels) are scored for evidence but excluded (#1160)', 'structurally_ineligible': ineligible_report, 'n_perm': int(n_perm), 'min_achievable_p_value': 1.0 / (n_perm + 1), 'non_degeneracy_thresholds': vars(thresholds), 'subset_status': subset_status, 'handrule_held_out': {s: h['report'] for s, h in handrule.items()}, 'baseline_note': "the merged #1080 evidence was measured at n_perm=200 with the incumbent at a knife-edge (OOS p = 10/201); handrule_held_out.canonical re-measures that baseline at this run's resolved n_perm so enriched-vs-baseline and the incumbent-trustworthy verdict share one resolution (#1095 4c)", 'ablation': ablation, 'candidates': candidates, 'winner': {'subset': winner['subset'], 'family': winner['family'], 'k': winner['k']} if winner else None, 'live_wiring_delta': LIVE_WIRING_DELTA}

def build_parser():
    import argparse
    p = argparse.ArgumentParser(description='#1095 enriched unsupervised vol-regime bake-off')
    p.add_argument('--symbol', default='BTC/USDT')
    p.add_argument('--timeframe', default='1h')
    p.add_argument('--in-sample', default='is')
    p.add_argument('--held-out', default='oos')
    p.add_argument('--period', type=int, default=48)
    p.add_argument('--filter-window', type=int, default=64)
    p.add_argument('--htf-multiple', type=int, default=4)
    p.add_argument('--vol-window', type=int, default=None, help='volume z-score window (default: period)')
    p.add_argument('--seed', type=int, default=0)
    p.add_argument('--n-perm', type=int, default=None, help='block-shuffle permutation count for the significance arm (default: auto-resolved so the Bonferroni-corrected alpha over the COMBINED subsets x families x K family is achievable with headroom; explicit values below the achievability floor are rejected)')
    p.add_argument('--json', default=None, help='write the bake-off report JSON here')
    return p

def main(argv=None):
    import json
    args = build_parser().parse_args(argv)
    report = run_bakeoff(args.symbol, args.timeframe, in_sample=args.in_sample, held_out=args.held_out, period=args.period, filter_window=args.filter_window, htf_multiple=args.htf_multiple, vol_window=args.vol_window, seed=args.seed, n_perm=args.n_perm)
    text = json.dumps(report, indent=2, default=float)
    if args.json:
        with open(args.json, 'w') as fh:
            fh.write(text)
    print(text)
    w = report['winner']
    if w:
        print(f'\nWINNER: {w}', file=sys.stderr)
    else:
        print("\nWINNER: none eligible (no gate-passing, non-degenerate, Bonferroni-clearing candidate on this window) — second negative result; see 'ablation' for whether any added feature improved separation even without shipping.", file=sys.stderr)
    return 0
if __name__ == '__main__':
    raise SystemExit(main())
