"""Backtest-vs-live parity diff tool (#906 D7.4).

The parity contract between the backtester and the live scheduler is:
same ``compute_*`` function, same window, same params → same decision.
The backtester evaluates a strategy ONCE over the full vectorized frame
and reads decisions row-by-row; the live check scripts re-evaluate the
strategy every cycle over a trailing fetch window (``--ohlcv-limit``,
default 200 in ``shared_scripts/check_strategy.py``) and act on the LAST
closed bar. Any strategy whose bar-N output depends on the frame it was
computed in — full-series normalization, unseeded rolling state, warmup
that never converges — silently diverges between the two paths, and no
existing test catches it because both suites only exercise one path.

This tool replays both paths over the same candles and emits a per-bar
diff of the decision surface:

  • ``signal``          — vectorized value at bar N  vs  the decision a
                          check-script run over a window ENDING at bar N
                          would emit (live semantics — see below).
  • ``open_action``     — backtester semantics (the ``open_action`` column
                          when the strategy emits it, else derived from
                          ``signal``)  vs  live semantics (always derived
                          from the composed signal, exactly as
                          ``finalize_decision`` does). A strategy whose
                          column disagrees with ``sign(signal)`` is a real
                          parity break and is flagged here.
  • ``close_fraction``  — max across ``close_fraction*`` columns AND any
                          configured close-strategy refs, same comparison.
                          Registry close evaluators (``tiered_tp_atr`` …)
                          are invoked through the SAME
                          ``close_registry_loader.evaluate`` on both
                          sides; the backtest side feeds bar-N close +
                          full-frame closed-bar ATR + full-frame regime
                          (engine semantics), the live side feeds
                          ``latest_atr``/``prepare_check_regime`` on the
                          trailing window (check-script semantics) — so a
                          mismatch isolates window-dependent evaluator
                          inputs.
  • ``regime``          — full-frame ``compute_regime`` label at bar N vs
                          the ``prepare_check_regime`` label on the
                          trailing window (the per-bar generalization of
                          the last-bar parity test in
                          ``test_backtester_regime.py``).

The live side is not a re-implementation: it calls the same helpers the
live check script calls — ``prepare_check_regime`` → ``params["regime"]``
injection → ``evaluate_open_close`` / ``finalize_decision`` with
``close_registry_loader.evaluate`` — so the replay IS check-script
semantics, not a model of them. (The HTF filter is the one deliberate
omission: it needs a second-timeframe fetch and is orthogonal to
frame-dependence.)

Non-registry (signal-strategy) close refs are rejected upfront with the
engine's own error: ``Backtester`` refuses unknown close names at init,
so there is no backtest path to diff against. The live composition's
signal-strategy close fallback (``legacy_close_fraction_from_signal``)
is a live-only surface; comparing it against a silent bt-side zero
would manufacture mismatches, so the tool fails loudly instead.

When close refs are configured, both sides share a single simulated
position lifecycle (side / avg_cost / quantities / entry values),
seeded from the backtest-effective open/close decisions — shared so the
position itself can never be the source of a diff. The bt-side registry
evaluator's fraction folds back into the next bar's quantity (the
engine's ``pending_close_fraction``), so cumulative tier ladders advance
exactly as the engine books them and each compared fraction is the
per-bar increment, not a repeated cumulative value. The dict shape each
evaluator sees mirrors its real caller: the backtest side always passes
``regime`` (possibly empty) and ``entry_atr``, exactly like
``Backtester._evaluate_close_strategies`` (#747); the live side omits
empty keys, exactly like the check scripts. The asymmetry is deliberate
— a diff at empty-regime bars surfaces that genuine engine-vs-live
shape divergence instead of masking it.

Every row also carries ``backtest_effective_*`` columns — the post-
``shift(1)`` values the engine actually reads at bar N — informational
only, never part of the match.

Usage (ad-hoc):
  uv run --no-sync python backtest/parity_diff.py \
      --strategy supertrend --symbol BTC/USDT --timeframe 1h \
      [--since 2024-01-01] [--params '{"period": 10}'] \
      [--registry spot|futures] [--window 200] [--stride 1] \
      [--close tiered_tp_atr] [--close 'name:{"param": 1}'] \
      [--regime] [--fills] [--csv /tmp/diff.csv] [--jsonl /tmp/diff.jsonl]

Usage (exact live config, #641 loader):
  uv run --no-sync python backtest/parity_diff.py \
      --config scheduler/config.json --strategy-id hl-supertrend-btc \
      [--since 2024-01-01] [--fills]

``--fills`` additionally runs the full ``Backtester`` over the same
candles and reports the simulated entry/exit fills (price + modeled fee)
so a decision-level diff can be lined up against the trades it produced.

Exit code 0 when the paths agree on every compared bar, 1 when any bar
differs (CI-friendly), 2 on usage/data errors. A run that compares zero
bars (data shorter than the trailing window, or --since/--stride
leaving nothing to compare) is a data error — a vacuous comparison is
not agreement.

The per-bar loop re-runs the strategy O(N) times on trailing windows —
this is a debugging tool, not a benchmark; bound the range with --since
or thin the comparison with --stride for long histories.
"""

import argparse
import inspect
import json
import os
import sys
from dataclasses import dataclass, field
from typing import Optional

