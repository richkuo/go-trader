#!/usr/bin/env python3
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

EXCLUDED_TRADE_TYPES = {"scale_in", "funding", "hedge"}
HISTORY_PAGE = 200
LAST_N = 5
DISCORD_CONTENT_LIMIT = 2000
DEDUPE_WINDOW_S = 600


def parse_args():
    p = argparse.ArgumentParser(
        description="Post the cross-coin paper leaderboard from a go-trader status server to a Discord channel. "
        "Reads DISCORD_BOT_TOKEN, and GO_TRADER_STATUS_TOKEN when the status server has a token."
    )
    p.add_argument("--port", type=int, required=True, help="status_port of the paper service")
    p.add_argument("--channel", required=True, help="Discord channel id")
    p.add_argument("--top", type=int, default=15, help="rows to post (default 15)")
    p.add_argument("--dry-run", action="store_true", help="print the message and post nothing")
    p.add_argument(
        "--sentinel",
        default=os.path.join(
            os.environ.get("XDG_STATE_HOME") or os.path.expanduser("~/.local/state"),
            "go-trader-paper-leaderboard.sentinel",
        ),
        help="dedupe marker file (default: $XDG_STATE_HOME or ~/.local/state)",
    )
    args = p.parse_args()
    if args.top < 1:
        p.error("--top must be at least 1")
    return args


def api_get(port, path, params=None):
    url = f"http://localhost:{port}{path}"
    if params:
        url += "?" + urllib.parse.urlencode(params)
    req = urllib.request.Request(url)
    token = os.environ.get("GO_TRADER_STATUS_TOKEN", "")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read())


def trade_net_pnl(t):
    pnl = t.get("realized_pnl") or 0.0
    if t.get("pnl_gross"):
        return pnl - (t.get("exchange_fee") or 0.0)
    return pnl


def recent_activity(port, strategy_id):
    last_ts = None
    order = []
    nets = {}
    offset = 0
    while True:
        page = api_get(port, "/history", {"strategy": strategy_id, "limit": HISTORY_PAGE, "offset": offset})
        trades = page.get("trades") or []
        for i, t in enumerate(trades):
            if last_ts is None:
                last_ts = t.get("timestamp")
            if not t.get("is_close") or t.get("trade_type") in EXCLUDED_TRADE_TYPES:
                continue
            key = t.get("position_id") or f"legacy:{offset + i}"
            if key not in nets:
                order.append(key)
                nets[key] = 0.0
            nets[key] += trade_net_pnl(t)
        offset += len(trades)
        if len(order) > LAST_N or not trades or offset >= (page.get("total") or 0):
            break
    outcomes = []
    for key in reversed(order[:LAST_N]):
        net = nets[key]
        outcomes.append("W" if net > 0 else "L" if net < 0 else "B")
    return outcomes, last_ts


def fmt_month_day(ts):
    if not ts:
        return None
    date_part = ts.split("T")[0].split(" ")[0]
    parts = date_part.split("-")
    if len(parts) >= 3:
        try:
            mo = int(parts[1])
            dy = int(parts[2])
            if 1 <= mo <= 12:
                return f"{mo:02d}-{dy:02d}"
        except ValueError:
            pass
    return date_part[5:10]


def should_post(sentinel):
    if os.environ.get("PAPER_LB_FORCE") == "1":
        print("[dedupe] PAPER_LB_FORCE=1, bypassing dedupe")
        return True
    try:
        mtime = os.lstat(sentinel).st_mtime
    except FileNotFoundError:
        return True
    age = time.time() - mtime
    if age < DEDUPE_WINDOW_S:
        print(
            f"[SKIP] Last post was {int(age // 60)}m{int(age % 60):02d}s ago "
            f"(sentinel={sentinel}); within dedupe window of {DEDUPE_WINDOW_S}s. Refusing to post."
        )
        return False
    return True


def mark_posted(sentinel):
    os.makedirs(os.path.dirname(sentinel), mode=0o700, exist_ok=True)
    fd = os.open(sentinel, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(str(time.time()))


def post_discord_message(token, channel_id, content):
    url = f"https://discord.com/api/v10/channels/{channel_id}/messages"
    req = urllib.request.Request(url, data=json.dumps({"content": content}).encode(), method="POST")
    req.add_header("Authorization", f"Bot {token}")
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "go-trader-paper-leaderboard/1.0")
    try:
        with urllib.request.urlopen(req, timeout=10):
            print(f"  Posted to channel {channel_id}")
            return True
    except urllib.error.HTTPError as e:
        print(f"[ERROR] Discord API {e.code}: {e.read().decode()[:200]}", file=sys.stderr)
    except Exception as e:
        print(f"[ERROR] Discord send failed: {e}", file=sys.stderr)
    return False


