from __future__ import annotations
import argparse
import copy
import json
import os
import shlex
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor
_THIS_DIR = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_THIS_DIR, '..'))
if _THIS_DIR not in sys.path:
    sys.path.insert(0, _THIS_DIR)
from eval_windows import DATASETS as M1_DATASETS, WINDOWS as M1_WINDOWS, expand_sweep, validate_candidate
from exit_policy_ab import candidate_is_replayable
from regime_stats import benjamini_hochberg
HARNESS_REL = {'m1_noise': 'backtest/gross_edge_noise.py', 'm1': 'backtest/eval_windows.py', 'm3': 'backtest/exit_diagnostics.py', 'm5': 'backtest/fee_audit.py', 'm6': 'backtest/exit_policy_ab.py', 'mc': 'backtest/monte_carlo.py'}
HARNESS_ABS = {k: os.path.join(_REPO, v) for k, v in HARNESS_REL.items()}
KNOWN_HARNESSES = set(HARNESS_REL)
OPEN_HARNESSES = ('m1_noise', 'm1', 'm3', 'm5', 'mc')
DEFAULT_HARNESSES = ['m1_noise', 'm1', 'm3', 'm5']
KNOWN_REGISTRIES = ('spot', 'futures')
ADVISORY_HARNESSES = ('mc',)
MC_DEFAULT_N_PATHS = 10000
MC_SCHEME_FOR_COLUMNS = 'permute'
VERDICT_ORDER = ['survivor', 'positive_uncorrected_only', 'positive_but_not_significant', 'incumbent_stands', 'inconclusive', 'noise_gate_blocked', 'excluded_not_replayable', 'run_failed']
FOOTER = 'Suggest-only. No config was modified and no live default was written. Promotion is a human decision.'

def _resolve_ref(ref, spec_dir: str) -> dict:
    if isinstance(ref, dict):
        return copy.deepcopy(ref)
    if isinstance(ref, str):
        path = ref if os.path.isabs(ref) else os.path.join(spec_dir, ref)
        with open(path) as fh:
            return json.load(fh)
    raise ValueError(f'expected an inline object or a filename, got {ref!r}')

def load_spec(raw: dict, spec_dir: str) -> dict:
    if not isinstance(raw, dict):
        raise ValueError('spec must be a JSON object')
    registry = str(raw.get('registry') or 'spot').strip().lower()
    if registry not in KNOWN_REGISTRIES:
        raise ValueError(f'registry must be one of {KNOWN_REGISTRIES}, got {registry!r} (M6 is spot/futures only; perps backtest via the futures registry)')
    harnesses = list(raw.get('harnesses') or DEFAULT_HARNESSES)
    unknown = [h for h in harnesses if h not in KNOWN_HARNESSES]
    if unknown:
        raise ValueError(f'unknown harnesses {unknown}; known: {sorted(KNOWN_HARNESSES)}')
    windows = list(raw.get('windows') or ['is', 'oos'])
    bad_windows = [w for w in windows if w not in M1_WINDOWS]
    if bad_windows:
        raise ValueError(f'unknown windows {bad_windows}; known: {list(M1_WINDOWS)}')
    correction = dict(raw.get('correction') or {})
    method = str(correction.get('method') or 'benjamini_hochberg')
    if method != 'benjamini_hochberg':
        raise ValueError("correction.method must be 'benjamini_hochberg' (the only supported family correction, per #1210 scope)")
    alpha = float(correction.get('alpha', 0.05))
    family_size = correction.get('family_size')
    if family_size is not None:
        if isinstance(family_size, bool) or not isinstance(family_size, int):
            raise ValueError('correction.family_size must be an integer (the searched candidate-family size N for a selection-aware two-stage run; #1338)')
        if family_size < 1:
            raise ValueError('correction.family_size must be >= 1')
    base = _resolve_ref(raw['base'], spec_dir) if raw.get('base') is not None else None
    candidates = []
    for c in raw.get('candidates') or []:
        if 'key' not in c or 'candidate' not in c:
            raise ValueError("each candidates[] entry needs 'key' and 'candidate'")
        cand = _resolve_ref(c['candidate'], spec_dir)
        validate_candidate(copy.deepcopy(cand))
        candidates.append({'key': c['key'], 'candidate': cand, 'harnesses': list(c.get('harnesses') or harnesses), 'hypothesis': c.get('hypothesis')})
    mc = dict(raw.get('mc') or {})
    if mc:
        unknown_mc = set(mc) - {'kill_switch_pct', 'config', 'strategy_id', 'n_paths'}
        if unknown_mc:
            raise ValueError(f'unknown mc keys {sorted(unknown_mc)}; known: kill_switch_pct, config, strategy_id, n_paths')
        if mc.get('kill_switch_pct') is not None and mc.get('config'):
            raise ValueError("mc block sets both 'kill_switch_pct' and 'config'; they are mutually exclusive threshold sources (monte_carlo.py refuses both)")
        if bool(mc.get('config')) != bool(mc.get('strategy_id')):
            raise ValueError("mc 'config' and 'strategy_id' go together (the strategy id selects whose max_drawdown_pct hierarchy resolves)")
        if mc.get('config'):
            cfg = mc['config']
            mc['config'] = cfg if os.path.isabs(cfg) else os.path.join(spec_dir, cfg)
        if mc.get('n_paths') is not None and int(mc['n_paths']) < 1:
            raise ValueError("mc 'n_paths' must be >= 1")
    m6 = None
    if raw.get('m6') is not None:
        m6 = dict(raw['m6'])
        bc = m6.get('baseline_config')
        if bc and isinstance(bc, str):
            m6['baseline_config'] = bc if os.path.isabs(bc) else os.path.join(spec_dir, bc)
        if bool(m6.get('baseline_config')) == bool(m6.get('incumbent_close')):
            raise ValueError("m6 block needs EXACTLY one of 'baseline_config' (resolve the live close from a daemon config) or 'incumbent_close' (an explicit close-ref ladder); got " + ('both' if m6.get('baseline_config') else 'neither'))
        if not m6.get('strategy_id'):
            if m6.get('close_stack_specs'):
                raise ValueError("m6 'close_stack_specs' are generated variants that cannot carry a per-variant 'strategy_id'; set an m6-level 'strategy_id' (the open-strategy name, or the config's strategy id when using 'baseline_config').")
            for v in m6.get('candidate_close_variants') or []:
                if not v.get('strategy_id'):
                    raise ValueError('m6 candidate_close_variant ' + repr(v.get('key') or '<unkeyed>') + " has no 'strategy_id' and the m6 block sets no default; add an m6-level 'strategy_id' or a per-variant override (the open-strategy name, or the config's strategy id when using 'baseline_config').")
    return {'study': str(raw.get('study') or 'unnamed_study'), 'registry': registry, 'harnesses': harnesses, 'windows': windows, 'datasets': raw.get('datasets'), 'correction': {'method': method, 'alpha': alpha, 'family_size': family_size}, 'base': base, 'candidates': candidates, 'sweep': raw.get('sweep'), 'gate_variants': raw.get('gate_variants'), 'm6': m6, 'mc': mc, 'spec_dir': spec_dir}

