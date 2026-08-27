from __future__ import annotations
import sys
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional, Tuple
CANONICAL_TREND_REGIME_LABELS: Tuple[str, ...] = ('trending_up', 'trending_down', 'ranging')
_deprecated_keys_warned: set = set()

def _warn_deprecated_key(old: str, canonical: str) -> None:
    if old in _deprecated_keys_warned:
        return
    _deprecated_keys_warned.add(old)
    print(f'[DEPRECATED] config key {old!r} is deprecated; use {canonical!r} (#841)', file=sys.stderr)

def _regime_entry_atr_raw(entry_raw: dict):
    has_canon = 'atr_multiple' in entry_raw
    has_legacy = 'atr' in entry_raw
    if has_canon and has_legacy:
        return (None, False, "set only one of 'atr_multiple' or 'atr' ('atr' is the deprecated alias)")
    if has_canon:
        return (entry_raw.get('atr_multiple'), True, None)
    return (None, False, None)

def close_params_are_unified_regime(params) -> bool:
    return isinstance(params, dict) and REGIME_CLASSIFIER_KEY in params

def unified_regime_scalar_params(params: dict, regime: str):
    trend = params.get(REGIME_CLASSIFIER_KEY)
    if not isinstance(trend, dict):
        return (None, 0.0)
    r = (regime or '').strip()
    label = trend.get(r)
    if not isinstance(label, dict):
        if r in ('ranging_directional_up', 'ranging_directional_down'):
            label = trend.get('ranging_directional')
    if not isinstance(label, dict) or 'tp_tiers' not in label:
        return (None, 0.0)
    scalar = {'tp_tiers': label['tp_tiers']}
    if 'atr_source' in params:
        scalar['atr_source'] = params['atr_source']
    sl = 0.0
    try:
        sl = float(label.get('stop_loss_atr', 0) or 0)
    except (TypeError, ValueError):
        sl = 0.0
    return (scalar, sl)
REGIME_CLASSIFIER_KEY = 'trend_regime'
CANONICAL_TREND_REGIME_LABELS = ('trending_up', 'trending_down', 'ranging')
_RANGING_DIRECTIONAL_BARE = 'ranging_directional'
_RANGING_DIRECTIONAL_SUBS = ('ranging_directional_up', 'ranging_directional_down')

def validate_unified_regime_close(params: dict, labels=None) -> List[str]:
    errs: List[str] = []
    for k in params or {}:
        if k not in (REGIME_CLASSIFIER_KEY, 'atr_source'):
            errs.append(f'unified close: unknown param {k!r} (allowed: trend_regime, atr_source)')
    trend = (params or {}).get(REGIME_CLASSIFIER_KEY)
    if not isinstance(trend, dict):
        errs.append(f'unified close.{REGIME_CLASSIFIER_KEY}: must be an object')
        return errs
    label_vocab = list(labels) if labels else list(CANONICAL_TREND_REGIME_LABELS)
    valid = set(label_vocab)
    for l in sorted(set(trend) - valid):
        errs.append(f"unified close.{REGIME_CLASSIFIER_KEY}: unknown regime label {l!r} (expected one of: {', '.join(label_vocab)})")
    bare_directional = trend.get(_RANGING_DIRECTIONAL_BARE) is not None
    for l in label_vocab:
        if l not in trend:
            if bare_directional and l in _RANGING_DIRECTIONAL_SUBS:
                continue
            errs.append(f'unified close.{REGIME_CLASSIFIER_KEY}: missing required regime label {l!r} (must be exhaustive — no silent fallback)')
            continue
        lm = trend[l]
        if not isinstance(lm, dict):
            errs.append(f'unified close.{REGIME_CLASSIFIER_KEY}.{l}: must be an object')
            continue
        for k in lm:
            if k not in ('stop_loss_atr', 'tp_tiers'):
                errs.append(f'unified close.{REGIME_CLASSIFIER_KEY}.{l}: unknown key {k!r} (allowed: stop_loss_atr, tp_tiers)')
        if 'stop_loss_atr' not in lm:
            errs.append(f"unified close.{REGIME_CLASSIFIER_KEY}.{l}: missing required 'stop_loss_atr' (the unified close owns the per-regime SL)")
        else:
            try:
                sl = float(lm['stop_loss_atr'])
            except (TypeError, ValueError):
                sl = -1.0
            if not sl > 0:
                errs.append(f'unified close.{REGIME_CLASSIFIER_KEY}.{l}.stop_loss_atr: must be > 0')
        if 'tp_tiers' not in lm:
            errs.append(f"unified close.{REGIME_CLASSIFIER_KEY}.{l}: missing required 'tp_tiers'")
            continue
        errs.extend(_validate_unified_tier_list(lm['tp_tiers'], f'unified close.{REGIME_CLASSIFIER_KEY}.{l}'))
    return errs

