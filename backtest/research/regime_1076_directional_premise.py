from __future__ import annotations
import os
import sys
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_BACKTEST = os.path.abspath(os.path.join(_THIS_DIR, '..'))
_ROOT = os.path.abspath(os.path.join(_BACKTEST, '..'))
for _p in (_BACKTEST, _ROOT, os.path.join(_ROOT, 'shared_tools')):
    if _p not in sys.path:
        sys.path.insert(0, _p)
import numpy as np
import pandas as pd
from regime import compute_regime, compute_regime_composite, composite_feature_matrix, _DEFAULT_COMPOSITE_THRESHOLDS
from data_fetcher import load_cached_data
from eval_windows import WINDOWS, PLATFORM
from regime_diagnostics import forward_returns, separation, stability, per_state_significance
from regime_stats import benjamini_hochberg
HELD_OUT_FORWARD = ('is', 'oos')
DEFAULT_SYMBOLS = ('BTC/USDT', 'ETH/USDT', 'SOL/USDT')
DEFAULT_TIMEFRAMES = ('1h', '4h')
DEFAULT_WINDOWS = ('is', 'oos', '2023', '2024', '2025H1')
DEFAULT_HORIZONS = (1, 4, 8, 12, 24, 48, 72)
DEFAULT_CLASSIFIERS = ('adx', 'composite')
COMPOSITE_PERIOD = 48
ADX_PERIOD = 14
ADX_THRESHOLD = 20.0

def parse_symbol_spec(spec: str) -> tuple[str, str | None]:
    raw = (spec or '').strip()
    if not raw:
        raise ValueError('empty symbol spec')
    head, sep, tail = raw.rpartition('@')
    if not sep:
        return (raw, None)
    symbol = head.strip()
    exchange = tail.strip()
    if not symbol:
        raise ValueError(f"symbol spec {spec!r} has an empty symbol before '@'")
    if not exchange:
        raise ValueError(f"symbol spec {spec!r} has an empty exchange after '@'")
    return (symbol, exchange)

def parse_symbols_arg(raw: str) -> tuple[tuple[str, str | None], ...]:
    specs = tuple((parse_symbol_spec(part) for part in (raw or '').split(',') if part.strip()))
    if not specs:
        raise ValueError('--symbols resolved to no symbols')
    seen: dict = {}
    for symbol, exchange in specs:
        if symbol in seen:
            raise ValueError(f"duplicate symbol {symbol!r} in --symbols (sources {seen[symbol] or 'default'} and {exchange or 'default'}); every symbol-keyed surface downstream is a dict, so the two entries would merge into one recorded series")
        seen[symbol] = exchange
    return specs

def normalize_symbol_specs(symbols) -> tuple[tuple[str, str | None], ...]:
    out = []
    for entry in symbols:
        if isinstance(entry, str):
            out.append((entry, None))
            continue
        symbol, exchange = entry
        out.append((str(symbol), None if exchange is None else str(exchange)))
    return tuple(out)

def resolve_data_sources(symbols) -> dict:
    return {symbol: exchange or PLATFORM for symbol, exchange in normalize_symbol_specs(symbols)}

def _clip_window(df, start, end):
    if df is None or len(df) == 0:
        return df
    if not isinstance(df.index, pd.DatetimeIndex):
        raise ValueError(f'OHLCV frame must carry a DatetimeIndex to clip to an eval window; got {type(df.index).__name__}')
    if start is not None:
        df = df[df.index >= pd.Timestamp(start)]
    if end is not None:
        df = df[df.index <= pd.Timestamp(end)]
    return df

def coverage_table(rows) -> list:
    agg: dict = {}
    for r in rows:
        key = (str(r['symbol']), str(r['timeframe']), str(r['window']))
        e = agg.setdefault(key, {'symbol': key[0], 'timeframe': key[1], 'window': key[2], 'source': str(r.get('source', '')), 'rows': 0, 'bars': {}})
        e['rows'] += 1
        e['bars'][str(r['classifier'])] = int(r.get('n_bars', 0) or 0)
    return [agg[k] for k in sorted(agg)]

def _policy_direction(label: str) -> int:
    if label.startswith('trending_up'):
        return +1
    if label.startswith('trending_down'):
        return -1
    return 0

def _label_stream(close_df, classifier, th):
    if classifier == 'composite':
        labels = compute_regime_composite(close_df, period=COMPOSITE_PERIOD, thresholds=th)['regime'].to_numpy()
        features = composite_feature_matrix(close_df, COMPOSITE_PERIOD, th).to_numpy()
        valid = ~np.isnan(features).any(axis=1)
    elif classifier == 'adx':
        labels = compute_regime(close_df, period=ADX_PERIOD, adx_threshold=ADX_THRESHOLD)['regime'].to_numpy()
        valid = np.ones(len(labels), dtype=bool)
        valid[:ADX_PERIOD] = False
    else:
        raise SystemExit(f'unknown classifier {classifier!r}')
    return (close_df['close'].to_numpy(), labels, valid)

