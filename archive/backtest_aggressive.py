"""
Backtest current (conservative) vs aggressive parameters for all 4 active strategies.
"""
import sys
sys.path.insert(0, ".")

from data_fetcher import fetch_ohlcv
from strategies import apply_strategy
from backtester import Backtester, format_results

# Current (conservative) vs Aggressive params
CONFIGS = {
    "momentum": {
        "symbol": "BTC/USDT",
        "timeframe": "1h",
        "conservative": {"roc_period": 14, "threshold": 5.0},
        "aggressive": {"roc_period": 7, "threshold": 2.0},
    },
    "momentum_sol": {
        "strategy": "momentum",
        "symbol": "SOL/USDT",
        "timeframe": "1h",
        "conservative": {"roc_period": 14, "threshold": 5.0},
        "aggressive": {"roc_period": 7, "threshold": 2.0},
    },
    "pairs_spread": {
        "symbol": "BTC/USDT",
        "timeframe": "1d",
        "conservative": {"lookback": 30, "entry_z": 2.0, "exit_z": 0.5},
        "aggressive": {"lookback": 15, "entry_z": 1.2, "exit_z": 0.3},
    },
    "volume_weighted": {
        "symbol": "SOL/USDT",
        "timeframe": "1h",
        "conservative": {"sma_period": 20, "vol_multiplier": 1.5},
        "aggressive": {"sma_period": 10, "vol_multiplier": 1.0},
    },
}

bt = Backtester(initial_capital=1000)

for name, cfg in CONFIGS.items():
    strat_name = cfg.get("strategy", name)
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
    
    for mode in ["conservative", "aggressive"]:
        params = cfg[mode]
        df_signals = apply_strategy(strat_name, df, params)
        results = bt.run(df_signals, strategy_name=f"{name} ({mode})",
                        symbol=symbol, timeframe=tf, params=params, save=False)
        print(format_results(results))
    
    print(f"\n  📊 Aggressive params: {cfg['aggressive']}")
    print(f"  📊 Conservative params: {cfg['conservative']}")
