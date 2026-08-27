import argparse
import json
import os
import platform
import resource
import statistics
import subprocess
import sys
import tempfile
import time
REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
CHECK_SCRIPT = os.path.join('shared_scripts', 'check_hyperliquid.py')
PYTHON = os.path.join('.venv', 'bin', 'python3')
DEFAULT_STRATEGIES = ['breakout', 'momentum_pro', 'mean_reversion_pro', 'rsi_bb_combo']
SYMBOL = 'BTC'
TIMEFRAME = '1h'
OHLCV_LIMIT = 200
ATR_METHOD = 'simple'
MARK_PRICE = '25000'

def build_fixture(path: str, bars: int=200) -> str:
    candles = []
    price = 25000.0
    start_ms = 1700000000000
    for i in range(bars):
        price += 12.0 if i % 5 else -37.0
        candles.append([start_ms + i * 3600000, price - 4.0, price + 30.0, price - 28.0, price, 1000.0 + i])
    with open(path, 'w') as fh:
        json.dump(candles, fh)
    return path
SITECUSTOMIZE = '\nimport json\nimport os\nimport sys\n\n_repo = os.environ["GO_TRADER_BENCH_REPO"]\n_fixture = os.environ["GO_TRADER_BENCH_FIXTURE"]\n\nwith open(_fixture, "r") as _fh:\n    _CANDLES = json.load(_fh)\n\nsys.path.insert(0, os.path.join(_repo, "platforms", "hyperliquid"))\nimport adapter as _adapter\n\n\ndef _pinned_get_ohlcv(self, symbol, interval="1h", limit=200):\n    return _CANDLES[-limit:] if limit and limit > 0 else list(_CANDLES)\n\n\n_adapter.HyperliquidExchangeAdapter.get_ohlcv = _pinned_get_ohlcv\nprint("[bench] candle fixture injected", file=sys.stderr)\n'

def _write_sitecustomize(dir_path: str) -> str:
    path = os.path.join(dir_path, 'sitecustomize.py')
    with open(path, 'w') as fh:
        fh.write(SITECUSTOMIZE)
    return path

def _bench_env(fixture: str, inject_dir: str) -> dict:
    env = dict(os.environ)
    env['GO_TRADER_BENCH_REPO'] = REPO_ROOT
    env['GO_TRADER_BENCH_FIXTURE'] = fixture
    existing = env.get('PYTHONPATH', '')
    env['PYTHONPATH'] = inject_dir + (os.pathsep + existing if existing else '')
    env['GO_TRADER_HL_OHLCV_CACHE'] = '0'
    return env

def _verify_injection(env: dict) -> None:
    proc = subprocess.run([PYTHON, CHECK_SCRIPT] + _strategy_argv(DEFAULT_STRATEGIES[0]), cwd=REPO_ROOT, env=env, capture_output=True, check=False)
    stderr = proc.stderr.decode(errors='replace')
    if '[bench] candle fixture injected' not in stderr:
        raise SystemExit('candle fixture injection did not take effect; refusing to publish a network-bound benchmark.\nchild stderr:\n' + stderr)
    try:
        payload = json.loads(proc.stdout.decode())
    except ValueError:
        raise SystemExit('preflight child produced no JSON:\n' + stderr)
    if payload.get('error'):
        raise SystemExit('preflight child errored: %s' % payload['error'])

def _verify_batched_arm(env: dict) -> None:
    strategies = _workload(2)
    proc = subprocess.run([PYTHON, CHECK_SCRIPT] + _batch_argv(), cwd=REPO_ROOT, env=env, input=_batch_stdin(strategies).encode(), capture_output=True, check=False)
    stderr = proc.stderr.decode(errors='replace')
    if proc.returncode != 0:
        raise SystemExit('batched preflight child exited %d:\n%s' % (proc.returncode, stderr))
    try:
        payload = json.loads(proc.stdout.decode())
    except ValueError:
        raise SystemExit('batched preflight child produced no JSON:\n' + stderr)
    if payload.get('error'):
        raise SystemExit('batched preflight child errored: %s' % payload['error'])
    results = payload.get('results')
    if not isinstance(results, list) or len(results) != len(strategies):
        raise SystemExit('batched preflight returned %r results for %d slots; refusing to publish a batched timing.\n%s' % (len(results) if isinstance(results, list) else results, len(strategies), stderr))
    for slot in results:
        if slot.get('error'):
            raise SystemExit('batched preflight slot %s errored: %s' % (slot.get('id'), slot['error']))

def _maxrss_mb(maxrss: int) -> float:
    divisor = 1024.0 * 1024.0 if sys.platform == 'darwin' else 1024.0
    return round(maxrss / divisor, 1)

def _child_usage():
    ru = resource.getrusage(resource.RUSAGE_CHILDREN)
    return (ru.ru_utime + ru.ru_stime, ru.ru_maxrss)

