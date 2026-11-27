import math
from decimal import ROUND_CEILING, ROUND_FLOOR, Decimal

ARM_PARAMS = {
    "stop_atr_mult": 1.0,
    "trail_atr_mult": 1.0,
    "tp1_atr_mult": 1.5,
    "tp1_fraction": 0.5,
    "limit_expiry_bars": 6,
    "structural_buffer_ticks": 2,
    "max_hold_bars": 96,
    "risk_pct": 0.5,
    "leverage": 1.0,
}

ENTRY_ARMS = ("market", "ce_limit")
STOP_ARMS = ("atr", "structural")
TARGET_ARMS = ("tiered", "pool")
REFERENCE_ARM = {"entry": "market", "stop": "atr", "target": "tiered"}


def arm_matrix():
    return [{"entry": e, "stop": s, "target": t} for e in ENTRY_ARMS for s in STOP_ARMS for t in TARGET_ARMS]


def arm_label(arm):
    return f"{arm['entry']}+{arm['stop']}+{arm['target']}"


def bar_path(o, h, lo, c):
    return [o, lo, h, c] if c >= o else [o, h, lo, c]


def floor_qty(qty, decimals):
    if not math.isfinite(qty) or qty <= 0:
        return 0.0
    step = Decimal(1).scaleb(-int(decimals))
    return float(Decimal(repr(float(qty))).quantize(step, rounding=ROUND_FLOOR))


def round_to_tick(px, tick, side, passive=True):
    step = Decimal(repr(float(tick)))
    units = Decimal(repr(float(px))) / step
    down = (side > 0) == passive
    units = units.to_integral_value(rounding=ROUND_FLOOR if down else ROUND_CEILING)
    return float(units * step)


def _segments(path, start):
    pts = path[start:]
    return [(pts[k], pts[k + 1]) for k in range(len(pts) - 1)]


def _crossed(a, b, level):
    lo, hi = (a, b) if a <= b else (b, a)
    return lo <= level <= hi


class _Ledger:
    def __init__(self, capital):
        self.capital = capital
        self.equity = capital
        self.peak = capital
        self.max_dd = 0.0
        self.trades = []

    def book(self, trade):
        self.equity += trade["net_pnl"]
        self.peak = max(self.peak, self.equity)
        if self.peak > 0:
            self.max_dd = max(self.max_dd, (self.peak - self.equity) / self.peak)
        self.trades.append(trade)


def _funding_charge(funding, start_ms, end_ms, side, qty_at, mark_at):
    total = 0.0
    for ts, rate in funding:
        if start_ms < ts <= end_ms:
            qty = qty_at(ts)
            if qty > 0:
                total -= side * qty * mark_at(ts) * rate
    return total


