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
  • ``_entry_fraction`` (column-based, #980) — shift(1) alongside the signal;
                         scales the notional committed at open (1.0 = full
                         notional; remainder stays as a cash reserve).
                         Backtest-research surface only: live order sizing is
                         config-driven and ignores the column entirely.
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

SL/TP intra-bar races (#1271)
-----------------------------
The engine-tracked stop-loss trigger is resolved intra-bar by default
(``intrabar_resolution="ohlc_walk"``): the fill bar is walked O -> H/L -> C,
so a bar whose range touches the armed trigger stops the position out ON that
bar — priced at the trigger, or at the bar's open when the bar gaps through
the trigger. The walk is conservative by construction (adverse-move-first):
an intra-bar SL pierce wins over any same-bar close-evaluator exit, because
the engine cannot order a bar's high against its low and a real on-chain stop
executes on touch. Close evaluators stay bar-close-marked black boxes (they
expose no trigger level to walk): a TP is credited only when the bar's close
confirms it and its fill keeps the N-close -> N+1-open contract — a bar that
gaps beyond the TP level therefore fills at its open. Trailing/ratchet
triggers seeded at open are close-anchored (see below) and become
pierce-eligible from the bar after open; fixed (entry-geometry) triggers are
pierce-eligible on the fill bar itself. ``intrabar_resolution="bar_close"``
restores the pre-#1271 bar-level convention — SL hits detected on the close
only and filled at the next bar's open — for reproducing documented
baselines. Documented under ``Backtest`` in CLAUDE.md.

Scale-in / pyramiding (#1276)
-----------------------------
Opt-in via ``allow_scale_in=True`` (+ optional ``scale_in`` caps/spacing dict),
mirroring the live #873 semantics: a same-direction signal on an open position
ADDS size instead of being skipped. The gate is a 1:1 port of
``perpsScaleInDecision`` (scheduler/scale_in.go) evaluated against the
DECISION bar's close (the signal bar, N); the add fills at bar N+1's open —
the same N-close -> N+1-open contract as entries. The blend mirrors
``applyScaleIn``: ``avg_cost`` becomes the qty-weighted blend (drives PnL),
``initial_quantity`` grows (partial-close pro-rating baseline), and on the
FIRST add the risk anchor freezes to the pre-blend ``avg_cost`` — every SL/TP
geometry consumer (sl_after bumps, tiered-TP thresholds, ratchet watermarks)
reads the frozen anchor, never the blend, mirroring live's
``Position.riskAnchorPrice()`` on-chain protection geometry
(hyperliquid_protection.go / post_tp_sl.go / trailing_tp_ratchet.go).
``EntryATR``, the stamped regime, and cleared-tier watermarks stay frozen at
first entry. Each add pays a taker commission on its notional; adds create no
``Trade`` rows (parity with live's ``trade_type="scale_in"`` #T exclusion) —
``results["scale_in_adds"]`` counts them. Sizing notes: the default per-add
notional is the notional the position's FIRST leg committed (the backtest
image of live's per-cycle ``PerpsOpenNotional`` — live margin cash is ≈
stable across an open, frozen here for determinism);
``scale_in.add_notional_usd`` is literal USD, so parity holds when
``initial_capital`` ≈ the live account. A long add spends notional from
``cash``, which may go NEGATIVE — the correct spot-model image of live's
margin-based virtual notional (only the fee leaves live cash); equity =
``cash + position×mark`` stays exact throughout. Adds bypass the regime
entry gate (live ``regimeBlocksOpen`` passes posQty>0). Virtual close
evaluators here read the frozen anchor as ``avg_cost`` — matching the live
LIVE (on-chain) path; live PAPER's virtual evaluators see the blend, a known
live paper-vs-live divergence this engine resolves toward the money path.

Live parity limitations (#906 audit)
------------------------------------
Surfaces intentionally **not** modeled here (use ``backtest/parity_diff.py`` for
decision-layer parity checks; see ``backtest/AUDIT.md`` for the full matrix):

  • **Resting manual limit orders** (#883) — maker fills / partial OID reconcile
    have no bar-level simulation.
  • **``tiered_tp_atr_live_regime_dynamic``** (#843) — rejected at
    ``run_backtest.load_strategy_config`` (on-chain regime hysteresis).
  • **Inline trailing SL at open** (#885) — live arms same-cycle; backtest seeds
    trailing/ratchet triggers on the bar after open (no naked-gap modeling).
"""

import sys
import os
import math
from typing import Any, Optional, Tuple

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
# Repo root, so `from shared_strategies.close... import` (post_tp_sl.py,
# trailing_tp_ratchet.py — loaded unconditionally in __init__) resolves under
# script-style invocation (`python backtest/run_backtest.py`), where only the
# script's own directory is on sys.path. pytest masks this by inserting the
# root during collection of the shared_strategies package tests.
_REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
if _REPO_ROOT not in sys.path:
    sys.path.insert(0, _REPO_ROOT)

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
_regime_allows_entry_fn = None

# Post-TP SL helpers (#709). Loaded via spec_from_file_location to avoid the
# same registry-name collision that close_registry_loader works around — this
# module lives in shared_strategies/close/ but isn't a registered strategy,
# so we import it directly without going through the registry.
_post_tp_sl_module = None
_trailing_ratchet_module = None


def _load_regime():
    global _ensure_regime_fn, _regime_allows_entry_fn
    if _ensure_regime_fn is None:
        from regime import ensure_regime_columns as _ensure_regime_columns
        from regime import regime_label_allows_entry as _allows_entry
        _ensure_regime_fn = _ensure_regime_columns
        _regime_allows_entry_fn = _allows_entry
    return _ensure_regime_fn


def _regime_allows_entry(allowed, bar_regime: str, on_failure: str = "open") -> bool:
    """#1124 entry-gate with the one-directional directional family rule.

    A bare ``ranging_directional`` in ``allowed`` matches its ``_up``/``_down``
    sub-labels, mirroring the Go ``regimeAllowsEntry`` gate for live/backtest
    parity. #1278: an empty ``bar_regime`` (warmup bar 0 after the #730 shift,
    mid-series gaps) resolves per ``on_failure`` — ``"open"`` (default) admits
    the entry, ``"closed"`` blocks it — mirroring the live
    ``regime_gate_on_failure`` policy. Falls back to plain membership if the
    regime module isn't loaded.
    """
    if _regime_allows_entry_fn is None:
        _load_regime()
    if _regime_allows_entry_fn is None:
        if not allowed:
            return True
        if not bar_regime:
            return on_failure != "closed"
        return bar_regime in allowed
    return _regime_allows_entry_fn(allowed, bar_regime, on_failure)


def _regime_primary_labels(spec: Optional[dict]) -> Optional[tuple]:
    """Valid regime labels for the PRIMARY (medium-first) window's classifier —
    the vocabulary the backtester's single stamped regime label uses (#1058).

    The regime-keyed exit consumers (``stop_loss_atr_regime`` /
    ``trailing_stop_atr_regime`` / the ``sl_after`` regime block) validate and
    resolve their per-label entries against this set, mirroring live's
    ``regimeLabelsForStrategyWindow`` -> ``regimeLabelsForClassifier`` thread into
    ``parseRegimeATRBlock`` / ``validatePostTPStopLossRulesWithLabels``. Without
    it a composite-primary config would either be falsely rejected ("unknown
    regime label 'ranging_quiet'") or, for an ADX-keyed block, silently resolve
    to the default stop under a composite stamp.

    Returns ``None`` for the legacy ADX path (no spec, or an ADX primary window)
    so the parsers fall back to their canonical 3-label ADX default and the ADX
    path stays byte-identical. Returns the sorted composite label tuple only when
    the primary window is composite.
    """
    if not spec:
        return None
    from regime import (
        valid_labels_for_classifier,
        REGIME_PRIMARY_WINDOW_KEY,
        CLASSIFIER_ADX,
    )
    primary_key = (
        REGIME_PRIMARY_WINDOW_KEY
        if REGIME_PRIMARY_WINDOW_KEY in spec
        else sorted(spec.keys())[0]
    )
    classifier = str(spec[primary_key].get("classifier") or CLASSIFIER_ADX).strip().lower()
    if classifier == CLASSIFIER_ADX:
        return None
    return tuple(sorted(valid_labels_for_classifier(classifier)))


def _normalize_regime_directional_policy(policy: Optional[dict]) -> Optional[dict]:
    """Validate and compact ``regime_directional_policy`` for replay (#1025).

    Go owns vocabulary validation because labels depend on the selected regime
    classifier/window. The backtester only mirrors the already-validated runtime
    shape: ``trend_regime`` label -> {direction, invert_signal}.
    """
    if not policy:
        return None
    if not isinstance(policy, dict):
        raise ValueError("regime_directional_policy must be an object")
    raw = policy.get("trend_regime")
    if not isinstance(raw, dict):
        raise ValueError(
            "regime_directional_policy must contain a trend_regime object"
        )
    parsed: dict[str, dict[str, object]] = {}
    for label, entry in raw.items():
        if not isinstance(entry, dict):
            raise ValueError(
                f"regime_directional_policy.{label}: must be an object"
            )
        direction = entry.get("direction")
        if not isinstance(direction, str):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be a string"
            )
        if direction not in ("long", "short", "both"):
            raise ValueError(
                f"regime_directional_policy.{label}.direction: must be "
                f"'long', 'short', or 'both'"
            )
        invert = entry.get("invert_signal", False)
        if not isinstance(invert, bool):
            raise ValueError(
                f"regime_directional_policy.{label}.invert_signal: "
                f"must be a boolean"
            )
        for key in entry:
            if key not in ("direction", "invert_signal"):
                raise ValueError(
                    f"regime_directional_policy.{label}: unknown key {key!r}"
                )
        parsed[str(label).strip()] = {
            "direction": direction,
            "invert_signal": invert,
        }
    return parsed or None


def _gate_directional_policy_by_states(
    policy: Optional[dict], cert_states: Optional[dict],
) -> Optional[dict]:
    """#1085 PER-STATE evidence gate (mirrors Go gatedDirectionalEntry): retain a
    regime's override only where the certified evidence supports the configured
    direction for that state — config == certified sign, or config "both" (which
    defers to the signal and never contradicts). A state with no certified
    direction, or whose config contradicts the certified sign, is DROPPED so that
    regime resolves to base direction.

    cert_states semantics:
      - None  -> "honor all" (legacy cell-level certified caller; the policy is
                 returned unchanged),
      - {}    -> uncertified cell: every state dropped (returns {} -> caller nulls),
      - dict  -> keep only the per-state-supported overrides."""
    if not policy:
        return policy
    # #1124/#1228: live Resolve falls back from a sub-label stamp to the bare
    # ranging_directional policy entry (exact match wins first). Mirror that by
    # expanding a bare entry onto its uncovered subs before the per-state gate,
    # so a cert for e.g. ranging_directional_up honors a bare-only policy the
    # way live does.
    from regime import RANGING_DIRECTIONAL_BARE, RANGING_DIRECTIONAL_SUBS
    bare_entry = policy.get(RANGING_DIRECTIONAL_BARE)
    if isinstance(bare_entry, dict):
        expanded = dict(policy)
        for sub in sorted(RANGING_DIRECTIONAL_SUBS):
            expanded.setdefault(sub, bare_entry)
        policy = expanded
    if cert_states is None:
        return policy
    gated = {}
    for label, entry in policy.items():
        cert_dir = str(cert_states.get(label) or "").strip().lower()
        if not cert_dir:
            continue
        direction = str((entry or {}).get("direction") or "").strip().lower()
        if direction != "both" and direction != cert_dir:
            continue
        gated[label] = entry
    return gated


def _resolve_regime_directional_entry(
    policy: Optional[dict],
    current_regime: str,
    position_regime: str = "",
    position_qty: float = 0.0,
) -> Optional[dict]:
    """Return the effective per-regime direction/invert override (#1025)."""
    if not policy:
        return None
    regime = str(current_regime or "").strip()
    if position_qty > 0 and str(position_regime or "").strip():
        regime = str(position_regime or "").strip()
    # Exact match ONLY — deliberately no bare→sub fallback here. Runtime
    # always resolves the ALREADY-GATED policy
    # (_gate_directional_policy_by_states expands a bare ranging_directional
    # entry onto its subs SUBJECT TO each sub's own per-state certification),
    # so a bare-certified-but-sub-uncertified stamp must miss and fall back to
    # base, mirroring live's fail-closed gatedDirectionalEntry (#1085/#1124
    # exact-match cert, no family fallback).
    entry = policy.get(regime)
    return dict(entry) if isinstance(entry, dict) else None


def _apply_direction_invert_value(
    signal: int,
    uses_open_close: bool,
    direction: Optional[str],
    invert_signal: bool,
) -> int:
    """Scalar form of Backtester._apply_direction_invert (#1025)."""
    sig = int(signal)
    if invert_signal and sig != 0:
        sig = -sig
    d = (direction or "").strip().lower()
    if uses_open_close and d in ("long", "short"):
        if d == "long" and sig < 0:
            return 0
        if d == "short" and sig > 0:
            return 0
    return sig


def _signal_from_open_action(action: str) -> int:
    action = str(action or "").strip().lower()
    if action == "long":
        return 1
    if action == "short":
        return -1
    return 0


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
    # trailing_tp_ratchet.py does `from _helpers import …` / `from regime_atr
    # import …` at module level — both live in shared_strategies/close, so the
    # dir must be importable BEFORE exec_module. The constructor's own
    # _ensure_close_strategies_path() call runs after the ratchet load (#1152:
    # a ratchet close ref in a process that never imported the close registry —
    # e.g. exit_policy_ab — crashed here on ModuleNotFoundError: _helpers).
    _ensure_close_strategies_path()
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


# Timeframe-independent sentinel for the risk-adjusted floor applied to blown
# (liquidated) legs (#1005). Must be uniform across timeframes so two equally
# dead legs tie regardless of which timeframe they busted on. The earlier floor
# used the per-leg ``-ann_factor`` (1h ≈ -93.6, 4h ≈ -46.8), which let the SAME
# total loss carry a ~2x different Sharpe by timeframe and perturbed mean-Sharpe
# rankings of liquidated strategies by bust timeframe rather than severity. The
# magnitude (100, mirroring the -100% return floor) dominates any surviving
# leg's annualized Sharpe on the harness timeframes (1h/4h).
LIQUIDATED_METRIC_FLOOR = 100.0


# Taker fee rates per platform — mirrors scheduler/fees.go:CalculatePlatformSpotFee
# and related constants. test_platform_fees.py scrapes fees.go to enforce parity.
PLATFORM_FEE_PCT = {
    "binanceus":   0.001,    # BinanceSpotFeePct
    "hyperliquid": 0.00045,  # HyperliquidTakerFeePct (base tier, #1315)
    "robinhood":   0.0,      # RobinhoodCryptoFeePct (no commission)
    "luno":        0.01,     # LunoTakerFeePct
    "okx":         0.001,    # OKXSpotTakerFeePct
    "okx-perps":   0.0005,   # OKXPerpsTakerFeePct
}

# Hyperliquid base-tier maker rate — parity pair with
# scheduler/fees.go:HyperliquidMakerFeePct (#1315). The bar-level simulator
# has no maker-priced fills (all fills are market/trigger at next open;
# resting manual limit orders are an unmodeled #906 parity limitation), so
# this constant exists for parity and for future consumers, not the fill loop.
HYPERLIQUID_MAKER_FEE_PCT = 0.00015


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


def _parse_profile_allocation(alloc: Optional[dict]) -> Optional[dict]:
    """Validate and compact a regime_profile_allocation block for the engine
    (#998). Returns None when unset; raises ValueError on a malformed block so
    a misconfigured --config fails loudly rather than silently single-profile.

    The compact form mirrors the Go RegimeProfileAllocation: profiles (label ->
    profile name), param_sets (profile -> params), confirm_bars, initial_profile.
    The window key and per-profile signal computation live in run_backtest.py;
    the engine only replays the switch over the supplied ``signal__<profile>``
    columns.
    """
    if not alloc:
        return None
    profiles = dict(alloc.get("profiles") or {})
    param_sets = dict(alloc.get("param_sets") or {})
    confirm_bars = int(alloc.get("confirm_bars") or 0)
    initial_profile = str(alloc.get("initial_profile") or "").strip()
    if len(param_sets) != 2:
        raise ValueError(
            f"regime_profile_allocation.param_sets must define exactly 2 "
            f"profiles (the M4 two-profile model), got {len(param_sets)}"
        )
    if confirm_bars < 1:
        raise ValueError("regime_profile_allocation.confirm_bars must be >= 1")
    if initial_profile not in param_sets:
        raise ValueError(
            f"regime_profile_allocation.initial_profile={initial_profile!r} "
            f"is not a param_sets profile {sorted(param_sets)}"
        )
    for lbl, prof in profiles.items():
        if prof not in param_sets:
            raise ValueError(
                f"regime_profile_allocation.profiles[{lbl!r}]={prof!r} is not "
                f"a param_sets profile {sorted(param_sets)}"
            )
    return {
        "profiles": profiles,
        "param_sets": param_sets,
        "confirm_bars": confirm_bars,
        "initial_profile": initial_profile,
        "names": sorted(param_sets),
    }


class _ProfileSwitcher:
    """Per-bar flat-only, confirm_bars hysteresis profile switch — the exact
    state machine resolveRegimeProfile replays live (#998). The backtester is
    bar-cadenced, so every ``step`` is a closed-bar advance.
    """

    def __init__(self, alloc: dict):
        self._profiles = alloc["profiles"]
        self._confirm_bars = alloc["confirm_bars"]
        self.active = alloc["initial_profile"]
        self._pending = ""
        self._seen = 0

    def step(self, label: str, flat: bool) -> str:
        """Advance one closed bar and return the profile governing THIS bar's
        open decision. ``flat`` is the position state at decision time (the
        backtester's position carried into this bar)."""
        desired = self._profiles.get((label or "").strip(), "")
        if desired == "":
            # Fail-open / unknown label: freeze the counter, hold active.
            return self.active
        if desired == self.active:
            self._pending = ""
            self._seen = 0
            return self.active
        # Desired differs from active: accrue hysteresis.
        if self._pending == desired:
            self._seen += 1
        else:
            self._pending = desired
            self._seen = 1
        # Commit only when flat AND the desired profile has persisted long
        # enough. While a position is open the counter keeps growing but the
        # switch is deferred to the first flat bar.
        if flat and self._seen >= self._confirm_bars:
            self.active = desired
            self._pending = ""
            self._seen = 0
        return self.active


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


def _validated_entry_fraction_series(df: pd.DataFrame) -> pd.Series:
    """Validate the strategy-emitted ``entry_fraction`` column (#980).

    Values must be in (0, 1]: 1.0 = full notional (today's behavior), a
    fraction commits that share of flat-state cash at the open and leaves the
    remainder as a cash reserve. NaN means "no opinion" and resolves to full
    notional downstream. 0 is rejected rather than treated as "skip the
    entry" — suppressing an entry is the signal's job, and a 0-fraction open
    would book a zero-share phantom trade.
    """
    vals = df["entry_fraction"].astype(float)
    bad = vals[vals.notna() & ((vals <= 0) | (vals > 1))]
    if not bad.empty:
        values = sorted(set(bad.tolist()))
        raise ValueError(
            f"entry_fraction values must be in (0, 1] — got {values}"
        )
    return vals


class _ScaleInState:
    """Per-position scale-in bookkeeping (#1276), mirroring the live
    ``Position`` fields the #873 blend maintains (scheduler/scale_in.go).

    ``risk_anchor_price`` is 0.0 until the first add; geometry consumers read
    ``risk_anchor_price or avg_cost`` — the exact backtest analogue of live's
    ``Position.riskAnchorPrice()`` (scheduler/portfolio.go). ``base_open_notional``
    freezes the notional the position's FIRST leg committed, the backtest image
    of live's per-cycle ``PerpsOpenNotional`` default per-add sizing (live
    margin cash stays ≈ constant across an open; frozen here for determinism).
    One ``reset()`` shared by every full-close site so the reset list can't
    drift.
    """

    __slots__ = ("risk_anchor_price", "scale_in_count", "last_add_price",
                 "added_notional_usd", "base_open_notional")

    def __init__(self):
        self.reset()

    def reset(self) -> None:
        self.risk_anchor_price = 0.0
        self.scale_in_count = 0
        self.last_add_price = 0.0
        self.added_notional_usd = 0.0
        self.base_open_notional = 0.0

    def geom_cost(self, avg_cost: float) -> float:
        """SL/TP trigger geometry price: the frozen first entry once the
        position has scaled in, else the (un-blended) avg_cost."""
        if self.risk_anchor_price > 0:
            return self.risk_anchor_price
        return avg_cost


_SCALE_IN_CFG_KEYS = (
    "max_adds", "max_added_notional_usd", "add_spacing_atr", "add_notional_usd",
)


def _normalize_scale_in_cfg(scale_in: Optional[dict]) -> dict:
    """Validate + normalize a live ``scale_in`` config block (#1276), mirroring
    the live validateConfig bounds (scheduler/config.go). Unknown keys are
    rejected loudly — a typo'd cap would otherwise silently not gate."""
    cfg = {"max_adds": 0, "max_added_notional_usd": 0.0,
           "add_spacing_atr": 0.0, "add_notional_usd": 0.0}
    if not scale_in:
        return cfg
    if not isinstance(scale_in, dict):
        raise ValueError(
            f"scale_in must be a dict of {list(_SCALE_IN_CFG_KEYS)}, "
            f"got {type(scale_in).__name__}"
        )
    unknown = sorted(set(scale_in) - set(_SCALE_IN_CFG_KEYS))
    if unknown:
        raise ValueError(
            f"scale_in has unknown key(s) {unknown}; "
            f"supported: {list(_SCALE_IN_CFG_KEYS)}"
        )
    max_adds = scale_in.get("max_adds", 0) or 0
    if int(max_adds) != max_adds or int(max_adds) < 0:
        raise ValueError(f"scale_in.max_adds must be an int >= 0, got {max_adds!r}")
    cfg["max_adds"] = int(max_adds)
    for key in ("max_added_notional_usd", "add_notional_usd"):
        val = float(scale_in.get(key, 0) or 0)
        if val < 0:
            raise ValueError(f"scale_in.{key} must be >= 0, got {val}")
        cfg[key] = val
    cfg["add_spacing_atr"] = float(scale_in.get("add_spacing_atr", 0) or 0)
    return cfg


def _scale_in_decision(scale_cfg: dict, side: str, quantity: float,
                       avg_cost: float, entry_atr: float, scale_in_count: int,
                       added_notional_usd: float, last_add_price: float,
                       signal: int, price: float,
                       default_open_notional: float) -> Tuple[float, bool, str]:
    """Pure gate for a same-direction add — a line-for-line port of the live
    ``perpsScaleInDecision`` (scheduler/scale_in.go #873), including the 1e-9
    tolerances, the ``last_add_price -> avg_cost`` fallback, and the
    positive-EntryATR spacing requirement. ``price`` is the DECISION-time
    price (live: cycle mark ≈ signal bar's close). Returns
    ``(add_qty, ok, reason)``; the opt-in (``allow_scale_in``) check lives at
    the call site, matching the caller-gated live dispatch.
    """
    if price <= 0:
        return 0.0, False, "no price for scale-in"
    # Direction match: a buy adds to a long, a sell adds to a short. Anything
    # else is a close/cover/flip/fresh-open handled by the existing paths.
    if not ((signal == 1 and side == "long" and quantity > 0)
            or (signal == -1 and side == "short" and quantity > 0)):
        return 0.0, False, "not a same-direction add"

    max_adds = int(scale_cfg.get("max_adds", 0) or 0)
    if max_adds > 0 and scale_in_count >= max_adds:
        return 0.0, False, "scale-in max_adds reached"

    add_notional = default_open_notional
    if (scale_cfg.get("add_notional_usd", 0) or 0) > 0:
        add_notional = float(scale_cfg["add_notional_usd"])
    if add_notional <= 0:
        return 0.0, False, "scale-in add notional resolves to zero"
    # Cumulative ACTUAL added notional + this add's REQUESTED notional — an
    # approximate guardrail, not an exact ceiling (live #873 review).
    max_added = float(scale_cfg.get("max_added_notional_usd", 0) or 0)
    if max_added > 0 and added_notional_usd + add_notional > max_added + 1e-9:
        return 0.0, False, "scale-in max_added_notional_usd reached"

    spacing = float(scale_cfg.get("add_spacing_atr", 0) or 0)
    if spacing != 0:
        if entry_atr <= 0:
            return 0.0, False, "scale-in spacing requires a positive EntryATR"
        last_add = last_add_price
        if last_add <= 0:
            last_add = avg_cost
        direction = -1.0 if side == "short" else 1.0
        # favorable_move > 0 when price has moved in the position's favor
        # since the last entry leg (up for a long, down for a short).
        favorable_move = (price - last_add) * direction
        needed = spacing * entry_atr
        if spacing > 0:
            # add-to-winners: require an in-favor move of at least `needed`.
            if favorable_move + 1e-9 < needed:
                return 0.0, False, "scale-in spacing (add-to-winners) not reached"
        else:
            # average-down: require an adverse move of at least |needed|.
            if -favorable_move + 1e-9 < -needed:
                return 0.0, False, "scale-in spacing (average-down) not reached"

    return add_notional / price, True, ""


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
        # #997 hold telemetry — stamped at close via _stamp_hold(). Defaults
        # keep pre-#997 callers/tests valid (a trade never stamped just reports
        # zeros). bars_held is closed-bar count since the entry-fill bar
        # inclusive (filled at bar N's open -> bars_held==1 at bar N's close).
        # mfe_pct / mae_pct are signed, side-aware excursions vs entry price
        # (mfe >= 0 favourable, mae <= 0 adverse); bars_to_* index when each
        # extreme occurred. entry_fee / exit_fee are this leg's commissions.
        self.bars_held = 0
        self.mfe_pct = 0.0
        self.mae_pct = 0.0
        self.bars_to_mfe = 0
        self.bars_to_mae = 0
        self.entry_atr = 0.0
        self.entry_fee = 0.0
        self.exit_fee = 0.0
        self.exit_reason = ""
        # #1276: add legs blended into the position this leg closes out of
        # (0 for never-scaled positions; additive telemetry like #997).
        self.scale_in_adds = 0

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
            # #997 hold telemetry (additive; existing consumers ignore these).
            "bars_held": self.bars_held,
            "mfe_pct": round(self.mfe_pct * 100, 4),
            "mae_pct": round(self.mae_pct * 100, 4),
            "bars_to_mfe": self.bars_to_mfe,
            "bars_to_mae": self.bars_to_mae,
            "entry_atr": round(self.entry_atr, 6),
            "entry_fee": round(self.entry_fee, 6),
            "exit_fee": round(self.exit_fee, 6),
            "exit_reason": self.exit_reason,
            # #1276: additive; existing consumers ignore it.
            "scale_in_adds": self.scale_in_adds,
        }


class _HoldTracker:
    """Per-position intra-hold excursion + holding-time accumulator (#997).

    Output-only: feeds the exit-quality diagnostic, never a trading decision,
    so reading the current bar's high/low at its own close is look-ahead-safe.
    Reset at every open via ``open()``; advanced once per held bar via
    ``step()`` (called after this bar's open-fill close/open processing so a
    trade closed at the bar's open does not absorb that bar's range, while a
    trade opened at the bar's open does); read at close via ``metrics()``.
    """

    __slots__ = ("bars", "high", "low", "high_bar", "low_bar",
                 "entry_fee", "entry_price", "side")

    def __init__(self):
        self.open(0.0, "long", 0.0)

    def open(self, entry_price: float, side: str, entry_fee: float) -> None:
        self.bars = 0
        self.high = entry_price
        self.low = entry_price
        self.high_bar = 0
        self.low_bar = 0
        self.entry_fee = entry_fee
        self.entry_price = entry_price
        self.side = side

    def step(self, high: float, low: float) -> None:
        self.bars += 1
        if high > self.high:
            self.high = high
            self.high_bar = self.bars
        if low < self.low:
            self.low = low
            self.low_bar = self.bars

    def metrics(self):
        """Return (mfe_pct, mae_pct, bars_to_mfe, bars_to_mae), side-aware."""
        e = self.entry_price
        if e <= 0:
            return 0.0, 0.0, 0, 0
        if self.side == "long":
            return (self.high - e) / e, (self.low - e) / e, self.high_bar, self.low_bar
        return (e - self.low) / e, (e - self.high) / e, self.low_bar, self.high_bar


def _stamp_hold(trade, hold: "_HoldTracker", *, entry_atr: float,
                exit_fee: float, reason: str, qty_frac: float = 1.0) -> None:
    """Stamp #997 hold telemetry onto a closing trade leg.

    ``qty_frac`` pro-rates the entry commission for a partial-close leg (each
    leg gets its share of the single entry fee; the legs' fractions sum to 1).

    This is the single fee-netting chokepoint for every close site (#1241):
    ``Trade.close()`` sets a gross ``pnl``; here we net BOTH the pro-rated
    entry fee and the exit fee out of it so every leg reports PnL net of fees,
    matching live's net-PnL convention (``tradeNetPnL``). Callers must NOT
    deduct the exit fee from ``pnl`` themselves — doing so double-counts it.
    """
    mfe, mae, b_mfe, b_mae = hold.metrics()
    trade.bars_held = hold.bars
    trade.mfe_pct = mfe
    trade.mae_pct = mae
    trade.bars_to_mfe = b_mfe
    trade.bars_to_mae = b_mae
    trade.entry_atr = entry_atr
    trade.entry_fee = hold.entry_fee * qty_frac
    trade.exit_fee = exit_fee
    trade.exit_reason = reason
    # Net both fees out of the gross pnl set by Trade.close() (#1241).
    trade.pnl -= trade.entry_fee + trade.exit_fee


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
                 regime_windows_spec: Optional[dict] = None,
                 allowed_regimes: Optional[list[str]] = None,
                 regime_gate_on_failure: str = "open",
                 stop_loss_atr_mult: Optional[float] = None,
                 stop_loss_pct: Optional[float] = None,
                 stop_loss_margin_pct: Optional[float] = None,
                 trailing_stop_atr_mult: Optional[float] = None,
                 trailing_stop_pct: Optional[float] = None,
                 stop_loss_atr_regime: Optional[dict] = None,
                 trailing_stop_atr_regime: Optional[dict] = None,
                 strategy_type: str = "perps",
                 direction: Optional[str] = None,
                 invert_signal: bool = False,
                 regime_directional_policy: Optional[dict] = None,
                 regime_directional_certified: bool = False,
                 regime_directional_certified_states: Optional[dict] = None,
                 regime_timeframe: Optional[str] = None,
                 profile_allocation: Optional[dict] = None,
                 intrabar_resolution: str = "ohlc_walk",
                 risk_per_trade_pct: Optional[float] = None,
                 allow_scale_in: bool = False,
                 scale_in: Optional[dict] = None):
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
            regime_timeframe: Accepted for ``load_strategy_config`` spread
                compatibility. The runner applies this before constructing the
                engine by aligning regime columns onto the trading DataFrame.
            intrabar_resolution: How the engine-tracked SL trigger resolves
                against the fill bar (#1271). ``"ohlc_walk"`` (default) walks
                O -> H/L -> C: a range touch stops out ON that bar at the
                trigger price (or at the open on a gap-through), winning
                adverse-move-first over any same-bar close-evaluator exit.
                ``"bar_close"`` restores the legacy pre-#1271 convention
                (hit detected on the close only, filled at the next bar's
                open) for reproducing documented baselines. See the module
                docstring, "SL/TP intra-bar races".
            risk_per_trade_pct: Opt-in risk-per-trade sizing (#1268),
                mirroring the live ``risk_per_trade_pct`` config field: each
                open commits ``shares = (cash × pct/100) / stop_distance``
                (invest = ``cash × (pct/100) × price/stop_distance``, capped
                at 1.0 × cash — the backtester models no leverage, so the
                live ``cash × exchange_leverage`` cap tightens to 1× here; a
                one-time warning prints when the cap binds). Stop distance
                comes from the configured stop owner: ``*_atr_mult × entry
                ATR`` (the signal bar's closed ATR, same value the SL
                geometry uses), or ``price × *_pct`` (this engine's pct
                fields are FRACTIONS, matching the SL trigger math). An
                unresolvable distance (no usable ATR at the signal bar)
                SKIPS the entry — fail-closed, matching live. Requires one
                of the four scalar stop owners; regime-resolved owners, a
                margin-pct-only stop, and a strategy-emitted
                ``entry_fraction`` column are rejected loudly. None keeps
                the legacy full-notional sizing byte-identical.
            allow_scale_in: Opt-in scale-in / pyramiding (#1276), mirroring
                the live ``allow_scale_in`` field (#873): a same-direction
                signal on an open position ADDS size instead of being
                skipped. Default False keeps every existing run
                byte-identical. See the module docstring, "Scale-in /
                pyramiding".
            scale_in: Optional caps/spacing dict (live ``scale_in`` block):
                ``max_adds`` (int, 0 = unlimited), ``max_added_notional_usd``
                (0 = unlimited), ``add_spacing_atr`` (signed EntryATR
                multiples; >0 add-to-winners, <0 average-down, 0 = no gate),
                ``add_notional_usd`` (0 = the position's first-leg notional).
                Rejected when set without ``allow_scale_in`` (mirrors the
                live validator).
        """
        self.initial_capital = initial_capital
        self.platform = platform
        # #1271: intra-bar SL race resolution mode. Validated eagerly so a
        # typo'd mode fails at construction, not as a silent legacy fallback.
        self.intrabar_resolution = str(intrabar_resolution or "").strip().lower()
        if self.intrabar_resolution not in ("ohlc_walk", "bar_close"):
            raise ValueError(
                f"intrabar_resolution must be 'ohlc_walk' or 'bar_close', "
                f"got {intrabar_resolution!r}"
            )
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
        self.regime_timeframe = str(regime_timeframe or "").strip() or None
        self.regime_period = regime_period
        self.regime_adx_threshold = regime_adx_threshold
        # #1058: optional composite (9-state) regime. When set, this is the live
        # ``regime.windows`` spec map (already normalized: name -> {classifier,
        # period, adx_threshold|thresholds}). The per-bar ``regime`` column and
        # the close evaluator's ``_run_position_regime`` are then classified from
        # the PRIMARY window — "medium" if present, else the first sorted key —
        # mirroring live's REGIME_PRIMARY_WINDOW_KEY selection in
        # regime_from_injected_payload. None keeps the legacy single-lookback ADX
        # path (regime_period / regime_adx_threshold) byte-identical.
        self.regime_windows_spec = dict(regime_windows_spec) if regime_windows_spec else None
        # #1058: vocabulary the regime-keyed exit consumers (SL/trailing/sl_after)
        # validate + resolve against — the primary window's classifier labels, so
        # composite substates parse and resolve instead of being rejected or
        # silently falling back to the default stop. None = legacy ADX (canonical).
        self._regime_primary_labels = _regime_primary_labels(self.regime_windows_spec)
        self.allowed_regimes = list(allowed_regimes or [])
        # #1278: entry-gate failure policy for empty/unavailable regime labels
        # (warmup bar 0 after the #730 shift, mid-series NaN gaps), mirroring
        # the live regime_gate_on_failure field. "open" (default) admits the
        # entry — byte-identical to the pre-#1278 baselines; "closed" blocks
        # fresh entries on empty-label bars when a gate is configured. Closes
        # are never gated either way. Unknown values raise at construction.
        _norm_gate = str(regime_gate_on_failure or "").strip().lower() or "open"
        if _norm_gate not in ("open", "closed"):
            raise ValueError(
                f"regime_gate_on_failure must be 'open' or 'closed', "
                f"got {regime_gate_on_failure!r}"
            )
        self.regime_gate_on_failure = _norm_gate
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
        self.regime_directional_policy = _normalize_regime_directional_policy(
            regime_directional_policy,
        )
        # #1085: evidence gate (PER STATE). The directional-selection surface is
        # DEFAULT-OFF and resolves to base direction unless this (asset, timeframe,
        # classifier) is certified — AND, per state, the configured direction
        # agrees with the certified sign (or is "both"). Dropping a state's
        # override here makes every downstream resolver (_resolve_* /
        # _effective_directional_entry) fall back to base for that regime, exactly
        # mirroring the live per-state gate (gatedDirectionalEntry) — so a backtest
        # can never show a directional edge the live path suppresses, including a
        # cell whose config contradicts the certified sign for some state. The
        # caller (run_backtest.py) reads the SAME artifact the live daemon reads:
        # it passes the certified per-state map; the legacy bool is the cell-level
        # fallback (honor-all) for callers that don't supply the map.
        if self.regime_directional_policy is not None:
            if regime_directional_certified_states is not None:
                cert_states = regime_directional_certified_states
            elif bool(regime_directional_certified):
                cert_states = None  # legacy cell-level certified caller: honor all states
            else:
                cert_states = {}  # uncertified cell: drop every state -> base
            self.regime_directional_policy = _gate_directional_policy_by_states(
                self.regime_directional_policy, cert_states,
            )
            if not self.regime_directional_policy:
                print("[#1085] regime_directional_policy present but NOT certified "
                      "(or no state survives the per-state sign gate) for this "
                      "(asset,timeframe,classifier) — DEFAULT-OFF in backtest "
                      "(base direction), mirroring live (#1076 negative result).",
                      file=sys.stderr)
                self.regime_directional_policy = None
        if self.regime_directional_policy is not None and not self.regime_enabled:
            raise ValueError(
                "regime_directional_policy requires regime_enabled=True"
            )
        # #998: regime-profile allocation. When set, run() expects per-profile
        # signal columns ("signal__<profile>") plus a "_profile_label" column
        # (the long-window regime label per bar) and replays the live flat-only,
        # confirm_bars hysteresis switch inside the per-bar loop. None = single
        # profile (the normal path). Validated into a compact dict.
        self._profile_alloc = _parse_profile_allocation(profile_allocation)
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
        # #841 unified per-regime close SL ownership; populated below when the
        # regime-ATR helpers are imported (unified requires a regime tiered
        # close, which forces that import path).
        self._unified_close_params: Optional[dict] = None
        self._unified_scalar_params = None
        self._uses_trailing_ratchet_close = any(
            (r.get("name") or "").strip().lower()
            in ("trailing_tp_ratchet", "trailing_tp_ratchet_regime")
            for r in self._close_refs
        )
        # #997 zscore_target: the rolling z-score the evaluator reads is
        # computed once per run from closed-bar data (same N-close -> N+1-open
        # fill contract as ATR). Resolve the lookback here. Reject a duplicate
        # ref outright: close_params is keyed by name (last-write-wins), so a
        # second zscore_target with a different lookback would silently lose
        # its window — a footgun, not a feature.
        _zscore_refs = [
            r for r in self._close_refs
            if (r.get("name") or "").strip().lower() == "zscore_target"
        ]
        if len(_zscore_refs) > 1:
            raise ValueError(
                "duplicate zscore_target close refs are not supported "
                "(close params are keyed by name; the second would silently "
                "override the first's lookback)"
            )
        self._zscore_lookback = 0
        if _zscore_refs:
            try:
                self._zscore_lookback = int(
                    (_zscore_refs[0].get("params") or {}).get("lookback", 0) or 0
                )
            except (TypeError, ValueError):
                self._zscore_lookback = 0
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
                close_params_are_unified_regime,
                parse_regime_atr_block,
                resolve_regime_atr,
                unified_regime_scalar_params,
                validate_unified_regime_close,
            )

            # #841 unified per-regime close block: the close ref owns the
            # per-regime stop loss (stop_loss_atr inside each label entry).
            # Mirrors Go: unifiedCloseStopLossATR arms the SL, and
            # validateUnifiedCloseSoleOwner forbids any strategy-level stop
            # owner alongside the block. Without this, a unified close with
            # stop_loss_atr backtested with no stop at all (#1228 audit).
            self._unified_close_params = None
            self._unified_scalar_params = unified_regime_scalar_params
            for _ref in self._close_refs:
                _n = (_ref.get("name") or "").strip().lower()
                if _n not in ("tiered_tp_atr_regime", "tiered_tp_atr_live_regime"):
                    continue
                _params = _ref.get("params") or {}
                if close_params_are_unified_regime(_params):
                    self._unified_close_params = dict(_params)
                break
            if self._unified_close_params is not None:
                # #1228 review round 3: mirror live's validateUnifiedRegimeClose
                # — every stampable label must carry a positive stop_loss_atr
                # (and >=2 valid tp_tiers). Live rejects such a config at load;
                # simulating it would run a regime with no stop at all.
                _unified_errs = validate_unified_regime_close(
                    self._unified_close_params,
                    labels=self._regime_primary_labels,
                )
                if _unified_errs:
                    raise ValueError(
                        "Invalid unified per-regime close block: "
                        + "; ".join(_unified_errs)
                    )
                _sole_owner_conflicts = [
                    ("stop_loss_atr_mult", self.stop_loss_atr_mult),
                    ("stop_loss_pct", self.stop_loss_pct),
                    ("stop_loss_margin_pct", self.stop_loss_margin_pct),
                    ("trailing_stop_atr_mult", self.trailing_stop_atr_mult),
                    ("trailing_stop_pct", self.trailing_stop_pct),
                ]
                for _field, _val in _sole_owner_conflicts:
                    if _val is not None and _val > 0:
                        raise ValueError(
                            f"{_field} is not allowed alongside a unified "
                            "per-regime close — the close owns the SL via "
                            "per-regime stop_loss_atr"
                        )
                if self.stop_loss_atr_regime is not None or (
                    self.trailing_stop_atr_regime is not None
                ):
                    raise ValueError(
                        "stop_loss_atr_regime/trailing_stop_atr_regime are not "
                        "allowed alongside a unified per-regime close — the "
                        "close owns the SL via per-regime stop_loss_atr"
                    )

            regime_errs: list[str] = []
            if self.stop_loss_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.stop_loss_atr_regime,
                    "stop_loss_atr_regime",
                    SURFACE_STOP_LOSS,
                    labels=self._regime_primary_labels,
                )
                regime_errs.extend(errs)
                self._stop_loss_regime_block = blk
            if self.trailing_stop_atr_regime is not None:
                blk, errs = parse_regime_atr_block(
                    self.trailing_stop_atr_regime,
                    "trailing_stop_atr_regime",
                    SURFACE_TRAILING,
                    labels=self._regime_primary_labels,
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
        # #1058: reject a tiered_tp_atr_regime / tiered_tp_atr_live_regime tier set
        # keyed by labels the primary window's classifier can never emit (e.g. an
        # ADX-keyed block under a composite primary, or the inverse) — live
        # rejects it at config-load (regime_atr.go parseRegimeTPTiers with the
        # ATR-window classifier labels). Without this the tier-fraction parser
        # infers labels from the keys and resolve_regime_tier silently misses on
        # every stamped label, disabling all take-profit tiers (a silent 0-TP run).
        _tier_vocab_errs = self._sl_mod.validate_regime_tiered_tp_labels(
            self._close_refs, labels=self._regime_primary_labels,
        )
        if _tier_vocab_errs:
            raise ValueError(
                "Invalid regime tiered-TP configuration: " + "; ".join(_tier_vocab_errs)
            )
        self._sl_after_rules_static, _sl_parse_errs = (
            self._sl_mod.parse_strategy_tp_sl_after_rules(
                self._close_refs, labels=self._regime_primary_labels)
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
                labels=self._regime_primary_labels,
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

        # #1268: risk-per-trade sizing — mirror the live validateRiskPerTradePct
        # gate: bounds (0, 10] and a stop owner whose distance is resolvable at
        # sizing time. Regime-resolved owners and the unified close resolve
        # their SL after the open, and a margin-pct-only stop needs leverage
        # context this engine doesn't carry — all rejected loudly, exactly the
        # configs the live daemon refuses to load with this field.
        self.risk_per_trade_pct: Optional[float] = None
        if risk_per_trade_pct is not None:
            pct = float(risk_per_trade_pct)
            if not (0 < pct <= 10):
                raise ValueError(
                    f"risk_per_trade_pct must be in (0, 10], got {pct}"
                )
            if self._unified_close_params is not None:
                raise ValueError(
                    "risk_per_trade_pct cannot size from the unified "
                    "per-regime close block — its SL resolves per-regime "
                    "after open, so the stop distance is unknowable at "
                    "sizing time (#1268; live rejects this at config load)"
                )
            if self.stop_loss_atr_regime or self.trailing_stop_atr_regime:
                raise ValueError(
                    "risk_per_trade_pct cannot size from a regime-resolved "
                    "stop owner (stop_loss_atr_regime / "
                    "trailing_stop_atr_regime) — the SL resolves from the "
                    "regime stamped after open (#1268; live rejects this at "
                    "config load)"
                )
            has_atr_owner = (
                (self.trailing_stop_atr_mult or 0) > 0
                or (self.stop_loss_atr_mult or 0) > 0
            )
            has_pct_owner = (
                (self.trailing_stop_pct or 0) > 0
                or (self.stop_loss_pct or 0) > 0
            )
            if not (has_atr_owner or has_pct_owner):
                if (self.stop_loss_margin_pct or 0) > 0:
                    raise ValueError(
                        "risk_per_trade_pct cannot size from a "
                        "stop_loss_margin_pct-only stop in backtests — the "
                        "backtester does not model leverage, so the price "
                        "distance cannot be derived. Use stop_loss_atr_mult, "
                        "trailing_stop_atr_mult, stop_loss_pct, or "
                        "trailing_stop_pct."
                    )
                raise ValueError(
                    "risk_per_trade_pct requires an explicit stop owner "
                    "(stop_loss_atr_mult, trailing_stop_atr_mult, "
                    "stop_loss_pct, or trailing_stop_pct) to derive the "
                    "stop distance from (#1268)"
                )
            self.risk_per_trade_pct = pct
        # One-time-warning latches for the risk-sizing run diagnostics.
        self._risk_cap_warned = False
        self._risk_skip_warned = False

        # #1276: opt-in scale-in / pyramiding, mirroring the live #873
        # semantics. Validated eagerly — a scale_in block without the flag,
        # bad bounds, or the #1268 risk-sizing combo fail at construction,
        # exactly the configs the live daemon refuses to load.
        self.allow_scale_in = bool(allow_scale_in)
        if scale_in and not self.allow_scale_in:
            raise ValueError(
                "scale_in block is set but allow_scale_in is false — enable "
                "allow_scale_in or remove the block (the live daemon rejects "
                "this config at startup)"
            )
        self._scale_in_cfg = _normalize_scale_in_cfg(scale_in)
        if self.allow_scale_in and self.risk_per_trade_pct is not None:
            raise ValueError(
                "allow_scale_in is mutually exclusive with risk_per_trade_pct "
                "(#1268: add legs re-size off the frozen SL geometry, breaking "
                "the constant-dollar-risk invariant; the live daemon rejects "
                "this config at startup)"
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
          - plain signal path: structurally single-leg. Long/flat (default):
            ``signal=1`` opens long, ``signal=-1`` only *closes* the long, so
            ``direction="long"`` already matches live and needs no mask.
            ``direction="short"`` (#989) flips the path's interpretation in
            ``run()`` instead (``-1`` opens a short, ``+1`` closes it) — the
            mask is skipped for the same reason (it would suppress closes).
            ``"both"`` stays unmodelable here (one signal cannot open one
            side and close the other) and is rejected at config/candidate
            load (``run_backtest.load_strategy_config``).
        """
        return sig_int.map(
            lambda s: _apply_direction_invert_value(
                int(s), uses_open_close, self.direction, self.invert_signal,
            )
        ).astype(int)

    def _effective_directional_entry(
        self, current_regime: str, position_regime: str, position_qty: float,
    ) -> tuple[str, bool]:
        entry = _resolve_regime_directional_entry(
            self.regime_directional_policy,
            current_regime,
            position_regime,
            abs(position_qty),
        )
        if entry is None:
            return self.direction or "", self.invert_signal
        return str(entry["direction"]), bool(entry["invert_signal"])

    def _normalize_profile_signals(self, df: pd.DataFrame, uses_open_close: bool) -> None:
        """Normalize each profile's ``signal__<p>`` column exactly like the
        single-signal path (domain check → invert/direction gate → look-ahead
        shift) and shift ``_profile_label`` so the per-bar switch reads bar N's
        label to govern the N+1 fill (#998). Mutates ``df`` in place.

        Each profile differs only in the OPEN signal; closes come from the shared
        close evaluator, so the engine derives ``_open_action__<p>`` from each
        profile's signal but keeps a single (profile-independent) ``_close_fraction``.
        """
        for p in self._profile_alloc["names"]:
            col = "signal__" + p
            sig_raw = df[col].fillna(0).astype(float)
            non_integral = sig_raw[sig_raw != sig_raw.round()]
            if not non_integral.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got non-integral values "
                    f"{sorted(set(non_integral.unique().tolist()))}"
                )
            sig_int = sig_raw.astype(int)
            bad = sig_int[~sig_int.isin([-1, 0, 1])]
            if not bad.empty:
                raise ValueError(
                    f"{col} must be in {{-1, 0, 1}} — got unexpected values "
                    f"{sorted(bad.unique().tolist())}"
                )
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            if uses_open_close:
                df["_open_action__" + p] = (
                    sig_int.map(_open_action_from_signal).shift(1).fillna("none")
                )
            df[col] = sig_int.shift(1).fillna(0).astype(int)
        # Dummy single-signal column so downstream code that references
        # ``df["signal"]`` doesn't KeyError; the per-bar loop overrides ``signal``
        # (and ``open_action``) from the active profile each bar.
        df["signal"] = 0
        if uses_open_close:
            df["_open_action"] = "none"
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)
        df["_profile_label"] = df["_profile_label"].shift(1).fillna("")

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
        # #989: short/flat plain path — the exact mirror of the structural
        # long/flat path, engaged by direction="short" with no close evaluator
        # (live open-as-close semantics on a short-only strategy): signal=-1
        # OPENS a short, signal=+1 CLOSES it. The open/close engine path is
        # unaffected (direction masking there already models short opens).
        # direction="both" remains unmodelable on the plain path (one signal
        # cannot both open one side and close the other) — rejected at
        # config/candidate load AND here, so API callers that bypass the
        # loaders cannot silently score a long/flat run as "both".
        if self.direction == "both" and not uses_open_close:
            raise ValueError(
                "direction='both' requires a close evaluator (open/close "
                "engine path) — the plain single-leg path cannot open one "
                "side and close the other, so the run would silently score "
                "long/flat. Backtest each leg separately with "
                "direction='long' / direction='short'."
            )
        if self.regime_directional_policy is not None and not uses_open_close:
            both_labels = sorted(
                label for label, entry in self.regime_directional_policy.items()
                if entry.get("direction") == "both"
            )
            if both_labels:
                raise ValueError(
                    "regime_directional_policy direction='both' requires a "
                    "close evaluator on the plain signal path; labels with "
                    f"both: {both_labels}"
                )
        plain_short = (not uses_open_close) and self.direction == "short"
        if plain_short and starting_long:
            raise ValueError(
                "starting_long cannot seed a direction='short' plain-path "
                "run — the short/flat path never emits a long close, so the "
                "seeded long would be carried untouched to end-of-data."
            )
        has_profile_alloc = self._profile_alloc is not None
        if has_profile_alloc:
            if "_profile_label" not in df.columns:
                raise ValueError(
                    "regime_profile_allocation backtest requires a '_profile_label' column"
                )
            missing = [
                p for p in self._profile_alloc["names"]
                if ("signal__" + p) not in df.columns
            ]
            if missing:
                raise ValueError(
                    f"regime_profile_allocation backtest is missing signal columns "
                    f"for profiles {missing} (expected 'signal__<profile>')"
                )
        if "signal" not in df.columns and not uses_open_close and not has_profile_alloc:
            raise ValueError("DataFrame must have a 'signal' column or open_action/close_fraction columns")

        df = df.copy()
        if has_profile_alloc:
            self._normalize_profile_signals(df, uses_open_close)
        elif "signal" in df.columns:
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
            # #942: static entry transforms (invert_signal, then direction
            # gating) apply BEFORE the look-ahead shift. #1025 regime policies
            # are position-aware, so they replay later inside the per-bar loop
            # where the stamped position regime is known.
            if self.regime_directional_policy is None:
                sig_int = self._apply_direction_invert(sig_int, uses_open_close)
            signal_for_open = sig_int
            df["signal"] = sig_int.shift(1).fillna(0).astype(int)
        else:
            signal_for_open = pd.Series(0, index=df.index)
            df["signal"] = 0

        if uses_open_close and not has_profile_alloc:
            if "open_action" in df.columns:
                open_actions = df["open_action"].map(_normalize_open_action)
            else:
                open_actions = signal_for_open.map(_open_action_from_signal)
            df["_open_action"] = open_actions.shift(1).fillna("none")
            df["_close_fraction"] = _max_close_fraction_series(df).shift(1).fillna(0.0)

        # #980: per-bar entry sizing. ``entry_fraction`` is a decision input
        # computed from bar N's closed data, so it shifts forward with the
        # signal; the shift hole and NaN (warmup / no opinion) resolve to
        # 1.0 = full notional, today's behavior. Applies to every open path
        # (plain long/short, open/close engine, profile allocation — the
        # column is profile-independent, like ``_close_fraction``).
        if "entry_fraction" in df.columns:
            df["_entry_fraction"] = (
                _validated_entry_fraction_series(df).shift(1).fillna(1.0)
            )

        # Regime: inject vectorized labels before the per-bar loop so each bar
        # can gate new entries. Mirrors the live path: latest_regime(df) on the
        # same OHLCV window → identical label by construction (same algorithm).
        if self.regime_enabled and "regime" not in df.columns:
            ensure_regime = _load_regime()
            # #1058: when a composite windows spec is threaded, ensure_regime_columns
            # classifies the PRIMARY window (medium-first) — composite (9-state) or
            # ADX — exactly as live's regime store does for the strategy_regime that
            # feeds close evaluators. Without it, the legacy single-lookback ADX
            # (period / adx_threshold) path is unchanged.
            ensure_regime(
                df,
                period=self.regime_period,
                adx_threshold=self.regime_adx_threshold,
                windows_spec=self.regime_windows_spec,
            )

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
        # rows from upstream gaps) → empty string after fillna. The entry
        # gate resolves that empty label per ``regime_gate_on_failure``
        # (#1278): the default ``"open"`` admits the entry (matching the
        # live #879 fail-open gate); ``"closed"`` blocks it. Intentional —
        # do not "fix" the fillna to forward-fill or interpolate: the empty
        # label is the honest "no regime data at decision time" signal the
        # policy keys off.
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
        # #997: reason that produced the pending close, carried to the next
        # bar's open-fill so the closed leg records WHICH mechanism exited it.
        pending_close_reason = ""
        # #997 hold telemetry accumulator (intra-hold excursions + bars held).
        hold = _HoldTracker()
        # #1276 scale-in state (anchor/counters/first-leg notional) + totals.
        scale = _ScaleInState()
        scale_in_adds_total = 0
        scale_in_added_notional_total = 0.0

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
        # #1271: intra-bar OHLC-walk SL resolution (default mode). The pierce
        # check reads the fill bar's full range, so it must only ever run
        # against a trigger that was actually ARMED during the bar:
        #   - a trigger carried from a prior bar (or bumped at this bar's
        #     open by sl_after — suppressed that bar via #715, live next bar),
        #   - a fixed (entry-geometry) trigger seeded at this bar's open.
        # A trailing seed is anchored at the bar's CLOSE (the engine's
        # bar-after-open arming convention — see the module docstring), so it
        # is NOT an armed level during its entry bar; pierce-checking it there
        # would stop out at a level derived from data the bar hadn't produced
        # yet. ``sl_pierce_armed`` tracks that: set per-seed at the open
        # blocks, promoted to True at the end of every bar a position
        # survives (by then any trigger is a real carried level).
        walk_mode = self.intrabar_resolution == "ohlc_walk"
        sl_pierce_armed = False
        self._active_sl_after_rules = self._sl_after_rules_static
        self._run_tp_tier_thresholds = list(self._tp_tier_thresholds_static)
        self._run_stop_loss_atr_mult: Optional[float] = None
        self._run_trailing_stop_atr_mult: Optional[float] = None
        self._run_position_regime = ""
        sl_after_active = self._sl_after_pipeline_enabled
        trailing_ratchet_active = self._uses_trailing_ratchet_close

        # #997 zscore_target: rolling z of close over the ref's lookback,
        # computed from closed-bar data. Bar N's value uses bars [N-lb+1, N]
        # (population std, ddof=0); warmup rows are NaN and the evaluator
        # no-ops on them. Passed to the evaluator at end-of-bar exactly like
        # ATR, so the resulting close fills at the next bar's open.
        zscore_series = None
        if self._zscore_lookback > 0 and "close" in df.columns:
            lb = self._zscore_lookback
            closes = df["close"].astype(float)
            roll = closes.rolling(lb)
            std = roll.std(ddof=0)
            zscore_series = (closes - roll.mean()) / std.replace(0.0, float("nan"))

        # #1196: the open strategy's anchored VWAP line for avwap_stop. Same
        # current-bar end-of-bar access contract as ATR below; the column is
        # causal (the AVWAP anchor at bar b uses only pivots confirmed by b).
        avwap_series = df["avwap"] if "avwap" in df.columns else None
        # #1196 review: if avwap_stop is configured but the open strategy never
        # produces a usable avwap line (no column, or all-NaN/non-positive across
        # the whole run), the exit can never fire — warn once per run rather than
        # no-opping silently on every bar. Fails safe either way (engine SL still
        # protects the position).
        if self._close_names_include_avwap_stop():
            avwap_usable = avwap_series is not None and bool(
                (pd.to_numeric(avwap_series, errors="coerce") > 0).any()
            )
            if not avwap_usable:
                from strategy_composition import warn_avwap_stop_missing_context
                warn_avwap_stop_missing_context()

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
                    labels=self._regime_primary_labels,
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
            # #841 unified per-regime close: the block owns the SL — resolve
            # the stamped regime's stop_loss_atr as the run's fixed-SL mult
            # (mirrors unifiedCloseStopLossATR in scheduler/regime_unified.go).
            if (
                self._run_stop_loss_atr_mult is None
                and self._unified_close_params is not None
                and self._unified_scalar_params is not None
                and lab
            ):
                _, _usl = self._unified_scalar_params(
                    self._unified_close_params, lab
                )
                if _usl and _usl > 0:
                    self._run_stop_loss_atr_mult = float(_usl)
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
            # #1276: a seeded position starts un-scaled; its first-leg
            # notional is the seeded commitment.
            scale.reset()
            scale.base_open_notional = position * effective_entry
            # #997: seed hold telemetry for the walk-forward-seeded position.
            # bars_held starts at 0 (its true warmup hold length is unknown).
            hold.open(effective_entry, "long", entry_commission)
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
            # Arm the carried position's SL the same way the open block does
            # (PR #1000 review): a seeded position never routes through the
            # open block, so without this the fixed/trailing trigger stays at
            # 0 for its entire lifetime and ATR stacks score as
            # hold-to-reversal on every fold that opens already-long. The
            # trail anchors at max(entry, seed high-water) — live would have
            # walked the HWM through the warmup bars. Works for both paths:
            # the plain signal path's hit check reads the same sl_trigger_px.
            seed_hwm = starting_long.get("high_water", 0.0)
            try:
                seed_hwm = float(seed_hwm or 0.0)
            except (TypeError, ValueError):
                seed_hwm = 0.0
            hwm_anchor = max(effective_entry, seed_hwm)
            if sl_after_active and self._run_tp_tier_thresholds:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                sl_high_water_px = 0.0
            elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                sl_trigger_px = _initial_trail_trigger(
                    "long", hwm_anchor, entry_atr_value,
                    self._run_trailing_stop_atr_mult,
                )
                sl_high_water_px = hwm_anchor
            else:
                sl_trigger_px = self._initial_sl_trigger(
                    "long", avg_cost, entry_atr_value,
                )
                if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                    sl_trigger_px = _initial_trail_trigger(
                        "long", hwm_anchor, entry_atr_value,
                        self._run_trailing_stop_atr_mult,
                    )
                sl_high_water_px = hwm_anchor
            sl_tiers_processed = 0
            post_tp_trail_mult = None
            # #1271: every seeded-position trigger above is entry/warmup-
            # derived (avg_cost geometry or the carried high-water anchor) —
            # a real level as of bar 0's open, so it's pierce-eligible.
            sl_pierce_armed = True

        profile_switcher = (
            _ProfileSwitcher(self._profile_alloc) if has_profile_alloc else None
        )
        active_profile = ""

        # #988: carry strategies (delta_neutral_funding) attach a per-bar
        # `funding_accrual` column — the total funding rate accrued over the bar.
        # When present, book it each bar against the position carried into the
        # bar. Auto-detected so no construction-site plumbing is needed; other
        # strategies' frames have no such column and are unaffected.
        book_funding = "funding_accrual" in df.columns
        total_funding_pnl = 0.0

        has_entry_fraction = "_entry_fraction" in df.columns

        # #1268: risk-per-trade sizing. Mutually exclusive with a
        # strategy-emitted entry_fraction column — live sizing has no
        # entry_fraction input, so composing the two would diverge from the
        # live formula the mode exists to reproduce.
        risk_mode = (self.risk_per_trade_pct or 0) > 0
        if risk_mode and has_entry_fraction:
            raise ValueError(
                "risk_per_trade_pct is mutually exclusive with a "
                "strategy-emitted entry_fraction column (#1268) — the live "
                "sizer has no entry_fraction input, so composing them would "
                "diverge from the live sizing formula"
            )
        risk_skipped_entries = 0

        # #1276: scale-in state + per-run totals. The entry_fraction column is
        # a fresh-entry research hook with no live counterpart — composing it
        # with the live add-sizing rule would make the default per-add
        # notional (the first leg's committed notional) depend on a
        # backtest-only input; reject loudly like the #1268 combo above.
        if self.allow_scale_in and has_entry_fraction:
            raise ValueError(
                "allow_scale_in is mutually exclusive with a strategy-emitted "
                "entry_fraction column (#1276) — the live per-add sizing "
                "(the fresh-open notional) has no entry_fraction input, so "
                "composing them would diverge from the live add-sizing rule"
            )
        # Decision-time price for the add gate: the signal bar's close (bar
        # N; live evaluates spacing at cycle time ≈ that close). The signal
        # is already shift(1)'d, so on the fill row (N+1) the decision price
        # is the PREVIOUS row's close.
        prev_close_arr = (
            df["close"].shift(1).to_numpy(dtype=float)
            if self.allow_scale_in else None
        )

        def _try_scale_in_add(i: int, side: str, fill_price: float) -> bool:
            """Attempt a same-direction add on the fill row (#1276). Gate
            evaluated against bar N's close; fill at this row's open with
            slippage + commission. Mutates position/cash/avg_cost/
            initial_quantity/current_trade/hold + the scale state — the
            Python image of live's perpsScaleInDecision + applyScaleIn.
            Returns True when an add was applied."""
            nonlocal position, cash, avg_cost, initial_quantity
            nonlocal scale_in_adds_total, scale_in_added_notional_total
            decision_price = float(prev_close_arr[i])
            if not (decision_price > 0):  # NaN on row 0
                return False
            default_notional = scale.base_open_notional
            add_qty, ok, _reason = _scale_in_decision(
                self._scale_in_cfg, side, abs(position), avg_cost,
                entry_atr_value, scale.scale_in_count,
                scale.added_notional_usd, scale.last_add_price,
                1 if side == "long" else -1, decision_price, default_notional,
            )
            if not ok or add_qty <= 0:
                return False
            if side == "long":
                eff = fill_price * (1 + self.slippage_pct)
            else:
                eff = fill_price * (1 - self.slippage_pct)
            notional = add_qty * eff
            commission = notional * self.commission_pct
            if side == "long":
                # Notional leaves cash (may go negative — the spot-model
                # image of live's margin-based virtual notional; equity =
                # cash + position×mark stays exact). See module docstring.
                cash -= notional + commission
                position += add_qty
            else:
                # Net-proceeds form of the existing short-open math.
                cash += notional - commission
                position -= add_qty
            # The blend — exactly applyScaleIn (scheduler/scale_in.go):
            # freeze the anchor to the pre-blend avg_cost on the FIRST add,
            # blend avg_cost, grow InitialQuantity; EntryATR / the stamped
            # regime / tier watermarks stay untouched (frozen entry).
            if scale.risk_anchor_price <= 0:
                scale.risk_anchor_price = avg_cost
            old_qty = abs(position) - add_qty
            new_qty = old_qty + add_qty
            avg_cost = (old_qty * avg_cost + add_qty * eff) / new_qty
            initial_quantity += add_qty
            scale.scale_in_count += 1
            scale.last_add_price = eff
            scale.added_notional_usd += notional
            scale_in_adds_total += 1
            scale_in_added_notional_total += notional
            if current_trade is not None:
                # PnL uses the blend: the leg's entry price IS the blended
                # avg_cost (Trade.close computes pnl from entry_price).
                current_trade.entry_price = avg_cost
                current_trade.shares += add_qty
                current_trade.scale_in_adds = scale.scale_in_count
            # The add's commission joins the single entry-fee pool that
            # _stamp_hold pro-rates across close legs (#1241).
            hold.entry_fee += commission
            return True

        for i, (idx, row) in enumerate(df.iterrows()):
            fill_price = row["open"] if has_open else row["close"]
            mark_price = row["close"]
            signal = row["signal"]
            # #980: fraction of flat-state cash committed if an entry fills at
            # this bar's open. Normalized/validated pre-loop; 1.0 = full
            # notional (the only value when no ``entry_fraction`` column).
            entry_fraction = (
                float(row["_entry_fraction"]) if has_entry_fraction else 1.0
            )
            # #1268: in risk mode the committed fraction is derived per open
            # from the stop distance; an unresolvable distance (no usable ATR
            # at the signal bar) blocks the entry — fail-closed like live,
            # never a full-notional fallback. Closes are never blocked.
            risk_entry_blocked = False
            if risk_mode:
                risk_fraction = self._risk_entry_fraction(
                    atr_series, idx, fill_price,
                )
                if risk_fraction is None:
                    risk_entry_blocked = True
                    entry_wanted = (
                        str(row.get("_open_action", "none")) in ("long", "short")
                        if uses_open_close
                        else int(signal) != 0
                    )
                    if position == 0 and entry_wanted:
                        risk_skipped_entries += 1
                        if not self._risk_skip_warned:
                            self._risk_skip_warned = True
                            print(
                                f"[#1268] risk_per_trade_pct: entry skipped at "
                                f"{idx} — stop distance unresolvable (no usable "
                                f"ATR at the signal bar); fail-closed, matching "
                                f"live. Further skips counted silently.",
                                file=sys.stderr,
                            )
                else:
                    entry_fraction = risk_fraction
            # #998: regime-profile allocation — advance the flat-only, confirm_bars
            # hysteresis switch and read the active profile's signal for this bar.
            # ``flat`` is the position carried into the bar (= the position state at
            # the decision time the shifted columns correspond to).
            if profile_switcher is not None:
                active_profile = profile_switcher.step(
                    str(row.get("_profile_label", "") or ""), position == 0
                )
                signal = row["signal__" + active_profile]

            bar_regime = str(row.get("regime", "")) if self.regime_enabled else ""
            effective_direction = self.direction or ""
            effective_invert = self.invert_signal
            plain_short_for_bar = plain_short
            if self.regime_directional_policy is not None:
                effective_direction, effective_invert = self._effective_directional_entry(
                    bar_regime,
                    self._run_position_regime,
                    abs(position),
                )
                if not uses_open_close:
                    signal = _apply_direction_invert_value(
                        int(signal),
                        uses_open_close=False,
                        direction=effective_direction,
                        invert_signal=effective_invert,
                    )
                    plain_short_for_bar = effective_direction == "short"

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

            # #988: book funding carry on the position carried into this bar,
            # over this bar's interval, before marking equity to the close. A
            # long pays funding when the rate is positive, a short receives it:
            #   funding_cash = -position * mark * rate
            # (position is signed: + long, - short), which lands in `cash` and
            # flows into the close-marked equity below. Newly opened positions
            # this bar start accruing next bar; closed positions stop after
            # their last full bar — the standard discrete one-bar convention.
            if book_funding and position != 0:
                accrual = row.get("funding_accrual", 0.0)
                accrual = float(accrual) if accrual == accrual else 0.0  # NaN→0
                if accrual != 0.0:
                    funding_cash = -position * mark_price * accrual
                    cash += funding_cash
                    total_funding_pnl += funding_cash

            equity = cash + position * mark_price
            equity_curve.append({"date": idx, "equity": equity})

            # Regime gate: block new entries when the prior bar's regime
            # isn't in the allowed set. Existing positions are always managed
            # by close paths. ``compute_regime`` initializes every row to
            # ``"ranging"`` (warmup bars included). After the post-injection
            # shift (#730) row 0 is empty — an empty label resolves per
            # ``regime_gate_on_failure`` (#1278): the default ``"open"``
            # ADMITS the bar-0 entry (matching the live #879 fail-open gate);
            # ``"closed"`` blocks it (matching live fail-closed).
            regime_blocked = (
                self.regime_enabled
                and bool(self.allowed_regimes)
                and not _regime_allows_entry(
                    self.allowed_regimes, bar_regime, self.regime_gate_on_failure
                )
            )

            if uses_open_close:
                col_close_fraction = float(row.get("_close_fraction", 0.0))
                # #997: attribute the exit. The column-driven fraction (open
                # signal acting as close) wins ties; otherwise the pending
                # reason from the prior bar's evaluator / SL hit carries.
                if col_close_fraction >= pending_close_fraction:
                    close_fraction = col_close_fraction
                    close_reason = "column_close_fraction" if col_close_fraction > 0 else ""
                else:
                    close_fraction = pending_close_fraction
                    close_reason = pending_close_reason
                pending_close_fraction = 0.0
                pending_close_reason = ""
                if profile_switcher is not None:
                    open_action = row.get("_open_action__" + active_profile, "none")
                else:
                    open_action = row.get("_open_action", "none")
                # #1025: capture the raw open-action signal now; the directional
                # gate is applied AFTER the close leg below (see the gate just
                # before the open block) so a same-bar close→reopen re-resolves
                # the entry direction against the regime that applies to a flat
                # book at this bar, not the just-closed position's frozen regime.
                raw_open_signal = (
                    _signal_from_open_action(open_action)
                    if self.regime_directional_policy is not None
                    else 0
                )

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
                        # Exit fee (and pro-rated entry fee) is netted inside
                        # _stamp_hold below — do NOT deduct it here too (#1241).
                        # #997: stamp hold telemetry. This leg exits at THIS
                        # bar's open, so hold reflects bars through the prior
                        # bar (step() for this bar runs after the open-fill
                        # block, below). Entry fee is pro-rated by the leg's
                        # share of the original position.
                        qty_frac = (qty_to_close / initial_quantity) if initial_quantity > 0 else 1.0
                        _stamp_hold(closed, hold, entry_atr=entry_atr_value,
                                    exit_fee=commission,
                                    reason=close_reason or "close_strategy",
                                    qty_frac=qty_frac)
                        closed.scale_in_adds = scale.scale_in_count
                        trades.append(closed)
                        current_trade.shares -= qty_to_close
                        if current_trade.shares <= 1e-12:
                            current_trade = None

                    if abs(position) <= 1e-12:
                        position = 0.0
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        scale.reset()
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
                                # #1276: bump geometry pins to the frozen
                                # first-entry anchor across scale-in adds
                                # (mirrors post_tp_sl.go riskAnchorPrice()).
                                avg_cost=scale.geom_cost(avg_cost),
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

                # #1025 same-bar flip: resolve the entry direction/invert
                # against the position state AFTER this bar's close leg. The
                # open block below only ever fires from a flat book, so its gate
                # must use the regime that applies to a flat book at this bar —
                # never a regime frozen to a position that was already fully
                # closed earlier in the same bar. When the close above fully
                # exited the incoming position, _run_position_regime was cleared
                # (position == 0) and _effective_directional_entry falls through
                # to the CURRENT bar regime, matching live (which re-evaluates a
                # fresh entry from the current regime on the next cycle). A
                # surviving leg (partial close, or no close) keeps position != 0
                # and its frozen regime, so the open block won't fire and the
                # freeze is preserved unchanged.
                if self.regime_directional_policy is not None:
                    entry_direction, entry_invert = self._effective_directional_entry(
                        bar_regime,
                        self._run_position_regime,
                        abs(position),
                    )
                    open_action = _open_action_from_signal(
                        _apply_direction_invert_value(
                            raw_open_signal,
                            uses_open_close=True,
                            direction=entry_direction,
                            invert_signal=entry_invert,
                        )
                    )

                # Entry guard (PR #1004 review): a blown short can leave
                # flat-state cash <= 0 (buy-back cost exceeded the 2x notional
                # held). Opening from non-positive cash computes negative
                # shares, silently flipping the position sign against the
                # booked trade side and inverting all subsequent PnL. The
                # account is economically bust — skip the entry. cash == 0 is
                # included: it would book a zero-share phantom trade.
                if open_action == "long" and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                    effective_price = fill_price * (1 + self.slippage_pct)
                    # #980: commit entry_fraction of flat-state cash; the
                    # remainder stays as a reserve (fraction 1.0 = full
                    # notional, today's math exactly).
                    invest = cash * entry_fraction
                    commission = invest * self.commission_pct
                    available = invest - commission
                    shares = available / effective_price
                    position = shares
                    cash -= invest

                    current_trade = Trade(idx, effective_price, "long")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "long", commission)
                    # #1276: fresh open — clean scale-in state; freeze the
                    # first leg's committed notional as the default per-add
                    # size (the backtest image of live's PerpsOpenNotional).
                    scale.reset()
                    scale.base_open_notional = shares * effective_price
                    stamp_open_from_label(_entry_stamp(row))
                    # Seed the SL trigger at open. sl_after configs seed only
                    # when usable tier thresholds exist (#716 item 3 — without
                    # thresholds the post-TP machinery never fires); otherwise
                    # a bare fixed/trailing stop alongside a close evaluator is
                    # seeded here and simulated by the end-of-bar hit check
                    # below (#996 — live arms this SL via
                    # runHyperliquidProtectionSync / armTrailingStopAtOpenNow
                    # independently of sl_after; pre-#996 the engine path
                    # silently dropped it).
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        # #1271: entry-geometry (fixed) trigger — armed at the
                        # open, pierce-eligible on the fill bar itself.
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "long", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        # #1271: close-anchored trailing seed — not an armed
                        # level during this bar; pierce-eligible next bar.
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        sl_trigger_px = self._initial_sl_trigger(
                            "long", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "long", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                elif open_action == "short" and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                    effective_price = fill_price * (1 - self.slippage_pct)
                    # #980: entry_fraction of flat-state cash is the committed
                    # margin; pay commission from it, receive the short-sale
                    # proceeds on top of the untouched reserve (fraction 1.0
                    # reduces to cash = 2 * notional, today's math exactly).
                    margin = cash * entry_fraction
                    commission = margin * self.commission_pct
                    notional = margin - commission
                    shares = notional / effective_price
                    cash += 2 * notional - margin
                    position = -shares

                    current_trade = Trade(idx, effective_price, "short")
                    current_trade.shares = shares
                    avg_cost = effective_price
                    initial_quantity = shares
                    entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                    hold.open(effective_price, "short", commission)
                    # #1276: fresh open — see the long-side comment.
                    scale.reset()
                    scale.base_open_notional = shares * effective_price
                    stamp_open_from_label(_entry_stamp(row))
                    if sl_after_active and self._run_tp_tier_thresholds:
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        # #1271: fixed trigger — pierce-eligible on the fill
                        # bar; trailing seeds arm next bar (long-side comment).
                        sl_pierce_armed = sl_trigger_px > 0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                    elif trailing_ratchet_active and self._run_trailing_stop_atr_mult:
                        sl_trigger_px = _initial_trail_trigger(
                            "short", mark_price, entry_atr_value,
                            self._run_trailing_stop_atr_mult,
                        )
                        sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                    else:
                        # Bare fixed/trailing stop with a close evaluator —
                        # see the long-side comment (#996).
                        sl_trigger_px = self._initial_sl_trigger(
                            "short", avg_cost, entry_atr_value,
                        )
                        sl_pierce_armed = sl_trigger_px > 0
                        if sl_trigger_px <= 0 and self._run_trailing_stop_atr_mult:
                            sl_trigger_px = _initial_trail_trigger(
                                "short", mark_price, entry_atr_value,
                                self._run_trailing_stop_atr_mult,
                            )
                            sl_pierce_armed = False
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = mark_price
                # #1276: same-direction signal on an OPEN position — a
                # scale-in add when opted in and the caps/spacing gate
                # passes; otherwise the (unchanged) skip. Deliberately NOT
                # gated on cash > 0 (margin semantics — see the module
                # docstring), regime_blocked (live regimeBlocksOpen passes
                # posQty>0), or risk_entry_blocked (#1268 mutual exclusion
                # makes risk mode unreachable here). open_action was already
                # re-resolved position-aware for #1025 directional policies
                # above, so the add-intent honors the position-frozen regime.
                elif (
                    self.allow_scale_in
                    and open_action == "long"
                    and position > 0
                ):
                    _try_scale_in_add(i, "long", fill_price)
                elif (
                    self.allow_scale_in
                    and open_action == "short"
                    and position < 0
                ):
                    _try_scale_in_add(i, "short", fill_price)

                # #997: advance hold telemetry for the position held through
                # this bar. Runs AFTER the open-fill close/open block so a leg
                # closed at this bar's open excludes this bar's range, while a
                # position opened (or held) at this bar's open includes it.
                # Output-only — never feeds a decision — so reading this bar's
                # high/low at its close is look-ahead-safe.
                if position != 0:
                    hold.step(
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                    )

                # #1271 intra-bar OHLC-walk SL resolution (default mode): a
                # bar whose range touches the armed trigger stops the
                # position out ON this bar — at the trigger price, or at the
                # bar's open when the bar gapped through the trigger.
                # Adverse-move-first by construction: the close evaluators
                # below never see the stopped-out position, so a same-bar TP
                # the bar-close mark would have credited loses the race.
                # Suppressed on sl_after bump bars (#715 — live's fresh SL
                # OID lands mid-cycle, active from the next bar) and while a
                # close-anchored trailing seed is unarmed on its entry bar
                # (see the open blocks above / module docstring).
                if (
                    walk_mode
                    and position != 0
                    and sl_pierce_armed
                    and sl_trigger_px > 0
                    and not sl_after_just_applied
                    and avg_cost > 0
                ):
                    side_now = "long" if position > 0 else "short"
                    raw_fill = self._intrabar_sl_fill(
                        side_now,
                        float(row["open"]) if has_open else mark_price,
                        float(row.get("high", mark_price) or mark_price),
                        float(row.get("low", mark_price) or mark_price),
                        sl_trigger_px,
                    )
                    if raw_fill is not None:
                        qty_to_close = abs(position)
                        if position > 0:
                            effective_price = raw_fill * (1 - self.slippage_pct)
                            proceeds = qty_to_close * effective_price
                            commission = proceeds * self.commission_pct
                            cash += proceeds - commission
                        else:
                            effective_price = raw_fill * (1 + self.slippage_pct)
                            cost = qty_to_close * effective_price
                            commission = cost * self.commission_pct
                            cash -= cost + commission
                        position = 0.0
                        if current_trade:
                            closed = Trade(
                                current_trade.entry_date,
                                current_trade.entry_price,
                                current_trade.side,
                            )
                            closed.shares = qty_to_close
                            closed.close(idx, effective_price)
                            # Entry fee pro-rated by this leg's share of the
                            # original position (earlier partial-close legs
                            # already netted theirs) — #1241 convention.
                            qty_frac = (
                                qty_to_close / initial_quantity
                                if initial_quantity > 0 else 1.0
                            )
                            _stamp_hold(closed, hold,
                                        entry_atr=entry_atr_value,
                                        exit_fee=commission, reason="sl",
                                        qty_frac=qty_frac)
                            closed.scale_in_adds = scale.scale_in_count
                            trades.append(closed)
                            current_trade = None
                        avg_cost = 0.0
                        initial_quantity = 0.0
                        entry_atr_value = 0.0
                        scale.reset()
                        sl_trigger_px = 0.0
                        sl_tiers_processed = 0
                        post_tp_trail_mult = None
                        sl_high_water_px = 0.0
                        sl_pierce_armed = False
                        self._active_sl_after_rules = self._sl_after_rules_static
                        self._run_tp_tier_thresholds = list(
                            self._tp_tier_thresholds_static,
                        )
                        self._run_stop_loss_atr_mult = None
                        self._run_trailing_stop_atr_mult = None
                        self._run_position_regime = ""

                # End-of-bar: evaluate close strategies against the now-current
                # position using this bar's close as the mark. The result is
                # applied at the NEXT bar's open (mirrors live: eval at end of
                # bar, fill at next open).
                if self.close_strategies and position != 0 and avg_cost > 0:
                    # #1276: evaluator geometry (tiered-TP thresholds etc.)
                    # reads the frozen anchor, never the blend — mirroring
                    # the live ON-CHAIN protection path
                    # (hyperliquid_protection.go / hyperliquid_balance.go
                    # via riskAnchorPrice()); PnL stays on the blend.
                    pending_close_fraction, pending_close_reason = self._evaluate_close_strategies(
                        position, scale.geom_cost(avg_cost), initial_quantity,
                        entry_atr_value,
                        mark_price, atr_series, idx,
                        position_regime=self._run_position_regime,
                        market_regime=_bar_close_regime(row),
                        bars_held=hold.bars,
                        zscore_series=zscore_series,
                        avwap_series=avwap_series,
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
                                # #1276: ratchet tier geometry pins to the
                                # frozen anchor (trailing_tp_ratchet.go).
                                avg_cost=scale.geom_cost(avg_cost),
                                entry_atr=entry_atr_value,
                                side=side_now,
                                post_tp_trail_mult=post_tp_trail_mult,
                                trailing_stop_atr_mult=base_trail,
                            )
                        )

                # End-of-bar: walk the trailing-stop high-water mark (for
                # trail_from_here transitions, the ratchet, or a bare scalar
                # trailing stop) and check whether the SL trigger has been
                # hit by this bar's close. A hit produces
                # pending_close_fraction=1.0 which fills at the next bar's
                # open — same alignment as the rest of the close pipeline.
                # #996: also runs for bare fixed/trailing/pct stops paired
                # with a close evaluator, which live arms independently of
                # sl_after (pre-#996 these were silently dropped here).
                scalar_stop_active = (
                    (self._run_stop_loss_atr_mult or 0) > 0
                    or (self._run_trailing_stop_atr_mult or 0) > 0
                    or (self.stop_loss_pct or 0) > 0
                )
                if (
                    (sl_after_active or trailing_ratchet_active
                     or scalar_stop_active)
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
                    # #1271: the close-detected hit (filled at the next bar's
                    # open) is the LEGACY resolution — under the default
                    # OHLC walk it is strictly subsumed by the intra-bar
                    # pierce check above (close <= trigger implies the low
                    # touched it), which already booked the exit at the
                    # trigger price on this bar.
                    if not walk_mode and sl_trigger_px > 0 and self._sl_hit(
                        side_now, mark_price, sl_trigger_px,
                    ):
                        pending_close_fraction = 1.0
                        pending_close_reason = "sl"
                # #1271: any trigger surviving to a bar's end (seeded,
                # walked, or sl_after-bumped) is a real carried level for
                # the next bar — pierce-eligible from here on.
                if position != 0:
                    sl_pierce_armed = True
                continue

            # Standalone hard stop fires first: close at this bar's open before
            # any new signal is acted on (the hit was flagged on the prior bar's
            # close, so this is the next-bar-open fill).
            if pending_signal_sl_close and position > 0:
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                # #980: additive — a fractional entry leaves a cash reserve
                # (and funding accrual lands in cash) that an overwrite here
                # would silently destroy. Identical when cash == 0.
                cash += proceeds - commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""
                continue

            # Short/flat mirror of the standalone-stop fill above: buy back
            # the short at this bar's open (#989).
            if pending_signal_sl_close and position < 0:
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0
                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal_sl")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                pending_signal_sl_close = False
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""
                continue

            # NOTE: this signal path runs one leg at a time. Default
            # (long/flat): signal == 1 opens a long, signal == -1 only
            # *closes* it; a short is never opened. With direction="short"
            # (#989) the interpretation mirrors: signal == -1 opens a short,
            # signal == 1 only *closes* it. Bidirectional strategies are
            # therefore measured one leg per run — long leg by default,
            # short leg via direction="short" — never both in one run.
            # ``cash > 0`` on every open: a blown short leaves flat-state cash
            # <= 0, and opening from it computes negative shares — a phantom
            # position whose sign contradicts the booked side (PR #1004
            # review). Bust account: entries skip until end of data. The
            # long/flat path can't reach negative cash today, but carries the
            # same guard so the invariant holds by construction.
            if plain_short_for_bar and signal == -1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                # SELL — open short. Mirrors the engine path's short-open
                # mechanics: pay commission from the committed margin, receive
                # the short-sale proceeds (#980: entry_fraction scales the
                # committed margin; 1.0 reduces to cash = 2 * notional).
                effective_price = fill_price * (1 - self.slippage_pct)
                margin = cash * entry_fraction
                commission = margin * self.commission_pct
                notional = margin - commission
                shares = notional / effective_price
                cash += 2 * notional - margin
                position = -shares

                current_trade = Trade(idx, effective_price, "short")
                current_trade.shares = shares
                # #1276: fresh open — clean scale-in state; freeze the first
                # leg's committed notional as the default per-add size.
                scale.reset()
                scale.base_open_notional = shares * effective_price

                # Standalone stop seeding — mirror of the long block below
                # (fixed ATR mult > trailing ATR mult > fixed pct), triggers
                # placed ABOVE the entry for a short.
                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "short", commission)
                stamp_open_from_label(_entry_stamp(row))
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                # #1271: fixed (avg_cost-geometry) seeds are pierce-eligible
                # on the fill bar; the close-anchored trailing seed arms on
                # the next bar (see the open/close-path open blocks).
                sl_pierce_armed = False
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost + self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price + self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 + self.stop_loss_pct)
                    sl_pierce_armed = True

            elif plain_short_for_bar and signal == 1 and position < 0:
                # BUY — close short (buy back)
                effective_price = fill_price * (1 + self.slippage_pct)
                cost = abs(position) * effective_price
                commission = cost * self.commission_pct
                cash -= cost + commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""

            elif not plain_short_for_bar and signal == 1 and position == 0 and cash > 0 and not regime_blocked and not risk_entry_blocked:
                # BUY — go long (#980: entry_fraction of flat-state cash;
                # 1.0 = all available cash, today's math exactly)
                effective_price = fill_price * (1 + self.slippage_pct)
                invest = cash * entry_fraction
                commission = invest * self.commission_pct
                available = invest - commission
                shares = available / effective_price
                position = shares
                cash -= invest

                current_trade = Trade(idx, effective_price, "long")
                current_trade.shares = shares
                # #1276: fresh open — see the short-side comment above.
                scale.reset()
                scale.base_open_notional = shares * effective_price

                # Seed a standalone stop for the plain signal path (fixed ATR
                # mult > trailing ATR mult > fixed pct). entry_atr is the
                # closed-bar ATR at the fill bar (same convention as the
                # open/close path's _stamp_entry_atr).
                avg_cost = effective_price
                entry_atr_value = self._stamp_entry_atr(atr_series, idx, effective_price)
                hold.open(effective_price, "long", commission)
                stamp_open_from_label(_entry_stamp(row))
                sl_trigger_px = 0.0
                sl_high_water_px = mark_price
                # #1271: same arming rule as the short block above.
                sl_pierce_armed = False
                if (
                    self.stop_loss_atr_mult is not None
                    and self.stop_loss_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = avg_cost - self.stop_loss_atr_mult * entry_atr_value
                    sl_pierce_armed = True
                elif (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    sl_trigger_px = mark_price - self.trailing_stop_atr_mult * entry_atr_value
                elif self.stop_loss_pct is not None and self.stop_loss_pct > 0:
                    sl_trigger_px = avg_cost * (1 - self.stop_loss_pct)
                    sl_pierce_armed = True

            elif signal == -1 and position > 0:
                # SELL — close long position
                effective_price = fill_price * (1 - self.slippage_pct)
                proceeds = position * effective_price
                commission = proceeds * self.commission_pct
                # #980: additive — preserve any fractional-entry cash reserve
                # (identical when cash == 0, the full-notional case).
                cash += proceeds - commission
                position = 0.0

                if current_trade:
                    current_trade.close(idx, effective_price)
                    _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                                exit_fee=commission, reason="signal")
                    current_trade.scale_in_adds = scale.scale_in_count
                    trades.append(current_trade)
                    current_trade = None
                sl_trigger_px = 0.0
                avg_cost = 0.0
                entry_atr_value = 0.0
                sl_high_water_px = 0.0
                scale.reset()
                self._run_position_regime = ""

            # #1276: plain-path same-direction signal on an OPEN position —
            # a scale-in add when opted in and the gate passes; otherwise the
            # (unchanged) skip. Same guards rationale as the open/close-path
            # add branch (no cash>0 / regime gate — adds bypass both).
            elif (
                self.allow_scale_in
                and not plain_short_for_bar
                and signal == 1
                and position > 0
            ):
                _try_scale_in_add(i, "long", fill_price)
            elif (
                self.allow_scale_in
                and plain_short_for_bar
                and signal == -1
                and position < 0
            ):
                _try_scale_in_add(i, "short", fill_price)

            # #997: advance hold telemetry for a position held through this bar
            # (plain long/flat path). Same rationale as the open/close path:
            # runs after the open-fill BUY/SELL block, output-only.
            if position != 0:
                hold.step(
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                )

            # #1271 intra-bar OHLC-walk SL resolution — plain-path mirror of
            # the open/close-path block above: a range touch on the armed
            # trigger books the exit ON this bar at the trigger price (or at
            # the open on a gap-through), replacing the legacy close-detect /
            # next-open-fill pending mechanism.
            if (
                walk_mode
                and position != 0
                and sl_pierce_armed
                and sl_trigger_px > 0
            ):
                side_now = "long" if position > 0 else "short"
                raw_fill = self._intrabar_sl_fill(
                    side_now,
                    float(row["open"]) if has_open else mark_price,
                    float(row.get("high", mark_price) or mark_price),
                    float(row.get("low", mark_price) or mark_price),
                    sl_trigger_px,
                )
                if raw_fill is not None:
                    if position > 0:
                        effective_price = raw_fill * (1 - self.slippage_pct)
                        proceeds = position * effective_price
                        commission = proceeds * self.commission_pct
                        cash += proceeds - commission
                    else:
                        effective_price = raw_fill * (1 + self.slippage_pct)
                        cost = abs(position) * effective_price
                        commission = cost * self.commission_pct
                        cash -= cost + commission
                    position = 0.0
                    if current_trade:
                        current_trade.close(idx, effective_price)
                        _stamp_hold(current_trade, hold,
                                    entry_atr=entry_atr_value,
                                    exit_fee=commission, reason="signal_sl")
                        current_trade.scale_in_adds = scale.scale_in_count
                        trades.append(current_trade)
                        current_trade = None
                    sl_trigger_px = 0.0
                    avg_cost = 0.0
                    entry_atr_value = 0.0
                    sl_high_water_px = 0.0
                    sl_pierce_armed = False
                    scale.reset()
                    self._run_position_regime = ""

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
                # #1271: close-detect/next-open-fill is legacy-only — the
                # walk's pierce check above subsumes it (see the open/close
                # path's end-of-bar comment).
                if not walk_mode and self._sl_hit("long", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True
            elif position < 0 and sl_trigger_px > 0:
                # Short mirror (#989): the trail anchors on a LOW-water mark
                # (sl_high_water_px doubles as the favourable-extreme anchor,
                # matching _walk_trail's convention) and only ever tightens
                # the trigger DOWN; a close at/above the trigger fires.
                if (
                    self.trailing_stop_atr_mult is not None
                    and self.trailing_stop_atr_mult > 0
                    and entry_atr_value > 0
                ):
                    if mark_price < sl_high_water_px:
                        sl_high_water_px = mark_price
                    candidate = sl_high_water_px + self.trailing_stop_atr_mult * entry_atr_value
                    if candidate < sl_trigger_px:
                        sl_trigger_px = candidate
                if not walk_mode and self._sl_hit("short", mark_price, sl_trigger_px):
                    pending_signal_sl_close = True

            # #1271: promote any surviving trigger to a carried, pierce-
            # eligible level for the next bar (plain-path mirror of the
            # open/close path's pre-continue arming).
            if position != 0:
                sl_pierce_armed = True

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
                # This leg carries whatever shares remain after any earlier
                # engine partial-close legs (each of which already netted its
                # pro-rated share of the single entry commission). hold.entry_fee
                # is still the FULL entry fee, so pro-rate by the remaining
                # fraction here too — else the entry fee is netted more than
                # once across the position's legs (#1241).
                eod_qty_frac = (
                    current_trade.shares / initial_quantity
                    if initial_quantity > 0 else 1.0
                )
                _stamp_hold(current_trade, hold, entry_atr=entry_atr_value,
                            exit_fee=commission, reason="end_of_data",
                            qty_frac=eod_qty_frac)
                current_trade.scale_in_adds = scale.scale_in_count
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
            "total_funding_pnl": round(total_funding_pnl, 4),
            "params": open_ref.get("params") or params or {},
            "open_strategy": open_ref,
            "close_strategies": [dict(r) for r in self._close_refs],
            "trades": [t.to_dict() for t in trades],
        })
        if risk_mode:
            # #1268: surface the fail-closed skips so a run that took fewer
            # entries than the signal produced is auditable.
            metrics["risk_per_trade_pct"] = self.risk_per_trade_pct
            metrics["risk_sizing_skipped_entries"] = risk_skipped_entries
        if self.allow_scale_in:
            # #1276: add-leg telemetry — adds create no Trade rows (live #T
            # parity), so surface them here for auditability.
            metrics["scale_in_adds"] = scale_in_adds_total
            metrics["scale_in_added_notional_usd"] = round(
                scale_in_added_notional_total, 6,
            )

        if save:
            store_backtest_result(metrics)

        return metrics

    def _risk_entry_fraction(self, atr_series: Optional[pd.Series], idx,
                             price: float) -> Optional[float]:
        """Per-open committed-cash fraction under risk-per-trade sizing (#1268).

        Mirrors the live formula in this engine's fraction-of-cash terms:
        ``shares = (cash × pct/100) / stop_distance`` ⇒ ``invest = cash ×
        (pct/100) × price/stop_distance`` ⇒ fraction = ``(pct/100) ×
        price/stop_distance``. Stop distance comes from the configured owner:
        ``trailing_stop_atr_mult``/``stop_loss_atr_mult`` × the signal bar's
        closed ATR (``_stamp_entry_atr`` — the same value the SL geometry
        stamps, so a stop-out realizes exactly the risked dollars), else
        ``price × trailing_stop_pct``/``stop_loss_pct`` (fractions in this
        engine, matching the SL trigger math). Returns None when the distance
        is unresolvable — the caller SKIPS the entry (fail-closed, matching
        live). The fraction is capped at 1.0: the backtester models no
        leverage, so the live ``cash × exchange_leverage`` notional cap
        tightens to 1× cash here (warned once per run when it binds).
        """
        pct = float(self.risk_per_trade_pct or 0)
        if pct <= 0 or price <= 0:
            return None
        dist = None
        atr_mult = 0.0
        if (self.trailing_stop_atr_mult or 0) > 0:
            atr_mult = float(self.trailing_stop_atr_mult)
        elif (self.stop_loss_atr_mult or 0) > 0:
            atr_mult = float(self.stop_loss_atr_mult)
        if atr_mult > 0:
            atr = self._stamp_entry_atr(atr_series, idx, price)
            if atr <= 0:
                return None
            dist = atr_mult * atr
        elif (self.trailing_stop_pct or 0) > 0:
            dist = price * float(self.trailing_stop_pct)
        elif (self.stop_loss_pct or 0) > 0:
            dist = price * float(self.stop_loss_pct)
        if dist is None or dist <= 0:
            return None
        fraction = (pct / 100.0) * price / dist
        if fraction > 1.0:
            if not self._risk_cap_warned:
                self._risk_cap_warned = True
                print(
                    f"[#1268] risk_per_trade_pct: risk-derived notional "
                    f"exceeds available cash at {idx} (fraction "
                    f"{fraction:.2f} capped at 1.0) — the backtester models "
                    f"no leverage, so live would size up to cash × "
                    f"exchange_leverage here. Further caps applied silently.",
                    file=sys.stderr,
                )
            fraction = 1.0
        return fraction

    def _stamp_entry_atr(self, atr_series: Optional[pd.Series], idx,
                         entry_price: float) -> float:
        """Return the ATR to stamp as ``Position.EntryATR`` for a fill at
        ``idx``.

        Reads the bar BEFORE ``idx``: the fill happens at ``idx``'s open, when
        that bar's own high/low/close (and hence its ATR) are still unknown.
        Live stamps the last closed bar's ATR at order time (signal bar N; the
        fill bar is N+1), so the closed-bar value is bar N's — reading the
        fill bar's ATR leaked that bar's own range into the stop/TP geometry
        (#1228 audit). Mirrors ``stampEntryATRIfOpened`` in scheduler/main.go:
        rejects NaN and any value greater than 50% of the entry price as a
        plausibility guard. Returns 0.0 when no usable ATR is available —
        close evaluators that require ATR (``tiered_tp_atr``) then fall
        through with a no-op until a position with a valid ATR is opened.
        """
        if atr_series is None or entry_price <= 0:
            return 0.0
        try:
            pos = int(atr_series.index.get_loc(idx))
        except (KeyError, TypeError, ValueError):
            # Missing label, or a non-unique index where get_loc returns a
            # slice/mask — no unambiguous prior bar; treat as no usable ATR.
            return 0.0
        if pos < 1:
            return 0.0
        try:
            value = float(atr_series.iloc[pos - 1])
        except (TypeError, ValueError):
            return 0.0
        if not (value > 0):  # rejects NaN, 0, negative
            return 0.0
        if value > 0.5 * entry_price:
            return 0.0
        return value

    def _close_names_include_avwap_stop(self) -> bool:
        """True when ``avwap_stop`` is among the configured close names (#1196)."""
        from strategy_composition import close_names_include_avwap_stop
        return close_names_include_avwap_stop(self.close_strategies)

    def _evaluate_close_strategies(self, position: float, avg_cost: float,
                                   initial_quantity: float,
                                   entry_atr_value: float,
                                   mark_price: float,
                                   atr_series: Optional[pd.Series],
                                   idx,
                                   *,
                                   position_regime: str = "",
                                   market_regime: str = "",
                                   bars_held: int = 0,
                                   zscore_series: Optional[pd.Series] = None,
                                   avwap_series: Optional[pd.Series] = None
                                   ) -> Tuple[float, str]:
        """Run every configured close evaluator against the simulated position
        and return ``(max close_fraction, reason of the winning evaluator)``.
        Same max-wins resolution as the live composition flow in
        shared_tools/strategy_composition.py.
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
            # #997: holding-time context for time_stop. Closed-bar count since
            # the entry-fill bar inclusive (live check scripts don't pass this
            # yet — live wiring deferred; time_stop fails safe without it).
            "bars_held": int(bars_held),
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

        # #997: rolling z-score for zscore_target. Current-bar (closed) value,
        # same N-close -> N+1-open fill alignment as ATR above. NaN warmup
        # rows are omitted so the evaluator no-ops on them.
        if zscore_series is not None:
            try:
                z = float(zscore_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                z = float("nan")
            if z == z:  # not NaN
                market_dict["zscore"] = z

        # #1196: live re-anchored AVWAP for avwap_stop. Current-bar (closed)
        # value, same N-close -> N+1-open fill alignment as ATR above. NaN
        # warmup rows (pre-anchor) are omitted so the evaluator no-ops on them.
        if avwap_series is not None:
            try:
                avwap_value = float(avwap_series.loc[idx])
            except (KeyError, TypeError, ValueError):
                avwap_value = float("nan")
            if avwap_value == avwap_value and avwap_value > 0:
                market_dict["avwap"] = avwap_value

        best = 0.0
        best_reason = ""
        for name in self.close_strategies:
            params = self.close_params.get(name)
            result = evaluate(name, position_dict, market_dict, params)
            fraction = float(result.get("close_fraction", 0.0) or 0.0)
            if fraction > best:
                best = fraction
                best_reason = str(result.get("reason") or name)
                if best >= 1.0:
                    # Full close already wins — remaining evaluators can't change the outcome.
                    return 1.0, best_reason
        return min(max(best, 0.0), 1.0), best_reason

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
    def _intrabar_sl_fill(side: str, open_px: float, high_px: float,
                          low_px: float, trigger_px: float) -> Optional[float]:
        """Intra-bar OHLC-walk SL resolution (#1271): return the raw fill
        price for a bar whose range touched the armed trigger, or ``None``
        when the trigger survived the bar.

        Walks O first: a bar that OPENS beyond the trigger (gap-through)
        fills at the open — the trigger price no longer exists in the market.
        Otherwise a range touch (long: ``low <= trigger``; short: ``high >=
        trigger``) fills at the trigger price, mirroring an on-chain stop
        executing on touch. The caller applies slippage/commission on top,
        same as every other exit path. Frames without open/high/low columns
        pass the close for all three, degrading to the legacy close-touch
        detection rule — the open==close touch then routes through the
        gap-through branch and fills at the close (the only price the frame
        carries), one bar earlier than the legacy next-bar fill.
        """
        if trigger_px <= 0:
            return None
        if side == "long":
            if open_px > 0 and open_px <= trigger_px:
                return open_px
            if low_px > 0 and low_px <= trigger_px:
                return trigger_px
        elif side == "short":
            if open_px >= trigger_px:
                return open_px
            if high_px >= trigger_px:
                return trigger_px
        return None

    @staticmethod
    def _sl_hit(side: str, mark_price: float, trigger_px: float) -> bool:
        """Bar-close SL hit detection — the LEGACY (``bar_close``) resolution.
        For a long, fires when ``mark_price <= trigger_px``; for a short, when
        ``mark_price >= trigger_px``. Under the default ``ohlc_walk`` mode the
        run loop uses ``_intrabar_sl_fill`` instead (which strictly subsumes
        this check); this remains for the legacy flag and direct unit tests.
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

        # Liquidation floor (#1005): a stop-less short losing >100% drives
        # equity negative, and pct_change over a negative base inverts return
        # signs (a deepening blowup reads as a positive return, a recovery as
        # negative), corrupting Sharpe/Sortino/volatility. A real account is
        # dead at zero — floor the curve at 0 from the first bust bar onward
        # (sticky: no resurrection if the position later recovers) and flag
        # the run so harness consumers (eval_windows, fee_audit) can surface
        # it. Post-bust bars contribute 0/0 = NaN returns, dropped below.
        liquidated = bool((equity <= 0).any())
        if liquidated:
            bust_pos = int(np.argmax(equity.values <= 0))
            equity = equity.copy()
            equity.iloc[bust_pos:] = 0.0

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

        # Sortino Ratio — canonical downside deviation: the root-mean-square of
        # min(r, 0) about a MAR of 0 (NOT the sample std of negatives about their
        # own mean), annualized by the same factor as Sharpe. The RMS is taken
        # over ALL return observations, so a single losing bar still yields a
        # finite, well-defined denominator. Zero downside (no losing bars, or a
        # downside deviation that rounds to 0) means Sortino is mathematically
        # undefined — report None, never a neutral 0.0 that would rank a
        # flawless leg below a mediocre one.
        if len(daily_returns) > 0:
            neg = daily_returns.clip(upper=0.0)
            downside_dev = float(np.sqrt((neg**2).mean()))
        else:
            downside_dev = 0.0
        if downside_dev > 0:
            sortino = (daily_returns.mean() / downside_dev) * ann_factor
        else:
            sortino = None

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
            # All-win legs (no losing trades → gross_loss == 0) have an
            # undefined profit factor. Report None, never float("inf"): a NaN/inf
            # would serialize as the nonstandard JSON `Infinity` token that strict
            # parsers (incl. Go json.Unmarshal) reject.
            profit_factor = gross_profit / gross_loss if gross_loss > 0 else None

            # Net return fraction per leg (net t.pnl over entry notional), so
            # avg_win/avg_loss share the net convention with the win/loss
            # buckets above — t.pnl_pct is GROSS and would misreport a
            # gross-winner/net-loser bucketed as a loss (#1241).
            def _net_pnl_pct(t):
                notional = t.shares * t.entry_price
                return (t.pnl / notional) if notional > 0 else 0.0
            avg_win = np.mean([_net_pnl_pct(t) for t in winning]) if winning else 0
            avg_loss = np.mean([_net_pnl_pct(t) for t in losing]) if losing else 0
        else:
            win_rate = 0
            profit_factor = 0
            avg_win = 0
            avg_loss = 0

        # Volatility (annualized) — same timeframe-aware factor as Sharpe.
        volatility = daily_returns.std() * ann_factor if len(daily_returns) > 1 else 0

        # Calmar ratio
        calmar = annual_return / abs(max_drawdown) if max_drawdown != 0 else 0

        # Liquidation risk-adjusted floor (#1005): when the sticky floor leaves
        # <2 surviving returns (a leg busting within 1-2 bars — the post-bust
        # NaN tail drops out), the variance guards above collapse Sharpe/
        # Sortino/volatility to a NEUTRAL 0.0. That reads a dead account as
        # "fine" and ranks a fast blowup ABOVE a slow one — re-inverting the
        # exact axis this issue fixed. Floor every blown leg to a fixed sentinel
        # so all deaths tie below any surviving leg, mirroring the −100% floor
        # already applied to return/DD. The sentinel is timeframe-INDEPENDENT
        # (not -ann_factor) so two equally-dead legs tie regardless of bust
        # timeframe, and not path-dependent so an earlier bust never out-ranks
        # a later one.
        # Liquidation forces the sentinel floor regardless of the values above
        # — a busted account is NOT "zero downside"; the None-Sortino convention
        # never applies here (#1005 override wins).
        if liquidated:
            sharpe = sortino = -LIQUIDATED_METRIC_FLOOR
            volatility = LIQUIDATED_METRIC_FLOOR

        return {
            "total_return_pct": round(total_return * 100, 2),
            "annual_return_pct": round(annual_return * 100, 2),
            "sharpe_ratio": round(sharpe, 3),
            "sortino_ratio": round(sortino, 3) if sortino is not None else None,
            "max_drawdown_pct": round(max_drawdown * 100, 2),
            "calmar_ratio": round(calmar, 3),
            "volatility_pct": round(volatility * 100, 2),
            "win_rate": round(win_rate * 100, 2),
            "profit_factor": round(profit_factor, 3) if profit_factor is not None else None,
            "total_trades": total_trades,
            "avg_win_pct": round(avg_win * 100, 2),
            "avg_loss_pct": round(avg_loss * 100, 2),
            "liquidated": liquidated,
        }


def _fmt_opt(value, spec: str = ".3f", none_text: str = "n/a") -> str:
    """Format an optional numeric metric, rendering None (undefined) as text
    instead of raising on a format spec that only accepts numbers."""
    if value is None:
        return none_text
    return format(value, spec)


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
    ]
    if results.get("liquidated"):
        lines.append(
            "  *** LIQUIDATED: equity hit 0 — metrics floored at the bust bar ***"
        )
    lines += [
        f"{'─'*60}",
        f"  RETURNS",
        f"    Total Return:    {results['total_return_pct']:+.2f}%",
        f"    Annual Return:   {results['annual_return_pct']:+.2f}%",
        f"    Volatility:      {results.get('volatility_pct', 0):.2f}%",
        f"{'─'*60}",
        f"  RISK METRICS",
        f"    Sharpe Ratio:    {results['sharpe_ratio']:.3f}",
        f"    Sortino Ratio:   {_fmt_opt(results['sortino_ratio'])}",
        f"    Max Drawdown:    {results['max_drawdown_pct']:.2f}%",
        f"    Calmar Ratio:    {results.get('calmar_ratio', 0):.3f}",
        f"{'─'*60}",
        f"  TRADE STATS",
        f"    Total Trades:    {results['total_trades']}",
        f"    Win Rate:        {results['win_rate']:.1f}%",
        f"    Profit Factor:   {_fmt_opt(results['profit_factor'])}",
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
