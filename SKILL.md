# Go Trading Bot Setup Skill

This skill guides first-time setup of the Go trading bot (spot + options strategies for crypto).

## Invocation
User says: "Set up go-trader" or "Install go trading bot" or "Configure go-trader"

## Prerequisites
- OpenClaw agent with file system access
- Go runtime installed (1.23.6+)
- Python 3.12+ with uv package manager
- Git installed

## Steps

### 1. Check if already set up
```bash
test -f /root/.openclaw/workspace/go-trader/scheduler/config.json && echo "Already configured"
```

If `config.json` exists, ask user if they want to reconfigure.

### 2. Clone repository (if needed)
```bash
cd /root/.openclaw/workspace
git clone https://github.com/richkuo/go-trader.git
cd go-trader
```

### 3. Copy example configs
```bash
cp scheduler/config.example.json scheduler/config.json
cp scheduler/state.example.json scheduler/state.json
```

### 4. Prompt user for values

Ask in order:

**Discord Bot Token:**
```
"Discord bot token for trade alerts?
This will be used to post cycle summaries and trade notifications.
Format: MTQ3...
(Get from Discord Developer Portal)"
```

Prompt for the token:
```bash
echo "Discord bot token?"
read BOT_TOKEN
```

**Discord Channel IDs:**
```
"Discord channel ID for SPOT trading alerts?
Example: 1234567890123456789"

"Discord channel ID for OPTIONS trading alerts?
Example: 9876543210987654321"
```

**Exchange API Keys (optional for paper trading):**
```
"Binance API key? (leave blank for paper trading mode)"
"Binance API secret? (leave blank for paper trading mode)"
```

### 5. Write values to config.json

Update the Discord configuration:
```bash
SPOT_CHANNEL="${USER_SPOT_CHANNEL}"
OPTIONS_CHANNEL="${USER_OPTIONS_CHANNEL}"

# Use Python to update the JSON config
python3 << EOF
import json

with open('scheduler/config.json', 'r') as f:
    config = json.load(f)

config['discord']['token'] = '${BOT_TOKEN}'
config['discord']['channels']['spot'] = '${SPOT_CHANNEL}'
config['discord']['channels']['options'] = '${OPTIONS_CHANNEL}'

with open('scheduler/config.json', 'w') as f:
    json.dump(config, f, indent=2)
    f.write('\n')

print("✓ Config updated")
EOF
```

**Important:** Ensure Discord channels are in OpenClaw's allowlist:
```bash
if [ -n "$SPOT_CHANNEL" ] || [ -n "$OPTIONS_CHANNEL" ]; then
  echo "✓ Discord configured"
  echo "  Make sure these channels are in OpenClaw's Discord allowlist"
  echo "  Run: openclaw config get channels.discord.guilds"
fi
```

### 6. Install Python dependencies
```bash
# Install uv if not present
curl -LsSf https://astral.sh/uv/install.sh | sh

# Create virtual environment and install dependencies
uv venv
uv sync
```

### 7. Build Go scheduler
```bash
cd scheduler
go build -o trading-scheduler .
cd ..
```

### 8. Test configuration
```bash
# Quick test run (won't place trades)
./scheduler/trading-scheduler --help
```

Verify binary runs without errors.

### 9. Set up systemd service

The Discord token is already in `scheduler/config.json`, so just install the service:
```bash
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader
```

### 10. Verify service is running
```bash
sudo systemctl status go-trader
curl -s localhost:8099/status | jq .
```

### 11. Confirm to user

Determine mode:
```bash
MODE="paper trading"
[ -n "$BINANCE_API_KEY" ] && MODE="live trading"

DISCORD_STATUS="disabled"
[ -n "$SPOT_CHANNEL" ] && DISCORD_STATUS="enabled → spot: ${SPOT_CHANNEL}, options: ${OPTIONS_CHANNEL}"
```

Message:
```
✅ Go trading bot installed and running!

Mode: ${MODE}
Strategies: 30 (14 spot + 16 options)
Discord alerts: ${DISCORD_STATUS}
Port: 8099
Logs: journalctl -u go-trader -f

Check status: systemctl status go-trader
Web status: curl localhost:8099/status

Spot strategies run every 5 minutes
Options strategies run every 20 minutes
Discord summaries: spot hourly, options per check
```

## Notes

- **Paper trading mode** by default (no exchange API keys = simulated trades only)
- Bot runs **30 strategies** across spot (Binance) and options (Deribit + IBKR)
- **Discord setup**: Bot uses OpenClaw's Discord bot token (shared)
  - Spot channel: hourly summaries + immediate trade alerts
  - Options channel: per-check summaries + immediate trade alerts
  - Channels must be in OpenClaw's Discord allowlist
- **State persistence**: `scheduler/state.json` tracks positions, P&L, circuit breakers
- **Config changes**: Edit `scheduler/config.json`, then `systemctl restart go-trader`
- Service auto-restarts on failure (10 second delay)

## Strategy Overview

**Spot strategies (5-minute interval):**
- Momentum (BTC, ETH, SOL)
- RSI (BTC, ETH, SOL)
- MACD (BTC, ETH)
- Volume Weighted (BTC, ETH, SOL)
- Pairs Spread (BTC, ETH, SOL)

**Options strategies (20-minute interval):**
- Deribit: vol mean reversion, momentum, protective puts, covered calls, wheel, butterfly
- IBKR: same 6 strategies mirrored for CME crypto options

All strategies start with $1,000 capital and 60% max drawdown (spot) or 20% (options).

## Troubleshooting

**Service won't start:**
```bash
journalctl -u go-trader -n 50
```

**Rebuild Go binary:**
```bash
cd scheduler
go build -o trading-scheduler .
```

**Reset state (clear all positions):**
```bash
cp scheduler/state.example.json scheduler/state.json
systemctl restart go-trader
```

**Change Discord channels:**
```bash
# Edit scheduler/config.json manually or use Python/jq
systemctl restart go-trader
```

**Update strategies:**
Edit `scheduler/config.json` → `strategies` array, then restart.
