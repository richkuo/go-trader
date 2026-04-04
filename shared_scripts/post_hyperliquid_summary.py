#!/usr/bin/env python3
"""Post a one-off Hyperliquid snapshot summary to Discord.

Usage:
    python3 shared_scripts/post_hyperliquid_summary.py [--config PATH] [--state PATH] [--dry-run]

Reads state.json + config.json, fetches current prices from the Hyperliquid API,
formats a summary matching the scheduler's Discord output, and posts it.
"""

import argparse
import json
import os
import sys
import urllib.request


def load_json(path):
    with open(path, "r") as f:
        return json.load(f)


def fetch_prices():
    """Fetch current mid prices from Hyperliquid API."""
    url = "https://api.hyperliquid.xyz/info"
    payload = json.dumps({"type": "allMids"}).encode()
    req = urllib.request.Request(url, data=payload, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read())
    except Exception as e:
        print(f"[WARN] Failed to fetch prices from Hyperliquid API: {e}", file=sys.stderr)
        return {}


def fmt_comma(v):
    """Format a number with comma separators (matches Go fmtComma)."""
    n = int(abs(v))
    return f"{n:,}"


def portfolio_value(strategy_state, prices):
    """Calculate total portfolio value for a strategy (matches Go PortfolioValue)."""
    total = strategy_state.get("cash", 0.0)
    for sym, pos in strategy_state.get("positions", {}).items():
        price = prices.get(sym, pos.get("avg_cost", 0))
        qty = pos.get("quantity", 0)
        avg_cost = pos.get("avg_cost", 0)
        multiplier = pos.get("multiplier", 0)
        side = pos.get("side", "long")
        if multiplier > 0:
            if side == "long":
                total += qty * multiplier * (price - avg_cost)
            else:
                total += qty * multiplier * (avg_cost - price)
        elif side == "long":
            total += qty * price
        else:
            total += qty * (2 * avg_cost - price)
    for _, opt in strategy_state.get("option_positions", {}).items():
        total += opt.get("current_value_usd", 0)
    return total


def resolve_hl_channel(config):
    """Find the Discord channel for hyperliquid strategies."""
    channels = config.get("discord", {}).get("channels", {})
    return channels.get("hyperliquid") or channels.get("perps") or ""


def build_prices_map(raw_mids, assets):
    """Build a {ASSET/USDT: price} map from Hyperliquid allMids response."""
    prices = {}
    for asset in assets:
        key = asset.upper()
        if key in raw_mids:
            prices[f"{key}/USDT"] = float(raw_mids[key])
        # Also try without /USDT suffix for direct matches
        key_slash = f"{key}/USDT"
        if key_slash not in prices:
            # Try lowercase
            if key.lower() in raw_mids:
                prices[f"{key}/USDT"] = float(raw_mids[key.lower()])
    return prices


def extract_asset(strategy_cfg):
    """Extract asset from strategy config args (matches Go extractAsset)."""
    args = strategy_cfg.get("args", [])
    if len(args) > 1:
        asset = args[1].upper()
        return asset.replace("/USDT", "")
    return ""


def extract_strategy_name(strategy_cfg):
    """Extract short strategy name (matches Go extractStrategyName)."""
    if strategy_cfg.get("type") == "options" and strategy_cfg.get("args"):
        return strategy_cfg["args"][0]
    parts = strategy_cfg.get("id", "").split("-")
    if strategy_cfg.get("type") == "perps" and len(parts) >= 3 and parts[0] == "hl":
        return parts[1]
    return parts[0] if parts else "unknown"


