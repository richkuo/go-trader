#!/usr/bin/env python3
"""Hedged funding-carry pair backtester (#1326).

Simulates the structure `delta_neutral_funding` actually runs live — SHORT the
perp to collect funding while holding an equal-notional SPOT long as the delta
offset — so the strategy can finally earn a real edge verdict. The existing
Backtester holds exactly one leg, so its result for this strategy is dominated
by the naked short's price PnL (e.g. SOL 2023 legs ≈ −30%), precisely the
component the live spot hedge is designed to cancel (#1280,
docs/research/1280-edge-verdicts.md). This harness books both legs, so what
survives is net-of-fees funding carry.

Design (mirrors backtest_pairs.py's two-leg accounting, no live path):
- Perp SHORT leg: isolated margin (`leverage`/`maintenance_margin`), per-fill
  taker fee, per-bar funding booked from the #988 `funding_accrual` series
  (short receives when accrual is positive), isolated-margin liquidation with
  the loss capped at the margin posted (gap-through cap, HL isolated mode).
- Spot LONG leg: fully funded (cash outlay = notional), NO funding, NO leverage,
  NO liquidation — it can only lose its notional, which the account already
  holds as cash.
- Entry/exit come from the SAME registry signal the live strategy emits
  (`reg.apply_strategy("delta_neutral_funding", ...)`): signal −1 opens/holds
  the pair, +1 closes it, 0 holds. So the harness adjudicates exactly the
  entries live would take, including the full-7d-window warmup.
- Delta drift is computed HERE from the two legs' notionals — the registry's
  `delta_drift_pct`/`rebalance_needed` columns are hardcoded 0.0 placeholders
  (registry.py:1275-1276), so only `drift_threshold` is reused. When drift
  exceeds `drift_threshold` the SPOT leg is traded back to the perp notional at
  the next bar open, paying a fee.

Look-ahead contract mirrors backtest_pairs.py (#730/#731): a signal at bar N
fills at bar N+1 open; funding/marks use closed-bar values.

Not for live execution:
- Basis is unmodeled by default: both legs mark on the SAME cached close series,
  so a perfect single-series hedge cancels price PnL exactly and drift stays 0
  (rebalances=0) — the harness then measures pure carry minus costs, the ideal
  delta-neutral reading. Pass `--perp-symbol` to mark the perp leg on a second
  cached series (a real perp/spot basis), which drives genuine tracking drift
  and exercises rebalancing.
- Quantities are not rounded to exchange lot/tick size.
"""

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

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "shared_tools"))

# Reuse the audit constants/pure helpers rather than redefining them, so the
# dataset/window universe and the liquidated-metric floors stay in lockstep
# with eval_windows / backtester (tests assert the equality).
from eval_windows import (  # noqa: E402
    DATASETS,
    WINDOWS,
    PLATFORM,
    DEFAULT_CAPITAL,
    LIQUIDATED_DDADJ_FLOOR,
    dataset_key,
    parse_dataset_arg,
)
from backtester import LIQUIDATED_METRIC_FLOOR  # noqa: E402

STRATEGY_NAME = "delta_neutral_funding"

# Hyperliquid base-tier taker fee (0.045%/side), matching backtester.PLATFORM_FEE_PCT
# ["hyperliquid"] and eval_windows FEE_PLATFORM — audits price the fees we pay.
DEFAULT_FEE_PCT = 0.00045


# ---------------------------------------------------------------------------
# Pure helpers (no I/O; unit-tested without data access).
# ---------------------------------------------------------------------------

def delta_drift_pct(qty_perp: float, mark_perp: float,
                    qty_spot: float, mark_spot: float) -> float:
    """Delta drift as a percentage of the perp (hedge-anchor) notional.

    The two legs open at equal notional; they drift apart when their marks
    diverge (perp/spot basis) or after a leg is re-sized. Anchored on the perp
    notional because the perp is the funding-bearing leg the spot hedges.
    """
    perp_notional = qty_perp * mark_perp
    if perp_notional <= 0:
        return 0.0
    spot_notional = qty_spot * mark_spot
    return abs(spot_notional - perp_notional) / perp_notional * 100.0


def rebalance_spot_qty(qty_perp: float, mark_perp: float,
                       mark_spot: float) -> tuple:
    """Spot qty that restores notional parity with the perp leg, plus the
    traded notional (for the rebalance fee). Marks are next-bar-open fills."""
    if mark_spot <= 0:
        return 0.0, 0.0
    target_qty_spot = qty_perp * mark_perp / mark_spot
    return target_qty_spot, 0.0  # traded notional filled in by caller vs old qty


