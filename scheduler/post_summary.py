#!/usr/bin/env python3
"""Post a Hyperliquid live summary snapshot to Discord."""

import json
import sys
import os
from datetime import datetime, timezone

# Load state
with open("scheduler/state.json") as f:
    state = json.load(f)

# Load config
with open("scheduler/config.json") as f:
    config = json.load(f)

DISCORD_TOKEN = config["discord"]["token"]
CHANNEL_ID = config["discord"]["channels"]["hyperliquid"]

# Fetch current ETH price
import urllib.request
req = urllib.request.Request(
    "https://api.hyperliquid.xyz/info",
    data=json.dumps({"type": "allMids"}).encode(),
    headers={"Content-Type": "application/json"}
)
with urllib.request.urlopen(req, timeout=10) as resp:
    mids = json.loads(resp.read())
eth_price = float(mids.get("ETH", 0))

# Build summary
strategies = state["strategies"]
total_value = 0
lines = []

platform_icon = "⚡"
platform_name = "Hyperliquid"

lines.append(f"{platform_icon} ** {platform_name} Summary**\n")
lines.append(f"Cycle #{state['cycle_count']} | {datetime.now(timezone.utc).strftime('%H:%M UTC')}\n")
lines.append(f"ETH ${eth_price:.2f}\n\n")

for sid, strat in strategies.items():
    strat_config = next((s for s in config["strategies"] if s["id"] == sid), None)
    strat_type = strat_config["type"] if strat_config else "perps"

    cash = strat.get("cash", 0)
    positions = strat.get("positions", {})
    trades = strat.get("trade_history", [])
    risk = strat.get("risk_state", {})

    total_pnl = 0
    for t in trades:
        total_pnl += t.get("value", 0)
    initial = strat.get("initial_capital", 0)
    current_value = cash
    for pos in positions.values():
        current_value += pos.get("quantity", 0) * eth_price

    pnl_pct = ((current_value - initial) / initial * 100) if initial > 0 else 0

    side = list(positions.values())[0]["side"] if positions else "flat"
    pos_str = ""
    if positions:
        for sym, pos in positions.items():
            qty = pos.get("quantity", 0)
            avg = pos.get("avg_cost", 0)
            pos_str = f"{side.upper()} {qty:.4f} {sym} @ ${avg:.2f}"

    cb = "🔴 CB" if risk.get("circuit_breaker") else "🟢 OK"
    dd = risk.get("current_drawdown_pct", 0) * 100

    lines.append(f"**{sid}**\n")
    lines.append(f"  Value: ${current_value:.2f} ({pnl_pct:+.2f}%) | {cb} | DD: {dd:.1f}%\n")
    if pos_str:
        lines.append(f"  {pos_str}\n")
    lines.append(f"  Trades: {len(trades)} | Cash: ${cash:.2f}\n\n")
    total_value += current_value

lines.append(f"**Total Value:** ${total_value:.2f}")

content = "".join(lines)

# Post to Discord
import urllib.request
import urllib.parse

payload = json.dumps({"content": content}).encode()
req = urllib.request.Request(
    f"https://discord.com/api/v10/channels/{CHANNEL_ID}/messages",
    data=payload,
    headers={
        "Authorization": f"Bot {DISCORD_TOKEN}",
        "Content-Type": "application/json"
    }
)
with urllib.request.urlopen(req, timeout=10) as resp:
    result = json.loads(resp.read())
    print(f"Posted message ID: {result.get('id')}")
