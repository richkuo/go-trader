# Agent Setup Guide — go-trader

**Repository:** `https://github.com/richkuo/go-trader.git`

This is a self-contained setup guide for AI agents. Give this file to any AI coding agent and it will handle the full installation — cloning the repo, installing dependencies, configuring Discord/strategies/risk, building, and starting the service.

**For OpenClaw agents:** This is the skill entry point. Read it when a user says "set up go-trader", "install go trading bot", or "configure go-trader".

---

## Step 1: Prerequisites

Check each prerequisite. Install anything missing (ask user before installing).

### 1a. Python 3.12+
```bash
python3 --version
```
If missing or < 3.12, ask:
> Python 3.12+ is required. Want me to install it?

### 1b. uv (Python package manager)
```bash
uv --version 2>/dev/null || echo "NOT_INSTALLED"
```
If missing, install:
```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

### 1c. Go runtime (1.23+)
```bash
go version 2>/dev/null || /usr/local/go/bin/go version 2>/dev/null || echo "NOT_INSTALLED"
```
If missing, ask:
> Go 1.23+ is required to build the scheduler. Want me to install it?

Install with:
```bash
curl -sL https://go.dev/dl/go1.23.6.linux-amd64.tar.gz | tar -C /usr/local -xzf -
```
Note: Go may not be in PATH. Use `/usr/local/go/bin/go` if `go` doesn't resolve.

### 1d. Git
```bash
git --version
```

---

## Step 2: Clone Repository

Check if already installed:
```bash
test -d go-trader/scheduler && echo "EXISTS" || echo "FRESH"
```

**If EXISTS**, ask:
> go-trader is already installed. Do you want to:
> 1. Reconfigure (keep code, redo setup)
> 2. Update (pull latest + rebuild)
> 3. Fresh install (delete and start over)

**If FRESH**, clone from GitHub:
```bash
git clone https://github.com/richkuo/go-trader.git
cd go-trader
```

**If the clone fails** (private repo or auth issue), ask:
> I couldn't clone the repository. Do you have a GitHub token or SSH key configured?
> You can also download it manually: https://github.com/richkuo/go-trader

---

## Step 3: Install Python Dependencies

```bash
cd go-trader
uv sync
```
**Verify:** `.venv/bin/python3` should exist after this.

No user input needed for this step.

---

## Step 4: Discord Configuration

Ask:
> Do you want Discord trade alerts? The bot will post summaries and trade notifications to Discord channels.
>
> (yes / no)

### If no:
Set `discord.enabled = false` in config. Skip to Step 5.

### If yes:

#### 4a. Discord Bot Token
Ask:
> I need your Discord bot token. This is used to post trade alerts.
>
> Where to find it: [Discord Developer Portal](https://discord.com/developers/applications) → your application → Bot → Token
>
> **Security:** I'll store this as a systemd environment variable, not in config files.
>
> Paste your bot token:

Store the token for use in Step 8 (systemd service). Do NOT write it to config.json.

#### 4b. Spot Alerts Channel
Ask:
> Which Discord channel should receive **spot trading** alerts?
>
> This channel will get:
> - Hourly summaries showing PnL for each spot strategy (BTC/ETH/SOL)
> - Immediate notifications when a spot trade executes
>
> I need the **channel ID** — right-click the channel → "Copy Channel ID"
> (Enable Developer Mode in Discord Settings → Advanced if you don't see this option)
>
> Spot channel ID:

#### 4c. Options Alerts Channel
Ask:
> Which Discord channel should receive **options trading** alerts?
>
> This channel will get:
> - Per-check summaries split by exchange (Deribit + IBKR)
> - Individual strategy PnL with recent trade history
> - Immediate trade notifications
>
> This can be the same channel as spot, or a different one.
>
> Options channel ID:

#### 4d. Discord Server (Guild) ID
Ask:
> What's the Discord server (guild) ID where these channels are?
>
> Right-click the server icon → "Copy Server ID"
>
> Server ID:

Store this for OpenClaw allowlist configuration in Step 7.

#### 4e. Summary Frequency
Ask:
> How often should spot summaries post to Discord?
>
> 1. **Hourly** (recommended) — one summary per hour, trades posted immediately
> 2. **Per check** — summary every 5 minutes (can be noisy)
>
> Options summaries always post per check (every 20 minutes).
>
> Your preference: (1 or 2, default: 1)

Map response:
- 1 or blank → `"hourly"`
- 2 → `"per_check"`

---

## Step 5: Trading Configuration

#### 5a. Trading Mode
Ask:
> Do you want to run in paper trading mode (simulated) or live trading?
>
> **Paper mode** (recommended): No real money. Simulates trades with virtual capital. Good for testing strategies before going live.
>
> **Live mode**: Requires exchange API keys. Real trades with real money.
>
> (paper / live, default: paper)

**If live**, prompt for exchange API keys:
> Binance API key:
> Binance API secret:

Store these for the systemd environment in Step 8.

#### 5b. Per-Strategy Capital
Ask:
> How much starting capital per strategy (in USD)?
>
> Default is $1,000 per strategy. With 30 strategies, that's $30,000 total paper capital.
>
> You can change individual strategy amounts later in the config.
>
> Capital per strategy: (default: 1000)

#### 5c. Risk Tolerance — Max Drawdown
Ask:
> What's your maximum drawdown tolerance? When a strategy's losses exceed this percentage, a circuit breaker pauses trading for 24 hours.
>
> - **Spot strategies** default: 60%
> - **Options strategies** default: 40% (measured from peak value)
>
> Do you want to customize these, or use the defaults?
>
> 1. Use defaults (recommended)
> 2. Set custom values
>
> (1 or 2, default: 1)

**If 2:**
> Max drawdown for spot strategies (%, default: 60):
> Max drawdown for options strategies (%, default: 20):

---

## Step 6: Strategy Selection

Ask:
> go-trader comes with 30 strategies across three groups:
>
> **Spot (14 strategies)** — BTC, ETH, SOL on Binance
>   momentum, RSI, MACD, volume-weighted, pairs spread
>
> **Deribit Options (8 strategies)** — BTC, ETH options
>   vol mean reversion, momentum, puts, calls, wheel, butterfly
>
> **IBKR/CME Options (8 strategies)** — BTC, ETH options (CME Micro)
>   Same 6 strategies as Deribit, for head-to-head comparison
>
> Do you want to:
> 1. **Run all 30** (recommended for paper trading)
> 2. **Choose by group** (enable/disable spot, Deribit, IBKR)
> 3. **Pick individual strategies**
>
> (1, 2, or 3, default: 1)

### If 1 (all strategies):
Use the full default strategy set. Skip to Step 6b.

### If 2 (by group):
Ask for each group:
> Enable **spot strategies** (momentum, RSI, MACD, volume-weighted, pairs on BTC/ETH/SOL)? (yes/no, default: yes)
> Enable **Deribit options** (vol MR, momentum, puts, calls, wheel, butterfly on BTC/ETH)? (yes/no, default: yes)
> Enable **IBKR/CME options** (same strategies as Deribit, CME Micro contracts)? (yes/no, default: yes)

### If 3 (individual):
Present each strategy and ask yes/no. Group them for readability:

> **Spot Strategies** (5-minute checks):
>
> | # | Strategy | Assets | Description | Enable? |
> |---|----------|--------|-------------|---------|
> | 1 | momentum | BTC, ETH, SOL | Rate of change breakouts | (y/n) |
> | 2 | rsi | BTC, ETH, SOL | Buy oversold, sell overbought | (y/n) |
> | 3 | macd | BTC, ETH | MACD/signal crossovers | (y/n) |
> | 4 | volume_weighted | BTC, ETH, SOL | Trend + volume confirmation | (y/n) |
> | 5 | pairs_spread | BTC/ETH, BTC/SOL, ETH/SOL | Spread z-score stat arb | (y/n) |
>
> Which spot strategies do you want? (e.g., "1,2,4" or "all" or "none")

Then repeat for options:
> **Deribit Options** (20-minute checks, BTC + ETH each):
>
> | # | Strategy | Description | Enable? |
> |---|----------|-------------|---------|
> | 1 | vol_mean_reversion | High IV → sell strangles, Low IV → buy straddles | (y/n) |
> | 2 | momentum_options | ROC breakout → directional options | (y/n) |
> | 3 | protective_puts | Buy 12% OTM puts, 45 DTE | (y/n) |
> | 4 | covered_calls | Sell 12% OTM calls, 21 DTE | (y/n) |
> | 5 | wheel | Sell 6% OTM puts, 37 DTE | (y/n) |
> | 6 | butterfly | ±5% wing butterfly spread, 30 DTE | (y/n) |
>
> Which Deribit strategies? (e.g., "1,3,6" or "all" or "none")

> **IBKR/CME Options** — Same strategies as Deribit but using CME Micro contracts:
>
> Run the same selection as Deribit, or choose differently?
> 1. Same as Deribit
> 2. Choose individually
> 3. None
>
> (1, 2, or 3)

### 6b. Theta Harvesting (Options)
Only ask if any options strategies were enabled:

Ask:
> **Theta harvesting** lets the bot close sold options early instead of holding to expiry:
> - **Profit target**: Close when X% of premium captured (e.g., 60%)
> - **Stop loss**: Close if loss exceeds X% of premium (e.g., 200% = 2× premium)
> - **Min DTE**: Force-close when fewer than N days to expiry (avoid gamma risk)
>
> Do you want to configure theta harvesting?
> 1. **Enable with defaults** (60% profit, 200% stop, 3 days min DTE) — recommended
> 2. **Custom values**
> 3. **Disable** (options ride to expiry or circuit breaker)
>
> (1, 2, or 3, default: 1)

**If 2:**
> Profit target (% of premium to capture before closing, default: 60):
> Stop loss (% of premium loss before closing, default: 200):
> Minimum DTE to force-close (days, default: 3):

---

## Step 7: Write Configuration

Using all gathered inputs, generate `scheduler/config.json`.

### 7a. Build config.json

Start from `scheduler/config.example.json` as a template. For each enabled strategy, add an entry with:
- `id`: Use the naming convention `{strategy}-{asset}` for spot, `deribit-{strategy}-{asset}` or `ibkr-{strategy}-{asset}` for options
- `type`: `"spot"` or `"options"`
- `script`: `"shared_scripts/check_strategy.py"` (spot), `"shared_scripts/check_options.py"` (options — any platform)
- `args`: Strategy-specific arguments (see config.example.json for format)
- `capital`: User's chosen amount
- `max_drawdown_pct`: User's chosen value (spot default: 60, options default: 40)
- `interval_seconds`: 300 for spot, 1200 for options
- `theta_harvest`: If enabled, include the config block

Discord config:
- `discord.enabled`: true/false based on Step 4
- `discord.token`: Always `""` (token comes from env var)
- `discord.channels.spot`: Channel ID from Step 4b
- `discord.channels.options`: Channel ID from Step 4c
- `discord.spot_summary_freq`: From Step 4e
- `discord.options_summary_freq`: `"per_check"`

### 7b. OpenClaw Discord Allowlist (if applicable)

If the agent is running inside OpenClaw and Discord was configured, add the channels to OpenClaw's guild allowlist so the bot can post:

```bash
# Using OpenClaw gateway config.patch:
# channels.discord.guilds.<GUILD_ID>.channels.<SPOT_CHANNEL>.requireMention = false
# channels.discord.guilds.<GUILD_ID>.channels.<OPTIONS_CHANNEL>.requireMention = false
```

Or via CLI:
```bash
openclaw config set "channels.discord.guilds.${GUILD_ID}.channels.${SPOT_CHANNEL}.requireMention" false
openclaw config set "channels.discord.guilds.${GUILD_ID}.channels.${OPTIONS_CHANNEL}.requireMention" false
```

### 7c. Confirm with User

Show the user a summary before proceeding:
> Here's your configuration:
>
> **Mode:** Paper trading
> **Strategies:** {N} total ({spot_count} spot, {deribit_count} Deribit, {ibkr_count} IBKR)
> **Capital:** ${amount} per strategy (${total} total)
> **Risk:** {spot_drawdown}% max drawdown (spot), {options_drawdown}% (options)
> **Theta harvesting:** {enabled/disabled} {details if enabled}
> **Discord:** {enabled/disabled}
>   📈 Spot alerts → #{channel_name} ({freq})
>   🎯 Options alerts → #{channel_name} (per check)
>
> Proceed? (yes / no)

If no, ask which part they want to change and loop back to the relevant step.

---

## Step 8: Build & Install

### 8a. Build Go Binary
```bash
cd scheduler
/usr/local/go/bin/go build -o ../go-trader .
cd ..
```
If `go` is in PATH, just use `go build`. Check both.

**Verify:** `./go-trader --help` should print usage.

### 8b. Test Run
```bash
./go-trader --config scheduler/config.json --once
```
Check for errors. If Discord is configured, a summary should appear in the channels.

### 8c. Install systemd Service

Create or update the service file. Include the Discord token and any exchange API keys as environment variables:

```ini
[Unit]
Description=Go Trading Scheduler
After=network.target

