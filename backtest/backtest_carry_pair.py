from __future__ import annotations
import argparse
import json
import math
import os
import statistics
import sys
from dataclasses import dataclass, field
from typing import Optional
import numpy as np
import pandas as pd
sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
from eval_windows import DATASETS, WINDOWS, PLATFORM, DEFAULT_CAPITAL, LIQUIDATED_DDADJ_FLOOR, dataset_key, parse_dataset_arg
from backtester import LIQUIDATED_METRIC_FLOOR
STRATEGY_NAME = 'delta_neutral_funding'
DEFAULT_FEE_PCT = 0.00045

def delta_drift_pct(qty_perp: float, mark_perp: float, qty_spot: float, mark_spot: float) -> float:
    perp_notional = qty_perp * mark_perp
    if perp_notional <= 0:
        return 0.0
    spot_notional = qty_spot * mark_spot
    return abs(spot_notional - perp_notional) / perp_notional * 100.0

def rebalance_spot_qty(qty_perp: float, mark_perp: float, mark_spot: float) -> tuple:
    if mark_spot <= 0:
        return (0.0, 0.0)
    target_qty_spot = qty_perp * mark_perp / mark_spot
    return (target_qty_spot, 0.0)

def liquidation_loss(notional: float, leverage: float, maintenance_margin: float) -> float:
    return notional * (1.0 / leverage - maintenance_margin)

def dd_adjusted_return(return_pct: float, max_dd_pct: float) -> float:
    if not max_dd_pct:
        return 0.0
    return return_pct / abs(max_dd_pct)

def leg_from_carry_results(results: 'CarryResults') -> dict:
    ret = results.total_return_pct
    dd = results.max_drawdown_pct
    liquidated = results.account_liquidated
    gross_mag = abs(results.price_pnl) + abs(results.funding_pnl) + results.fees
    funding_share = results.funding_pnl / gross_mag if gross_mag > 0 else 0.0
    return {'sharpe': -LIQUIDATED_METRIC_FLOOR if liquidated else results.sharpe, 'return_pct': ret, 'max_dd_pct': dd, 'ddadj': LIQUIDATED_DDADJ_FLOOR if liquidated else round(dd_adjusted_return(ret, dd), 3), 'trades': results.pairs_opened, 'funding_pnl': round(results.funding_pnl, 4), 'price_pnl': round(results.price_pnl, 4), 'fees': round(results.fees, 4), 'rebalances': results.rebalances, 'perp_liquidations': results.perp_liquidations, 'liquidated': liquidated, 'funding_share': round(funding_share, 4), 'bars_funded': results.bars_funded}

def aggregate_legs(legs: dict) -> dict:
    present = {ds: leg for ds, leg in legs.items() if leg is not None}
    if not present:
        return {'datasets': 0, 'traded_datasets': 0, 'degenerate': True, 'verdict_inputs': []}
    traded = sum((1 for leg in present.values() if leg['trades'] > 0))
    liquidated = sum((1 for leg in present.values() if leg['liquidated']))
    degenerate = traded < math.ceil(len(present) / 2)
    return {'datasets': len(present), 'traded_datasets': traded, 'liquidated_legs': liquidated, 'degenerate': degenerate, 'mean_return_pct': round(statistics.mean((l['return_pct'] for l in present.values())), 3), 'mean_sharpe': round(statistics.mean((l['sharpe'] for l in present.values())), 3), 'mean_ddadj': round(statistics.mean((l['ddadj'] for l in present.values())), 3), 'total_funding_pnl': round(sum((l['funding_pnl'] for l in present.values())), 4), 'total_price_pnl': round(sum((l['price_pnl'] for l in present.values())), 4), 'total_fees': round(sum((l['fees'] for l in present.values())), 4), 'total_rebalances': sum((l['rebalances'] for l in present.values()))}