import pandas as pd

sys.path.insert(0, os.path.join(os.path.dirname(__file__), '..', 'shared_tools'))
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
# Repo root: post_tp_sl.py (loaded lazily by Backtester) imports via the
# `shared_strategies.` package path, which is only resolvable from the root.
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))

from atr import ensure_atr_indicator, latest_atr
from regime import prepare_check_regime
from registry_loader import load_registry
from strategy_composition import (
    compose_signal,
    evaluate_open_close,
    finalize_decision,
    open_action_from_signal,
    rewrite_deprecated_close_ref,
)
from close_registry_loader import (
    evaluate as close_evaluate,
    list_strategies as close_list_strategies,
)
from backtester import (
    Backtester,
    _apply_direction_invert_value,
    _max_close_fraction_series,
    _normalize_open_action,
    _normalize_regime_directional_policy,
    _open_action_from_signal,
    _resolve_regime_directional_entry,
    _signal_from_open_action,
    fee_pct_for_platform,
)

# Mirror live: check_strategy.py refuses to evaluate fewer than 30 candles.
LIVE_MIN_CANDLES = 30
# Engine default slippage, read from the Backtester signature so the
# scaffold's effective_price math can never drift out of sync with it.
_ENGINE_SLIPPAGE_PCT = float(
    inspect.signature(Backtester.__init__).parameters["slippage_pct"].default
)
# Mirror live: check scripts fetch --ohlcv-limit candles (default 200).
DEFAULT_WINDOW = 200


@dataclass
class ParityConfig:
    """Everything both replay paths need to evaluate one strategy."""
    strategy_name: str
    params: dict = field(default_factory=dict)
    registry: str = "spot"
    platform: str = "binanceus"
    symbol: str = "BTC/USDT"
    timeframe: str = "1h"
    # Close refs as ``[{"name": ..., "params": {...}}]`` — single-element in
    # practice since #842; the list shape matches the Backtester kwarg.
    close_refs: Optional[list] = None
    regime_enabled: bool = False
    regime_period: int = 14
    regime_adx_threshold: float = 20.0
    direction: Optional[str] = None
    invert_signal: bool = False
    regime_directional_policy: Optional[dict] = None
    # #1085: the certified PER-STATE direction map (None when uncertified). Fed to
    # the Backtester so it applies the SAME per-state sign gate the live daemon
    # does — a state whose config contradicts the certified sign resolves to base.
    regime_directional_certified_states: Optional[dict] = None

    def __post_init__(self):
        self.regime_directional_policy = _normalize_regime_directional_policy(
            self.regime_directional_policy,
        )


def config_from_live_config(config_path: str, strategy_id: str,
                            platform: str = "") -> ParityConfig:
    """Build a ParityConfig from a live go-trader config (#641 loader).

    Reuses ``run_backtest.load_strategy_config`` for the open/close refs
    (so the same v13+ gate, #842 single-close collapse, and HL-live-only
    rejections apply), then reads the raw strategy entry for the symbol /
    timeframe / registry / regime settings the loader doesn't return.
    """
    from run_backtest import load_strategy_config
    # #1228: live applies user_defaults.{close,regime_atr,manual}
    # unconditionally in loadConfig, so a live-parity replay must inject them
    # too — without this the diff could report CLEAN against effective params
    # live never runs.
    loaded = load_strategy_config(config_path, strategy_id,
                                  inject_user_defaults=True)
    with open(config_path) as fh:
        raw = json.load(fh)
    entry = next(
        (sc for sc in raw.get("strategies", []) or []
         if sc.get("id") == strategy_id),
        {},
    )
    args = entry.get("args") or []
    regime = raw.get("regime") or entry.get("regime") or {}
    open_ref = loaded["open_strategy"]
    stype = str(entry.get("type", "spot"))
    symbol = str(args[1]) if len(args) > 1 else "BTC/USDT"
    timeframe = str(args[2]) if len(args) > 2 else "1h"
    # #1085: apply the same evidence gate the live daemon and backtester do. We
    # resolve the certified PER-STATE direction map (not just a cell-level bool)
    # and hand it to the Backtester, which drops each state whose configured side
    # contradicts the certified sign (or is uncertified) to base direction — so
    # parity_diff reflects the gated per-state runtime, not the ungated config.
    rdp = loaded.get("regime_directional_policy")
    rdp_cert_states = None
    if rdp:
        from directional_certification import (
            load_certifications, certified_states,
            config_directional_classifier,
        )
        certs = load_certifications()
        # Resolve the directional window's classifier exactly as live (#1085).
        clf = config_directional_classifier(regime, entry)
        rdp_cert_states = certified_states(certs, symbol, timeframe, clf)
    return ParityConfig(
        strategy_name=open_ref["name"],
        params=dict(open_ref.get("params") or {}),
        registry="futures" if stype in ("perps", "futures", "manual") else "spot",
        platform=platform or ("hyperliquid" if stype in ("perps", "manual")
                              else "binanceus"),
        symbol=symbol,
        timeframe=timeframe,
        close_refs=loaded.get("close_strategies") or None,
        regime_enabled=bool(loaded.get("regime_enabled", regime.get("enabled"))),
        regime_period=int(loaded.get("regime_period", regime.get("period", 14)) or 14),
        regime_adx_threshold=float(
            loaded.get("regime_adx_threshold", regime.get("adx_threshold", 20.0)) or 20.0
        ),
        direction=loaded.get("direction"),
        invert_signal=bool(loaded.get("invert_signal")),
        regime_directional_policy=rdp,
        regime_directional_certified_states=rdp_cert_states,
    )


