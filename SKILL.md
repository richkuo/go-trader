# Go Trading Bot Setup Skill

This skill guides setup of the Go trading bot (spot + options crypto strategies with Discord alerts).

## Invocation
User says: "Set up go-trader", "Install go trading bot", or "Configure go-trader"

## Prerequisites
- OpenClaw agent with filesystem access
- Go runtime (1.23.6+) — on this server: `/usr/local/go/bin/go`
- Python 3.12+ with uv package manager
- Git installed

## Setup Flow

### 1. Check Existing Installation
```bash
test -f scheduler/config.json && echo "EXISTS" || echo "FRESH"
```
If config exists, ask: "go-trader is already configured. Do you want to reconfigure or just update?"

### 2. Clone Repository (if needed)
```bash
cd /root/.openclaw/workspace
git clone https://github.com/richkuo/go-trader.git
cd go-trader
```

### 3. Create Config from Example
```bash
cp scheduler/config.example.json scheduler/config.json
cp scheduler/state.example.json scheduler/state.json
```

### 4. Gather User Input

Prompt for each value in order. Explain what each is for and where to find it.

#### 4a. Discord Setup

**Discord Bot Token:**
> I need your Discord bot token for trade alerts. This posts cycle summaries and trade notifications to your server.
>
> Get it from the [Discord Developer Portal](https://discord.com/developers/applications) → your bot → Bot → Token.
>
> Format: `MTQ3MDAwMDE2...`
>
> **Security note:** I'll store this as an environment variable in the systemd service, not in config.json.

If user provides token, store it for step 7. If blank, Discord alerts will be disabled.

**Discord Channel for Spot Alerts:**
> Which Discord channel should receive **spot trading** alerts? These include:
> - Hourly summaries with PnL per strategy (BTC/ETH/SOL momentum, RSI, MACD, etc.)
> - Immediate notifications when a spot trade executes
>
> I need the **channel ID** — right-click the channel → Copy Channel ID.
> (Enable Developer Mode in Discord Settings → Advanced if you don't see this option.)

**Discord Channel for Options Alerts:**
> Which Discord channel should receive **options trading** alerts? These include:
> - Per-check summaries split by exchange (Deribit + IBKR)
> - Individual strategy PnL with trade history
> - Immediate notifications on options trades
>
> This can be the same channel as spot, or a different one to keep things organized.
> I need the **channel ID**.

**Discord Server (Guild) ID:**
> What's the Discord server ID where these channels live?
> Right-click the server icon → Copy Server ID.
>
> I need this to add the channels to OpenClaw's Discord allowlist so the bot can post there.

**Summary Frequency (optional):**
> How often should spot summaries post?
> - `hourly` (default) — one summary per hour, trades posted immediately
> - `per_check` — summary every 5 minutes (noisy)
>
> Options summaries always post per-check (every 20 minutes).

Default to `hourly` for spot, `per_check` for options if user doesn't specify.

#### 4b. Exchange API Keys

**Binance API Key (optional):**
> Do you have Binance API credentials for live trading?
> Leave blank to run in **paper trading mode** (simulated trades, no real money).
>
> Paper mode is recommended for initial setup — you can add API keys later.

If blank, skip Binance secret prompt.

#### 4c. Capital & Risk (optional)

**Per-Strategy Capital:**
> Each strategy starts with $1,000 by default. Want a different amount?
> This applies to all 30 strategies (14 spot + 16 options).

**Max Drawdown:**
> Default max drawdown is 60% for spot, 20% for options. Circuit breakers pause trading when hit.
> Want to customize?

Most users should keep defaults.

### 5. Write Configuration

**Update config.json:**
```python
python3 << 'EOF'
import json

with open('scheduler/config.json', 'r') as f:
    config = json.load(f)

# Discord channels (token goes in env var, NOT config)
config['discord']['enabled'] = True
config['discord']['token'] = ''  # Intentionally blank — use env var
config['discord']['channels']['spot'] = '${SPOT_CHANNEL}'
config['discord']['channels']['options'] = '${OPTIONS_CHANNEL}'
config['discord']['spot_summary_freq'] = '${SPOT_FREQ}'
config['discord']['options_summary_freq'] = '${OPTIONS_FREQ}'

with open('scheduler/config.json', 'w') as f:
    json.dump(config, f, indent=2)
    f.write('\n')

print("✓ Config written")
EOF
```

**Add channels to OpenClaw's Discord allowlist:**
This is required so the bot (which shares OpenClaw's Discord token) can post to these channels.

```bash
# Use gateway config.patch to add channels to the guild allowlist
# The agent should use the gateway tool:
# gateway config.patch with:
#   channels.discord.guilds.<GUILD_ID>.channels.<SPOT_CHANNEL>.requireMention = false
#   channels.discord.guilds.<GUILD_ID>.channels.<OPTIONS_CHANNEL>.requireMention = false
```

Or via OpenClaw CLI:
```bash
openclaw config set "channels.discord.guilds.${GUILD_ID}.channels.${SPOT_CHANNEL}.requireMention" false
openclaw config set "channels.discord.guilds.${GUILD_ID}.channels.${OPTIONS_CHANNEL}.requireMention" false
```

### 6. Install Python Dependencies
```bash
curl -LsSf https://astral.sh/uv/install.sh | sh  # if uv not installed
uv venv
uv sync
```

### 7. Build Go Scheduler
```bash
cd scheduler
/usr/local/go/bin/go build -o ../go-trader .
cd ..
```

### 8. Install systemd Service

Write the service file with the Discord token as an environment variable:

```ini
[Unit]
Description=Go Trading Scheduler
After=network.target

[Service]
Type=simple
WorkingDirectory=/root/.openclaw/workspace/go-trader
ExecStart=/root/.openclaw/workspace/go-trader/go-trader --config scheduler/config.json
Environment="DISCORD_BOT_TOKEN=${BOT_TOKEN}"
Restart=always
RestartSec=10
StandardOutput=append:/root/.openclaw/workspace/go-trader/logs/scheduler.log
StandardError=append:/root/.openclaw/workspace/go-trader/logs/scheduler.log

[Install]
WantedBy=multi-user.target
```

```bash
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader
```

### 9. Verify

```bash
systemctl is-active go-trader
curl -s localhost:8099/status | python3 -m json.tool
```

Wait for first cycle (~5 min), then check the Discord channels for the first summary.

### 10. Confirm to User

```
✅ Go trading bot is running!

Mode: ${MODE}  (paper trading / live)
Strategies: 30 (14 spot + 16 options)
Discord alerts:
  📈 Spot → #${SPOT_CHANNEL_NAME} (${SPOT_FREQ})
  🎯 Options → #${OPTIONS_CHANNEL_NAME} (per check)
Status: curl localhost:8099/status
Logs: journalctl -u go-trader -f

Spot strategies check every 5 minutes, summaries ${SPOT_FREQ}.
Options strategies check every 20 minutes, summaries per check.
Trades post immediately to the relevant channel.
```

## Discord Output Format

The bot posts two types of messages:

**Spot Summary (📈):**
- Prices: BTC, ETH, SOL
- Per-strategy PnL with trade count
- Last 3 trades per strategy
- Starting capital → current value with total PnL

**Options Summary (🎯):**
- Split into Deribit and IBKR sections
- Per-strategy PnL with trade history
- Shows option positions and current values

**Trade Alerts:**
- Posted immediately when a trade executes
- Shows side (BUY/SELL), symbol, price, timestamp

## Reconfiguration

### Change Discord Channels
Edit `scheduler/config.json` → `discord.channels`, then:
```bash
systemctl restart go-trader
```
Also update OpenClaw's allowlist if the new channels aren't already added.

### Change Discord Token
Update the systemd environment variable:
```bash
sudo systemctl edit go-trader
# Add: Environment="DISCORD_BOT_TOKEN=new_token_here"
sudo systemctl restart go-trader
```

### Add/Remove Strategies
Edit `scheduler/config.json` → `strategies` array, then:
```bash
systemctl restart go-trader
```
State for removed strategies is auto-pruned. New strategies initialize with fresh capital.

### Switch to Live Trading
Add exchange API keys to environment:
```bash
sudo systemctl edit go-trader
# Add:
# Environment="BINANCE_API_KEY=..."
# Environment="BINANCE_API_SECRET=..."
sudo systemctl restart go-trader
```

## Strategy Reference

**Spot (5-min checks, hourly Discord summaries):**
- Momentum, RSI, MACD, Volume Weighted, Pairs Spread
- Assets: BTC/USDT, ETH/USDT, SOL/USDT
- Default: $1K capital, 60% max drawdown

**Options (20-min checks, per-check Discord summaries):**
- Vol Mean Reversion, Momentum, Protective Puts, Covered Calls, Wheel, Butterfly
- Exchanges: Deribit (testnet), IBKR (simulated)
- Assets: BTC, ETH
- Default: $1K capital, 20% max drawdown

## Troubleshooting

**No Discord messages:** Check token is set (`echo $DISCORD_BOT_TOKEN` in service env), channel IDs are correct, bot has Send Messages permission in the channel, and channels are in OpenClaw's allowlist.

**Service won't start:** `journalctl -u go-trader -n 50`

**Rebuild after Go code changes:** `cd scheduler && /usr/local/go/bin/go build -o ../go-trader . && systemctl restart go-trader`

**Python changes:** Just `systemctl restart go-trader` (no rebuild needed).

**Reset all positions:** `cp scheduler/state.example.json scheduler/state.json && systemctl restart go-trader`