def carry_verdict(window_summaries: dict) -> str:
    traded = [s for s in window_summaries.values() if s.get('traded_datasets', 0) > 0 and (not s.get('degenerate'))]
    if not traded:
        return 'no_trades'
    positive = sum((1 for s in traded if s['mean_return_pct'] > 0))
    any_liquidated = any((s.get('liquidated_legs', 0) > 0 for s in traded))
    net_total = sum((s['mean_return_pct'] for s in traded))
    if net_total <= 0:
        return 'deprecate'
    if not any_liquidated and positive >= math.ceil(len(traded) / 2):
        return 'healthy'
    return 'marginal'

def bar_hours_from_index(index: pd.Index, default: float=1.0) -> float:
    try:
        deltas = pd.Series(index).diff().dropna()
        if len(deltas):
            secs = deltas.median().total_seconds()
            if secs and secs > 0:
                return secs / 3600.0
    except (AttributeError, TypeError):
        pass
    return default

@dataclass
class CarryEpisode:
    entry_bar: int
    entry_time: pd.Timestamp
    entry_perp: float
    entry_spot: float
    qty_perp: float
    qty_spot: float
    notional_perp: float
    margin_perp: float
    exit_bar: Optional[int] = None
    exit_time: Optional[pd.Timestamp] = None
    price_pnl: float = 0.0
    funding: float = 0.0
    fees: float = 0.0
    realized_spot_pnl: float = 0.0
    rebalances: int = 0
    exit_reason: str = ''

    @property
    def net_pnl(self) -> float:
        return self.price_pnl + self.funding - self.fees

@dataclass
class CarryResults:
    episodes: list = field(default_factory=list)
    equity_curve: pd.Series = field(default_factory=lambda: pd.Series(dtype=float))
    initial_capital: float = 0.0
    final_equity: float = 0.0
    total_return_pct: float = 0.0
    sharpe: float = 0.0
    max_drawdown_pct: float = 0.0
    pairs_opened: int = 0
    price_pnl: float = 0.0
    funding_pnl: float = 0.0
    fees: float = 0.0
    rebalances: int = 0
    perp_liquidations: int = 0
    bars_funded: int = 0
    account_liquidated: bool = False