def _sanitize_label(label: str) -> str:
    return label.replace('=', '').replace(' ', '.').replace('/', '_')

def _open_entry(key: str, candidate: dict, harnesses: list, hypothesis) -> dict:
    limitations = []
    if 'm5' in harnesses and candidate.get('params'):
        limitations.append('m5_params_unaudited')
    return {'key': key, 'kind': 'open', 'candidate': candidate, 'harnesses': [h for h in harnesses if h in OPEN_HARNESSES], 'hypothesis': hypothesis, 'precondition_errors': [], 'limitations': limitations}

def _exit_ab_entry(key: str, variant: dict, m6: dict) -> dict:
    close_refs = variant.get('candidate_close')
    errors = []
    if not candidate_is_replayable(close_refs):
        errors.append('excluded_not_replayable')
    return {'key': key, 'kind': 'exit_ab', 'candidate': {'baseline_config': m6.get('baseline_config'), 'incumbent_close': m6.get('incumbent_close'), 'strategy_id': variant.get('strategy_id') or m6.get('strategy_id'), 'candidate_close': close_refs, 'candidate_stops': variant.get('candidate_stops', 'inherit'), 'allowed_regimes': list(variant.get('allowed_regimes') or [])}, 'harnesses': ['m6'], 'hypothesis': variant.get('hypothesis'), 'precondition_errors': errors, 'limitations': []}

def expand_candidates(spec: dict) -> list:
    entries = []
    default_harnesses = spec['harnesses']
    for c in spec['candidates']:
        entries.append(_open_entry(c['key'], c['candidate'], c['harnesses'], c['hypothesis']))
    sweep, gate_variants, base = (spec.get('sweep'), spec.get('gate_variants'), spec.get('base'))
    if sweep or gate_variants:
        if base is None:
            raise ValueError("sweep/gate_variants require a 'base' candidate")
        base_name = base.get('name', 'base')
        if sweep:
            sweep_specs = [(k, list(v)) for k, v in sweep.items()]
            seeds = []
            for label, params in expand_sweep(dict(base.get('params') or {}), sweep_specs):
                cand = copy.deepcopy(base)
                cand['params'] = params
                seeds.append((_sanitize_label(label), cand))
        else:
            seeds = [(None, copy.deepcopy(base))]
        for slabel, scand in seeds:
            for gv in gate_variants or [None]:
                cand = copy.deepcopy(scand)
                parts = [base_name]
                if slabel:
                    parts.append(slabel)
                if gv:
                    if not gv.get('allowed_regimes'):
                        raise ValueError("gate_variants[] entry needs 'allowed_regimes'")
                    cand['allowed_regimes'] = list(gv['allowed_regimes'])
                    if gv.get('regime_windows_spec'):
                        cand['regime_windows_spec'] = gv['regime_windows_spec']
                    parts.append(gv['label'])
                validate_candidate(copy.deepcopy(cand))
                entries.append(_open_entry('.'.join(parts), cand, default_harnesses, None))
    m6 = spec.get('m6')
    if m6:
        variants = list(m6.get('candidate_close_variants') or [])
        stack_specs = m6.get('close_stack_specs')
        if stack_specs:
            from optimizer import generate_close_stack_grid
            for i, stack in enumerate(generate_close_stack_grid(stack_specs)):
                close_refs = list(stack.get('close_strategies') or [])
                if stack.get('stop_loss_atr_mult'):
                    close_refs.append({'name': 'stop_loss_atr_mult', 'params': {'atr_mult': stack['stop_loss_atr_mult']}})
                elif stack.get('trailing_stop_atr_mult'):
                    close_refs.append({'name': 'trailing_stop_atr_mult', 'params': {'atr_mult': stack['trailing_stop_atr_mult']}})
                variants.append({'key': f'close_stack_{i}', 'candidate_close': close_refs, 'allowed_regimes': m6.get('allowed_regimes')})
        for v in variants:
            if 'key' not in v:
                raise ValueError("each m6 candidate_close_variants[] entry needs 'key'")
            entries.append(_exit_ab_entry(f"m6.{v['key']}", v, m6))
    seen = set()
    for e in entries:
        if e['key'] in seen:
            raise ValueError(f"duplicate candidate key: {e['key']}")
        seen.add(e['key'])
    return entries