def _normalize_signal(value) -> int:
    """Collapse a raw signal to {-1, 0, 1}, mirroring the engine's contract.

    NaN → 0 (the engine ``fillna(0)``s); any other value outside
    {-1, 0, 1} is rejected the way ``Backtester.run`` rejects it. On such
    a signal the engine raises while the live check script's
    ``normalize_signal`` coerces (``int(0.5)`` → 0) — a real divergence.
    Normalizing it identically on both tool paths would report CLEAN for
    a value neither real path produces, so the tool surfaces the contract
    violation loudly instead.
    """
    try:
        f = float(value)
    except (TypeError, ValueError):
        return 0
    if pd.isna(f):
        return 0
    if f not in (-1.0, 0.0, 1.0):
        raise ValueError(
            f"signal must be in {{-1, 0, 1}} — got {value!r}. The engine "
            f"rejects this signal (Backtester.run) while the live check "
            f"script would coerce it; fix the strategy rather than relying "
            f"on either behavior."
        )
    return int(f)


def _close_names(close_refs: Optional[list]) -> list:
    return [str(r.get("name", "")).strip() for r in (close_refs or [])
            if r.get("name")]


def _effective_directional_pair(cfg: ParityConfig, current_regime: str,
                                position_ctx: Optional[dict]) -> tuple[str, bool]:
    direction = cfg.direction or ""
    invert = bool(cfg.invert_signal)
    entry = _resolve_regime_directional_entry(
        cfg.regime_directional_policy,
        current_regime,
        str((position_ctx or {}).get("regime", "") or ""),
        float((position_ctx or {}).get("current_quantity", 0.0) or 0.0),
    )
    if entry is not None:
        direction = str(entry["direction"])
        invert = bool(entry["invert_signal"])
    return direction, invert


def _transform_entry_signal(signal: int, cfg: ParityConfig,
                            current_regime: str,
                            position_ctx: Optional[dict],
                            *,
                            uses_open_close: bool) -> int:
    direction, invert = _effective_directional_pair(
        cfg, current_regime, position_ctx,
    )
    return _apply_direction_invert_value(
        int(signal),
        uses_open_close=uses_open_close,
        direction=direction,
        invert_signal=invert,
    )


def _close_params_by_name(close_refs: Optional[list]) -> dict:
    return {str(r["name"]).strip(): dict(r.get("params") or {})
            for r in (close_refs or []) if r.get("name")}


def _has_close_fraction_columns(result_df: pd.DataFrame) -> bool:
    return any(c == "close_fraction" or str(c).startswith("close_fraction:")
               for c in result_df.columns)


def _full_frame_decisions(result_df: pd.DataFrame) -> pd.DataFrame:
    """Extract the backtest-path decision surface for every bar.

    ``open_action`` mirrors the engine's normalization: prefer the column
    when the strategy emits it, else derive from the signal — the same
    branch ``Backtester.run`` takes before shifting.
    """
    out = pd.DataFrame(index=result_df.index)
    out["signal"] = result_df.get(
        "signal", pd.Series(0, index=result_df.index)
    ).map(_normalize_signal)
    if "open_action" in result_df.columns:
        out["open_action"] = result_df["open_action"].map(_normalize_open_action)
    else:
        out["open_action"] = out["signal"].map(_open_action_from_signal)
    if _has_close_fraction_columns(result_df):
        out["close_fraction"] = _max_close_fraction_series(result_df)
    else:
        out["close_fraction"] = 0.0
    return out