class CarryPairBacktester:

    def __init__(self, initial_capital: float=DEFAULT_CAPITAL, base_notional: float=750.0, leverage: float=3.0, maintenance_margin: float=0.02, entry_threshold: float=0.0001, exit_threshold: float=5e-05, drift_threshold: float=2.0, perp_fee_pct: float=DEFAULT_FEE_PCT, spot_fee_pct: float=DEFAULT_FEE_PCT, bar_hours: float=1.0, bars_per_year: Optional[int]=None):
        if initial_capital <= 0:
            raise ValueError('initial_capital must be positive')
        if base_notional <= 0:
            raise ValueError('base_notional must be positive')
        if leverage <= 0:
            raise ValueError('leverage must be positive')
        if maintenance_margin < 0 or maintenance_margin >= 1.0 / leverage:
            raise ValueError('maintenance_margin must be in [0, 1/leverage)')
        if entry_threshold <= exit_threshold:
            raise ValueError('entry_threshold must exceed exit_threshold')
        if drift_threshold <= 0:
            raise ValueError('drift_threshold must be positive')
        if bar_hours <= 0:
            raise ValueError('bar_hours must be positive')
        self.initial_capital = float(initial_capital)
        self.base_notional = float(base_notional)
        self.leverage = float(leverage)
        self.maintenance_margin = float(maintenance_margin)
        self.entry_threshold = float(entry_threshold)
        self.exit_threshold = float(exit_threshold)
        self.drift_threshold = float(drift_threshold)
        self.perp_fee_pct = float(perp_fee_pct)
        self.spot_fee_pct = float(spot_fee_pct)
        self.bar_hours = float(bar_hours)
        if bars_per_year is None:
            self.bars_per_year = int(round(24 * 365 / self.bar_hours))
        else:
            self.bars_per_year = int(bars_per_year)
        needed = self.base_notional + self.base_notional / self.leverage
        if needed > self.initial_capital:
            print(f'warning: pair needs ${needed:.2f} (spot cash + perp margin) but initial capital is ${self.initial_capital:.2f} — runs as if capital were free', file=sys.stderr)

    def run(self, df: pd.DataFrame) -> CarryResults:
        for col in ('open', 'close', 'signal'):
            if col not in df.columns:
                raise ValueError(f"input needs a '{col}' column")
        n = len(df)
        signal = df['signal'].to_numpy()
        spot_open = df['open'].to_numpy(dtype=float)
        spot_close = df['close'].to_numpy(dtype=float)
        perp_open = df['perp_open'].to_numpy(dtype=float) if 'perp_open' in df.columns else spot_open
        perp_close = df['perp_close'].to_numpy(dtype=float) if 'perp_close' in df.columns else spot_close
        accrual = df['funding_accrual'].to_numpy(dtype=float) if 'funding_accrual' in df.columns else np.zeros(n, dtype=float)
        equity = self.initial_capital
        equity_curve = np.full(n, np.nan)
        episodes: list = []
        pos: Optional[CarryEpisode] = None
        perp_liquidations = 0
        bars_funded = 0
        for i in range(n):
            if pos is not None:
                mark_perp = perp_close[i]
                mark_spot = spot_close[i]
                pos.price_pnl = pos.qty_perp * (pos.entry_perp - mark_perp) + pos.realized_spot_pnl + pos.qty_spot * (mark_spot - pos.entry_spot)
                if i > pos.entry_bar:
                    bars_funded += self._book_funding_bar(pos, i, mark_perp, accrual)
                perp_loss = -(pos.qty_perp * (pos.entry_perp - mark_perp))
                if perp_loss >= liquidation_loss(pos.notional_perp, self.leverage, self.maintenance_margin):
                    capped_perp = -min(perp_loss, pos.margin_perp)
                    spot_pnl = pos.realized_spot_pnl + pos.qty_spot * (mark_spot - pos.entry_spot)
                    pos.price_pnl = capped_perp + spot_pnl
                    pos.fees += pos.qty_perp * mark_perp * self.perp_fee_pct + pos.qty_spot * mark_spot * self.spot_fee_pct
                    pos.exit_bar = i
                    pos.exit_time = df.index[i]
                    pos.exit_reason = 'liquidation'
                    perp_liquidations += 1
                    equity += pos.net_pnl
                    episodes.append(pos)
                    pos = None
            equity_curve[i] = equity + (pos.net_pnl if pos is not None else 0.0)
            if i + 1 >= n:
                continue
            sig = signal[i]
            if pos is None:
                if sig == -1:
                    pos = self._open_pair(i + 1, perp_open, spot_open, df.index)
            elif sig == 1:
                exit_bar = i + 1
                if exit_bar > pos.entry_bar:
                    bars_funded += self._book_funding_bar(pos, exit_bar, perp_close[exit_bar], accrual)
                self._close_pair(pos, exit_bar, perp_open, spot_open, df.index, 'exit_signal')
                equity += pos.net_pnl
                episodes.append(pos)
                pos = None
            else:
                self._maybe_rebalance(pos, i + 1, perp_open, spot_open)
        if pos is not None:
            self._close_pair(pos, n - 1, perp_open, spot_open, df.index, 'end_of_data')
            equity += pos.net_pnl
            episodes.append(pos)
        return self._summarize(episodes, equity_curve, df.index, perp_liquidations, bars_funded)

    def _book_funding_bar(self, ep: CarryEpisode, bar_idx: int, mark_perp: float, accrual) -> int:
        acc = accrual[bar_idx]
        if math.isnan(acc) or acc == 0.0:
            return 0
        ep.funding += ep.qty_perp * mark_perp * acc
        return 1

    def _open_pair(self, fill_bar: int, perp_open, spot_open, index: pd.Index) -> CarryEpisode:
        entry_perp = perp_open[fill_bar]
        entry_spot = spot_open[fill_bar]
        qty_perp = self.base_notional / entry_perp
        qty_spot = self.base_notional / entry_spot
        ep = CarryEpisode(entry_bar=fill_bar, entry_time=index[fill_bar], entry_perp=entry_perp, entry_spot=entry_spot, qty_perp=qty_perp, qty_spot=qty_spot, notional_perp=self.base_notional, margin_perp=self.base_notional / self.leverage)
        ep.fees = self.base_notional * self.perp_fee_pct + self.base_notional * self.spot_fee_pct
        return ep

    def _close_pair(self, ep: CarryEpisode, fill_bar: int, perp_open, spot_open, index: pd.Index, reason: str) -> None:
        exit_perp = perp_open[fill_bar]
        exit_spot = spot_open[fill_bar]
        ep.exit_bar = fill_bar
        ep.exit_time = index[fill_bar]
        ep.price_pnl = ep.qty_perp * (ep.entry_perp - exit_perp) + ep.realized_spot_pnl + ep.qty_spot * (exit_spot - ep.entry_spot)
        ep.fees += ep.qty_perp * exit_perp * self.perp_fee_pct + ep.qty_spot * exit_spot * self.spot_fee_pct
        ep.exit_reason = reason

    def _maybe_rebalance(self, ep: CarryEpisode, fill_bar: int, perp_open, spot_open) -> None:
        mark_perp = perp_open[fill_bar]
        mark_spot = spot_open[fill_bar]
        drift = delta_drift_pct(ep.qty_perp, mark_perp, ep.qty_spot, mark_spot)
        if drift <= self.drift_threshold:
            return
        target_qty_spot, _ = rebalance_spot_qty(ep.qty_perp, mark_perp, mark_spot)
        delta = target_qty_spot - ep.qty_spot
        traded_notional = abs(delta) * mark_spot
        if delta < 0:
            ep.realized_spot_pnl += -delta * (mark_spot - ep.entry_spot)
        elif delta > 0:
            ep.entry_spot = (ep.qty_spot * ep.entry_spot + delta * mark_spot) / target_qty_spot
        ep.qty_spot = target_qty_spot
        ep.fees += traded_notional * self.spot_fee_pct
        ep.rebalances += 1

    def _summarize(self, episodes: list, equity_curve: np.ndarray, index: pd.Index, perp_liquidations: int, bars_funded: int) -> CarryResults:
        eq = pd.Series(equity_curve, index=index).ffill().fillna(self.initial_capital)
        account_liquidated = bool((eq <= 0).any())
        if account_liquidated:
            bust = eq.le(0).idxmax()
            eq.loc[bust:] = 0.0
        returns = eq.pct_change().dropna()
        if len(returns) > 1 and returns.std() > 0:
            sharpe = float(returns.mean() / returns.std() * math.sqrt(self.bars_per_year))
        else:
            sharpe = 0.0
        cummax = eq.cummax()
        dd = (eq - cummax) / cummax.replace(0.0, np.nan)
        max_dd = float(dd.min()) if len(dd.dropna()) else 0.0
        final_equity = float(eq.iloc[-1]) if len(eq) else self.initial_capital
        if account_liquidated:
            total_return_pct = -LIQUIDATED_METRIC_FLOOR
            max_dd = -1.0
        else:
            total_return_pct = (final_equity / self.initial_capital - 1.0) * 100.0
        return CarryResults(episodes=episodes, equity_curve=eq, initial_capital=self.initial_capital, final_equity=final_equity, total_return_pct=total_return_pct, sharpe=sharpe, max_drawdown_pct=max_dd * 100.0, pairs_opened=len(episodes), price_pnl=sum((e.price_pnl for e in episodes)), funding_pnl=sum((e.funding for e in episodes)), fees=sum((e.fees for e in episodes)), rebalances=sum((e.rebalances for e in episodes)), perp_liquidations=perp_liquidations, bars_funded=bars_funded, account_liquidated=account_liquidated)

