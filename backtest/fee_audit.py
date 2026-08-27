from __future__ import annotations
import argparse
import json
import os
import statistics
import sys
from typing import List, Optional
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
from eval_windows import DATASETS, DEFAULT_CAPITAL, FEE_PLATFORM, WINDOWS, dataset_key, parse_dataset_arg, run_leg
SKIP_STRATEGIES = {'hold'}
LIVE_BIDIRECTIONAL_STRATEGIES = frozenset({'triple_ema_bidir', 'tema_cross_bd', 'session_breakout', 'donchian_breakout', 'chart_pattern', 'liquidity_sweeps', 'bear_pullback_st', 'vwap_rejection_st', 'momentum_pro', 'mean_reversion_pro', 'rsi_bb_combo', 'consolidation_range', 'mtf_confluence', 'vol_momentum', 'funding_skew', 'regime_adaptive', 'anchored_vwap', 'anchored_vwap_channel', 'anchored_vwap_reversion', 'atr_band_revert'})
DEFAULT_WINDOWS = ('is', 'oos')
YEAR_DAYS = 365.25
VERDICT_ORDER = ('deprecate', 'graduate_m1', 'healthy', 'unscreened_short', 'no_trades')

def _mean(values: List[float]) -> Optional[float]:
    vals = [v for v in values if v is not None]
    return statistics.mean(vals) if vals else None

def strategy_is_short_capable(default_params: Optional[dict], name: str) -> bool:
    if name not in LIVE_BIDIRECTIONAL_STRATEGIES:
        return False
    dp = default_params or {}
    if 'allow_short' in dp:
        return bool(dp['allow_short'])
    return True

def trades_per_year(total_trades: int, total_span_days: float, year_days: float=YEAR_DAYS) -> Optional[float]:
    if not total_span_days or total_span_days <= 0:
        return None
    return total_trades / (total_span_days / year_days)

def salvage_verdict(total_trades: int, mean_gross: Optional[float], mean_net: Optional[float], short_unmeasured: bool=False) -> str:
    if total_trades == 0 or mean_gross is None:
        return 'unscreened_short' if short_unmeasured else 'no_trades'
    if mean_gross <= 0:
        return 'unscreened_short' if short_unmeasured else 'deprecate'
    if mean_net is None or mean_net <= 0:
        return 'graduate_m1'
    return 'healthy'

def aggregate_strategy(strategy: str, registry_label: str, leg_results: List[dict], short_capable: bool=False) -> dict:
    data_legs = [l for l in leg_results if l.get('error') is None and l.get('net_ret') is not None]
    errors = [l for l in leg_results if l.get('error') is not None]
    n_legs = len(data_legs)
    total_trades = sum((int(l['trades']) for l in data_legs))
    total_span = sum((float(l['span_days']) for l in data_legs if l.get('span_days')))
    short_unmeasured = bool(short_capable)
    mean_gross = _mean([l.get('gross_ret') for l in data_legs])
    mean_net = _mean([l.get('net_ret') for l in data_legs])
    mean_sharpe = _mean([l.get('net_sharpe') for l in data_legs])
    n_liquidated = sum((1 for l in data_legs if l.get('liquidated')))
    fee_drag = mean_gross - mean_net if mean_gross is not None and mean_net is not None else None
    tpy = trades_per_year(total_trades, total_span)
    drag_per_trade = fee_drag * n_legs / total_trades if fee_drag is not None and total_trades else None
    verdict = salvage_verdict(total_trades, mean_gross, mean_net, short_unmeasured)
    return {'strategy': strategy, 'registry': registry_label, 'trades': total_trades, 'span_days': round(total_span, 2), 'trades_per_year': round(tpy, 1) if tpy is not None else None, 'mean_gross_ret': round(mean_gross, 3) if mean_gross is not None else None, 'mean_net_ret': round(mean_net, 3) if mean_net is not None else None, 'fee_drag_pp': round(fee_drag, 3) if fee_drag is not None else None, 'drag_per_trade_pp': round(drag_per_trade, 4) if drag_per_trade is not None else None, 'mean_net_sharpe': round(mean_sharpe, 3) if mean_sharpe is not None else None, 'short_unmeasured': short_unmeasured, 'n_legs': n_legs, 'n_liquidated': n_liquidated, 'n_errors': len(errors), 'errors': errors, 'verdict': verdict}

def rank_rows(rows: List[dict]) -> List[dict]:

    def key(r):
        no_trades = r['verdict'] == 'no_trades'
        drag = r['fee_drag_pp'] if r['fee_drag_pp'] is not None else float('-inf')
        return (no_trades, -drag, r['strategy'], r.get('registry', ''))
    return sorted(rows, key=key)