def _live_bar_decision(window: pd.DataFrame, cfg: ParityConfig, reg,
                       position_side: str = "",
                       position_ctx: Optional[dict] = None) -> dict:
    """Run the check-script decision path on a window ending at the bar.

    This is ``shared_scripts/check_strategy.py``'s flow with the fetch and
    JSON plumbing removed: ``prepare_check_regime`` → ``params["regime"]``
    injection → either ``evaluate_open_close``+``finalize_decision`` (when
    close refs are configured) or a plain last-bar signal read.
    """
    _stdout_regime, live_regime, strategy_regime = prepare_check_regime(
        window,
        regime_enabled=cfg.regime_enabled,
        period=cfg.regime_period,
        adx_threshold=cfg.regime_adx_threshold,
    )
    params = dict(cfg.params or {})
    # check_strategy.py injects the regime snapshot unconditionally;
    # apply_strategy strips it for strategies that don't declare it (#720).
    params["regime"] = strategy_regime

    close_names = _close_names(cfg.close_refs)
    decision = {"regime": str(live_regime or "")}
    if close_names:
        market_ctx = {"mark_price": float(window["close"].iloc[-1])}
        atr_now = latest_atr(window)
        if atr_now > 0:
            market_ctx["atr"] = atr_now
        if live_regime:
            market_ctx["regime"] = live_regime
        evaluation = evaluate_open_close(
            reg.apply_strategy,
            reg.get_strategy,
            window,
            cfg.strategy_name,
            None,
            close_names,
            position_side,
            params,
            position_ctx or {},
            close_evaluate=close_evaluate,
            market_ctx=market_ctx,
            close_params_by_name=_close_params_by_name(cfg.close_refs),
        )
        open_signal = _transform_entry_signal(
            int(evaluation.open_signal),
            cfg,
            str(live_regime or ""),
            position_ctx,
            uses_open_close=True,
        )
        final = finalize_decision(evaluation, position_side, open_signal)
        decision["signal"] = int(final["signal"])
        decision["open_action"] = str(final["open_action"])
        decision["close_fraction"] = float(final["close_fraction"])
        # Column-emitting closes on the open result still count, exactly as
        # the engine reads them — max-wins against the evaluator output.
        if _has_close_fraction_columns(evaluation.open_result_df):
            decision["close_fraction"] = max(
                decision["close_fraction"],
                float(_max_close_fraction_series(
                    evaluation.open_result_df).iloc[-1]),
            )
        return decision

    result_df = reg.apply_strategy(cfg.strategy_name, window, params)
    last = result_df.iloc[-1]
    # Same strict normalizer as the backtest side: in-contract signals
    # collapse identically, out-of-contract signals raise (see
    # _normalize_signal) instead of being coerced into a false CLEAN.
    decision["signal"] = _transform_entry_signal(
        _normalize_signal(last.get("signal", 0)),
        cfg,
        str(live_regime or ""),
        position_ctx,
        uses_open_close=False,
    )
    decision["open_action"] = open_action_from_signal(decision["signal"])
    if _has_close_fraction_columns(result_df):
        decision["close_fraction"] = float(
            _max_close_fraction_series(result_df).iloc[-1])
    else:
        decision["close_fraction"] = 0.0
    return decision


def _bt_close_evaluator_fraction(cfg: ParityConfig, i: int,
                                 df: pd.DataFrame, atr_full: pd.Series,
                                 regime_full: Optional[pd.Series],
                                 position_ctx: Optional[dict]) -> float:
    """Backtest-side registry close evaluation at bar ``i``.

    Mirrors ``Backtester``'s close-evaluator inputs: bar-N close as the
    mark, full-frame closed-bar ATR at bar N, full-frame regime label at
    bar N. Same ``close_registry_loader.evaluate``, same position
    lifecycle as the live side — but the dict SHAPE mirrors the engine
    (#747): ``regime`` always present (possibly empty) in both dicts,
    ``entry_atr`` always a float. The live side omits empty keys like
    the check scripts do; the asymmetry deliberately lets the tool
    surface a genuine engine-vs-live shape divergence at empty-regime
    bars instead of masking it.
    """
    if not position_ctx:
        return 0.0
    position = {
        "side": str(position_ctx.get("side", "")),
        "avg_cost": float(position_ctx.get("avg_cost", 0.0) or 0.0),
        "current_quantity": float(
            position_ctx.get("current_quantity", 0.0) or 0.0),
        "initial_quantity": float(
            position_ctx.get("initial_quantity", 0.0) or 0.0),
        "entry_atr": float(position_ctx.get("entry_atr", 0.0) or 0.0),
        "regime": str(position_ctx.get("regime", "") or ""),
    }
    label = ""
    if regime_full is not None:
        raw_label = regime_full.iloc[i]
        label = "" if pd.isna(raw_label) else str(raw_label)
    market = {"mark_price": float(df["close"].iloc[i]), "regime": label}
    atr_val = float(atr_full.iloc[i]) if pd.notna(atr_full.iloc[i]) else 0.0
    if atr_val > 0:
        market["atr"] = atr_val
    params_by_name = _close_params_by_name(cfg.close_refs)
    best = 0.0
    for name in _close_names(cfg.close_refs):
        resolved, ref_params = rewrite_deprecated_close_ref(
            name, params_by_name.get(name))
        if ref_params is None and resolved == cfg.strategy_name:
            ref_params = dict(cfg.params or {})
        # Unknown names were rejected upfront (compute_parity_frame), so any
        # ValueError here is a genuine evaluator error — propagate, exactly
        # as the engine would.
        result = close_evaluate(resolved, position, market, ref_params)
        best = max(best, float(result.get("close_fraction", 0.0) or 0.0))
    return best