def _run(args, stdin_text=None, env=None):
    proc = subprocess.run([PYTHON, CHECK_SCRIPT] + args, cwd=REPO_ROOT, input=stdin_text.encode() if stdin_text is not None else None, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=env, check=False)
    if proc.returncode != 0:
        raise SystemExit('benchmark child exited %d (argv: %s); refusing to publish a timing for work that did not complete.' % (proc.returncode, ' '.join(args)))

def _strategy_argv(strategy: str) -> list:
    refs = json.dumps({'open': {'name': strategy, 'params': {}}})
    return [strategy, SYMBOL, TIMEFRAME, '--mode=paper', '--ohlcv-limit', str(OHLCV_LIMIT), '--atr-method=' + ATR_METHOD, '--mark-price=' + MARK_PRICE, '--strategy-refs', refs]

def _batch_argv() -> list:
    return ['--batch-check', '--symbol=' + SYMBOL, '--timeframe=' + TIMEFRAME, '--ohlcv-limit', str(OHLCV_LIMIT), '--atr-method=' + ATR_METHOD, '--mark-price=' + MARK_PRICE]

def _batch_stdin(strategies: list) -> str:
    slots = []
    for i, strategy in enumerate(strategies):
        slots.append({'id': f'bench-{i}', 'strategy': strategy, 'mode': 'paper', 'strategy_refs': {'open': {'name': strategy, 'params': {}}}})
    return json.dumps({'v': 1, 'slots': slots})

def _workload(n: int) -> list:
    return [DEFAULT_STRATEGIES[i % len(DEFAULT_STRATEGIES)] for i in range(n)]

def measure(arm: str, strategies: list, env: dict) -> dict:
    cpu0, _ = _child_usage()
    started = time.perf_counter()
    if arm == 'unbatched':
        for strategy in strategies:
            _run(_strategy_argv(strategy), env=env)
        starts = len(strategies)
    else:
        _run(_batch_argv(), stdin_text=_batch_stdin(strategies), env=env)
        starts = 1
    wall = time.perf_counter() - started
    cpu1, maxrss = _child_usage()
    return {'wall_s': wall, 'cpu_s': cpu1 - cpu0, 'process_starts': starts, 'peak_child_rss_mb': _maxrss_mb(maxrss)}

def summarize(records: list) -> dict:
    walls = sorted((r['wall_s'] for r in records))
    cpus = [r['cpu_s'] for r in records]
    p95_index = min(len(walls) - 1, int(round(0.95 * (len(walls) - 1))))
    return {'reps': len(records), 'wall_median_s': round(statistics.median(walls), 4), 'wall_p95_s': round(walls[p95_index], 4), 'cpu_median_s': round(statistics.median(cpus), 4), 'process_starts': records[0]['process_starts'], 'peak_child_rss_mb': max((r['peak_child_rss_mb'] for r in records))}

def main(argv=None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--sizes', default='2,5,10,20', help='comma-separated group sizes to measure')
    parser.add_argument('--reps', type=int, default=10)
    parser.add_argument('--warmups', type=int, default=2)
    parser.add_argument('--fixture', default=os.path.join(REPO_ROOT, 'docs', 'benchmarks', 'hl_batch_candles.json'), help='candle fixture path; generated when absent')
    parser.add_argument('--json', default=None, help='write the raw records here')
    args = parser.parse_args(argv)
    if not os.path.exists(os.path.join(REPO_ROOT, PYTHON)):
        print(f'missing {PYTHON} — run `uv sync` first', file=sys.stderr)
        return 2
    if not os.path.exists(args.fixture):
        build_fixture(args.fixture)
    inject_dir = tempfile.mkdtemp(prefix='hl_bench_inject_')
    _write_sitecustomize(inject_dir)
    env = _bench_env(args.fixture, inject_dir)
    _verify_injection(env)
    _verify_batched_arm(env)
    host = {'platform': platform.platform(), 'processor': platform.processor() or platform.machine(), 'cpu_count': os.cpu_count(), 'python': platform.python_version()}
    out = {'host': host, 'fixture': os.path.basename(args.fixture), 'ohlcv_limit': OHLCV_LIMIT, 'results': []}
    for size in [int(s) for s in args.sizes.split(',') if s.strip()]:
        strategies = _workload(size)
        for arm in ('unbatched', 'batched'):
            for _ in range(args.warmups):
                measure(arm, strategies, env)
            records = [measure(arm, strategies, env) for _ in range(args.reps)]
            summary = summarize(records)
            summary.update({'n': size, 'arm': arm})
            out['results'].append({'summary': summary, 'records': records})
            print(f"n={size:>3} {arm:<9} wall_median={summary['wall_median_s']}s wall_p95={summary['wall_p95_s']}s cpu_median={summary['cpu_median_s']}s starts={summary['process_starts']} peak_child_rss_mb={summary['peak_child_rss_mb']}")
    if args.json:
        with open(args.json, 'w') as fh:
            json.dump(out, fh, indent=2, sort_keys=True)
        print(f'raw records written to {args.json}')
    return 0
if __name__ == '__main__':
    sys.exit(main())