def _csv(items) -> str:
    return ','.join(items)

def m1_argv_tail(candidate_path, registry, windows, datasets, out_json) -> list:
    tail = ['--candidate-json', candidate_path, '--registry', registry, '--windows', _csv(windows), '--json', out_json]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    return tail

def noise_argv_tail(strategy, params_json, registry, direction, windows, datasets, resamples, seed, alpha, out_json) -> list:
    tail = ['--strategy', strategy, '--registry', registry, '--windows', _csv(windows), '--resamples', str(resamples), '--seed', str(seed), '--alpha', str(alpha), '--json', out_json]
    if params_json:
        tail += ['--params', params_json]
    if direction:
        tail += ['--direction', direction]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    return tail

def m3_argv_tail(strategy, params_json, registry, direction, close_json, windows, datasets, out_json) -> list:
    tail = ['--strategy', strategy, '--registry', registry, '--windows', _csv(windows), '--json', out_json]
    if params_json:
        tail += ['--params', params_json]
    if direction:
        tail += ['--direction', direction]
    if close_json:
        tail += ['--close-strategies', close_json]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    return tail

def m5_argv_tail(strategy, registry, direction, windows, datasets, out_json) -> list:
    tail = ['--strategies', strategy, '--registry', registry, '--windows', _csv(windows), '--json', out_json]
    if direction:
        tail += ['--direction', direction]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    return tail

def mc_argv_tail(candidate_path, registry, windows, datasets, n_paths, seed, mc: dict, out_json) -> list:
    tail = ['--candidate-json', candidate_path, '--registry', registry, '--windows', _csv(windows), '--n-paths', str(n_paths), '--seed', str(seed), '--json', out_json]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    mc = mc or {}
    if mc.get('config'):
        tail += ['--config', mc['config'], '--strategy-id', mc['strategy_id']]
    elif mc.get('kill_switch_pct') is not None:
        tail += ['--kill-switch-pct', str(mc['kill_switch_pct'])]
    return tail

def m6_argv_tail(m6_candidate, registry, windows, datasets, resamples, seed, out_json) -> list:
    tail = ['--strategy', m6_candidate['strategy_id'], '--registry', registry, '--candidate-close', json.dumps(m6_candidate['candidate_close']), '--candidate-stops', m6_candidate.get('candidate_stops', 'inherit'), '--windows', _csv(windows), '--bootstrap-resamples', str(resamples), '--seed', str(seed), '--json', out_json]
    if m6_candidate.get('baseline_config'):
        tail += ['--baseline-config', m6_candidate['baseline_config']]
    elif m6_candidate.get('incumbent_close'):
        tail += ['--incumbent-close', json.dumps(m6_candidate['incumbent_close'])]
    for label in m6_candidate.get('allowed_regimes') or []:
        tail += ['--allowed-regimes', label]
    if datasets:
        tail += ['--datasets', _csv(datasets)]
    return tail

def extract_m1(payload: dict) -> dict:
    out = {}
    for score in payload.get('window_scores') or []:
        w = score.get('window')
        if w is None:
            continue
        out[w] = {'verdict': score.get('verdict'), 'mean_sharpe': score.get('mean_sharpe'), 'mean_ddadj': score.get('mean_ddadj')}
    return out

def extract_noise(payload: dict) -> dict:
    tl = payload.get('trade_level') or {}
    perm = tl.get('permutation') or {}
    summary = tl.get('summary') or {}
    return {'verdict': tl.get('verdict'), 'permutation_p': perm.get('p_value'), 'mean': perm.get('mean'), 'n': summary.get('n')}