def _simulate_position_contexts(bt: pd.DataFrame, df: pd.DataFrame,
                                atr_full: pd.Series,
                                regime_full: Optional[pd.Series],
                                cfg: Optional[ParityConfig] = None,
                                *,
                                return_decisions: bool = False) -> tuple:
    """Walk the backtest-effective decisions and track a scaffold position.

    The context exists so close evaluators see a plausible position on
    BOTH sides — it is shared, so it can never be the source of a diff.
    Entries/exits follow the engine's shift(1) semantics: bar N acts on
    bar N-1's signal/open_action/close_fraction. ``contexts[i]`` is the
    position AFTER bar i's open fills — the state the engine holds during
    its end-of-bar close evaluation, and the position live's bar-i cycle
    sees.

    Registry close-evaluator output is folded back into the quantity the
    NEXT bar — the engine's ``pending_close_fraction`` — so the cumulative
    tier ladder advances exactly as the engine books it: 0.4 once, then 0,
    then the increment to 0.8 at the next rung. Without the fold,
    ``current_quantity`` would stay pinned at ``initial_quantity`` and
    every post-tier-1 bar would repeat the cumulative fraction — a value
    neither the engine nor live produces.

    Returns ``(contexts, registry_fractions)`` by default. With
    ``return_decisions=True`` also returns the transformed decision frame.
    ``registry_fractions[i]``
    is the bt-side evaluator fraction at bar i computed with that shared
    context — the same value ``compute_parity_frame`` compares, so the
    scaffold's accounting and the comparison can never disagree.
    """
    decisions = bt.copy()
    contexts = []
    registry_fractions = []
    side = ""
    avg_cost = 0.0
    qty = 0.0
    initial_qty = 0.0
    entry_atr = 0.0
    entry_regime = ""

    for i in range(len(df)):
        if i > 0:
            # Backtest-effective inputs at bar i = unshifted bar i-1 values;
            # the registry-evaluator fraction from bar i-1 fills here too,
            # exactly like the engine's pending_close_fraction (max-wins
            # against the column path, same resolution as the comparison).
            eff_signal = int(decisions["signal"].iloc[i - 1])
            eff_action = str(decisions["open_action"].iloc[i - 1])
            eff_close = max(float(decisions["close_fraction"].iloc[i - 1]),
                            registry_fractions[i - 1])
            mark = float(df["close"].iloc[i])

            if side and (eff_close > 0
                         or (side == "long" and eff_signal < 0)
                         or (side == "short" and eff_signal > 0)):
                frac = eff_close if eff_close > 0 else 1.0
                # Engine booking: qty_to_close = abs(position) * fraction —
                # the fraction applies to the CURRENT position, not the
                # initial size (Backtester.run's close-fraction fill).
                qty = max(qty - qty * min(frac, 1.0), 0.0)
                if qty <= 1e-12:
                    side = ""
                    avg_cost = qty = initial_qty = entry_atr = 0.0
                    entry_regime = ""
            elif not side and eff_action in ("long", "short"):
                side = eff_action
                # Engine fill semantics: an open fills at the fill bar's
                # OPEN (fallback close), adjusted by the engine's default
                # slippage — ``effective_price`` in Backtester.run, not the
                # bar's close.
                fill_price = (float(df["open"].iloc[i])
                              if "open" in df.columns else mark)
                if side == "long":
                    avg_cost = fill_price * (1 + _ENGINE_SLIPPAGE_PCT)
                else:
                    avg_cost = fill_price * (1 - _ENGINE_SLIPPAGE_PCT)
                qty = initial_qty = 1.0
                atr_val = atr_full.iloc[i]
                entry_atr = float(atr_val) if pd.notna(atr_val) else 0.0
                # Engine plausibility guard (_stamp_entry_atr, mirroring
                # Go's stampEntryATRIfOpened): non-positive or > 50% of the
                # entry price (effective_price, same as the engine) stamps
                # 0.0, so ATR-requiring close evaluators no-op.
                if not (0.0 < entry_atr <= 0.5 * avg_cost):
                    entry_atr = 0.0
                if regime_full is not None:
                    # Engine semantics: the position regime is stamped from
                    # the SHIFTED regime column at the fill row — the
                    # decision bar's (i-1) label, which is also what live
                    # stamps at open (the label computed alongside the
                    # signal). The fill bar's own label (i) would match
                    # neither side.
                    raw_label = regime_full.iloc[i - 1]
                    entry_regime = "" if pd.isna(raw_label) else str(raw_label)

        ctx = None
        if side:
            ctx = {
                "side": side,
                "avg_cost": avg_cost,
                "current_quantity": qty,
                "initial_quantity": initial_qty or qty,
            }
            if entry_atr > 0:
                ctx["entry_atr"] = entry_atr
            if entry_regime:
                ctx["regime"] = entry_regime
        if cfg is not None:
            label = ""
            if regime_full is not None:
                raw_label = regime_full.iloc[i]
                label = "" if pd.isna(raw_label) else str(raw_label)
            if _close_names(cfg.close_refs):
                transformed = _transform_entry_signal(
                    _signal_from_open_action(bt["open_action"].iloc[i]),
                    cfg,
                    label,
                    ctx,
                    uses_open_close=True,
                )
            else:
                transformed = _transform_entry_signal(
                    int(bt["signal"].iloc[i]),
                    cfg,
                    label,
                    ctx,
                    uses_open_close=False,
                )
            decisions.iloc[i, decisions.columns.get_loc("signal")] = transformed
            decisions.iloc[i, decisions.columns.get_loc("open_action")] = (
                _open_action_from_signal(transformed)
            )
        contexts.append(ctx)
        registry_fractions.append(
            _bt_close_evaluator_fraction(cfg, i, df, atr_full,
                                         regime_full, ctx)
            if (cfg is not None and ctx is not None) else 0.0
        )
    if return_decisions:
        return contexts, registry_fractions, decisions
    return contexts, registry_fractions