[Service]
Type=simple
WorkingDirectory={PROJECT_DIR}
ExecStart={PROJECT_DIR}/go-trader --config scheduler/config.json
Environment="DISCORD_BOT_TOKEN={token}"
Restart=always
RestartSec=10
StandardOutput=append:{PROJECT_DIR}/logs/scheduler.log
StandardError=append:{PROJECT_DIR}/logs/scheduler.log

[Install]
WantedBy=multi-user.target
```

If live trading, also add:
```ini
Environment="BINANCE_API_KEY={key}"
Environment="BINANCE_API_SECRET={secret}"
```

```bash
mkdir -p logs
sudo cp go-trader.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable go-trader
sudo systemctl start go-trader
```

### Step 8d: Auto-Update (Git Pull)

Ask the user which update method they prefer:

- **A) Cron job** — daily automatic pull, no code change
- **B) Heartbeat** — integrated into scheduler loop, requires Go change
- **C) Manual** — pull on demand when needed

#### Option A: Cron Job (recommended — no code change)

Add a cron entry to pull once a day (e.g. at 03:00). Only rebuilds and restarts if there are actual changes:
```bash
crontab -e
# Add this line:
0 3 * * * cd /root/.openclaw/workspace/go-trader && git pull --ff-only | grep -q 'Already up to date' || (cd scheduler && /usr/local/go/bin/go build -o ../go-trader . && systemctl restart go-trader) >> /var/log/go-trader-autoupdate.log 2>&1
```

> Logs to `/var/log/go-trader-autoupdate.log`. Rebuild and restart only trigger when upstream has changes.

#### Option B: Heartbeat in Scheduler Loop (integrated — requires Go change)

Add a `git_pull_interval_cycles` field to `scheduler/config.json` (e.g., `12` = every ~1 hour at 5-min cycles), then add a `runGitPull()` call in the scheduler's main loop that fires every N cycles.

> **Note:** Same caveat — Go source changes still need a rebuild + restart to take effect even with this enabled. This option is best for Python/config-only workflows.

#### Option C: Manual

No configuration needed. Pull whenever you push changes:
```bash
cd /path/to/go-trader && git pull --ff-only
sudo systemctl restart go-trader  # only needed for Go source or config changes
```

After adding the cron or heartbeat, verify it works:
```bash
# Cron: check the log after the scheduled time
tail /tmp/go-trader-pull.log

