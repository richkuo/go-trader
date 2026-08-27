from __future__ import annotations
import json
import inspect
import sys
from dataclasses import dataclass
from typing import Callable, Iterable, Optional
import pandas as pd
VALID_POSITION_SIDES = {'', 'long', 'short'}
VALID_OPEN_ACTIONS = {'long', 'short', 'none'}
POSITION_CONTEXT_PARAM_KEYS = {'side', 'avg_cost', 'current_quantity', 'initial_quantity', 'entry_atr', 'regime'}

@dataclass
class CloseEvaluation:
    strategy: str
    close_fraction: float

@dataclass
class OpenCloseEvaluation:
    open_strategy: str
    close_strategies: list[str]
    open_result_df: pd.DataFrame
    open_signal: int
    close_evaluations: list[CloseEvaluation]

def parse_strategy_refs_arg(raw: Optional[str]) -> Optional[dict]:
    if not raw:
        return None
    import json
    payload = json.loads(raw)
    open_block = payload.get('open') or {}
    closes_block = payload.get('closes') or []
    open_name = open_block.get('name') or None
    open_params = open_block.get('params') or None
    close_names: list[str] = []
    close_params_by_name: dict[str, dict] = {}
    for ref in closes_block:
        if not isinstance(ref, dict):
            continue
        name = (ref.get('name') or '').strip()
        if not name:
            continue
        close_names.append(name)
        ref_params = ref.get('params')
        if ref_params:
            close_params_by_name[name] = ref_params
    return {'open_name': open_name, 'open_params': open_params, 'close_csv': ','.join(close_names) if close_names else None, 'close_params_by_name': close_params_by_name or None}

def parse_close_strategies(raw: Optional[str | Iterable[str]]) -> list[str]:
    if raw is None:
        return []
    if isinstance(raw, str):
        parts = raw.split(',')
    else:
        parts = list(raw)
    return [str(part).strip() for part in parts if str(part).strip()]

def normalize_signal(value) -> int:
    try:
        signal = int(value)
    except (TypeError, ValueError):
        return 0
    if signal > 0:
        return 1
    if signal < 0:
        return -1
    return 0

def open_action_from_signal(signal: int) -> str:
    if signal > 0:
        return 'long'
    if signal < 0:
        return 'short'
    return 'none'

def clamp_close_fraction(value) -> float:
    try:
        fraction = float(value)
    except (TypeError, ValueError):
        return 0.0
    if fraction < 0:
        return 0.0
    if fraction > 1:
        return 1.0
    return fraction

def legacy_close_fraction_from_signal(signal: int, position_side: str) -> float:
    position_side = (position_side or '').strip().lower()
    if position_side == 'long' and signal < 0:
        return 1.0
    if position_side == 'short' and signal > 0:
        return 1.0
    return 0.0

def max_close_fraction(evaluations: Iterable[CloseEvaluation]) -> tuple[float, str]:
    best_fraction = 0.0
    best_strategy = ''
    for evaluation in evaluations:
        fraction = clamp_close_fraction(evaluation.close_fraction)
        if fraction > best_fraction:
            best_fraction = fraction
            best_strategy = evaluation.strategy
    return (best_fraction, best_strategy)

def compose_signal(open_action: str, close_fraction: float, position_side: str) -> int:
    position_side = (position_side or '').strip().lower()
    open_action = (open_action or 'none').strip().lower()
    if position_side not in VALID_POSITION_SIDES:
        raise ValueError(f'position_side must be one of {sorted(VALID_POSITION_SIDES)}, got {position_side!r}')
    if open_action not in VALID_OPEN_ACTIONS:
        raise ValueError(f'open_action must be one of {sorted(VALID_OPEN_ACTIONS)}, got {open_action!r}')
    if clamp_close_fraction(close_fraction) > 0:
        if position_side == 'long':
            return -1
        if position_side == 'short':
            return 1
        return 0
    if position_side:
        return 0
    if open_action == 'long':
        return 1
    if open_action == 'short':
        return -1
    return 0

def effective_close_strategies(positional_strategy: str, open_strategy: Optional[str], close_strategies: Optional[Iterable[str]]) -> list[str]:
    explicit = parse_close_strategies(close_strategies)
    if explicit:
        return explicit
    return [(open_strategy or positional_strategy).strip()]

