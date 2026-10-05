import json
import os
import sys
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.dirname(HERE)
REPO = os.path.abspath(os.path.join(FIXTURE, "..", "..", ".."))
sys.path.insert(0, os.path.join(REPO, "backtest"))
sys.path.insert(0, os.path.join(REPO, "shared_tools"))

import offline_manifest as om
from atr import ensure_atr_indicator
from backtester import Backtester
from registry_loader import load_registry

TAKER = 0.00045
RECORD_LATENCY_S = 41
STRICT = "hl-strict-btc"
SCALE = "hl-scalein-btc"
MANUAL = "hl-manual-eth"
COLS = ("rowid, strategy_id, timestamp, symbol, position_id, side, quantity, price, value, trade_type, "
        "details, exchange_order_id, exchange_fee, is_close, realized_pnl, pnl_gross, fee_source, regime, "
        "entry_atr, stop_loss_atr_mult, stop_loss_trigger_px, stop_loss_oid, tp_oids_json, tp_tiers_json, manual")


def q(value) -> str:
    if value is None:
        return "NULL"
    if isinstance(value, bool):
        return "1" if value else "0"
    if isinstance(value, int):
        return str(value)
    if isinstance(value, float):
        return repr(value)
    return "'" + str(value).replace("'", "''") + "'"


def r6(x: float) -> float:
    return round(x, 6)


def ts(text: str, seconds: int = 0) -> str:
    t = datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc) + timedelta(seconds=seconds)
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


class Book:

    def __init__(self):
        self.rows = []
        self.next_id = 100

    def trade(self, strategy, when, symbol, pos, side, qty, px, trade_type, details, oid, fee,
              is_close, realized, gross, fee_source="userfills", manual=False, rowid=None):
        rid = rowid if rowid is not None else self.next_id
        self.next_id = max(self.next_id, rid) + 7
        vals = (rid, strategy, when, symbol, pos, side, float(qty), float(px), r6(qty * px), trade_type,
                details, oid, float(fee), 1 if is_close else 0, float(realized), 1 if gross else 0,
                fee_source, "", 0.0, None, 0.0, 0, "", "", 1 if manual else 0)
        self.rows.append(f"INSERT INTO trades ({COLS}) VALUES (" + ", ".join(q(v) for v in vals) + ")")
        return rid

    def funding(self, strategy, when, symbol, amount, dedup):
        return self.trade(strategy, when, symbol, "", "funding", 0.0, 0.0, "funding", "Funding payment",
                          dedup, 0.0, False, r6(amount), True, fee_source="")

    def raw(self, sql):
        self.rows.append(sql)


def strict_simulation(initial_cash: float):
    manifest = om.load_manifest(os.path.join(FIXTURE, "market", "manifest.json"))
    ds = manifest["datasets"][0]
    frame, win, _ = om.window_frame(manifest, ds, "comparison")
    frame, _ = om.attach_funding_cost(frame, ds, win)
    params = {"fast_period": 5, "slow_period": 20}
    signals = ensure_atr_indicator(load_registry("futures").apply_strategy("sma_crossover", frame, params))
    scored = om.slice_window(signals, win)
    bt = Backtester(
        initial_capital=initial_cash, platform="hyperliquid",
        execution_spec=om.execution_spec(manifest, ds), direction="long",
        close_strategies=[{"name": "tiered_tp_pct", "params": {"tp_tiers": [
            {"profit_pct": 0.01, "close_fraction": 0.5}, {"profit_pct": 0.02, "close_fraction": 1.0}]}}],
    )
    res = bt.run(scored, strategy_name="sma_crossover", symbol="BTC", timeframe="1h", params=params,
                 save=False, indicator_frame=signals, record_events=True)
    return res["ledger_events"]