def verdict_counts(rows: List[dict]) -> dict:
    counts = {v: 0 for v in VERDICT_ORDER}
    for r in rows:
        counts[r['verdict']] = counts.get(r['verdict'], 0) + 1
    return counts

def _md_num(v, prec: int=2) -> str:
    if v is None:
        return '—'
    return f'{v:.{prec}f}'

def render_markdown(ranked: List[dict], meta: dict) -> str:
    lines = ['# Fee-aware selectivity audit (#999 M5)', '', 'Registry-wide trade-count x fee-drag screen. Each strategy leg is run twice on the eval_windows.py harness — once with the audit fee model, once with commission and slippage zeroed — to isolate fee drag and apply the salvage test (does a positive *gross* edge exist under the churn?).', '', '## Reproduce', '', '```', meta['command'], '```', '', f"- Generated: {meta.get('date', 'see git history')}", f"- Registries: {meta['registry']}", f"- Windows: {meta['windows_desc']}", f"- Datasets: {meta['datasets_desc']}", f"- Direction: {meta.get('direction', 'long')}", f"- Capital: {meta['capital']}", f'- Fee model: {FEE_PLATFORM} platform taker fee + 5 bps slippage (net); commission=0 and slippage=0 (gross). Fee drag = mean per-leg (gross - net) return.', '', "Returns are mean per-leg total-return %; trades are summed across all scored legs; trades/yr is annualized over the summed calendar span. **Verdicts:** `deprecate` (gross <= 0, no edge to salvage), `graduate_m1` (gross > 0, net <= 0 — raise selectivity), `healthy` (net > 0), `unscreened_short` (emitted short entries the long/flat harness drops — long leg alone can't justify deprecate/no_trades), `no_trades` (never fired). A † flags a row whose short half was unmeasured (verdict reflects the long leg only).", '', '| rank | strategy | reg | trades | trades/yr | gross %/leg | net %/leg | fee drag (pp) | drag/trade (pp) | net Sharpe | verdict |', '|-----:|----------|-----|-------:|----------:|------------:|----------:|--------------:|----------------:|-----------:|---------|']
    for i, r in enumerate(ranked, 1):
        dagger = ' †' if r.get('short_unmeasured') else ''
        lines.append(f"| {i} | {r['strategy']}{dagger} | {r['registry']} | {r['trades']} | {_md_num(r['trades_per_year'], 1)} | {_md_num(r['mean_gross_ret'])} | {_md_num(r['mean_net_ret'])} | {_md_num(r['fee_drag_pp'])} | {_md_num(r['drag_per_trade_pp'], 4)} | {_md_num(r['mean_net_sharpe'])} | `{r['verdict']}` |")
    deprecate = [r for r in ranked if r['verdict'] == 'deprecate']
    graduate = [r for r in ranked if r['verdict'] == 'graduate_m1']
    unscreened = [r for r in ranked if r['verdict'] == 'unscreened_short']
    errored = [r for r in ranked if r['n_errors']]
    lines += ['', '## Deprecation list (gross edge <= 0 — fee filter cannot save)', '']
    if deprecate:
        for r in deprecate:
            lines.append(f"- **{r['strategy']}** ({r['registry']}): gross {_md_num(r['mean_gross_ret'])}%, net {_md_num(r['mean_net_ret'])}%, {r['trades']} trades ({_md_num(r['trades_per_year'], 1)}/yr)")
    else:
        lines.append('- (none)')
    lines += ['', '## M1 graduations (gross > 0, net <= 0 — mechanism: raise selectivity)', '']
    if graduate:
        for r in graduate:
            note = ' — long leg only (short half unscreened)' if r.get('short_unmeasured') else ' — raise selectivity'
            lines.append(f"- **{r['strategy']}** ({r['registry']}): gross {_md_num(r['mean_gross_ret'])}%, net {_md_num(r['mean_net_ret'])}%, fee drag {_md_num(r['fee_drag_pp'])}pp over {r['trades']} trades ({_md_num(r['trades_per_year'], 1)}/yr){note}")
    else:
        lines.append('- (none)')
    lines += ['', '## Unscreened short legs (long/flat harness drops short entries — verdict withheld)', '']
    if unscreened:
        for r in unscreened:
            lines.append(f"- **{r['strategy']}** ({r['registry']}): short-capable (bidirectional / allow_short); the long/flat harness measured only its long leg (gross {_md_num(r['mean_gross_ret'])}%, net {_md_num(r['mean_net_ret'])}% over {r['trades']} long trades). Re-screen via the open/close engine (models both sides) before any deprecate/graduate call.")
    else:
        lines.append('- (none)')
    liquidated = [r for r in ranked if r.get('n_liquidated')]
    if liquidated:
        lines += ['', '## Liquidated legs (equity hit 0 — metrics floored at the bust bar, #1005)', '']
        for r in liquidated:
            lines.append(f"- **{r['strategy']}** ({r['registry']}): {r['n_liquidated']}/{r['n_legs']} leg(s) liquidated — return/DD read −100% for those legs; means above include the floored values")
    if errored:
        lines += ['', '## Errors / skips', '']
        for r in errored:
            reasons = sorted({e.get('error', '?') for e in r['errors']})
            lines.append(f"- **{r['strategy']}** ({r['registry']}): {r['n_errors']} errored leg(s) — {'; '.join(reasons)}")
    return '\n'.join(lines) + '\n'

