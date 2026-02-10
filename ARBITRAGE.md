# Polymarket + Binance Arbitrage Opportunity Tracker

## Overview

This system tracks arbitrage opportunities between Polymarket prediction markets and Binance spot prices for cryptocurrencies (BTC and SOL). It identifies when prediction market probabilities differ significantly from what current spot prices would suggest.

## How It Works

### 1. Data Sources
- **Binance**: Real-time spot prices for BTC/USDT and SOL/USDT via ccxt
- **Polymarket**: Prediction market contracts related to crypto price targets via CLOB API

### 2. Arbitrage Detection Logic

The system compares:
- **Theoretical Probability**: What the probability should be based on current spot price vs prediction target
- **Implied Probability**: What the prediction market is actually pricing (market price = probability)

**Example:**
- BTC current price: $45,000
- Polymarket question: "Will BTC hit $50,000 by March 31?"
- If market price is 0.30 (30% probability) but current price suggests 80% chance
- This creates a 50% arbitrage spread

### 3. Opportunity Identification

Opportunities are logged when:
- Spread between theoretical and implied probability > 2%
- Spread < 50% (to filter data errors)
- Valid price data exists for both sources

## File Structure

```
trading-bot/
├── arb_tracker.py          # Main tracking engine
├── arb_analyzer.py         # Pattern analysis and reporting
├── polymarket_client.py    # Polymarket API client
├── arb_memories.json       # Accumulated pattern memory
└── arb_logs/              # Daily opportunity logs
    ├── arb_opportunities_2024-02-10.jsonl
    └── arb_opportunities_2024-02-11.jsonl
```

## Usage

### Start the Tracker
```bash
python3 arb_tracker.py
```

The tracker runs continuously, logging opportunities to `arb_logs/` directory.

### Analyze Opportunities
```bash
python3 arb_analyzer.py
```

Generates comprehensive reports on:
- Spread patterns by symbol and time
- Market condition analysis  
- Recurring patterns and anomalies
- Historical statistics

### Test Polymarket Client
```bash
python3 polymarket_client.py
```

Tests connection to Polymarket API and displays available crypto markets.

## Log Format

Each opportunity is logged as JSON lines (.jsonl) with these fields:

```json
{
  "timestamp": "2024-02-10T15:30:45.123456Z",
  "symbol": "BTC",
  "source_price": 45000.00,
  "target_price": 50000.00,
  "market_price": 0.30,
  "implied_prob": 0.30,
  "theoretical_prob": 0.80,
  "spread_pct": 50.0,
  "direction": "market_undervalued",
  "contract_id": "0x1234...",
  "question": "Will BTC hit $50,000 by March 31?",
  "outcome": "Yes"
}
```

### Field Explanations
- `source_price`: Current Binance spot price
- `target_price`: Price target from prediction market question
- `market_price`: Current prediction market price (= implied probability)
- `implied_prob`: Market's assessment of probability
- `theoretical_prob`: Calculated probability based on current price vs target
- `spread_pct`: Absolute difference between probabilities (%)
- `direction`: "market_undervalued" or "market_overvalued"

## Memory System

`arb_memories.json` accumulates patterns over time:

- Total opportunities found
- Average spreads by symbol
- Time-of-day patterns
- Day-of-week distributions
- Largest historical spreads
- Market condition correlations

## Technical Implementation

### Architecture
- **Threading**: Separate threads for Binance polling, Polymarket polling, and memory saves
- **Polling-Based**: Uses REST API polling instead of WebSockets for compatibility
- **Rate Limited**: 10-second intervals for Binance, 30-second for Polymarket
- **Fault Tolerant**: Automatic reconnection and error handling

### Dependencies
```
ccxt >= 3.0.0          # Exchange connectivity
pandas >= 1.5.0        # Data analysis  
requests >= 2.28.0     # HTTP requests
```

### Configuration
Edit thresholds in `arb_tracker.py`:
```python
self.min_spread_pct = 2.0   # Minimum spread to log
self.max_spread_pct = 50.0  # Maximum spread (filter errors)
```

## Risk Management

⚠️ **OBSERVATION ONLY** - This system does NOT execute trades, only logs opportunities.

### Considerations for Live Trading
- **Execution Risk**: Time to execute both legs of arbitrage
- **Liquidity Risk**: Slippage on prediction market orders
- **Resolution Risk**: Prediction markets may resolve differently than expected
- **Capital Requirements**: Need funds on both Polymarket and Binance
- **Regulatory Risk**: Prediction market legality varies by jurisdiction

## Analysis Features

The analyzer provides insights on:

### Temporal Patterns
- Peak opportunity hours (UTC)
- Day-of-week distributions
- Seasonal trends

### Market Patterns  
- Price range impact on opportunities
- Volatility correlation
- Direction bias (over/undervalued)

### Symbol Analysis
- BTC vs SOL opportunity frequency
- Average spreads by crypto
- Cross-symbol correlations

### Anomaly Detection
- Unusually large spreads (>95th percentile)
- High-frequency opportunity periods
- Recurring problematic contracts

## Future Enhancements

Potential improvements:
1. **Real-time WebSocket feeds** (when packages available)
2. **Additional cryptocurrencies** (ETH, ADA, etc.)
3. **Machine learning models** for probability estimation
4. **Backtesting framework** for strategy evaluation
5. **Alert system** for high-value opportunities
6. **Web dashboard** for real-time monitoring

## Troubleshooting

### Common Issues

**No opportunities found:**
- Check internet connection
- Verify Polymarket API is accessible
- Ensure crypto-related prediction markets exist

**High error rates:**
- Reduce polling frequency
- Check API rate limits
- Verify JSON parsing logic

**Memory file corruption:**
- Delete `arb_memories.json` to reset
- Check disk space for log files

### Logging
- Adjust log level in source files
- Logs include timestamp, level, and detailed error messages
- Monitor log files for API connectivity issues

## Contributing

To extend the system:
1. Add new exchanges by implementing similar polling logic
2. Expand crypto coverage by updating symbol lists  
3. Improve probability calculations with better models
4. Add new analysis dimensions to the analyzer

## Disclaimer

This software is for educational and research purposes only. Cryptocurrency and prediction market trading involves substantial risk. The authors are not responsible for any financial losses. Always do your own research and consider your risk tolerance before trading.