def liquidation_loss(notional: float, leverage: float,
                     maintenance_margin: float) -> float:
    """Dollar loss on an isolated leg that triggers liquidation:
    (1/leverage − maintenance_margin) × notional. Same math as
    backtest_pairs._liquidation_loss, duplicated here so the carry harness
    stays a standalone module."""
    return notional * (1.0 / leverage - maintenance_margin)


def dd_adjusted_return(return_pct: float, max_dd_pct: float) -> float:
    """DDadj = return / |max drawdown| (#963); 0.0 when there is no drawdown
    (an untraded/flat leg carries no risk denominator)."""
    if not max_dd_pct:
        return 0.0
    return return_pct / abs(max_dd_pct)


def leg_from_carry_results(results: "CarryResults") -> dict:
    """Collapse a CarryResults to the per-leg metrics the verdict reports.

    ``funding_share`` decomposes the net edge: funding as a fraction of the
    gross magnitude (|price| + |funding| + fees), so a healthy verdict is
    visibly carry-driven, not residual price. A liquidated account (#1005)
    floors ddadj and Sharpe so a dead account always sorts below any survivor.
    """
    ret = results.total_return_pct
    dd = results.max_drawdown_pct
    liquidated = results.account_liquidated
    gross_mag = abs(results.price_pnl) + abs(results.funding_pnl) + results.fees
    funding_share = (results.funding_pnl / gross_mag) if gross_mag > 0 else 0.0
    return {
        "sharpe": (-LIQUIDATED_METRIC_FLOOR if liquidated else results.sharpe),
        "return_pct": ret,
        "max_dd_pct": dd,
        "ddadj": (LIQUIDATED_DDADJ_FLOOR if liquidated
                  else round(dd_adjusted_return(ret, dd), 3)),
        "trades": results.pairs_opened,
        "funding_pnl": round(results.funding_pnl, 4),
        "price_pnl": round(results.price_pnl, 4),
        "fees": round(results.fees, 4),
        "rebalances": results.rebalances,
        "perp_liquidations": results.perp_liquidations,
        "liquidated": liquidated,
        "funding_share": round(funding_share, 4),
        "bars_funded": results.bars_funded,
    }


def aggregate_legs(legs: dict) -> dict:
    """Per-window summary across datasets: {dataset_key: leg | None} -> means.

    ``degenerate`` uses the #976 majority-must-trade rule (a window where most
    legs never opened a pair is not a real result). Means are taken over legs
    that ran (non-None); totals sum funding/fees across the window.
    """
    present = {ds: leg for ds, leg in legs.items() if leg is not None}
    if not present:
        return {"datasets": 0, "traded_datasets": 0, "degenerate": True,
                "verdict_inputs": []}
    traded = sum(1 for leg in present.values() if leg["trades"] > 0)
    liquidated = sum(1 for leg in present.values() if leg["liquidated"])
    degenerate = traded < math.ceil(len(present) / 2)
    return {
        "datasets": len(present),
        "traded_datasets": traded,
        "liquidated_legs": liquidated,
        "degenerate": degenerate,
        "mean_return_pct": round(statistics.mean(l["return_pct"] for l in present.values()), 3),
        "mean_sharpe": round(statistics.mean(l["sharpe"] for l in present.values()), 3),
        "mean_ddadj": round(statistics.mean(l["ddadj"] for l in present.values()), 3),
        "total_funding_pnl": round(sum(l["funding_pnl"] for l in present.values()), 4),
        "total_price_pnl": round(sum(l["price_pnl"] for l in present.values()), 4),
        "total_fees": round(sum(l["fees"] for l in present.values()), 4),
        "total_rebalances": sum(l["rebalances"] for l in present.values()),
    }