def compute_parity_frame(
    df: pd.DataFrame,
    strategy_name: Optional[str] = None,
    params: Optional[dict] = None,
    registry: str = "spot",
    window: Optional[int] = DEFAULT_WINDOW,
    stride: int = 1,
    regime_enabled: bool = False,
    regime_period: int = 14,
    regime_adx_threshold: float = 20.0,
    close_refs: Optional[list] = None,
    cfg: Optional[ParityConfig] = None,
) -> pd.DataFrame:
    """Replay both decision paths over ``df`` and return the per-bar diff.

    Returns one row per compared bar with ``bt_*`` (vectorized full-frame /
    engine semantics) and ``live_*`` (trailing-window check-script
    semantics) columns, ``backtest_effective_*`` (post-shift(1) engine
    inputs, informational) and a ``match`` bool. Comparison starts at the
    first bar where the trailing window is full (``window`` bars, or
    ``LIVE_MIN_CANDLES`` for expanding mode) so every live evaluation sees
    the same window length it would in production — earlier bars would
    diff on warmup, not on parity.
    """
    if cfg is None:
        if not strategy_name:
            raise ValueError("strategy_name or cfg is required")
        cfg = ParityConfig(
            strategy_name=strategy_name,
            params=dict(params or {}),
            registry=registry,
            close_refs=close_refs,
            regime_enabled=regime_enabled,
            regime_period=regime_period,
            regime_adx_threshold=regime_adx_threshold,
        )
    if stride < 1:
        raise ValueError("stride must be >= 1")
    if window is not None and window < LIVE_MIN_CANDLES:
        raise ValueError(f"window must be >= {LIVE_MIN_CANDLES} (live minimum)")
    reg = load_registry(cfg.registry)
    full_result = reg.apply_strategy(
        cfg.strategy_name, df.copy(), dict(cfg.params or {}))
    bt = _full_frame_decisions(full_result)
    atr_full = ensure_atr_indicator(df.copy())["atr"]

    regime_full = None
    if cfg.regime_enabled:
        from regime import compute_regime
        regime_full = compute_regime(
            df, period=cfg.regime_period,
            adx_threshold=cfg.regime_adx_threshold,
        )["regime"]

    has_close_refs = bool(_close_names(cfg.close_refs))
    if has_close_refs:
        # Mirror the engine: Backtester rejects non-registry close names at
        # init, so a signal-strategy close ref has no backtest path to diff
        # — fail with the same error instead of silently contributing 0 on
        # the bt side while the live fallback fully evaluates it.
        available = set(close_list_strategies())
        for name in _close_names(cfg.close_refs):
            resolved, _ = rewrite_deprecated_close_ref(name, None)
            if resolved not in available:
                raise ValueError(
                    f"Unknown close strategy: {resolved}. The backtester "
                    f"rejects non-registry close refs at init, so there is "
                    f"no engine path to diff; the live signal-strategy close "
                    f"fallback is live-only. Available: {sorted(available)}"
                )
    needs_decision_walk = (
        has_close_refs
        or bool(cfg.direction)
        or bool(cfg.invert_signal)
        or cfg.regime_directional_policy is not None
    )
    if needs_decision_walk:
        contexts, registry_fracs, bt = _simulate_position_contexts(
            bt, df, atr_full, regime_full, cfg, return_decisions=True)
    else:
        contexts, registry_fracs = [None] * len(df), [0.0] * len(df)

    start = (window - 1) if window is not None else (LIVE_MIN_CANDLES - 1)
    rows = []
    for i in range(start, len(df), stride):
        lo = max(0, i + 1 - window) if window is not None else 0
        win = df.iloc[lo:i + 1].copy()
        ctx = contexts[i]
        side = str((ctx or {}).get("side", "") or "")
        live = _live_bar_decision(win, cfg, reg,
                                  position_side=side, position_ctx=ctx)

        bt_close = float(bt["close_fraction"].iloc[i])
        bt_signal = int(bt["signal"].iloc[i])
        if has_close_refs:
            # The scaffold already evaluated the bt-side registry close at
            # bar i with this exact context — reuse it so the fraction
            # compared here is the same one the ladder accounting applied.
            bt_close = max(bt_close, registry_fracs[i])
            # The live decision composes open intent + close intent +
            # position side into one signal (finalize_decision). Compose
            # the backtest inputs identically so the signal column compares
            # like-for-like — a diff then isolates input drift, never the
            # composition itself.
            bt_signal = compose_signal(
                str(bt["open_action"].iloc[i]), bt_close, side)

        row = {
            "ts": df.index[i],
            "bt_signal": bt_signal,
            "live_signal": int(live["signal"]),
            "bt_open_action": str(bt["open_action"].iloc[i]),
            "live_open_action": str(live["open_action"]),
            "bt_close_fraction": bt_close,
            "live_close_fraction": float(live["close_fraction"]),
        }
        match = (
            row["bt_signal"] == row["live_signal"]
            and row["bt_open_action"] == row["live_open_action"]
            and abs(row["bt_close_fraction"] - row["live_close_fraction"]) < 1e-9
        )
        if cfg.regime_enabled:
            row["bt_regime"] = str(regime_full.iloc[i])
            row["live_regime"] = str(live["regime"])
            match = match and row["bt_regime"] == row["live_regime"]
        # Post-shift(1) inputs the engine reads at bar i — informational.
        if i > 0:
            row["backtest_effective_signal"] = int(bt["signal"].iloc[i - 1])
            row["backtest_effective_open_action"] = str(
                bt["open_action"].iloc[i - 1])
            row["backtest_effective_close_fraction"] = float(
                bt["close_fraction"].iloc[i - 1])
            if cfg.regime_enabled:
                row["backtest_effective_regime"] = str(
                    regime_full.iloc[i - 1])
        row["match"] = match
        rows.append(row)
    return pd.DataFrame(rows)


