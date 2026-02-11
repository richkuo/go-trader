"""
Backtest moderate-aggressive parameters vs conservative baseline.
"""
import sys
sys.path.insert(0, ".")

from data_fetcher import fetch_ohlcv
from strategies import apply_strategy
from backtester import Backtester, format_results

CONFIGS = {
    "momentum_btc": {
        "strategy": "momentum",
        "symbol": "BTC/USDT",
        "timeframe": "1h",
        "conservative": {"roc_period": 14, "threshold": 5.0},
        "moderate": {"roc_period": 10, "threshold": 3.5},
    },
    "momentum_sol": {
        "strategy": "momentum",
        "symbol": "SOL/USDT",
        "timeframe": "1h",
        "conservative": {"roc_period": 14, "threshold": 5.0},
        "moderate": {"roc_period": 10, "threshold": 3.5},
    },
    "pairs_spread": {
        "strategy": "pairs_spread",
        "symbol": "BTC/USDT",
        "timeframe": "1d",
        "conservative": {"lookback": 30, "entry_z": 2.0, "exit_z": 0.5},
        "moderate": {"lookback": 20, "entry_z": 1.5, "exit_z": 0.4},
    },
    "volume_weighted": {
        "strategy": "volume_weighted",
        "symbol": "SOL/USDT",
        "timeframe": "1h",
        "conservative": {"sma_period": 20, "vol_multiplier": 1.5},
        "moderate": {"sma_period": 14, "vol_multiplier": 1.2},
    },
}

bt = Backtester(initial_capital=1000)

print("=" * 70)
print("  MODERATE vs CONSERVATIVE BACKTEST COMPARISON")
print("=" * 70)

for name, cfg in CONFIGS.items():
    strat_name = cfg["strategy"]
    symbol = cfg["symbol"]
    tf = cfg["timeframe"]

    print(f"\n{'#'*70}")
    print(f"  {name.upper()} — {symbol} {tf}")
    print(f"{'#'*70}")

    try:
        df = fetch_ohlcv(symbol, tf, limit=500, store=False)
        if df.empty or len(df) < 50:
            print(f"  ⚠️  Not enough data for {symbol}")
            continue
    except Exception as e:
        print(f"  ❌ Failed to fetch {symbol}: {e}")
        continue

    for mode in ["conservative", "moderate"]:
        params = cfg[mode]
        df_signals = apply_strategy(strat_name, df, params)
        results = bt.run(df_signals, strategy_name=f"{name} ({mode})",
                        symbol=symbol, timeframe=tf, params=params, save=False)
        print(format_results(results))

    print(f"\n  🔵 Conservative: {cfg['conservative']}")
    print(f"  🟠 Moderate:     {cfg['moderate']}")