def m6_window_rollup(payload: dict) -> dict:
    out = {}
    for wname, results in (payload.get('results') or {}).items():
        deltas, per_dataset = ([], [])
        votes_pos = votes_neg = n_paired = 0
        for d in results or []:
            t = d.get('per_regime')
            if not t or t.get('all', {}).get('paired_delta', {}).get('mean') is None:
                continue
            mean = t['all']['paired_delta']['mean']
            n = t['all']['n']
            p = t['all']['paired_delta'].get('signed_rank', {}).get('p_value')
            deltas.append((mean, n))
            n_paired += n
            per_dataset.append({'dataset': d.get('dataset'), 'mean': mean, 'n': n, 'p': p})
            if mean > 0:
                votes_pos += 1
            elif mean < 0:
                votes_neg += 1
        total_n = sum((n for _, n in deltas))
        pooled = round(sum((m * n for m, n in deltas)) / total_n, 4) if total_n else None
        out[wname] = {'paired_n': n_paired, 'pooled_delta_net_pct_per_entry': pooled, 'datasets_delta_pos': votes_pos, 'datasets_delta_neg': votes_neg, 'per_dataset': per_dataset}
    return out

def extract_m3(payload: dict) -> dict:
    out = {}
    for wname, per_ds in (payload.get('windows') or {}).items():
        out[wname] = {}
        for ds, diag in (per_ds or {}).items():
            if not diag:
                continue
            out[wname][ds] = {'bleed_modes': diag.get('bleed_modes'), 'fee_churn': diag.get('fee_churn')}
    return out

def _p95_max_dd(block: dict):
    return (block.get('max_dd_pct_percentiles') or {}).get('p95')

def _worst(values: list):
    present = [v for v in values if v is not None]
    return max(present) if present else None

def extract_mc(payload: dict) -> dict:
    out = {}
    for leg in payload.get('legs') or []:
        w = leg.get('window')
        if w is None:
            continue
        bucket = out.setdefault(w, {'per_dataset': {}, 'worst': {}})
        schemes = {}
        for b in leg.get('schemes') or []:
            schemes[b.get('scheme')] = {'p_dd_ge_kill_switch': b.get('p_dd_ge_kill_switch'), 'p95_max_dd': _p95_max_dd(b), 'p_final_below_start': b.get('p_final_below_start')}
        bucket['per_dataset'][leg.get('dataset')] = {'status': leg.get('status'), 'n_trades': leg.get('n_trades'), 'schemes': schemes}
    for bucket in out.values():
        per_ds = list(bucket['per_dataset'].values())
        scheme_names = {s for d in per_ds for s in d['schemes'] or {}}
        for scheme in sorted(scheme_names):
            rows = [d['schemes'].get(scheme) or {} for d in per_ds]
            bucket['worst'][scheme] = {k: _worst([r.get(k) for r in rows]) for k in ('p_dd_ge_kill_switch', 'p95_max_dd', 'p_final_below_start')}
    return out

def extract_m5(payload: dict, strategy: str) -> dict:
    for row in payload.get('rows') or []:
        if row.get('strategy') == strategy:
            return {'salvage_verdict': row.get('verdict'), 'fee_drag_pp': row.get('fee_drag_pp'), 'trades_per_year': row.get('trades_per_year'), 'mean_gross_ret': row.get('mean_gross_ret'), 'mean_net_ret': row.get('mean_net_ret')}
    return {}

def collect_family_pvalues(entries: list) -> list:
    tests = []
    seen_noise = set()
    for e in entries:
        r = e.get('results') or {}
        noise = (r.get('m1_noise') or {}).get('data')
        if noise is not None:
            fam = e.get('noise_family_key')
            if fam not in seen_noise:
                seen_noise.add(fam)
                p = noise.get('permutation_p')
                if p is not None:
                    tests.append({'candidate_key': e['key'], 'harness': 'm1_noise', 'noise_family_key': fam, 'window': None, 'dataset': None, 'p': float(p), 'effect_positive': (noise.get('mean') or 0) > 0})
        m6 = (r.get('m6') or {}).get('data')
        if m6 is not None:
            for wname, roll in sorted(m6.items()):
                for d in roll.get('per_dataset') or []:
                    if d.get('p') is None:
                        continue
                    tests.append({'candidate_key': e['key'], 'harness': 'm6', 'window': wname, 'dataset': d.get('dataset'), 'p': float(d['p']), 'effect_positive': (d.get('mean') or 0) > 0})
    return tests

def apply_family_correction(tests: list, alpha: float=0.05, family_size: int | None=None) -> dict:
    pvals = [t['p'] for t in tests]
    if family_size is not None and family_size < len(pvals):
        raise ValueError(f'correction.family_size={family_size} is smaller than the {len(pvals)} primary p-values this run produced; the searched family size must cover every test in the family (#1338)')
    mask = benjamini_hochberg(pvals, alpha, family_size=family_size)
    for t, passed in zip(tests, mask):
        t['bh_pass'] = bool(passed)
    passing = [t['p'] for t, ok in zip(tests, mask) if ok]
    tests_run = len(tests)
    m = family_size if family_size is not None else tests_run
    return {'method': 'benjamini_hochberg', 'alpha': alpha, 'm': m, 'tests_run': tests_run, 'effective_threshold': max(passing) if passing else None, 'bonferroni_threshold': alpha / m if m else None, 'n_survivors': sum(mask)}

def _tests_for(entry, tests: list) -> list:
    return [t for t in tests if t['candidate_key'] == entry['key']]

def gate_relevant_results(entry: dict) -> dict:
    return {h: r for h, r in (entry.get('results') or {}).items() if h not in ADVISORY_HARNESSES}