def carry_verdict(window_summaries: dict) -> str:
    """Recorded label from the per-window summaries (M5 salvage-verdict style).

    A funding-carry structure is not competing with directional incumbents, so
    this is an ABSOLUTE carry-vs-cost verdict, not an incumbent-relative bar:

    - ``no_trades``  — no window opened a pair anywhere.
    - ``deprecate``  — net carry ≤ 0 across the traded windows (fees eat it).
    - ``healthy``    — net return > 0 in a MAJORITY of traded, non-degenerate
                       windows with no account-level liquidation.
    - ``marginal``   — everything else (mixed, thin, or a liquidation appeared).
    """
    traded = [s for s in window_summaries.values()
              if s.get("traded_datasets", 0) > 0 and not s.get("degenerate")]
    if not traded:
        return "no_trades"
    positive = sum(1 for s in traded if s["mean_return_pct"] > 0)
    any_liquidated = any(s.get("liquidated_legs", 0) > 0 for s in traded)
    net_total = sum(s["mean_return_pct"] for s in traded)
    if net_total <= 0:
        return "deprecate"
    if not any_liquidated and positive >= math.ceil(len(traded) / 2):
        return "healthy"
    return "marginal"


def bar_hours_from_index(index: pd.Index, default: float = 1.0) -> float:
    """Median spacing of a DatetimeIndex in hours (timeframe-correct Sharpe /
    funding). Falls back to ``default`` on a non-datetime or single-bar index."""
    try:
        deltas = pd.Series(index).diff().dropna()
        if len(deltas):
            secs = deltas.median().total_seconds()
            if secs and secs > 0:
                return secs / 3600.0
    except (AttributeError, TypeError):
        pass
    return default


# ---------------------------------------------------------------------------
# Engine.
# ---------------------------------------------------------------------------