def run_carry_leg(reg, symbol: str, timeframe: str, window: tuple, params: Optional[dict]=None, capital: float=DEFAULT_CAPITAL, leverage: float=3.0, maintenance_margin: float=0.02, base_notional: float=750.0, perp_fee_pct: float=DEFAULT_FEE_PCT, spot_fee_pct: float=DEFAULT_FEE_PCT, perp_symbol: Optional[str]=None) -> Optional[dict]:
    import pandas as pd
    from data_fetcher import load_cached_data
    from run_backtest import _attach_funding_if_needed
    start, end = window
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
    if df.empty:
        return None
    if end is not None:
        df = df[df.index < pd.Timestamp(end)]
        if df.empty:
            return None
    df = _attach_funding_if_needed(df, STRATEGY_NAME, symbol, start)
    strat = reg.STRATEGY_REGISTRY.get(STRATEGY_NAME)
    if strat is None:
        raise SystemExit(f'Unknown strategy {STRATEGY_NAME!r}; the futures registry is required')
    strat_params = params if params is not None else strat['default_params']
    df_signals = reg.apply_strategy(STRATEGY_NAME, df, strat_params)
    if perp_symbol:
        perp_df = load_cached_data(perp_symbol, timeframe, exchange_id=PLATFORM, start_date=start, end_date=end)
        if end is not None and (not perp_df.empty):
            perp_df = perp_df[perp_df.index < pd.Timestamp(end)]
        common = df_signals.index.intersection(perp_df.index)
        if common.empty:
            return None
        df_signals = df_signals.loc[common]
        df_signals['perp_open'] = perp_df.loc[common, 'open'].astype(float).values
        df_signals['perp_close'] = perp_df.loc[common, 'close'].astype(float).values
    resolved = {**(strat['default_params'] or {}), **(strat_params or {})}
    bt = CarryPairBacktester(initial_capital=capital, base_notional=base_notional, leverage=leverage, maintenance_margin=maintenance_margin, entry_threshold=float(resolved.get('entry_threshold', 0.0001)), exit_threshold=float(resolved.get('exit_threshold', 5e-05)), drift_threshold=float(resolved.get('drift_threshold', 2.0)), perp_fee_pct=perp_fee_pct, spot_fee_pct=spot_fee_pct, bar_hours=bar_hours_from_index(df_signals.index))
    results = bt.run(df_signals)
    leg = leg_from_carry_results(results)
    try:
        span_days = (df_signals.index[-1] - df_signals.index[0]).total_seconds() / 86400.0
        leg['span_days'] = round(span_days, 4)
    except (AttributeError, TypeError):
        leg['span_days'] = None
    return leg

