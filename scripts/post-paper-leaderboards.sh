#!/usr/bin/env python3
"""
Post top-15 paper trading leaderboard across all strategies on the
consolidated paper deployment. Queries the single paper instance's
/status + scheduler/state.db, ranks by PnL%, and posts the top 15 to
the hl-paper-leaderboard Discord channel.

After the paper-fold migration (2026-09-30) the five per-coin paper
deployments were consolidated into a single `paper` deployment. This
script reads from that single consolidated deployment rather than the
five rolled-back per-coin paths.
"""
import json, os, sys, sqlite3, time, urllib.request, urllib.error

CHANNEL_ID = "1490924126712365115"
# Consolidated paper deployment: port 8103, scheduler/state.db under /opt.
INSTANCES = [
    ("go-trader-paper", 8103),
]
WORKSPACE = "/opt"
TOP_N = 15

# Dedup: the OpenClaw cron that drives this script (job
# 9801d67b-… at "0 0,12 * * *") has been double-posting when the primary
# AI service hits FailoverError ~100s into the run — the AI runs the
# script once before failing, then OpenClaw retries ~2-5 min later and
# runs it again. Sentinel file in /tmp blocks any second post within the
# window. 10 min covers every observed retry gap (max 5m30s); anything
# at the legit 12h cadence is unaffected. Set PAPER_LB_FORCE=1 to bypass.
SENTINEL_PATH = "/tmp/.paper-leaderboard-sentinel"
DEDUPE_WINDOW_S = 600