def format_summary(cycle, hl_strategies, state, prices, asset_filter=""):
    """Format a summary message matching Go FormatCategorySummary."""
    lines = []

    # Header
    icon = "\u26a1"  # lightning bolt
    asset_suffix = f" \u2014 {asset_filter}" if asset_filter else ""
    lines.append(f"{icon} **Hyperliquid Summary{asset_suffix}**")
    lines.append(f"Cycle #{cycle} | on-demand")

    # Prices line
    display_prices = prices
    if asset_filter:
        display_prices = {
            sym: p for sym, p in prices.items()
            if sym.split("/")[0].upper() == asset_filter.upper()
        }
    if display_prices:
        syms = sorted(display_prices.keys())
        parts = []
        for sym in syms:
            short = sym.replace("/USDT", "")
            parts.append(f"{short} ${display_prices[sym]:,.0f}")
        lines.append(" | ".join(parts))

    # Strategy table
    bots = []
    total_cap = 0.0
    total_value = 0.0
    for sc in hl_strategies:
        sid = sc["id"]
        ss = state.get("strategies", {}).get(sid)
        if ss is None:
            continue
        pv = portfolio_value(ss, prices)
        capital = sc.get("capital", 0)
        total_cap += capital
        total_value += pv
        pnl = pv - capital
        pnl_pct = (pnl / capital * 100) if capital > 0 else 0.0
        bots.append({
            "id": sid,
            "value": pv,
            "pnl": pnl,
            "pnl_pct": pnl_pct,
        })

    total_pnl = total_value - total_cap
    total_pnl_pct = (total_pnl / total_cap * 100) if total_cap > 0 else 0.0

    if bots:
        sep = "-----------------------------------------------"
        lines.append("")
        lines.append("```")
        lines.append(f"{'Strategy':<20s} {'Value':>10s} {'PnL':>10s} {'PnL%':>7s}")
        lines.append(sep)
        for bot in bots:
            label = bot["id"][:20]
            val_str = "$ " + fmt_comma(bot["value"])
            pnl_sign = "+" if bot["pnl"] >= 0 else "-"
            pnl_str = "$ " + pnl_sign + fmt_comma(abs(bot["pnl"]))
            pct_sign = "+" if bot["pnl_pct"] >= 0 else ""
            pct_str = f"{pct_sign}{bot['pnl_pct']:.1f}%"
            lines.append(f"{label:<20s} {val_str:>10s} {pnl_str:>10s} {pct_str:>7s}")
        lines.append(sep)
        tot_val_str = "$ " + fmt_comma(total_value)
        tot_pnl_sign = "+" if total_pnl >= 0 else "-"
        tot_pnl_str = "$ " + tot_pnl_sign + fmt_comma(abs(total_pnl))
        tot_pct_sign = "+" if total_pnl_pct >= 0 else ""
        tot_pct_str = f"{tot_pct_sign}{total_pnl_pct:.1f}%"
        lines.append(f"{'TOTAL':<20s} {tot_val_str:>10s} {tot_pnl_str:>10s} {tot_pct_str:>7s}")
        lines.append("```")

    return "\n".join(lines)


def post_to_discord(token, channel_id, content):
    """Post a message to a Discord channel."""
    url = f"https://discord.com/api/v10/channels/{channel_id}/messages"
    payload = json.dumps({"content": content}).encode()
    req = urllib.request.Request(url, data=payload, headers={
        "Authorization": f"Bot {token}",
        "Content-Type": "application/json",
    })
    with urllib.request.urlopen(req, timeout=10) as resp:
        result = json.loads(resp.read())
    return result.get("id", "")


def main():
    parser = argparse.ArgumentParser(description="Post a one-off Hyperliquid snapshot summary to Discord")
    parser.add_argument("--config", default="scheduler/config.json", help="Path to config.json")
    parser.add_argument("--state", default="scheduler/state.json", help="Path to state.json")
    parser.add_argument("--dry-run", action="store_true", help="Print summary without posting to Discord")
    args = parser.parse_args()

    # Load config
    if not os.path.exists(args.config):
        print(f"Error: config file not found: {args.config}", file=sys.stderr)
        sys.exit(1)
    config = load_json(args.config)

    # Resolve state path: use per-platform state file if configured
    state_path = args.state
    platforms = config.get("platforms", {})
    if "hyperliquid" in platforms:
        plat_state = platforms["hyperliquid"].get("state_file", "")
        if plat_state and os.path.exists(plat_state):
            state_path = plat_state

    if not os.path.exists(state_path):
        print(f"Error: state file not found: {state_path}", file=sys.stderr)
        sys.exit(1)
    state = load_json(state_path)

    # Filter hyperliquid strategies from config
    hl_strategies = [s for s in config.get("strategies", []) if s.get("platform") == "hyperliquid"]
    if not hl_strategies:
        print("No hyperliquid strategies found in config", file=sys.stderr)
        sys.exit(1)

    # Collect unique assets
    assets = set()
    for sc in hl_strategies:
        asset = extract_asset(sc)
        if asset:
            assets.add(asset)

    # Fetch prices
    raw_mids = fetch_prices()
    prices = build_prices_map(raw_mids, assets)

    cycle = state.get("cycle_count", 0)

    # Group by asset if multiple assets
    ASSET_ORDER = {"BTC": 0, "ETH": 1, "SOL": 2, "BNB": 3}
    sorted_assets = sorted(assets, key=lambda a: (ASSET_ORDER.get(a, 99), a))

    if len(sorted_assets) <= 1:
        # Single asset or no grouping needed
        asset_filter = sorted_assets[0] if sorted_assets else ""
        summary = format_summary(cycle, hl_strategies, state, prices, asset_filter)
    else:
        # Multiple assets: one combined summary
        summary = format_summary(cycle, hl_strategies, state, prices)

    if args.dry_run:
        print(summary)
        return

    # Resolve Discord channel and token
    token = os.environ.get("DISCORD_BOT_TOKEN", config.get("discord", {}).get("token", ""))
    if not token:
        print("Error: no Discord token (set DISCORD_BOT_TOKEN or configure in config.json)", file=sys.stderr)
        sys.exit(1)

    channel_id = resolve_hl_channel(config)
    if not channel_id:
        print("Error: no Discord channel configured for hyperliquid (check discord.channels in config.json)", file=sys.stderr)
        sys.exit(1)

    msg_id = post_to_discord(token, channel_id, summary)
    print(f"Posted summary to channel {channel_id} (message {msg_id})")


if __name__ == "__main__":
    main()