def build() -> list:
    book = Book()
    book.raw("INSERT INTO strategies (id, type, platform, cash, initial_capital) VALUES "
             "('hl-strict-btc','perps','hyperliquid',1000,1000), ('hl-scalein-btc','perps','hyperliquid',1000,1000), "
             "('hl-manual-eth','manual','hyperliquid',500,500)")

    pre_open_px, pre_close_px, pre_qty = 59000.0, 59600.0, 0.0169
    pre_open_fee = r6(pre_qty * pre_open_px * TAKER)
    pre_close_fee = r6(pre_qty * pre_close_px * TAKER)
    pre_gross = r6(pre_qty * (pre_close_px - pre_open_px))
    book.trade(STRICT, "2026-01-04T10:00:41Z", "BTC", "pos-s-0", "buy", pre_qty, pre_open_px, "perps",
               "Open long BTC", "oid-s-0-open", pre_open_fee, False, 0.0, True, rowid=11)
    pre_funding = [(f"2026-01-04T{h:02d}:00:00Z", -0.012375 - 0.000125 * h) for h in (11, 12, 13, 14, 15)]
    for i, (when, amount) in enumerate(pre_funding):
        book.funding(STRICT, when, "BTC", amount, f"funding:s0-{i}")
    book.trade(STRICT, "2026-01-04T15:00:41Z", "BTC", "pos-s-0", "sell", pre_qty, pre_close_px, "perps",
               "Close long BTC", "oid-s-0-close", pre_close_fee, True, pre_gross, True, rowid=60)
    book.raw("INSERT INTO trade_diagnostics (rowid, strategy_id, position_id, symbol, side, timeframe, close_reason, "
             "entry_price, exit_price, quantity, realized_pnl, opened_at, closed_at) VALUES "
             f"(21, 'hl-strict-btc', 'pos-s-0', 'BTC', 'long', '1h', 'tp_tier', {pre_open_px!r}, {pre_close_px!r}, "
             f"{pre_qty!r}, {pre_gross!r}, '2026-01-04T10:00:41Z', '2026-01-04T15:00:41Z')")
    pre_net = (-pre_open_fee) + (pre_gross - pre_close_fee) + sum(r6(a) for _, a in pre_funding)
    starting_cash = 1000.0 + pre_net

    env = strict_simulation(starting_cash)
    positions = {}
    seq = 0
    for ev in env["events"]:
        if ev["kind"] == "terminal_liquidation":
            continue
        if ev["kind"] == "funding":
            book.funding(STRICT, ev["bar_timestamp"], "BTC", ev["funding_cash"], f"funding:s-{ev['seq']}")
            continue
        pid = ev["position_local_id"]
        if pid not in positions:
            seq += 1
            positions[pid] = {"id": f"pos-s-{seq}", "entry_px": None, "closes": 0, "opened_at": "",
                              "closed_qty": 0.0, "last": None}
        p = positions[pid]
        px = float(round(ev["effective_price"]))
        qty = ev["quantity"]
        fee = r6(qty * px * TAKER)
        when = ts(ev["bar_timestamp"], RECORD_LATENCY_S)
        if ev["kind"] == "open":
            p["entry_px"] = px
            p["opened_at"] = when
            book.trade(STRICT, when, "BTC", p["id"], "buy", qty, px, "perps", "Open long BTC",
                       f"oid-{p['id']}-open", fee, False, 0.0, True)
        elif ev["kind"] == "close":
            p["closes"] += 1
            gross = r6(qty * (px - p["entry_px"]))
            p["closed_qty"] += qty
            p["last"] = (ts(ev["bar_timestamp"], RECORD_LATENCY_S + p["closes"]), px)
            book.trade(STRICT, ts(ev["bar_timestamp"], RECORD_LATENCY_S + p["closes"]), "BTC", p["id"], "sell", qty,
                       px, "perps", "Take-profit tier close", f"oid-{p['id']}-close-{p['closes']}", fee, True,
                       gross, True)
        else:
            raise SystemExit(f"unexpected simulated event kind {ev['kind']!r}")
    open_id = env["interval_end"]["position_local_id"]
    tail = positions[open_id]
    tail_qty = env["interval_end"]["position_qty"]
    for h in (20, 21, 22):
        book.funding(STRICT, f"2026-01-06T{h:02d}:00:00Z", "BTC", -0.0128 - 0.0001 * h, f"funding:tail-{h}")
    first = round(tail_qty / 2, 5)
    for n, (when, qty, px) in enumerate(((("2026-01-06T22:00:42Z"), first, 60577.0),
                                         (("2026-01-06T23:00:43Z"), round(tail_qty - first, 5), 60983.0)), start=1):
        book.trade(STRICT, when, "BTC", tail["id"], "sell", qty, px, "perps", "Take-profit tier close",
                   f"oid-{tail['id']}-close-{n}", r6(qty * px * TAKER), True, r6(qty * (px - tail["entry_px"])), True)
    for pid, p in positions.items():
        if pid == open_id:
            continue
        closed_at, exit_px = p["last"]
        book.raw("INSERT INTO trade_diagnostics (strategy_id, position_id, symbol, side, timeframe, close_reason, "
                 "entry_price, exit_price, quantity, opened_at, closed_at) VALUES "
                 f"('hl-strict-btc', '{p['id']}', 'BTC', 'long', '1h', 'tp_tier', {p['entry_px']!r}, {exit_px!r}, "
                 f"{round(p['closed_qty'], 5)!r}, '{p['opened_at']}', '{closed_at}')")

    def scale(when, pos, side, qty, px, trade_type, details, fee, is_close, realized, gross):
        return book.trade(SCALE, when, "BTC", pos, side, qty, px, trade_type, details, f"oid-c-{when[-9:-1]}",
                          fee, is_close, realized, gross)

    scale("2026-01-05T03:00:41Z", "pos-c-1", "buy", 0.0042, 59310.0, "perps", "Open long BTC", 0.112096, False, 0.0, True)
    book.funding(SCALE, "2026-01-05T04:00:00Z", "BTC", -0.003114, "funding:c1-4")
    scale("2026-01-05T05:00:41Z", "pos-c-1", "buy", 0.0042, 59600.0, "scale_in", "Scale-in long", 0.11264, False, 0.0, True)
    book.funding(SCALE, "2026-01-05T05:00:00Z", "BTC", -0.003128, "funding:c1-5")
    scale("2026-01-05T06:00:42Z", "pos-c-1", "sell", 0.0042, 60010.0, "perps", "Take-profit tier close", 0.113419, True, 2.331, True)
    book.funding(SCALE, "2026-01-05T06:00:00Z", "BTC", -0.006261, "funding:c1-6")
    scale("2026-01-05T09:00:43Z", "pos-c-1", "sell", 0.0042, 60830.0, "perps", "Take-profit tier close (legacy row)", 0.114969, True, 5.660031, False)
    book.trade(SCALE, "2026-01-05T12:00:41Z", "BTC", "", "buy", 0.001, 60000.0, "", "", "", 0.027, False, 0.0, False)
    scale("2026-01-06T16:00:41Z", "pos-c-2", "buy", 0.0042, 59710.0, "perps", "Open long BTC (legacy row)", 0.11285, False, 0.0, False)
    book.funding(SCALE, "2026-01-06T17:00:00Z", "BTC", -0.003099, "funding:c2-17")
    scale("2026-01-06T17:00:41Z", "pos-c-2", "buy", 0.0042, 59900.0, "scale_in", "Scale-in long", 0.113211, False, 0.0, True)

    book.trade(MANUAL, "2026-01-05T01:30:41Z", "ETH", "pos-m-1", "buy", 1.0, 3000.0, "manual", "Manual open",
               "oid-m-1", 1.35, False, 0.0, True, manual=True)
    book.trade(MANUAL, "2026-01-05T09:10:41Z", "ETH", "pos-m-1", "sell", 1.0, 3090.0, "manual", "Manual close",
               "oid-m-2", 1.3905, True, 90.0, True, manual=True)
    book.raw("INSERT INTO trade_diagnostics (rowid, strategy_id, position_id, symbol, side, timeframe, close_reason, "
             "entry_price, exit_price, quantity, realized_pnl, opened_at, closed_at) VALUES "
             "(31, 'hl-manual-eth', 'pos-m-1', 'ETH', 'long', '1h', 'manual', 3000.0, 3090.0, 1.0, 90.0, "
             "'2026-01-05T01:30:41Z', '2026-01-05T09:10:41Z')")
    book.raw("INSERT INTO wallet_transfers (rowid, platform, account, time_ms, kind, amount_usd, dedup_id) VALUES "
             "(3, 'hyperliquid', '0xfixture', 1767585600000, 'funding_orphan', -0.0042, 'funding_orphan:f1'), "
             "(4, 'hyperliquid', '0xfixture', 1767600000000, 'deposit', 250.0, 'deposit:d1')")
    return book.rows, starting_cash, env


def main() -> int:
    rows, starting_cash, env = build()
    with open(os.path.join(HERE, "seed.sql"), "w") as fh:
        for row in rows:
            if "\n" in row:
                raise SystemExit("a seed statement must fit on one line")
            fh.write(row + "\n")
    summary = {
        "starting_cash": starting_cash,
        "simulated_events": sum(1 for e in env["events"] if e["kind"] != "funding"),
        "interval_end": env["interval_end"],
    }
    print(json.dumps(summary, indent=1))
    return 0


if __name__ == "__main__":
    sys.exit(main())
