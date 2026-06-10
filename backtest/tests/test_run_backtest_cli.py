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


def test_build_parser_accepts_composite_regime_labels_and_specs():
    parser = run_backtest._build_parser()
    args = parser.parse_args([
        "--regime-enabled",
        "--regime-classifier", "composite",
        "--regime-thresholds-json", '{"return_pct":0.02,"efficiency":0.6}',
        "--regime-windows-spec-json",
        '{"medium":{"classifier":"composite","period":50}}',
        "--regime-gate-window", "medium",
        "--allowed-regimes", "ranging_directional",
        "--allowed-regimes", "trending_up_clean",
    ])
    assert args.regime_classifier == "composite"
    assert args.regime_gate_window == "medium"
    assert args.allowed_regimes == ["ranging_directional", "trending_up_clean"]


def test_parse_regime_thresholds_json_requires_object():
    assert run_backtest._parse_regime_thresholds_json('{"adx": 20}') == {"adx": 20}
    with pytest.raises(SystemExit):
        run_backtest._parse_regime_thresholds_json("[1, 2, 3]")


def test_parse_regime_windows_spec_arg_normalizes_composite():
    spec = run_backtest._parse_regime_windows_spec_arg(
        '{"medium":{"classifier":"composite","period":50,'
        '"thresholds":{"return_pct":0.02}}}'
    )
    assert spec["medium"]["classifier"] == "composite"
    assert spec["medium"]["period"] == 50
    assert spec["medium"]["thresholds"]["return_pct"] == 0.02


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
            seen["kwargs"] = kwargs
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
        regime_enabled=True,
        regime_classifier="composite",
        regime_thresholds={"return_pct": 0.02},
        regime_windows_spec={"medium": {"classifier": "composite", "period": 50}},
        regime_gate_window="medium",
        allowed_regimes=["trending_up_clean"],
    )
    assert result is not None
    assert seen["platform"] == "robinhood", (
        f"platform did not thread through to Backtester — got {seen}"
    )
    assert seen["capital"] == 777.0
    assert seen["kwargs"]["regime_classifier"] == "composite"
    assert seen["kwargs"]["regime_thresholds"] == {"return_pct": 0.02}
    assert seen["kwargs"]["regime_windows_spec"]["medium"]["period"] == 50
    assert seen["kwargs"]["regime_gate_window"] == "medium"
    assert seen["kwargs"]["allowed_regimes"] == ["trending_up_clean"]