def _fmt(v, width=9, prec=2):
    if v is None:
        return ' ' * (width - 1) + '-'
    return f'{v:>{width}.{prec}f}'

def format_window_report(window_name: str, window: tuple, legs: dict, summary: dict) -> str:
    start, end = window
    lines = [f"\n== window {window_name} ({start} → {end or 'latest'}) ==", f"{'dataset':<14} {'ret%':>9} {'Sharpe':>9} {'DDadj':>9} {'fund$':>9} {'price$':>9} {'fees$':>9} {'fund%':>7} {'rebal':>6} {'pairs':>6}"]
    for ds in sorted(legs):
        leg = legs[ds]
        if leg is None:
            lines.append(f"{ds:<14} {'(no data)'}")
            continue
        tag = ' LIQ' if leg.get('liquidated') else ''
        lines.append(f"{ds:<14} {_fmt(leg['return_pct'])} {_fmt(leg['sharpe'])} {_fmt(leg['ddadj'])} {_fmt(leg['funding_pnl'])} {_fmt(leg['price_pnl'])} {_fmt(leg['fees'])} {_fmt(leg['funding_share'] * 100, width=7)} {leg['rebalances']:>6} {leg['trades']:>6}{tag}")
    if summary.get('datasets', 0):
        lines.append(f"{'mean/tot':<14} {_fmt(summary['mean_return_pct'])} {_fmt(summary['mean_sharpe'])} {_fmt(summary['mean_ddadj'])} {_fmt(summary['total_funding_pnl'])} {_fmt(summary['total_price_pnl'])} {_fmt(summary['total_fees'])} {'':>7} {summary['total_rebalances']:>6} {summary['traded_datasets']:>3}/{summary['datasets']:<2}" + (' [degenerate]' if summary.get('degenerate') else ''))
    return '\n'.join(lines)

