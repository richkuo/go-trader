#!/usr/bin/env python3
"""Batched vs unbatched Hyperliquid check benchmark (#1442).

Measures what the batched lane actually costs against what N sequential
per-strategy checks cost, on the SAME host, the SAME configuration and the
SAME candles. No speedup may be claimed without this artifact, and any target
stated later has to be arithmetically reachable for the group size it names.

Both arms are network-free, and the pinning lives ENTIRELY in this harness:
the child interpreters load a generated ``sitecustomize.py`` that replaces
``HyperliquidExchangeAdapter.get_ohlcv`` with a fixture reader, so no
benchmark switch exists anywhere on the trading path. ``--mark-price`` is
always supplied, so ``get_spot_price`` is never reached either. Funding-aware
strategies are excluded from the workload for the same reason. What the arms
differ in is exactly the thing under test:

  unbatched — N sequential ``check_hyperliquid.py <strategy> <symbol> <tf>``
              invocations, the shape the dispatch loop produces today.
  batched   — ONE ``check_hyperliquid.py --batch-check`` invocation carrying
              N slots on stdin.

Reported per arm and per N: wall time (median and p95 over the repetitions),
process starts, child processor time (user+system), and peak child RSS. The
raw records are printed as JSON so the artifact in docs/benchmarks/hl_batch.md
is a transcript rather than a summary.

Usage:
    uv run --no-sync python scripts/bench_hl_batch.py \
        --sizes 2,5,10,20 --reps 10 --warmups 2 --json bench.json
"""

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

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
CHECK_SCRIPT = os.path.join("shared_scripts", "check_hyperliquid.py")
PYTHON = os.path.join(".venv", "bin", "python3")

# Funding-aware strategies are excluded: they reach the network regardless of
# the candle fixture, which would measure the exchange, not the batch.
DEFAULT_STRATEGIES = ["breakout", "momentum_pro", "mean_reversion_pro", "rsi_bb_combo"]

SYMBOL = "BTC"
TIMEFRAME = "1h"
OHLCV_LIMIT = 200
ATR_METHOD = "simple"
MARK_PRICE = "25000"


def build_fixture(path: str, bars: int = 200) -> str:
    """Write a deterministic candle fixture (no network, no market data licence)."""
    candles = []
    price = 25_000.0
    start_ms = 1_700_000_000_000
    for i in range(bars):
        price += 12.0 if i % 5 else -37.0
        candles.append([
            start_ms + i * 3_600_000,
            price - 4.0, price + 30.0, price - 28.0, price, 1000.0 + i,
        ])
    with open(path, "w") as fh:
        json.dump(candles, fh)
    return path


# The child-side injection. Written into a temp directory that is prepended to
# PYTHONPATH, so CPython's `site` imports it before check_hyperliquid.py runs.
# It patches the adapter CLASS in place and pre-binds sys.modules["adapter"],
# which is the name check_hyperliquid.py imports lazily — so the patched method
# is the one the check actually calls. Nothing in the repository reads a
# benchmark environment variable; the seam exists only inside this harness.
SITECUSTOMIZE = '''
import json
import os
import sys

_repo = os.environ["GO_TRADER_BENCH_REPO"]
_fixture = os.environ["GO_TRADER_BENCH_FIXTURE"]

with open(_fixture, "r") as _fh:
    _CANDLES = json.load(_fh)

sys.path.insert(0, os.path.join(_repo, "platforms", "hyperliquid"))
import adapter as _adapter


def _pinned_get_ohlcv(self, symbol, interval="1h", limit=200):
    return _CANDLES[-limit:] if limit and limit > 0 else list(_CANDLES)


_adapter.HyperliquidExchangeAdapter.get_ohlcv = _pinned_get_ohlcv
print("[bench] candle fixture injected", file=sys.stderr)
'''


def _write_sitecustomize(dir_path: str) -> str:
    path = os.path.join(dir_path, "sitecustomize.py")
    with open(path, "w") as fh:
        fh.write(SITECUSTOMIZE)
    return path


def _bench_env(fixture: str, inject_dir: str) -> dict:
    env = dict(os.environ)
    env["GO_TRADER_BENCH_REPO"] = REPO_ROOT
    env["GO_TRADER_BENCH_FIXTURE"] = fixture
    existing = env.get("PYTHONPATH", "")
    env["PYTHONPATH"] = inject_dir + (os.pathsep + existing if existing else "")
    # Both arms read the same pinned candles, so the #839 disk cache would only
    # add noise; disable it so the measurement is of compute, not of cache luck.
    env["GO_TRADER_HL_OHLCV_CACHE"] = "0"
    return env


def _verify_injection(env: dict) -> None:
    """Fail loudly unless the child actually ran on the pinned candles.

    Without this a silently-failed injection would produce a network-bound
    benchmark that still prints plausible numbers — the artifact would be
    wrong and nothing would say so.
    """
    proc = subprocess.run(
        [PYTHON, CHECK_SCRIPT] + _strategy_argv(DEFAULT_STRATEGIES[0]),
        cwd=REPO_ROOT, env=env, capture_output=True, check=False,
    )
    stderr = proc.stderr.decode(errors="replace")
    if "[bench] candle fixture injected" not in stderr:
        raise SystemExit(
            "candle fixture injection did not take effect; refusing to publish a "
            "network-bound benchmark.\nchild stderr:\n" + stderr)
    try:
        payload = json.loads(proc.stdout.decode())
    except ValueError:
        raise SystemExit("preflight child produced no JSON:\n" + stderr)
    if payload.get("error"):
        raise SystemExit("preflight child errored: %s" % payload["error"])


