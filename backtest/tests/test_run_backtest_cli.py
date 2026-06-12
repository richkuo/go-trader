"""CLI wiring: ``--platform`` and ``--registry`` must flow from argparse
through to the constructed Backtester. Without this, a future refactor
that drops ``platform=args.platform`` would silently regress to BinanceUS
fees and every existing Backtester-level test would still pass."""
import pandas as pd
import pytest

import run_backtest


def test_build_parser_accepts_platform_and_registry():
    parser = run_backtest._build_parser()
    args = parser.parse_args([
        "--registry", "futures",
        "--platform", "hyperliquid",
        "--strategy", "sma_crossover",
        "--mode", "single",
    ])
    assert args.registry == "futures"
    assert args.platform == "hyperliquid"


def test_build_parser_rejects_unknown_platform():
    parser = run_backtest._build_parser()
    with pytest.raises(SystemExit):
        parser.parse_args(["--platform", "mystery-exchange"])


def test_build_parser_rejects_unknown_registry():
    parser = run_backtest._build_parser()
    with pytest.raises(SystemExit):
        parser.parse_args(["--registry", "options"])


def test_run_single_backtest_threads_platform_to_backtester(monkeypatch):
    """Stand in for Backtester and load_cached_data so we can observe the
    platform argument that actually reaches the constructor."""
    seen = {}

    class SpyBacktester:
        def __init__(self, initial_capital, platform="binanceus", **kwargs):
            seen["platform"] = platform
            seen["capital"] = initial_capital
            self.commission_pct = 0.123  # nonsense marker

        def run(self, df, **kwargs):
            return {
                "strategy_name": kwargs.get("strategy_name"),
                "symbol": "BTC/USDT",
                "timeframe": "1d",
                "start_date": str(df.index[0]),
                "end_date": str(df.index[-1]),
                "initial_capital": 1000.0,
                "final_capital": 1000.0,
                "total_return_pct": 0.0,
                "annual_return_pct": 0.0,
                "sharpe_ratio": 0.0,
                "sortino_ratio": 0.0,
                "max_drawdown_pct": 0.0,
                "calmar_ratio": 0.0,
                "volatility_pct": 0.0,
                "win_rate": 0.0,
                "profit_factor": 0.0,
                "total_trades": 0,
                "avg_win_pct": 0.0,
                "avg_loss_pct": 0.0,
                "trades": [],
                "params": {},
            }

    df = pd.DataFrame(
        {"open": [100] * 60, "high": [101] * 60, "low": [99] * 60,
         "close": [100] * 60, "volume": [1000] * 60},
        index=pd.date_range("2024-01-01", periods=60, freq="D"),
    )
    monkeypatch.setattr(run_backtest, "Backtester", SpyBacktester)
    monkeypatch.setattr(run_backtest, "load_cached_data",
                        lambda *a, **kw: df)

    result = run_backtest.run_single_backtest(
        strategy_name="sma_crossover",
        symbol="BTC/USDT",
        timeframe="1d",
        since="2024-01-01",
        capital=777.0,
        platform="robinhood",
        registry="spot",
    )
    assert result is not None
    assert seen["platform"] == "robinhood", (
        f"platform did not thread through to Backtester — got {seen}"
    )
    assert seen["capital"] == 777.0


def test_backtester_imports_under_script_style_sys_path(tmp_path):
    """Script-style invocation (`python backtest/run_backtest.py`) puts only
    backtest/ on sys.path. Backtester.__init__ unconditionally loads
    post_tp_sl.py, whose absolute `shared_strategies.close...` import needs the
    repo root — backtester.py must insert it itself (pytest masks the gap by
    inserting the root during shared_strategies package collection)."""
    import os
    import subprocess
    import sys

    backtest_dir = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    snippet = tmp_path / "script_style_import.py"
    snippet.write_text(
        "import sys\n"
        f"sys.path.insert(0, {backtest_dir!r})\n"
        "import backtester\n"
        "backtester.Backtester()\n"
        "print('OK')\n"
    )
    proc = subprocess.run(
        [sys.executable, str(snippet)],
        cwd=tmp_path, capture_output=True, text=True, timeout=120,
    )
    assert proc.returncode == 0, proc.stderr
    assert "OK" in proc.stdout


def test_build_parser_accepts_close_stack_flags():
    parser = run_backtest._build_parser()
    args = parser.parse_args([
        "--mode", "optimize", "--strategy", "sma_crossover",
        "--sweep-close", "--optimize-metric", "dd_adjusted_return",
        "--direction", "long",
    ])
    assert args.sweep_close is True
    assert args.optimize_metric == "dd_adjusted_return"
    assert args.direction == "long"
    assert args.close_stacks_json is None


def test_build_parser_rejects_unknown_optimize_metric():
    parser = run_backtest._build_parser()
    with pytest.raises(SystemExit):
        parser.parse_args(["--optimize-metric", "alpha_decay"])


def test_run_walk_forward_threads_close_stack_grid(monkeypatch):
    """The close-stack grid, metric, and direction must reach
    walk_forward_optimize — a dropped kwarg silently degrades #996 sweeps to
    a fixed-close run."""
    seen = {}

    def spy_wfo(df, strategy_name, param_ranges, **kwargs):
        seen.update(kwargs)
        return {"error": "spy", "strategy": strategy_name}

    monkeypatch.setattr(run_backtest, "walk_forward_optimize", spy_wfo)
    monkeypatch.setattr(
        run_backtest, "load_cached_data",
        lambda *a, **k: pd.DataFrame(
            {"open": [1.0] * 300, "high": [1.0] * 300, "low": [1.0] * 300,
             "close": [1.0] * 300, "volume": [1.0] * 300},
            index=pd.date_range("2024-01-01", periods=300, freq="D")))

    grid = [{"label": "baseline", "close_strategies": [],
             "stop_loss_atr_mult": None, "trailing_stop_atr_mult": None}]
    run_backtest.run_walk_forward(
        "sma_crossover", close_stack_grid=grid,
        optimize_metric="dd_adjusted_return", direction="long")
    assert seen["close_stack_grid"] == grid
    assert seen["optimize_metric"] == "dd_adjusted_return"
    assert seen["direction"] == "long"