def _validate_unified_tier_list(raw, ctx_label: str) -> List[str]:
    if not isinstance(raw, list):
        return [f'{ctx_label}.tp_tiers: must be a list, got {type(raw).__name__}']
    if len(raw) < 2:
        return [f'{ctx_label}.tp_tiers: must have at least 2 tiers, got {len(raw)}']
    errs: List[str] = []
    for i, item in enumerate(raw):
        if not isinstance(item, dict):
            errs.append(f'{ctx_label}.tp_tiers[{i}]: must be an object, got {type(item).__name__}')
            continue
        try:
            mult = float(item.get('atr_multiple'))
        except (TypeError, ValueError):
            mult = -1.0
        if not mult > 0:
            errs.append(f'{ctx_label}.tp_tiers[{i}].atr_multiple: must be > 0')
        try:
            frac = float(item.get('close_fraction'))
        except (TypeError, ValueError):
            frac = -1.0
        if not 0 < frac <= 1:
            errs.append(f'{ctx_label}.tp_tiers[{i}].close_fraction: must be in (0, 1]')
        if 'sl_after' in item:
            try:
                from post_tp_sl import parse_sl_after_rule, validate_sl_after_rule
            except ImportError:
                from .post_tp_sl import parse_sl_after_rule, validate_sl_after_rule
            try:
                rule = parse_sl_after_rule(item['sl_after'])
                validate_sl_after_rule(rule)
            except ValueError as e:
                errs.append(f'{ctx_label}.tp_tiers[{i}].sl_after: {e}')
            else:
                if rule.has_regime():
                    errs.append(f'{ctx_label}.tp_tiers[{i}].sl_after: must be scalar in a unified per-regime block (the regime is resolved at the top level; drop the trend_regime sub-block)')
        for k in item:
            if k not in ('atr_multiple', 'close_fraction', 'sl_after'):
                errs.append(f'{ctx_label}.tp_tiers[{i}]: unknown key {k!r} (allowed: atr_multiple, close_fraction, sl_after)')
    return errs
SURFACE_STOP_LOSS = 'stop_loss'
SURFACE_TRAILING = 'trailing'
SURFACE_TP_TIER_ATR_ONLY = 'tp_tier_atr_only'
SURFACE_TP_TIER_WITH_FRAC = 'tp_tier_with_frac'
SURFACE_SL_AFTER = 'sl_after'
SURFACE_SL_AFTER_TRAIL = 'sl_after_trail'

@dataclass(frozen=True)
class RegimeATREntry:
    atr: float = 0.0
    close_fraction: float = 0.0
    has_close_frac: bool = False

@dataclass
class RegimeATRBlock:
    use_defaults: bool = False
    trend_regime: Dict[str, RegimeATREntry] = field(default_factory=dict)

    def is_zero(self) -> bool:
        return not self.use_defaults and (not self.trend_regime)

    def resolve(self, regime: str) -> Optional[RegimeATREntry]:
        if not self.trend_regime:
            return None
        r = (regime or '').strip()
        entry = self.trend_regime.get(r)
        if entry is not None:
            return entry
        if r in ('ranging_directional_up', 'ranging_directional_down'):
            return self.trend_regime.get('ranging_directional')
        return None