def display_names(entries):
    stripped = {e["id"]: re.sub(r"-paper-[A-Za-z0-9_]+$", "", e["id"]) for e in entries}
    counts = {}
    for name in stripped.values():
        counts[name] = counts.get(name, 0) + 1
    return {sid: name if counts[name] == 1 else sid for sid, name in stripped.items()}


def build_message(top, total_count, names):
    rows = []
    for i, e in enumerate(top, 1):
        closed = e["wins"] + e["losses"]
        rows.append([
            str(i),
            names[e["id"]],
            f"{e['pnl_pct']:+.1f}%",
            f"{e['pnl']:+,.0f}",
            str(e["trades"]),
            f"{e['wins']}/{e['losses']}",
            f"{round(e['wins'] / closed * 100)}%" if closed > 0 else "-",
            "".join(e["outcomes"]) or "-",
            e["last_dt"] or "-",
        ])
    header = ["#", "Strategy", "PnL%", "$PnL", "#T", "W/L", "Win%", "Last 5", "Last"]
    left = {1, 7, 8}
    widths = [max(len(r[c]) for r in rows + [header]) for c in range(len(header))]

    def fmt(r):
        return "  ".join(v.ljust(w) if c in left else v.rjust(w) for c, (v, w) in enumerate(zip(r, widths))).rstrip()

    head = fmt(header)
    lines = [f"**Top {len(top)} Paper Trading Strategies**", "```", head, "-" * len(head)]
    lines += [fmt(r) for r in rows]
    lines.append("```")
    lines.append(f"_Across {total_count} strategies on the paper instance_")
    return "\n".join(lines)


def main():
    args = parse_args()
    try:
        board = api_get(args.port, "/api/leaderboard")
    except Exception as e:
        print(f"[ERROR] /api/leaderboard on port {args.port}: {e}", file=sys.stderr)
        sys.exit(1)
    entries = [e for e in board.get("entries") or [] if (e.get("capital") or 0) > 0]
    if not entries:
        print("[ERROR] No strategies found", file=sys.stderr)
        sys.exit(1)
    entries.sort(key=lambda e: e.get("pnl_pct") or 0.0, reverse=True)

    top = []
    for e in entries[: args.top]:
        try:
            outcomes, last_ts = recent_activity(args.port, e["id"])
        except Exception as err:
            print(f"[ERROR] /history for {e['id']}: {err}", file=sys.stderr)
            sys.exit(1)
        top.append({
            "id": e["id"],
            "pnl": e.get("pnl") or 0.0,
            "pnl_pct": e.get("pnl_pct") or 0.0,
            "trades": e.get("positions_opened") or 0,
            "wins": e.get("wins") or 0,
            "losses": e.get("losses") or 0,
            "outcomes": outcomes,
            "last_dt": fmt_month_day(last_ts),
        })

    stats_empty = all(
        (e.get("positions_opened") or 0) == 0 and (e.get("wins") or 0) + (e.get("losses") or 0) == 0
        for e in entries
    )
    if stats_empty and any(e["outcomes"] for e in top):
        print(
            "[ERROR] /api/leaderboard reports no trades for any strategy, but /history shows closed trades; "
            "refusing to post an all-zero leaderboard",
            file=sys.stderr,
        )
        sys.exit(1)

    content = build_message(top, len(entries), display_names(entries))
    print(content)
    print()
    if len(content) > DISCORD_CONTENT_LIMIT:
        print(
            f"[ERROR] message is {len(content)} characters, over Discord's {DISCORD_CONTENT_LIMIT}; "
            "lower --top",
            file=sys.stderr,
        )
        sys.exit(1)
    if args.dry_run:
        return

    token = os.environ.get("DISCORD_BOT_TOKEN", "")
    if not token:
        print("[ERROR] DISCORD_BOT_TOKEN is not set", file=sys.stderr)
        sys.exit(1)
    if not should_post(args.sentinel):
        return
    if not post_discord_message(token, args.channel, content):
        sys.exit(1)
    mark_posted(args.sentinel)
    print(f"[dedupe] Sentinel updated; next run within {DEDUPE_WINDOW_S}s will be skipped.")


if __name__ == "__main__":
    main()
