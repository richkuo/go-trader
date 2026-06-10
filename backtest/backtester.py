"""
Backtesting engine — simulates strategy execution on historical data.
Calculates comprehensive performance metrics.

Look-ahead contract (#730)
--------------------------
The engine simulates the live trading cycle, which runs at end-of-bar:
  1. Strategy reads OHLCV through bar N's close.
  2. Strategy emits signal, regime label, indicators (all using bar-N-closed data).
  3. Scheduler places order. Order fills at start of bar N+1's open.

To preserve that ordering in vectorized form, ``run()`` shifts every column
that represents a *decision input* forward by one bar before the per-bar
loop reads it, so row N+1 carries the decision values computed from bar N's
closed data:
  • ``signal``         — shift(1) in the signal-normalization block; fills
                         at row N+1's open in the per-bar entry block.
  • ``_open_action``   — shift(1) in the open/close-split normalization block.
  • ``_close_fraction`` (column-based) — shift(1) in the same block.
  • ``regime``         — shift(1) in the regime-shift block (post-injection,
                         #730) so entries gate on bar N's regime, not N+1's.

Close evaluators run end-of-bar in ``_evaluate_close_strategies`` against
bar N's mark and bar N's ATR. Their result becomes ``pending_close_fraction``,
applied at row N+1's open — same alignment as the rest of the close pipeline.
The current-bar ATR access in the ``market_dict`` build is intentional and
matches live: close evaluators see the ATR at decision time, not a frozen
entry-time snapshot.

Indicator semantics
-------------------
Indicator columns supplied by the caller (``atr``, ``regime``, ``adx``, etc.)
represent bar N's value computed from data through bar N's close. The engine
treats them as closed-bar quantities. Caller strategy scripts MUST NOT emit
forward-peeking signals (e.g. ``signal = (close.shift(-1) > close)``); the
signal ``shift(1)`` is the only look-ahead guard on the signal path and is
defeated by upstream peeking. ``backtest/tests/test_backtester_lookahead.py``
regression-tests the shift's effectiveness.

SL/TP intra-bar races
---------------------
Bar-level granularity — when an SL hit and a TP fill could both occur within
the same bar, the engine resolves them at bar close, not by intra-bar OHLC
walking. Documented under ``Backtest`` in CLAUDE.md.

Live parity limitations (#906 audit)
------------------------------------
Surfaces intentionally **not** modeled here (use ``backtest/parity_diff.py`` for
decision-layer parity checks; see ``backtest/AUDIT.md`` for the full matrix):

  • **Scale-in / pyramiding** (#873) — HL perps + manual live-only; same-direction
    adds are skipped in backtest.
  • **Resting manual limit orders** (#883) — maker fills / partial OID reconcile
    have no bar-level simulation.
  • **``tiered_tp_atr_live_regime_dynamic``** (#843) — rejected at
    ``run_backtest.load_strategy_config`` (on-chain regime hysteresis).
  • **``regime_directional_policy``** (#822) — rejected at config load; use static
    ``direction`` / ``invert_signal`` for backtests.
  • **Inline trailing SL at open** (#885) — live arms same-cycle; backtest seeds
    trailing/ratchet triggers on the bar after open (no naked-gap modeling).
"""

import sys
import os
import json
import math
from datetime import datetime
from typing import Any, Callable, Optional, Tuple

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))

import numpy as np
import pandas as pd

from storage import store_backtest_result
from atr import standard_atr

# Close-registry import is deferred until needed so backtests with no
# close_strategies don't pay the import cost. Uses ``close_registry_loader``
# to avoid the bare ``import registry`` collision with the open registry's
# module of the same name.
_close_registry = None

# Regime helper imported lazily so backtests without regime_enabled=True
# don't pay the import cost.
_ensure_regime_fn = None

# Post-TP SL helpers (#709). Loaded via spec_from_file_location to avoid the
# same registry-name collision that close_registry_loader works around — this
# module lives in shared_strategies/close/ but isn't a registered strategy,
# so we import it directly without going through the registry.
_post_tp_sl_module = None
_trailing_ratchet_module = None


def _load_regime():
    global _ensure_regime_fn
    if _ensure_regime_fn is None:
        from regime import ensure_regime_columns as _ensure_regime_columns
        _ensure_regime_fn = _ensure_regime_columns
    return _ensure_regime_fn


def _load_post_tp_sl():
    global _post_tp_sl_module
    if _post_tp_sl_module is not None:
        return _post_tp_sl_module
    import importlib.util
    name = "_go_trader_post_tp_sl"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "post_tp_sl.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    # Register in sys.modules BEFORE exec_module so @dataclass(frozen=True)
    # can resolve cls.__module__ via sys.modules during _is_type lookups.
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _post_tp_sl_module = mod
    return mod


def _load_trailing_ratchet():
    global _trailing_ratchet_module
    if _trailing_ratchet_module is not None:
        return _trailing_ratchet_module
    import importlib.util
    name = "_go_trader_trailing_ratchet"
    path = os.path.abspath(os.path.join(
        os.path.dirname(__file__), "..", "shared_strategies", "close", "trailing_tp_ratchet.py",
    ))
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    spec.loader.exec_module(mod)
    _trailing_ratchet_module = mod
    return mod


def _load_close_registry():
    global _close_registry
    if _close_registry is None:
        from close_registry_loader import evaluate as _evaluate, list_strategies as _list
        _close_registry = (_evaluate, _list)
    return _close_registry


_CLOSE_STRATEGIES_DIR = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "shared_strategies", "close")
)


def _ensure_close_strategies_path() -> None:
    """``regime_atr`` lives under ``shared_strategies/close``; add it to
    ``sys.path`` so the backtester can import the same module the close
    evaluators use without depending on PYTHONPATH."""
    if _CLOSE_STRATEGIES_DIR not in sys.path:
        sys.path.insert(0, _CLOSE_STRATEGIES_DIR)


def _rewrite_deprecated_close_ref(name: str, params: dict) -> tuple[str, dict]:
    """One-window shim: tp_at_pct → single-tier tiered_tp_pct (#841)."""
    if name != "tp_at_pct":
        return name, dict(params or {})
    pct = 0.03
    if params and params.get("pct") is not None:
        try:
            pct = max(float(params.get("pct", 0.03)), 0.0)
        except (TypeError, ValueError):
            pct = 0.03
    out = {
        "tp_tiers": [{"profit_pct": pct, "close_fraction": 1.0}],
    }
    if params and "sl_after" in params:
        out["sl_after"] = params["sl_after"]
    return "tiered_tp_pct", out


# Equity-curve points per year per timeframe — used to derive the Sharpe
# annualization factor. Crypto trades 24/7, so a 1d run has ~365 points/yr,
# a 4h run has ~365*6, etc. Hardcoding sqrt(365) overstated Sharpe by
# sqrt(periods_per_day) for any sub-daily timeframe (issue #304 M3).
TIMEFRAME_PERIODS_PER_YEAR = {
    "1m":  365 * 24 * 60,
    "5m":  365 * 24 * 12,
    "15m": 365 * 24 * 4,
    "30m": 365 * 24 * 2,
    "1h":  365 * 24,
    "2h":  365 * 12,
    "4h":  365 * 6,
    "6h":  365 * 4,
    "8h":  365 * 3,
    "12h": 365 * 2,
    "1d":  365,
    "1w":  52,
    "1M":  12,
}


def periods_per_year(timeframe: str) -> int:
    """Equity-curve samples per year for ``timeframe``; defaults to daily."""
    return TIMEFRAME_PERIODS_PER_YEAR.get(timeframe, 365)


# Taker fee rates per platform — mirrors scheduler/fees.go:CalculatePlatformSpotFee
# and related constants. test_platform_fees.py scrapes fees.go to enforce parity.
PLATFORM_FEE_PCT = {
    "binanceus":   0.001,    # BinanceSpotFeePct
    "hyperliquid": 0.00035,  # HyperliquidTakerFeePct
    "robinhood":   0.0,      # RobinhoodCryptoFeePct (no commission)
    "luno":        0.01,     # LunoTakerFeePct
    "okx":         0.001,    # OKXSpotTakerFeePct
    "okx-perps":   0.0005,   # OKXPerpsTakerFeePct
}