def any_gate_failure(entries: list) -> bool:
    return any(((v or {}).get('status') == 'failed' for e in entries for v in gate_relevant_results(e).values()))

def advisory_failures(entry: dict) -> list:
    return sorted((h for h, r in (entry.get('results') or {}).items() if h in ADVISORY_HARNESSES and (r or {}).get('status') == 'failed'))

def candidate_verdict(entry: dict, tests: list) -> str:
    if entry.get('precondition_errors'):
        return 'excluded_not_replayable'
    r = gate_relevant_results(entry)
    if any(((v or {}).get('status') == 'failed' for v in r.values())):
        return 'run_failed'
    my_tests = _tests_for(entry, tests)
    fam = entry.get('noise_family_key')
    noise_t = next((t for t in tests if t['harness'] == 'm1_noise' and t.get('noise_family_key') == fam), None)
    if entry['kind'] == 'open':
        noise = (r.get('m1_noise') or {}).get('data')
        if noise is not None and noise.get('verdict') == 'no_positive_edge':
            return 'noise_gate_blocked'
        m1 = (r.get('m1') or {}).get('data')
        if m1 is None:
            if noise is None:
                return 'inconclusive'
            if noise.get('verdict') == 'distinguishable_positive':
                if noise_t and (not noise_t.get('bh_pass')):
                    return 'positive_uncorrected_only'
                return 'survivor'
            return 'incumbent_stands'
        rollup = m1
        protocol = [w for w in ('is', 'oos') if w in rollup]
        if not protocol:
            return 'inconclusive'
        if not all((rollup[w].get('verdict') == 'pass' for w in protocol)):
            return 'incumbent_stands'
        if noise_t and (not noise_t.get('bh_pass')):
            return 'positive_uncorrected_only'
        return 'survivor'
    m6 = (r.get('m6') or {}).get('data')
    if m6 is None:
        return 'inconclusive'
    is_w, oos_w = (m6.get('is') or {}, m6.get('oos') or {})
    pooled_is = is_w.get('pooled_delta_net_pct_per_entry')
    pooled_oos = oos_w.get('pooled_delta_net_pct_per_entry')
    if pooled_is is None or pooled_oos is None:
        return 'inconclusive'
    if not (pooled_is > 0 and pooled_oos > 0):
        return 'incumbent_stands'
    pos = [t for t in my_tests if t['harness'] == 'm6' and t['effect_positive']]
    neg = [t for t in my_tests if t['harness'] == 'm6' and (not t['effect_positive'])]
    if any((t['p'] < 0.05 for t in neg)):
        return 'incumbent_stands'
    if any((t.get('bh_pass') for t in pos)):
        return 'survivor'
    if any((t['p'] < 0.05 for t in pos)):
        return 'positive_uncorrected_only'
    return 'positive_but_not_significant'

def _rank_score(entry: dict) -> float:
    r = entry.get('results') or {}
    m6 = (r.get('m6') or {}).get('data')
    if m6:
        oos = (m6.get('oos') or {}).get('pooled_delta_net_pct_per_entry')
        return oos if oos is not None else float('-inf')
    m1 = (r.get('m1') or {}).get('data')
    if m1:
        s = m1.get('oos', {}).get('mean_sharpe')
        return s if s is not None else float('-inf')
    return float('-inf')

def rank_shortlist(entries: list) -> list:
    order = {v: i for i, v in enumerate(VERDICT_ORDER)}
    return sorted(entries, key=lambda e: (order.get(e.get('verdict'), len(VERDICT_ORDER)), -_rank_score(e), e['key']))

def reproduction_command(entry: dict) -> list:
    cmds = []
    for harness, run in (entry.get('results') or {}).items():
        tail = run.get('argv_tail')
        if not tail:
            continue
        rel = HARNESS_REL[harness]
        cmds.append('uv run --no-sync python ' + rel + ' ' + ' '.join((shlex.quote(str(a)) for a in tail)))
    return cmds

def _mc_column_window(mc: dict):

    def pick(windows):
        return 'oos' if 'oos' in windows else sorted(windows)[0] if windows else None
    resampled = {w for w, b in mc.items() if MC_SCHEME_FOR_COLUMNS in (b.get('worst') or {})}
    usable = {w for w in resampled if any((v is not None for v in mc[w]['worst'][MC_SCHEME_FOR_COLUMNS].values()))}
    return pick(usable) or pick(resampled)

def _mc_segment(mc: dict) -> str:
    if not mc:
        return ''
    window = _mc_column_window(mc)
    if window is None:
        return ''
    stats = mc[window]['worst'][MC_SCHEME_FOR_COLUMNS]

    def _f(key, prec):
        v = stats.get(key)
        return '-' if v is None else format(v, f'.{prec}f')
    return f"  MC(adv,{window})=p95DD {_f('p95_max_dd', 1)}% pKS {_f('p_dd_ge_kill_switch', 3)} pDown {_f('p_final_below_start', 3)}"