@dataclass
class CarryEpisode:
    """One open→close hedged-pair episode."""
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
    price_pnl: float = 0.0       # perp short + spot long price PnL (marked)
    funding: float = 0.0         # cumulative funding carry (perp leg only)
    fees: float = 0.0            # entry + exit + rebalance fees
    rebalances: int = 0
    exit_reason: str = ""

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
    def __init__(
        self,
        initial_capital: float = DEFAULT_CAPITAL,
        base_notional: float = 750.0,
        leverage: float = 3.0,
        maintenance_margin: float = 0.02,
        entry_threshold: float = 0.0001,
        exit_threshold: float = 0.00005,
        drift_threshold: float = 2.0,
        perp_fee_pct: float = DEFAULT_FEE_PCT,
        spot_fee_pct: float = DEFAULT_FEE_PCT,
        bar_hours: float = 1.0,
        bars_per_year: Optional[int] = None,
    ):
        if initial_capital <= 0:
            raise ValueError("initial_capital must be positive")
        if base_notional <= 0:
            raise ValueError("base_notional must be positive")
        if leverage <= 0:
            raise ValueError("leverage must be positive")
        if maintenance_margin < 0 or maintenance_margin >= 1.0 / leverage:
            raise ValueError("maintenance_margin must be in [0, 1/leverage)")
        if entry_threshold <= exit_threshold:
            raise ValueError("entry_threshold must exceed exit_threshold")
        if drift_threshold <= 0:
            raise ValueError("drift_threshold must be positive")
        if bar_hours <= 0:
            raise ValueError("bar_hours must be positive")
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

        # Capital sufficiency: the spot leg is fully funded (base_notional cash)
        # and the perp leg posts base_notional/leverage margin. Warn (don't
        # reject) if that exceeds capital, so a stress config still runs.
        needed = self.base_notional + self.base_notional / self.leverage
        if needed > self.initial_capital:
            print(
                f"warning: pair needs ${needed:.2f} (spot cash + perp margin) "
                f"but initial capital is ${self.initial_capital:.2f} — runs as "
                f"if capital were free",
                file=sys.stderr,
            )

    def run(self, df: pd.DataFrame) -> CarryResults:
        """df needs `open`/`close` and a `signal` column. `funding_accrual`
        (per-bar carry, #988) and an optional `perp_close`/`perp_open` (basis
        mode) are used when present; missing funding accrues 0."""
        for col in ("open", "close", "signal"):
            if col not in df.columns:
                raise ValueError(f"input needs a '{col}' column")
        n = len(df)
        signal = df["signal"].to_numpy()
        spot_open = df["open"].to_numpy(dtype=float)
        spot_close = df["close"].to_numpy(dtype=float)
        # Perp marks default to the spot series (single-series hedge); basis
        # mode overrides with a second cached series aligned to the same index.
        perp_open = (df["perp_open"].to_numpy(dtype=float)
                     if "perp_open" in df.columns else spot_open)
        perp_close = (df["perp_close"].to_numpy(dtype=float)
                      if "perp_close" in df.columns else spot_close)
        accrual = (df["funding_accrual"].to_numpy(dtype=float)
                   if "funding_accrual" in df.columns
                   else np.zeros(n, dtype=float))

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
                # Short perp: profit when price falls. Long spot: profit when
                # price rises. In single-series mode these cancel exactly.
                pos.price_pnl = (pos.qty_perp * (pos.entry_perp - mark_perp)
                                 + pos.qty_spot * (mark_spot - pos.entry_spot))
                # Funding booked on the perp (short) leg only: receive when
                # accrual > 0 (longs pay shorts). Timeframe-correct via #988.
                # accrual[j] covers (T_{j-1}, T_j]; the pair filled at open[j] =
                # T_{entry_bar}, so a newly-opened position first accrues over
                # the NEXT full bar (i > entry_bar), never the pre-entry interval
                # accrual[entry_bar] — matching backtester.py:2425. The exit
                # bar's funding is booked at the close site (signal exit) or is
                # already covered here (liquidation/end-of-data both mark the
                # exit bar in this loop).
                if i > pos.entry_bar:
                    bars_funded += self._book_funding_bar(pos, i, mark_perp, accrual)

                # Isolated-margin liquidation on the perp leg only. The spot
                # leg is fully funded and cannot be liquidated.
                perp_loss = -(pos.qty_perp * (pos.entry_perp - mark_perp))
                if perp_loss >= liquidation_loss(pos.notional_perp, self.leverage,
                                                 self.maintenance_margin):
                    # Cap the perp loss at posted margin (isolated mode). The
                    # spot leg's offsetting gain is still credited in full.
                    capped_perp = -min(perp_loss, pos.margin_perp)
                    spot_pnl = pos.qty_spot * (mark_spot - pos.entry_spot)
                    pos.price_pnl = capped_perp + spot_pnl
                    pos.fees += (pos.qty_perp * mark_perp * self.perp_fee_pct
                                 + pos.qty_spot * mark_spot * self.spot_fee_pct)
                    pos.exit_bar = i
                    pos.exit_time = df.index[i]
                    pos.exit_reason = "liquidation"
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
            else:
                if sig == 1:
                    # Signal exit fills at bar i+1, which this loop never marks
                    # (pos is cleared here), so book that bar's funding — the
                    # final held interval (T_{exit_bar-1}, T_{exit_bar}] — now.
                    exit_bar = i + 1
                    if exit_bar > pos.entry_bar:
                        bars_funded += self._book_funding_bar(
                            pos, exit_bar, perp_open[exit_bar], accrual)
                    self._close_pair(pos, exit_bar, perp_open, spot_open,
                                     df.index, "exit_signal")
                    equity += pos.net_pnl
                    episodes.append(pos)
                    pos = None
                else:
                    # Still hedged: rebalance the spot leg back to parity if the
                    # legs have drifted past the threshold. Fill at next open.
                    self._maybe_rebalance(pos, i + 1, perp_open, spot_open)

        if pos is not None:
            self._close_pair(pos, n - 1, perp_open, spot_open, df.index,
                             "end_of_data")
            equity += pos.net_pnl
            episodes.append(pos)

        return self._summarize(episodes, equity_curve, df.index,
                               perp_liquidations, bars_funded)

    def _book_funding_bar(self, ep: CarryEpisode, bar_idx: int, mark_perp: float,
                          accrual) -> int:
        """Book one bar's perp-leg funding at ``mark_perp``; return 1 if a
        nonzero, non-NaN accrual was booked (for the bars_funded counter)."""
        acc = accrual[bar_idx]
        if math.isnan(acc) or acc == 0.0:
            return 0
        ep.funding += ep.qty_perp * mark_perp * acc
        return 1

    def _open_pair(self, fill_bar: int, perp_open, spot_open,
                   index: pd.Index) -> CarryEpisode:
        entry_perp = perp_open[fill_bar]
        entry_spot = spot_open[fill_bar]
        qty_perp = self.base_notional / entry_perp
        qty_spot = self.base_notional / entry_spot
        ep = CarryEpisode(
            entry_bar=fill_bar,
            entry_time=index[fill_bar],
            entry_perp=entry_perp,
            entry_spot=entry_spot,
            qty_perp=qty_perp,
            qty_spot=qty_spot,
            notional_perp=self.base_notional,
            margin_perp=self.base_notional / self.leverage,
        )
        ep.fees = (self.base_notional * self.perp_fee_pct
                   + self.base_notional * self.spot_fee_pct)
        return ep

    def _close_pair(self, ep: CarryEpisode, fill_bar: int, perp_open, spot_open,
                    index: pd.Index, reason: str) -> None:
        exit_perp = perp_open[fill_bar]
        exit_spot = spot_open[fill_bar]
        ep.exit_bar = fill_bar
        ep.exit_time = index[fill_bar]
        ep.price_pnl = (ep.qty_perp * (ep.entry_perp - exit_perp)
                        + ep.qty_spot * (exit_spot - ep.entry_spot))
        ep.fees += (ep.qty_perp * exit_perp * self.perp_fee_pct
                    + ep.qty_spot * exit_spot * self.spot_fee_pct)
        ep.exit_reason = reason

    def _maybe_rebalance(self, ep: CarryEpisode, fill_bar: int,
                         perp_open, spot_open) -> None:
        mark_perp = perp_open[fill_bar]
        mark_spot = spot_open[fill_bar]
        drift = delta_drift_pct(ep.qty_perp, mark_perp, ep.qty_spot, mark_spot)
        if drift <= self.drift_threshold:
            return
        target_qty_spot, _ = rebalance_spot_qty(ep.qty_perp, mark_perp, mark_spot)
        traded_notional = abs(target_qty_spot - ep.qty_spot) * mark_spot
        ep.qty_spot = target_qty_spot
        ep.fees += traded_notional * self.spot_fee_pct
        ep.rebalances += 1

    def _summarize(self, episodes: list, equity_curve: np.ndarray,
                   index: pd.Index, perp_liquidations: int,
                   bars_funded: int) -> CarryResults:
        eq = pd.Series(equity_curve, index=index).ffill().fillna(self.initial_capital)
        # #1005 sticky liquidation floor: once account equity hits ≤ 0, floor it
        # at 0 from that bar on (a negative-base pct_change inverts Sharpe sign).
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
            # Floor the reported metrics like eval_windows/backtester do.
            total_return_pct = -LIQUIDATED_METRIC_FLOOR
            max_dd = -1.0
        else:
            total_return_pct = (final_equity / self.initial_capital - 1.0) * 100.0
        return CarryResults(
            episodes=episodes,
            equity_curve=eq,
            initial_capital=self.initial_capital,
            final_equity=final_equity,
            total_return_pct=total_return_pct,
            sharpe=sharpe,
            max_drawdown_pct=max_dd * 100.0,
            pairs_opened=len(episodes),
            price_pnl=sum(e.price_pnl for e in episodes),
            funding_pnl=sum(e.funding for e in episodes),
            fees=sum(e.fees for e in episodes),
            rebalances=sum(e.rebalances for e in episodes),
            perp_liquidations=perp_liquidations,
            bars_funded=bars_funded,
            account_liquidated=account_liquidated,
        )