def format_summary(window_summaries: dict, verdict: str) -> str:
    lines = ['\n== summary ==', f"{'window':<10} {'ret%':>9} {'Sharpe':>9} {'fund$':>9} {'fees$':>9} {'traded':>8}"]
    for wname, s in window_summaries.items():
        if not s.get('datasets'):
            lines.append(f"{wname:<10} {'(no data)'}")
            continue
        lines.append(f"{wname:<10} {_fmt(s['mean_return_pct'])} {_fmt(s['mean_sharpe'])} {_fmt(s['total_funding_pnl'])} {_fmt(s['total_fees'])} {s['traded_datasets']:>3}/{s['datasets']:<3}" + ('  degenerate' if s.get('degenerate') else ''))
    lines.append(f'\nverdict: {verdict.upper()} — net-of-fees funding carry on the hedged pair (price PnL cancels by construction in single-series mode)')
    return '\n'.join(lines)

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description='Hedged funding-carry pair backtester (#1326)')
    p.add_argument('--params', default=None, help='delta_neutral_funding params JSON (overrides registry defaults for entry/exit/drift thresholds)')
    p.add_argument('--datasets', default=None, help='Comma list of SYMBOL:TIMEFRAME (default: the six audit datasets)')
    p.add_argument('--windows', default=None, help=f"Comma list of windows (default: all). Known: {', '.join(WINDOWS)}")
    p.add_argument('--initial-capital', type=float, default=DEFAULT_CAPITAL)
    p.add_argument('--base-notional', type=float, default=750.0)
    p.add_argument('--leverage', type=float, default=3.0)
    p.add_argument('--maintenance-margin', type=float, default=0.02)
    p.add_argument('--perp-fee-pct', type=float, default=DEFAULT_FEE_PCT)
    p.add_argument('--spot-fee-pct', type=float, default=DEFAULT_FEE_PCT)
    p.add_argument('--perp-symbol', default=None, help='Optional second cached symbol to mark the perp leg on (basis mode); default single-series (perp==spot marks).')
    p.add_argument('--json', default=None, dest='json_out', help='Write the full structured result to this path')
    return p

def main(argv: Optional[list]=None) -> int:
    args = build_parser().parse_args(argv)
    params = json.loads(args.params) if args.params else None
    if args.windows:
        window_names = [w.strip() for w in args.windows.split(',') if w.strip()]
        unknown = [w for w in window_names if w not in WINDOWS]
        if unknown:
            raise SystemExit(f'unknown windows {unknown}; known: {list(WINDOWS)}')
    else:
        window_names = list(WINDOWS)
    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(',') if d.strip()]
    else:
        datasets = list(DATASETS)
    from registry_loader import load_registry
    reg = load_registry('futures')
    print(f"strategy: {STRATEGY_NAME} (params: {params or 'registry defaults'}); structure: SHORT perp + LONG spot, funding on the perp leg only")
    window_legs: dict = {}
    window_summaries: dict = {}
    for wname in window_names:
        window = WINDOWS[wname]
        legs = {}
        for symbol, timeframe in datasets:
            ds = dataset_key(symbol, timeframe)
            legs[ds] = run_carry_leg(reg, symbol, timeframe, window, params=params, capital=args.initial_capital, leverage=args.leverage, maintenance_margin=args.maintenance_margin, base_notional=args.base_notional, perp_fee_pct=args.perp_fee_pct, spot_fee_pct=args.spot_fee_pct, perp_symbol=args.perp_symbol)
        summary = aggregate_legs(legs)
        window_legs[wname] = legs
        window_summaries[wname] = summary
        print(format_window_report(wname, window, legs, summary))
    verdict = carry_verdict(window_summaries)
    print(format_summary(window_summaries, verdict))
    if args.json_out:
        payload = {'strategy': STRATEGY_NAME, 'params': params, 'datasets': [dataset_key(s, t) for s, t in datasets], 'windows': {w: list(WINDOWS[w]) for w in window_names}, 'window_legs': window_legs, 'window_summaries': window_summaries, 'verdict': verdict}
        with open(args.json_out, 'w') as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f'\nwrote {args.json_out}')
    return 0
if __name__ == '__main__':
    sys.exit(main())