# Heartbeat: check scheduler logs after N cycles
journalctl -u go-trader -f | grep -i "git pull"
```

---

## Step 9: Verification

### 9a. Service Running
```bash
systemctl is-active go-trader
```
Expected: `active`

### 9b. Status Endpoint
```bash
curl -s localhost:8099/status | python3 -c "
import json, sys
d = json.load(sys.stdin)
print(f'Cycle: {d[\"cycle_count\"]}')
print(f'Strategies: {len(d[\"strategies\"])}')
for sym, price in d.get('prices', {}).items():
    print(f'  {sym}: \${price:,.2f}')
"
```

### 9c. Discord Check
If Discord is enabled, wait for the first cycle to complete (~5 minutes) and verify messages appear in the configured channels.

### 9d. Report to User

> ✅ **go-trader is running!**
>
> **Mode:** {paper/live}
> **Strategies:** {N} active
> **Status:** `curl localhost:8099/status`
> **Logs:** `journalctl -u go-trader -f`
>
> Spot strategies check every 5 minutes (summaries {freq}).
> Options strategies check every 20 minutes (summaries per check).
> Trades post immediately to Discord.
>
> **Useful commands:**
> - Stop: `sudo systemctl stop go-trader`
> - Restart: `sudo systemctl restart go-trader`
> - Status: `curl -s localhost:8099/status | python3 -m json.tool`
> - Reset positions: `cp scheduler/state.example.json scheduler/state.json && sudo systemctl restart go-trader`

---

## Backtesting

Run historical simulations using scripts in `backtest/`. All require `.venv/bin/python3` since dependencies (ccxt, pandas, numpy) are installed in the venv.

### Spot Strategy Backtest (`backtest/run_backtest.py`)

Requires `PYTHONPATH=core:strategies` to resolve imports.

```bash
# Single strategy run
PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
  --strategy <name> --symbol BTC/USDT --timeframe 1h --mode single