def _load(symbol, timeframe, window, classifier, th, exchange=None):
    start, end = WINDOWS[window]
    df = load_cached_data(symbol, timeframe, exchange_id=exchange or PLATFORM, start_date=start, end_date=end)
    df = _clip_window(df, start, end)
    if len(df) <= max(COMPOSITE_PERIOD, ADX_PERIOD) + 5:
        return None
    close, labels, valid = _label_stream(df, classifier, th)
    vlabels = labels[valid]
    st = stability(vlabels)
    mean_dwell = float(np.mean(list(st['mean_dwell'].values()))) if st['mean_dwell'] else 1.0
    return {'close': close, 'valid': valid, 'vlabels': vlabels, 'mean_dwell': mean_dwell, 'n_bars': int(np.count_nonzero(valid))}

def run(symbols, timeframes, windows, horizons, classifiers, th, n_perm, seed):
    rows = []
    specs = normalize_symbol_specs(symbols)
    for classifier in classifiers:
        for symbol, exchange in specs:
            source = exchange or PLATFORM
            for timeframe in timeframes:
                for window in windows:
                    d = _load(symbol, timeframe, window, classifier, th, exchange=exchange)
                    if d is None:
                        continue
                    for h in horizons:
                        fwd = forward_returns(d['close'], h)[d['valid']]
                        if np.isnan(fwd).all():
                            continue
                        bl = max(int(3 * d['mean_dwell']), h)
                        per_state = per_state_significance(d['vlabels'], fwd, bl, n_perm=n_perm, seed=seed)
                        sep = separation(d['vlabels'], fwd)['per_state']
                        for state, r in per_state.items():
                            pol = _policy_direction(state)
                            gap = float(r['gap'])
                            aligned = bool(pol != 0 and np.sign(gap) == pol)
                            rows.append({'classifier': classifier, 'symbol': symbol, 'source': source, 'n_bars': int(d['n_bars']), 'timeframe': timeframe, 'window': window, 'horizon': int(h), 'state': str(state), 'gap': gap, 'mean_fwd': float(sep.get(state, {}).get('mean', float('nan'))), 'p_value': float(r['p_value']), 'fdr_reject': bool(r['fdr_reject']), 'policy_dir': int(pol), 'sign_aligned': aligned, 'candidate_edge': bool(r['fdr_reject'] and aligned)})
    return rows

def _report_coverage(rows, symbols=None, timeframes=None, windows=None):
    cov = coverage_table(rows)
    print('SCREENED COVERAGE — (symbol, tf, window) cells that contributed rows:')
    print(f"{'symbol':18s} {'source':12s} {'tf':4s} {'window':8s} {'rows':>5s}  labeled bars")
    print('-' * 78)
    for e in cov:
        bars = ' '.join((f'{c}={n}' for c, n in sorted(e['bars'].items())))
        print(f"{e['symbol']:18s} {e['source']:12s} {e['timeframe']:4s} {e['window']:8s} {e['rows']:5d}  {bars}")
    if symbols is not None and timeframes is not None and (windows is not None):
        present = {(e['symbol'], e['timeframe'], e['window']) for e in cov}
        missing = []
        for symbol, _exchange in normalize_symbol_specs(symbols):
            for tf in timeframes:
                for w in windows:
                    if (symbol, tf, w) not in present:
                        missing.append((symbol, tf, w))
        if missing:
            print('\nWindows that contributed NO rows (too few bars after clipping to the window — e.g. the asset was not listed yet):')
            for symbol, tf, w in missing:
                print(f'  {symbol:18s} {tf:4s} {w}')
    print()

