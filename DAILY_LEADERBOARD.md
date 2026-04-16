# Daily Leaderboard Channel

## Overview

Read strategy data from two JSON files and post daily summaries to Discord.

## Data Sources

- `scheduler/state.db` — live strategy state (cash, portfolio_value, initial_capital, positions per strategy)
- `scheduler/config.json` — strategy definitions (id, type, platform per strategy)

## How to Calculate P&L Per Strategy

1. `portfolio_value` = value from state (if null or 0, use `cash`)
2. `capital` = `initial_capital` from state (fallback to `capital` from config)
3. `pnl = portfolio_value - capital`
4. `pnl_pct = (pnl / capital) * 100`

## Daily Report

Post 4 separate messages to Discord, one per category:

1. 📈 **Spot Leaderboard** — strategies where type = "spot"
2. ⚡ **Perps Leaderboard (Hyperliquid)** — type = "perps"
3. 🎯 **Options Leaderboard (Deribit/IBKR)** — type = "options"
4. 🏦 **Futures Leaderboard (TopStep/IBKR)** — type = "futures"

### Format

```
{emoji} **{title}**
Daily Report | {Month Day, Year}

```
Strategy                        Value         PnL     PnL%
-----------------------------------------------------------
{top 5 by PnL%, descending}
-----------------------------------------------------------
TOTAL ({N} strategies)     {total_val}  {total_pnl}  {total_pnl%}
```
🟢 {winning} winning · 🔴 {losing} losing · ⚪ {flat} flat
```

## All-Time Leaderboard (Midnight UTC)

Additionally, post an all-time leaderboard at midnight UTC:

- 🏆 **Top 10 All-Time Performers** — top 10 across ALL categories by PnL%
- 💀 **Bottom 10 All-Time Performers** — bottom 10 across ALL categories by PnL%
- Same table format but add a "Type" column between Strategy and Value

## Rules

- Show top 5 strategies sorted by PnL% descending
- TOTAL row includes ALL strategies in that category, not just top 5
- PnL uses `+`/`-` sign prefix
- Strategy IDs are left-aligned (26 chars wide), numbers are right-aligned
- Winning = PnL% > 0, Losing = PnL% < 0, Flat = PnL% == 0
- Post messages with 1s delay between them to avoid rate limits
- Use monospace code blocks for the table

## Discord Channel

Post to channel ID: `[input from user]`
