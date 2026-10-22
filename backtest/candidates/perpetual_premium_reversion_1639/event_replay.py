import bisect
import math
from decimal import ROUND_FLOOR, Decimal
from typing import Dict, List, Optional

COST_CASES = {
    "x1": {"taker_fee_pct": 0.00045, "extra_slippage_bps": 0.0, "latency_ms": 1_000},
    "x2": {"taker_fee_pct": 0.00090, "extra_slippage_bps": 1.0, "latency_ms": 2_000},
}
MIN_NOTIONAL_USD = 10.0
MAX_FILL_WAIT_MS = 10_000


class QuoteTape:
    def __init__(self, rows: list):
        self.rows = sorted(rows, key=lambda r: (r[0], r[7]))
        self.recv = [r[0] for r in self.rows]

    def first_at_or_after(self, ms: int, start: int = 0) -> int:
        return bisect.bisect_left(self.recv, ms, lo=start)

    def latest_at_or_before(self, ms: int) -> Optional[list]:
        i = bisect.bisect_right(self.recv, ms) - 1
        return self.rows[i] if i >= 0 else None


def _floor_size(qty: float, decimals: int) -> float:
    step = Decimal(1).scaleb(-decimals)
    return float(Decimal(repr(qty)).quantize(step, rounding=ROUND_FLOOR))


def _dec(qty: float) -> Decimal:
    return Decimal(repr(qty))


def _take(tape: QuoteTape, at_ms: int, side: str, qty: float, decimals: int, cost: dict, used: Dict[int, float]) -> list:
    i = tape.first_at_or_after(at_ms)
    remaining = qty
    parts = []
    while remaining > 0 and i < len(tape.rows):
        row = tape.rows[i]
        if row[0] > at_ms + MAX_FILL_WAIT_MS:
            break
        px = float(row[4]) if side == "buy" else float(row[2])
        displayed = float(row[5]) if side == "buy" else float(row[3])
        key = (i, side)
        avail = displayed - used.get(key, 0.0)
        take = _floor_size(min(remaining, avail), decimals)
        if take > 0:
            used[key] = used.get(key, 0.0) + take
            adj = px * (1 + cost["extra_slippage_bps"] / 10_000) if side == "buy" else px * (1 - cost["extra_slippage_bps"] / 10_000)
            parts.append({"recv_ms": row[0], "event_ms": row[1], "touch_px": px, "px": adj, "qty": take, "seq": row[7]})
            remaining = round(remaining - take, decimals + 2)
        i += 1
    return parts


def _fills(parts: list) -> list:
    return [{"recv_ms": p["recv_ms"], "px": p["px"], "qty": p["qty"]} for p in parts]


def _vwap(parts: list, qty: float) -> float:
    return sum(p["px"] * p["qty"] for p in parts) / qty


def _fill(tape: QuoteTape, at_ms: int, side: str, qty: float, decimals: int, cost: dict, used: Dict[int, float]):
    parts = _take(tape, at_ms, side, qty, decimals, cost, used)
    if not parts:
        return None
    qty_dec = sum(_dec(p["qty"]) for p in parts)
    qty_filled = float(qty_dec)
    return {"qty": qty_filled, "qty_dec": qty_dec, "px": _vwap(parts, qty_filled), "first_recv_ms": parts[0]["recv_ms"],
            "last_recv_ms": parts[-1]["recv_ms"], "parts": len(parts), "fills": _fills(parts)}


def _close(tape: QuoteTape, at_ms: int, side: str, entered: Decimal, decimals: int, cost: dict, used: Dict[int, float]):
    parts = []
    windows = 0
    remaining = entered
    start = at_ms
    while remaining > 0 and tape.first_at_or_after(start) < len(tape.rows):
        windows += 1
        parts += _take(tape, start, side, float(remaining), decimals, cost, used)
        remaining = entered - sum((_dec(p["qty"]) for p in parts), Decimal(0))
        nxt = bisect.bisect_right(tape.recv, start + MAX_FILL_WAIT_MS)
        if nxt >= len(tape.rows):
            break
        start = tape.rows[nxt][0]
    return parts, windows, remaining


def _open_qty(entry_fills: list, exit_fills: list, ts: int) -> Decimal:
    held = sum((_dec(f["qty"]) for f in entry_fills if f["recv_ms"] < ts), Decimal(0))
    return held - sum((_dec(f["qty"]) for f in exit_fills if f["recv_ms"] < ts), Decimal(0))


