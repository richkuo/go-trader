import bisect
import gzip
import hashlib
import json
import math
import os
import sys
from fractions import Fraction
from typing import Dict, List, Optional

import numpy as np

_HERE = os.path.dirname(os.path.abspath(__file__))
for _p in (_HERE, os.path.join(_HERE, "..", "shared_tools"), os.path.join(_HERE, "..", "shared_strategies", "open")):
    if _p not in sys.path:
        sys.path.insert(0, _p)

from backtester import _hl_floor_lot_size, clamp_stop_inside_liquidation, isolated_liquidation_price, perps_open_notional  # noqa: E402

LEDGER_SCHEMA = "go_trader_book_event_ledger/v1"
MINUTE_MS = 60_000
HOUR_MS = 3_600_000
DAY_MS = 86_400_000
MAX_LEVELS = 20
DEPTHS = (1, 5, 10, 20)
GUARD = 1e-9


class SimulationError(ValueError):
    pass


def _scale_digits(values) -> int:
    best = 0
    for v in values:
        if "." in v:
            best = max(best, len(v.split(".", 1)[1]))
    return best


class CoinBooks:
    def __init__(self, coin: str, chunks, markers: list, px_digits: int, sz_digits: int):
        self.coin = coin
        self.px_digits, self.sz_digits = px_digits, sz_digits
        self.px_scale, self.sz_scale = 10 ** px_digits, 10 ** sz_digits
        cols = {k: [] for k in ("seq", "event", "recv", "session", "bid_depth", "ask_depth", "nb", "na")}
        mats = {k: [] for k in ("bpx", "bsz", "bn", "apx", "asz", "an")}
        self.identity = []
        for chunk in chunks:
            m = len(chunk)
            part = {k: np.zeros((m, MAX_LEVELS), dtype=np.int64) for k in mats}
            for i, o in enumerate(chunk):
                cols["seq"].append(o["seq"])
                cols["event"].append(o["event_ms"])
                cols["recv"].append(o["recv_ms"])
                cols["session"].append(o["session"])
                cols["bid_depth"].append(o["bid_depth"])
                cols["ask_depth"].append(o["ask_depth"])
                cols["nb"].append(len(o["bids"]))
                cols["na"].append(len(o["asks"]))
                self.identity.append(o["identity"])
                for side, kp, ks, kn in (("bids", "bpx", "bsz", "bn"), ("asks", "apx", "asz", "an")):
                    for j2, (px, sz, n) in enumerate(o[side]):
                        part[kp][i, j2] = self._scaled(px, px_digits)
                        part[ks][i, j2] = self._scaled(sz, sz_digits)
                        part[kn][i, j2] = n
            for k in mats:
                mats[k].append(part[k])
        for k, v in cols.items():
            setattr(self, k, np.array(v, dtype=np.int64))
        for k, v in mats.items():
            setattr(self, k, np.concatenate(v) if v else np.zeros((0, MAX_LEVELS), dtype=np.int64))
        self.n = len(self.seq)
        if self.n and not (np.all(np.diff(self.recv) >= 0) and np.all(np.diff(self.seq) > 0)):
            raise SimulationError(f"{coin}: observations are not in receipt and sequence order")
        limit = (1 << 62) // MAX_LEVELS
        if self.n and max(int(self.bpx.max()), int(self.apx.max())) * max(int(self.bsz.max()), int(self.asz.max())) > limit:
            raise SimulationError(f"{coin}: scaled notionals overflow the exact integer bound")
        self.notional_scale = self.px_scale * self.sz_scale
        self.B, self.A = {}, {}
        for d in DEPTHS:
            self.B[d] = (self.bpx[:, :d] * self.bsz[:, :d]).sum(axis=1)
            self.A[d] = (self.apx[:, :d] * self.asz[:, :d]).sum(axis=1)
        self.best_bid = self.bpx[:, 0] if self.n else np.zeros(0, dtype=np.int64)
        self.best_ask = self.apx[:, 0] if self.n else np.zeros(0, dtype=np.int64)
        self.mid = (self.best_bid + self.best_ask) / 2.0 / self.px_scale
        self.mk_at = np.array([m["at_ms"] for m in markers], dtype=np.int64)
        self.mk_seq = np.array([m["seq"] for m in markers], dtype=np.int64)
        self.markers = markers

    def _text(self, v: int, digits: int) -> str:
        if digits == 0:
            return str(v)
        whole, frac = divmod(v, 10 ** digits)
        frac_s = str(frac).rjust(digits, "0").rstrip("0")
        return f"{whole}.{frac_s}" if frac_s else str(whole)

    def levels(self, i: int, side: str, depth: int = MAX_LEVELS) -> list:
        if side == "bids":
            px, sz, nn, cnt = self.bpx, self.bsz, self.bn, int(self.nb[i])
        else:
            px, sz, nn, cnt = self.apx, self.asz, self.an, int(self.na[i])
        return [[self._text(int(px[i, j]), self.px_digits), self._text(int(sz[i, j]), self.sz_digits), int(nn[i, j])]
                for j in range(min(cnt, depth))]

    @staticmethod
    def _scaled(text: str, digits: int) -> int:
        whole, _, frac = text.partition(".")
        if len(frac) > digits:
            raise SimulationError(f"decimal {text!r} has more than {digits} fractional digits")
        return int(whole) * 10 ** digits + (int(frac.ljust(digits, "0")) if digits else 0)

    def classify(self, depth: int, threshold: float, max_spread_bps: float, min_depth_usd: float) -> np.ndarray:
        b, a = self.B[depth], self.A[depth]
        ok_depth = (self.nb >= depth) & (self.na >= depth)
        total = b + a
        min_scaled = Fraction(repr(min_depth_usd)) * self.notional_scale
        mf = float(min_scaled)
        depth_ok = total.astype(np.float64) >= mf
        near = np.abs(total.astype(np.float64) - mf) <= max(1.0, abs(mf) * GUARD)
        for i in np.nonzero(near)[0]:
            depth_ok[i] = Fraction(int(total[i])) >= min_scaled
        m = Fraction(repr(max_spread_bps))
        num = (self.best_ask - self.best_bid).astype(np.float64) * 20000.0
        den = (self.best_ask + self.best_bid).astype(np.float64) * float(m)
        spread_ok = num <= den
        near = np.abs(num - den) <= np.maximum(1.0, np.abs(den) * GUARD)
        for i in np.nonzero(near)[0]:
            spread_ok[i] = 20000 * (int(self.best_ask[i]) - int(self.best_bid[i])) * m.denominator <= m.numerator * (int(self.best_ask[i]) + int(self.best_bid[i]))
        theta = Fraction(repr(threshold))
        with np.errstate(divide="ignore", invalid="ignore"):
            imb = (b - a).astype(np.float64) / total.astype(np.float64)
        q = np.zeros(self.n, dtype=np.int8)
        q[imb > float(theta)] = 1
        q[imb < -float(theta)] = -1
        near = (np.abs(imb - float(theta)) <= GUARD) | (np.abs(imb + float(theta)) <= GUARD)
        for i in np.nonzero(near)[0]:
            bi, ai = int(b[i]), int(a[i])
            if bi + ai <= 0:
                q[i] = 0
                continue
            v = Fraction(bi - ai, bi + ai)
            q[i] = 1 if v > theta else (-1 if v < -theta else 0)
        valid = ok_depth & (total > 0) & spread_ok & depth_ok
        q[~valid] = 0
        return q

    def features(self, i: int, depth: int) -> dict:
        b, a = int(self.B[depth][i]), int(self.A[depth][i])
        s = self.notional_scale
        bid, ask = int(self.best_bid[i]), int(self.best_ask[i])
        return {"B": b / s, "A": a / s, "total": (b + a) / s, "imbalance": (b - a) / (b + a) if b + a else None,
                "spread_bps": 20000.0 * (ask - bid) / (ask + bid), "best_bid": bid / self.px_scale, "best_ask": ask / self.px_scale}