# ---------------------------------------------------------------------------
# I/O layer.
# ---------------------------------------------------------------------------

def run_carry_leg(reg, symbol: str, timeframe: str, window: tuple,
                  params: Optional[dict] = None,
                  capital: float = DEFAULT_CAPITAL,
                  leverage: float = 3.0,
                  maintenance_margin: float = 0.02,
                  base_notional: float = 750.0,
                  perp_fee_pct: float = DEFAULT_FEE_PCT,
                  spot_fee_pct: float = DEFAULT_FEE_PCT,
                  perp_symbol: Optional[str] = None) -> Optional[dict]:
    """Load one (symbol, timeframe, window) leg, attach funding, run the engine.

    Reuses run_backtest._attach_funding_if_needed (the #988 coverage-store path
    eval_windows uses) so the `funding_rate` signal input and the
    `funding_accrual` carry match a live/M1 run bar-for-bar. Returns the pure
    leg dict, or None if the window has no data.
    """
    import pandas as pd  # noqa: F811 (local import mirrors eval_windows.run_leg)
    from data_fetcher import load_cached_data
    from run_backtest import _attach_funding_if_needed

    start, end = window
    df = load_cached_data(symbol, timeframe, exchange_id=PLATFORM,
                          start_date=start, end_date=end)
    if df.empty:
        return None
    # Half-open [start, end) slice so adjacent audit windows never double-count
    # the boundary bar (same convention as eval_windows.run_leg).
    if end is not None:
        df = df[df.index < pd.Timestamp(end)]
        if df.empty:
            return None

    df = _attach_funding_if_needed(df, STRATEGY_NAME, symbol, start)

    strat = reg.STRATEGY_REGISTRY.get(STRATEGY_NAME)
    if strat is None:
        raise SystemExit(f"Unknown strategy {STRATEGY_NAME!r}; the futures "
                         f"registry is required")
    strat_params = params if params is not None else strat["default_params"]
    df_signals = reg.apply_strategy(STRATEGY_NAME, df, strat_params)

    if perp_symbol:
        perp_df = load_cached_data(perp_symbol, timeframe, exchange_id=PLATFORM,
                                   start_date=start, end_date=end)
        if end is not None and not perp_df.empty:
            perp_df = perp_df[perp_df.index < pd.Timestamp(end)]
        common = df_signals.index.intersection(perp_df.index)
        if common.empty:
            return None
        df_signals = df_signals.loc[common]
        df_signals["perp_open"] = perp_df.loc[common, "open"].astype(float).values
        df_signals["perp_close"] = perp_df.loc[common, "close"].astype(float).values

    resolved = {**(strat["default_params"] or {}), **(strat_params or {})}
    bt = CarryPairBacktester(
        initial_capital=capital,
        base_notional=base_notional,
        leverage=leverage,
        maintenance_margin=maintenance_margin,
        entry_threshold=float(resolved.get("entry_threshold", 0.0001)),
        exit_threshold=float(resolved.get("exit_threshold", 0.00005)),
        drift_threshold=float(resolved.get("drift_threshold", 2.0)),
        perp_fee_pct=perp_fee_pct,
        spot_fee_pct=spot_fee_pct,
        bar_hours=bar_hours_from_index(df_signals.index),
    )
    results = bt.run(df_signals)
    leg = leg_from_carry_results(results)
    try:
        span_days = (df_signals.index[-1] - df_signals.index[0]).total_seconds() / 86400.0
        leg["span_days"] = round(span_days, 4)
    except (AttributeError, TypeError):
        leg["span_days"] = None
    return leg