def fee_pct_for_platform(platform: str) -> float:
    """Return taker fee rate for ``platform``; defaults to BinanceUS spot rate
    (0.1%) to match ``scheduler/fees.go:CalculateSpotFee``."""
    return PLATFORM_FEE_PCT.get(platform, PLATFORM_FEE_PCT["binanceus"])


def _open_action_from_signal(signal: int) -> str:
    if signal > 0:
        return "long"
    if signal < 0:
        return "short"
    return "none"


def _close_refs_use_regime_tiered_tp(refs: list[dict]) -> bool:
    for ref in refs:
        n = (ref.get("name") or "").strip().lower()
        if n in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
            return True
    return False


def _normalize_open_action(value) -> str:
    action = str(value or "none").strip().lower()
    if action not in {"long", "short", "none"}:
        raise ValueError(
            "open_action column must contain only 'long', 'short', or 'none' "
            f"(got {value!r})"
        )
    return action


def _close_fraction_columns(df: pd.DataFrame) -> list[str]:
    return [
        c for c in df.columns
        if c == "close_fraction" or str(c).startswith("close_fraction:")
    ]


def _max_close_fraction_series(df: pd.DataFrame) -> pd.Series:
    cols = _close_fraction_columns(df)
    if not cols:
        return pd.Series(0.0, index=df.index)
    fractions = df[cols].fillna(0).astype(float)
    bad = (fractions < 0) | (fractions > 1)
    if bad.any().any():
        values = sorted(set(fractions[bad].stack().tolist()))
        raise ValueError(f"close_fraction values must be in [0, 1] — got {values}")
    return fractions.max(axis=1)


class Trade:
    """Represents a single round-trip trade."""
    def __init__(self, entry_date, entry_price, side="long"):
        self.entry_date = entry_date
        self.entry_price = entry_price
        self.side = side
        self.exit_date = None
        self.exit_price = None
        self.pnl = 0.0
        self.pnl_pct = 0.0
        self.shares = 0.0

    def close(self, exit_date, exit_price):
        self.exit_date = exit_date
        self.exit_price = exit_price
        if self.side == "long":
            self.pnl_pct = (exit_price - self.entry_price) / self.entry_price
        else:
            self.pnl_pct = (self.entry_price - exit_price) / self.entry_price
        self.pnl = self.shares * self.entry_price * self.pnl_pct

    def to_dict(self):
        return {
            "entry_date": str(self.entry_date),
            "exit_date": str(self.exit_date),
            "entry_price": self.entry_price,
            "exit_price": self.exit_price,
            "side": self.side,
            "shares": self.shares,
            "pnl": round(self.pnl, 2),
            "pnl_pct": round(self.pnl_pct * 100, 2),
        }