REGIME_ATR_DEFAULTS_STOP_LOSS: Dict[str, RegimeATREntry] = {'trending_up': RegimeATREntry(atr=2.0), 'trending_down': RegimeATREntry(atr=2.0), 'ranging': RegimeATREntry(atr=1.5)}
REGIME_ATR_DEFAULTS_TRAILING: Dict[str, RegimeATREntry] = {'trending_up': RegimeATREntry(atr=2.5), 'trending_down': RegimeATREntry(atr=2.5), 'ranging': RegimeATREntry(atr=2.0), 'trending_up_clean': RegimeATREntry(atr=2.5), 'trending_down_clean': RegimeATREntry(atr=2.5), 'trending_up_choppy': RegimeATREntry(atr=2.25), 'trending_down_choppy': RegimeATREntry(atr=2.25), 'ranging_quiet': RegimeATREntry(atr=1.0), 'ranging_volatile': RegimeATREntry(atr=1.25), 'ranging_directional': RegimeATREntry(atr=1.5), 'ranging_directional_up': RegimeATREntry(atr=1.5), 'ranging_directional_down': RegimeATREntry(atr=1.5)}
REGIME_TP_TIER_GROUP_DEFAULTS: Dict[str, List[Tuple[float, float]]] = {'clean': [(2.5, 0.25), (4.0, 0.5), (5.5, 0.75), (7.0, 1.0)], 'choppy': [(1.5, 0.4), (3.0, 0.8), (5.0, 1.0)], 'ranging': [(0.5, 0.5), (1.0, 1.0)]}

def regime_close_default_group(label: str) -> Optional[str]:
    label = (label or '').strip()
    if not label:
        return None
    if label.endswith('_clean'):
        return 'clean'
    if label.endswith('_choppy'):
        return 'choppy'
    if label.startswith('ranging'):
        return 'ranging'
    if label.startswith('trending_up') or label.startswith('trending_down'):
        return 'choppy'
    return None

def _default_block_for_surface(surface: str) -> Optional[Dict[str, RegimeATREntry]]:
    if surface == SURFACE_STOP_LOSS:
        return dict(REGIME_ATR_DEFAULTS_STOP_LOSS)
    if surface == SURFACE_TRAILING:
        return dict(REGIME_ATR_DEFAULTS_TRAILING)
    return None