def _funding(funding: list, fund_ts: list, tape: QuoteTape, sign: int, entry_fills: list, exit_fills: list,
             through_ms: int) -> float:
    total = 0.0
    lo = bisect.bisect_right(fund_ts, entry_fills[0]["recv_ms"])
    hi = bisect.bisect_right(fund_ts, through_ms)
    for ts, rate in funding[lo:hi]:
        open_qty = _open_qty(entry_fills, exit_fills, ts)
        if open_qty <= 0:
            continue
        q = tape.latest_at_or_before(ts)
        if q is None:
            continue
        mid = (float(q[2]) + float(q[4])) / 2
        total -= sign * float(open_qty) * mid * rate
    return total


def replay_coin(coin: str, decisions: list, tape: QuoteTape, funding: list, size_decimals: int, capital: float,
                max_bars: int, bar_ms: int, stop_atr_mult: float, cost: dict) -> dict:
    trades = []
    refusals = []
    open_positions = []
    used: Dict[int, float] = {}
    busy_until = 0
    fund_ts = [f[0] for f in funding]
    tape_end_ms = tape.rows[-1][0] if tape.rows else 0
    for d in decisions:
        if d["signal"] == 0 or d["decision_ms"] < busy_until:
            continue
        side = "long" if d["signal"] > 0 else "short"
        entry_at = d["decision_ms"] + cost["latency_ms"]
        probe = tape.first_at_or_after(entry_at)
        if probe >= len(tape.rows) or tape.rows[probe][0] > entry_at + MAX_FILL_WAIT_MS:
            refusals.append({"decision_ms": d["decision_ms"], "side": side, "reason": "no_executable_event"})
            continue
        ref_px = float(tape.rows[probe][4]) if side == "long" else float(tape.rows[probe][2])
        qty = _floor_size(capital / ref_px, size_decimals)
        if qty * ref_px < MIN_NOTIONAL_USD or qty <= 0:
            refusals.append({"decision_ms": d["decision_ms"], "side": side, "reason": "below_venue_minimum"})
            continue
        entry = _fill(tape, entry_at, "buy" if side == "long" else "sell", qty, size_decimals, cost, used)
        if entry is None:
            refusals.append({"decision_ms": d["decision_ms"], "side": side, "reason": "no_displayed_size"})
            continue
        atr = d.get("atr")
        stop = None
        if atr is not None and math.isfinite(atr) and atr > 0 and stop_atr_mult > 0:
            stop = entry["px"] - stop_atr_mult * atr if side == "long" else entry["px"] + stop_atr_mult * atr
        time_exit_decision = d["decision_ms"] + max_bars * bar_ms
        exit_reason = "time_stop"
        exit_decision = time_exit_decision
        if stop is not None:
            i = tape.first_at_or_after(entry["last_recv_ms"] + 1)
            while i < len(tape.rows) and tape.rows[i][0] < time_exit_decision:
                row = tape.rows[i]
                if (side == "long" and float(row[2]) <= stop) or (side == "short" and float(row[4]) >= stop):
                    exit_reason, exit_decision = "sl", row[0]
                    break
                i += 1
        exit_at = exit_decision + cost["latency_ms"]
        parts, windows, remaining = _close(tape, exit_at, "sell" if side == "long" else "buy", entry["qty_dec"],
                                           size_decimals, cost, used)
        exit_fills = _fills(parts)
        closed_dec = entry["qty_dec"] - remaining
        closed = float(closed_dec)
        sign = 1 if side == "long" else -1
        record = {"coin": coin, "side": side, "decision_ms": d["decision_ms"], "entry_ms": entry["first_recv_ms"],
                  "entry_px": entry["px"], "entry_parts": entry["parts"], "entry_fills": entry["fills"],
                  "stop": stop, "exit_reason": exit_reason, "exit_decision_ms": exit_decision,
                  "exit_fills": exit_fills, "exit_windows": windows, "premium": d.get("premium"),
                  "z_extreme": d.get("z_extreme")}
        if remaining > 0:
            exit_px = _vwap(parts, closed) if parts else None
            entry_fee = cost["taker_fee_pct"] * entry["px"] * entry["qty"]
            exit_fee = cost["taker_fee_pct"] * exit_px * closed if parts else 0.0
            realized = sign * (exit_px - entry["px"]) * closed if parts else 0.0
            funding_pnl = _funding(funding, fund_ts, tape, sign, entry["fills"], exit_fills, tape_end_ms)
            open_positions.append({**record, "status": "open_at_recording_end", "qty": entry["qty"],
                                   "closed_qty": closed, "remaining_qty": float(remaining), "exit_px": exit_px,
                                   "exit_ms": parts[-1]["recv_ms"] if parts else None,
                                   "entry_fees_usd": entry_fee, "exit_fees_usd": exit_fee,
                                   "fees_usd": entry_fee + exit_fee, "realized_gross_usd": realized,
                                   "funding_usd": funding_pnl, "funding_through_ms": tape_end_ms,
                                   "realized_net_usd": realized - entry_fee - exit_fee + funding_pnl})
            busy_until = math.inf
            continue
        exit_px = _vwap(parts, closed)
        gross = sign * (exit_px - entry["px"]) * closed
        fees = cost["taker_fee_pct"] * (entry["px"] + exit_px) * closed
        funding_pnl = _funding(funding, fund_ts, tape, sign, entry["fills"], exit_fills, parts[-1]["recv_ms"])
        net = gross - fees + funding_pnl
        trades.append({**record, "qty": closed, "exit_ms": parts[-1]["recv_ms"], "exit_px": exit_px,
                       "exit_delayed": windows > 1, "gross_usd": gross, "fees_usd": fees,
                       "funding_usd": funding_pnl, "net_usd": net})
        busy_until = parts[-1]["recv_ms"]
    return {"trades": trades, "refusals": refusals, "open_positions": open_positions}