def extract_fills(df: pd.DataFrame, cfg: ParityConfig) -> list:
    """Run the full Backtester over ``df`` and return simulated fills.

    Lets a decision-level diff be lined up against the trades the engine
    would actually book (entry/exit price + modeled fee per leg).
    """
    reg = load_registry(cfg.registry)
    work = reg.apply_strategy(
        cfg.strategy_name, df.copy(), dict(cfg.params or {}))
    if cfg.close_refs:
        work = ensure_atr_indicator(work)
    bt = Backtester(
        initial_capital=1000.0,
        platform=cfg.platform,
        open_strategy={"name": cfg.strategy_name,
                       "params": dict(cfg.params or {})},
        close_strategies=cfg.close_refs,
        regime_enabled=cfg.regime_enabled,
        regime_period=cfg.regime_period,
        regime_adx_threshold=cfg.regime_adx_threshold,
        direction=cfg.direction,
        invert_signal=cfg.invert_signal,
        regime_directional_policy=cfg.regime_directional_policy,
        regime_directional_certified_states=cfg.regime_directional_certified_states,
    )
    metrics = bt.run(
        work,
        strategy_name=cfg.strategy_name,
        symbol=cfg.symbol,
        timeframe=cfg.timeframe,
        params=dict(cfg.params or {}),
        save=False,
    )
    fee_pct = fee_pct_for_platform(cfg.platform)
    fills = []
    for trade in metrics.get("trades", []) or []:
        shares = abs(float(trade.get("shares", 0) or 0))
        entry_px = float(trade.get("entry_price", 0) or 0)
        fills.append({
            "event": "entry",
            "bar": str(trade.get("entry_date", "")),
            "side": trade.get("side", "long"),
            "fill_px": entry_px,
            "fee": round(entry_px * shares * fee_pct, 6),
        })
        if trade.get("exit_date"):
            exit_px = float(trade.get("exit_price", 0) or 0)
            fills.append({
                "event": "exit",
                "bar": str(trade.get("exit_date", "")),
                "side": trade.get("side", "long"),
                "fill_px": exit_px,
                "fee": round(exit_px * shares * fee_pct, 6),
                "pnl": float(trade.get("pnl", 0) or 0),
            })
    return fills


def summarize(frame: pd.DataFrame) -> dict:
    """Aggregate a parity frame into a result summary."""
    if frame.empty:
        return {"bars_compared": 0, "mismatches": 0, "clean": True}
    mismatched = frame[~frame["match"]]
    summary = {
        "bars_compared": int(len(frame)),
        "mismatches": int(len(mismatched)),
        "clean": bool(mismatched.empty),
    }
    if not mismatched.empty:
        summary["first_mismatch"] = str(mismatched.iloc[0]["ts"])
        summary["last_mismatch"] = str(mismatched.iloc[-1]["ts"])
    return summary


def _parse_close_refs(raw_list: list) -> list:
    """Parse repeatable --close values: ``name`` or ``name:{json params}``."""
    refs = []
    for item in raw_list:
        if ":" in item:
            name, params_json = item.split(":", 1)
            refs.append({"name": name.strip(),
                         "params": json.loads(params_json)})
        else:
            refs.append({"name": item.strip(), "params": {}})
    return refs