# Compare two strategies
PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
  --strategy <name> --symbol BTC/USDT --timeframe 1h --mode compare

# Multi-symbol sweep
PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
  --strategy <name> --timeframe 1h --mode multi

# Parameter optimization
PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
  --strategy <name> --symbol BTC/USDT --timeframe 1h --mode optimize

# Limit history (e.g. last 90 days)
PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
  --strategy <name> --symbol BTC/USDT --timeframe 1h --since 90
```

Key flags: `--strategy`, `--symbol`, `--timeframe`, `--mode` (single/compare/multi/optimize), `--since` (days)

### Options Backtest (`backtest/backtest_options.py`)

Self-contained (only imports ccxt).

```bash
.venv/bin/python3 backtest/backtest_options.py \
  --underlying BTC --since 90 --capital 10000

# Verbose output
.venv/bin/python3 backtest/backtest_options.py \
  --underlying BTC --since 90 --capital 10000 --verbose
```

Key flags: `--underlying`, `--since` (days), `--capital`, `--verbose`

### Theta Harvest Comparison (`backtest/backtest_theta.py`)

Self-contained.

```bash
.venv/bin/python3 backtest/backtest_theta.py \
  --underlying BTC --since 90 --capital 10000
```

Key flags: `--underlying`, `--since` (days), `--capital`

---

## Reconfiguration

These can be done after initial setup without re-running the full guide.

### Change Discord Channels
Edit `scheduler/config.json` → `discord.channels`, then restart:
```bash
sudo systemctl restart go-trader
```
If new channels, also add to OpenClaw allowlist.

### Change Discord Token
```bash
sudo systemctl edit go-trader
# Add: Environment="DISCORD_BOT_TOKEN=new_token_here"
sudo systemctl restart go-trader
```

### Add/Remove Strategies
Edit `scheduler/config.json` → `strategies` array, then restart. Removed strategies are auto-pruned from state. New strategies initialize with fresh capital.

### Adjust Risk Settings
Edit `max_drawdown_pct` per strategy in config.json, then restart.

### Enable/Disable Theta Harvesting
Add or remove the `theta_harvest` block from individual strategy entries in config.json, then restart.

### Enable/Disable Auto-Pull (Git Pull)

**Cron-based:** Add or remove the cron entry (`crontab -e`). No restart needed.

**Heartbeat-based:** Set or remove `git_pull_interval_cycles` in config.json, then restart.

### Switch Paper → Live
Add exchange API keys to systemd environment:
```bash
sudo systemctl edit go-trader
# [Service]
# Environment="BINANCE_API_KEY=..."
# Environment="BINANCE_API_SECRET=..."
sudo systemctl restart go-trader
```

---

## `/go-trader` Command

When the user says `/go-trader`, "check bot status", "show strategy health", or "how are the bots doing", run this:

```bash
curl -s localhost:8099/status | python3 -c "
import json, sys
d = json.load(sys.stdin)
prices = d.get('prices', {})
strats = d.get('strategies', {})