def simulate(bars, setups, arm, *, costs, sz_decimals, tick, capital, funding=(), interval_ms, params=None):
    p = {**ARM_PARAMS, **(params or {})}
    o, h, lo, c, opens = bars["open"], bars["high"], bars["low"], bars["close"], bars["open_ms"]
    n = len(c)
    taker = costs["taker_fee_pct"]
    maker = costs["maker_fee_pct"]
    adverse = costs["half_spread_pct"] + costs["slippage_pct"]
    min_notional = costs["min_notional_usd"]
    ledger = _Ledger(capital)
    tallies = {"setups": 0, "placed": 0, "filled": 0, "no_fill": 0, "skipped_in_position": 0, "stop_invalid": 0,
               "below_min_notional": 0, "partial_skipped": 0}
    busy_until = -1

    def mark_at(ts):
        k = max(0, min(n - 1, (ts - opens[0]) // interval_ms))
        return float(c[int(k)])

    for st in sorted(setups, key=lambda s: s["i"]):
        tallies["setups"] += 1
        i, side = st["i"], st["side"]
        if i <= busy_until:
            tallies["skipped_in_position"] += 1
            continue
        if i + 1 >= n:
            continue
        tallies["placed"] += 1
        atr = st["atr"]
        fill_k, fill_px, fill_idx, entry_fee_rate, fill_kind = None, None, 0, taker, "market_next_open"
        if arm["entry"] == "market":
            fill_k = i + 1
            fill_px = o[fill_k] * (1 + side * adverse)
            fill_idx = 0
        else:
            limit = round_to_tick(st["entry_price"], tick, side)
            for k in range(i + 1, min(n, i + 1 + p["limit_expiry_bars"])):
                through = limit - side * tick
                if (side > 0 and o[k] <= through) or (side < 0 and o[k] >= through):
                    fill_k, fill_px, fill_idx, entry_fee_rate, fill_kind = k, float(o[k]), 0, maker, "limit_gap_open"
                    break
                path = bar_path(o[k], h[k], lo[k], c[k])
                hit = None
                for seg in range(3):
                    if _crossed(path[seg], path[seg + 1], through):
                        hit = seg
                        break
                if hit is not None:
                    fill_k, fill_px, fill_idx, entry_fee_rate, fill_kind = k, limit, hit + 1, maker, "limit_trade_through"
                    break
                if (side > 0 and c[k] < st["wick"]) or (side < 0 and c[k] > st["wick"]):
                    break
            if fill_k is None:
                tallies["no_fill"] += 1
                busy_until = max(busy_until, i)
                continue
        if arm["stop"] == "atr":
            stop = fill_px - side * p["stop_atr_mult"] * atr
        else:
            stop = st["wick"] - side * p["structural_buffer_ticks"] * tick
        dist = (fill_px - stop) * side
        if not math.isfinite(dist) or dist <= 0:
            tallies["stop_invalid"] += 1
            continue
        requested_risk = ledger.equity * p["risk_pct"] / 100.0
        qty = requested_risk / dist
        qty = min(qty, ledger.equity * p["leverage"] / fill_px)
        qty = floor_qty(qty, sz_decimals)
        if qty * fill_px < min_notional:
            tallies["below_min_notional"] += 1
            continue
        tallies["filled"] += 1
        if arm["target"] == "tiered":
            tp1 = fill_px + side * p["tp1_atr_mult"] * atr
        else:
            pools = [x for x in st.get("pools", ()) if x is not None and (x - fill_px) * side > 0]
            tp1 = (min(pools) if side > 0 else max(pools)) if pools else None
        tp1_qty = floor_qty(qty * p["tp1_fraction"], sz_decimals) if tp1 is not None else 0.0
        if tp1 is not None and (tp1_qty * tp1 < min_notional or (qty - tp1_qty) * fill_px < min_notional):
            tp1, tp1_qty = None, 0.0
            tallies["partial_skipped"] += 1
        fees = qty * fill_px * entry_fee_rate
        realized = 0.0
        remaining = qty
        legs = []
        best = fill_px
        cur_stop = stop
        exit_k = None
        qty_history = [(int(opens[fill_k]), qty)]
        for k in range(fill_k, n):
            path = bar_path(o[k], h[k], lo[k], c[k])
            start = 0
            if k == fill_k and fill_kind == "limit_trade_through":
                path = [fill_px] + path[fill_idx:]
            if k != fill_k:
                if (side > 0 and o[k] <= cur_stop) or (side < 0 and o[k] >= cur_stop):
                    px = o[k] * (1 - side * adverse)
                    realized += side * remaining * (px - fill_px)
                    fees += remaining * px * taker
                    legs.append({"kind": "stop_gap", "bar": k, "px": px, "qty": remaining})
                    remaining = 0.0
                    exit_k = k
                    break
            done = False
            for a, b in _segments(path, start):
                moving_against = (b - a) * side < 0
                if moving_against and _crossed(a, b, cur_stop):
                    px = cur_stop * (1 - side * adverse)
                    realized += side * remaining * (px - fill_px)
                    fees += remaining * px * taker
                    legs.append({"kind": "stop", "bar": k, "px": px, "qty": remaining})
                    remaining = 0.0
                    done = True
                    break
                if not moving_against and tp1 is not None and tp1_qty > 0 and remaining == qty and _crossed(a, b, tp1):
                    realized += side * tp1_qty * (tp1 - fill_px)
                    fees += tp1_qty * tp1 * maker
                    legs.append({"kind": "tp1", "bar": k, "px": tp1, "qty": tp1_qty})
                    remaining = qty - tp1_qty
                    qty_history.append((int(opens[k]), remaining))
            if done:
                exit_k = k
                break
            if k - fill_k >= p["max_hold_bars"]:
                px = c[k] * (1 - side * adverse)
                realized += side * remaining * (px - fill_px)
                fees += remaining * px * taker
                legs.append({"kind": "time", "bar": k, "px": px, "qty": remaining})
                remaining = 0.0
                exit_k = k
                break
            best = max(best, h[k]) if side > 0 else min(best, lo[k])
            trail = best - side * p["trail_atr_mult"] * atr
            cur_stop = max(cur_stop, trail) if side > 0 else min(cur_stop, trail)
        if exit_k is None:
            k = n - 1
            px = c[k]
            realized += side * remaining * (px - fill_px)
            legs.append({"kind": "end_of_data", "bar": k, "px": px, "qty": remaining})
            remaining = 0.0
            exit_k = k

        def qty_at(ts, hist=qty_history):
            q = 0.0
            for t0, qv in hist:
                if ts > t0:
                    q = qv
            return q

        fund = _funding_charge(funding, int(opens[fill_k]), int(opens[exit_k]) + interval_ms, side, qty_at, mark_at)
        net = realized - fees + fund
        busy_until = exit_k
        ledger.book({
            "setup_id": st.get("setup_id", ""), "side": "long" if side > 0 else "short", "decision_bar": i,
            "fill_bar": fill_k, "fill_kind": fill_kind, "fill_px": fill_px, "qty": qty, "notional": qty * fill_px,
            "initial_stop": stop, "requested_risk_usd": requested_risk, "initial_stop_risk_usd": qty * dist,
            "tp1": tp1, "exit_bar": exit_k, "legs": legs, "gross_pnl": realized, "fees": fees, "funding": fund,
            "net_pnl": net, "loss_beyond_initial_risk": (-net) > qty * dist,
        })
    trades = ledger.trades
    wins = [t["net_pnl"] for t in trades if t["net_pnl"] > 0]
    losses = [-t["net_pnl"] for t in trades if t["net_pnl"] <= 0]
    kelly = None
    if wins and losses:
        win_rate = len(wins) / len(trades)
        payoff = (sum(wins) / len(wins)) / (sum(losses) / len(losses)) if sum(losses) > 0 else None
        if payoff:
            kelly = win_rate - (1 - win_rate) / payoff
    placed = tallies["placed"]
    return {
        "arm": arm_label(arm),
        "trades": trades,
        "tallies": tallies,
        "net_pnl": sum(t["net_pnl"] for t in trades),
        "net_return_pct": 100.0 * sum(t["net_pnl"] for t in trades) / capital,
        "max_drawdown_pct": 100.0 * ledger.max_dd,
        "turnover": sum(t["notional"] for t in trades) / capital,
        "positions": len(trades),
        "no_fill_share": (tallies["no_fill"] / placed) if placed else None,
        "requested_risk_usd": sum(t["requested_risk_usd"] for t in trades),
        "initial_stop_risk_usd": sum(t["initial_stop_risk_usd"] for t in trades),
        "realized_loss_usd": sum(-t["net_pnl"] for t in trades if t["net_pnl"] < 0),
        "losses_beyond_initial_risk": sum(1 for t in trades if t["loss_beyond_initial_risk"]),
        "kelly_diagnostic": kelly,
    }