def _maxrss_mb(maxrss: int) -> float:
    """Normalize ru_maxrss to MiB. Linux reports KiB, macOS reports bytes."""
    divisor = 1024.0 * 1024.0 if sys.platform == "darwin" else 1024.0
    return round(maxrss / divisor, 1)


def _child_usage():
    ru = resource.getrusage(resource.RUSAGE_CHILDREN)
    return ru.ru_utime + ru.ru_stime, ru.ru_maxrss


def _run(args, stdin_text=None, env=None):
    subprocess.run(
        [PYTHON, CHECK_SCRIPT] + args,
        cwd=REPO_ROOT,
        input=stdin_text.encode() if stdin_text is not None else None,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        env=env,
        check=False,
    )


def _strategy_argv(strategy: str) -> list:
    refs = json.dumps({"open": {"name": strategy, "params": {}}})
    return [
        strategy, SYMBOL, TIMEFRAME, "--mode=paper",
        "--ohlcv-limit", str(OHLCV_LIMIT),
        "--atr-method=" + ATR_METHOD,
        "--mark-price=" + MARK_PRICE,
        "--strategy-refs", refs,
    ]


def _batch_argv() -> list:
    return [
        "--batch-check", "--symbol=" + SYMBOL, "--timeframe=" + TIMEFRAME,
        "--ohlcv-limit", str(OHLCV_LIMIT),
        "--atr-method=" + ATR_METHOD,
        "--mark-price=" + MARK_PRICE,
    ]


def _batch_stdin(strategies: list) -> str:
    slots = []
    for i, strategy in enumerate(strategies):
        slots.append({
            "id": f"bench-{i}",
            "strategy": strategy,
            "mode": "paper",
            "strategy_refs": {"open": {"name": strategy, "params": {}}},
        })
    return json.dumps({"v": 1, "slots": slots})


def _workload(n: int) -> list:
    return [DEFAULT_STRATEGIES[i % len(DEFAULT_STRATEGIES)] for i in range(n)]


def measure(arm: str, strategies: list, env: dict) -> dict:
    cpu0, _ = _child_usage()
    started = time.perf_counter()
    if arm == "unbatched":
        for strategy in strategies:
            _run(_strategy_argv(strategy), env=env)
        starts = len(strategies)
    else:
        _run(_batch_argv(), stdin_text=_batch_stdin(strategies), env=env)
        starts = 1
    wall = time.perf_counter() - started
    cpu1, maxrss = _child_usage()
    return {"wall_s": wall, "cpu_s": cpu1 - cpu0, "process_starts": starts,
            "peak_child_rss_mb": _maxrss_mb(maxrss)}


def summarize(records: list) -> dict:
    walls = sorted(r["wall_s"] for r in records)
    cpus = [r["cpu_s"] for r in records]
    p95_index = min(len(walls) - 1, int(round(0.95 * (len(walls) - 1))))
    return {
        "reps": len(records),
        "wall_median_s": round(statistics.median(walls), 4),
        "wall_p95_s": round(walls[p95_index], 4),
        "cpu_median_s": round(statistics.median(cpus), 4),
        "process_starts": records[0]["process_starts"],
        "peak_child_rss_mb": max(r["peak_child_rss_mb"] for r in records),
    }


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sizes", default="2,5,10,20",
                        help="comma-separated group sizes to measure")
    parser.add_argument("--reps", type=int, default=10)
    parser.add_argument("--warmups", type=int, default=2)
    parser.add_argument("--fixture",
                        default=os.path.join(REPO_ROOT, "docs", "benchmarks",
                                             "hl_batch_candles.json"),
                        help="candle fixture path; generated when absent")
    parser.add_argument("--json", default=None, help="write the raw records here")
    args = parser.parse_args(argv)

    if not os.path.exists(os.path.join(REPO_ROOT, PYTHON)):
        print(f"missing {PYTHON} — run `uv sync` first", file=sys.stderr)
        return 2
    if not os.path.exists(args.fixture):
        build_fixture(args.fixture)

    inject_dir = tempfile.mkdtemp(prefix="hl_bench_inject_")
    _write_sitecustomize(inject_dir)
    env = _bench_env(args.fixture, inject_dir)
    _verify_injection(env)

    host = {
        "platform": platform.platform(),
        "processor": platform.processor() or platform.machine(),
        "cpu_count": os.cpu_count(),
        "python": platform.python_version(),
    }
    out = {"host": host, "fixture": os.path.basename(args.fixture),
           "ohlcv_limit": OHLCV_LIMIT, "results": []}

    for size in [int(s) for s in args.sizes.split(",") if s.strip()]:
        strategies = _workload(size)
        for arm in ("unbatched", "batched"):
            for _ in range(args.warmups):
                measure(arm, strategies, env)
            records = [measure(arm, strategies, env) for _ in range(args.reps)]
            summary = summarize(records)
            summary.update({"n": size, "arm": arm})
            out["results"].append({"summary": summary, "records": records})
            print(f"n={size:>3} {arm:<9} "
                  f"wall_median={summary['wall_median_s']}s "
                  f"wall_p95={summary['wall_p95_s']}s "
                  f"cpu_median={summary['cpu_median_s']}s "
                  f"starts={summary['process_starts']} "
                  f"peak_child_rss_mb={summary['peak_child_rss_mb']}")

    if args.json:
        with open(args.json, "w") as fh:
            json.dump(out, fh, indent=2, sort_keys=True)
        print(f"raw records written to {args.json}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