print(f'=== GO-TRADER (Cycle {d[\"cycle_count\"]}) ===')
for sym, p in sorted(prices.items()):
    print(f'  {sym}: \${p:,.2f}')

total_val = sum(s['portfolio_value'] for s in strats.values())
total_cap = sum(s['initial_capital'] for s in strats.values())
total_pnl = total_val - total_cap
pct = (total_pnl/total_cap)*100 if total_cap else 0
print(f'\nPortfolio: \${total_cap:,.0f} → \${total_val:,.0f} ({total_pnl:+,.0f} / {pct:+.1f}%)')
print(f'Strategies: {len(strats)}')

# Circuit breakers
cb_active = [(id,s) for id,s in strats.items()
             if s['risk_state'].get('circuit_breaker_until','').startswith('20')]
print(f'Circuit breakers active: {len(cb_active)}')

# Rank by PnL
ranked = sorted(strats.items(), key=lambda x: x[1]['pnl_pct'], reverse=True)
print(f'\nTop 5:')
for id, s in ranked[:5]:
    print(f'  {id}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')
print(f'\nBottom 5:')
for id, s in ranked[-5:]:
    print(f'  {id}: {s[\"pnl_pct\"]:+.1f}% (\${s[\"pnl\"]:+,.0f}) | {s[\"trade_count\"]} trades')