def screen_leg(reg, name: str, symbol: str, timeframe: str, window: tuple, capital: float, direction: Optional[str]=None) -> Optional[dict]:
    try:
        net = run_leg(reg, name, None, symbol, timeframe, window, capital=capital, direction=direction)
    except Exception as exc:
        return {'dataset': dataset_key(symbol, timeframe), 'error': f'net: {exc}'}
    if net is None:
        return None
    try:
        gross = run_leg(reg, name, None, symbol, timeframe, window, capital=capital, direction=direction, commission_pct=0.0, slippage_pct=0.0)
    except Exception as exc:
        return {'dataset': dataset_key(symbol, timeframe), 'error': f'gross: {exc}'}
    if gross is None:
        return None
    if int(net['trades']) != int(gross['trades']):
        return {'dataset': dataset_key(symbol, timeframe), 'error': f"net/gross trade-count mismatch ({net['trades']} vs {gross['trades']}) — runs not comparable"}
    return {'dataset': dataset_key(symbol, timeframe), 'error': None, 'trades': net['trades'], 'span_days': net['span_days'], 'net_ret': net['return_pct'], 'gross_ret': gross['return_pct'], 'net_sharpe': net['sharpe'], 'liquidated': bool(net.get('liquidated') or gross.get('liquidated'))}

def screen_strategy(reg, name: str, registry_label: str, datasets: List[tuple], window_names: List[str], capital: float, direction: Optional[str]=None) -> dict:
    leg_results = []
    for wname in window_names:
        window = WINDOWS[wname]
        for symbol, timeframe in datasets:
            leg = screen_leg(reg, name, symbol, timeframe, window, capital, direction=direction)
            if leg is None:
                continue
            leg['window'] = wname
            leg_results.append(leg)
    short_capable = strategy_is_short_capable(_default_params(reg, name), name) and direction != 'short'
    return aggregate_strategy(name, registry_label, leg_results, short_capable)

def _default_params(reg, name) -> Optional[dict]:
    entry = reg.STRATEGY_REGISTRY.get(name) or {}
    return entry.get('default_params')

def enumerate_targets(registry_choice: str, subset: Optional[List[str]]=None) -> List[tuple]:
    from registry_loader import load_registry
    targets: List[tuple] = []
    seen = set()
    spot_reg = None
    if registry_choice in ('spot', 'both'):
        spot_reg = load_registry('spot')
        for n in spot_reg.STRATEGY_REGISTRY:
            if n in SKIP_STRATEGIES:
                continue
            targets.append((n, 'spot', spot_reg))
            seen.add(n)
    if registry_choice in ('futures', 'both'):
        fut_reg = load_registry('futures')
        for n in fut_reg.STRATEGY_REGISTRY:
            if n in SKIP_STRATEGIES:
                continue
            if registry_choice == 'both' and n in seen:
                if _default_params(spot_reg, n) == _default_params(fut_reg, n):
                    continue
            targets.append((n, 'futures', fut_reg))
    if subset:
        want = {s.strip() for s in subset if s.strip()}
        targets = [t for t in targets if t[0] in want]
        missing = want - {t[0] for t in targets}
        if missing:
            raise SystemExit(f'unknown strategies for registry={registry_choice!r}: {sorted(missing)}')
    return targets

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description='Registry-wide trade-count x fee-drag selectivity screen (#999 M5)')
    p.add_argument('--registry', choices=['spot', 'futures', 'both'], default='both', help='Registries to screen (default: both — futures-only names appended)')
    p.add_argument('--strategies', default=None, help='Optional comma list to restrict the screen (e.g. M5 candidates)')
    p.add_argument('--windows', default=None, help=f"Comma list of windows (default: {','.join(DEFAULT_WINDOWS)}). Known: {', '.join(WINDOWS)}")
    p.add_argument('--datasets', default=None, help='Comma list of SYMBOL:TIMEFRAME (default: the six audit datasets)')
    p.add_argument('--direction', default=None, choices=['long', 'short'], help='Entry side to screen. Default is the historical long/flat harness; pass short to measure a short leg (signal=-1 opens, +1 closes) instead of withholding short-capable rows as unscreened.')
    p.add_argument('--capital', type=float, default=DEFAULT_CAPITAL)
    p.add_argument('--json', default=None, dest='json_out', help='Write the full structured result to this path')
    p.add_argument('--markdown', default=None, dest='markdown_out', help='Write/overwrite the committed report table at this path')
    return p