def parse_regime_atr_block(raw: Any, ctx_label: str, surface: str, labels: Optional[Tuple[str, ...]]=None) -> Tuple[RegimeATRBlock, List[str]]:
    errs: List[str] = []
    labels = tuple(labels or CANONICAL_TREND_REGIME_LABELS)
    if raw is None:
        return (RegimeATRBlock(), errs)
    if not isinstance(raw, dict):
        errs.append(f'{ctx_label}: must be an object, got {type(raw).__name__}')
        return (RegimeATRBlock(), errs)
    allowed_top = {'use_defaults', REGIME_CLASSIFIER_KEY}
    for k in raw.keys():
        if k not in allowed_top:
            errs.append(f"{ctx_label}: unknown key {k!r} (expected 'use_defaults' or {REGIME_CLASSIFIER_KEY!r})")
    use_defaults_raw = raw.get('use_defaults')
    trend_raw = raw.get(REGIME_CLASSIFIER_KEY)
    has_use_defaults = 'use_defaults' in raw
    has_trend = REGIME_CLASSIFIER_KEY in raw
    use_defaults = False
    if has_use_defaults:
        if not isinstance(use_defaults_raw, bool):
            errs.append(f'{ctx_label}: use_defaults must be a boolean, got {type(use_defaults_raw).__name__}')
        else:
            use_defaults = use_defaults_raw
    if use_defaults and has_trend:
        errs.append(f'{ctx_label}: cannot combine use_defaults:true with explicit {REGIME_CLASSIFIER_KEY} (use_defaults is all-or-nothing)')
    if use_defaults:
        baseline = _default_block_for_surface(surface)
        if baseline is None:
            errs.append(f'{ctx_label}: use_defaults not supported on this surface (tier-level use_defaults is handled by the close evaluator parser)')
            return (RegimeATRBlock(), errs)
        return (RegimeATRBlock(use_defaults=True, trend_regime=baseline), errs)
    if not has_trend:
        errs.append(f'{ctx_label}: missing {REGIME_CLASSIFIER_KEY!r} (either set use_defaults:true or supply a trend_regime block)')
        return (RegimeATRBlock(), errs)
    if not isinstance(trend_raw, dict):
        errs.append(f'{ctx_label}: {REGIME_CLASSIFIER_KEY} must be an object, got {type(trend_raw).__name__}')
        return (RegimeATRBlock(), errs)
    valid_labels = set(labels)
    unknown_labels = sorted([k for k in trend_raw.keys() if k not in valid_labels])
    for k in unknown_labels:
        errs.append(f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: unknown regime label {k!r} (expected one of: {', '.join(labels)})")
    bare_directional_present = 'ranging_directional' in trend_raw
    missing_labels = [l for l in labels if l not in trend_raw and (not (l in ('ranging_directional_up', 'ranging_directional_down') and bare_directional_present))]
    if missing_labels:
        errs.append(f"{ctx_label}.{REGIME_CLASSIFIER_KEY}: missing required regime labels: {', '.join(missing_labels)} (must be exhaustive — no silent fallback)")
    result: Dict[str, RegimeATREntry] = {}
    allow_frac = surface == SURFACE_TP_TIER_WITH_FRAC
    allowed_entry_keys = {'atr_multiple'} | ({'close_fraction'} if allow_frac else set())
    for label in labels:
        entry_raw = trend_raw.get(label)
        if entry_raw is None:
            continue
        if not isinstance(entry_raw, dict):
            errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: must be an object, got {type(entry_raw).__name__}')
            continue
        entry_unknown = sorted([k for k in entry_raw.keys() if k not in allowed_entry_keys])
        for k in entry_unknown:
            hint = ''
            if k == 'close_fraction':
                hint = ' — close_fraction is only allowed inside close-evaluator tiers; for SL/trailing/sl_after surfaces, only atr_multiple is accepted'
            errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: unknown key {k!r}{hint}')
        atr_raw, atr_present, atr_err = _regime_entry_atr_raw(entry_raw)
        if atr_err:
            errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: {atr_err}')
            continue
        if not atr_present:
            errs.append(f"{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}: missing required 'atr_multiple'")
            continue
        try:
            atr = float(atr_raw)
        except (TypeError, ValueError):
            errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr_multiple: expected number, got {atr_raw!r}')
            continue
        if surface != SURFACE_SL_AFTER and atr <= 0:
            errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.atr_multiple: must be > 0, got {atr}')
            continue
        entry = RegimeATREntry(atr=atr)
        if allow_frac and 'close_fraction' in entry_raw:
            frac_raw = entry_raw['close_fraction']
            try:
                frac = float(frac_raw)
            except (TypeError, ValueError):
                errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.close_fraction: expected number, got {frac_raw!r}')
                continue
            if frac <= 0 or frac > 1:
                errs.append(f'{ctx_label}.{REGIME_CLASSIFIER_KEY}.{label}.close_fraction: must be in (0, 1], got {frac}')
                continue
            entry = RegimeATREntry(atr=atr, close_fraction=frac, has_close_frac=True)
        result[label] = entry
    return (RegimeATRBlock(trend_regime=result), errs)

def resolve_regime_atr(block: RegimeATRBlock, regime: str) -> Optional[float]:
    entry = block.resolve(regime)
    if entry is None or entry.atr <= 0:
        return None
    return entry.atr

@dataclass
class RegimeTierSpec:
    block: RegimeATRBlock
    tier_close_fraction: Optional[float] = None

def parse_regime_tp_tiers(raw_tiers: Any, ctx_label: str, use_defaults: bool, labels: Optional[Tuple[str, ...]]=None) -> Tuple[List[RegimeTierSpec], List[str]]:
    errs: List[str] = []
    labels = tuple(labels or CANONICAL_TREND_REGIME_LABELS)
    if use_defaults:
        if raw_tiers is not None:
            errs.append(f'{ctx_label}: cannot combine use_defaults:true with explicit tiers (use_defaults is all-or-nothing)')
            return ([], errs)
        label_ladders: Dict[str, List[Tuple[float, float]]] = {}
        max_tiers = 0
        for label in labels:
            group = regime_close_default_group(label)
            ladder = REGIME_TP_TIER_GROUP_DEFAULTS.get(group) if group else None
            if not ladder:
                continue
            label_ladders[label] = ladder
            if len(ladder) > max_tiers:
                max_tiers = len(ladder)
        out: List[RegimeTierSpec] = []
        for i in range(max_tiers):
            trend: Dict[str, RegimeATREntry] = {}
            for label, ladder in label_ladders.items():
                if i < len(ladder):
                    mult, frac = ladder[i]
                    trend[label] = RegimeATREntry(atr=mult, close_fraction=frac, has_close_frac=True)
            out.append(RegimeTierSpec(block=RegimeATRBlock(use_defaults=True, trend_regime=trend), tier_close_fraction=None))
        return (out, errs)
    if not isinstance(raw_tiers, list):
        errs.append(f'{ctx_label}: tiers must be a list when use_defaults is not set, got {type(raw_tiers).__name__}')
        return ([], errs)
    tiers: List[RegimeTierSpec] = []
    for idx, item in enumerate(raw_tiers):
        if not isinstance(item, dict):
            errs.append(f'{ctx_label}.tiers[{idx}]: must be an object')
            continue
        per_regime_has_frac = False
        trend_block = item.get(REGIME_CLASSIFIER_KEY)
        if isinstance(trend_block, dict):
            for v in trend_block.values():
                if isinstance(v, dict) and 'close_fraction' in v:
                    per_regime_has_frac = True
                    break
        tier_level_frac_present = 'close_fraction' in item
        if per_regime_has_frac and tier_level_frac_present:
            errs.append(f'{ctx_label}.tiers[{idx}]: cannot combine per-regime close_fraction with tier-level scalar close_fraction (pick one shape per tier)')
            continue
        if not per_regime_has_frac and (not tier_level_frac_present):
            errs.append(f'{ctx_label}.tiers[{idx}]: missing close_fraction (either at tier level or inside every per-regime entry)')
            continue
        surface = SURFACE_TP_TIER_WITH_FRAC if per_regime_has_frac else SURFACE_TP_TIER_ATR_ONLY
        tier_subset = {k: v for k, v in item.items() if k not in ('close_fraction', 'sl_after')}
        sub_label = f'{ctx_label}.tiers[{idx}]'
        block, sub_errs = parse_regime_atr_block(tier_subset, sub_label, surface, labels=labels)
        errs.extend(sub_errs)
        tier_frac: Optional[float] = None
        if tier_level_frac_present:
            try:
                tier_frac = float(item['close_fraction'])
            except (TypeError, ValueError):
                errs.append(f"{ctx_label}.tiers[{idx}].close_fraction: expected number, got {item['close_fraction']!r}")
                continue
            if tier_frac <= 0 or tier_frac > 1:
                errs.append(f'{ctx_label}.tiers[{idx}].close_fraction: must be in (0, 1], got {tier_frac}')
                continue
        tiers.append(RegimeTierSpec(block=block, tier_close_fraction=tier_frac))
    return (tiers, errs)

def resolve_regime_tier(spec: RegimeTierSpec, regime: str) -> Optional[Tuple[float, float]]:
    entry = spec.block.resolve(regime)
    if entry is None or entry.atr <= 0:
        return None
    if spec.tier_close_fraction is not None:
        return (entry.atr, spec.tier_close_fraction)
    if not entry.has_close_frac or entry.close_fraction <= 0:
        return None
    return (entry.atr, entry.close_fraction)