# Dead strategies
dead = [id for id,s in strats.items() if s['trade_count'] == 0]
if dead:
    print(f'\nDead (0 trades): {len(dead)} — {dead}')

# Circuit breaker details
if cb_active:
    print(f'\nCircuit breaker details:')
    for id, s in cb_active:
        rs = s['risk_state']
        print(f'  {id}: dd={rs[\"current_drawdown_pct\"]:.1f}% / max={rs[\"max_drawdown_pct\"]:.0f}% | until {rs[\"circuit_breaker_until\"][:19]}')
"
```

Present the output to the user in a readable format. Highlight any circuit breakers, dead strategies, or notable PnL changes.

---

## `/menu` Command

When the user says `/menu`, "show menu", "what can I configure", "what's available", or "help me get started", output the following overview directly (no bash command needed):

```
=== GO-TRADER MENU ===

1. TRADING PLATFORMS
   • Binance US  — spot trading: BTC, ETH, SOL
   • Deribit     — options trading: BTC, ETH
   • IBKR / CME  — options trading: BTC, ETH (CME Micro contracts, Black-Scholes pricing)

2. AVAILABLE STRATEGIES  (30 total)
   Spot (14):
     momentum (BTC,ETH,SOL), rsi (BTC,ETH,SOL), macd (BTC,ETH,SOL),
     volume_weighted (BTC), pairs_spread (BTC/ETH)
   Deribit Options (8):
     vol_mean_reversion, momentum_options, protective_puts, covered_calls,
     wheel, butterfly  — BTC + ETH each
   IBKR Options (8):
     same 6 strategies as Deribit — BTC + ETH each

3. ADJUSTABLE SETTINGS  (edit scheduler/config.json, then: sudo systemctl restart go-trader)
   Global:
     interval_seconds  — default cycle interval (seconds)
     state_file        — path to position/trade history file
     max_drawdown_pct  — portfolio-level circuit breaker
     notional_cap_usd  — max total notional exposure
   Per-strategy:
     capital           — starting capital (USD)
     max_drawdown_pct  — strategy-level circuit breaker
     interval_seconds  — per-strategy check frequency (0 = use global)
     theta_harvest.*   — profit_target_pct, stop_loss_pct, min_dte_close
   Discord:
     enabled           — true/false
     channel_id        — channel for alerts
     summary_interval  — how often to post summaries
   Environment (sudo systemctl edit go-trader):
     DISCORD_BOT_TOKEN, STATUS_AUTH_TOKEN
     BINANCE_API_KEY, BINANCE_API_SECRET