def _should_post():
    if os.environ.get("PAPER_LB_FORCE") == "1":
        print("[dedupe] PAPER_LB_FORCE=1 — bypassing dedupe")
        return True
    try:
        mtime = os.stat(SENTINEL_PATH).st_mtime
    except FileNotFoundError:
        return True
    age = time.time() - mtime
    if age < DEDUPE_WINDOW_S:
        mins = int(age // 60)
        secs = int(age % 60)
        print(
            f"[SKIP] Last post was {mins}m{secs:02d}s ago "
            f"(sentinel={SENTINEL_PATH}); within dedupe window of "
            f"{DEDUPE_WINDOW_S}s. Refusing to post."
        )
        return False
    return True


def _mark_posted():
    # Use open()+close rather than os.utime so a missing file gets created.
    with open(SENTINEL_PATH, "w") as f:
        f.write(str(time.time()))

def fetch_status(port):
    try:
        with urllib.request.urlopen(f"http://localhost:{port}/status", timeout=5) as r:
            return json.loads(r.read())
    except Exception as e:
        print(f"  [WARN] port {port}: {e}", file=sys.stderr)
        return None

def get_discord_token():
    # Use OpenClaw bot token (has access to all server channels)
    cfg_path = "/root/.openclaw/openclaw.json"
    with open(cfg_path) as f:
        return json.load(f).get("channels", {}).get("discord", {}).get("token", "")

def post_discord_message(token, channel_id, content):
    url = f"https://discord.com/api/v10/channels/{channel_id}/messages"
    body = json.dumps({"content": content}).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Authorization", f"Bot {token}")
    req.add_header("Content-Type", "application/json")
    req.add_header("User-Agent", "OpenClaw-PaperLeaderboard/1.0")
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            print(f"  Posted to channel {channel_id}")
            return True
    except urllib.error.HTTPError as e:
        print(f"  [ERROR] Discord API {e.code}: {e.read().decode()[:200]}", file=sys.stderr)
        return False
    except Exception as e:
        print(f"  [ERROR] Discord send failed: {e}", file=sys.stderr)
        return False

def fetch_winloss(name):
    """Query closed_positions from the paper instance's scheduler/state.db
    for W/L stats. The single consolidated deployment holds all paper
    strategies, so one DB read covers everything."""
    db_path = f"{WORKSPACE}/{name}/scheduler/state.db"
    stats = {}
    try:
        conn = sqlite3.connect(db_path)
        conn.row_factory = sqlite3.Row
        rows = conn.execute(
            "SELECT strategy_id, COUNT(*) as total, "
            "SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END) as wins, "
            "SUM(CASE WHEN realized_pnl <= 0 THEN 1 ELSE 0 END) as losses "
            "FROM closed_positions GROUP BY strategy_id"
        ).fetchall()
        for r in rows:
            stats[r["strategy_id"]] = {"wins": r["wins"], "losses": r["losses"]}
        conn.close()
    except Exception as e:
        print(f"  [WARN] winloss {name}: {e}", file=sys.stderr)
    return stats

MONTH_ABBR = ["", "Jan", "Feb", "Mar", "Apr", "May", "Jun",
              "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]

def _fmt_last_dt(ts):
    """Convert ISO 8601 timestamp to 'MM-DD' (zero-padded month-day)."""
    if not ts:
        return None
    # ISO 8601 may use 'T' or ' ' between date and time
    date_part = ts.split("T")[0].split(" ")[0]
    parts = date_part.split("-")
    if len(parts) >= 3:
        try:
            mo = int(parts[1])
            dy = int(parts[2])
            if 1 <= mo <= 12:
                return f"{mo:02d}-{dy:02d}"
        except (ValueError, IndexError):
            pass
    return date_part[5:10]  # fallback: MM-DD slice of YYYY-MM-DD

def fetch_last_trade_date(name):
    """Most recent trade timestamp per strategy (any trade, open or close).
    Returns {strategy_id: "MM-DD"} — month-day of the last trade."""
    db_path = f"{WORKSPACE}/{name}/scheduler/state.db"
    out = {}
    try:
        conn = sqlite3.connect(db_path)
        conn.row_factory = sqlite3.Row
        # ORDER BY ... DESC and take the first row per strategy in Python.
        # Uses idx_trades_strategy_timestamp for an index-only scan.
        rows = conn.execute(
            "SELECT strategy_id, timestamp FROM trades "
            "WHERE timestamp != '' "
            "ORDER BY strategy_id, timestamp DESC, rowid DESC"
        ).fetchall()
        conn.close()
        for r in rows:
            sid = r["strategy_id"]
            if sid in out:
                continue
            out[sid] = _fmt_last_dt(r["timestamp"])
    except Exception as e:
        print(f"  [WARN] last_trade_date {name}: {e}", file=sys.stderr)
    return out

def fetch_last_outcomes(name, n=5):
    """Query the most recent N closed trades per strategy (is_close=1).
    Returns {strategy_id: ["W"|"L", ...]} ordered oldest-to-newest so
    the rightmost character is the most recent close. Open trades are
    ignored — only completed trades have a realized outcome. If a
    strategy has fewer than N closes, the list is shorter."""
    db_path = f"{WORKSPACE}/{name}/scheduler/state.db"
    out = {}
    try:
        conn = sqlite3.connect(db_path)
        conn.row_factory = sqlite3.Row
        # DESC sort pulls newest first; we cap at N per strategy in Python
        # below. Fetching all rows then trimming avoids per-strategy
        # window functions and keeps the SQL portable.
        rows = conn.execute(
            "SELECT strategy_id, realized_pnl FROM trades "
            "WHERE is_close = 1 "
            "ORDER BY strategy_id, timestamp DESC"
        ).fetchall()
        conn.close()
        for r in rows:
            sid = r["strategy_id"]
            bucket = out.setdefault(sid, [])
            if len(bucket) >= n:
                continue
            bucket.append("W" if r["realized_pnl"] > 0 else "L")
        # Oldest-to-newest so the rightmost char = most recent close.
        for sid in out:
            out[sid].reverse()
    except Exception as e:
        print(f"  [WARN] last_outcomes {name}: {e}", file=sys.stderr)
    return out

def main():
    entries = []
    for name, port in INSTANCES:
        d = fetch_status(port)
        if not d:
            continue
        wl = fetch_winloss(name)
        outcomes = fetch_last_outcomes(name, n=5)
        last_dt = fetch_last_trade_date(name)
        for sid, s in d.get("strategies", {}).items():
            pnl = s.get("pnl", 0)
            cap = s.get("initial_capital", 1)
            if cap <= 0:
                continue
            pnl_pct = (pnl / cap) * 100
            w = wl.get(sid, {})
            entries.append({
                "id": sid,
                "pnl": pnl,
                "cap": cap,
                "pnl_pct": pnl_pct,
                "cash": s.get("cash", 0),
                "instance": name,
                "wins": w.get("wins", 0),
                "losses": w.get("losses", 0),
                "trades": w.get("wins", 0) + w.get("losses", 0),
                "outcomes": outcomes.get(sid, []),
                "last_dt": last_dt.get(sid),
            })

    if not entries:
        print("[ERROR] No strategies found", file=sys.stderr)
        sys.exit(1)

    entries.sort(key=lambda x: x["pnl_pct"], reverse=True)
    top = entries[:TOP_N]

    lines = ["🏆 **Top 15 Paper Trading Strategies**", "```"]
    lines.append(f"{'#':>2}  {'Strategy':<19} {'PnL%':>6}  {'$PnL':>9}  {'Trades':>6}  {'W/L (Win%)':>13}  {'Last 5':<7}  {'Last':<6}")
    lines.append("-" * 82)
    for i, e in enumerate(top, 1):
        total = e['wins'] + e['losses']
        if total > 0:
            win_pct = round(e['wins'] / total * 100)
        else:
            win_pct = 0
        wl_str = f"{e['wins']}/{e['losses']} ({win_pct}%)"
        # Last-5 column = sequence of W/L for the most recent closed
        # trades, oldest-to-newest so rightmost = newest. Dashes when
        # the strategy has no closed trades on record.
        outcomes = e.get("outcomes") or []
        last_str = "".join(outcomes) if outcomes else "—"
        # Last column = MM-DD of the most recent trade (open or close).
        # Dashes when the strategy has no trades on record.
        last_dt_str = e.get("last_dt") or "—"
        lines.append(
            f"{i:>2}  {e['id']:<19} {e['pnl_pct']:>+6.2f}  "
            f"{e['pnl']:>+9.2f}  {e['trades']:>6}  {wl_str:>13}  "
            f"{last_str:<7}  {last_dt_str:<6}"
        )
    lines.append("```")
    lines.append(f"_Across {len(entries)} strategies on the paper instance_")

    content = "\n".join(lines)
    print(content)
    print()

    token = get_discord_token()
    if not token:
        print("[ERROR] No Discord token found", file=sys.stderr)
        sys.exit(1)
    if not _should_post():
        sys.exit(0)
    if post_discord_message(token, CHANNEL_ID, content):
        _mark_posted()
        print("[dedupe] Sentinel updated; next run within "
              f"{DEDUPE_WINDOW_S}s will be skipped.")

if __name__ == "__main__":
    main()