def format_shortlist(report: dict) -> str:
    corr = report['correction']
    lines = [f"== auto-suggest shortlist: {report['study']} =="]
    if report.get('exploratory'):
        lines.append(f"*** EXPLORATORY — correction family incomplete (ran {report['ran']} of {report['total']} candidates; the committed artifact must come from a full-spec run) ***")
    thr = corr.get('effective_threshold')
    tests_run = corr.get('tests_run', corr['m'])
    m_note = f"m={corr['m']} (searched family; {tests_run} tested)" if tests_run != corr['m'] else f"m={corr['m']}"
    lines.append(f"correction: {corr['method']} alpha={corr['alpha']} over {m_note} pooled p-values; effective threshold {('p<=' + format(thr, '.4g') if thr is not None else 'none pass')} (Bonferroni {(format(corr['bonferroni_threshold'], '.4g') if corr['bonferroni_threshold'] else 'n/a')}); {corr['n_survivors']} test(s) survive.")
    lines.append('')
    for i, e in enumerate(report['ranked'], 1):
        r = e.get('results') or {}
        extra = ''
        m6 = (r.get('m6') or {}).get('data')
        if m6:
            pis = (m6.get('is') or {}).get('pooled_delta_net_pct_per_entry')
            poos = (m6.get('oos') or {}).get('pooled_delta_net_pct_per_entry')
            extra = f'  pooledΔ/e is={pis} oos={poos}'
        m1 = (r.get('m1') or {}).get('data')
        if m1:
            v = {w: s.get('verdict') for w, s in m1.items()}
            extra += f'  M1={v}'
        m5 = (r.get('m5') or {}).get('data')
        if m5:
            extra += f"  M5(ctx)={m5.get('salvage_verdict')}"
        extra += _mc_segment((r.get('mc') or {}).get('data'))
        limn = ' [' + ','.join(e['limitations']) + ']' if e.get('limitations') else ''
        lines.append(f"{i:>2}  {e['key']:<40} {e['verdict']:<26}{extra}{limn}")
    lines.append('')
    lines.append('M3/M5/MC figures are UNCORRECTED CONTEXT (no p-values), never counted as significance evidence.')
    lines.append(f"MC(adv) = trade-order Monte Carlo (#1274), {MC_SCHEME_FOR_COLUMNS} scheme, WORST dataset in the window: p95DD = P95 max drawdown, pKS = P(max DD >= kill switch), pDown = P(final < start). Advisory only — it does not gate promotion, and a failed MC run leaves the verdict untouched (flagged 'mc_run_failed'). Open candidates only; M6 exit-A/B entries carry no MC column.")
    lines.append(FOOTER)
    return '\n'.join(lines)

def _direction_for(candidate: dict):
    d = str(candidate.get('direction') or '').strip().lower()
    return d if d in ('long', 'short') else None

def _noise_family_key(candidate: dict) -> str:
    return json.dumps({'name': candidate.get('name'), 'params': candidate.get('params'), 'direction': _direction_for(candidate)}, sort_keys=True)

def _m5_family_key(candidate: dict) -> str:
    return json.dumps({'name': candidate.get('name'), 'direction': _direction_for(candidate)}, sort_keys=True)

def _run_harness(harness: str, tail: list, out_json: str) -> dict:
    argv = [sys.executable, HARNESS_ABS[harness]] + tail
    proc = subprocess.run(argv, capture_output=True, text=True)
    ok = proc.returncode == 0 and os.path.exists(out_json)
    run = {'harness': harness, 'argv_tail': tail, 'status': 'ok' if ok else 'failed'}
    if ok:
        with open(out_json) as fh:
            run['payload'] = json.load(fh)
    else:
        sys.stderr.write(f'[{harness}] FAILED rc={proc.returncode}\n{proc.stdout[-1500:]}\n{proc.stderr[-1500:]}\n')
    return run

def ensure_noise(entry: dict, spec: dict, out_dir: str, noise_cache: dict) -> None:
    cand = entry['candidate']
    entry['noise_family_key'] = _noise_family_key(cand)
    if 'm1_noise' not in entry['harnesses']:
        return
    fam = entry['noise_family_key']
    if fam not in noise_cache:
        out = os.path.join(out_dir, f"{entry['key']}.noise.json")
        tail = noise_argv_tail(cand['name'], json.dumps(cand['params']) if cand.get('params') else None, spec['registry'], _direction_for(cand), spec['windows'], spec['datasets'], spec['resamples'], spec['seed'], spec['correction']['alpha'], out)
        run = _run_harness('m1_noise', tail, out)
        if run['status'] == 'ok':
            run['data'] = extract_noise(run.pop('payload'))
        noise_cache[fam] = run

def ensure_m5(entry: dict, spec: dict, out_dir: str, m5_cache: dict) -> None:
    cand = entry['candidate']
    if 'm5' not in entry['harnesses']:
        return
    fam = _m5_family_key(cand)
    if fam not in m5_cache:
        direction = _direction_for(cand)
        out = os.path.join(out_dir, f"m5.{cand['name']}.{direction or 'long'}.json")
        tail = m5_argv_tail(cand['name'], spec['registry'], direction, spec['windows'], spec['datasets'], out)
        run = _run_harness('m5', tail, out)
        if run['status'] == 'ok':
            run['data'] = extract_m5(run.pop('payload'), cand['name'])
        m5_cache[fam] = run