def report(rows, classifiers, symbols=None, timeframes=None, windows=None):
    directional = [r for r in rows if r['policy_dir'] != 0]
    n_dir = len(directional)
    n_fdr = sum((r['fdr_reject'] for r in directional))
    candidates = [r for r in directional if r['candidate_edge']]
    print('=' * 78)
    print('PER-STATE DIRECTIONAL SIGNIFICANCE SUMMARY (#1076 scope 1)')
    print('=' * 78)
    print(f'directional-state tests (trending_up*/trending_down* only): {n_dir}')
    print(f'  FDR-significant (any sign):                {n_fdr}')
    print(f'  FDR-significant AND policy-sign-aligned:   {len(candidates)}  <- candidate edges')
    print()
    _report_coverage(rows, symbols, timeframes, windows)
    for c in classifiers:
        cr = [r for r in directional if r['classifier'] == c]
        if not cr:
            continue
        cf = sum((r['fdr_reject'] for r in cr))
        cc = sum((r['candidate_edge'] for r in cr))
        wrong = sum((r['fdr_reject'] and (not r['sign_aligned']) for r in cr))
        print(f'[{c:9s}] {len(cr):4d} tests | FDR-sig {cf:3d} (aligned {cc}, wrong-signed {wrong})')
    print()
    pvals = [r['p_value'] for r in directional]
    n = len(pvals)
    global_bh = benjamini_hochberg(pvals, alpha=0.05) if pvals else []
    bonf_thresh = 0.05 / n if n else 0.0
    n_global_bh = sum(global_bh)
    n_bonf = sum((p <= bonf_thresh for p in pvals))
    bh_aligned = sum((b and r['sign_aligned'] for b, r in zip(global_bh, directional)))
    bonf_aligned = sum((r['p_value'] <= bonf_thresh and r['sign_aligned'] for r in directional))
    print(f'GLOBAL multiple-comparisons correction across ALL {n} directional-state tests:')
    print(f'  Benjamini-Hochberg FDR q=0.05:  {n_global_bh:3d} survive ({bh_aligned} policy-aligned)')
    print(f'  Bonferroni  (p<= {bonf_thresh:.2e}): {n_bonf:3d} survive ({bonf_aligned} policy-aligned)')
    print()
    held = [r for r in candidates if r['window'] in HELD_OUT_FORWARD]
    oos = [r for r in candidates if r['window'] == 'oos']
    print('Within-cell candidate edges by window class:')
    print(f'  held-out forward (is/oos): {len(held):2d}    of which oos(2026-): {len(oos):2d}')
    print(f'  historical (2023/2024/2025H1): {len(candidates) - len(held):2d}')
    print()
    if candidates:
        print('WITHIN-CELL candidate edges (FDR-sig + policy-aligned; NOT globally corrected):')
        print(f"{'clf':10s} {'sym':9s} {'tf':4s} {'win':7s} {'h':>3s} {'state':22s} {'gap':>10s} {'mean_fwd':>10s} {'p':>7s} {'gBH':>4s}")
        print('-' * 100)
        bh_set = {id(r) for b, r in zip(global_bh, directional) if b}
        for r in sorted(candidates, key=lambda x: x['p_value']):
            print(f"{r['classifier']:10s} {r['symbol']:9s} {r['timeframe']:4s} {r['window']:7s} {r['horizon']:3d} {r['state']:22s} {r['gap']:10.5f} {r['mean_fwd']:10.5f} {r['p_value']:7.3f} {('Y' if id(r) in bh_set else '.'):>4s}")
        print('(gBH = survives GLOBAL Benjamini-Hochberg across the whole battery.)')
    else:
        print('NO within-cell candidate edges on any tested cell. The premise holds nowhere here.')
    print()

def build_parser():
    import argparse
    p = argparse.ArgumentParser(description='#1076 scope-1: regime->direction premise screen')
    p.add_argument('--symbols', default=','.join(DEFAULT_SYMBOLS), help=f"comma-separated SYMBOL[@exchange] specs. A bare symbol loads from the default data source ({PLATFORM}); an @exchange suffix loads that symbol from that exchange's cache namespace instead (#1443, e.g. HYPE/USDC:USDC@hyperliquid). The default source is never repointed (#1315 axis separation).")
    p.add_argument('--timeframes', default=','.join(DEFAULT_TIMEFRAMES))
    p.add_argument('--windows', default=','.join(DEFAULT_WINDOWS), help=f"comma-separated; known: {', '.join(WINDOWS)}")
    p.add_argument('--horizons', default=','.join((str(h) for h in DEFAULT_HORIZONS)))
    p.add_argument('--classifiers', default=','.join(DEFAULT_CLASSIFIERS), help='comma-separated subset of: adx, composite')
    p.add_argument('--n-perm', type=int, default=500)
    p.add_argument('--seed', type=int, default=0)
    p.add_argument('--out', default='', help='optional path to dump all result rows as JSON')
    return p

def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    th = dict(_DEFAULT_COMPOSITE_THRESHOLDS)
    try:
        symbols = parse_symbols_arg(args.symbols)
    except ValueError as exc:
        raise SystemExit(f'--symbols: {exc}')
    timeframes = tuple((t.strip() for t in args.timeframes.split(',') if t.strip()))
    windows = tuple((w.strip() for w in args.windows.split(',') if w.strip()))
    for w in windows:
        if w not in WINDOWS:
            raise SystemExit(f'unknown window {w}; known: {list(WINDOWS)}')
    horizons = tuple((int(h) for h in args.horizons.split(',')))
    classifiers = tuple((c.strip() for c in args.classifiers.split(',') if c.strip()))
    sources = resolve_data_sources(symbols)
    print(f'# universe: {sorted(sources)} x {list(timeframes)} x {list(windows)}')
    print('# data sources: ' + ' '.join((f'{sym}={src}' for sym, src in sorted(sources.items()))))
    print(f'# classifiers={list(classifiers)} horizons={list(horizons)} n_perm={args.n_perm} default_platform={PLATFORM}\n')
    rows = run(symbols, timeframes, windows, horizons, classifiers, th, args.n_perm, args.seed)
    report(rows, classifiers, symbols=symbols, timeframes=timeframes, windows=windows)
    if args.out:
        import json
        with open(args.out, 'w') as fh:
            json.dump({'universe': {'symbols': sorted(sources), 'timeframes': list(timeframes), 'windows': list(windows), 'horizons': list(horizons), 'classifiers': list(classifiers), 'n_perm': args.n_perm, 'default_platform': PLATFORM, 'data_sources': sources}, 'coverage': coverage_table(rows), 'rows': rows}, fh, indent=2)
        print(f'# wrote {len(rows)} rows -> {args.out}')
    return 0
if __name__ == '__main__':
    raise SystemExit(main())