def _safe_list_strategy_names(list_fn: Optional[Callable[[], Iterable[str]]]) -> list[str]:
    if list_fn is None:
        return []
    try:
        return list(list_fn())
    except Exception:
        return []
_DEPRECATED_CLOSE_NAMES = {'tp_at_pct': 'tiered_tp_pct'}

def canonical_close_name(name: str) -> str:
    name = (name or '').strip()
    return _DEPRECATED_CLOSE_NAMES.get(name, name)

def close_names_include_avwap_stop(close_names: Iterable[str]) -> bool:
    return any((canonical_close_name(name) == 'avwap_stop' for name in close_names))

def warn_avwap_stop_missing_context() -> None:
    print("[WARN] close strategy 'avwap_stop' is configured but the open strategy produced no usable 'avwap' value; the AVWAP exit can never fire and the position is protected only by the engine stop-loss. Verify the open strategy is an AVWAP-family strategy that emits an 'avwap' column (#1196).", file=sys.stderr)

def rewrite_deprecated_close_ref(name: str, params: Optional[dict]) -> tuple[str, Optional[dict]]:
    name = (name or '').strip()
    resolved = canonical_close_name(name)
    if name != 'tp_at_pct':
        return (resolved, params)
    pct = 0.03
    if params and params.get('pct') is not None:
        try:
            pct = max(float(params.get('pct', 0.03)), 0.0)
        except (TypeError, ValueError):
            pct = 0.03
    out: dict = {'tp_tiers': [{'profit_pct': pct, 'close_fraction': 1.0}]}
    if params and 'sl_after' in params:
        out['sl_after'] = params['sl_after']
    return (resolved, out)

def reject_backtest_only_strategies(names: Iterable[str], get_strategy: Callable[[str], dict]) -> None:
    for name in names:
        entry = get_strategy(name)
        if isinstance(entry, dict) and entry.get('backtest_only'):
            raise ValueError(f"Strategy '{name}' is registered backtest_only (offline research, #1138) — it must not be evaluated on a live check path. Wiring it to live requires explicit human sign-off after parity/Sharpe/M1 checks.")

def validate_close_strategy_names(close_names: Iterable[str], get_open_strategy: Callable[[str], object], get_close_strategy: Callable[[str], object], list_open_strategies: Optional[Callable[[], Iterable[str]]]=None, list_close_strategies: Optional[Callable[[], Iterable[str]]]=None) -> None:
    for name in close_names:
        resolved = canonical_close_name(name)
        try:
            get_close_strategy(resolved)
            continue
        except ValueError:
            pass
        try:
            entry = get_open_strategy(resolved)
        except ValueError as exc:
            raise ValueError(f'Unknown close strategy: {name}. Available close strategies: {_safe_list_strategy_names(list_close_strategies)}; fallback open strategies: {_safe_list_strategy_names(list_open_strategies)}') from exc
        if isinstance(entry, dict) and entry.get('backtest_only'):
            raise ValueError(f"Close strategy '{name}' resolves to the backtest_only open strategy fallback (offline research, #1138) — it must not be evaluated on a live check path.")

def _last_signal(result_df: pd.DataFrame) -> int:
    if result_df.empty:
        return 0
    return normalize_signal(result_df.iloc[-1].get('signal', 0))

def _last_close_fraction(result_df: pd.DataFrame, signal: int, position_side: str) -> float:
    if not result_df.empty and 'close_fraction' in result_df.columns:
        return clamp_close_fraction(result_df.iloc[-1].get('close_fraction', 0))
    return legacy_close_fraction_from_signal(signal, position_side)

def _cache_key_params(params: Optional[dict]) -> str:
    if params is None:
        return ''
    try:
        return json.dumps(params, sort_keys=True, default=str)
    except TypeError:
        return repr(sorted(((str(k), repr(v)) for k, v in params.items())))

def _merge_close_params(base: Optional[dict], position_ctx: Optional[dict]) -> Optional[dict]:
    if not position_ctx:
        return base
    merged = dict(base or {})
    merged.update(position_ctx)
    return merged

def _default_market_ctx(df: pd.DataFrame) -> dict:
    if df.empty or 'close' not in df.columns:
        return {}
    try:
        return {'mark_price': float(df['close'].iloc[-1])}
    except (TypeError, ValueError):
        return {}