def main(argv: Optional[list] = None) -> int:
    parser = argparse.ArgumentParser(
        description="Per-bar backtest-vs-live decision diff (#906 D7.4)"
    )
    parser.add_argument("--strategy", default=None,
                        help="Open strategy name (ad-hoc mode)")
    parser.add_argument("--symbol", default="BTC/USDT")
    parser.add_argument("--timeframe", default="1h")
    parser.add_argument("--since", default=None,
                        help="Start date YYYY-MM-DD (bounds the replay)")
    parser.add_argument("--params", default=None,
                        help="JSON dict of strategy param overrides")
    parser.add_argument("--registry", choices=["spot", "futures"],
                        default="spot")
    parser.add_argument("--platform", default=None,
                        help="Fee platform for --fills (ad-hoc default "
                             "binanceus; --config mode auto-detects from the "
                             "strategy type unless explicitly set)")
    parser.add_argument("--close", action="append", default=[],
                        help="Close ref: name or name:{json params} "
                             "(repeatable; single ref since #842)")
    parser.add_argument("--config", default=None,
                        help="Live go-trader config JSON (v13+); replaces "
                             "--strategy/--params/--close with the exact "
                             "live refs")
    parser.add_argument("--strategy-id", default=None,
                        help="Strategy id inside --config")
    parser.add_argument("--window", type=int, default=DEFAULT_WINDOW,
                        help=f"Trailing candle window for the live path "
                             f"(default {DEFAULT_WINDOW}, matching the check "
                             f"scripts' --ohlcv-limit); 0 = expanding window")
    parser.add_argument("--stride", type=int, default=1,
                        help="Compare every Nth bar (speeds up long ranges)")
    parser.add_argument("--regime", action="store_true",
                        help="Also diff the regime label per bar")
    parser.add_argument("--regime-period", type=int, default=14)
    parser.add_argument("--regime-adx-threshold", type=float, default=20.0)
    parser.add_argument("--fills", action="store_true",
                        help="Also run the full Backtester and report "
                             "simulated entry/exit fills")
    parser.add_argument("--csv", default=None,
                        help="Write the full per-bar frame to this CSV path")
    parser.add_argument("--jsonl", default=None,
                        help="Write per-bar rows (and fills with --fills) "
                             "as JSON lines to this path")
    parser.add_argument("--max-print", type=int, default=20,
                        help="Max mismatching rows printed to stdout")
    args = parser.parse_args(argv)

    if args.config:
        if not args.strategy_id:
            print("--strategy-id is required with --config", file=sys.stderr)
            return 2
        try:
            # An unset --platform must never force binanceus here — the
            # empty string lets the loader auto-detect from the strategy
            # type (perps/manual → hyperliquid); an explicit flag wins.
            cfg = config_from_live_config(args.config, args.strategy_id,
                                          platform=args.platform or "")
        except (ValueError, OSError, json.JSONDecodeError) as e:
            print(f"--config: {e}", file=sys.stderr)
            return 2
        cfg.regime_enabled = cfg.regime_enabled or args.regime
        symbol, timeframe = cfg.symbol, cfg.timeframe
    else:
        if not args.strategy:
            print("--strategy is required (or use --config + --strategy-id)",
                  file=sys.stderr)
            return 2
        params = None
        if args.params:
            try:
                params = json.loads(args.params)
            except json.JSONDecodeError as e:
                print(f"--params is not valid JSON: {e}", file=sys.stderr)
                return 2
            if not isinstance(params, dict):
                print("--params must be a JSON object", file=sys.stderr)
                return 2
        try:
            close_refs = _parse_close_refs(args.close) or None
        except json.JSONDecodeError as e:
            print(f"--close params are not valid JSON: {e}", file=sys.stderr)
            return 2
        cfg = ParityConfig(
            strategy_name=args.strategy,
            params=dict(params or {}),
            registry=args.registry,
            platform=args.platform or "binanceus",
            symbol=args.symbol,
            timeframe=args.timeframe,
            close_refs=close_refs,
            regime_enabled=args.regime,
            regime_period=args.regime_period,
            regime_adx_threshold=args.regime_adx_threshold,
        )
        symbol, timeframe = args.symbol, args.timeframe

    from data_fetcher import load_cached_data
    df = load_cached_data(symbol, timeframe, start_date=args.since)
    if df is None or df.empty:
        print(f"No cached data for {symbol} {timeframe} — run a "
              f"backtest first to populate the cache.", file=sys.stderr)
        return 2

    window = args.window if args.window and args.window > 0 else None
    try:
        frame = compute_parity_frame(df, cfg=cfg, window=window,
                                     stride=args.stride)
    except ValueError as e:
        print(f"parity: {e}", file=sys.stderr)
        return 2
    result = summarize(frame)
    if result["bars_compared"] == 0:
        print(f"No bars compared: {len(df)} candles loaded but the trailing "
              f"window needs {window or LIVE_MIN_CANDLES} (after --since/"
              f"--stride). A zero-bar comparison is a data error, not a pass.",
              file=sys.stderr)
        return 2
    fills = extract_fills(df, cfg) if args.fills else []

    if args.csv:
        frame.to_csv(args.csv, index=False)
        print(f"Per-bar frame written to {args.csv}")
    if args.jsonl:
        with open(args.jsonl, "w") as fh:
            for rec in frame.to_dict(orient="records"):
                rec["ts"] = str(rec["ts"])
                fh.write(json.dumps(rec, sort_keys=True, default=str) + "\n")
            for fill in fills:
                fh.write(json.dumps({"fill": fill}, sort_keys=True) + "\n")
        print(f"JSONL written to {args.jsonl}")

    print(f"\nParity diff: {cfg.strategy_name} on {symbol} {timeframe} "
          f"(window={'expanding' if window is None else window}, "
          f"stride={args.stride})")
    print(f"  Bars compared: {result['bars_compared']}")
    print(f"  Mismatches:    {result['mismatches']}")
    if args.fills:
        print(f"  Fills:         {len(fills)}")
    if result["clean"]:
        print("  CLEAN — backtest and live paths agree on every compared bar.")
        return 0

    print(f"  First mismatch: {result['first_mismatch']}")
    print(f"  Last mismatch:  {result['last_mismatch']}")
    mismatched = frame[~frame["match"]]
    with pd.option_context("display.max_columns", None, "display.width", 240):
        print(mismatched.head(args.max_print).to_string(index=False))
    if len(mismatched) > args.max_print:
        print(f"  … {len(mismatched) - args.max_print} more "
              f"(use --csv for the full frame)")
    return 1


if __name__ == "__main__":
    sys.exit(main())