def run_open_entry(entry: dict, spec: dict, out_dir: str, noise_cache: dict, m5_cache: dict) -> dict:
    cand = entry['candidate']
    reg, windows, datasets = (spec['registry'], spec['windows'], spec['datasets'])
    key = entry['key']
    results = {}
    ensure_noise(entry, spec, out_dir, noise_cache)
    if 'm1_noise' in entry['harnesses']:
        results['m1_noise'] = noise_cache[entry['noise_family_key']]
    written = []

    def candidate_path() -> str:
        path = os.path.join(out_dir, f'{key}.candidate.json')
        if not written:
            with open(path, 'w') as fh:
                json.dump(cand, fh, indent=2)
            written.append(path)
        return path
    if 'm1' in entry['harnesses']:
        cand_path = candidate_path()
        out = os.path.join(out_dir, f'{key}.m1.json')
        run = _run_harness('m1', m1_argv_tail(cand_path, reg, windows, datasets, out), out)
        if run['status'] == 'ok':
            run['data'] = extract_m1(run.pop('payload'))
        results['m1'] = run
    if 'm3' in entry['harnesses']:
        out = os.path.join(out_dir, f'{key}.m3.json')
        close_json = json.dumps(cand['close_strategies']) if cand.get('close_strategies') else None
        tail = m3_argv_tail(cand['name'], json.dumps(cand['params']) if cand.get('params') else None, reg, _direction_for(cand), close_json, windows, datasets, out)
        run = _run_harness('m3', tail, out)
        if run['status'] == 'ok':
            run['data'] = extract_m3(run.pop('payload'))
        results['m3'] = run
    if 'm5' in entry['harnesses']:
        results['m5'] = m5_cache[_m5_family_key(cand)]
    if 'mc' in entry['harnesses']:
        out = os.path.join(out_dir, f'{key}.mc.json')
        mc = spec.get('mc') or {}
        tail = mc_argv_tail(candidate_path(), reg, windows, datasets, mc.get('n_paths') or MC_DEFAULT_N_PATHS, spec['seed'], mc, out)
        run = _run_harness('mc', tail, out)
        if run['status'] == 'ok':
            run['data'] = extract_mc(run.pop('payload'))
        results['mc'] = run
    entry['results'] = results
    for h in advisory_failures(entry):
        entry['limitations'].append(f'{h}_run_failed')
    return entry

def run_exit_ab_entry(entry: dict, spec: dict, out_dir: str) -> dict:
    if entry['precondition_errors']:
        entry['results'] = {}
        return entry
    out = os.path.join(out_dir, f"{entry['key']}.m6.json")
    tail = m6_argv_tail(entry['candidate'], spec['registry'], spec['windows'], spec['datasets'], spec['resamples'], spec['seed'], out)
    run = _run_harness('m6', tail, out)
    if run['status'] == 'ok':
        run['data'] = m6_window_rollup(run.pop('payload'))
    entry['results'] = {'m6': run}
    return entry

def _cmd(harness: str, tail: list) -> str:
    return 'uv run --no-sync python ' + HARNESS_REL[harness] + ' ' + ' '.join((shlex.quote(str(a)) for a in tail))

def _dry_run_commands(entries: list, spec: dict, out_dir: str) -> list:
    cmds = []
    for e in entries:
        reg, windows, datasets = (spec['registry'], spec['windows'], spec['datasets'])
        if e['precondition_errors']:
            cmds.append(f"# {e['key']}: SKIP ({','.join(e['precondition_errors'])})")
            continue
        if e['kind'] == 'open':
            cand = e['candidate']
            key, direction = (e['key'], _direction_for(cand))
            params_json = json.dumps(cand['params']) if cand.get('params') else None
            cand_path = os.path.join(out_dir, f'{key}.candidate.json')
            if 'm1_noise' in e['harnesses']:
                cmds.append(_cmd('m1_noise', noise_argv_tail(cand['name'], params_json, reg, direction, windows, datasets, spec['resamples'], spec['seed'], spec['correction']['alpha'], os.path.join(out_dir, f'{key}.noise.json'))))
            if 'm1' in e['harnesses']:
                cmds.append(_cmd('m1', m1_argv_tail(cand_path, reg, windows, datasets, os.path.join(out_dir, f'{key}.m1.json'))))
            if 'm3' in e['harnesses']:
                close_json = json.dumps(cand['close_strategies']) if cand.get('close_strategies') else None
                cmds.append(_cmd('m3', m3_argv_tail(cand['name'], params_json, reg, direction, close_json, windows, datasets, os.path.join(out_dir, f'{key}.m3.json'))))
            if 'm5' in e['harnesses']:
                cmds.append(_cmd('m5', m5_argv_tail(cand['name'], reg, direction, windows, datasets, os.path.join(out_dir, f"m5.{cand['name']}.{direction or 'long'}.json"))))
            if 'mc' in e['harnesses']:
                mc = spec.get('mc') or {}
                cmds.append(_cmd('mc', mc_argv_tail(cand_path, reg, windows, datasets, mc.get('n_paths') or MC_DEFAULT_N_PATHS, spec['seed'], mc, os.path.join(out_dir, f'{key}.mc.json'))))
        else:
            cmds.append(_cmd('m6', m6_argv_tail(e['candidate'], reg, windows, datasets, spec['resamples'], spec['seed'], os.path.join(out_dir, f"{e['key']}.m6.json"))))
    return cmds