# ---------------------------------------------------------------------------
# Reporting.
# ---------------------------------------------------------------------------

def _fmt(v, width=9, prec=2):
    if v is None:
        return " " * (width - 1) + "-"
    return f"{v:>{width}.{prec}f}"


def format_window_report(window_name: str, window: tuple, legs: dict,
                         summary: dict) -> str:
    start, end = window
    lines = [
        f"\n== window {window_name} ({start} → {end or 'latest'}) ==",
        f"{'dataset':<14} {'ret%':>9} {'Sharpe':>9} {'DDadj':>9} "
        f"{'fund$':>9} {'price$':>9} {'fees$':>9} {'fund%':>7} "
        f"{'rebal':>6} {'pairs':>6}",
    ]
    for ds in sorted(legs):
        leg = legs[ds]
        if leg is None:
            lines.append(f"{ds:<14} {'(no data)'}")
            continue
        tag = " LIQ" if leg.get("liquidated") else ""
        lines.append(
            f"{ds:<14} {_fmt(leg['return_pct'])} {_fmt(leg['sharpe'])} "
            f"{_fmt(leg['ddadj'])} {_fmt(leg['funding_pnl'])} "
            f"{_fmt(leg['price_pnl'])} {_fmt(leg['fees'])} "
            f"{_fmt(leg['funding_share'] * 100, width=7)} "
            f"{leg['rebalances']:>6} {leg['trades']:>6}{tag}"
        )
    if summary.get("datasets", 0):
        lines.append(
            f"{'mean/tot':<14} {_fmt(summary['mean_return_pct'])} "
            f"{_fmt(summary['mean_sharpe'])} {_fmt(summary['mean_ddadj'])} "
            f"{_fmt(summary['total_funding_pnl'])} {_fmt(summary['total_price_pnl'])} "
            f"{_fmt(summary['total_fees'])} {'':>7} "
            f"{summary['total_rebalances']:>6} "
            f"{summary['traded_datasets']:>3}/{summary['datasets']:<2}"
            + (" [degenerate]" if summary.get("degenerate") else "")
        )
    return "\n".join(lines)