4. COMMANDS
   /menu       — this overview
   /go-trader  — live status dashboard (cycle, prices, PnL, circuit breakers)
   System:
     sudo systemctl start|stop|restart go-trader
     sudo systemctl status go-trader
     journalctl -u go-trader -n 50 --no-pager
     curl -s localhost:8099/status | python3 -m json.tool

5. BACKTESTING
   Spot:
     PYTHONPATH=core:strategies .venv/bin/python3 backtest/run_backtest.py \
       --strategy <n> --symbol BTC/USDT --timeframe 1h \
       --mode single|compare|multi|optimize
   Options:
     .venv/bin/python3 backtest/backtest_options.py --underlying BTC --since YYYY-MM-DD --capital 10000
     .venv/bin/python3 backtest/backtest_theta.py   --underlying BTC --since YYYY-MM-DD --capital 10000

For full details on any section, ask about it or see the relevant section in SKILL.md.
```

---

## Adjustable Settings Reference

All settings live in `scheduler/config.json`. After any change, restart the service:
```bash
sudo systemctl restart go-trader
```

Config changes are synced to state on startup — no need to reset positions.

### Global Settings

| Setting | Key | Default | Description |
|---------|-----|---------|-------------|
| Check interval | `interval_seconds` | 300 (5 min) | Global default cycle interval in seconds |
| State file path | `state_file` | `scheduler/state.json` | Where positions and trade history are stored |

### Per-Strategy Settings

Each entry in the `strategies` array supports:

| Setting | Key | Default | Description |
|---------|-----|---------|-------------|
| Capital | `capital` | 1000 | Starting capital in USD for this strategy |
| Max drawdown | `max_drawdown_pct` | Spot: 60, Options: 40 | Circuit breaker triggers when drawdown from peak exceeds this %. Measured from the strategy's peak portfolio value, not initial capital. |
| Check interval | `interval_seconds` | Uses global | How often this strategy checks for signals (seconds). 0 = use global default. Spot typically 300 (5 min), options 1200 (20 min). |
| Theta harvest | `theta_harvest.enabled` | false | Enable early exit on sold options |
| Theta profit target | `theta_harvest.profit_target_pct` | 60 | Close sold option when this % of premium is captured |
| Theta stop loss | `theta_harvest.stop_loss_pct` | 200 | Close sold option if loss exceeds this % of premium (200 = 2× premium) |
| Theta min DTE | `theta_harvest.min_dte_close` | 3 | Force-close positions with fewer than N days to expiry |

### Discord Settings

| Setting | Key | Default | Description |
|---------|-----|---------|-------------|
| Enable Discord | `discord.enabled` | true | Turn Discord notifications on/off |
| Spot channel | `discord.channels.spot` | — | Channel ID for spot trading summaries |
| Options channel | `discord.channels.options` | — | Channel ID for options trading summaries |
| Spot frequency | `discord.spot_summary_freq` | `"hourly"` | How often spot summaries post: `"hourly"` or `"per_check"` |
| Options frequency | `discord.options_summary_freq` | `"per_check"` | How often options summaries post: `"per_check"` or `"hourly"` |

### Environment Variables

Set via systemd override (`sudo systemctl edit go-trader`):

| Variable | Description |
|----------|-------------|
| `DISCORD_BOT_TOKEN` | Discord bot token (never store in config.json) |
| `STATUS_AUTH_TOKEN` | Optional: require Bearer token for /status endpoint |
| `BINANCE_API_KEY` | Binance API key (live trading only) |
| `BINANCE_API_SECRET` | Binance API secret (live trading only) |

### Example: Adjusting a Strategy

To change deribit-vol-btc to $2,000 capital with 50% max drawdown and theta harvesting:

```json
{
  "id": "deribit-vol-btc",
  "type": "options",
  "script": "shared_scripts/check_options.py",
  "args": ["vol_mean_reversion", "BTC", "--platform=deribit"],
  "capital": 2000,
  "max_drawdown_pct": 50,
  "interval_seconds": 1200,
  "theta_harvest": {
    "enabled": true,
    "profit_target_pct": 60,
    "stop_loss_pct": 200,
    "min_dte_close": 3
  }
}
```

Then restart: `sudo systemctl restart go-trader`

**Note:** Changing `capital` on an existing strategy does NOT reset its positions or cash. It only changes the `initial_capital` reference for PnL calculations. To fully reset a strategy, delete it from `scheduler/state.json` and restart.

---

## Strategy Reference (for config generation)

### Spot Strategy Entries

Each spot strategy needs entries for each asset it supports:

```json
{"id": "momentum-btc", "type": "spot", "script": "shared_scripts/check_strategy.py", "args": ["momentum", "BTC/USDT", "1h"], "capital": 1000, "max_drawdown_pct": 60, "interval_seconds": 300}
{"id": "momentum-eth", "type": "spot", "script": "shared_scripts/check_strategy.py", "args": ["momentum", "ETH/USDT", "1h"], "capital": 1000, "max_drawdown_pct": 60, "interval_seconds": 300}
{"id": "momentum-sol", "type": "spot", "script": "shared_scripts/check_strategy.py", "args": ["momentum", "SOL/USDT", "1h"], "capital": 1000, "max_drawdown_pct": 60, "interval_seconds": 300}
```

**Strategies and their assets:**
- `momentum`: BTC, ETH, SOL
- `rsi`: BTC, ETH, SOL
- `macd`: BTC, ETH
- `volume_weighted`: BTC, ETH, SOL
- `pairs_spread`: Requires two assets — `args: ["pairs_spread", "BTC/USDT", "1d", "ETH/USDT"]`

**Pairs strategy IDs and args:**
```json
{"id": "pairs-btc-eth", "args": ["pairs_spread", "BTC/USDT", "1d", "ETH/USDT"], "interval_seconds": 86400}
{"id": "pairs-btc-sol", "args": ["pairs_spread", "BTC/USDT", "1d", "SOL/USDT"], "interval_seconds": 86400}
{"id": "pairs-eth-sol", "args": ["pairs_spread", "ETH/USDT", "1d", "SOL/USDT"], "interval_seconds": 86400}
```

### Deribit Options Entries

Each Deribit strategy runs on BTC and ETH:

```json
{"id": "deribit-vol-btc", "type": "options", "script": "shared_scripts/check_options.py", "args": ["vol_mean_reversion", "BTC", "--platform=deribit"], "capital": 1000, "max_drawdown_pct": 40, "interval_seconds": 1200}
{"id": "deribit-vol-eth", "type": "options", "script": "shared_scripts/check_options.py", "args": ["vol_mean_reversion", "ETH", "--platform=deribit"], "capital": 1000, "max_drawdown_pct": 40, "interval_seconds": 1200}
```

**Strategy arg names:** `vol_mean_reversion`, `momentum_options`, `protective_puts`, `covered_calls`, `wheel`, `butterfly`

**ID convention:** `deribit-{strategy_short}-{asset}` where strategy_short is:
- `vol_mean_reversion` → `vol`
- `momentum_options` → `momentum`
- `protective_puts` → `puts`
- `covered_calls` → `calls`
- `wheel` → `wheel`
- `butterfly` → `butterfly`

### IBKR/CME Options Entries

Same as Deribit but with different script and ID prefix:

```json
{"id": "ibkr-vol-btc", "type": "options", "script": "shared_scripts/check_options.py", "args": ["vol_mean_reversion", "BTC", "--platform=ibkr"], "capital": 1000, "max_drawdown_pct": 40, "interval_seconds": 1200}
```

**ID convention:** `ibkr-{strategy_short}-{asset}` (same short names as Deribit)