def ceil_second(ms):
    return -(-ms // 1000) * 1000


def candidate_entries(books: CoinBooks, q: np.ndarray, n_confirm: int, max_age_ms: int, window_ms: int) -> list:
    n = books.n
    if n == 0:
        return []
    kind = np.concatenate([np.zeros(n, dtype=np.int8), np.ones(len(books.mk_at), dtype=np.int8)])
    t = np.concatenate([books.recv, books.mk_at])
    sq = np.concatenate([books.seq, books.mk_seq])
    idx = np.concatenate([np.arange(n), np.arange(len(books.mk_at))])
    order = np.lexsort((sq, t))
    kind, t, idx = kind[order], t[order], idx[order]
    qi = np.where(kind == 0, q[idx * (kind == 0)], 9).astype(np.int8)
    start = np.ones(len(qi), dtype=bool)
    start[1:] = (qi[1:] != qi[:-1]) | (qi[1:] == 0) | (qi[1:] == 9)
    run_id = np.cumsum(start)
    first = np.zeros(len(qi), dtype=np.int64)
    first_pos = np.nonzero(start)[0]
    first[:] = first_pos[run_id - 1]
    pos = np.arange(len(qi)) - first + 1
    hits = np.nonzero(((qi == 1) | (qi == -1)) & (pos == n_confirm))[0]
    out = []
    for k in hits:
        s = first[k]
        if s == 0:
            continue
        ob = idx[k]
        cross = max(int(books.event[ob]), int(books.recv[ob]))
        D = ceil_second(cross)
        if int(t[s - 1]) < D - window_ms:
            continue
        if D - int(books.event[ob]) > max_age_ms or D - int(books.recv[ob]) > max_age_ms:
            continue
        cancelled = False
        j = k + 1
        while j < len(qi) and int(t[j]) <= D:
            if kind[j] == 1:
                cancelled = True
                break
            nxt = idx[j]
            if int(books.event[nxt]) <= D and qi[j] != qi[k]:
                cancelled = True
                break
            j += 1
        if cancelled:
            continue
        bk = t[s - 1]
        bkind = kind[s - 1]
        if bkind == 1:
            m = books.markers[idx[s - 1]]
            boundary = f"marker:{m['seq']}:{m['reason']}:{m['at_ms']}"
        else:
            boundary = f"book:{books.identity[idx[s - 1]]}"
        first_obs = idx[s]
        out.append({"deadline_ms": int(D), "coin": books.coin, "direction": int(qi[k]), "crossing": int(ob),
                    "first": int(first_obs), "boundary": boundary, "boundary_ms": int(bk)})
    return out


def section_at(books: CoinBooks, deadline_ms: int, depth: int, window_ms: int, time_semantics: str, contract: dict) -> dict:
    lo = bisect.bisect_left(books.recv, deadline_ms - window_ms)
    hi = bisect.bisect_right(books.recv, deadline_ms)
    obs = []
    for i in range(lo, hi):
        obs.append({"seq": int(books.seq[i]), "event_ms": int(books.event[i]), "recv_ms": int(books.recv[i]),
                    "session": int(books.session[i]), "identity": books.identity[i], "bid_depth": int(books.bid_depth[i]),
                    "ask_depth": int(books.ask_depth[i]), "bids": books.levels(i, "bids", depth), "asks": books.levels(i, "asks", depth)})
    mlo = bisect.bisect_left(books.mk_at, deadline_ms - window_ms)
    mhi = bisect.bisect_right(books.mk_at, deadline_ms)
    marks = []
    for m in books.markers[mlo:mhi]:
        row = {"seq": m["seq"], "at_ms": m["at_ms"], "reason": m["reason"], "session": m["session"]}
        if m.get("detail"):
            row["detail"] = m["detail"]
        marks.append(row)
    return {"kind": "l2_book", "coin": books.coin, **contract, "time_semantics": time_semantics, "window_ms": window_ms,
            "capacity": 4096, "depth": depth, "cutoff_ms": deadline_ms, "retained_from_ms": deadline_ms - window_ms,
            "prefix_complete": True, "owner_instance": "hyperliquid-archive", "snapshot_id": "", "available": True,
            "observations": obs, "markers": marks}


class Bars:
    def __init__(self, books: CoinBooks, first_minute: int, last_minute: int):
        self.first = first_minute
        count = (last_minute - first_minute) // MINUTE_MS + 1
        self.count = count
        self.open = np.full(count, np.nan)
        self.high = np.full(count, np.nan)
        self.low = np.full(count, np.nan)
        self.close = np.full(count, np.nan)
        self.complete = np.zeros(count, dtype=bool)
        minute = (books.recv // MINUTE_MS) * MINUTE_MS
        mk_minutes = set(int(x) for x in (books.mk_at // MINUTE_MS) * MINUTE_MS)
        if books.n:
            bounds = np.nonzero(np.diff(minute))[0] + 1
            starts = np.concatenate([[0], bounds])
            ends = np.concatenate([bounds, [books.n]])
            for s, e in zip(starts, ends):
                m = int(minute[s])
                k = (m - first_minute) // MINUTE_MS
                if not 0 <= k < count:
                    continue
                mids = books.mid[s:e]
                self.open[k], self.close[k] = mids[0], mids[-1]
                self.high[k], self.low[k] = mids.max(), mids.min()
                ok = (int(books.recv[s]) - m <= 10_000 and m + MINUTE_MS - int(books.recv[e - 1]) <= 10_000
                      and m not in mk_minutes)
                self.complete[k] = ok
        self.open[~self.complete] = np.nan
        self.high[~self.complete] = np.nan
        self.low[~self.complete] = np.nan
        self.close[~self.complete] = np.nan

    def index(self, minute_ms: int) -> int:
        return (minute_ms - self.first) // MINUTE_MS

    def atr_before(self, t_ms: int, period: int = 14) -> Optional[float]:
        k = self.index((t_ms // MINUTE_MS) * MINUTE_MS)
        lo = k - period - 1
        if lo < 0 or k > self.count:
            return None
        if not self.complete[lo:k].all():
            return None
        h, l, c = self.high[lo + 1:k], self.low[lo + 1:k], self.close[lo:k - 1]
        tr = np.maximum(h - l, np.maximum(np.abs(h - c), np.abs(l - c)))
        return float(tr.mean())


class Marks:
    def __init__(self, rows: Dict[int, tuple]):
        self.rows = rows
        self.times = np.array(sorted(rows), dtype=np.int64)

    def at(self, t: int):
        return self.rows.get(t)


def walk(books: CoinBooks, i: int, buy: bool, qty: float) -> tuple:
    if buy:
        px_row, sz_row, cnt = books.apx[i], books.asz[i], int(books.na[i])
    else:
        px_row, sz_row, cnt = books.bpx[i], books.bsz[i], int(books.nb[i])
    filled, cost = 0.0, 0.0
    for j in range(cnt):
        px, sz = px_row[j] / books.px_scale, sz_row[j] / books.sz_scale
        take = min(float(sz), qty - filled)
        if take <= 1e-15:
            break
        filled += take
        cost += take * float(px)
    return filled, cost


def walk_lots(books: CoinBooks, i: int, buy: bool, qty: float, decimals: int) -> tuple:
    got, cost = walk(books, i, buy, qty)
    if got + 1e-12 >= qty:
        return qty, cost
    lot = _hl_floor_lot_size(got, decimals)
    if lot <= 0:
        return 0.0, 0.0
    return walk(books, i, buy, lot)


def find_book(books: CoinBooks, after_ms: int, at_or_after_ms: int, within_ms: Optional[int]) -> Optional[int]:
    i = bisect.bisect_right(books.recv, max(after_ms, at_or_after_ms - 1))
    while i < books.n:
        r, e = int(books.recv[i]), int(books.event[i])
        if within_ms is not None and r > at_or_after_ms + within_ms:
            return None
        if r > after_ms and e > after_ms and r >= at_or_after_ms and e >= at_or_after_ms:
            return i
        i += 1
    return None


def simulate(entries: list, market: dict, cfg: dict, split: dict) -> dict:
    books_by: Dict[str, CoinBooks] = market["books"]
    bars_by: Dict[str, Bars] = market["bars"]
    marks_by: Dict[str, Marks] = market["marks"]
    funding_by: Dict[str, Dict[int, float]] = market["funding"]
    eligible_days = market["eligible_days"]
    sz_dec = market["sz_decimals"]
    max_lev = market["max_leverage"]
    start, end = split["start_ms"], split["end_ms"]
    exec_delay = cfg["settle_ms"] + cfg["processing_ms"]
    fee = cfg["taker_fee_pct"]
    slip = cfg["slippage_bps"] / 10000.0
    margin = cfg["margin_per_entry"]
    leverage = cfg["leverage"]
    equity0 = cfg["initial_equity"]
    tail = end + cfg["exit_tail_ms"]
    from close_registry_loader import evaluate as close_evaluate
    time_stop_bars = 0
    while close_evaluate("time_stop", {"bars_held": time_stop_bars}, {}, {"max_bars": cfg["time_stop_bars"]})["close_fraction"] < 1.0:
        time_stop_bars += 1
        if time_stop_bars > 100_000:
            raise SimulationError("the time_stop close owner never closes")
    events = []
    positions = []
    refusals: Dict[str, int] = {}
    held_gaps = {"mark_minutes": 0, "funding_hours": 0, "unfilled_exits": 0}
    busy_until = {c: -1 for c in books_by}

    def refuse(why):
        refusals[why] = refusals.get(why, 0) + 1

    import heapq
    pending = []
    eq = equity0
    peak = equity0
    latched = False
    day_key = None
    day_start_eq = equity0
    day_pre = 0.0

    def apply_until(t_ms):
        nonlocal eq, peak, day_key, day_start_eq, day_pre
        while pending and pending[0][0] <= t_ms:
            t_ev, net_ev, pre_ev = heapq.heappop(pending)
            d = (t_ev // DAY_MS) * DAY_MS
            if d != day_key:
                day_key, day_start_eq, day_pre = d, eq, 0.0
            eq += net_ev
            day_pre += pre_ev
            peak = max(peak, eq)
        d = (t_ms // DAY_MS) * DAY_MS
        if d != day_key:
            day_key, day_start_eq, day_pre = d, eq, 0.0

    for ent in sorted(entries, key=lambda e: (e["deadline_ms"], e["coin"], e.get("entry_id", ""))):
        D, coin, side = ent["deadline_ms"], ent["coin"], ent["direction"]
        if not start <= D < end:
            continue
        apply_until(D)
        if D >= end - cfg["purge_ms"]:
            refuse("purge")
            continue
        if (D // DAY_MS) * DAY_MS not in eligible_days:
            refuse("ineligible_day")
            continue
        if busy_until[coin] >= D:
            refuse("position_open")
            continue
        if latched or (peak - eq) >= cfg["drawdown_latch_pct"] / 100.0 * peak:
            latched = True
            refuse("drawdown_latch")
            continue
        if -day_pre >= cfg["daily_loss_pct"] / 100.0 * day_start_eq:
            refuse("daily_loss_hold")
            continue
        books = books_by[coin]
        E = D + exec_delay
        atr = bars_by[coin].atr_before(E)
        if atr is None or not atr > 0:
            refuse("missing_atr")
            continue
        fi = find_book(books, D, E, cfg["fill_within_ms"])
        if fi is None:
            refuse("no_executable_book")
            continue
        buy = side > 0
        ref = (int(books.best_ask[fi]) if buy else int(books.best_bid[fi])) / books.px_scale
        notional = perps_open_notional(eq, 0, leverage, margin)
        qty = _hl_floor_lot_size(notional / ref, sz_dec[coin])
        if qty <= 0:
            refuse("lot_floor_zero")
            continue
        filled, cost = walk_lots(books, fi, buy, qty, sz_dec[coin])
        if filled <= 0:
            refuse("no_depth")
            continue
        avg = cost / max(filled, 1e-18)
        price = avg * (1 + slip) if buy else avg * (1 - slip)
        if filled * price < cfg["min_notional_usd"]:
            refuse("below_minimum")
            continue
        if filled * atr > cfg["pre_entry_stop_pct"] / 100.0 * eq:
            refuse("stop_risk_bound")
            continue
        fill_t = int(books.recv[fi])
        entry_fee = filled * price * fee
        side_name = "long" if buy else "short"
        stop = price - atr if buy else price + atr
        liq = isolated_liquidation_price(side_name, price, filled, filled * price / leverage, max_lev[coin])
        stop, clamped = clamp_stop_inside_liquidation(side_name, stop, liq)
        if not stop > 0:
            refuse("protection_geometry")
            continue
        time_stop_D = ceil_second(fill_t + time_stop_bars * MINUTE_MS)
        marks = marks_by[coin]
        trigger = None
        t = (fill_t // MINUTE_MS + 1) * MINUTE_MS
        while t < time_stop_D:
            row = marks.at(t)
            if row is None:
                held_gaps["mark_minutes"] += 1
            else:
                mk = row[0]
                if (buy and mk <= stop) or (not buy and mk >= stop):
                    trigger = t
                    break
            t += MINUTE_MS
        if trigger is not None:
            reason, req_after, req_at = "stop", trigger, trigger + 1
        else:
            reason, req_after, req_at = "time_stop", time_stop_D, time_stop_D + exec_delay
        remaining = filled
        exit_cost = 0.0
        exit_fills = []
        xi = find_book(books, req_after, req_at, None)
        while remaining > 1e-12 and xi is not None and int(books.recv[xi]) <= tail:
            got, c = walk_lots(books, xi, not buy, remaining, sz_dec[coin])
            if got > 0:
                exit_cost += c
                exit_fills.append((int(books.recv[xi]), got))
                remaining = round(remaining - got, sz_dec[coin]) if remaining - got > 1e-12 else 0.0
            if remaining > 1e-12:
                xi = xi + 1 if xi + 1 < books.n else None
        if remaining > 1e-12:
            held_gaps["unfilled_exits"] += 1
            refuse("terminal_inventory")
            positions.append({"coin": coin, "side": side, "entry_id": ent.get("entry_id", ""), "deadline_ms": D, "fill_ms": fill_t,
                              "closed": False, "qty": filled, "entry_price": price})
            busy_until[coin] = tail
            continue
        exit_t = exit_fills[-1][0]
        xavg = exit_cost / filled
        xprice = xavg * (1 - slip) if buy else xavg * (1 + slip)
        exit_fee = filled * xprice * fee
        pre = (xprice - price) * filled * (1 if buy else -1)
        funding = 0.0
        h = (fill_t // HOUR_MS + 1) * HOUR_MS
        while h <= exit_t:
            rate = funding_by[coin].get(h)
            row = marks.at(h)
            if rate is None or row is None:
                held_gaps["funding_hours"] += 1
            else:
                funding += filled * row[1] * rate * (1 if buy else -1)
            h += HOUR_MS
        net = pre - entry_fee - exit_fee - funding
        positions.append({"coin": coin, "side": side, "entry_id": ent.get("entry_id", ""), "deadline_ms": D, "fill_ms": fill_t,
                          "exit_ms": exit_t, "exit_reason": reason, "qty": filled, "entry_price": price, "exit_price": xprice,
                          "atr": atr, "stop": stop, "stop_clamped": bool(clamped), "pre_fee_pnl": pre, "fees": entry_fee + exit_fee,
                          "funding": funding, "net": net, "turnover": filled * (price + xprice), "closed": True})
        heapq.heappush(pending, (exit_t, net, pre))
        busy_until[coin] = exit_t
    closed = [p for p in positions if p["closed"]]
    net = sum(p["net"] for p in closed)
    curve, peak, mdd = 0.0, 0.0, 0.0
    for p in sorted(closed, key=lambda p: p["exit_ms"]):
        curve += p["net"]
        peak = max(peak, curve)
        mdd = max(mdd, peak - curve)
    daily: Dict[int, float] = {}
    for p in closed:
        d = (p["exit_ms"] // DAY_MS) * DAY_MS
        daily[d] = daily.get(d, 0.0) + p["net"]
    by_coin, by_dir = {}, {}
    for p in closed:
        by_coin[p["coin"]] = by_coin.get(p["coin"], 0.0) + p["net"]
        k = "long" if p["side"] > 0 else "short"
        by_dir[k] = by_dir.get(k, 0.0) + p["net"]
    gaps = sum(held_gaps.values())
    return {"positions": positions, "closed": len(closed), "terminal": len(positions) - len(closed), "net": net,
            "pre_fee": sum(p["pre_fee_pnl"] for p in closed), "fees": sum(p["fees"] for p in closed),
            "funding": sum(p["funding"] for p in closed), "turnover": sum(p["turnover"] for p in closed),
            "max_drawdown": mdd, "daily": daily, "by_coin": by_coin, "by_direction": by_dir, "refusals": refusals,
            "held_gaps": held_gaps, "held_complete": gaps == 0, "entries_offered": sum(1 for e in entries if start <= e["deadline_ms"] < end)}