def format_summary(window_summaries: dict, verdict: str) -> str:
    lines = ["\n== summary ==",
             f"{'window':<10} {'ret%':>9} {'Sharpe':>9} {'fund$':>9} "
             f"{'fees$':>9} {'traded':>8}"]
    for wname, s in window_summaries.items():
        if not s.get("datasets"):
            lines.append(f"{wname:<10} {'(no data)'}")
            continue
        lines.append(
            f"{wname:<10} {_fmt(s['mean_return_pct'])} {_fmt(s['mean_sharpe'])} "
            f"{_fmt(s['total_funding_pnl'])} {_fmt(s['total_fees'])} "
            f"{s['traded_datasets']:>3}/{s['datasets']:<3}"
            + ("  degenerate" if s.get("degenerate") else "")
        )
    lines.append(f"\nverdict: {verdict.upper()} — net-of-fees funding carry on "
                 f"the hedged pair (price PnL cancels by construction in "
                 f"single-series mode)")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# CLI.
# ---------------------------------------------------------------------------

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="Hedged funding-carry pair backtester (#1326)")
    p.add_argument("--params", default=None,
                   help="delta_neutral_funding params JSON (overrides registry "
                        "defaults for entry/exit/drift thresholds)")
    p.add_argument("--datasets", default=None,
                   help="Comma list of SYMBOL:TIMEFRAME (default: the six audit "
                        "datasets)")
    p.add_argument("--windows", default=None,
                   help=f"Comma list of windows (default: all). "
                        f"Known: {', '.join(WINDOWS)}")
    p.add_argument("--initial-capital", type=float, default=DEFAULT_CAPITAL)
    p.add_argument("--base-notional", type=float, default=750.0)
    p.add_argument("--leverage", type=float, default=3.0)
    p.add_argument("--maintenance-margin", type=float, default=0.02)
    p.add_argument("--perp-fee-pct", type=float, default=DEFAULT_FEE_PCT)
    p.add_argument("--spot-fee-pct", type=float, default=DEFAULT_FEE_PCT)
    p.add_argument("--perp-symbol", default=None,
                   help="Optional second cached symbol to mark the perp leg on "
                        "(basis mode); default single-series (perp==spot marks).")
    p.add_argument("--json", default=None, dest="json_out",
                   help="Write the full structured result to this path")
    return p


def main(argv: Optional[list] = None) -> int:
    args = build_parser().parse_args(argv)

    params = json.loads(args.params) if args.params else None

    if args.windows:
        window_names = [w.strip() for w in args.windows.split(",") if w.strip()]
        unknown = [w for w in window_names if w not in WINDOWS]
        if unknown:
            raise SystemExit(f"unknown windows {unknown}; known: {list(WINDOWS)}")
    else:
        window_names = list(WINDOWS)

    if args.datasets:
        datasets = [parse_dataset_arg(d) for d in args.datasets.split(",") if d.strip()]
    else:
        datasets = list(DATASETS)

    from registry_loader import load_registry
    reg = load_registry("futures")

    print(f"strategy: {STRATEGY_NAME} "
          f"(params: {params or 'registry defaults'}); "
          f"structure: SHORT perp + LONG spot, funding on the perp leg only")

    window_legs: dict = {}
    window_summaries: dict = {}
    for wname in window_names:
        window = WINDOWS[wname]
        legs = {}
        for symbol, timeframe in datasets:
            ds = dataset_key(symbol, timeframe)
            legs[ds] = run_carry_leg(
                reg, symbol, timeframe, window, params=params,
                capital=args.initial_capital,
                leverage=args.leverage,
                maintenance_margin=args.maintenance_margin,
                base_notional=args.base_notional,
                perp_fee_pct=args.perp_fee_pct,
                spot_fee_pct=args.spot_fee_pct,
                perp_symbol=args.perp_symbol)
        summary = aggregate_legs(legs)
        window_legs[wname] = legs
        window_summaries[wname] = summary
        print(format_window_report(wname, window, legs, summary))

    verdict = carry_verdict(window_summaries)
    print(format_summary(window_summaries, verdict))

    if args.json_out:
        payload = {
            "strategy": STRATEGY_NAME,
            "params": params,
            "datasets": [dataset_key(s, t) for s, t in datasets],
            "windows": {w: list(WINDOWS[w]) for w in window_names},
            "window_legs": window_legs,
            "window_summaries": window_summaries,
            "verdict": verdict,
        }
        with open(args.json_out, "w") as fh:
            json.dump(payload, fh, indent=2, default=str)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