def _resolve_windows(arg: Optional[str]) -> List[str]:
    if not arg:
        return list(DEFAULT_WINDOWS)
    names = [w.strip() for w in arg.split(',') if w.strip()]
    unknown = [w for w in names if w not in WINDOWS]
    if unknown:
        raise SystemExit(f'unknown windows {unknown}; known: {list(WINDOWS)}')
    return names

def main(argv: Optional[List[str]]=None) -> int:
    args = build_parser().parse_args(argv)
    window_names = _resolve_windows(args.windows)
    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(',') if d.strip()]
    else:
        datasets = list(DATASETS)
    subset = args.strategies.split(',') if args.strategies else None
    targets = enumerate_targets(args.registry, subset)
    print(f'screening {len(targets)} strategies x {len(datasets)} datasets x {len(window_names)} windows (net + gross runs each)')
    rows = []
    for idx, (name, label, reg) in enumerate(targets, 1):
        print(f'  [{idx}/{len(targets)}] {name} ({label}) ...', flush=True)
        rows.append(screen_strategy(reg, name, label, datasets, window_names, args.capital, direction=args.direction))
    ranked = rank_rows(rows)
    counts = verdict_counts(ranked)
    print(f"\n{'#':>3}  {'strategy':<22} {'reg':<7} {'trades':>7} {'tr/yr':>7} {'gross%':>8} {'net%':>8} {'drag(pp)':>9}  verdict")
    for i, r in enumerate(ranked, 1):
        print(f"{i:>3}  {r['strategy']:<22} {r['registry']:<7} {r['trades']:>7} {_md_num(r['trades_per_year'], 1):>7} {_md_num(r['mean_gross_ret']):>8} {_md_num(r['mean_net_ret']):>8} {_md_num(r['fee_drag_pp']):>9}  {r['verdict']}")
    print(f'\nverdicts: ' + ', '.join((f'{v}={counts[v]}' for v in VERDICT_ORDER)))
    from datetime import date
    meta = {'command': _reproduce_command(args), 'date': date.today().isoformat(), 'registry': args.registry, 'windows_desc': ', '.join((f"{w} ({WINDOWS[w][0]} → {WINDOWS[w][1] or 'latest'})" for w in window_names)), 'datasets_desc': ', '.join((dataset_key(s, t) for s, t in datasets)), 'direction': args.direction or 'long', 'capital': args.capital}
    if args.markdown_out:
        with open(args.markdown_out, 'w') as fh:
            fh.write(render_markdown(ranked, meta))
        print(f'\nwrote {args.markdown_out}')
    if args.json_out:
        payload = {'registry': args.registry, 'windows': {w: list(WINDOWS[w]) for w in window_names}, 'datasets': [dataset_key(s, t) for s, t in datasets], 'direction': args.direction or 'long', 'capital': args.capital, 'verdict_counts': counts, 'rows': ranked}
        with open(args.json_out, 'w') as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f'wrote {args.json_out}')
    return 0

def _reproduce_command(args) -> str:
    parts = ['uv run --no-sync python backtest/fee_audit.py', f'--registry {args.registry}']
    if args.strategies:
        parts.append(f'--strategies {args.strategies}')
    if args.windows:
        parts.append(f'--windows {args.windows}')
    if args.datasets:
        parts.append(f'--datasets {args.datasets}')
    if args.direction:
        parts.append(f'--direction {args.direction}')
    if args.capital != DEFAULT_CAPITAL:
        parts.append(f'--capital {args.capital}')
    if args.markdown_out:
        parts.append(f'--markdown {args.markdown_out}')
    return ' '.join(parts)
if __name__ == '__main__':
    sys.exit(main())