def _serializable(entry: dict) -> dict:
    out = {k: entry[k] for k in ('key', 'kind', 'hypothesis', 'verdict', 'limitations', 'precondition_errors')}
    out['candidate'] = entry['candidate']
    out['evidence'] = {h: {'status': r.get('status'), 'data': r.get('data')} for h, r in (entry.get('results') or {}).items()}
    out['reproduce'] = reproduction_command(entry)
    return out

def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser()
    p.add_argument('--spec', required=True, help='Path to a suggest.json spec')
    p.add_argument('--jobs', type=int, default=4)
    p.add_argument('--out-dir', default=None, help='Per-run harness JSON dir (default: <spec_dir>/<study>_runs)')
    p.add_argument('--only', default=None, help='Comma list of candidate keys (stamps the report EXPLORATORY — the correction is only valid over the full family)')
    p.add_argument('--windows', default=None, help='Override spec windows')
    p.add_argument('--datasets', default=None, help='Override spec datasets (comma list SYMBOL:TIMEFRAME)')
    p.add_argument('--alpha', type=float, default=None, help='Override correction alpha')
    p.add_argument('--seed', type=int, default=1066)
    p.add_argument('--bootstrap-resamples', type=int, default=10000)
    p.add_argument('--dry-run', action='store_true', help='Print every planned harness command; run nothing')
    p.add_argument('--json', default=None, dest='json_out', help='Write the artifact')
    p.add_argument('--markdown', default=None, dest='markdown_out')
    return p

def main(argv=None) -> int:
    args = build_parser().parse_args(argv)
    spec_path = os.path.abspath(args.spec)
    with open(spec_path) as fh:
        raw = json.load(fh)
    spec = load_spec(raw, os.path.dirname(spec_path))
    if args.windows:
        spec['windows'] = [w.strip() for w in args.windows.split(',') if w.strip()]
        bad = [w for w in spec['windows'] if w not in M1_WINDOWS]
        if bad:
            raise SystemExit(f'unknown windows {bad}; known: {list(M1_WINDOWS)}')
    if args.datasets is not None:
        spec['datasets'] = [d.strip() for d in args.datasets.split(',') if d.strip()]
    if args.alpha is not None:
        spec['correction']['alpha'] = args.alpha
    spec['seed'] = args.seed
    spec['resamples'] = args.bootstrap_resamples
    entries = expand_candidates(spec)
    total = len(entries)
    exploratory = False
    if args.only:
        keys = {k.strip() for k in args.only.split(',') if k.strip()}
        unknown = keys - {e['key'] for e in entries}
        if unknown:
            raise SystemExit(f'unknown candidate keys: {sorted(unknown)}')
        entries = [e for e in entries if e['key'] in keys]
        exploratory = len(entries) < total
    out_dir = args.out_dir or os.path.join(spec['spec_dir'], f"{spec['study']}_runs")
    if args.dry_run:
        for cmd in _dry_run_commands(entries, spec, out_dir):
            print(cmd)
        print('\n# dry-run: nothing executed. ' + FOOTER)
        return 0
    os.makedirs(out_dir, exist_ok=True)
    noise_cache, m5_cache = ({}, {})
    open_entries = [e for e in entries if e['kind'] == 'open']
    ab_entries = [e for e in entries if e['kind'] == 'exit_ab']
    for e in open_entries:
        ensure_noise(e, spec, out_dir, noise_cache)
        ensure_m5(e, spec, out_dir, m5_cache)
    with ThreadPoolExecutor(max_workers=max(1, args.jobs)) as ex:
        list(ex.map(lambda e: run_open_entry(e, spec, out_dir, noise_cache, m5_cache), open_entries))
        list(ex.map(lambda e: run_exit_ab_entry(e, spec, out_dir), ab_entries))
    tests = collect_family_pvalues(entries)
    correction = apply_family_correction(tests, spec['correction']['alpha'], family_size=spec['correction'].get('family_size'))
    for e in entries:
        e['verdict'] = candidate_verdict(e, tests)
    ranked = rank_shortlist(entries)
    report = {'study': spec['study'], 'issue': 1210, 'registry': spec['registry'], 'windows': spec['windows'], 'correction': correction, 'family_tests': tests, 'exploratory': exploratory, 'ran': len(entries), 'total': total, 'ranked': [_serializable(e) for e in ranked], 'note': FOOTER}
    text = format_shortlist({**report, 'ranked': ranked})
    print(text)
    if args.json_out:
        with open(args.json_out, 'w') as fh:
            json.dump(report, fh, indent=2, default=str)
        print(f'\nwrote {args.json_out}')
    if args.markdown_out:
        with open(args.markdown_out, 'w') as fh:
            fh.write('```\n' + text + '\n```\n')
        print(f'wrote {args.markdown_out}')
    return 1 if any_gate_failure(entries) else 0
if __name__ == '__main__':
    raise SystemExit(main())