def _is_unknown_close_strategy_error(exc: ValueError) -> bool:
    return str(exc).startswith('Unknown close strategy:')

def strip_unsupported_position_context(fn, params: dict) -> dict:
    if not params:
        return params
    sig = inspect.signature(fn)
    accepted = {name for name, p in sig.parameters.items() if name != 'df' and p.kind in (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)}
    return {key: value for key, value in params.items() if key in accepted or key not in POSITION_CONTEXT_PARAM_KEYS}

def evaluate_open_close(apply_strategy: Callable[[str, pd.DataFrame, Optional[dict]], pd.DataFrame], get_strategy: Callable[[str], object], df: pd.DataFrame, positional_strategy: str, open_strategy: Optional[str], close_strategies: Optional[Iterable[str]], position_side: str, params: Optional[dict]=None, position_ctx: Optional[dict]=None, close_evaluate: Optional[Callable[[str, dict, dict, Optional[dict]], dict]]=None, market_ctx: Optional[dict]=None, close_params_by_name: Optional[dict[str, dict]]=None) -> OpenCloseEvaluation:
    open_name = (open_strategy or positional_strategy).strip()
    close_names = effective_close_strategies(positional_strategy, open_name, close_strategies)
    cache: dict[tuple[str, str], pd.DataFrame] = {}

    def run(name: str, run_params: Optional[dict]) -> pd.DataFrame:
        if not name:
            raise ValueError('strategy name must not be empty')
        key = (name, _cache_key_params(run_params))
        if key not in cache:
            get_strategy(name)
            cache[key] = apply_strategy(name, df, run_params)
        return cache[key]
    open_result = run(open_name, params)
    open_signal = _last_signal(open_result)
    close_evals: list[CloseEvaluation] = []
    market = market_ctx if market_ctx is not None else _default_market_ctx(df)
    avwap_injected = False
    if not open_result.empty and 'avwap' in open_result.columns:
        try:
            avwap_value = float(open_result['avwap'].iloc[-1])
        except (TypeError, ValueError):
            avwap_value = float('nan')
        if avwap_value == avwap_value and avwap_value > 0:
            market = {**market, 'avwap': avwap_value}
            avwap_injected = True
    if not avwap_injected and close_names_include_avwap_stop(close_names):
        warn_avwap_stop_missing_context()
    for name in close_names:
        resolved, _ = rewrite_deprecated_close_ref(name, None)
        if close_params_by_name and name in close_params_by_name:
            base_close_params = close_params_by_name[name]
        elif close_params_by_name and resolved in close_params_by_name:
            base_close_params = close_params_by_name[resolved]
        elif name == open_name or resolved == open_name:
            base_close_params = params
        else:
            base_close_params = None
        resolved, base_close_params = rewrite_deprecated_close_ref(name, base_close_params)
        if close_evaluate is not None:
            try:
                result = close_evaluate(resolved, position_ctx or {}, market, base_close_params)
                close_evals.append(CloseEvaluation(strategy=resolved, close_fraction=result.get('close_fraction', 0.0)))
                continue
            except ValueError as exc:
                if not _is_unknown_close_strategy_error(exc):
                    raise
        close_params = _merge_close_params(base_close_params, position_ctx)
        result = run(resolved, close_params)
        signal = _last_signal(result)
        close_evals.append(CloseEvaluation(strategy=resolved, close_fraction=_last_close_fraction(result, signal, position_side)))
    return OpenCloseEvaluation(open_strategy=open_name, close_strategies=close_names, open_result_df=open_result, open_signal=open_signal, close_evaluations=close_evals)

def finalize_decision(evaluation: OpenCloseEvaluation, position_side: str, open_signal: Optional[int]=None) -> dict:
    signal = evaluation.open_signal if open_signal is None else normalize_signal(open_signal)
    open_action = open_action_from_signal(signal)
    close_fraction, close_strategy = max_close_fraction(evaluation.close_evaluations)
    return {'open_strategy': evaluation.open_strategy, 'close_strategies': evaluation.close_strategies, 'open_action': open_action, 'close_fraction': close_fraction, 'close_strategy': close_strategy, 'signal': compose_signal(open_action, close_fraction, position_side)}