class Backtester:
    """
    Event-driven backtesting engine.

    Usage:
        bt = Backtester(initial_capital=1000)
        results = bt.run(df_with_signals, strategy_name="SMA Crossover")
    """

    def __init__(self, initial_capital: float = 1000.0,
                 commission_pct: Optional[float] = None,
                 slippage_pct: float = 0.0005,
                 platform: str = "binanceus",
                 open_strategy: Optional[dict] = None,
                 close_strategies: Optional[list[dict]] = None,
                 regime_enabled: bool = False,
                 regime_period: int = 14,
                 regime_adx_threshold: float = 20.0,
                 allowed_regimes: Optional[list[str]] = None,
                 stop_loss_atr_mult: Optional[float] = None,
                 stop_loss_pct: Optional[float] = None,
                 stop_loss_margin_pct: Optional[float] = None,
                 trailing_stop_atr_mult: Optional[float] = None,
                 trailing_stop_pct: Optional[float] = None,
                 stop_loss_atr_regime: Optional[dict] = None,
                 trailing_stop_atr_regime: Optional[dict] = None,
                 strategy_type: str = "perps",
                 direction: Optional[str] = None,
                 invert_signal: bool = False):
        """
        Args:
            initial_capital: Starting portfolio value.
            commission_pct: Commission per trade as fraction. If ``None``,
                derived from ``platform`` using ``PLATFORM_FEE_PCT`` (which
                mirrors ``scheduler/fees.go``). Pass an explicit value to
                override (e.g. in tests).
            slippage_pct: Slippage per trade as fraction (0.0005 = 5 bps).
            platform: Exchange fee model — one of ``PLATFORM_FEE_PCT`` keys.
                Unknown platforms fall back to BinanceUS (0.1%) with no
                warning, matching the Go dispatch default.
            open_strategy: Optional ``{"name": str, "params": dict}`` ref
                describing the open evaluator that produced the signal column
                on the DataFrame passed to ``run()``. The caller is responsible
                for actually applying the open strategy; this ref is recorded
                on the result for reporting parity with the live config (#641).
            close_strategies: Ordered list of close-evaluator refs, each
                ``{"name": str, "params": dict}``. The named evaluator must be
                registered in ``shared_strategies/close/registry.py``; per-ref
                ``params`` are merged over the registry's ``default_params`` at
                evaluation time. Each ref runs per-bar against the simulated
                position; the max ``close_fraction`` across refs wins (max-wins
                vs any column-based ``close_fraction`` mirrors the live
                composition contract). Replaces the pre-#641 parallel
                ``close_strategies: list[str]`` + ``close_params: dict`` pair.
        """
        self.initial_capital = initial_capital
        self.platform = platform
        self.commission_pct = (
            commission_pct if commission_pct is not None
            else fee_pct_for_platform(platform)
        )
        self.slippage_pct = slippage_pct
        self.open_strategy = dict(open_strategy or {})
        # Normalize close refs into the form the eval loop wants. Each ref must
        # have a non-empty `name`; missing/empty `params` becomes an empty dict.
        self._close_refs: list[dict] = []
        for ref in close_strategies or []:
            if not isinstance(ref, dict):
                raise ValueError(
                    f"close_strategies entries must be dicts of shape "
                    f"{{'name': str, 'params': dict}}, got {type(ref).__name__}"
                )
            name = (ref.get("name") or "").strip()
            if not name:
                raise ValueError(f"close_strategies ref missing 'name': {ref}")
            params = dict(ref.get("params") or {})
            name, params = _rewrite_deprecated_close_ref(name, params)
            self._close_refs.append({
                "name": name,
                "params": params,
            })
        # Derived views for the per-bar evaluation loop. The list preserves
        # caller-provided order; the params map is keyed by name. If a caller
        # passes the same name twice with different params, the map keeps only
        # the last write — both list iterations would then see the second
        # ref's params. This is fine under max-wins resolution but a footgun
        # if a future change ever reads param state per-iteration; reject
        # duplicates here if behavior depends on per-occurrence params.
        self.close_strategies = [r["name"] for r in self._close_refs]
        self.close_params = {r["name"]: r["params"] for r in self._close_refs}
        self.regime_enabled = regime_enabled
        self.regime_period = regime_period
        self.regime_adx_threshold = regime_adx_threshold
        self.allowed_regimes = list(allowed_regimes or [])
        self.stop_loss_atr_mult = stop_loss_atr_mult
        self.stop_loss_pct = stop_loss_pct
        self.stop_loss_margin_pct = stop_loss_margin_pct
        self.trailing_stop_atr_mult = trailing_stop_atr_mult
        self.trailing_stop_pct = trailing_stop_pct
        self.strategy_type = strategy_type
        # #942: live strategy-level entry transforms the backtester must mirror
        # so --config doesn't silently diverge from the daemon. ``invert_signal``
        # flips BUY<->SELL; ``direction`` gates which side may open. Both are
        # applied to the raw signal in ``run()`` (see _apply_direction_invert),
        # mirroring the live order (applySignalInversion before EffectiveDirection).
        self.direction = (str(direction).strip().lower() if direction else None)
        self.invert_signal = bool(invert_signal)
        self.stop_loss_atr_regime = (
            dict(stop_loss_atr_regime) if stop_loss_atr_regime else None
        )
        self.trailing_stop_atr_regime = (
            dict(trailing_stop_atr_regime) if trailing_stop_atr_regime else None
        )
        self._stop_loss_regime_block = None
        self._trailing_stop_regime_block = None
        self._uses_regime_tiered_close = _close_refs_use_regime_tiered_tp(
            self._close_refs,
        )
        self._uses_trailing_ratchet_close = any(
            (r.get("name") or "").strip().lower()
            in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime")
            for r in self._close_refs
        )
        self._ratchet_mod = None
        self._ratchet_ref: Optional[dict] = None
        self._ratchet_tiers_run: list = []
        if self._uses_trailing_ratchet_close:
            self._ratchet_mod = _load_trailing_ratchet()
            for ref in self._close_refs:
                n = (ref.get("name") or "").strip().lower()
                if n in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime"):
                    self._ratchet_ref = ref
                    break
            _regime_ratchet = (
                (self._ratchet_ref or {}).get("name") or ""
            ).strip().lower() == "trailing_tp_ratchet_regime"
            if _regime_ratchet:
                # #870: the regime variant's opening trail / SL owner is the
                # per-regime trailing_stop_atr_regime block (scalar mult rejected).
                if self.trailing_stop_atr_regime is None:
                    raise ValueError(
                        "trailing_tp_ratchet_regime requires trailing_stop_atr_regime"
                    )
            elif (
                self.trailing_stop_atr_mult is None
                or self.trailing_stop_atr_mult <= 0
            ):
                raise ValueError(
                    "trailing_tp_ratchet requires trailing_stop_atr_mult > 0"
                )
            if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                raise ValueError(
                    "trailing_tp_ratchet* cannot combine with trailing_stop_pct"
                )
        _needs_regime_atr = (
            self.stop_loss_atr_regime is not None
            or self.trailing_stop_atr_regime is not None
            or self._uses_regime_tiered_close
        )
        if _needs_regime_atr:
            _ensure_close_strategies_path()
            from regime_atr import (  # type: ignore
                SURFACE_STOP_LOSS,
                SURFACE_TRAILING,
                parse_regime_atr_block,
                resolve_regime_atr,
            )

            regime_errs: list[str] = []
            if self.stop_loss_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.stop_loss_atr_regime,
                    "stop_loss_atr_regime",
                    SURFACE_STOP_LOSS,
                )
                regime_errs.extend(errs)
                self._stop_loss_regime_block = blk
            if self.trailing_stop_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.trailing_stop_atr_regime,
                    "trailing_stop_atr_regime",
                    SURFACE_TRAILING,
                )
                regime_errs.extend(errs)
                self._trailing_stop_regime_block = blk
            if regime_errs:
                raise ValueError(
                    "Invalid regime ATR stop configuration: " + "; ".join(regime_errs)
                )

            def _active_regime_sl(blk) -> bool:
                return blk is not None and not blk.is_zero()

            if _active_regime_sl(self._stop_loss_regime_block):
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_atr_mult"
                    )
                if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_pct"
                    )
                if (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "stop_loss_margin_pct"
                    )
                if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trailing_stop_pct"
                    )
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trailing_stop_atr_mult"
                    )
                if _active_regime_sl(self._trailing_stop_regime_block):
                    raise ValueError(
                        "stop_loss_atr_regime is mutually exclusive with "
                        "trailing_stop_atr_regime"
                    )

            if _active_regime_sl(self._trailing_stop_regime_block):
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    raise ValueError(
                        "trailing_stop_atr_regime is mutually exclusive with "
                        "trailing_stop_atr_mult"
                    )
                if self.trailing_stop_pct is not None and self.trailing_stop_pct > 0:
                    raise ValueError(
                        "trailing_stop_atr_regime is mutually exclusive with "
                        "trailing_stop_pct"
                    )
                if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    raise ValueError(
                        "trailing_stop_atr_regime is mutually exclusive with "
                        "stop_loss_pct"
                    )
                if (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                ):
                    raise ValueError(
                        "trailing_stop_atr_regime is mutually exclusive with "
                        "stop_loss_margin_pct"
                    )
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    raise ValueError(
                        "trailing_stop_atr_regime is mutually exclusive with "
                        "stop_loss_atr_mult"
                    )
            self._resolve_regime_atr = resolve_regime_atr
        else:
            self._resolve_regime_atr = None  # type: ignore[assignment]
            _evaluate, list_strategies = _load_close_registry()
            available = set(list_strategies())
            for name in self.close_strategies:
                if name not in available:
                    raise ValueError(
                        f"Unknown close strategy: {name}. "
                        f"Available: {sorted(available)}"
                    )

        # Parse sl_after rules and the corresponding tier close-fraction
        # thresholds once at init. When no sl_after is configured this is a
        # no-op and the per-bar SL machinery in run() short-circuits.
        self._sl_mod = _load_post_tp_sl()
        self._sl_after_rules_static, _sl_parse_errs = (
            self._sl_mod.parse_strategy_tp_sl_after_rules(self._close_refs)
        )
        self._tp_tier_thresholds_static = self._sl_mod.parse_tp_tier_close_fractions(
            self._close_refs,
        )
        # Per-run mutable views (``run()`` re-seeds at each open). Unit tests
        # that call ``_maybe_apply_sl_after`` directly expect these attrs to
        # exist without going through ``run()``.
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._run_position_regime = ""
        # Validation parity with the live config — reject the same bad combos
        # at backtest-load time so users can't silently get a no-op. Run
        # whenever ANY close ref carries an `sl_after` key (or the tiered
        # parser reported errors), so misplaced keys on non-tiered refs are
        # also surfaced — matching the live validator's "fail loud at load"
        # behavior in scheduler/post_tp_sl.go.
        any_sl_after_key = False
        for ref in self._close_refs:
            params = ref.get("params") or {}
            if "sl_after" in params:
                any_sl_after_key = True
                break
            tiers_raw = params.get("tp_tiers", params.get("tiers"))  # #841 canonical key
            if isinstance(tiers_raw, list) and any(
                isinstance(t, dict) and "sl_after" in t for t in tiers_raw
            ):
                any_sl_after_key = True
                break
        self._any_sl_after_key = any_sl_after_key
        # For regime-tiered closes, ``parse_strategy_tp_sl_after_rules(..., regime=None)``
        # leaves ``per_tier`` empty at load time; ``stamp_open_from_label`` in
        # ``run()`` reparses with the stamped regime before any tier fires.
        self._sl_after_pipeline_enabled = (
            self._sl_after_rules_static.has_any() or any_sl_after_key
        )
        if (
            self._sl_after_rules_static.has_any()
            or _sl_parse_errs
            or any_sl_after_key
        ):
            errs = self._sl_mod.validate_post_tp_stop_loss_rules(
                self._close_refs,
                stop_loss_atr_mult=self.stop_loss_atr_mult,
                stop_loss_pct=self.stop_loss_pct,
                stop_loss_margin_pct=self.stop_loss_margin_pct,
                trailing_stop_atr_mult=self.trailing_stop_atr_mult,
                trailing_stop_pct=self.trailing_stop_pct,
                stop_loss_atr_regime=self.stop_loss_atr_regime,
                strategy_type=self.strategy_type,
            )
            if errs:
                raise ValueError(
                    "Invalid sl_after configuration: " + "; ".join(errs)
                )
            # The backtester does not carry leverage context, so the
            # margin-pct branch of EffectiveStopLossPct can't be modeled here
            # (_initial_sl_trigger returns 0 for margin-pct-only configs).
            # The live validator accepts margin_pct as satisfying the
            # "fixed SL" precondition, so we reject loudly here rather than
            # silently produce a backtest where the pre-bump SL never fires.
            if self._sl_after_rules_static.has_any():
                # #736 explicitly defers regime-aware sl_after backtester
                # parity to the parallel parity issue. Parsing the shape works
                # (so live configs round-trip), but the per-bar engine here
                # would silently fall back to zero scalars — atr_offset regime
                # collapses to breakeven and trail_from_here regime defers
                # every bar. Fail loud at load instead of producing results
                # that look right but ignore the per-regime values.
                regime_rules = []
                if self._sl_after_rules_static.default.has_regime():
                    regime_rules.append("strategy-level default")
                for idx, r in enumerate(self._sl_after_rules_static.per_tier):
                    if r.has_regime():
                        regime_rules.append(f"tier[{idx}]")
                if regime_rules:
                    raise ValueError(
                        "Invalid sl_after configuration: regime-aware "
                        "trend_regime block is HL-live-only in this release "
                        "(backtester parity deferred — see #736). Found on: "
                        + ", ".join(regime_rules)
                        + ". Use the scalar atr_mult / trail_from_here.atr_mult "
                        "form for backtesting."
                    )
                has_atr_sl = (
                    (
                        self.stop_loss_atr_mult is not None
                        and self.stop_loss_atr_mult > 0
                    )
                    or (
                        self._stop_loss_regime_block is not None
                        and not self._stop_loss_regime_block.is_zero()
                    )
                )
                has_pct_sl = (
                    self.stop_loss_pct is not None and self.stop_loss_pct > 0
                )
                has_margin_sl = (
                    self.stop_loss_margin_pct is not None
                    and self.stop_loss_margin_pct > 0
                )
                if has_margin_sl and not (has_atr_sl or has_pct_sl):
                    raise ValueError(
                        "Invalid sl_after configuration: "
                        "stop_loss_margin_pct cannot be the sole fixed SL "
                        "in backtests — the backtester does not model "
                        "leverage, so the pre-TP SL would never fire and "
                        "the post-TP bump would diverge from live. Use "
                        "stop_loss_atr_mult or stop_loss_pct."
                    )

    def _apply_direction_invert(self, sig_int: pd.Series,
                                uses_open_close: bool) -> pd.Series:
        """Apply live ``invert_signal`` then ``direction`` gating to the raw
        integer signal (#942).

        Mirrors the live scheduler ordering: ``applySignalInversion`` flips
        BUY<->SELL (scheduler/main.go) BEFORE ``EffectiveDirection`` /
        ``PerpsOrderSkipReason`` gate which side may OPEN. Both are integer
        frame transforms on ``{-1, 0, 1}``.

        Direction masks the signal that would OPEN a disallowed side. The
        signal's meaning is path-dependent, so masking is too:

          - open/close path (``uses_open_close``): ``signal>0`` opens long,
            ``signal<0`` opens short, and closes come from the close evaluator.
            Masking the disallowed open side is exact and never suppresses a
            close.
          - plain long/flat path: ``signal=1`` opens long, ``signal=-1`` only
            *closes* the long (a short is never opened). It is structurally
            long-only, so ``direction="long"`` already matches live and needs
            no mask; ``"short"``/``"both"`` are unmodelable here and are
            rejected at config load (``run_backtest.load_strategy_config``).
            Masking ``-1`` in this path would wrongly suppress long-closes, so
            it is intentionally skipped.
        """
        sig = sig_int
        if self.invert_signal:
            # ``-sig`` keeps the {-1, 0, 1} domain (0 stays 0).
            sig = -sig
        d = self.direction or ""
        if uses_open_close and d in ("long", "short"):
            if d == "long":
                # Disallow short opens: drop signal<0, keep long-opens/holds.
                sig = sig.where(sig >= 0, 0)
            else:  # "short"
                # Disallow long opens: drop signal>0, keep short-opens/holds.
                sig = sig.where(sig <= 0, 0)
        return sig.astype(int)

    def run(self, df: pd.DataFrame, strategy_name: str = "Unknown",
            symbol: str = "BTC/USDT", timeframe: str = "1d",
            params: Optional[dict] = None, save: bool = True,
            starting_long: Optional[dict] = None) -> dict:
        """
        Run backtest on a DataFrame that already has a 'signal' column.
        signal: 1 = buy, -1 = sell, 0 = hold

        Execution model matches the live scheduler: a signal produced by the
        close of bar t is read after the bar finishes and filled at bar t+1's
        open (no look-ahead bias). Falls back to close when an ``open`` column
        is not present.

        starting_long: optional dict with keys ``entry_price`` (float, USD),
            ``entry_date`` (index value, defaults to df.index[0]), and
            optional ``entry_atr`` (float, used to stamp the seeded
            position's EntryATR so ATR-based close evaluators like
            ``tiered_tp_atr`` work across walk-forward fold boundaries).
            When provided, the run begins already-long: the full
            ``initial_capital`` is treated as committed at ``entry_price``
            (minus one commission for the implicit buy). Use for carrying
            walk-forward position state across a fold boundary so SELL
            signals in the first train bars actually close the warmup
            position instead of being dropped as "sell while flat".
            Note: ``equity[0]`` for a seeded run reflects the starting
            position's mark-to-market (``shares * close[0]``), not
            ``initial_capital``. ``_calculate_metrics`` anchors
            ``total_return_pct`` and ``max_drawdown_pct`` at
            ``self.initial_capital`` so the baseline is consistent with
            unseeded runs, while ``sharpe`` and ``volatility`` are
            computed from ``pct_change()`` and are unaffected.

        Returns dict with all performance metrics.
        """
        uses_open_close = (
            "open_action" in df.columns
            or bool(_close_fraction_columns(df))
            or bool(self.close_strategies)
        )
        if "signal" not in df.columns and not uses_open_close:
            raise ValueError("DataFrame must have a 'signal' column or open_action/close_fraction columns")

        df = df.copy()
        if "signal" in df.columns:
            # Contract: signal ∈ {-1, 0, 1}. position.diff() emits ±1.0 floats
            # and some strategies emit ints; coerce NaN → 0, reject non-integral
            # floats before casting, and then reject any out-of-domain integer.
            sig_raw = df["signal"].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"non-integral values {sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"signal column must be in {{-1, 0, 1}} — got "
                    f"unexpected values {sorted(bad.unique().tolist())}"
                )
            # #942: apply the live entry transforms (invert_signal, then
            # direction gating) to the raw signal BEFORE the open snapshot and
            # the look-ahead shift, so both ``signal_for_open`` and the shifted
            # ``df["signal"]`` see the gated values. No-op unless --config wired
            # direction/invert_signal from the live strategy entry.
            sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            signal_for_open = sig_int
            df["signal"] = sig_int.shift(1).fillna(0).astype(int)
        else:
            signal_for_open = pd.Series(0, index=df.index)
            df["signal"] = 0

        if uses_open_close:
            if "open_action" in df.columns:
                open_actions = df["open_action"].map(_normalize_open_action)
            else:
                open_actions = signal_for_open.map(_open_action_from_signal)
            df["_open_action"] = open_actions.shift(1).fillna("none")
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)

        # Regime: inject vectorized labels before the per-bar loop so each bar
        # can gate new entries. Mirrors the live path: latest_regime(df) on the
        # same OHLCV window → identical label by construction (same algorithm).
        if self.regime_enabled and "regime" not in df.columns:
            ensure_regime = _load_regime()
            ensure_regime(df, period=self.regime_period, adx_threshold=self.regime_adx_threshold)

        # Snapshot the bar-close regime label before any shift so close
        # evaluators that re-resolve per bar (``tiered_tp_atr_live_regime``)
        # read the same ``ensure_regime_columns`` output as live (#737).
        if "regime" in df.columns:
            df["_regime_bar_close"] = df["regime"].copy()

        # Shift regime to match the signal shift in the signal-normalization
        # block above. In live, the regime label is computed from bar N's
        # closed data at the same moment as the signal; both gate the order
        # that fills at bar N+1's open. Here the signal is already shifted
        # forward by one row, so the regime consumed at row N+1 must be bar
        # N's regime — not the regime that would only be knowable after bar
        # N+1 closes. Without this shift, the entry gate in the per-bar loop
        # reads a future bar's regime relative to the decision time, which
        # is look-ahead bias (#730).
        #
        # Empty/missing regime (e.g. row 0 after shift, or mid-series NaN
        # rows from upstream gaps) → empty string after fillna, which fails
        # the ``in allowed_regimes`` check and blocks the entry. That matches
        # live behavior: no regime data, no entry. Intentional — do not
        # "fix" the fillna to forward-fill or interpolate.
        if self.regime_enabled and "regime" in df.columns:
            df["regime"] = df["regime"].shift(1).fillna("")

        has_open = "open" in df.columns

        def _entry_stamp(row) -> str:
            if self.regime_enabled:
                return str(row.get("regime", "") or "").strip()
            return str(row.get("_regime_bar_close", "") or "").strip()

        def _bar_close_regime(row) -> str:
            return str(row.get("_regime_bar_close", "") or "").strip()

        cash = self.initial_capital
        position = 0.0  # shares held
        trades = []
        current_trade = None
        equity_curve = []

        # Position context for close-strategy evaluators. Stamped at open,
        # cleared at full close. ``initial_quantity`` is preserved across
        # partial closes so tiered evaluators can compute incremental
        # ``close_fraction`` correctly (mirrors live ``Position.InitialQuantity``).
        avg_cost = 0.0
        initial_quantity = 0.0
        entry_atr_value = 0.0
        pending_close_fraction = 0.0

        # Post-TP SL adjustment state (#709). Only meaningful when sl_after is
        # configured; otherwise the per-bar machinery short-circuits and these
        # values are never consulted.
        sl_trigger_px = 0.0
        sl_tiers_processed = 0
        post_tp_trail_mult: Optional[float] = None
        sl_high_water_px = 0.0

        # Standalone hard-stop state for the simple signal path (no close
        # strategy). The open/close pipeline above seeds its own SL trigger from
        # the sl_after/TP machinery; the plain signal path has none, so a bare
        # stop_loss_atr_mult / trailing_stop_atr_mult would otherwise no-op. A
        # hit is detected at bar close and fills at the next bar's open, matching
        # the engine's N→N+1 fill convention.
        pending_signal_sl_close = False
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._run_position_regime = ""
        sl_after_active = self._sl_after_pipeline_enabled
        trailing_ratchet_active = self._uses_trailing_ratchet_close

        atr_series = df["atr"] if "atr" in df.columns else None
        # An ATR-multiple stop/trail needs an `atr` series to stamp entry_atr;
        # without it the stop silently no-ops (entry_atr stays 0). Strategies that
        # don't emit `atr` (e.g. momentum_pro) would otherwise run stopless. Inject
        # a standard ATR(14) so scalar ATR stops work for any open strategy, not
        # only those paired with a close evaluator that pre-injects it.
        if atr_series is None and (
            (self.stop_loss_atr_mult is not None and self.stop_loss_atr_mult > 0)
            or (self.trailing_stop_atr_mult is not None and self.trailing_stop_atr_mult > 0)
        ):
            atr_series = standard_atr(df)

        def _initial_trail_trigger(side: str, mark: float, entry_atr: float,
                                    trail_mult: float) -> float:
            if mark <= 0 or entry_atr <= 0 or trail_mult <= 0:
                return 0.0
            if side == "long":
                return mark - trail_mult * entry_atr
            if side == "short":
                return mark + trail_mult * entry_atr
            return 0.0

        def stamp_open_from_label(stamp: str) -> None:
            lab = (stamp or "").strip()
            self._run_position_regime = lab
            if self._uses_regime_tiered_close:
                rules_rt, _ = self._sl_mod.parse_strategy_tp_sl_after_rules(
                    self._close_refs, regime=lab,
                )
                self._active_sl_after_rules = rules_rt
                self._run_tp_tier_thresholds = self._sl_mod.parse_tp_tier_close_fractions(
                    self._close_refs, regime=lab,
                )
            else:
                self._active_sl_after_rules = self._sl_after_rules_static
                self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)

            self._ratchet_tiers_run = []
            if self._uses_trailing_ratchet_close and self._ratchet_mod and self._ratchet_ref:
                regime_table = (
                    (self._ratchet_ref.get("name") or "").strip().lower()
                    == "trailing_tp_ratchet_regime"
                )
                tiers, terr = self._ratchet_mod.resolve_tiers_for_regime(
                    self._ratchet_ref.get("params") or {},
                    lab,
                    regime_table=regime_table,
                )
                if terr:
                    raise ValueError(
                        "trailing_tp_ratchet tier resolution failed: "
                        + "; ".join(terr)
                    )
                self._ratchet_tiers_run = tiers

            self._run_stop_loss_atr_mult = None
            self._run_trailing_stop_atr_mult = None
            if self._resolve_regime_atr is not None and lab:
                if (
                    self._stop_loss_regime_block is not None
                    and not self._stop_loss_regime_block.is_zero()
                ):
                    self._run_stop_loss_atr_mult = self._resolve_regime_atr(
                        self._stop_loss_regime_block, lab,
                    )
                if (
                    self._trailing_stop_regime_block is not None
                    and not self._trailing_stop_regime_block.is_zero()
                ):
                    self._run_trailing_stop_atr_mult = self._resolve_regime_atr(
                        self._trailing_stop_regime_block, lab,
                    )
            if self._run_stop_loss_atr_mult is None:
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                ):
                    self._run_stop_loss_atr_mult = self.stop_loss_atr_mult
            if self._run_trailing_stop_atr_mult is None:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                ):
                    self._run_trailing_stop_atr_mult = self.trailing_stop_atr_mult

        if starting_long:
            effective_entry = starting_long["entry_price"]
            entry_commission = self.initial_capital * self.commission_pct
            available = self.initial_capital - entry_commission
            position = available / effective_entry
            cash = 0.0
            current_trade = Trade(
                starting_long.get("entry_date", df.index[0]),
                effective_entry, "long",
            )
            current_trade.shares = position
            avg_cost = effective_entry
            initial_quantity = position
            # Optional ATR for the seeded position so walk-forward folds with
            # ATR-based close evaluators (tiered_tp_atr) don't silently no-op
            # for the seeded position's lifetime. Same plausibility guard as
            # _stamp_entry_atr (rejects non-positive and >50% of entry price).
            seed_atr = starting_long.get("entry_atr", 0.0)
            try:
                seed_atr = float(seed_atr or 0.0)
            except (TypeError, ValueError):
                seed_atr = 0.0
            if seed_atr > 0 and seed_atr <= 0.5 * effective_entry:
                entry_atr_value = seed_atr
            stamp = str(starting_long.get("entry_regime", "") or "").strip()
            if not stamp:
                stamp = _entry_stamp(df.iloc[0])
            stamp_open_from_label(stamp)

        for i, (idx, row) in enumerate(df.iterrows()):
            fill_price = row["open"] if has_open else row["close"]
            mark_price = row["close"]
            signal = row["signal"]

            # Per-bar reset: when _maybe_apply_sl_after bumps the SL trigger on
            # this bar, the end-of-bar block (both the trail-walker HWM update
            # and the SL hit check) must skip — live places the new SL OID
            # mid-cycle after the TP fill, and the backtester's bar-level
            # granularity collapses that delay to zero. Without the gate, a
            # bar that bumps SL to (say) breakeven and then closes below
            # breakeven would fire SL on the same bar; live would not, because
            # the bump and the close happen at different intra-bar moments.
            # Skipping the walker on the bump bar is also correct: live
            # wouldn't walk the HWM until the next cycle either, and the
            # trail_from_here path inside _maybe_apply_sl_after already seeds
            # the HWM at the partial-close fill price. See #715.
            sl_after_just_applied = False

            equity = cash + position * mark_price
            equity_curve.append({"date": idx, "equity": equity})

            # Regime gate: block new entries when the prior bar's regime
            # isn't in the allowed set. Existing positions are always managed
            # by close paths. ``compute_regime`` initializes every row to
            # ``"ranging"`` (warmup bars included). After the post-injection
            # shift (#730) row 0 is empty — that empty label fails the
            # ``in allowed_regimes`` check and blocks the bar-0 entry, which
            # is correct (no prior-bar data, no decision).
            bar_regime = str(row.get("regime", "")) if self.regime_enabled else ""
            regime_blocked = (
                self.regime_enabled
                and bool(self.allowed_regimes)
                and bar_regime not in self.allowed_regimes
            )

            if uses_open_close:
                col_close_fraction = float(row.get("_close_fraction", 0.0))
                close_fraction = max(col_close_fraction, pending_close_fraction)
                pending_close_fraction = 0.0
                open_action = row.get("_open_action", "none")

                if close_fraction > 0 and position != 0:
                    qty_to_close = abs(position) * min(close_fraction, 1.0)
                    if position > 0:
                        effective_price = fill_price * (1 - self.slippage_pct)
                        proceeds = qty_to_close * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                        position -= qty_to_close
                    else:
                        effective_price = fill_price * (1 + self.slippage_pct)
                        cost = qty_to_close * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                        position += qty_to_close

                    if current_trade:
                        closed = Trade(current_trade.entry_date, current_trade.entry_price, current_trade.side)
                        closed.shares = qty_to_close
                        closed.close(idx, effective_price)
                        closed.pnl -= commission
                        trades.append(closed)
                        current_trade.shares -= qty_to_close
                        if current_trade.shares <= 1e-12:
                            current_trade = None

                    if abs(position) <= 1e-12:
                        position = 0.0
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        # Reset post-TP SL state on full close so the next
                        # open starts clean.
                        sl_trigger_px = 0.0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                        sl_after_just_applied = False
                        self._active_sl_after_rules = self._sl_after_rules_static
                        self._run_tp_tier_thresholds = list(
                            self._tp_tier_thresholds_static,
                        )
                        self._run_stop_loss_atr_mult = None
                        self._run_trailing_stop_atr_mult = None
                        self._run_position_regime = ""
                    elif sl_after_active and self._run_tp_tier_thresholds:
                        # After applying a partial close at this bar's open,
                        # detect which tier(s) just cleared and apply the
                        # highest cleared tier's sl_after rule. The end-of-bar
                        # SL hit check is suppressed on bars where the trigger
                        # actually moved (see sl_after_just_applied init at
                        # loop top and the gate at the SL hit check below) —
                        # this models the live delay between TP fill and the
                        # new SL OID landing on-chain. See #715.
                        side_now = "long" if position > 0 else "short"
                        prev_trigger = sl_trigger_px
                        prev_post_tp_trail = post_tp_trail_mult
                        sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, \
                            sl_high_water_px = self._maybe_apply_sl_after(
                                side=side_now,
                                avg_cost=avg_cost,
                                entry_atr=entry_atr_value,
                                position_qty=abs(position),
                                initial_qty=initial_quantity,
                                mark_price=mark_price,
                                fill_price=fill_price,
                                sl_trigger_px=sl_trigger_px,
                                sl_tiers_processed=sl_tiers_processed,
                                post_tp_trail_mult=post_tp_trail_mult,
                                sl_high_water_px=sl_high_water_px,
                            )
                        # Only set the suppression flag when an actual bump
                        # occurred — empty-rule tier advances (watermark
                        # increments without trigger change) leave the SL
                        # untouched, so the hit check should still run.
                        if (
                            sl_trigger_px != prev_trigger
                            or post_tp_trail_mult != prev_post_tp_trail
                        ):
                            sl_after_just_applied = True

                if open_action == "long" and position == 0 and not regime_blocked:
                    effective_price = fill_price * (1 + self.slippage_pct)
                    commission = cash * self.commission_pct
                    available = cash - commission
                    shares = available / effective_price
                    position = shares
                    cash = 0.0

                    current_trade = Trade(idx, effective_price, "long")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    stamp_open_from_label(_entry_stamp(row))
                    # #716 item 3: seed the SL trigger only when sl_after has
                    # usable tier thresholds. Without thresholds, the post-TP
                    # adjustment machinery never fires (`_maybe_apply_sl_after`
                    # is gated on `self._run_tp_tier_thresholds`), so a seeded-
                    # then-never-adjusted trigger would represent a phantom
                    # fixed SL the rest of the engine doesn't actually
                    # simulate. Mirrors the live shape, where the fixed SL is
                    # placed by `runHyperliquidProtectionSync` independently
                    # of `sl_after` configuration.
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "long", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif open_action == "short" and position == 0 and not regime_blocked:
                    effective_price = fill_price * (1 - self.slippage_pct)
                    commission = cash * self.commission_pct
                    notional = cash - commission
                    shares = notional / effective_price
                    cash = 2 * notional  # pay commission, receive short-sale proceeds
                    position = -shares

                    current_trade = Trade(idx, effective_price, "short")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    stamp_open_from_label(_entry_stamp(row))
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "short", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price

                # End-of-bar: evaluate close strategies against the now-current
                # position using this bar's close as the mark. The result is
                # applied at the NEXT bar's open (mirrors live: eval at end of
                # bar, fill at next open).
                if self.close_strategies and position != 0 and avg_cost > 0:
                    pending_close_fraction = self._evaluate_close_strategies(
                        position, avg_cost, initial_quantity, entry_atr_value,
                        mark_price, atr_series, idx,
                        position_regime=self._run_position_regime,
                        market_regime=_bar_close_regime(row),
                    )
                    if (
                        trailing_ratchet_active
                        and self._ratchet_mod
                        and self._ratchet_tiers_run
                        and position != 0
                        and entry_atr_value > 0
                    ):
                        side_now = "long" if position > 0 else "short"
                        base_trail = self._run_trailing_stop_atr_mult or 0.0
                        sl_tiers_processed, post_tp_trail_mult = (
                            self._ratchet_mod.maybe_apply_mark_ratchet(
                                self._ratchet_tiers_run,
                                watermark=sl_tiers_processed,
                                mark_price=mark_price,
                                avg_cost=avg_cost,
                                entry_atr=entry_atr_value,
                                side=side_now,
                                post_tp_trail_mult=post_tp_trail_mult,
                                trailing_stop_atr_mult=base_trail,
                            )
                        )

                # End-of-bar: walk the trailing-stop high-water mark (only
                # active after a TP tier transitioned the position to
                # trail_from_here mode) and check whether the SL trigger has
                # been hit by this bar's close. A hit produces
                # pending_close_fraction=1.0 which fills at the next bar's
                # open — same alignment as the rest of the close pipeline.
                if (
                    (sl_after_active or trailing_ratchet_active)
                    and not sl_after_just_applied
                    and position != 0
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    trail_mult = post_tp_trail_mult
                    if trail_mult is None or trail_mult <= 0:
                        trail_mult = self._run_trailing_stop_atr_mult
                    if (
                        trail_mult is not None
                        and trail_mult > 0
                        and entry_atr_value > 0
                    ):
                        sl_trigger_px, sl_high_water_px = self._walk_trail(
                            side=side_now,
                            mark_price=mark_price,
                            entry_atr=entry_atr_value,
                            trail_mult=trail_mult,
                            sl_trigger_px=sl_trigger_px,
                            sl_high_water_px=sl_high_water_px,
                        )
                    if sl_trigger_px > 0 and self._sl_hit(
                        side_now, mark_price, sl_trigger_px,
                    ):
                        pending_close_fraction = 1.0
                continue

            # Standalone hard stop fires first: close at this bar's open before
            # any new signal is acted on (the hit was flagged on the prior bar's
            # close, so this is the next-bar-open fill).
            if pending_signal_sl_close and position > 0:
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash = proceeds - commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                continue

            # NOTE: this signal path is long/flat only — signal == 1 opens a
            # long, signal == -1 only *closes* it; a short is never opened. So
            # OOS validation of bidirectional strategies (momentum_pro,
            # mean_reversion_pro, consolidation_range) exercises the LONG side
            # only; their live short signals are not covered by backtest.
            if signal == 1 and position == 0 and not regime_blocked:
                # BUY — go long with all available cash
                effective_price = fill_price * (1 + self.slippage_pct)
                commission = cash * self.commission_pct
                available = cash - commission
                shares = available / effective_price
                position = shares
                cash = 0.0

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares

                # Seed a standalone stop for the plain signal path (fixed ATR
                # mult > trailing ATR mult > fixed pct). entry_atr is the
                # closed-bar ATR at the fill bar (same convention as the
                # open/close path's _stamp_entry_atr).
                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost - self.stop_loss_atr_mult * entry_atr_value
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price - self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 - self.stop_loss_pct)

            elif signal == -1 and position > 0:
                # SELL — close long position
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                cash = proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0

            # End-of-bar: for a trailing ATR stop, ratchet the trigger up on new
            # highs; then check whether this bar's close breached the trigger.
            # A hit fills at the next bar's open via pending_signal_sl_close.
            if position > 0 and sl_trigger_px > 0:
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price > sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px - self.trailing_stop_atr_mult * entry_atr_value
                    if candidate > sl_trigger_px:
                        sl_trigger_px = candidate
                if self._sl_hit("long", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True

        # Close any open position at the end
        if position != 0:
            if position > 0:
                final_price = df["close"].iloc[-1] * (1 - self.slippage_pct)
                proceeds = position * final_price
                commission = proceeds * self.commission_pct
                cash += proceeds - commission
            else:
                final_price = df["close"].iloc[-1] * (1 + self.slippage_pct)
                cost = abs(position) * final_price
                commission = cost * self.commission_pct
                cash -= cost + commission
            position = 0.0

            if current_trade:
                current_trade.close(df.index[-1], final_price)
                trades.append(current_trade)

        final_equity = cash
        equity_df = pd.DataFrame(equity_curve).set_index("date")

        # Calculate metrics
        metrics = self._calculate_metrics(equity_df, trades, df, timeframe)
        # Resolve the open strategy ref for reporting. Caller can supply it
        # in __init__ (preferred — matches the live config shape from #640) or
        # via run()'s strategy_name + params (legacy path).
        open_ref = dict(self.open_strategy) if self.open_strategy else {}
        if not open_ref.get("name") and strategy_name:
            open_ref["name"] = strategy_name
        if "params" not in open_ref and params:
            open_ref["params"] = dict(params)
        metrics.update({
            "strategy_name": open_ref.get("name") or strategy_name,
            "symbol": symbol,
            "timeframe": timeframe,
            "start_date": str(df.index[0]),
            "end_date": str(df.index[-1]),
            "initial_capital": self.initial_capital,
            "final_capital": round(final_equity, 2),
            "params": open_ref.get("params") or params or {},
            "open_strategy": open_ref,
            "close_strategies": [dict(r) for r in self._close_refs],
            "trades": [t.to_dict() for t in trades],
        })

        if save:
            store_backtest_result(metrics)

        return metrics

    def _stamp_entry_atr(self, atr_series: Optional[pd.Series], idx,
                         entry_price: float) -> float:
        """Return the ATR at ``idx`` for stamping ``Position.EntryATR``.

        Mirrors ``stampEntryATRIfOpened`` in scheduler/main.go: rejects NaN
        and any value greater than 50% of the entry price as a plausibility
        guard. Returns 0.0 when no usable ATR is available — close evaluators
        that require ATR (``tiered_tp_atr``) then fall through with a no-op
        until a position with a valid ATR is opened.
        """
        if atr_series is None or entry_price <= 0:
            return 0.0
        try:
            value = float(atr_series.loc[idx])
        except (KeyError, TypeError, ValueError):
            return 0.0
        if not (value > 0):  # rejects NaN, 0, negative
            return 0.0
        if value > 0.5 * entry_price:
            return 0.0
        return value

    def _evaluate_close_strategies(self, position: float, avg_cost: float,
                                   initial_quantity: float,
                                   entry_atr_value: float,
                                   mark_price: float,
                                   atr_series: Optional[pd.Series],
                                   idx,
                                   *,
                                   position_regime: str = "",
                                   market_regime: str = "") -> float:
        """Run every configured close evaluator against the simulated position
        and return the max ``close_fraction``. Same max-wins resolution as the
        live composition flow in shared_tools/strategy_composition.py.
        """
        evaluate, _list_strategies = _load_close_registry()
        side = "long" if position > 0 else "short"
        position_dict = {
            "side": side,
            "avg_cost": float(avg_cost),
            "current_quantity": float(abs(position)),
            "initial_quantity": float(initial_quantity or abs(position)),
            "entry_atr": float(entry_atr_value),
            "regime": str(position_regime or ""),
        }
        # Always pass ``regime`` (possibly empty) so live-regime evaluators see
        # the same key shape as live check scripts — empty/NaN bars no-op with
        # an explicit label instead of a missing dict key (#747 review).
        market_dict = {
            "mark_price": float(mark_price),
            "regime": str(market_regime or ""),
        }
        if atr_series is not None:
            # Current-bar ATR access is intentional and matches live (#730):
            # close evaluators run end-of-bar with this bar's closed mark and
            # this bar's closed ATR; the resulting close_fraction becomes
            # pending_close_fraction and applies at the NEXT bar's open. This
            # is the live ``tiered_tp_atr_live`` contract — see CLAUDE.md
            # "ATR for close evaluators". Entries, by contrast, gate on the
            # PRIOR bar's regime/signal via the shifts in the
            # signal-normalization and regime-shift blocks at the top of
            # ``run()`` — different timing for different decision points,
            # both matching live.
            try:
                live_atr = float(atr_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                live_atr = 0.0
            if live_atr > 0:
                market_dict["atr"] = live_atr

        best = 0.0
        for name in self.close_strategies:
            params = self.close_params.get(name)
            result = evaluate(name, position_dict, market_dict, params)
            fraction = float(result.get("close_fraction", 0.0) or 0.0)
            if fraction > best:
                best = fraction
                if best >= 1.0:
                    # Full close already wins — remaining evaluators can't change the outcome.
                    return 1.0
        return min(max(best, 0.0), 1.0)

    def _initial_sl_trigger(self, side: str, avg_cost: float,
                            entry_atr: float) -> float:
        """Seed the simulated SL trigger at open from the strategy's fixed SL
        config (#709). Returns 0.0 when no usable fixed SL is configured —
        the post-TP machinery still tracks tier fills and will start
        adjusting once a TP fires; the gate at the run loop just won't fire
        a pre-TP SL hit because the trigger is 0.

        Mirrors the live priority order: ATR-based SL > pct-based SL. The
        margin-pct branch is intentionally not modeled here (requires
        leverage context the backtester doesn't carry).
        """
        if avg_cost <= 0 or side not in ("long", "short"):
            return 0.0
        if (
            self._run_stop_loss_atr_mult is not None
            and self._run_stop_loss_atr_mult > 0
            and entry_atr > 0
        ):
            distance = self._run_stop_loss_atr_mult * entry_atr
            return avg_cost - distance if side == "long" else avg_cost + distance
        if self.stop_loss_pct is not None and self.stop_loss_pct > 0:
            return (
                avg_cost * (1 - self.stop_loss_pct)
                if side == "long"
                else avg_cost * (1 + self.stop_loss_pct)
            )
        return 0.0

    @staticmethod
    def _sl_hit(side: str, mark_price: float, trigger_px: float) -> bool:
        """Bar-level SL hit detection. For a long, fires when ``mark_price <=
        trigger_px``; for a short, fires when ``mark_price >= trigger_px``.
        Intra-bar trigger races (high/low piercing without close confirming)
        are not simulated — same caveat as elsewhere in the bar-level engine.
        """
        if trigger_px <= 0 or mark_price <= 0:
            return False
        if side == "long":
            return mark_price <= trigger_px
        if side == "short":
            return mark_price >= trigger_px
        return False

    @staticmethod
    def _walk_trail(side: str, mark_price: float, entry_atr: float,
                    trail_mult: float, sl_trigger_px: float,
                    sl_high_water_px: float) -> Tuple[float, float]:
        """Walk the trailing-stop high-water mark and tighten the SL trigger.
        Used after a ``trail_from_here`` transition. Mirrors the live walker
        in scheduler/hyperliquid_trailing_stop.go: trigger only moves
        favorably (long → up, short → down), never loosens. Returns
        ``(new_trigger_px, new_hwm)``.
        """
        if mark_price <= 0 or entry_atr <= 0 or trail_mult <= 0:
            return sl_trigger_px, sl_high_water_px
        new_trigger = sl_trigger_px
        new_hwm = sl_high_water_px
        if side == "long":
            if mark_price > new_hwm:
                new_hwm = mark_price
            candidate = new_hwm - trail_mult * entry_atr
            if candidate > new_trigger:
                new_trigger = candidate
        elif side == "short":
            if new_hwm <= 0 or mark_price < new_hwm:
                new_hwm = mark_price
            candidate = new_hwm + trail_mult * entry_atr
            if new_trigger <= 0 or candidate < new_trigger:
                new_trigger = candidate
        return new_trigger, new_hwm

    def _maybe_apply_sl_after(
        self, *, side: str, avg_cost: float, entry_atr: float,
        position_qty: float, initial_qty: float, mark_price: float,
        fill_price: float, sl_trigger_px: float, sl_tiers_processed: int,
        post_tp_trail_mult: Optional[float], sl_high_water_px: float,
    ) -> Tuple[float, int, Optional[float], float]:
        """After a partial close has just been applied, find the highest
        cleared TP tier and (if its rule is non-empty) update the simulated
        SL trigger via ``compute_post_tp_stop_loss_trigger``.

        Mirrors the live ``runPostTPStopLossAdjustment`` semantics:
          - "highest cleared tier wins" on multi-tier same-bar fills
          - empty rule for a tier still advances the watermark so we don't
            re-evaluate it next bar
          - ``trail_from_here`` is applied even without a pre-existing SL
            (the live equivalent requires SL OID; the backtester has no
            OIDs so we just install the new trigger and seed the HWM)
          - sl_after only fires for sole-cleared rules with usable inputs
            (compute_ok=True); otherwise it defers to a future bar

        For ``trail_from_here`` the trigger is seeded at the current
        ``fill_price`` (the price the partial close just filled at) — this
        mirrors the live "fill at next bar's open and seed walker there"
        behavior more faithfully than seeding at the bar's close.
        """
        if initial_qty <= 0 or position_qty <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        closed_ratio = 1.0 - (position_qty / initial_qty)
        if closed_ratio <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        highest = self._sl_mod.find_highest_cleared_tier(
            self._run_tp_tier_thresholds, closed_ratio, sl_tiers_processed,
        )
        if highest < 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        raw_rule = self._active_sl_after_rules.for_tier(highest)
        if raw_rule.is_empty():
            return sl_trigger_px, highest + 1, post_tp_trail_mult, sl_high_water_px
        # Defer when no fixed SL is currently armed — mirrors the live
        # `currentOID == 0` short-circuit in scheduler/post_tp_sl.go (~L510).
        # In the backtester an unarmed SL means _initial_sl_trigger couldn't
        # seed one (e.g., ATR-mult SL with entry_atr=0 at open). Without this
        # gate, `breakeven` would still install a fresh trigger where live
        # would defer; ATR-dependent rules already short-circuit below via
        # compute_ok=False, so this is breakeven-specific in practice. Do
        # NOT advance the watermark — same as the live behavior.
        if sl_trigger_px <= 0:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        tier_multiple = self._active_sl_after_rules.tier_multiple(highest)
        rule = raw_rule.resolve_for_regime(self._run_position_regime, tier_multiple)
        if rule is None:
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        # Seed mark for trail_from_here at fill_price — that's the price the
        # partial close just filled at, matching the live "trigger seeded at
        # mark when SL is updated post-fill" intent.
        seed_mark = fill_price if fill_price > 0 else mark_price
        new_trigger, _mode, ok = self._sl_mod.compute_post_tp_stop_loss_trigger(
            rule, side, avg_cost, entry_atr, seed_mark,
        )
        if not ok:
            # Defer without advancing the watermark — next bar (with usable
            # inputs) retries.
            return sl_trigger_px, sl_tiers_processed, post_tp_trail_mult, sl_high_water_px
        new_post_tp_trail = post_tp_trail_mult
        new_hwm = sl_high_water_px
        if rule.kind == "trail_from_here":
            new_post_tp_trail = rule.trail_atr_mult
            new_hwm = seed_mark
        return new_trigger, highest + 1, new_post_tp_trail, new_hwm

    def _calculate_metrics(self, equity_df: pd.DataFrame, trades: list,
                           df: pd.DataFrame, timeframe: str = "1d") -> dict:
        """Calculate comprehensive performance metrics."""
        equity = equity_df["equity"]
        ann_factor = math.sqrt(periods_per_year(timeframe))

        # Anchor return + drawdown at initial_capital so seeded runs (where
        # equity[0] reflects the starting_long mark-to-market, not the true
        # pre-trade balance) don't distort the baseline. For non-seeded runs
        # this is a no-op because equity[0] == initial_capital.
        total_return = (equity.iloc[-1] - self.initial_capital) / self.initial_capital

        # Annualized return
        days = (df.index[-1] - df.index[0]).days
        years = max(days / 365.25, 0.01)
        annual_return = (1 + total_return) ** (1 / years) - 1 if total_return > -1 else -1

        # Daily returns for ratio calculations
        daily_returns = equity.pct_change().dropna()

        # Sharpe Ratio — annualized using the timeframe's periods-per-year
        # (sqrt(365*6) for 4h, sqrt(365*24) for 1h, etc.) so sub-daily
        # timeframes don't get inflated by a factor of sqrt(periods_per_day).
        if len(daily_returns) > 1 and daily_returns.std() > 0:
            sharpe = (daily_returns.mean() / daily_returns.std()) * ann_factor
        else:
            sharpe = 0.0

        # Sortino Ratio (only downside deviation)
        downside = daily_returns[daily_returns < 0]
        if len(downside) > 1 and downside.std() > 0:
            sortino = (daily_returns.mean() / downside.std()) * ann_factor
        else:
            sortino = 0.0

        # Max Drawdown — floor the running peak at initial_capital so the
        # baseline is always the true starting balance, not a seeded
        # mark-to-market that may already be below initial_capital.
        cummax_raw = equity.cummax()
        cummax = cummax_raw.where(cummax_raw >= self.initial_capital, self.initial_capital)
        drawdown = (equity - cummax) / cummax
        max_drawdown = drawdown.min()

        # Trade statistics
        total_trades = len(trades)
        if total_trades > 0:
            winning = [t for t in trades if t.pnl > 0]
            losing = [t for t in trades if t.pnl <= 0]
            win_rate = len(winning) / total_trades

            gross_profit = sum(t.pnl for t in winning) if winning else 0
            gross_loss = abs(sum(t.pnl for t in losing)) if losing else 0
            profit_factor = gross_profit / gross_loss if gross_loss > 0 else float("inf")

            avg_win = np.mean([t.pnl_pct for t in winning]) if winning else 0
            avg_loss = np.mean([t.pnl_pct for t in losing]) if losing else 0
        else:
            win_rate = 0
            profit_factor = 0
            avg_win = 0
            avg_loss = 0

        # Volatility (annualized) — same timeframe-aware factor as Sharpe.
        volatility = daily_returns.std() * ann_factor if len(daily_returns) > 1 else 0

        # Calmar ratio
        calmar = annual_return / abs(max_drawdown) if max_drawdown != 0 else 0

        return {
            "total_return_pct": round(total_return * 100, 2),
            "annual_return_pct": round(annual_return * 100, 2),
            "sharpe_ratio": round(sharpe, 3),
            "sortino_ratio": round(sortino, 3),
            "max_drawdown_pct": round(max_drawdown * 100, 2),
            "calmar_ratio": round(calmar, 3),
            "volatility_pct": round(volatility * 100, 2),
            "win_rate": round(win_rate * 100, 2),
            "profit_factor": round(profit_factor, 3),
            "total_trades": total_trades,
            "avg_win_pct": round(avg_win * 100, 2),
            "avg_loss_pct": round(avg_loss * 100, 2),
        }


def format_results(results: dict) -> str:
    """Pretty-print backtest results."""
    lines = [
        f"\n{'='*60}",
        f"  BACKTEST RESULTS: {results['strategy_name']}",
        f"{'='*60}",
        f"  Symbol:          {results['symbol']}",
        f"  Timeframe:       {results['timeframe']}",
        f"  Period:          {results['start_date'][:10]} → {results['end_date'][:10]}",
        f"  Initial Capital: ${results['initial_capital']:,.2f}",
        f"  Final Capital:   ${results['final_capital']:,.2f}",
        f"{'─'*60}",
        f"  RETURNS",
        f"    Total Return:    {results['total_return_pct']:+.2f}%",
        f"    Annual Return:   {results['annual_return_pct']:+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*60}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results['sharpe_ratio']:.3f}",
        f"    Sortino Ratio:   {results['sortino_ratio']:.3f}",
        f"    Max Drawdown:    {results['max_drawdown_pct']:.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*60}",
        f"  TRADE STATS",
        f"    Total Trades:    {results['total_trades']}",
        f"    Win Rate:        {results['win_rate']:.1f}%",
        f"    Profit Factor:   {results['profit_factor']:.3f}",
        f"    Avg Win:         {results.get('avg_win_pct', 0):+.2f}%",
        f"    Avg Loss:        {results.get('avg_loss_pct', 0):+.2f}%",
        f"{'='*60}",
    ]
    return "\n".join(lines)


if __name__ == "__main__":
    # Quick test with synthetic data
    np.random.seed(42)
    dates = pd.date_range("2023-01-01", periods=200, freq="D")
    prices = 100 + np.cumsum(np.random.randn(200) * 2)
    df = pd.DataFrame({
        "close": prices,
    }, index=dates)

    # Add simple alternating signals for testing
    df["signal"] = 0
    df.iloc[10, df.columns.get_loc("signal")] = 1  # buy
    df.iloc[30, df.columns.get_loc("signal")] = -1  # sell
    df.iloc[50, df.columns.get_loc("signal")] = 1  # buy
    df.iloc[80, df.columns.get_loc("signal")] = -1  # sell

    bt = Backtester(initial_capital=1000)
    results = bt.run(df, strategy_name="Test", save=False)
    print(format_results(results))