def open_exposure(open_positions: list) -> dict:
    remaining = {}
    for p in open_positions:
        remaining[p["coin"]] = remaining.get(p["coin"], Decimal(0)) + _dec(p["remaining_qty"])
    n = len(open_positions)
    return {
        "complete": n == 0,
        "statistics_scope": "closed_positions_only",
        "incomplete_reason": None if n == 0 else (f"{n} position(s) still open at the end of the recording; "
                                                  "closed-position statistics exclude their exposure and costs"),
        "open_positions": n,
        "open_remaining_qty": {k: float(v) for k, v in sorted(remaining.items())},
        "open_fees_usd": round(sum(p["fees_usd"] for p in open_positions), 6),
        "open_realized_gross_usd": round(sum(p["realized_gross_usd"] for p in open_positions), 6),
        "open_funding_usd": round(sum(p["funding_usd"] for p in open_positions), 6),
        "open_realized_net_usd": round(sum(p["realized_net_usd"] for p in open_positions), 6),
    }


def summarize(trades: list, capital: float, open_positions: Optional[list] = None) -> dict:
    exposure = open_exposure(open_positions or [])
    if not trades:
        return {"positions": 0, "net_pnl_usd": 0.0, "gross_usd": 0.0, "fees_usd": 0.0, "funding_usd": 0.0,
                "long_positions": 0, "short_positions": 0, "max_dd_usd": 0.0, "win_rate": None, "per_coin": {},
                "top_coin_abs_pnl_share": None, "turnover_usd": 0.0, **exposure}
    per = {}
    for t in trades:
        p = per.setdefault(t["coin"], {"positions": 0, "net_pnl_usd": 0.0})
        p["positions"] += 1
        p["net_pnl_usd"] += t["net_usd"]
    equity = 0.0
    peak = 0.0
    dd = 0.0
    for t in sorted(trades, key=lambda x: x["exit_ms"]):
        equity += t["net_usd"]
        peak = max(peak, equity)
        dd = min(dd, equity - peak)
    abs_coin = [abs(v["net_pnl_usd"]) for v in per.values()]
    net = sum(t["net_usd"] for t in trades)
    return {
        "positions": len(trades),
        "long_positions": sum(1 for t in trades if t["side"] == "long"),
        "short_positions": sum(1 for t in trades if t["side"] == "short"),
        "net_pnl_usd": round(net, 6),
        "gross_usd": round(sum(t["gross_usd"] for t in trades), 6),
        "fees_usd": round(sum(t["fees_usd"] for t in trades), 6),
        "funding_usd": round(sum(t["funding_usd"] for t in trades), 6),
        "return_pct_of_capital_per_coin": round(100 * net / (capital * max(1, len(per))), 6),
        "max_dd_usd": round(dd, 6),
        "win_rate": round(sum(1 for t in trades if t["net_usd"] > 0) / len(trades), 4),
        "turnover_usd": round(sum(t["qty"] * (t["entry_px"] + t["exit_px"]) for t in trades), 2),
        "delayed_exits": sum(1 for t in trades if t["exit_delayed"]),
        "per_coin": {k: {"positions": v["positions"], "net_pnl_usd": round(v["net_pnl_usd"], 6)} for k, v in sorted(per.items())},
        "top_coin_abs_pnl_share": round(max(abs_coin) / sum(abs_coin), 4) if sum(abs_coin) > 0 else None,
        **exposure,
    }
